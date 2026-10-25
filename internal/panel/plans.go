package panel

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Тарифы.
//
// Продавец продаёт не «23 дня и 47 гигабайт», а «Месяц» и «Год». Без тарифов
// он вводит срок и трафик руками у каждого покупателя, и ошибка на один ноль
// выдаёт год вместо месяца. Тариф — шаблон: покупатель запоминает, по какому
// тарифу куплен, но срок и лимиты у него свои, и правка или удаление тарифа
// уже проданных не трогает.

// ErrPlanConflict — в одном запросе и тариф, и свои лимиты.
var ErrPlanConflict = errors.New("тариф и свои срок или лимиты вместе не задаются: либо тариф, либо свои значения")

// ErrNoSuchPlan — тарифа с таким номером нет.
var ErrNoSuchPlan = errors.New("нет такого тарифа")

// Plan — тариф.
type Plan struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`

	// Days — срок в днях; 0 — бессрочно.
	Days int `json:"days"`

	// TrafficLimit — байт на период; 0 — без ограничения.
	TrafficLimit int64 `json:"traffic_limit"`

	// MaxIPs — устройств одновременно; 0 — без ограничения.
	MaxIPs int `json:"max_ips"`

	// SpeedLimit — байт в секунду; 0 — без ограничения.
	SpeedLimit int64 `json:"speed_limit"`

	// Note — цена или пометка для продавца, например «300 ₽». Панель её не
	// считает: денег у проекта нет и не будет (см. README).
	Note string `json:"note,omitempty"`

	// TrafficReset — когда расход начинается заново внутри срока: "" —
	// только при продлении, "month" — каждый месяц от дня покупки (reset.go).
	TrafficReset string `json:"traffic_reset,omitempty"`

	CreatedAt time.Time `json:"created_at"`
}

// PlanParams — поля тарифа при создании и правке.
type PlanParams struct {
	Name         string `json:"name"`
	Days         int    `json:"days"`
	TrafficLimit int64  `json:"traffic_limit"`
	MaxIPs       int    `json:"max_ips"`
	SpeedLimit   int64  `json:"speed_limit"`
	Note         string `json:"note"`
	TrafficReset string `json:"traffic_reset"`
}

func (p *PlanParams) check() error {
	p.Name = strings.TrimSpace(p.Name)
	p.Note = strings.TrimSpace(p.Note)
	switch {
	case p.Name == "":
		return errors.New("у тарифа нет названия")
	case len([]rune(p.Name)) > 60:
		return errors.New("название тарифа длиннее 60 знаков")
	case len([]rune(p.Note)) > 120:
		return errors.New("пометка тарифа длиннее 120 знаков")
	// Верхняя граница срока — чтобы опечатка «3650» не выдала десять лет.
	// Кому нужно навсегда, ставит 0.
	case p.Days < 0 || p.Days > 3660:
		return errors.New("срок тарифа — от 0 (бессрочно) до 3660 дней")
	case p.TrafficLimit < 0 || p.MaxIPs < 0 || p.SpeedLimit < 0:
		return errors.New("лимиты тарифа не бывают отрицательными")
	case p.TrafficReset != ResetNever && p.TrafficReset != ResetMonthly:
		return fmt.Errorf("сброс трафика — %q или %q, а не %q", ResetNever, ResetMonthly, p.TrafficReset)
	}
	return nil
}

// ListPlans — все тарифы, от коротких к длинным: так их и выбирают.
func (s *Store) ListPlans(ctx context.Context) ([]Plan, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, days, traffic_limit, max_ips, speed_limit, note, traffic_reset, created_at
		FROM plans ORDER BY CASE WHEN days = 0 THEN 1 ELSE 0 END, days, traffic_limit, id`)
	if err != nil {
		return nil, fmt.Errorf("чтение тарифов: %w", err)
	}
	defer rows.Close()
	list := []Plan{}
	for rows.Next() {
		p, err := scanPlan(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, p)
	}
	return list, rows.Err()
}

type rowScanner interface{ Scan(dest ...any) error }

func scanPlan(row rowScanner) (Plan, error) {
	var (
		p       Plan
		created string
	)
	if err := row.Scan(&p.ID, &p.Name, &p.Days, &p.TrafficLimit, &p.MaxIPs, &p.SpeedLimit, &p.Note, &p.TrafficReset, &created); err != nil {
		return Plan{}, err
	}
	p.CreatedAt = parse(created)
	return p, nil
}

// GetPlan — тариф по номеру.
func (s *Store) GetPlan(ctx context.Context, id int64) (Plan, error) {
	return getPlan(ctx, s.db, id)
}

