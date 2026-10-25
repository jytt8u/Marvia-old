package panel

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/jytt8u/marvia/internal/connguard"
)

// Защита нод от флуда соединениями — настройки продавца.
//
// Сами пределы живут на ноде (internal/connguard): частота новых соединений
// с одного адреса, запас на пик и общее число открытых. Значения по
// умолчанию подобраны под CGNAT, и большинству продавцов трогать их не
// нужно. Но когда ноду долбят, ждать новой версии некогда: продавец
// ужесточает пределы или закрывает адреса из панели, и ноды применяют это
// со следующей синхронизацией, без перезапуска и без входа на сервер.
//
// Объёмный DDoS — гигабиты в канал — этим не лечится, и панель этого не
// обещает: канал забит раньше, чем пакет дошёл до ноды.

// GuardSettings — пределы для всех нод продавца. Нули — значения по
// умолчанию ноды.
type GuardSettings struct {
	Rate    float64  `json:"rate"`
	Burst   float64  `json:"burst"`
	MaxOpen int      `json:"max_open"`
	Blocked []string `json:"blocked"`
}

// GuardDefaults — значения ноды по умолчанию: их панель показывает рядом с
// полями, чтобы было видно, от чего отступаешь.
var GuardDefaults = GuardSettings{Rate: connguard.DefaultRate, Burst: connguard.DefaultBurst, MaxOpen: connguard.DefaultMaxOpen}

// maxBlocked — сколько адресов и сетей можно закрыть. Каждое соединение
// сверяется со всем списком, и тысяча — с запасом против любой ручной
// атаки; против десятков тысяч адресов нужен хостер с защитой, а не список.
const maxBlocked = 1000

func (g *GuardSettings) check() error {
	switch {
	// Нижние границы — чтобы опечатка не заперла ноду от своих же
	// покупателей: клиент при подключении открывает несколько соединений
	// разом, а за адресом CGNAT их сотни.
	case g.Rate != 0 && (g.Rate < 1 || g.Rate > 10000):
		return errors.New("частота — от 1 до 10000 новых соединений в секунду с адреса")
	case g.Burst != 0 && (g.Burst < 10 || g.Burst > 100000):
		return errors.New("запас на пик — от 10 до 100000 соединений")
	case g.MaxOpen != 0 && (g.MaxOpen < 500 || g.MaxOpen > 1000000):
		return errors.New("открытых соединений — от 500 до 1000000")
	case len(g.Blocked) > maxBlocked:
		return fmt.Errorf("закрыть можно не больше %d адресов и сетей", maxBlocked)
	}
	seen := map[string]bool{}
	clean := make([]string, 0, len(g.Blocked))
	for _, s := range g.Blocked {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		p, err := blockedPrefix(s)
		if err != nil {
			return err
		}
		// Одиночный адрес храним как есть, без /32: так его и вписали, и так
		// его ищут глазами в списке.
		key := p.String()
		if p.IsSingleIP() {
			key = p.Addr().String()
		}
		if !seen[key] {
			seen[key] = true
			clean = append(clean, key)
		}
	}
	g.Blocked = clean
	return nil
}

// blockedPrefix разбирает адрес или сеть и не даёт закрыть полмира: сеть
// шире /8 для IPv4 и /32 для IPv6 почти наверняка опечатка, и она отрезала
// бы целые страны покупателей.
func blockedPrefix(s string) (netip.Prefix, error) {
	if a, err := netip.ParseAddr(s); err == nil {
		a = a.Unmap()
		return netip.PrefixFrom(a, a.BitLen()), nil
	}
	p, err := netip.ParsePrefix(s)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("не понял адрес %q: нужен адрес вида 203.0.113.7 или сеть 203.0.113.0/24", s)
	}
	p = p.Masked()
	if (p.Addr().Is4() && p.Bits() < 8) || (p.Addr().Is6() && p.Bits() < 32) {
		return netip.Prefix{}, fmt.Errorf("сеть %s слишком широкая: закрыла бы покупателей целыми странами", s)
	}
	return p, nil
}

// GuardSettings читает настройку. Испорченная запись — значения по
// умолчанию: лучше обычная защита, чем нода, запертая мусором.
func (s *Store) GuardSettings(ctx context.Context) (GuardSettings, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = 'guard'`).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return GuardSettings{Blocked: []string{}}, nil
	}
	if err != nil {
		return GuardSettings{}, fmt.Errorf("чтение защиты: %w", err)
	}
	var g GuardSettings
	if json.Unmarshal([]byte(raw), &g) != nil || g.check() != nil {
		return GuardSettings{Blocked: []string{}}, nil
	}
	return g, nil
}

// SetGuardSettings проверяет и сохраняет настройку.
func (s *Store) SetGuardSettings(ctx context.Context, g GuardSettings) (GuardSettings, error) {
	if err := g.check(); err != nil {
		return GuardSettings{}, err
	}
	raw, err := json.Marshal(g)
	if err != nil {
		return GuardSettings{}, err
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO settings (key, value) VALUES ('guard', ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value`, string(raw)); err != nil {
		return GuardSettings{}, fmt.Errorf("сохранение защиты: %w", err)
	}
	return g, nil
}

