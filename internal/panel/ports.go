package panel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"strconv"
	"strings"
	"time"
)

// Запасные порты ноды и переключение на них.
//
// Нода слушает основной порт и запасные (marvia-node -alt-listen) и
// сообщает запасные панели. Когда основной перестаёт проходить — нода жива и
// синхронизируется, а покупатели дозвониться не могут, или вход каскада
// дважды подряд не достучался, — панель переводит подписки на следующий
// порт и на трое суток кладёт объявление «обновите подписку»: приложения
// Marvia переедут сами, а у Happ и v2RayTun это сделает человек.

// portSwitchEvery — не чаще раза в шесть часов: порт может не пройти и по
// причине, которая лечится не им, и переключение каждые пять минут гоняло
// бы покупателей по кругу.
const portSwitchEvery = 6 * time.Hour

// portNoticeFor — сколько держать объявление о смене порта.
const portNoticeFor = 72 * time.Hour

// effectiveAddress — адрес, который уходит покупателям: с выбранным портом,
// если он не основной.
func effectiveAddress(n Node) string {
	if n.ActivePort == 0 {
		return n.Address
	}
	host, _, err := net.SplitHostPort(n.Address)
	if err != nil {
		return n.Address
	}
	return net.JoinHostPort(host, strconv.Itoa(n.ActivePort))
}

func joinPorts(ports []int) string {
	parts := make([]string, len(ports))
	for i, p := range ports {
		parts[i] = strconv.Itoa(p)
	}
	return strings.Join(parts, ",")
}

func splitPorts(s string) []int {
	var out []int
	for _, part := range strings.Split(s, ",") {
		if p, err := strconv.Atoi(strings.TrimSpace(part)); err == nil && p > 0 && p < 65536 {
			out = append(out, p)
		}
	}
	return out
}

// ReportAltPorts запоминает запасные порты, о которых сообщила нода. Порт,
// которого нода больше не слушает, перестаёт быть выбранным: вести туда
// покупателей значит вести в пустоту.
func (s *Store) ReportAltPorts(ctx context.Context, nodeID int64, ports []int) error {
	if ports == nil {
		return nil
	}
	if len(ports) > 8 {
		ports = ports[:8]
	}
	clean := splitPorts(joinPorts(ports))
	_, err := s.db.ExecContext(ctx, `UPDATE nodes SET alt_ports = ? WHERE id = ?`, joinPorts(clean), nodeID)
	if err != nil {
		return err
	}
	var active int
	if err := s.db.QueryRowContext(ctx, `SELECT active_port FROM nodes WHERE id = ?`, nodeID).Scan(&active); err != nil || active == 0 {
		return err
	}
	for _, p := range clean {
		if p == active {
			return nil
		}
	}
	_, err = s.db.ExecContext(ctx, `UPDATE nodes SET active_port = 0 WHERE id = ?`, nodeID)
	return err
}

// checkActivePort — порт должен быть основным или одним из запасных.
func checkActivePort(n Node, port int) error {
	if port == 0 {
		return nil
	}
	if _, mainPort, err := net.SplitHostPort(n.Address); err == nil && mainPort == strconv.Itoa(port) {
		return nil
	}
	for _, p := range n.AltPorts {
		if p == port {
			return nil
		}
	}
	return inputError{errors.New("этот порт нода не слушает: выбери основной или один из запасных")}
}

// portBlocked — основной путь к ноде не проходит, хотя сама нода жива.
func portBlocked(n Node, h Health, entries []Node, now time.Time) bool {
	if !n.Enabled || n.LastSeen == nil || now.Sub(*n.LastSeen) > standbySilence {
		// Нода молчит — дело не в порте, переключать бесполезно.
		return false
	}
	if h.Degraded() {
		return true
	}
	for _, e := range entries {
		if e.ExitID == n.ID && e.Probe != nil && e.Probe.Fails >= 2 {
			return true
		}
	}
	return false
}

