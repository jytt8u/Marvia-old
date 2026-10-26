package panel_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/jytt8u/marvia/internal/panel"
)

func (h *harness) promo(body map[string]any) panel.Promo {
	h.t.Helper()
	var out struct {
		Promo panel.Promo `json:"promo"`
	}
	if code := h.do(http.MethodPost, "/api/v1/promos", adminToken, body, &out); code != http.StatusOK {
		h.t.Fatalf("промокод: код %d", code)
	}
	return out.Promo
}

func (h *harness) redeem(userID int64, code string) int {
	h.t.Helper()
	return h.do(http.MethodPost, "/api/v1/users/"+itoa(userID)+"/promo", adminToken, map[string]any{"code": code}, nil)
}

// Промокод даёт дни и трафик один раз на человека: сто нажатий кнопки в
// боте не превращают «+7 дней» в вечный доступ.
func TestPromoGivesItsBonusOncePerClient(t *testing.T) {
	h := newHarness(t)
	h.promo(map[string]any{"code": "LETO7", "days": 7, "traffic": 10 * gb})
	created := h.createUser(50 * gb)
	id := created.User.ID
	h.do(http.MethodPatch, "/api/v1/users/"+itoa(id), adminToken, map[string]any{"expires_at": "30d"}, nil)
	before := h.getUser(id)

	if code := h.redeem(id, "leto7"); code != http.StatusOK {
		t.Fatalf("код без учёта регистра не принят: %d", code)
	}
	after := h.getUser(id)
	if absDur(after.ExpiresAt.Sub(*before.ExpiresAt)-7*24*time.Hour) > time.Second || after.TrafficLimit != 60*gb {
		t.Fatalf("после кода: срок %v → %v, лимит %d", before.ExpiresAt, after.ExpiresAt, after.TrafficLimit)
	}
	if code := h.redeem(id, "LETO7"); code != http.StatusConflict {
		t.Fatalf("повторный код принят: %d", code)
	}
}

// Код с пределом использований и сроком перестаёт работать, а бессрочный
// и безлимитный клиент от «+дней» и «+гигабайт» не становится ограниченным.
func TestPromoLimitsHoldAndForeverStaysForever(t *testing.T) {
	h := newHarness(t)
	h.promo(map[string]any{"code": "ONE", "days": 3, "traffic": gb, "max_uses": 1})
	first, second := h.createUser(0), h.createUser(0)
	if code := h.redeem(first.User.ID, "ONE"); code != http.StatusOK {
		t.Fatalf("первое применение: %d", code)
	}
	if got := h.getUser(first.User.ID); got.ExpiresAt != nil || got.TrafficLimit != 0 {
		t.Fatalf("бессрочный безлимит после кода: срок %v, лимит %d", got.ExpiresAt, got.TrafficLimit)
	}
	if code := h.redeem(second.User.ID, "ONE"); code != http.StatusNotFound {
		t.Fatalf("код сверх предела принят: %d", code)
	}
	h.promo(map[string]any{"code": "OLD", "days": 3, "expires_at": time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)})
	if code := h.redeem(second.User.ID, "OLD"); code != http.StatusNotFound {
		t.Fatalf("истёкший код принят: %d", code)
	}
}

// Пробный — один раз на человека, даже если продавец удалил его и завёл
// заново; без ключа продавца пробный не выдаётся вовсе.
func TestTrialIsOncePerPersonEvenAfterDeletion(t *testing.T) {
	h := newHarness(t)
	trial := h.createPlan(map[string]any{"name": "Пробный", "days": 3, "traffic_limit": 5 * gb, "trial": true})
	var created createUserResponse
	if code := h.do(http.MethodPost, "/api/v1/users", adminToken, map[string]any{"label": "a", "external_id": "tg:1", "plan_id": trial.ID}, &created); code != http.StatusOK {
		t.Fatalf("первый пробный: %d", code)
	}
	h.do(http.MethodDelete, "/api/v1/users/"+itoa(created.User.ID), adminToken, nil, nil)
	if code := h.do(http.MethodPost, "/api/v1/users", adminToken, map[string]any{"label": "a", "external_id": "tg:1", "plan_id": trial.ID}, nil); code != http.StatusConflict {
		t.Fatalf("второй пробный после удаления: %d", code)
	}
	if code := h.do(http.MethodPost, "/api/v1/users", adminToken, map[string]any{"label": "b", "plan_id": trial.ID}, nil); code != http.StatusBadRequest {
		t.Fatalf("пробный без ключа продавца: %d", code)
	}
	var other createUserResponse
	h.do(http.MethodPost, "/api/v1/users", adminToken, map[string]any{"label": "c", "external_id": "tg:2", "plan_id": trial.ID}, &other)
	if code := h.do(http.MethodPost, "/api/v1/users/"+itoa(other.User.ID)+"/renew", adminToken, map[string]any{"plan_id": trial.ID}, nil); code != http.StatusConflict {
		t.Fatalf("продление пробным: %d", code)
	}
}

// Пригласивший получает дни за платного друга, но не за пробного: иначе
// любой копил бы дни, заводя себе «друзей» пробными.
func TestReferrerIsRewardedForPaidFriendsOnly(t *testing.T) {
	h := newHarness(t)
	h.do(http.MethodPut, "/api/v1/referral", adminToken, map[string]any{"days": 5}, nil)
	paid := month(h)
	trial := h.createPlan(map[string]any{"name": "Пробный", "days": 3, "trial": true})
	var host createUserResponse
	h.do(http.MethodPost, "/api/v1/users", adminToken, map[string]any{"label": "host", "external_id": "tg:10", "plan_id": paid.ID}, &host)
	before := *h.getUser(host.User.ID).ExpiresAt

	h.do(http.MethodPost, "/api/v1/users", adminToken, map[string]any{"label": "trial friend", "external_id": "tg:11", "plan_id": trial.ID, "referrer": "tg:10"}, nil)
	if got := *h.getUser(host.User.ID).ExpiresAt; !got.Equal(before) {
		t.Fatalf("за пробного друга добавлено %v", got.Sub(before))
	}
	var friend createUserResponse
	h.do(http.MethodPost, "/api/v1/users", adminToken, map[string]any{"label": "friend", "external_id": "tg:12", "plan_id": paid.ID, "referrer": "tg:10"}, &friend)
	got := h.getUser(host.User.ID)
	if absDur(got.ExpiresAt.Sub(before)-5*24*time.Hour) > time.Second {
		t.Fatalf("за платного друга добавлено %v, ждали 5 дней", got.ExpiresAt.Sub(before))
	}
	if got.Referrals != 2 {
		t.Fatalf("привёл %d, ждали 2", got.Referrals)
	}
	if f := h.getUser(friend.User.ID); f.ReferredBy == nil || *f.ReferredBy != host.User.ID {
		t.Fatalf("у друга не записано, кто привёл: %v", f.ReferredBy)
	}
	// Незнакомый пригласивший продажу не ломает.
	if code := h.do(http.MethodPost, "/api/v1/users", adminToken, map[string]any{"label": "x", "external_id": "tg:13", "plan_id": paid.ID, "referrer": "tg:999"}, nil); code != http.StatusOK {
		t.Fatalf("продажа с незнакомым пригласившим: %d", code)
	}
}
