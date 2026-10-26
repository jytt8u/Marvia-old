package panel_test

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jytt8u/marvia/internal/users"
)

func (h *harness) subJSON(token string) string {
	h.t.Helper()
	resp, err := http.Get(h.server.URL + "/sub/" + token + "?format=json")
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return string(raw)
}

// Нода на обслуживании пропадает из подписок и перестаёт пускать — клиенты
// переезжают на живые, — а после окна возвращается сама.
func TestNodeInMaintenanceIsHiddenAndComesBack(t *testing.T) {
	h := newHarness(t)
	node := h.createNode("helsinki-1")
	created := h.createUser(0)
	window := map[string]any{"from": time.Now().Add(-time.Minute).UTC(), "until": time.Now().Add(time.Hour).UTC()}
	if code := h.do(http.MethodPatch, "/api/v1/nodes/"+itoa(node.Node.ID), adminToken, map[string]any{"maintenance": window}, nil); code != http.StatusOK {
		t.Fatalf("окно обслуживания: код %d", code)
	}
	if strings.Contains(h.subJSON(created.User.SubToken), "helsinki-1") {
		t.Fatal("нода на обслуживании осталась в подписке")
	}
	if code := h.do(http.MethodGet, "/api/v1/node/users", node.Token, nil, nil); code != http.StatusForbidden {
		t.Fatalf("нода на обслуживании получила список: код %d", code)
	}
	h.do(http.MethodPatch, "/api/v1/nodes/"+itoa(node.Node.ID), adminToken, map[string]any{"maintenance": map[string]any{"until": time.Time{}}}, nil)
	if !strings.Contains(h.subJSON(created.User.SubToken), "helsinki-1") {
		t.Fatal("после снятия окна нода не вернулась")
	}
	long := map[string]any{"from": time.Now().UTC(), "until": time.Now().Add(60 * 24 * time.Hour).UTC()}
	if code := h.do(http.MethodPatch, "/api/v1/nodes/"+itoa(node.Node.ID), adminToken, map[string]any{"maintenance": long}, nil); code != http.StatusBadRequest {
		t.Fatalf("окно на два месяца принято: код %d", code)
	}
}

// Нода группы видна и пускает только своих: «премиум» держится не на том,
// что ссылку не раздали.
func TestGroupNodeIsOnlyForItsGroup(t *testing.T) {
	h := newHarness(t)
	premium := h.createNode("premium-1")
	h.createNode("common-1")
	h.do(http.MethodPatch, "/api/v1/nodes/"+itoa(premium.Node.ID), adminToken, map[string]any{"groups": []string{"premium"}}, nil)
	plain := h.createUser(0)
	vip := h.createUser(0)
	h.do(http.MethodPatch, "/api/v1/users/"+itoa(vip.User.ID), adminToken, map[string]any{"node_group": "premium"}, nil)

	if body := h.subJSON(plain.User.SubToken); strings.Contains(body, "premium-1") || !strings.Contains(body, "common-1") {
		t.Fatalf("обычный видит: %s", body)
	}
	if body := h.subJSON(vip.User.SubToken); !strings.Contains(body, "premium-1") || !strings.Contains(body, "common-1") {
		t.Fatalf("премиум видит: %s", body)
	}
	list := h.nodeUsers(premium.Token)
	accounts := map[string]bool{}
	for _, u := range list {
		accounts[u.AccountID()] = true
	}
	if accounts[itoa(plain.User.ID)] || !accounts[itoa(vip.User.ID)] {
		t.Fatalf("премиум-нода пускает: %v", accounts)
	}
}

// Расход ноды за период хостера считается с дня сброса, и панель отдаёт его
// рядом с лимитом.
func TestHosterLimitShowsThisPeriodsUsage(t *testing.T) {
	h := newHarness(t)
	node := h.createNode("fi-1")
	created := h.createUser(0)
	h.do(http.MethodPatch, "/api/v1/nodes/"+itoa(node.Node.ID), adminToken, map[string]any{"monthly_limit": 1000 * gb, "limit_day": 1}, nil)
	account := h.nodeUsers(node.Token)[0].AccountID()
	h.do(http.MethodPost, "/api/v1/node/usage", node.Token, map[string]any{"usage": map[string]users.Usage{account: {Down: 30 * gb}}}, nil)
	_ = created
	var out struct {
		Nodes []struct {
			ID           int64 `json:"id"`
			MonthlyLimit int64 `json:"monthly_limit"`
			MonthUsed    int64 `json:"month_used"`
		} `json:"nodes"`
	}
	h.do(http.MethodGet, "/api/v1/nodes", adminToken, nil, &out)
	if len(out.Nodes) != 1 || out.Nodes[0].MonthlyLimit != 1000*gb || out.Nodes[0].MonthUsed != 30*gb {
		t.Fatalf("ноды: %+v", out.Nodes)
	}
	if code := h.do(http.MethodPatch, "/api/v1/nodes/"+itoa(node.Node.ID), adminToken, map[string]any{"limit_day": 31}, nil); code != http.StatusBadRequest {
		t.Fatalf("день сброса 31 принят: код %d", code)
	}
}

// Запасная нода не выходит в подписки, пока основная жива, — её адреса нет
// ни у кого, — и выходит сама, когда основная легла.
func TestStandbyNodeAppearsOnlyWhenItsPrimaryIsDown(t *testing.T) {
	h := newHarness(t)
	primary := h.createNode("main-1")
	standby := h.createNode("spare-1")
	h.nodeUsers(primary.Token) // основная на связи
	if code := h.do(http.MethodPatch, "/api/v1/nodes/"+itoa(standby.Node.ID), adminToken, map[string]any{"standby_for": primary.Node.ID}, nil); code != http.StatusOK {
		t.Fatalf("назначение запасной: код %d", code)
	}
	created := h.createUser(0)
	if body := h.subJSON(created.User.SubToken); strings.Contains(body, "spare-1") || !strings.Contains(body, "main-1") {
		t.Fatalf("при живой основной: %s", body)
	}
	if len(h.nodeUsers(standby.Token)) == 0 {
		t.Fatal("запасная не получает покупателей и не сможет принять их сразу")
	}
	h.do(http.MethodPatch, "/api/v1/nodes/"+itoa(primary.Node.ID), adminToken, map[string]any{"enabled": false}, nil)
	if body := h.subJSON(created.User.SubToken); !strings.Contains(body, "spare-1") {
		t.Fatalf("основная легла, а запасной в подписке нет: %s", body)
	}
	for _, bad := range []int64{standby.Node.ID} {
		if code := h.do(http.MethodPatch, "/api/v1/nodes/"+itoa(standby.Node.ID), adminToken, map[string]any{"standby_for": bad}, nil); code != http.StatusBadRequest {
			t.Fatalf("запасная для самой себя: код %d", code)
		}
	}
	other := h.createNode("x")
	if code := h.do(http.MethodPatch, "/api/v1/nodes/"+itoa(other.Node.ID), adminToken, map[string]any{"standby_for": standby.Node.ID}, nil); code != http.StatusBadRequest {
		t.Fatalf("запасная для запасной: код %d", code)
	}
}
