package panel_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jytt8u/marvia/internal/panel"
)

func (h *harness) reportAltPorts(token string, ports []int) {
	h.t.Helper()
	h.do(http.MethodPost, "/api/v1/node/usage", token, map[string]any{"usage": map[string]any{}, "version": "v0.13.0", "alt_ports": ports}, nil)
}

// Продавец переводит подписки на запасной порт, который нода слушает; на
// порт, которого она не слушает, — нельзя: вести туда людей значит в пустоту.
func TestSubscriptionFollowsTheChosenPort(t *testing.T) {
	h := newHarness(t)
	node := h.createNode("fi-1")
	h.reportAltPorts(node.Token, []int{2053, 8880})
	created := h.createUser(0)
	if code := h.do(http.MethodPatch, "/api/v1/nodes/"+itoa(node.Node.ID), adminToken, map[string]any{"active_port": 2053}, nil); code != http.StatusOK {
		t.Fatalf("выбор порта: код %d", code)
	}
	if body := h.subJSON(created.User.SubToken); !strings.Contains(body, ":2053") {
		t.Fatalf("подписка не на запасном порту: %s", body)
	}
	if code := h.do(http.MethodPatch, "/api/v1/nodes/"+itoa(node.Node.ID), adminToken, map[string]any{"active_port": 9999}, nil); code != http.StatusBadRequest {
		t.Fatalf("порт, которого нода не слушает, принят: %d", code)
	}
	// Нода перестала слушать 2053 — выбор сам возвращается на основной.
	h.reportAltPorts(node.Token, []int{8880})
	if body := h.subJSON(created.User.SubToken); strings.Contains(body, ":2053") {
		t.Fatalf("подписка осталась на порту, которого нода больше не слушает: %s", body)
	}
}

// Живая нода, до которой покупатели не дозваниваются, переезжает на
// запасной порт сама, а в подписке появляется строка «обновите подписку».
// Повторно — не раньше чем через шесть часов, чтобы не гонять людей по кругу.
func TestBlockedPortSwitchesToTheSpareAndTellsPeople(t *testing.T) {
	h := newHarness(t)
	node := h.createNode("fi-1")
	h.reportAltPorts(node.Token, []int{2053})
	ctx := context.Background()
	var users []int64
	for i := 0; i < 3; i++ {
		u := h.createUser(0)
		users = append(users, u.User.ID)
		if err := h.store.SaveReports(ctx, u.User.ID, []panel.Report{{NodeID: node.Node.ID, OK: false}}); err != nil {
			t.Fatal(err)
		}
	}
	h.nodeUsers(node.Token) // нода на связи
	names, err := h.store.SwitchBlockedPorts(ctx, time.Now())
	if err != nil || len(names) != 1 {
		t.Fatalf("переключено %v, %v", names, err)
	}
	created := h.createUser(0)
	body := h.subJSON(created.User.SubToken)
	if !strings.Contains(body, ":2053") || !strings.Contains(body, "обновите подписку") {
		t.Fatalf("после переключения: %s", body)
	}
	for _, id := range users {
		_ = h.store.SaveReports(ctx, id, []panel.Report{{NodeID: node.Node.ID, OK: false}})
	}
	if again, _ := h.store.SwitchBlockedPorts(ctx, time.Now().Add(time.Hour)); len(again) != 0 {
		t.Fatalf("переключено снова через час: %v", again)
	}
}
