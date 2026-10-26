package panel

import (
	"context"
	"fmt"
	"time"
)

// Журнал событий.
//
// Отвечает на один вопрос: что делали с панелью. Завели человека, продлили,
// отключили, сменили ключ, добавили ноду, выпустили ключ боту. Без этого спор
// «я платил» — слово против слова, а «куда делась нода» — догадки.
//
// Чем он намеренно НЕ является.
//
// Это не слежка за людьми. Здесь нет ни адресов, ни того, кто когда
// подключался, ни тем более куда ходил: подключения видит нода, и она про них
// ничего не пишет — см. why в cmd/marvia-node/serve.go. Записывается только
// то, что сделал человек с админским токеном или бот с ключом, то есть
// действия владельца панели над своей же панелью.
//
// Записи привязаны к покупателю внешним ключом с каскадом. Удалили человека —
// исчезли и записи о нём. Журнал, переживающий удаление, превратил бы
// «удалили значит удалили» в неправду, а именно это обещание панель и даёт
// в остальных местах.
//
// Хранится ограниченный срок: вечный журнал — это база, которая растёт, пока
// не кончится диск, и которую всё опаснее терять.

// Виды событий. Строкой, а не числом: журнал читают глазами, в том числе
// запросом к базе напрямую, и `user.extend` понятнее семёрки.
const (
	EventUserCreate     = "user.create"
	EventUserUpdate     = "user.update"
	EventUserDelete     = "user.delete"
	EventCredAdd        = "cred.add"
	EventCredRotate     = "cred.rotate"
	EventCredDelete     = "cred.delete"
	EventSubToken       = "user.subtoken"
	EventNodeCreate     = "node.create"
	EventNodeUpdate     = "node.update"
	EventNodeDelete     = "node.delete"
	EventKeyCreate      = "key.create"
	EventKeyRevoke      = "key.revoke"
	EventAlertsUpdate   = "alerts.update"
	EventPanelUpgrade   = "panel.upgrade"
	EventNodesUpgrade   = "nodes.upgrade"
	EventSellerUpdate   = "seller.update"
	EventUserRenew      = "user.renew"
	EventUserReset      = "user.reset"
	EventPlanCreate     = "plan.create"
	EventPlanUpdate     = "plan.update"
	EventPlanDelete     = "plan.delete"
	EventWebhooksUpdate = "webhooks.update"
	EventWebhookFailed  = "webhook.failed"
	EventAutoUpdate     = "autoupdate.update"
	EventGuardUpdate    = "guard.update"
	EventUserFreeze     = "user.freeze"
	EventUserUnfreeze   = "user.unfreeze"
	EventUserDevices    = "user.devices"
	EventUsersExport    = "users.export"
	EventPromoCreate    = "promo.create"
	EventPromoDelete    = "promo.delete"
	EventUserPromo      = "user.promo"
	EventReferralUpdate = "referral.update"
	EventUserReferral   = "user.referral"
	EventBotTexts       = "bot_texts.update"
	EventPanelLogin     = "panel.login"
	EventPanelRestore   = "panel.restore"
	EventNightSpeed     = "night_speed.update"

	// ActorBonus — от чьего имени пишется награда за приглашённого: её
	// выдаёт не админ и не бот, а правило, которое продавец включил.
	ActorBonus = "bonus"
)

// ActorAdmin — действие сделано админским токеном. Ключи ботов записываются
// своим именем, чтобы было видно, чей бот наделал дел.
const ActorAdmin = "admin"

// EventsKeepDays — сколько держим записи.
//
// Три месяца: спор об оплате случается в пределах нескольких недель, а дальше
// запись становится не доказательством, а просто грузом, который жалко
// потерять и опасно хранить.
const EventsKeepDays = 90

// Event — одна запись журнала.
type Event struct {
	ID     int64     `json:"id"`
	At     time.Time `json:"at"`
	Actor  string    `json:"actor"`
	Action string    `json:"action"`

	// UserID и NodeID — над кем действовали. Ноль означает «ни над кем»:
	// например, выпуск ключа боту.
	UserID int64 `json:"user_id,omitempty"`
	NodeID int64 `json:"node_id,omitempty"`

	// Detail — короткое пояснение своими словами: «продлён на 30 дней»,
	// «отключён». Секретов здесь быть не должно: журнал смотрят с экрана и
	// пересылают.
	Detail string `json:"detail,omitempty"`
}

// Record кладёт запись в журнал.
//
// Ошибку не возвращает: несделанная запись не повод отменить само действие.
// Продлённая подписка важнее строчки о ней, и падать на журнале — значит
// ломать панель ради её летописи.
func (s *Store) Record(ctx context.Context, e Event) {
	if e.Actor == "" {
		e.Actor = ActorAdmin
	}
	_, _ = s.db.ExecContext(ctx, `
		INSERT INTO events (at, actor, action, user_id, node_id, detail)
		VALUES (?, ?, ?, ?, ?, ?)`,
		format(time.Now().UTC()), e.Actor, e.Action,
		nullID(e.UserID), nullID(e.NodeID), e.Detail)
}

// nullID превращает ноль в NULL: внешний ключ на несуществующую запись
// SQLite не примет, а ноль — это именно «ни над кем».
func nullID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

// Events отдаёт последние записи журнала, новые сверху.
func (s *Store) Events(ctx context.Context, limit int) ([]Event, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT id, at, actor, action, COALESCE(user_id, 0), COALESCE(node_id, 0), detail
		FROM events ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("чтение журнала: %w", err)
	}
	defer rows.Close()

	var out []Event
	for rows.Next() {
		var (
			e  Event
			at string
		)
		if err := rows.Scan(&e.ID, &at, &e.Actor, &e.Action, &e.UserID, &e.NodeID, &e.Detail); err != nil {
			return nil, err
		}
		e.At = parse(at)
		out = append(out, e)
	}
	return out, rows.Err()
}

// ForgetOldEvents убирает записи старше keep суток.
func (s *Store) ForgetOldEvents(ctx context.Context, keep int) error {
	if keep <= 0 {
		keep = EventsKeepDays
	}
	edge := time.Now().UTC().AddDate(0, 0, -keep).Format("2006-01-02")
	if _, err := s.db.ExecContext(ctx, `DELETE FROM events WHERE at < ?`, edge); err != nil {
		return fmt.Errorf("очистка журнала: %w", err)
	}
	return nil
}
