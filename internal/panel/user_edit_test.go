package panel_test

import (
	"net/http"
	"testing"
)

// Срок можно снять так же явно, как назначить: продавец, подаривший доступ
// навсегда, не должен ставить дату через сто лет.
func TestClientCanBeMadeForeverAgain(t *testing.T) {
	h := newHarness(t)
	created := h.createUser(0)
	h.do(http.MethodPatch, "/api/v1/users/"+itoa(created.User.ID), adminToken, map[string]any{"expires_at": "30d"}, nil)
	if h.getUser(created.User.ID).ExpiresAt == nil {
		t.Fatal("срок не назначился")
	}
	if code := h.do(http.MethodPatch, "/api/v1/users/"+itoa(created.User.ID), adminToken, map[string]any{"no_expiry": true, "label": "Навсегда"}, nil); code != http.StatusOK {
		t.Fatalf("снятие срока: код %d", code)
	}
	got := h.getUser(created.User.ID)
	if got.ExpiresAt != nil || got.Label != "Навсегда" {
		t.Fatalf("после снятия срока: срок %v, имя %q", got.ExpiresAt, got.Label)
	}
	if code := h.do(http.MethodPatch, "/api/v1/users/"+itoa(created.User.ID), adminToken, map[string]any{"no_expiry": true, "expires_at": "30d"}, nil); code != http.StatusBadRequest {
		t.Fatalf("срок и бессрочно вместе приняты: код %d", code)
	}
}
