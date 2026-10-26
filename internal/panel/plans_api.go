package panel

import (
	"errors"
	"fmt"
	"net/http"
)

func (a *API) listPlans(w http.ResponseWriter, r *http.Request) {
	list, err := a.store.ListPlans(r.Context())
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	ok(w, map[string]any{"plans": list})
}

func (a *API) createPlan(w http.ResponseWriter, r *http.Request) {
	var p PlanParams
	if !decode(w, r, &p) {
		return
	}
	plan, err := a.store.CreatePlan(r.Context(), p)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	a.record(r, EventPlanCreate, Event{Detail: plan.Name})
	ok(w, map[string]any{"plan": plan})
}

func (a *API) updatePlan(w http.ResponseWriter, r *http.Request) {
	id, okID := pathID(w, r)
	if !okID {
		return
	}
	var p PlanParams
	if !decode(w, r, &p) {
		return
	}
	plan, err := a.store.UpdatePlan(r.Context(), id, p)
	if errors.Is(err, ErrNoSuchPlan) {
		fail(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	a.record(r, EventPlanUpdate, Event{Detail: plan.Name})
	ok(w, map[string]any{"plan": plan})
}

func (a *API) deletePlan(w http.ResponseWriter, r *http.Request) {
	id, okID := pathID(w, r)
	if !okID {
		return
	}
	plan, err := a.store.GetPlan(r.Context(), id)
	if err == nil {
		err = a.store.DeletePlan(r.Context(), id)
	}
	if errors.Is(err, ErrNoSuchPlan) {
		fail(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	a.record(r, EventPlanDelete, Event{Detail: plan.Name})
	ok(w, map[string]any{"deleted": true})
}

// renewUser продлевает покупателя по тарифу — то, что бот делает после
// каждой оплаты, а продавец — кнопкой «Продлить».
//
// Ключ идемпотентности тот же, что у продления через PATCH: платёжная
// система повторяет уведомление, пока бот не ответил, и без него одна
// оплата давала бы два месяца.
func (a *API) renewUser(w http.ResponseWriter, r *http.Request) {
	id, okID := pathID(w, r)
	if !okID {
		return
	}
	var body struct {
		PlanID int64 `json:"plan_id"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.PlanID == 0 {
		fail(w, http.StatusBadRequest, "не указан тариф: plan_id")
		return
	}
	scope := fmt.Sprintf("renew:%d", id)
	if handled := a.replayed(w, r, scope); handled {
		return
	}

	user, err := a.store.RenewUser(r.Context(), id, body.PlanID)
	if errors.Is(err, ErrNoSuchPlan) {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if errors.Is(err, ErrTrialRenew) {
		fail(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		respondStoreErr(w, err)
		return
	}
	plan, _ := a.store.GetPlan(r.Context(), body.PlanID)
	user = visible(r, []User{user})[0]
	a.remember(r, scope, user.ID, map[string]any{"user": user})
	a.record(r, EventUserRenew, Event{UserID: user.ID, Detail: plan.Name})
	ok(w, map[string]any{"user": user})
}

// resetTraffic обнуляет расход покупателя — когда продавец дарит трафик или
// исправляет ошибку, не меняя срок.
func (a *API) resetTraffic(w http.ResponseWriter, r *http.Request) {
	id, okID := pathID(w, r)
	if !okID {
		return
	}
	user, err := a.store.ResetTraffic(r.Context(), id)
	if err != nil {
		respondStoreErr(w, err)
		return
	}
	a.record(r, EventUserReset, Event{UserID: user.ID})
	ok(w, map[string]any{"user": visible(r, []User{user})[0]})
}
