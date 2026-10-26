package panel

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"
)

// Каскад: вход в стране покупателя и зарубежная нода-выход.
//
// Зарубежные адреса режут подсетями хостеров целиком, при белых списках
// мобильный интернет пускает только к своим, российским. Маскировка от этого
// не спасает: блокируют адрес, а не то, что по нему ходит. Вход ставится в
// российском облаке и перекладывает байты на выход; от облака до выхода связь
// межсерверная, её белые списки не трогают.
//
// Вход — та же запись в таблице нод со ссылкой на выход (exit_id). Так у него
// бесплатно есть всё, что есть у ноды: свой токен, «на связи», версия,
// обновление кнопкой, выключение. Ключей у входа нет: протокол насквозь
// говорит выход, и в подписку вход уходит с параметрами выхода и своим
// адресом (viaEntries).
//
// Чего вход не получает: список покупателей. Панель говорит ему одно — куда
// перекладывать. Выходу — одно: адреса его входов, от которых верить
// заголовку с адресом покупателя (internal/proxyproto).

// cascadeSince — с какой версии нода понимает заголовок входа. Выход
// постарше принял бы заголовок за мусор и отдал бы покупателю сайт
// прикрытия: вход «работал» бы, а не подключался никто.
const cascadeSince = "v0.13.0-alpha.24"

// checkExit проверяет, годится ли нода в выход каскада.
func (a *API) checkExit(ctx context.Context, id int64) error {
	exit, err := a.store.GetNode(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return errors.New("такой ноды нет")
	}
	if err != nil {
		return err
	}
	switch {
	case exit.ExitID != 0:
		return errors.New("это сам вход: выходом может быть только обычная нода")
	case exit.WSPath != "":
		// За CDN адрес ноды и так скрыт, и протокол там другой: CDN
		// расшифровывает TLS у себя, и сырые байты от входа он не поймёт.
		return errors.New("нода за CDN не бывает выходом: её адрес и так скрыт")
	case exit.Version != "" && OlderVersion(exit.Version, cascadeSince):
		return fmt.Errorf("нода на %s не понимает входов: обнови её до %s или новее", exit.Version, cascadeSince)
	}
	return nil
}

// viaEntries раскрывает входы для подписки: каждый вход становится копией
// своего выхода — те же ключи, маскировка и страна, но адрес входа и его
// собственный номер. По номеру клиенты сообщают, отвечает ли нода, и
// сводка доступности у входа и выхода своя: заблокированный выход и живой
// вход к нему — разные строки.
//
// QUIC через вход не идёт: вход перекладывает только TCP.
func viaEntries(nodes []Node) []Node {
	exits := make(map[int64]Node, len(nodes))
	for _, n := range nodes {
		if n.ExitID == 0 {
			exits[n.ID] = n
		}
	}
	out := make([]Node, 0, len(nodes))
	for _, n := range nodes {
		if n.ExitID == 0 {
			out = append(out, n)
			continue
		}
		exit, ok := exits[n.ExitID]
		if !ok {
			continue
		}
		v := exit
		v.ID = n.ID
		v.Address = n.Address
		v.QUIC = false
		v.Enabled = n.Enabled && exit.Enabled
		v.LastSeen = n.LastSeen
		// Для кого вход — решают группы входа: его ставят ради конкретных
		// покупателей, а выход может служить всем.
		v.Groups = n.Groups
		// Страна — выхода: покупатель выбирает, где окажется его адрес, а
		// не через какой город туда ехать. Вход — в имени.
		v.Name = entryTitle(exit.Name, n.Name)
		out = append(out, v)
	}
	return out
}

// entryTitle — имя входа для покупателя: «Хельсинки · вход Москва».
func entryTitle(exit, entry string) string {
	if entry == "" {
		return exit + " · через вход"
	}
	return exit + " · вход " + entry
}

// entrySync — ответ входу на сверку: куда перекладывать. Покупателей нет:
// входу они не нужны, а изъятый вход не должен их выдать.
func (a *API) entrySync(w http.ResponseWriter, r *http.Request, n Node) {
	if ip := callerIP(r); net.ParseIP(ip) != nil {
		if err := a.store.SetRelayIP(r.Context(), n.ID, ip); err != nil {
			fail(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	to := ""
	exit, err := a.store.GetNode(r.Context(), n.ExitID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Выключенный выход — пустой адрес: вход перестаёт перекладывать, а не
	// ведёт покупателей туда, откуда продавец их убрал.
	if err == nil && exit.Enabled {
		to = exit.Address
	}
	ok(w, map[string]any{"users": []any{}, "upgrade_to": n.UpgradeTo, "relay_to": to})
}

// SetRelayIP запоминает, откуда вход сверяется с панелью. Пишем только при
// перемене: сверка идёт каждые четверть минуты.
func (s *Store) SetRelayIP(ctx context.Context, id int64, ip string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE nodes SET relay_ip = ? WHERE id = ? AND relay_ip <> ?`, ip, id, ip)
	if err != nil {
		return fmt.Errorf("адрес входа: %w", err)
	}
	return nil
}

// RelayIPs — адреса включённых входов ноды: от них она верит заголовку с
// адресом покупателя. Два источника: адрес из записи входа, если это IP, и
// адрес, с которого вход сверяется с панелью, — у облачной машины они
// обычно совпадают, но за NAT облака бывают разными.
func (s *Store) RelayIPs(ctx context.Context, exitID int64) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT address, relay_ip FROM nodes WHERE exit_id = ? AND enabled = 1`, exitID)
	if err != nil {
		return nil, fmt.Errorf("входы ноды: %w", err)
	}
	defer rows.Close()
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		if ip := net.ParseIP(s); ip != nil && !seen[ip.String()] {
			seen[ip.String()] = true
			out = append(out, ip.String())
		}
	}
	for rows.Next() {
		var address, relayIP string
		if err := rows.Scan(&address, &relayIP); err != nil {
			return nil, err
		}
		add(hostOfAddr(address))
		add(relayIP)
	}
	return out, rows.Err()
}

// SubscriptionNodes — ноды в том виде, в каком они уходят покупателям:
// входы раскрыты в копии своих выходов.
//
// group — группа нод покупателя: ноды других групп ему не уходят.
func (s *Store) SubscriptionNodes(ctx context.Context, group string) ([]Node, error) {
	nodes, err := s.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	// Запасные — только вместо легших основных; сводка отчётов не
	// прочиталась — считаем основные здоровыми по ней, но молчание и
	// выключение видно и без неё.
	health, _ := s.NodeHealth(ctx)
	return forGroup(withStandby(viaEntries(nodes), health, time.Now()), group), nil
}
