package panel

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"
)

// Парк нод: нагрузка машин, ночная скорость и обновление по очереди.

// NodeSys — нагрузка машины ноды по её последнему отчёту.
type NodeSys struct {
	Load     float64   `json:"load"`
	CPUs     int       `json:"cpus"`
	MemTotal int64     `json:"mem_total"`
	MemAvail int64     `json:"mem_avail"`
	At       time.Time `json:"at"`
}

// ReportNodeSys запоминает нагрузку. Пустой отчёт (нода не на Linux или
// /proc закрыт) не затирает прежний.
func (s *Store) ReportNodeSys(ctx context.Context, nodeID int64, sys NodeSys, now time.Time) error {
	if sys.CPUs <= 0 && sys.MemTotal <= 0 {
		return nil
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE nodes SET sys_load = ?, sys_cpus = ?, sys_mem_total = ?, sys_mem_avail = ?, sys_at = ? WHERE id = ?`,
		sys.Load, sys.CPUs, sys.MemTotal, sys.MemAvail, format(now.UTC()), nodeID)
	return err
}

// NightSpeed — ночью без потолка скорости: «днём 50 Мбит/с, ночью без
// ограничения» — частый тариф, и ночью канал ноды всё равно простаивает.
type NightSpeed struct {
	Enabled bool `json:"enabled"`
	// From и Until — часы по Москве; окно может переходить через полночь.
	From  int `json:"from"`
	Until int `json:"until"`
}

func (n NightSpeed) check() error {
	if n.From < 0 || n.From > 23 || n.Until < 0 || n.Until > 23 || (n.Enabled && n.From == n.Until) {
		return errors.New("ночное окно — часы от 0 до 23, начало и конец разные")
	}
	return nil
}

// active — идёт ли ночное окно в now.
func (n NightSpeed) active(now time.Time) bool {
	if !n.Enabled {
		return false
	}
	h := now.In(moscow).Hour()
	if n.From < n.Until {
		return h >= n.From && h < n.Until
	}
	return h >= n.From || h < n.Until
}

func readNightSpeed(ctx context.Context, q rowQuerier) NightSpeed {
	var raw string
	if q.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = 'night_speed'`).Scan(&raw) != nil {
		return NightSpeed{}
	}
	var n NightSpeed
	if json.Unmarshal([]byte(raw), &n) != nil || n.check() != nil {
		return NightSpeed{}
	}
	return n
}

// SetNightSpeed сохраняет ночное окно.
func (s *Store) SetNightSpeed(ctx context.Context, n NightSpeed) error {
	if err := n.check(); err != nil {
		return err
	}
	raw, _ := json.Marshal(n)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO settings (key, value) VALUES ('night_speed', ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value`, string(raw))
	return err
}

func (a *API) getNightSpeed(w http.ResponseWriter, r *http.Request) {
	ok(w, map[string]any{"night_speed": readNightSpeed(r.Context(), a.store.db)})
}

func (a *API) setNightSpeed(w http.ResponseWriter, r *http.Request) {
	var n NightSpeed
	if !decode(w, r, &n) {
		return
	}
	if err := a.store.SetNightSpeed(r.Context(), n); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	a.record(r, EventNightSpeed, Event{Detail: fmt.Sprintf("%t, %02d–%02d", n.Enabled, n.From, n.Until)})
	ok(w, map[string]any{"night_speed": n})
}

// Обновление нод по очереди.
//
// Раньше «Обновить все» просило обновиться все ноды разом, и на минуту
// перезапуска покупатели всех нод оставались без связи одновременно, а
// сломанная сборка ломала весь парк сразу. Теперь просьба уходит одной
// ноде; следующая — когда эта сообщила новую версию или не справилась за
// rollStuck. Клиенты тем временем переезжают на соседние.

// rollStuck — сколько ждать одну ноду. Обычное обновление — минута-две;
// больше двадцати — нода, скорее всего, не может обновиться, и держать
// из-за неё весь парк незачем: просьба у неё остаётся, очередь идёт дальше.
const rollStuck = 20 * time.Minute

// rollEvery — как часто проверять очередь, если нода не сообщила о себе.
const rollEvery = time.Minute

// RollNodeUpgrades двигает очередь: если ни одна нода не обновляется
// прямо сейчас, просит следующую. Возвращает, сколько нод ещё ждут.
func (s *Store) RollNodeUpgrades(ctx context.Context, now time.Time) (int, error) {
	var target string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = 'rolling_upgrade'`).Scan(&target)
	if errors.Is(err, sql.ErrNoRows) || target == "" {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	nodes, err := s.ListNodes(ctx)
	if err != nil {
		return 0, err
	}
	asked := map[int64]time.Time{}
	rows, err := s.db.QueryContext(ctx, `SELECT id, upgrade_asked FROM nodes WHERE upgrade_asked IS NOT NULL`)
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var id int64
		var at string
		if rows.Scan(&id, &at) == nil {
			asked[id] = parse(at)
		}
	}
	rows.Close()

	waiting, busy := 0, false
	var next *Node
	for i, n := range nodes {
		if !n.Enabled || !OlderVersion(n.Version, target) {
			continue
		}
		if n.UpgradeTo == target {
			if at, ok := asked[n.ID]; ok && now.Sub(at) < rollStuck {
				busy = true
			}
			continue
		}
		waiting++
		if next == nil {
			next = &nodes[i]
		}
	}
	if next == nil {
		if !busy {
			_, err := s.db.ExecContext(ctx, `DELETE FROM settings WHERE key = 'rolling_upgrade'`)
			return 0, err
		}
		return 0, nil
	}
	if busy {
		return waiting, nil
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE nodes SET upgrade_to = ?, upgrade_asked = ? WHERE id = ?`, target, format(now.UTC()), next.ID); err != nil {
		return waiting, err
	}
	return waiting - 1, nil
}

// KeepRolling двигает очередь обновлений, пока не отменён ctx: на случай,
// если нода обновилась и замолчала, не прислав версию.
func (a *API) KeepRolling(ctx context.Context) {
	t := time.NewTicker(rollEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			if _, err := a.store.RollNodeUpgrades(ctx, now); err != nil {
				log.Printf("очередь обновления нод: %v", err)
			}
		}
	}
}
