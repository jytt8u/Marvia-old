package panel

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Промокоды, пробный период и рефералы — то, чем продавец привлекает людей.
// Денег панель по-прежнему не считает (см. README): промокод даёт дни и
// гигабайты, а не скидку, и цену по-прежнему решает бот.

var (
	// ErrNoSuchPromo — кода нет, он истёк или кончились использования.
	ErrNoSuchPromo = errors.New("нет такого промокода, он истёк или кончился")
	// ErrPromoUsed — этот покупатель код уже применял.
	ErrPromoUsed = errors.New("этот покупатель уже применял этот промокод")
	// ErrTrialUsed — пробный период этому человеку уже давали.
	ErrTrialUsed = errors.New("пробный период этому человеку уже давали")
	// ErrTrialNeedsKey — пробный выдаётся только с ключом продавца: без него
	// не узнать, что человек тот же.
	ErrTrialNeedsKey = errors.New("пробный тариф выдаётся только с external_id: без него не узнать, что человек тот же")
	// ErrTrialRenew — пробным не продлевают.
	ErrTrialRenew = errors.New("пробным тарифом не продлевают: он один раз на человека")
)

// Promo — промокод.
type Promo struct {
	ID int64 `json:"id"`
	// Code сравнивается без учёта регистра: покупатель набирает его руками.
	Code string `json:"code"`
	// Days — сколько дней добавить к сроку; бессрочным не добавляется.
	Days int `json:"days"`
	// Traffic — сколько байт добавить к лимиту; безлимитным не добавляется.
	Traffic int64 `json:"traffic"`
	// MaxUses — сколько людей может применить код; 0 — сколько угодно.
	MaxUses int `json:"max_uses"`
	Uses    int `json:"uses"`
	// ExpiresAt — до какого времени код действует; nil — без срока.
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
}

// PromoParams — новый промокод.
type PromoParams struct {
	Code      string  `json:"code"`
	Days      int     `json:"days"`
	Traffic   int64   `json:"traffic"`
	MaxUses   int     `json:"max_uses"`
	ExpiresAt *Expiry `json:"expires_at,omitempty"`
}

func (p *PromoParams) check() error {
	p.Code = strings.TrimSpace(p.Code)
	switch {
	case p.Code == "" || len([]rune(p.Code)) > 32 || strings.ContainsAny(p.Code, " \t\n"):
		return errors.New("промокод — от 1 до 32 знаков без пробелов")
	case p.Days < 0 || p.Days > 366:
		return errors.New("дни по промокоду — от 0 до 366")
	case p.Traffic < 0 || p.Traffic > 10<<40:
		return errors.New("трафик по промокоду — от 0 до 10 ТБ")
	case p.Days == 0 && p.Traffic == 0:
		return errors.New("промокод ничего не даёт: нужны дни или трафик")
	case p.MaxUses < 0:
		return errors.New("число использований не бывает отрицательным")
	}
	return nil
}

const promoCols = `id, code, days, traffic, max_uses, uses, expires_at, created_at`

func scanPromo(row rowScanner) (Promo, error) {
	var (
		p       Promo
		expires sql.NullString
		created string
	)
	if err := row.Scan(&p.ID, &p.Code, &p.Days, &p.Traffic, &p.MaxUses, &p.Uses, &expires, &created); err != nil {
		return Promo{}, err
	}
	p.ExpiresAt = parseNullTime(expires)
	p.CreatedAt = parse(created)
	return p, nil
}

// ListPromos — все промокоды, новые сверху.
func (s *Store) ListPromos(ctx context.Context) ([]Promo, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+promoCols+` FROM promos ORDER BY id DESC`)
	if err != nil {
		return nil, fmt.Errorf("чтение промокодов: %w", err)
	}
	defer rows.Close()
	list := []Promo{}
	for rows.Next() {
		p, err := scanPromo(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, p)
	}
	return list, rows.Err()
}

