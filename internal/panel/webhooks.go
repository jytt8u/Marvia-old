package panel

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jytt8u/marvia/internal/egress"
)

const (
	WebhookUserExpired  = "user.expired"
	WebhookTrafficEmpty = "user.traffic_exhausted"
	WebhookTrafficLow   = "user.traffic_low"
	WebhookNodeDown     = "node.down"
	WebhookNodeUp       = "node.up"
	webhookLifetime     = 24 * time.Hour
)

// Снимок события и отметка его обнаружения живут в одной транзакции. Иначе
// перезапуск между «заметили» и «положили в очередь» потерял бы оповещение.
// Каскад нужен для обещания «удалили покупателя — удалили его данные».
const webhookSchema = `
CREATE TABLE IF NOT EXISTS webhook_queue (
    id TEXT PRIMARY KEY,
    endpoint TEXT NOT NULL,
    event TEXT NOT NULL,
    body BLOB NOT NULL,
    signature TEXT NOT NULL,
    next_at TEXT NOT NULL,
    deadline TEXT NOT NULL,
    attempts INTEGER NOT NULL DEFAULT 0,
    user_id INTEGER REFERENCES users(id) ON DELETE CASCADE,
    node_id INTEGER REFERENCES nodes(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS webhook_due ON webhook_queue(next_at);
CREATE TABLE IF NOT EXISTS webhook_state (
    key TEXT PRIMARY KEY,
    active INTEGER NOT NULL,
    user_id INTEGER REFERENCES users(id) ON DELETE CASCADE,
    node_id INTEGER REFERENCES nodes(id) ON DELETE CASCADE
);
`

type WebhookSettings struct {
	Enabled bool     `json:"enabled"`
	URLs    []string `json:"urls"`
	Secret  string   `json:"secret,omitempty"`
}

func (c WebhookSettings) Ready() bool { return c.Enabled && len(c.URLs) > 0 && len(c.Secret) >= 32 }

// DNS проверяется ещё раз при соединении через egress.Dial: предварительная
// проверка строки не защищает от имени, сменившего адрес на внутренний.
func checkWebhookURL(raw string) (string, error) {
	if len(raw) > 2048 {
		return "", errors.New("адрес вебхука длиннее 2048 байт")
	}
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		return "", errors.New("адрес вебхука должен быть https:// без имени пользователя и фрагмента")
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.Contains(host, "%") {
		return "", errors.New("вебхук не может вести в локальную сеть")
	}
	if ip, err := netip.ParseAddr(host); err == nil && egress.Forbidden(ip) {
		return "", errors.New("вебхук не может вести в локальную сеть")
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 || egress.ForbiddenPort(n) {
			return "", errors.New("недопустимый порт вебхука")
		}
	}
	u.Host = strings.ToLower(u.Host)
	return u.String(), nil
}

func (c WebhookSettings) check() (WebhookSettings, error) {
	if len(c.URLs) > 10 {
		return c, errors.New("можно задать не больше десяти адресов вебхуков")
	}
	if len(c.Secret) > 256 || (c.Secret != "" && len(c.Secret) < 32) {
		return c, errors.New("секрет вебхуков должен содержать от 32 до 256 байт")
	}
	seen := make(map[string]bool)
	urls := make([]string, 0, len(c.URLs))
	for _, raw := range c.URLs {
		endpoint, err := checkWebhookURL(raw)
		if err != nil {
			return c, err
		}
		if seen[endpoint] {
			return c, errors.New("адрес вебхука повторяется")
		}
		seen[endpoint] = true
		urls = append(urls, endpoint)
	}
	c.URLs = urls
	if c.Enabled && !c.Ready() {
		return c, errors.New("для вебхуков нужны адрес и секрет подписи")
	}
	return c, nil
}

