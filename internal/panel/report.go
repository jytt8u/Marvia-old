package panel

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Отчёт за месяц и тексты для бота.

// MonthReport — что случилось за календарный месяц по Москве.
type MonthReport struct {
	Month string `json:"month"`
	// New — сколько покупателей заведено за месяц (из тех, кто не удалён).
	New int `json:"new"`
	// Renewals — сколько раз продлевали: тарифом, сроком или «+30 дней».
	Renewals int `json:"renewals"`
	// Promos — сколько промокодов применено.
	Promos int `json:"promos"`
	// Lapsed — у скольких срок кончился в этом месяце и так и не продлён.
	Lapsed int `json:"lapsed"`
	// Traffic — расход по нодам за месяц.
	Traffic []NodeMonth `json:"traffic"`
}

// NodeMonth — расход ноды за месяц.
type NodeMonth struct {
	NodeID int64  `json:"node_id"`
	Name   string `json:"name"`
	Bytes  int64  `json:"bytes"`
}

// MonthBounds — начало месяца и начало следующего по Москве, в UTC.
func monthBounds(month string) (time.Time, time.Time, error) {
	start, err := time.ParseInLocation("2006-01", month, moscow)
	if err != nil {
		return time.Time{}, time.Time{}, errors.New("месяц — в виде 2026-10")
	}
	return start.UTC(), start.AddDate(0, 1, 0).UTC(), nil
}

// MonthReport собирает отчёт. Только числа: кто именно ушёл, видно в списке
// клиентов фильтром, а в отчёте — сколько.
func (s *Store) MonthReport(ctx context.Context, month string, now time.Time) (MonthReport, error) {
	from, to, err := monthBounds(month)
	if err != nil {
		return MonthReport{}, err
	}
	r := MonthReport{Month: month, Traffic: []NodeMonth{}}
	f, t := format(from), format(to)
	queries := []struct {
		dst *int
		sql string
		arg []any
	}{
		{&r.New, `SELECT COUNT(*) FROM users WHERE created_at >= ? AND created_at < ?`, []any{f, t}},
		{&r.Renewals, `SELECT COUNT(*) FROM events WHERE action = ? AND at >= ? AND at < ?`, []any{EventUserRenew, f, t}},
		{&r.Promos, `SELECT COUNT(*) FROM promo_uses WHERE at >= ? AND at < ?`, []any{f, t}},
		// Кончился в этом месяце и до сих пор не продлён: срок в пределах
		// месяца и уже в прошлом.
		{&r.Lapsed, `SELECT COUNT(*) FROM users WHERE expires_at >= ? AND expires_at < ? AND expires_at < ?`, []any{f, t, format(now.UTC())}},
	}
	for _, q := range queries {
		if err := s.db.QueryRowContext(ctx, q.sql, q.arg...).Scan(q.dst); err != nil {
			return MonthReport{}, fmt.Errorf("отчёт: %w", err)
		}
	}
	// usage_daily хранит дни по UTC; для месяца разница в три часа на
	// краях — доли процента трафика, и ради них отдельный счёт не нужен.
	rows, err := s.db.QueryContext(ctx, `
		SELECT n.id, n.name, COALESCE(SUM(d.up + d.down), 0)
		FROM nodes n LEFT JOIN usage_daily d ON d.node_id = n.id AND d.day >= ? AND d.day < ?
		GROUP BY n.id ORDER BY 3 DESC, n.id`, from.Format("2006-01-02"), to.Format("2006-01-02"))
	if err != nil {
		return MonthReport{}, fmt.Errorf("отчёт: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var n NodeMonth
		if err := rows.Scan(&n.NodeID, &n.Name, &n.Bytes); err != nil {
			return MonthReport{}, err
		}
		r.Traffic = append(r.Traffic, n)
	}
	return r, rows.Err()
}

func (a *API) monthReport(w http.ResponseWriter, r *http.Request) {
	month := r.URL.Query().Get("month")
	if month == "" {
		month = time.Now().In(moscow).Format("2006-01")
	}
	rep, err := a.store.MonthReport(r.Context(), month, time.Now())
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	ok(w, map[string]any{"report": rep})
}

// BotTexts — готовые тексты для покупателя к событиям вебхуков.
//
// Бот продавца получает событие и сам решает, что написать. Но писать
// «доступ кончился» в коде бота — значит менять код ради каждой запятой.
// Здесь продавец пишет текст сам, а панель кладёт его в событие полем text
// с подставленными именем, датой и остатком.
type BotTexts map[string]string

// botTextEvents — события, к которым бывает текст.
var botTextEvents = []string{EventUserCreate, EventUserRenew, WebhookUserExpired, WebhookTrafficLow, WebhookTrafficEmpty}

const maxBotText = 1000

// BotTexts читает тексты.
func (s *Store) BotTexts(ctx context.Context) (BotTexts, error) {
	return readBotTexts(ctx, s.db)
}

type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func readBotTexts(ctx context.Context, q rowQuerier) (BotTexts, error) {
	var raw string
	err := q.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = 'bot_texts'`).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return BotTexts{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := BotTexts{}
	if json.Unmarshal([]byte(raw), &out) != nil {
		return BotTexts{}, nil
	}
	return out, nil
}

// SetBotTexts сохраняет тексты. Незнакомое событие — ошибка, а не молчание:
// опечатка в имени события значила бы текст, который никогда не уйдёт.
func (s *Store) SetBotTexts(ctx context.Context, texts BotTexts) error {
	clean := BotTexts{}
	for event, text := range texts {
		known := false
		for _, e := range botTextEvents {
			known = known || e == event
		}
		if !known {
			return fmt.Errorf("нет такого события для текста: %q", event)
		}
		text = strings.TrimSpace(text)
		if len([]rune(text)) > maxBotText {
			return fmt.Errorf("текст к %s длиннее %d знаков", event, maxBotText)
		}
		if text != "" {
			clean[event] = text
		}
	}
	raw, _ := json.Marshal(clean)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO settings (key, value) VALUES ('bot_texts', ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value`, string(raw))
	return err
}