// CreatePromo заводит промокод.
func (s *Store) CreatePromo(ctx context.Context, p PromoParams) (Promo, error) {
	if err := p.check(); err != nil {
		return Promo{}, err
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO promos (code, days, traffic, max_uses, uses, expires_at, created_at) VALUES (?, ?, ?, ?, 0, ?, ?)`,
		p.Code, p.Days, p.Traffic, p.MaxUses, nullTime(p.ExpiresAt.at()), format(time.Now().UTC()))
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return Promo{}, errors.New("такой промокод уже есть")
		}
		return Promo{}, fmt.Errorf("создание промокода: %w", err)
	}
	id, _ := res.LastInsertId()
	return scanPromo(s.db.QueryRowContext(ctx, `SELECT `+promoCols+` FROM promos WHERE id = ?`, id))
}

// DeletePromo удаляет промокод. Уже полученные по нему дни остаются.
func (s *Store) DeletePromo(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM promos WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// RedeemPromo применяет промокод к покупателю.
//
// Одним кодом один человек пользуется один раз: иначе «+7 дней» в чате на
// тысячу человек превращался бы в вечный доступ у любого, кто нажал
// кнопку в боте сто раз. Проверка, учёт и прибавка — в одной транзакции,
// чтобы два одновременных нажатия не прошли оба.
func (s *Store) RedeemPromo(ctx context.Context, userID int64, code string, now time.Time) (User, Promo, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, Promo{}, err
	}
	defer func() { _ = tx.Rollback() }()

	p, err := scanPromo(tx.QueryRowContext(ctx, `SELECT `+promoCols+` FROM promos WHERE code = ? COLLATE NOCASE`, strings.TrimSpace(code)))
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, Promo{}, ErrNoSuchPromo
	}
	if err != nil {
		return User{}, Promo{}, err
	}
	if (p.ExpiresAt != nil && !p.ExpiresAt.After(now)) || (p.MaxUses > 0 && p.Uses >= p.MaxUses) {
		return User{}, Promo{}, ErrNoSuchPromo
	}
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE id = ?`, userID).Scan(&exists); err != nil {
		return User{}, Promo{}, err
	}
	if exists == 0 {
		return User{}, Promo{}, ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO promo_uses (promo_id, user_id, at) VALUES (?, ?, ?)`, p.ID, userID, format(now.UTC())); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") || strings.Contains(err.Error(), "PRIMARY KEY") {
			return User{}, Promo{}, ErrPromoUsed
		}
		return User{}, Promo{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE promos SET uses = uses + 1 WHERE id = ?`, p.ID); err != nil {
		return User{}, Promo{}, err
	}
	if err := bonusTx(ctx, tx, userID, time.Duration(p.Days)*24*time.Hour, p.Traffic, now); err != nil {
		return User{}, Promo{}, err
	}
	if err := tx.Commit(); err != nil {
		return User{}, Promo{}, err
	}
	p.Uses++
	user, err := s.GetUser(ctx, userID)
	return user, p, err
}

// bonusTx прибавляет дни и трафик. Бессрочному дни не добавляются, а
// безлимитному — трафик: «+7 дней» не должен превращать вечный доступ в
// недельный, а «+10 ГБ» — безлимит в десять гигабайт.
func bonusTx(ctx context.Context, tx *sql.Tx, userID int64, d time.Duration, traffic int64, now time.Time) error {
	var expires sql.NullString
	var limit int64
	if err := tx.QueryRowContext(ctx, `SELECT expires_at, traffic_limit FROM users WHERE id = ?`, userID).Scan(&expires, &limit); err != nil {
		return err
	}
	if d > 0 && expires.Valid {
		base := now.UTC()
		if t := parse(expires.String); t.After(base) {
			base = t
		}
		if _, err := tx.ExecContext(ctx, `UPDATE users SET expires_at = ? WHERE id = ?`, format(base.Add(d)), userID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM webhook_state WHERE key = ?`, fmt.Sprintf("%s:%d", WebhookUserExpired, userID)); err != nil {
			return err
		}
	}
	if traffic > 0 && limit > 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE users SET traffic_limit = traffic_limit + ? WHERE id = ?`, traffic, userID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM webhook_state WHERE user_id = ? AND key IN (?, ?)`, userID,
			fmt.Sprintf("%s:%d", WebhookTrafficLow, userID), fmt.Sprintf("%s:%d", WebhookTrafficEmpty, userID)); err != nil {
			return err
		}
	}
	return nil
}

// trialKey — след ключа продавца, по которому узнают, что пробный уже
// давали. Хеш, а не сам telegram id: запись живёт и после удаления
// покупателя — иначе удалил и завёл заново, и пробный снова доступен, — а
// хранить номер человека, которого продавец удалил, панели незачем.
func trialKey(externalID string) string {
	sum := sha256.Sum256([]byte("marvia-trial|" + externalID))
	return hex.EncodeToString(sum[:])
}

// claimTrialTx отмечает пробный для ключа продавца или отказывает, если он
// уже был.
func claimTrialTx(ctx context.Context, tx *sql.Tx, externalID string, now time.Time) error {
	if externalID == "" {
		return ErrTrialNeedsKey
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO trials (key, at) VALUES (?, ?)`, trialKey(externalID), format(now.UTC())); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") || strings.Contains(err.Error(), "PRIMARY KEY") {
			return ErrTrialUsed
		}
		return err
	}
	return nil
}

