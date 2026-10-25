package panel

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"time"
)

// Ежемесячный сброс трафика.
//
// «Год, 50 ГБ в месяц» без него не продать: квота на весь срок кончилась бы
// в феврале, а обнулять её каждому руками в день покупки продавец забудет
// уже на третьем покупателе. Сброс трогает только расход — срок, лимиты и
// доступ остаются как были.
//
// Расписание живёт у покупателя, а не у тарифа: правка тарифа уже
// проданных не меняет (см. plans.go), и сброс тоже не должен.

const (
	// ResetNever — расход обнуляется только продлением или кнопкой.
	ResetNever = ""
	// ResetMonthly — каждый месяц в тот же день, что куплено или продлено.
	ResetMonthly = "month"
)

// resetEvery — как часто искать, кому пора. Сброс на десять минут позже
// дня покупки никто не заметит, а каждую минуту гонять запрос по всем
// покупателям незачем.
const resetEvery = 10 * time.Minute

// ActorSchedule — от чьего имени сброс по расписанию пишет в журнал. Не
// ActorAuto: тот подписан «автообновление», и сброс выглядел бы его делом.
const ActorSchedule = "schedule"

// resetSchedule — начало отсчёта и первый сброс для покупателя, купившего
// или продлившего в now. Для тарифа без сброса оба NULL: так и хранится
// «по расписанию не сбрасывать».
func resetSchedule(kind string, now time.Time) (anchor, next any) {
	if kind != ResetMonthly {
		return nil, nil
	}
	// Секунды без долей: время в базе сравнивается строкой, а у RFC3339Nano
	// без долей и с долями порядок символов не совпадает с порядком времени.
	from := now.UTC().Truncate(time.Second)
	return format(from), format(nextReset(from, from))
}

// nextReset — ближайший после after день сброса для отсчёта от anchor.
//
// Считается от начала отсчёта, а не от прошлого сброса: купивший 31 января
// получает сброс 28 февраля и снова 31 марта. Прибавляй месяц к прошлому
// сбросу — с февраля он съехал бы на 28-е навсегда, а Go ещё и перенёс бы
// «31 февраля» на 3 марта. Календарь — московский: покупка в час ночи по
// Москве — это 1-е число для продавца, хотя по UTC ещё 31-е.
func nextReset(anchor, after time.Time) time.Time {
	a := anchor.In(moscow)
	// Предел — чтобы испорченная дата в базе не крутила цикл вечно: двести
	// лет месячных периодов хватит любому тарифу.
	for k := 1; k <= 12*200; k++ {
		t := addMonthsClamped(a, k)
		if t.After(after) {
			return t.UTC()
		}
	}
	return after.Add(30 * 24 * time.Hour).UTC()
}

// addMonthsClamped — тот же день через k месяцев, а если такого дня нет —
// последний день месяца.
func addMonthsClamped(t time.Time, k int) time.Time {
	first := time.Date(t.Year(), t.Month()+time.Month(k), 1, t.Hour(), t.Minute(), t.Second(), 0, t.Location())
	last := first.AddDate(0, 1, -1).Day()
	day := t.Day()
	if day > last {
		day = last
	}
	return time.Date(first.Year(), first.Month(), day, t.Hour(), t.Minute(), t.Second(), 0, t.Location())
}

// ResetDue обнуляет расход тем, у кого к now настал день сброса, и
// возвращает их номера.
//
// Панель, пролежавшая выключенной три месяца, сбрасывает один раз, а не
// три: расход за эти месяцы — уже один период, и тройной сброс ничего бы
// не дал. Следующий день сброса — первый после now.
//
// Истёкшим срок сброса переносится без самого сброса: доступа у них всё
// равно нет, а журнал не должен годами копить ежемесячное «сброшено» у
// покупателей, ушедших год назад. Продлят — продление обнулит расход само.
func (s *Store) ResetDue(ctx context.Context, now time.Time) ([]int64, error) {
	stamp := format(now.UTC().Truncate(time.Second))
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, reset_anchor, expires_at FROM users
		WHERE reset_at IS NOT NULL AND reset_at <= ? AND reset_anchor IS NOT NULL`, stamp)
	if err != nil {
		return nil, fmt.Errorf("поиск сбросов: %w", err)
	}
	type due struct {
		id      int64
		anchor  time.Time
		expired bool
	}
	var list []due
	for rows.Next() {
		var (
			d       due
			anchor  string
			expires sql.NullString
		)
		if err := rows.Scan(&d.id, &anchor, &expires); err != nil {
			rows.Close()
			return nil, err
		}
		d.anchor = parse(anchor)
		if e := parseNullTime(expires); e != nil && !e.After(now) {
			d.expired = true
		}
		list = append(list, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var done []int64
	for _, d := range list {
		ok, err := s.resetOne(ctx, d.id, !d.expired, format(nextReset(d.anchor, now)), stamp)
		if err != nil {
			return done, err
		}
		if ok && !d.expired {
			done = append(done, d.id)
		}
	}
	return done, nil
}

// resetOne переносит день сброса и, если wipe, обнуляет расход — в одной
// транзакции, чтобы упавшая посередине панель не сбросила дважды.
//
// Условие reset_at <= stamp повторяется в UPDATE: продление между поиском
// и сбросом уже поставило новый отсчёт, и сбрасывать поверх него нельзя.
func (s *Store) resetOne(ctx context.Context, id int64, wipe bool, next, stamp string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	set := `reset_at = ?`
	if wipe {
		set = `traffic_offset = ` + usedTotalSQL + `, reset_at = ?`
	}
	res, err := tx.ExecContext(ctx, `UPDATE users SET `+set+` WHERE id = ? AND reset_at IS NOT NULL AND reset_at <= ?`, next, id, stamp)
	if err != nil {
		return false, fmt.Errorf("сброс трафика: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, nil
	}
	if wipe {
		if _, err := tx.ExecContext(ctx, `DELETE FROM webhook_state WHERE user_id = ? AND key IN (?, ?)`, id, fmt.Sprintf("%s:%d", WebhookTrafficLow, id), fmt.Sprintf("%s:%d", WebhookTrafficEmpty, id)); err != nil {
			return false, err
		}
	}
	return true, tx.Commit()
}

// KeepTrafficReset сбрасывает расход по расписанию тарифов, пока не отменён
// ctx. Первый проход — сразу: панель могла пролежать выключенной в чей-то
// день сброса, и ждать ещё десять минут незачем.
func (a *API) KeepTrafficReset(ctx context.Context) {
	t := time.NewTicker(resetEvery)
	defer t.Stop()
	now := time.Now()
	for {
		ids, err := a.store.ResetDue(ctx, now)
		if err != nil {
			log.Printf("ежемесячный сброс трафика: %v", err)
		}
		for _, id := range ids {
			a.store.Record(ctx, Event{Action: EventUserReset, Actor: ActorSchedule, UserID: id, Detail: "ежемесячный"})
		}
		select {
		case <-ctx.Done():
			return
		case now = <-t.C:
		}
	}
}
