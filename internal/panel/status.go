package panel

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"time"

	"github.com/jytt8u/marvia/internal/look"
	"github.com/jytt8u/marvia/internal/seller"
	"golang.org/x/text/language"
)

// Страница статуса нод.
//
// «У меня не работает» продавец слышит чаще всего, и половина таких
// сообщений — когда лежит одна нода, а приложение уже переехало на другую,
// или идёт объявленное обслуживание. Ссылка на страницу в канале продавца
// отвечает на это раньше, чем покупатель напишет.
//
// Страница открыта без входа, поэтому показывает только то, что покупатель
// и так видит в приложении: имя ноды и жива ли она. Ни адресов, ни
// нагрузки, ни числа покупателей. Путь — случайный: панель не должна
// отвечать сканерам на /status, а продавец, поменяв ссылку, отзывает
// старую.

// statusSilence — сколько нода может молчать, оставаясь «работает». Тот же
// порог, что выводит запасную в подписки: страница не должна говорить
// «всё хорошо», пока клиенты уже переезжают.
const statusSilence = standbySilence

// StatusPage — включена ли страница и по какому пути она открывается.
type StatusPage struct {
	Enabled bool   `json:"enabled"`
	Slug    string `json:"slug,omitempty"`
}

// StatusPage читает настройку. Нет записи — страница выключена.
func (s *Store) StatusPage(ctx context.Context) (StatusPage, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = 'status_page'`).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return StatusPage{}, nil
	}
	if err != nil {
		return StatusPage{}, fmt.Errorf("чтение страницы статуса: %w", err)
	}
	var out StatusPage
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		// Испорченная запись — страница выключена, а не панель упала.
		return StatusPage{}, nil
	}
	return out, nil
}

// SetStatusPage сохраняет настройку.
func (s *Store) SetStatusPage(ctx context.Context, v StatusPage) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO settings (key, value) VALUES ('status_page', ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value`, string(raw))
	if err != nil {
		return fmt.Errorf("сохранение страницы статуса: %w", err)
	}
	return nil
}

// newStatusSlug — 16 случайных байт: путь, который не угадать перебором.
func newStatusSlug() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

type statusView struct {
	Enabled bool   `json:"enabled"`
	URL     string `json:"url,omitempty"`
}

func (a *API) statusView(p StatusPage) statusView {
	v := statusView{Enabled: p.Enabled}
	if p.Enabled && p.Slug != "" {
		// Тот же адрес, что у подписок: он задан продавцом, а Host запроса
		// мог прийти от кого угодно. Не задан — путь без адреса.
		v.URL = a.subBase + "/status/" + p.Slug
	}
	return v
}

// getStatusPage отдаёт, включена ли страница, и её адрес.
func (a *API) getStatusPage(w http.ResponseWriter, r *http.Request) {
	p, err := a.store.StatusPage(r.Context())
	if err != nil {
		respondStoreErr(w, err)
		return
	}
	ok(w, a.statusView(p))
}

// setStatusPage включает, выключает или меняет ссылку. Новая ссылка
// отзывает старую: её могли переслать туда, где её видеть не должны.
func (a *API) setStatusPage(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Enabled bool `json:"enabled"`
		NewLink bool `json:"new_link"`
	}
	if !decode(w, r, &body) {
		return
	}
	was, err := a.store.StatusPage(r.Context())
	if err != nil {
		respondStoreErr(w, err)
		return
	}
	next := StatusPage{Enabled: body.Enabled, Slug: was.Slug}
	if next.Enabled && (next.Slug == "" || body.NewLink) {
		if next.Slug, err = newStatusSlug(); err != nil {
			fail(w, http.StatusInternalServerError, "не удалось придумать ссылку")
			return
		}
	}
	if err := a.store.SetStatusPage(r.Context(), next); err != nil {
		respondStoreErr(w, err)
		return
	}
	detail := ""
	switch {
	case next.Enabled && !was.Enabled:
		detail = "страница статуса включена"
	case !next.Enabled && was.Enabled:
		detail = "страница статуса выключена"
	case next.Enabled && next.Slug != was.Slug:
		detail = "у страницы статуса новая ссылка"
	}
	if detail != "" {
		a.record(r, EventSellerUpdate, Event{Detail: detail})
	}
	ok(w, a.statusView(next))
}

