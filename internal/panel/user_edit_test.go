package panel_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jytt8u/marvia/internal/panel"
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

// Заморозка не крадёт и не дарит дни: сколько стоял — на столько и
// сдвигается срок.
func TestFrozenClientLosesNoDaysAndGetsNoneExtra(t *testing.T) {
	h := newHarness(t)
	created := h.createUser(0)
	id := created.User.ID
	h.do(http.MethodPatch, "/api/v1/users/"+itoa(id), adminToken, map[string]any{"expires_at": "30d"}, nil)
	before := *h.getUser(id).ExpiresAt

	ctx := context.Background()
	start := time.Now()
	if _, err := h.store.FreezeUser(ctx, id, start); err != nil {
		t.Fatal(err)
	}
	frozen := h.getUser(id)
	if frozen.Enabled || frozen.FrozenAt == nil {
		t.Fatalf("заморожен, но включён %v, пометка %v", frozen.Enabled, frozen.FrozenAt)
	}
	if _, err := h.store.FreezeUser(ctx, id, start); !errors.Is(err, panel.ErrAlreadyFrozen) {
		t.Fatalf("повторная заморозка: %v", err)
	}
	if _, err := h.store.UnfreezeUser(ctx, id, start.Add(10*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	after := h.getUser(id)
	if !after.Enabled || after.FrozenAt != nil {
		t.Fatalf("после разморозки включён %v, пометка %v", after.Enabled, after.FrozenAt)
	}
	if shift := after.ExpiresAt.Sub(before); absDur(shift-10*24*time.Hour) > time.Second {
		t.Fatalf("срок сдвинулся на %v, а стоял 10 дней", shift)
	}
}

// «Включить» и продление замороженного размораживают его: иначе пометка
// осталась бы, и следующая разморозка сдвинула бы срок второй раз.
func TestEnablingOrRenewingAFrozenClientUnfreezesHim(t *testing.T) {
	h := newHarness(t)
	plan := month(h)
	for _, how := range []string{"enable", "renew"} {
		var created createUserResponse
		h.do(http.MethodPost, "/api/v1/users", adminToken, map[string]any{"label": how, "plan_id": plan.ID}, &created)
		id := created.User.ID
		if code := h.do(http.MethodPost, "/api/v1/users/"+itoa(id)+"/freeze", adminToken, nil, nil); code != http.StatusOK {
			t.Fatalf("заморозка: код %d", code)
		}
		if how == "enable" {
			h.do(http.MethodPatch, "/api/v1/users/"+itoa(id), adminToken, map[string]any{"enabled": true}, nil)
		} else {
			h.do(http.MethodPost, "/api/v1/users/"+itoa(id)+"/renew", adminToken, map[string]any{"plan_id": plan.ID}, nil)
		}
		if got := h.getUser(id); !got.Enabled || got.FrozenAt != nil {
			t.Fatalf("%s: включён %v, пометка заморозки %v", how, got.Enabled, got.FrozenAt)
		}
		if code := h.do(http.MethodPost, "/api/v1/users/"+itoa(id)+"/unfreeze", adminToken, nil, nil); code != http.StatusConflict {
			t.Fatalf("%s: разморозка уже размороженного — код %d", how, code)
		}
	}
}

// Заметка продавца видна в панели, но не боту: «должен за прошлый месяц»
// не должно уезжать в чужие системы.
func TestSellerNoteStaysInThePanel(t *testing.T) {
	h := newHarness(t)
	created := h.createUser(0)
	id := created.User.ID
	h.do(http.MethodPatch, "/api/v1/users/"+itoa(id), adminToken, map[string]any{"note": "оплатил налом"}, nil)
	if got := h.getUser(id).Note; got != "оплатил налом" {
		t.Fatalf("админ видит заметку %q", got)
	}
	var key struct {
		Secret string `json:"secret"`
	}
	h.do(http.MethodPost, "/api/v1/keys", adminToken, map[string]any{"name": "бот", "scopes": []string{"users"}}, &key)
	var out struct {
		User panel.User `json:"user"`
	}
	h.do(http.MethodGet, "/api/v1/users/"+itoa(id), key.Secret, nil, &out)
	if out.User.Note != "" {
		t.Fatalf("бот видит заметку %q", out.User.Note)
	}
	long := strings.Repeat("я", 301)
	if code := h.do(http.MethodPatch, "/api/v1/users/"+itoa(id), adminToken, map[string]any{"note": long}, nil); code != http.StatusBadRequest {
		t.Fatalf("заметка на 301 знак принята: код %d", code)
	}
}

// «Сбросить устройства» доходит до ноды временем сброса — адресов у панели
// нет, и ноде она их не шлёт.
func TestDeviceResetReachesNodesAsATimeNotAddresses(t *testing.T) {
	h := newHarness(t)
	node := h.createNode("fi-1")
	created := h.createUser(0)
	id := created.User.ID
	h.do(http.MethodPatch, "/api/v1/users/"+itoa(id), adminToken, map[string]any{"max_ips": 2}, nil)
	if got := h.nodeUsers(node.Token)[0].DevicesFrom; !got.IsZero() {
		t.Fatalf("до сброса нода получила %v", got)
	}
	if code := h.do(http.MethodPost, "/api/v1/users/"+itoa(id)+"/reset-devices", adminToken, nil, nil); code != http.StatusOK {
		t.Fatalf("сброс устройств: код %d", code)
	}
	if got := h.nodeUsers(node.Token)[0].DevicesFrom; absDur(time.Since(got)) > time.Minute {
		t.Fatalf("нода получила время сброса %v", got)
	}
}