// renderBotText подставляет в шаблон {name}, {date}, {days}, {left} и
// {used}. Незнакомые скобки остаются как есть: продавец увидит их в первом
// же сообщении и поправит, а молча выкинутое слово он бы не заметил.
func renderBotText(tmpl string, u WebhookUser, now time.Time) string {
	if tmpl == "" {
		return ""
	}
	date, days := "бессрочно", "∞"
	if u.ExpiresAt != nil {
		date = u.ExpiresAt.In(moscow).Format("02.01.2006")
		d := int(u.ExpiresAt.Sub(now).Hours() / 24)
		if d < 0 {
			d = 0
		}
		days = strconv.Itoa(d)
	}
	left := "без лимита"
	if u.TrafficLimit > 0 {
		rest := u.TrafficLimit - u.Used
		if rest < 0 {
			rest = 0
		}
		left = humanBytes(rest)
	}
	name := u.Label
	if name == "" {
		name = "#" + strconv.FormatInt(u.ID, 10)
	}
	return strings.NewReplacer("{name}", name, "{date}", date, "{days}", days, "{left}", left, "{used}", humanBytes(u.Used)).Replace(tmpl)
}

func humanBytes(b int64) string {
	const gb = 1 << 30
	if b >= gb {
		return strconv.FormatFloat(float64(b)/gb, 'f', 1, 64) + " ГБ"
	}
	return strconv.FormatInt(b>>20, 10) + " МБ"
}

func (a *API) getBotTexts(w http.ResponseWriter, r *http.Request) {
	texts, err := a.store.BotTexts(r.Context())
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	ok(w, map[string]any{"texts": texts, "events": botTextEvents})
}

func (a *API) setBotTexts(w http.ResponseWriter, r *http.Request) {
	var body BotTexts
	if !decode(w, r, &body) {
		return
	}
	if err := a.store.SetBotTexts(r.Context(), body); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	a.record(r, EventBotTexts, Event{})
	ok(w, map[string]any{"saved": true})
}