// ReferralSettings — награда тому, кто привёл покупателя.
type ReferralSettings struct {
	// Days — сколько дней добавить пригласившему; 0 — рефералы выключены.
	Days int `json:"days"`
}

// ReferralSettings читает настройку.
func (s *Store) ReferralSettings(ctx context.Context) (ReferralSettings, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = 'referral'`).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return ReferralSettings{}, nil
	}
	if err != nil {
		return ReferralSettings{}, err
	}
	var r ReferralSettings
	if json.Unmarshal([]byte(raw), &r) != nil || r.Days < 0 || r.Days > 366 {
		return ReferralSettings{}, nil
	}
	return r, nil
}

// SetReferralSettings сохраняет настройку.
func (s *Store) SetReferralSettings(ctx context.Context, r ReferralSettings) error {
	if r.Days < 0 || r.Days > 366 {
		return errors.New("дней за приглашённого — от 0 до 366")
	}
	raw, _ := json.Marshal(r)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO settings (key, value) VALUES ('referral', ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value`, string(raw))
	return err
}

// referTx связывает нового покупателя с пригласившим и награждает того.
// Возвращает номер пригласившего, если награда выдана.
//
// Награда — только за платного: за пробный её не дают, иначе любой заводил
// бы себе «друзей» пробными и копил дни бесплатно.
func referTx(ctx context.Context, tx *sql.Tx, newID int64, referrer string, paid bool, now time.Time) (int64, error) {
	referrer = strings.TrimSpace(referrer)
	if referrer == "" {
		return 0, nil
	}
	var refID int64
	err := tx.QueryRowContext(ctx, `SELECT id FROM users WHERE external_id = ?`, referrer).Scan(&refID)
	if errors.Is(err, sql.ErrNoRows) || refID == newID {
		// Незнакомый пригласивший — не ошибка продажи: покупатель получает
		// доступ, просто награждать некого.
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE users SET referred_by = ? WHERE id = ?`, refID, newID); err != nil {
		return 0, err
	}
	if !paid {
		return 0, nil
	}
	var raw string
	days := 0
	if tx.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = 'referral'`).Scan(&raw) == nil {
		var r ReferralSettings
		if json.Unmarshal([]byte(raw), &r) == nil {
			days = r.Days
		}
	}
	if days <= 0 {
		return 0, nil
	}
	if err := bonusTx(ctx, tx, refID, time.Duration(days)*24*time.Hour, 0, now); err != nil {
		return 0, err
	}
	return refID, nil
}

func (a *API) listPromos(w http.ResponseWriter, r *http.Request) {
	list, err := a.store.ListPromos(r.Context())
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	ref, _ := a.store.ReferralSettings(r.Context())
	ok(w, map[string]any{"promos": list, "referral": ref})
}

func (a *API) createPromo(w http.ResponseWriter, r *http.Request) {
	var p PromoParams
	if !decode(w, r, &p) {
		return
	}
	promo, err := a.store.CreatePromo(r.Context(), p)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	a.record(r, EventPromoCreate, Event{Detail: promo.Code})
	ok(w, map[string]any{"promo": promo})
}

func (a *API) deletePromo(w http.ResponseWriter, r *http.Request) {
	id, okID := pathID(w, r)
	if !okID {
		return
	}
	if err := a.store.DeletePromo(r.Context(), id); err != nil {
		respondStoreErr(w, err)
		return
	}
	a.record(r, EventPromoDelete, Event{})
	ok(w, map[string]any{"deleted": true})
}

func (a *API) redeemPromo(w http.ResponseWriter, r *http.Request) {
	id, okID := pathID(w, r)
	if !okID {
		return
	}
	var body struct {
		Code string `json:"code"`
	}
	if !decode(w, r, &body) {
		return
	}
	user, promo, err := a.store.RedeemPromo(r.Context(), id, body.Code, time.Now())
	switch {
	case errors.Is(err, ErrNoSuchPromo):
		fail(w, http.StatusNotFound, err.Error())
		return
	case errors.Is(err, ErrPromoUsed):
		fail(w, http.StatusConflict, err.Error())
		return
	case err != nil:
		respondStoreErr(w, err)
		return
	}
	a.record(r, EventUserPromo, Event{UserID: user.ID, Detail: promo.Code})
	ok(w, map[string]any{"user": visible(r, []User{user})[0], "promo": promo})
}

func (a *API) setReferral(w http.ResponseWriter, r *http.Request) {
	var body ReferralSettings
	if !decode(w, r, &body) {
		return
	}
	if err := a.store.SetReferralSettings(r.Context(), body); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	a.record(r, EventReferralUpdate, Event{Detail: fmt.Sprintf("+%d дней", body.Days)})
	ok(w, map[string]any{"referral": body})
}
