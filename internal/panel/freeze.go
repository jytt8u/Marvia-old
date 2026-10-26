package panel

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// Заморозка, сброс устройств и заметка — то, что продавец делает с одним
// покупателем руками, не трогая тариф.

// ErrAlreadyFrozen — покупатель уже заморожен.
var ErrAlreadyFrozen = errors.New("покупатель уже заморожен")

// ErrNotFrozen — покупатель не заморожен.
var ErrNotFrozen = errors.New("покупатель не заморожен")

// maxNote — сколько знаков в заметке продавца. Заметка — пара слов
// («оплатил налом», «друг Пети»), а не досье: панель не CRM, и копить в ней
// подробности о людях незачем.
const maxNote = 300

// ErrNoteTooLong — заметка длиннее maxNote.
var ErrNoteTooLong = fmt.Errorf("заметка длиннее %d знаков", maxNote)

// FreezeUser приостанавливает доступ с сохранением оставшихся дней.
//
// Отключить можно и кнопкой «Отключить», но тогда срок идёт дальше: клиент,
// уехавший на месяц, возвращается к доступу, который кончился. Заморозка
// запоминает, когда остановили, и при разморозке сдвигает срок на столько
// же — ни дня не теряется и ни дня не дарится.
func (s *Store) FreezeUser(ctx context.Context, id int64, now time.Time) (User, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE users SET frozen_at = ?, enabled = 0 WHERE id = ? AND frozen_at IS NULL`, format(now.UTC()), id)
	if err != nil {
		return User{}, fmt.Errorf("заморозка: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if _, err := s.GetUser(ctx, id); err != nil {
			return User{}, err
		}
		return User{}, ErrAlreadyFrozen
	}
	return s.GetUser(ctx, id)
}

// UnfreezeUser возвращает доступ и сдвигает срок на время заморозки.
func (s *Store) UnfreezeUser(ctx context.Context, id int64, now time.Time) (User, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, err
	}
	defer func() { _ = tx.Rollback() }()
	was, err := unfreezeTx(ctx, tx, id, now)
	if err != nil {
		return User{}, err
	}
	if !was {
		return User{}, ErrNotFrozen
	}
	if err := tx.Commit(); err != nil {
		return User{}, err
	}
	return s.GetUser(ctx, id)
}

// unfreezeTx размораживает внутри чужой транзакции и говорит, был ли
// покупатель заморожен. Её зовут все пути, которые включают покупателя —
// «Включить», продление по тарифу, разморозка: включённый мимо неё остался
// бы с пометкой заморозки, и следующая разморозка сдвинула бы срок второй
// раз.
func unfreezeTx(ctx context.Context, tx *sql.Tx, id int64, now time.Time) (bool, error) {
	var frozen, expires sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT frozen_at, expires_at FROM users WHERE id = ?`, id).Scan(&frozen, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrNotFound
	}
	if err != nil {
		return false, err
	}
	if !frozen.Valid {
		return false, nil
	}
	var until any
	if e := parseNullTime(expires); e != nil {
		paused := now.UTC().Sub(parse(frozen.String))
		if paused < 0 {
			paused = 0
		}
		until = format(e.Add(paused))
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE users SET frozen_at = NULL, enabled = 1, expires_at = ? WHERE id = ?`, until, id); err != nil {
		return false, fmt.Errorf("разморозка: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM webhook_state WHERE key = ?`, fmt.Sprintf("%s:%d", WebhookUserExpired, id)); err != nil {
		return false, err
	}
	return true, nil
}

// ResetDevices освобождает лимит устройств: адреса, с которых покупатель
// приходил до этой минуты, ноды перестают считать.
//
// Человек сменил телефон или переехал с Wi-Fi на мобильную сеть — и упёрся
// в лимит, пока старые адреса не забудутся сами через час. Ноде панель шлёт
// только время сброса, не адреса: их у панели и нет.
func (s *Store) ResetDevices(ctx context.Context, id int64, now time.Time) (User, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE users SET devices_from = ? WHERE id = ?`, format(now.UTC()), id)
	if err != nil {
		return User{}, fmt.Errorf("сброс устройств: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return User{}, ErrNotFound
	}
	return s.GetUser(ctx, id)
}

func (a *API) freezeUser(w http.ResponseWriter, r *http.Request) {
	a.userAction(w, r, EventUserFreeze, func(id int64) (User, error) { return a.store.FreezeUser(r.Context(), id, time.Now()) })
}

func (a *API) unfreezeUser(w http.ResponseWriter, r *http.Request) {
	a.userAction(w, r, EventUserUnfreeze, func(id int64) (User, error) { return a.store.UnfreezeUser(r.Context(), id, time.Now()) })
}

func (a *API) resetDevices(w http.ResponseWriter, r *http.Request) {
	a.userAction(w, r, EventUserDevices, func(id int64) (User, error) { return a.store.ResetDevices(r.Context(), id, time.Now()) })
}

// userAction — общий путь простых действий над покупателем: номер из пути,
// действие, запись в журнал, ответ с покупателем.
func (a *API) userAction(w http.ResponseWriter, r *http.Request, event string, do func(int64) (User, error)) {
	id, okID := pathID(w, r)
	if !okID {
		return
	}
	user, err := do(id)
	if errors.Is(err, ErrAlreadyFrozen) || errors.Is(err, ErrNotFrozen) {
		fail(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		respondStoreErr(w, err)
		return
	}
	a.record(r, event, Event{UserID: user.ID})
	ok(w, map[string]any{"user": visible(r, []User{user})[0]})
}
