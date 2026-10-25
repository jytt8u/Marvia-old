package panel_test

import (
	"net/http"
	"testing"

	"github.com/jytt8u/marvia/internal/panel"
)

type nodeGuardResponse struct {
	Guard panel.GuardSettings `json:"guard"`
}

// Продавец ужесточил пределы и закрыл сеть — ноды получают это со
// следующей синхронизацией. Сброс на значения по умолчанию возвращает нодам
// обычные пределы, а не оставляет прежние строгие.
func TestSellerFloodLimitsReachNodesOnNextSync(t *testing.T) {
	h := newHarness(t)
	node := h.createNode("fi-1")
	if code := h.do(http.MethodPut, "/api/v1/guard", adminToken, map[string]any{
		"rate": 5, "burst": 50, "max_open": 5000, "blocked": []string{" 203.0.113.7 ", "198.51.100.0/24", "203.0.113.7"},
	}, nil); code != http.StatusOK {
		t.Fatalf("сохранение защиты: код %d", code)
	}
	var got nodeGuardResponse
	h.do(http.MethodGet, "/api/v1/node/users", node.Token, nil, &got)
	g := got.Guard
	if g.Rate != 5 || g.Burst != 50 || g.MaxOpen != 5000 {
		t.Fatalf("нода получила пределы %+v", g)
	}
	if len(g.Blocked) != 2 || g.Blocked[0] != "203.0.113.7" || g.Blocked[1] != "198.51.100.0/24" {
		t.Fatalf("закрытые адреса у ноды: %v", g.Blocked)
	}

	h.do(http.MethodPut, "/api/v1/guard", adminToken, map[string]any{}, nil)
	h.do(http.MethodGet, "/api/v1/node/users", node.Token, nil, &got)
	if d := panel.GuardDefaults; got.Guard.Rate != d.Rate || got.Guard.Burst != d.Burst || got.Guard.MaxOpen != d.MaxOpen || len(got.Guard.Blocked) != 0 {
		t.Fatalf("после сброса нода получила %+v", got.Guard)
	}
}

// Опечатка не должна запереть ноду от своих же покупателей: сеть на полмира
// и частота меньше одного соединения в секунду не принимаются.
func TestFloodSettingsThatWouldLockOutBuyersAreRefused(t *testing.T) {
	h := newHarness(t)
	for _, body := range []map[string]any{
		{"blocked": []string{"0.0.0.0/0"}},
		{"blocked": []string{"2000::/3"}},
		{"blocked": []string{"не адрес"}},
		{"rate": 0.5},
		{"burst": 2},
		{"max_open": 10},
	} {
		if code := h.do(http.MethodPut, "/api/v1/guard", adminToken, body, nil); code != http.StatusBadRequest {
			t.Errorf("%v принято: код %d", body, code)
		}
	}
}

// Пределы нод решают, кто вообще дозвонится, — менять их может только
// админ, не бот.
func TestBotKeyCannotTouchFloodProtection(t *testing.T) {
	h := newHarness(t)
	var key struct {
		Secret string `json:"secret"`
	}
	h.do(http.MethodPost, "/api/v1/keys", adminToken, map[string]any{"name": "бот", "scopes": []string{"users", "nodes"}}, &key)
	if code := h.do(http.MethodPut, "/api/v1/guard", key.Secret, map[string]any{"blocked": []string{"203.0.113.7"}}, nil); code != http.StatusForbidden {
		t.Fatalf("бот поменял защиту: код %d", code)
	}
}

// Нода сообщает, сколько отбила, и панель показывает это по нодам за сутки;
// тихая нода — с нулями, а не пропадает из списка.
func TestPanelShowsWhatEachNodeRejectedToday(t *testing.T) {
	h := newHarness(t)
	loud, quiet := h.createNode("fi-1"), h.createNode("nl-1")
	for range 2 {
		h.do(http.MethodPost, "/api/v1/node/usage", loud.Token, map[string]any{
			"usage": map[string]any{}, "guard": map[string]any{"by_rate": 100, "by_open": 1, "by_block": 7},
		}, nil)
	}
	var out struct {
		Day []panel.GuardDay `json:"day"`
	}
	if code := h.do(http.MethodGet, "/api/v1/guard", adminToken, nil, &out); code != http.StatusOK {
		t.Fatalf("чтение защиты: код %d", code)
	}
	byID := map[int64]panel.GuardDay{}
	for _, d := range out.Day {
		byID[d.NodeID] = d
	}
	if d := byID[loud.Node.ID]; d.ByRate != 200 || d.ByOpen != 2 || d.ByBlock != 14 {
		t.Fatalf("у шумной ноды %+v", d)
	}
	if d, ok := byID[quiet.Node.ID]; !ok || d.ByRate+d.ByOpen+d.ByBlock != 0 {
		t.Fatalf("тихая нода: есть %v, %+v", ok, d)
	}
}