type webhookReader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func readWebhookSettings(ctx context.Context, db webhookReader) (WebhookSettings, error) {
	var raw string
	err := db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = 'webhooks'`).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return WebhookSettings{}, nil
	}
	if err != nil {
		return WebhookSettings{}, fmt.Errorf("чтение настроек вебхуков: %w", err)
	}
	var cfg WebhookSettings
	if json.Unmarshal([]byte(raw), &cfg) != nil {
		return WebhookSettings{}, nil
	}
	checked, err := cfg.check()
	if err != nil {
		return WebhookSettings{}, nil
	}
	return checked, nil
}

func (s *Store) WebhookSettings(ctx context.Context) (WebhookSettings, error) {
	return readWebhookSettings(ctx, s.db)
}

func (s *Store) SetWebhookSettings(ctx context.Context, cfg WebhookSettings) error {
	cfg, err := cfg.check()
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	old, err := readWebhookSettings(ctx, tx)
	if err != nil {
		return err
	}
	// Удалённому получателю больше не отправляем. Смена секрета отменяет
	// старые подписи; молча посылать их боту с новым секретом бесполезно.
	if !cfg.Ready() || old.Secret != cfg.Secret {
		if _, err := tx.ExecContext(ctx, `DELETE FROM webhook_queue`); err != nil {
			return err
		}
	} else {
		rows, err := tx.QueryContext(ctx, `SELECT DISTINCT endpoint FROM webhook_queue`)
		if err != nil {
			return err
		}
		var removed []string
		for rows.Next() {
			var endpoint string
			if err := rows.Scan(&endpoint); err != nil {
				rows.Close()
				return err
			}
			found := false
			for _, kept := range cfg.URLs {
				if kept == endpoint {
					found = true
				}
			}
			if !found {
				removed = append(removed, endpoint)
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, endpoint := range removed {
			if _, err := tx.ExecContext(ctx, `DELETE FROM webhook_queue WHERE endpoint = ?`, endpoint); err != nil {
				return err
			}
		}
	}
	if !old.Ready() {
		if _, err := tx.ExecContext(ctx, `DELETE FROM webhook_state`); err != nil {
			return err
		}
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO settings(key, value) VALUES ('webhooks', ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, string(raw)); err != nil {
		return fmt.Errorf("сохранение настроек вебхуков: %w", err)
	}
	return tx.Commit()
}

// Отдельные типы — белый список полей. Сериализация User или Node целиком
// унесла бы токены и ключи при первом же добавлении поля в эти структуры.
type WebhookUser struct {
	ID           int64      `json:"id"`
	Label        string     `json:"label"`
	ExternalID   string     `json:"external_id"`
	ExpiresAt    *time.Time `json:"expires_at"`
	TrafficLimit int64      `json:"traffic_limit"`
	Used         int64      `json:"used"`
	PlanID       *int64     `json:"plan_id"`
}

type WebhookNode struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

type WebhookEvent struct {
	Event string       `json:"event"`
	At    time.Time    `json:"at"`
	User  *WebhookUser `json:"user,omitempty"`
	Node  *WebhookNode `json:"node,omitempty"`

	// Text — готовое сообщение покупателю из текстов продавца (report.go);
	// нет текста к событию — поля нет, и бот пишет своё.
	Text string `json:"text,omitempty"`
}

func webhookSignature(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func enqueueWebhookTx(ctx context.Context, tx *sql.Tx, event WebhookEvent) error {
	cfg, err := readWebhookSettings(ctx, tx)
	if err != nil || !cfg.Ready() {
		return err
	}
	if event.User != nil {
		texts, err := readBotTexts(ctx, tx)
		if err != nil {
			return err
		}
		event.Text = renderBotText(texts[event.Event], *event.User, event.At)
	}
	body, err := json.Marshal(event)
	if err != nil {
		return err
	}
	var userID, nodeID int64
	if event.User != nil {
		userID = event.User.ID
	}
	if event.Node != nil {
		nodeID = event.Node.ID
	}
	for _, endpoint := range cfg.URLs {
		id, err := NewToken()
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO webhook_queue(id, endpoint, event, body, signature, next_at, deadline, user_id, node_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			id, endpoint, event.Event, body, webhookSignature(cfg.Secret, body), format(event.At), format(event.At.Add(webhookLifetime)), nullID(userID), nullID(nodeID))
		if err != nil {
			return fmt.Errorf("сохранение вебхука: %w", err)
		}
	}
	return nil
}

const webhookUserSQL = `SELECT id, label, COALESCE(external_id, ''), expires_at, traffic_limit, MAX(0, ` + usedTotalSQL + ` - traffic_offset), plan_id FROM users`

type webhookScanner interface{ Scan(...any) error }

func scanWebhookUser(row webhookScanner) (WebhookUser, error) {
	var user WebhookUser
	var expires sql.NullString
	var plan sql.NullInt64
	err := row.Scan(&user.ID, &user.Label, &user.ExternalID, &expires, &user.TrafficLimit, &user.Used, &plan)
	if expires.Valid {
		at := parse(expires.String)
		user.ExpiresAt = &at
	}
	if plan.Valid {
		user.PlanID = &plan.Int64
	}
	return user, err
}

func queueUserWebhookTx(ctx context.Context, tx *sql.Tx, id int64, kind string) error {
	user, err := scanWebhookUser(tx.QueryRowContext(ctx, webhookUserSQL+` WHERE id = ?`, id))
	if err != nil {
		return err
	}
	return enqueueWebhookTx(ctx, tx, WebhookEvent{Event: kind, At: time.Now().UTC(), User: &user})
}

func resetWebhookUserState(ctx context.Context, tx *sql.Tx, id int64) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM webhook_state WHERE user_id = ?`, id)
	return err
}
