package panel_test

import (
	"bytes"
	"context"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jytt8u/marvia/internal/panel"
	"github.com/jytt8u/marvia/internal/users"
)

// Обещания docs/privacy.md, проверенные по самой базе и журналу, а не по
// коду, который в них пишет: код можно поменять и забыть про столбец.

// buyerIP — адрес покупателя, который не должен осесть нигде.
const buyerIP = "203.0.113.77"

// TestPanelSchemaHasNoPlaceForBuyerAddresses — в базе панели нет столбца,
// куда лёг бы адрес, устройство, почта или телефон покупателя.
//
// Проверяется по именам столбцов всех таблиц. Исключения перечислены с
// причиной: новый столбец с таким именем обязан пройти через этот список, то
// есть через осознанное решение, а не появиться между делом.
func TestPanelSchemaHasNoPlaceForBuyerAddresses(t *testing.T) {
	store, _, _ := usageStore(t)
	ctx := context.Background()

	allowed := map[string]string{
		"nodes.address":      "адрес сервера продавца, а не покупателя",
		"nodes.relay_ip":     "адрес входа каскада — сервера продавца, с которого тот сверяется с панелью",
		"nodes.country":      "страна ноды, её пишет продавец",
		"users.max_ips":      "лимит устройств — число, а не адреса",
		"plans.max_ips":      "лимит устройств в тарифе — тоже число",
		"users.devices_from": "когда продавец сбросил устройства — время, а не адреса и не устройства",
		"presence.ips":       "сколько адресов было за окно — число, сами адреса остаются на ноде в памяти",
	}
	suspicious := []string{"ip", "addr", "remote", "agent", "mail", "phone", "tel", "geo", "city", "country", "device", "imei", "mac"}

	for _, table := range store.Tables(ctx) {
		for _, column := range store.Columns(ctx, table) {
			name := table + "." + column
			if _, ok := allowed[name]; ok {
				continue
			}
			for _, word := range suspicious {
				if strings.Contains(strings.ToLower(column), word) {
					t.Errorf("столбец %s похож на данные о покупателе (%q): либо убрать, либо объяснить в списке исключений", name, word)
				}
			}
		}
	}
}