// guardKeep — сколько панель помнит отклонённое по часам. Неделя — чтобы
// видеть «вчера было тихо, сегодня долбят» и не копить лишнего.
const guardKeep = 7 * 24 * time.Hour

// ReportGuard прибавляет отклонённое нодой к её часу.
func (s *Store) ReportGuard(ctx context.Context, nodeID int64, st connguard.Stats, now time.Time) error {
	if st.ByRate < 0 || st.ByOpen < 0 || st.ByBlock < 0 || st.ByRate+st.ByOpen+st.ByBlock == 0 {
		return nil
	}
	hour := now.UTC().Truncate(time.Hour).Format(time.RFC3339)
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO node_guard (node_id, hour, by_rate, by_open, by_block) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (node_id, hour) DO UPDATE SET
			by_rate = by_rate + excluded.by_rate,
			by_open = by_open + excluded.by_open,
			by_block = by_block + excluded.by_block`,
		nodeID, hour, st.ByRate, st.ByOpen, st.ByBlock); err != nil {
		return fmt.Errorf("учёт отклонённых: %w", err)
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM node_guard WHERE hour < ?`,
		now.UTC().Add(-guardKeep).Truncate(time.Hour).Format(time.RFC3339))
	return err
}

// GuardDay — сколько нода отклонила за последние сутки.
type GuardDay struct {
	NodeID  int64  `json:"node_id"`
	Name    string `json:"name"`
	ByRate  int64  `json:"by_rate"`
	ByOpen  int64  `json:"by_open"`
	ByBlock int64  `json:"by_block"`
}

// GuardLastDay — отклонённое по нодам за сутки, тихие ноды тоже: «ноль»
// — тоже ответ, «вас не долбят».
func (s *Store) GuardLastDay(ctx context.Context, now time.Time) ([]GuardDay, error) {
	since := now.UTC().Add(-24 * time.Hour).Truncate(time.Hour).Format(time.RFC3339)
	rows, err := s.db.QueryContext(ctx, `
		SELECT n.id, n.name,
		       COALESCE(SUM(g.by_rate), 0), COALESCE(SUM(g.by_open), 0), COALESCE(SUM(g.by_block), 0)
		FROM nodes n LEFT JOIN node_guard g ON g.node_id = n.id AND g.hour >= ?
		GROUP BY n.id ORDER BY n.id`, since)
	if err != nil {
		return nil, fmt.Errorf("чтение отклонённых: %w", err)
	}
	defer rows.Close()
	list := []GuardDay{}
	for rows.Next() {
		var d GuardDay
		if err := rows.Scan(&d.NodeID, &d.Name, &d.ByRate, &d.ByOpen, &d.ByBlock); err != nil {
			return nil, err
		}
		list = append(list, d)
	}
	return list, rows.Err()
}

// nodeGuard — пределы для ответа ноде: нули заменены значениями по
// умолчанию, чтобы сброшенная продавцом настройка вернула ноде обычные
// пределы, а не оставила прежние строгие.
func nodeGuard(g GuardSettings) GuardSettings {
	if g.Rate == 0 {
		g.Rate = GuardDefaults.Rate
	}
	if g.Burst == 0 {
		g.Burst = GuardDefaults.Burst
	}
	if g.MaxOpen == 0 {
		g.MaxOpen = GuardDefaults.MaxOpen
	}
	if g.Blocked == nil {
		g.Blocked = []string{}
	}
	return g
}

func (a *API) getGuard(w http.ResponseWriter, r *http.Request) {
	g, err := a.store.GuardSettings(r.Context())
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	day, err := a.store.GuardLastDay(r.Context(), time.Now())
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	ok(w, map[string]any{"guard": g, "defaults": GuardDefaults, "day": day})
}

func (a *API) setGuard(w http.ResponseWriter, r *http.Request) {
	var g GuardSettings
	if !decode(w, r, &g) {
		return
	}
	saved, err := a.store.SetGuardSettings(r.Context(), g)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	a.record(r, EventGuardUpdate, Event{Detail: fmt.Sprintf("закрыто адресов и сетей: %d", len(saved.Blocked))})
	ok(w, map[string]any{"guard": saved})
}
