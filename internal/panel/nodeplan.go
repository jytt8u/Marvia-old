package panel

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Лимит трафика у хостера, окно обслуживания и группы нод.

// MaintWindow — когда нода на обслуживании. Пока окно идёт, нода для всех
// выключена: панель отвечает ей «выключена», она перестаёт пускать, а
// клиенты Marvia переезжают на живую ноду сами. Окно одно и разовое:
// повторяющиеся работы продавец заводит заново, а забытое расписание
// гасило бы ноду каждую ночь, когда о нём уже никто не помнит.
type MaintWindow struct {
	From  time.Time `json:"from"`
	Until time.Time `json:"until"`
}

// inMaintenance — идёт ли окно в now.
func inMaintenance(from, until *time.Time, now time.Time) bool {
	return from != nil && until != nil && !now.Before(*from) && now.Before(*until)
}

// limitPeriodStart — начало текущего расчётного периода хостера: последнее
// наступившее число day (1–28) по UTC. День больше 28 не бывает: у февраля
// его нет, и счёт съезжал бы.
func limitPeriodStart(day int, now time.Time) time.Time {
	if day < 1 || day > 28 {
		day = 1
	}
	now = now.UTC()
	start := time.Date(now.Year(), now.Month(), day, 0, 0, 0, 0, time.UTC)
	if start.After(now) {
		start = start.AddDate(0, -1, 0)
	}
	return start
}

// fillMonthUsed считает расход ноды за текущий период хостера — только у
// тех, кому лимит задан.
func (s *Store) fillMonthUsed(ctx context.Context, list []Node, now time.Time) error {
	for i := range list {
		n := &list[i]
		if n.MonthlyLimit <= 0 {
			continue
		}
		start := limitPeriodStart(n.LimitDay, now).Format("2006-01-02")
		if err := s.db.QueryRowContext(ctx,
			`SELECT COALESCE(SUM(up + down), 0) FROM usage_daily WHERE node_id = ? AND day >= ?`, n.ID, start).
			Scan(&n.MonthUsed); err != nil {
			return fmt.Errorf("расход ноды за период: %w", err)
		}
	}
	return nil
}

// nodeGroupsOK проверяет группы ноды: те же правила, что у меток.
func cleanGroups(in []string) ([]string, error) {
	out, err := cleanTags(in)
	if err != nil {
		return nil, errors.New("группы: не больше 10, каждая до 24 знаков, без запятых")
	}
	return out, nil
}

// groupAllows — видит ли покупатель группы group ноду с группами groups.
// Нода без групп — для всех; с группами — только для своих.
func groupAllows(groups []string, group string) bool {
	if len(groups) == 0 {
		return true
	}
	for _, g := range groups {
		if strings.EqualFold(g, group) {
			return true
		}
	}
	return false
}

// forGroup оставляет ноды, которые видит покупатель группы group.
func forGroup(nodes []Node, group string) []Node {
	out := nodes[:0:0]
	for _, n := range nodes {
		if groupAllows(n.Groups, group) {
			out = append(out, n)
		}
	}
	return out
}

// checkMaint проверяет окно: конец позже начала и не дальше месяца — окно
// на полгода почти наверняка опечатка, и нода молча пропала бы у всех.
func checkMaint(w MaintWindow) error {
	if w.Until.IsZero() {
		return nil
	}
	switch {
	case !w.Until.After(w.From):
		return errors.New("обслуживание: конец должен быть позже начала")
	case w.Until.Sub(w.From) > 31*24*time.Hour:
		return errors.New("обслуживание: окно не длиннее месяца")
	}
	return nil
}

// inputError — ошибка в том, что прислали, а не в панели: ответ 400, а не
// 500. Без различия продавец на опечатку в адресе получал «ошибка сервера»
// и шёл искать поломку там, где её нет.
type inputError struct{ error }

func (e inputError) Unwrap() error { return e.error }

func badInput(msg string) error { return inputError{errors.New(msg)} }

func badInputf(format string, args ...any) error { return inputError{fmt.Errorf(format, args...)} }