type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func getPlan(ctx context.Context, q querier, id int64) (Plan, error) {
	p, err := scanPlan(q.QueryRowContext(ctx, `
		SELECT id, name, days, traffic_limit, max_ips, speed_limit, note, traffic_reset, created_at
		FROM plans WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Plan{}, ErrNoSuchPlan
	}
	return p, err
}

// CreatePlan заводит тариф.
func (s *Store) CreatePlan(ctx context.Context, p PlanParams) (Plan, error) {
	if err := p.check(); err != nil {
		return Plan{}, err
	}
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO plans (name, days, traffic_limit, max_ips, speed_limit, note, traffic_reset, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		p.Name, p.Days, p.TrafficLimit, p.MaxIPs, p.SpeedLimit, p.Note, p.TrafficReset, format(time.Now().UTC()))
	if err != nil {
		return Plan{}, fmt.Errorf("создание тарифа: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Plan{}, err
	}
	return s.GetPlan(ctx, id)
}

// UpdatePlan меняет тариф. Уже купившие по нему не меняются — до своего
// следующего продления.
func (s *Store) UpdatePlan(ctx context.Context, id int64, p PlanParams) (Plan, error) {
	if err := p.check(); err != nil {
		return Plan{}, err
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE plans SET name = ?, days = ?, traffic_limit = ?, max_ips = ?, speed_limit = ?, note = ?, traffic_reset = ?
		WHERE id = ?`, p.Name, p.Days, p.TrafficLimit, p.MaxIPs, p.SpeedLimit, p.Note, p.TrafficReset, id)
	if err != nil {
		return Plan{}, fmt.Errorf("правка тарифа: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Plan{}, ErrNoSuchPlan
	}
	return s.GetPlan(ctx, id)
}

// DeletePlan удаляет тариф. У купивших по нему остаются их срок и лимиты, а
// пометка тарифа снимается внешним ключом (ON DELETE SET NULL).
func (s *Store) DeletePlan(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM plans WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("удаление тарифа: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNoSuchPlan
	}
	return nil
}

// applyPlan подставляет в параметры нового покупателя срок и лимиты тарифа.
func (s *Store) applyPlan(ctx context.Context, p *CreateUserParams, now time.Time) error {
	if p.PlanID == nil {
		return nil
	}
	if p.ExpiresAt != nil || p.TrafficLimit != 0 || p.MaxIPs != 0 || p.SpeedLimit != 0 {
		return ErrPlanConflict
	}
	plan, err := s.GetPlan(ctx, *p.PlanID)
	if err != nil {
		return err
	}
	if plan.Days > 0 {
		until := now.Add(time.Duration(plan.Days) * 24 * time.Hour)
		p.ExpiresAt = &Expiry{until}
	}
	p.TrafficLimit, p.MaxIPs, p.SpeedLimit = plan.TrafficLimit, plan.MaxIPs, plan.SpeedLimit
	p.trafficReset = plan.TrafficReset
	return nil
}

// RenewUser продлевает покупателя по тарифу: это новый оплаченный период.
//
// Срок добавляется к большему из «сейчас» и текущего окончания — продливший
// заранее остаток не теряет. Лимиты становятся тарифными, а расход
// начинается заново: месяц на 50 ГБ означает 50 ГБ на этот месяц, а не
// остаток прошлого. Бессрочный тариф снимает срок.
//
// Продление начинает и новый отсчёт месячных сбросов: день продления
// становится днём, когда расход обнуляется каждый месяц. Тариф без
// ежемесячного сброса его снимает — продлённый по «Году на 500 ГБ» не должен
// получать по полной квоте каждый месяц по памяти о прошлом тарифе.
func (s *Store) RenewUser(ctx context.Context, id, planID int64) (User, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, err
	}
	defer func() { _ = tx.Rollback() }()

	plan, err := getPlan(ctx, tx, planID)
	if err != nil {
		return User{}, err
	}

	var current sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT expires_at FROM users WHERE id = ?`, id).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, err
	}

	now := time.Now().UTC()
	anchor, next := resetSchedule(plan.TrafficReset, now)
	var expires any
	if plan.Days > 0 {
		base := now
		if current.Valid {
			if t := parse(current.String); t.After(base) {
				base = t
			}
		}
		expires = format(base.Add(time.Duration(plan.Days) * 24 * time.Hour))
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE users SET expires_at = ?, traffic_limit = ?, max_ips = ?, speed_limit = ?, plan_id = ?,
		       enabled = 1, traffic_offset = `+usedTotalSQL+`, reset_anchor = ?, reset_at = ?
		WHERE id = ?`,
		expires, plan.TrafficLimit, plan.MaxIPs, plan.SpeedLimit, plan.ID, anchor, next, id); err != nil {
		return User{}, fmt.Errorf("продление по тарифу: %w", err)
	}
	if err := resetWebhookUserState(ctx, tx, id); err != nil {
		return User{}, err
	}
	if err := queueUserWebhookTx(ctx, tx, id, EventUserRenew); err != nil {
		return User{}, err
	}
	if err := tx.Commit(); err != nil {
		return User{}, err
	}
	return s.GetUser(ctx, id)
}

// ResetTraffic обнуляет расход покупателя.
//
// Сами счётчики не трогаем: нода присылает накопительный итог и следующим же
// отчётом вернула бы стёртое. Вместо этого запоминаем, сколько было на
// момент сброса, и вычитаем это из расхода — и в панели, и в остатке,
// который получают ноды (см. NodeUsers).
func (s *Store) ResetTraffic(ctx context.Context, id int64) (User, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE users SET traffic_offset = `+usedTotalSQL+` WHERE id = ?`, id)
	if err != nil {
		return User{}, fmt.Errorf("сброс трафика: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return User{}, ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM webhook_state WHERE user_id = ? AND key IN (?, ?)`, id, fmt.Sprintf("%s:%d", WebhookTrafficLow, id), fmt.Sprintf("%s:%d", WebhookTrafficEmpty, id)); err != nil {
		return User{}, err
	}
	if err := tx.Commit(); err != nil {
		return User{}, err
	}
	return s.GetUser(ctx, id)
}

// usedTotalSQL — весь расход покупателя за жизнь, без поправки на сбросы.
// Выражение для UPDATE users: ссылается на users.id и users.used_before.
const usedTotalSQL = `(used_before + COALESCE((SELECT SUM(up + down) FROM usage WHERE usage.user_id = users.id), 0))`