// nextPort — следующий по кругу: основной (0), затем запасные.
func nextPort(n Node) int {
	ring := append([]int{0}, n.AltPorts...)
	for i, p := range ring {
		if p == n.ActivePort {
			return ring[(i+1)%len(ring)]
		}
	}
	return ring[1%len(ring)]
}

// SwitchBlockedPorts переводит заблокированные ноды на следующий порт.
// Возвращает имена переключённых — для оповещения.
func (s *Store) SwitchBlockedPorts(ctx context.Context, now time.Time) ([]string, error) {
	nodes, err := s.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	health, _ := s.NodeHealth(ctx)
	var switched []string
	for _, n := range nodes {
		if len(n.AltPorts) == 0 || !portBlocked(n, health[n.ID], nodes, now) {
			continue
		}
		var last string
		_ = s.db.QueryRowContext(ctx, `SELECT COALESCE(port_switched, '') FROM nodes WHERE id = ?`, n.ID).Scan(&last)
		if last != "" && now.Sub(parse(last)) < portSwitchEvery {
			continue
		}
		next := nextPort(n)
		if _, err := s.db.ExecContext(ctx, `UPDATE nodes SET active_port = ?, port_switched = ? WHERE id = ?`, next, format(now.UTC()), n.ID); err != nil {
			return switched, err
		}
		// Отчёты о прежнем порте больше не про этот путь: без сброса
		// следующая проверка снова увидела бы «не проходит» и погнала
		// покупателей дальше по кругу.
		if _, err := s.db.ExecContext(ctx, `DELETE FROM node_reports WHERE node_id = ?`, n.ID); err != nil {
			return switched, err
		}
		if err := s.setPortNotice(ctx, n.Name, now); err != nil {
			return switched, err
		}
		label := strconv.Itoa(next)
		if next == 0 {
			label = "основной"
		}
		s.Record(ctx, Event{Action: EventNodePort, Actor: ActorAuto, NodeID: n.ID, Detail: "порт: " + label})
		switched = append(switched, n.Name)
	}
	return switched, nil
}

type portNotice struct {
	Nodes []string  `json:"nodes"`
	Until time.Time `json:"until"`
}

func (s *Store) setPortNotice(ctx context.Context, name string, now time.Time) error {
	n := s.portNotice(ctx, now)
	n.Until = now.Add(portNoticeFor).UTC()
	found := false
	for _, x := range n.Nodes {
		found = found || x == name
	}
	if !found {
		n.Nodes = append(n.Nodes, name)
	}
	raw, _ := json.Marshal(n)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO settings (key, value) VALUES ('port_notice', ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value`, string(raw))
	return err
}

// portNotice — действующее объявление; истёкшее — пустое.
func (s *Store) portNotice(ctx context.Context, now time.Time) portNotice {
	var raw string
	if s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = 'port_notice'`).Scan(&raw) != nil {
		return portNotice{}
	}
	var n portNotice
	if json.Unmarshal([]byte(raw), &n) != nil || !now.Before(n.Until) {
		return portNotice{}
	}
	return n
}

// withPortNotice дописывает к объявлению продавца строку о смене порта.
func (s *Store) withPortNotice(ctx context.Context, announce string, now time.Time) string {
	n := s.portNotice(ctx, now)
	if len(n.Nodes) == 0 {
		return announce
	}
	line := fmt.Sprintf("Сервер %s сменил порт. Не подключается — обновите подписку.", strings.Join(n.Nodes, ", "))
	if announce == "" {
		return line
	}
	return announce + "\n" + line
}

// KeepPortsOpen проверяет раз в пять минут, не пора ли сменить порт.
func (a *API) KeepPortsOpen(ctx context.Context, say func(string)) {
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			names, err := a.store.SwitchBlockedPorts(ctx, now)
			if err != nil {
				log.Printf("смена портов: %v", err)
			}
			for _, name := range names {
				if say != nil {
					say("Нода «" + name + "» перестала проходить по своему порту — подписки переведены на запасной.")
				}
			}
		}
	}
}