// TestBuyerAddressReachesNeitherDatabaseNorJournal — адрес покупателя,
// пришедшего за подпиской и с отчётом о нодах, не оседает ни в базе, ни в
// журнале панели.
//
// Адрес подставлен заголовками обратного прокси: за ним панель и стоит у
// большинства продавцов, и это ровно то место, где его соблазнительно
// «запомнить для статистики».
func TestBuyerAddressReachesNeitherDatabaseNorJournal(t *testing.T) {
	dir := t.TempDir()
	store, err := panel.Open(filepath.Join(dir, "panel.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()

	var journal bytes.Buffer
	was, flags := log.Writer(), log.Flags()
	log.SetOutput(&journal)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(was); log.SetFlags(flags) })

	user, _, err := store.CreateUser(ctx, panel.CreateUserParams{Label: "покупатель"})
	if err != nil {
		t.Fatal(err)
	}
	node, _, err := store.CreateNode(ctx, panel.CreateNodeParams{Name: "fi-1", Address: "198.51.100.1:443"})
	if err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(panel.NewAPI(store, adminToken, "https://sub.example.com", t.TempDir()).Handler())
	t.Cleanup(server.Close)

	send := func(method, path, body string) {
		t.Helper()
		req, err := http.NewRequest(method, server.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Forwarded-For", buyerIP)
		req.Header.Set("X-Real-IP", buyerIP)
		req.Header.Set("User-Agent", "Marvia/1.0 ("+buyerIP+")")
		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}
	send(http.MethodGet, "/sub/"+user.SubToken, "")
	send(http.MethodGet, "/sub/"+user.SubToken+"?format=json", "")
	send(http.MethodPost, "/sub/"+user.SubToken+"/report",
		`{"reports":[{"node_id":`+strconv.FormatInt(node.ID, 10)+`,"ok":true,"latency_ms":42}]}`)

	if strings.Contains(journal.String(), buyerIP) {
		t.Errorf("адрес покупателя в журнале панели: %q", journal.String())
	}

	// Всё, что панель держит на диске: сама база и её журнал WAL.
	_ = store.Close()
	files, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		raw, err := os.ReadFile(filepath.Join(dir, f.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(raw, []byte(buyerIP)) {
			t.Errorf("адрес покупателя лёг в %s", f.Name())
		}
	}
}

// TestAvailabilityReportStoresOnlyNodeSuccessAndLatency — от отчёта клиента
// о нодах в базе остаётся номер ноды, успех, задержка, время пробной порции,
// чей отчёт и когда.
//
// «Чей» нужен, чтобы один покупатель не мог накрутить жалобы количеством, —
// он заменяет свой прошлый отчёт, а не добавляет новый. Время порции — число
// про ноду, а не про человека: по нему видно, что ноду режут. Больше ничего:
// ни адреса, ни сети, ни устройства.
func TestAvailabilityReportStoresOnlyNodeSuccessAndLatency(t *testing.T) {
	store, _, _ := usageStore(t)
	got := strings.Join(store.Columns(context.Background(), "node_reports"), ",")
	if want := "node_id,user_id,ok,latency_ms,reported_at,fetch_ms"; got != want {
		t.Fatalf("в отчётах о доступности столбцы %s, обещаны %s", got, want)
	}
}

// TestAvailabilityReportsAreForgottenAfterTheWindow — отчёт о доступности
// живёт столько, сколько его читает сводка, и ни часом дольше.
//
// Раньше строка оставалась навсегда: сводка смотрит последние шесть часов, а
// в базе копилось «этот покупатель тогда-то пробовал эту ноду» за всё время.
func TestAvailabilityReportsAreForgottenAfterTheWindow(t *testing.T) {
	store, userID, nodeID := usageStore(t)
	ctx := context.Background()

	if err := store.SaveReports(ctx, userID, []panel.Report{{NodeID: nodeID, OK: true, LatencyMS: 40}}); err != nil {
		t.Fatal(err)
	}
	if err := store.Forget(ctx); err != nil {
		t.Fatal(err)
	}
	if n := store.Rows(ctx, "node_reports"); n != 1 {
		t.Fatalf("свежий отчёт убран раньше срока: строк %d", n)
	}

	if err := store.BackdateReports(ctx, 7*time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := store.Forget(ctx); err != nil {
		t.Fatal(err)
	}
	if n := store.Rows(ctx, "node_reports"); n != 0 {
		t.Fatalf("отчёт старше окна остался в базе: строк %d", n)
	}
}

// TestOldRecordsAreForgottenWithoutRestart — уборка по сроку идёт сама, а
// не только при запуске панели.
//
// Панель работает месяцами. Разовая чистка на старте означала бы, что
// обещанные сроки хранения соблюдаются лишь у тех, кто её часто перезапускает.
func TestOldRecordsAreForgottenWithoutRestart(t *testing.T) {
	store, userID, nodeID := usageStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	if err := store.SaveReports(ctx, userID, []panel.Report{{NodeID: nodeID, OK: false}}); err != nil {
		t.Fatal(err)
	}
	if err := store.BackdateReports(ctx, 7*time.Hour); err != nil {
		t.Fatal(err)
	}

	go store.KeepForgetting(ctx, 10*time.Millisecond, nil)

	deadline := time.Now().Add(5 * time.Second)
	for store.Rows(ctx, "node_reports") != 0 {
		if time.Now().After(deadline) {
			t.Fatal("старый отчёт так и лежит: уборка без перезапуска не идёт")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestPanelForgetsWhichNodeABuyerLeft — ушедший покупатель не оставляет
// строки «был на этой ноде тогда-то».
//
// Раньше на каждую ноду, где человек бывал, оставалось своё время последней
// связи — карта его переездов по странам. Теперь от ушедшего остаётся одно
// время у него самого, без ноды.
func TestPanelForgetsWhichNodeABuyerLeft(t *testing.T) {
	store, userID, nodeID := usageStore(t)
	ctx := context.Background()
	if err := store.TouchNode(ctx, nodeID); err != nil {
		t.Fatal(err)
	}

	presence(t, store, nodeID, userID, users.Presence{Conns: 1})
	report(t, store, nodeID, userID, 100, 100)
	presence(t, store, nodeID, userID, users.Presence{})

	if n := store.PresenceRows(ctx); n != 0 {
		t.Fatalf("после ухода осталось строк присутствия: %d", n)
	}
	u, err := store.GetUser(ctx, userID)
	if err != nil {
		t.Fatal(err)
	}
	if u.LastSeen == nil {
		t.Fatal("после ухода панель забыла, что он вообще был")
	}
}

// TestUpgradeFoldsPerNodeLastSeenIntoOne — обновление сворачивает прежнее
// время связи по нодам в одно и не теряет его.
func TestUpgradeFoldsPerNodeLastSeenIntoOne(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "panel.db")
	store, err := panel.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	user, _, err := store.CreateUser(ctx, panel.CreateUserParams{Label: "покупатель"})
	if err != nil {
		t.Fatal(err)
	}
	fi, _, err := store.CreateNode(ctx, panel.CreateNodeParams{Name: "fi", Address: "198.51.100.1:443"})
	if err != nil {
		t.Fatal(err)
	}
	tr, _, err := store.CreateNode(ctx, panel.CreateNodeParams{Name: "tr", Address: "198.51.100.2:443"})
	if err != nil {
		t.Fatal(err)
	}

	// Так их оставляла прежняя версия: строка на каждую ноду, где он бывал.
	for _, step := range []struct {
		node int64
		at   string
	}{{fi.ID, "2026-09-01T10:00:00Z"}, {tr.ID, "2026-09-14T10:00:00Z"}} {
		if err := store.Exec(ctx, `INSERT INTO presence (user_id, node_id, conns, ips, seen_at) VALUES (?, ?, 0, 0, ?)`,
			user.ID, step.node, step.at); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Exec(ctx, `UPDATE users SET last_seen = NULL`); err != nil {
		t.Fatal(err)
	}
	_ = store.Close()

	store, err = panel.Open(path)
	if err != nil {
		t.Fatalf("обновлённая панель не поднялась: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	if n := store.PresenceRows(ctx); n != 0 {
		t.Fatalf("после обновления остались строки по нодам: %d", n)
	}
	got, err := store.GetUser(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.LastSeen == nil || got.LastSeen.Format(time.RFC3339) != "2026-09-14T10:00:00Z" {
		t.Fatalf("время последней связи после обновления %v, ждали самое свежее из прежних", got.LastSeen)
	}
}

// TestUsageKeepsNoTimeOfLastUseOnANode — накопительный расход по ноде не
// хранит, когда покупатель там был в последний раз.
func TestUsageKeepsNoTimeOfLastUseOnANode(t *testing.T) {
	store, userID, nodeID := usageStore(t)
	ctx := context.Background()
	report(t, store, nodeID, userID, 100, 100)

	if n := store.Rows(ctx, "usage WHERE updated_at <> ''"); n != 0 {
		t.Fatalf("у расхода по ноде записано время: строк %d", n)
	}
}

// TestDeletedUserLeavesNoRememberedResponse — удаление покупателя уносит и
// сохранённый ответ на повтор оплаты.
//
// В этом ответе лежит сам покупатель: метка, ключ продавца, ссылки. Раньше он
// жил ещё неделю после удаления, и «удалили значит удалили» было неправдой.
func TestDeletedUserLeavesNoRememberedResponse(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	var created userView
	if code := h.withKey(http.MethodPost, "/api/v1/users", "оплата-1",
		map[string]any{"label": "Артём", "external_id": "tg-100500"}, &created); code != http.StatusOK {
		t.Fatalf("продажа: код %d", code)
	}
	path := "/api/v1/users/" + itoa(created.User.ID)
	if code := h.withKey(http.MethodPatch, path, "оплата-2", map[string]any{"extend_by": "30d"}, nil); code != http.StatusOK {
		t.Fatalf("продление: код %d", code)
	}
	if n := h.store.Rows(ctx, "idempotency"); n != 2 {
		t.Fatalf("ответов на повтор %d, ждали два — проверять нечего", n)
	}

	if code := h.do(http.MethodDelete, path, adminToken, nil, nil); code != http.StatusNoContent {
		t.Fatalf("удаление: код %d", code)
	}
	if n := h.store.Rows(ctx, "idempotency"); n != 0 {
		t.Fatalf("после удаления покупателя осталось сохранённых ответов: %d", n)
	}
}