// Состояния ноды на странице.
const (
	nodeUp    = "up"
	nodeDown  = "down"
	nodeMaint = "maint"
)

type statusNode struct {
	Name  string
	State string
	Label string
}

// statusNodes — ноды так, как их видят покупатели: входы каскада вместо
// выходов, запасные — только когда работают вместо основной. Выключенные
// продавцом не показываем вовсе: их нет и в подписках.
func statusNodes(nodes []Node, health map[int64]Health, now time.Time, text statusText) []statusNode {
	var out []statusNode
	for _, n := range withStandby(viaEntries(nodes), health, now) {
		v := statusNode{Name: n.Name}
		switch {
		case n.Maintenance:
			v.State, v.Label = nodeMaint, text.Maint
		case !n.Enabled:
			continue
		case n.LastSeen != nil && now.Sub(*n.LastSeen) <= statusSilence:
			v.State, v.Label = nodeUp, text.Up
		default:
			v.State, v.Label = nodeDown, text.Down
		}
		out = append(out, v)
	}
	return out
}

type statusText struct {
	Language, Title, Intro, Up, Down, Maint, Empty, Updated, AllUp, SomeDown string
}

var (
	statusRussian = statusText{
		Language: "ru", Title: "Состояние серверов",
		Intro: "Если один сервер не отвечает, приложение само переходит на живой. Страница обновляется раз в минуту.",
		Up:    "работает", Down: "не отвечает", Maint: "обслуживание",
		Empty: "Серверов пока нет.", Updated: "Обновлено",
		AllUp: "Всё работает", SomeDown: "Есть перебои",
	}
	statusEnglish = statusText{
		Language: "en", Title: "Server status",
		Intro: "If one server stops responding, the app moves to a live one by itself. This page refreshes every minute.",
		Up:    "working", Down: "not responding", Maint: "maintenance",
		Empty: "No servers yet.", Updated: "Updated",
		AllUp: "All systems working", SomeDown: "Some servers are down",
	}
)

//go:embed web/status.html
var statusHTML string

var statusTemplate = template.Must(template.New("status").Parse(statusHTML))

type statusPageView struct {
	Text     statusText
	Nodes    []statusNode
	AllUp    bool
	Announce string
	Updated  string
	Nonce    string
	Mark     template.URL
}

// statusPublic — сама страница. Выключена или путь не тот — 404, как у
// любого несуществующего адреса: по ответу не понять, есть ли тут панель.
func (a *API) statusPublic(w http.ResponseWriter, r *http.Request) {
	p, err := a.store.StatusPage(r.Context())
	if err != nil || !p.Enabled || p.Slug == "" || r.PathValue("slug") != p.Slug {
		http.NotFound(w, r)
		return
	}
	text := statusRussian
	if _, index := language.MatchStrings(buyerLanguages, r.Header.Get("Accept-Language")); index == 1 {
		text = statusEnglish
	}
	nodes, err := a.store.ListNodes(r.Context())
	if err != nil {
		http.Error(w, "страница статуса временно недоступна", http.StatusServiceUnavailable)
		return
	}
	health, _ := a.store.NodeHealth(r.Context())
	now := time.Now()
	view := statusPageView{Text: text, Nodes: statusNodes(nodes, health, now, text), AllUp: true}
	for _, n := range view.Nodes {
		if n.State != nodeUp {
			view.AllUp = false
		}
	}
	view.Updated = now.UTC().Format("2006-01-02 15:04 UTC")
	if info, err := a.store.SellerInfo(r.Context()); err == nil {
		view.Announce = seller.Clean(info).Announce
	}
	view.Mark = template.URL("data:image/png;base64," + base64.StdEncoding.EncodeToString(look.Mark()))
	if view.Nonce, err = newNonce(); err != nil {
		http.Error(w, "страница статуса временно недоступна", http.StatusInternalServerError)
		return
	}
	var body bytes.Buffer
	if err := statusTemplate.Execute(&body, view); err != nil {
		http.Error(w, "страница статуса временно недоступна", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Language", text.Language)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'none'; style-src 'nonce-"+view.Nonce+"'; img-src data:; form-action 'none'; frame-ancestors 'none'; base-uri 'none'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Robots-Tag", "noindex, nofollow, noarchive")
	_, _ = w.Write(body.Bytes())
}
