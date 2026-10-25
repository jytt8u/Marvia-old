package panel_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/jytt8u/marvia/internal/panel"
	"github.com/jytt8u/marvia/internal/users"
)

func yearMonthly(h *harness) panel.Plan {
	return h.createPlan(map[string]any{
		"name": "Год, 50 ГБ в месяц", "days": 365, "traffic_limit": 50 * gb, "traffic_reset": "month",
	})
}

func (h *harness) spend(node createNodeResponse, bytes int64) {
	h.t.Helper()
	account := h.nodeUsers(node.Token)[0].AccountID()
	h.do(http.MethodPost, "/api/v1/node/usage", node.Token,
		map[string]any{"usage": map[string]users.Usage{account: {Down: bytes}}}, nil)
}

func (h *harness) resetDue(at time.Time) []int64 {
	h.t.Helper()
	ids, err := h.store.ResetDue(context.Background(), at)
	if err != nil {
		h.t.Fatal(err)
	}
	return ids
}

func TestYearlyPlanWithMonthlyResetGivesFullQuotaEveryMonth(t *testing.T) {
	h := newHarness(t)
	plan := yearMonthly(h)
	if plan.TrafficReset != panel.ResetMonthly {
		t.Fatalf("тариф запомнил сброс %q", plan.TrafficReset)
	}
	node := h.createNode("fi-1")
	var created createUserResponse
	h.do(http.MethodPost, "/api/v1/users", adminToken, map[string]any{"label": "Ира", "plan_id": plan.ID}, &created)
	first := created.User.TrafficResetAt
	if first == nil || absDur(first.Sub(time.Now().AddDate(0, 1, 0))) > 3*24*time.Hour {
		t.Fatalf("первый сброс назначен на %v, ожидался через месяц", first)
	}

	h.spend(node, 40*gb)
	if ids := h.resetDue(first.Add(-time.Hour)); len(ids) != 0 {
		t.Fatalf("сброшено раньше срока: %v", ids)
	}
	if used := h.getUser(created.User.ID).Used; used != 40*gb {
		t.Fatalf("до дня сброса расход %d", used)
	}

	if ids := h.resetDue(first.Add(time.Minute)); len(ids) != 1 || ids[0] != created.User.ID {
		t.Fatalf("в день сброса сброшены %v", ids)
	}
	after := h.getUser(created.User.ID)
	if after.Used != 0 {
		t.Fatalf("после сброса расход %d", after.Used)
	}
	if after.TrafficResetAt == nil || !after.TrafficResetAt.After(*first) {
		t.Fatalf("следующий сброс %v не позже прошлого %v", after.TrafficResetAt, first)
	}
	// Срок от сброса не меняется — это не продление.
	if !after.ExpiresAt.Equal(*created.User.ExpiresAt) {
		t.Fatalf("сброс сдвинул срок: было %v, стало %v", created.User.ExpiresAt, after.ExpiresAt)
	}
	// Нода уже насчитала 40 ГБ своим счётчиком и должна пустить ещё на 50.
	if got := h.nodeUsers(node.Token)[0]; !got.Enabled || got.TrafficLimit != 90*gb {
		t.Fatalf("нода после сброса: включён %v, лимит %d", got.Enabled, got.TrafficLimit)
	}
}

func TestPanelThatSleptThreeMonthsResetsOnceNotThrice(t *testing.T) {
	h := newHarness(t)
	plan := yearMonthly(h)
	var created createUserResponse
	h.do(http.MethodPost, "/api/v1/users", adminToken, map[string]any{"label": "Олег", "plan_id": plan.ID}, &created)

	woke := time.Now().AddDate(0, 3, 10)
	if ids := h.resetDue(woke); len(ids) != 1 {
		t.Fatalf("после трёх месяцев сброшено %d раз, ожидался один", len(ids))
	}
	if ids := h.resetDue(woke); len(ids) != 0 {
		t.Fatalf("повторный проход сбросил ещё раз: %v", ids)
	}
	if next := h.getUser(created.User.ID).TrafficResetAt; next == nil || !next.After(woke) {
		t.Fatalf("следующий сброс %v не после пробуждения %v", next, woke)
	}
}

func TestResetDayStaysOnTheThirtyFirstAfterShortMonths(t *testing.T) {
	msk := time.FixedZone("MSK", 3*60*60)
	anchor := time.Date(2027, time.January, 31, 12, 0, 0, 0, msk)
	feb := panel.NextReset(anchor, anchor)
	if want := time.Date(2027, time.February, 28, 12, 0, 0, 0, msk); !feb.Equal(want) {
		t.Fatalf("после 31 января сброс %v, ожидалось 28 февраля", feb.In(msk))
	}
	mar := panel.NextReset(anchor, feb)
	if want := time.Date(2027, time.March, 31, 12, 0, 0, 0, msk); !mar.Equal(want) {
		t.Fatalf("после февраля сброс %v, ожидалось 31 марта", mar.In(msk))
	}
}

func TestRenewByPlanWithoutResetStopsMonthlyReset(t *testing.T) {
	h := newHarness(t)
	yearly, plain := yearMonthly(h), month(h)
	var created createUserResponse
	h.do(http.MethodPost, "/api/v1/users", adminToken, map[string]any{"label": "Нина", "plan_id": yearly.ID}, &created)
	if code := h.do(http.MethodPost, "/api/v1/users/"+itoa(created.User.ID)+"/renew", adminToken,
		map[string]any{"plan_id": plain.ID}, nil); code != http.StatusOK {
		t.Fatalf("продление: код %d", code)
	}
	if got := h.getUser(created.User.ID).TrafficResetAt; got != nil {
		t.Fatalf("тариф без сброса оставил сброс на %v", got)
	}
	if ids := h.resetDue(time.Now().AddDate(1, 0, 0)); len(ids) != 0 {
		t.Fatalf("сброшены по старому тарифу: %v", ids)
	}
}

func TestExpiredClientIsNotResetButHisScheduleMovesOn(t *testing.T) {
	h := newHarness(t)
	plan := yearMonthly(h)
	node := h.createNode("fi-1")
	var created createUserResponse
	h.do(http.MethodPost, "/api/v1/users", adminToken, map[string]any{"label": "Пётр", "plan_id": plan.ID}, &created)
	h.spend(node, 10*gb)
	past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	h.do(http.MethodPatch, "/api/v1/users/"+itoa(created.User.ID), adminToken, map[string]any{"expires_at": past}, nil)

	at := created.User.TrafficResetAt.Add(time.Minute)
	if ids := h.resetDue(at); len(ids) != 0 {
		t.Fatalf("истёкший сброшен: %v", ids)
	}
	got := h.getUser(created.User.ID)
	if got.Used != 10*gb {
		t.Fatalf("у истёкшего расход %d", got.Used)
	}
	if got.TrafficResetAt == nil || !got.TrafficResetAt.After(at) {
		t.Fatalf("день сброса истёкшего не перенесён: %v", got.TrafficResetAt)
	}
}

func TestPlanRefusesResetPanelCannotDo(t *testing.T) {
	h := newHarness(t)
	code := h.do(http.MethodPost, "/api/v1/plans", adminToken,
		map[string]any{"name": "Неделя", "days": 7, "traffic_reset": "week"}, nil)
	if code != http.StatusBadRequest {
		t.Fatalf("незнакомый сброс принят: код %d", code)
	}
}
