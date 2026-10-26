// Package panel — управляющий слой: пользователи, подписки, ноды, статистика.
//
// Панель живёт отдельно от нод и никогда не пропускает через себя трафик
// пользователей. Нода — расходник, её теряют и блокируют; панель должна
// пережить любое число потерянных нод.
package panel

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/jytt8u/marvia/internal/users"
	"github.com/jytt8u/marvia/internal/version"
	"github.com/jytt8u/marvia/internal/vp1"

	_ "modernc.org/sqlite" // чистый Go, без cgo: нода и панель кросс-компилируются одной командой
)

// ErrNotFound — запрошенной записи нет.
var ErrNotFound = errors.New("запись не найдена")

// Виды учётных данных.
//
// Один пользователь может иметь несколько наборов: ключ для нашего протокола,
// UUID для VLESS, пароль для Trojan. Так продавец переводит покупателей на
// Veil, не заставляя их менять приложение, — а это самое дорогое в переезде.
const (
	CredVP1    = "vp1"
	CredVLESS  = "vless"
	CredTrojan = "trojan"
)

// User — подписчик в терминах панели.
type User struct {
	ID           int64      `json:"id"`
	Label        string     `json:"label"`
	Enabled      bool       `json:"enabled"`
	ExpiresAt    *time.Time `json:"expires_at,omitempty"`
	TrafficLimit int64      `json:"traffic_limit"`
	MaxIPs       int        `json:"max_ips"`
	MaxConns     int        `json:"max_conns"`

	// SpeedLimit — потолок скорости в байтах в секунду, 0 означает «без
	// ограничения». Продавцу это тарифы: «100 Мбит/с» и «без ограничений»
	// продаются по-разному, а по одной лишь квоте в гигабайтах их не развести.
	SpeedLimit int64  `json:"speed_limit"`
	SubToken   string `json:"sub_token"`

	// ExternalID — ключ покупателя в системе продавца, обычно telegram id.
	//
	// Нужен затем, чтобы бот не держал вторую базу соответствий. Панель
	// считает его уникальным: повторная продажа тому же ключу не заводит
	// второго подписчика, а возвращает существующего. Платёжные системы
	// повторяют уведомление при сбое, и без этого одна оплата давала бы два
	// доступа.
	ExternalID string    `json:"external_id,omitempty"`
	CreatedAt  time.Time `json:"created_at"`

	// Used — расход по всем нодам с последнего сброса или продления по
	// тарифу, заполняется при чтении.
	Used int64 `json:"used"`

	// PlanID — тариф, по которому куплено или последний раз продлено; nil —
	// свои значения или тариф удалён.
	PlanID *int64 `json:"plan_id,omitempty"`

	// TrafficReset и TrafficResetAt — как часто и когда в следующий раз
	// расход обнулится сам, по расписанию тарифа (reset.go); пусто и nil —
	// только при продлении.
	TrafficReset   string     `json:"traffic_reset,omitempty"`
	TrafficResetAt *time.Time `json:"traffic_reset_at,omitempty"`

	// FrozenAt — когда доступ заморожен; nil — не заморожен (freeze.go).
	FrozenAt *time.Time `json:"frozen_at,omitempty"`

	// Note — заметка продавца; ботам не отдаётся (см. visible).
	Note string `json:"note,omitempty"`

	// Online, Devices, LastSeen — на связи ли он прямо сейчас. Заполняются при
	// чтении из последнего отчёта каждой живой ноды: соединений всего,
	// адресов за час (только у тех, кому задан лимит устройств — остальным
	// нода адреса не считает) и когда последний раз был на связи. Ни адресов,
	// ни стран, ни истории: это срез, а не журнал.
	//
	// LastSeen — одно время на покупателя, без ноды: на какой из них он был в
	// последний раз, панель не помнит. Для «он вообще подключался?» этого
	// хватает, а время по каждой ноде было бы картой его переездов.
	Online   int        `json:"online"`
	Devices  int        `json:"devices"`
	LastSeen *time.Time `json:"last_seen,omitempty"`

	// Credentials — наборы доступа. Секреты здесь публичные (публичный ключ,
	// UUID): приватную часть панель не хранит.
	Credentials []Credential `json:"credentials,omitempty"`
}

// Credential — один набор доступа пользователя.
type Credential struct {
	ID        int64     `json:"id"`
	Kind      string    `json:"kind"`
	Secret    string    `json:"secret"`
	Label     string    `json:"label"`
	CreatedAt time.Time `json:"created_at"`
}

// Node — точка входа.
type Node struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`

	// Country — страна словами. Пишет продавец; если не написал, панель
	// подставляет страну по адресу (geo.go). Написанное продавцом главнее:
	// у хостеров адреса числятся то в Нидерландах, то в США, и база адресов
	// может отправить «Финляндию» в Германию. Флаг клиенты подбирают по
	// написанному — так же делают Happ и Hiddify.
	Country string `json:"country,omitempty"`
	Address string `json:"address"`
	SNI     string `json:"sni"`

	// SNIExtra — запасные имена прикрытия, кроме SNI.
	//
	// Работают только под REALITY: там подлинность ноды подтверждает её ключ,
	// а не сертификат, и в SNI годится любое имя, которое нода принимает (флаг
	// -reality-sni у неё же). Одно имя на ноду означает, что блокировка домена
	// убивает ноду целиком, хотя её адрес жив.
	//
	// В базе лежит строкой через запятую, наружу уходит списком.
	SNIExtra  []string `json:"sni_extra,omitempty"`
	PublicKey string   `json:"public_key"`

	// RealityPublicKey и RealityShortID заполняются, когда нода работает под
	// маскировкой REALITY. По ним собираются ссылки для чужих клиентов:
	// параметры pbk и sid.
	RealityPublicKey string `json:"reality_public_key,omitempty"`
	RealityShortID   string `json:"reality_short_id,omitempty"`

	// WSPath заполняется, когда нода работает за CDN через WebSocket.
	// Тогда в ссылки уходит type=ws, а адрес указывает на CDN, а не на ноду.
	WSPath string `json:"ws_path,omitempty"`

	// QUIC — нода принимает ещё и по UDP на том же порту. Клиент пробует его
	// первым и молча уходит на TCP там, где UDP режут.
	QUIC bool `json:"quic,omitempty"`

	// Fingerprint — чьё TLS-приветствие изображает клиент. Пусто — Chrome.
	// См. fingerprint.go.
	Fingerprint string `json:"fingerprint,omitempty"`

	Enabled   bool       `json:"enabled"`
	LastSeen  *time.Time `json:"last_seen,omitempty"`
	CreatedAt time.Time  `json:"created_at"`

	// Version и Arch — сборка ноды, как она сама сообщила в последнем
	// отчёте. Пусто у нод старше этой возможности: они версию не шлют.
	Version string `json:"version,omitempty"`
	Arch    string `json:"arch,omitempty"`

	// UpgradeTo — версия, до которой продавец попросил ноду обновиться;
	// пусто, когда не просил или нода уже дошла.
	UpgradeTo string `json:"upgrade_to,omitempty"`

	// ExitID — у входа каскада: на какую ноду он перекладывает. Ноль — обычная
	// нода. См. cascade.go.
	ExitID int64 `json:"exit_id,omitempty"`
}

// Store — хранилище панели поверх SQLite.
type Store struct {
	db *sql.DB

	// backupKey — публичный ключ, которым шифруются копии базы. Пусто
	// означает, что копии остаются открытыми, как было раньше.
	backupKey []byte
}

const schema = `
PRAGMA journal_mode = WAL;
PRAGMA foreign_keys = ON;

-- Тарифы — шаблоны срока и лимитов, см. plans.go.
CREATE TABLE IF NOT EXISTS plans (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    name          TEXT    NOT NULL,
    days          INTEGER NOT NULL DEFAULT 0,
    traffic_limit INTEGER NOT NULL DEFAULT 0,
    max_ips       INTEGER NOT NULL DEFAULT 0,
    speed_limit   INTEGER NOT NULL DEFAULT 0,
    note          TEXT    NOT NULL DEFAULT '',
    created_at    TEXT    NOT NULL
);

CREATE TABLE IF NOT EXISTS users (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    label         TEXT    NOT NULL DEFAULT '',
    enabled       INTEGER NOT NULL DEFAULT 1,
    expires_at    TEXT,
    traffic_limit INTEGER NOT NULL DEFAULT 0,
    max_ips       INTEGER NOT NULL DEFAULT 0,
    max_conns     INTEGER NOT NULL DEFAULT 0,
    speed_limit   INTEGER NOT NULL DEFAULT 0,
    sub_token     TEXT    NOT NULL UNIQUE,
    external_id   TEXT,
    created_at    TEXT    NOT NULL,
    used_before   INTEGER NOT NULL DEFAULT 0,
    -- Тариф, по которому куплено, и расход на момент последнего сброса (см.
    -- plans.go). Тариф пропадает с удалением тарифа, срок и лимиты остаются.
    plan_id       INTEGER REFERENCES plans(id) ON DELETE SET NULL,
    traffic_offset INTEGER NOT NULL DEFAULT 0,
    -- Когда покупатель в последний раз был на связи — на любой ноде, без
    -- указания какой. См. forgetWhichNodeWhen.
    last_seen     TEXT
);

-- Адреса подписки прежней панели, см. import.go. Отдельной таблицей, а не
-- столбцом: у покупателя 3x-ui их бывает несколько, а у Marzban хранится не
-- адрес, а то, чем его проверить.
CREATE TABLE IF NOT EXISTS sub_aliases (
    kind       TEXT    NOT NULL,
    key        TEXT    NOT NULL,
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    secret     TEXT    NOT NULL DEFAULT '',
    not_before TEXT,
    PRIMARY KEY (kind, key)
);



CREATE TABLE IF NOT EXISTS credentials (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    kind       TEXT    NOT NULL,
    secret     TEXT    NOT NULL,
    label      TEXT    NOT NULL DEFAULT '',
    created_at TEXT    NOT NULL,
    UNIQUE (kind, secret)
);

CREATE INDEX IF NOT EXISTS credentials_user ON credentials(user_id);

CREATE TABLE IF NOT EXISTS nodes (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    name       TEXT    NOT NULL,
    country    TEXT    NOT NULL DEFAULT '',
    address    TEXT    NOT NULL,
    sni        TEXT    NOT NULL DEFAULT '',
    sni_extra  TEXT    NOT NULL DEFAULT '',
    public_key TEXT    NOT NULL,
    reality_public_key TEXT NOT NULL DEFAULT '',
    reality_short_id   TEXT NOT NULL DEFAULT '',
    ws_path            TEXT NOT NULL DEFAULT '',
    quic               INTEGER NOT NULL DEFAULT 0,
    fingerprint        TEXT NOT NULL DEFAULT '',
    token_hash TEXT    NOT NULL UNIQUE,
    enabled    INTEGER NOT NULL DEFAULT 1,
    last_seen  TEXT,
    created_at TEXT    NOT NULL
);


CREATE TABLE IF NOT EXISTS api_keys (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    name         TEXT    NOT NULL,
    token_hash   TEXT    NOT NULL UNIQUE,
    scopes       TEXT    NOT NULL,
    created_at   TEXT    NOT NULL,
    last_used_at TEXT,
    revoked_at   TEXT
);

CREATE TABLE IF NOT EXISTS node_invites (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    token_hash TEXT    NOT NULL UNIQUE,
    label      TEXT    NOT NULL DEFAULT '',
    expires_at TEXT    NOT NULL,
    used_at    TEXT,
    node_id    INTEGER REFERENCES nodes(id) ON DELETE SET NULL,
    created_at TEXT    NOT NULL
);

CREATE TABLE IF NOT EXISTS node_reports (
    node_id     INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    ok          INTEGER NOT NULL,
    latency_ms  INTEGER NOT NULL DEFAULT 0,
    reported_at TEXT    NOT NULL,
    PRIMARY KEY (node_id, user_id)
);

CREATE TABLE IF NOT EXISTS usage (
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    node_id    INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    up         INTEGER NOT NULL DEFAULT 0,
    down       INTEGER NOT NULL DEFAULT 0,
    updated_at TEXT    NOT NULL,
    PRIMARY KEY (user_id, node_id)
);

-- История расхода по суткам.
--
-- Таблица usage выше хранит только накопительный итог: сколько всего прошло
-- через эту ноду у этого человека. По ней нельзя ответить ни на «сколько за
-- вчера», ни на «растём ли мы» — а это первое, что продавец хочет видеть.
--
-- Нода присылает тот же накопительный итог, поэтому здесь копятся разницы
-- между её отчётами. День берём в UTC: продавец и его покупатели живут в
-- разных поясах, и любой другой выбор был бы произволом в пользу одного.
CREATE TABLE IF NOT EXISTS usage_daily (
    day     TEXT    NOT NULL,
    node_id INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    up      INTEGER NOT NULL DEFAULT 0,
    down    INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (day, node_id)
);

CREATE INDEX IF NOT EXISTS usage_daily_day ON usage_daily(day);

CREATE TABLE IF NOT EXISTS events (
    id      INTEGER PRIMARY KEY AUTOINCREMENT,
    at      TEXT    NOT NULL,
    actor   TEXT    NOT NULL,
    action  TEXT    NOT NULL,
    user_id INTEGER REFERENCES users(id) ON DELETE CASCADE,
    node_id INTEGER REFERENCES nodes(id) ON DELETE CASCADE,
    detail  TEXT    NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS events_at ON events(at);

CREATE TABLE IF NOT EXISTS settings (
    key   TEXT NOT NULL PRIMARY KEY,
    value TEXT NOT NULL
);

-- user_id — чей это ответ. В ответе лежит сам покупатель с меткой и ключом
-- продавца, и удаление покупателя обязано уносить и эту копию, а не ждать
-- неделю, пока она истечёт.
CREATE TABLE IF NOT EXISTS idempotency (
    key        TEXT NOT NULL PRIMARY KEY,
    scope      TEXT NOT NULL,
    response   TEXT NOT NULL,
    created_at TEXT NOT NULL,
    user_id    INTEGER REFERENCES users(id) ON DELETE CASCADE
);
`

// Open открывает или создаёт базу панели.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("открытие базы %s: %w", path, err)
	}
	// SQLite не любит параллельных писателей, а выигрыш от пула здесь нулевой.
	db.SetMaxOpenConns(1)

	if _, err := db.Exec(schema + webhookSchema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("создание схемы: %w", err)
	}
	if err := migrate(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

// migrate доводит уже существующую базу до текущей схемы.
//
// CREATE TABLE IF NOT EXISTS новых столбцов не добавляет, а обновление панели
// не должно требовать от продавца ручных действий с базой. Попытка добавить
// уже существующий столбец — не ошибка, а признак, что миграция отработала
// в прошлый запуск.
func migrate(db *sql.DB) error {
	steps := []string{
		`ALTER TABLE nodes ADD COLUMN reality_public_key TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE nodes ADD COLUMN reality_short_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE nodes ADD COLUMN ws_path TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE nodes ADD COLUMN country TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE nodes ADD COLUMN quic INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE nodes ADD COLUMN sni_extra TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE nodes ADD COLUMN version TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE nodes ADD COLUMN arch TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE nodes ADD COLUMN upgrade_to TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE users ADD COLUMN speed_limit INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE users ADD COLUMN external_id TEXT`,
		`CREATE TABLE IF NOT EXISTS api_keys (id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT NOT NULL, token_hash TEXT NOT NULL UNIQUE, scopes TEXT NOT NULL, created_at TEXT NOT NULL, last_used_at TEXT, revoked_at TEXT)`,
		// Индекс живёт только здесь, а не в схеме. Схема выполняется первой, и
		// на уже существующей базе CREATE TABLE IF NOT EXISTS столбца не
		// добавляет — индекс по нему упал бы раньше, чем миграция успела бы
		// этот столбец завести. Панель не поднялась бы вовсе, и не у меня, а у
		// каждого продавца при обновлении.
		`CREATE UNIQUE INDEX IF NOT EXISTS users_external ON users(external_id) WHERE external_id IS NOT NULL`,
		`CREATE TABLE IF NOT EXISTS idempotency (key TEXT NOT NULL PRIMARY KEY, scope TEXT NOT NULL, response TEXT NOT NULL, created_at TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS usage_daily (day TEXT NOT NULL, node_id INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE, up INTEGER NOT NULL DEFAULT 0, down INTEGER NOT NULL DEFAULT 0, PRIMARY KEY (day, node_id))`,
		`CREATE INDEX IF NOT EXISTS usage_daily_day ON usage_daily(day)`,
		`CREATE TABLE IF NOT EXISTS settings (key TEXT NOT NULL PRIMARY KEY, value TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS events (id INTEGER PRIMARY KEY AUTOINCREMENT, at TEXT NOT NULL, actor TEXT NOT NULL, action TEXT NOT NULL, user_id INTEGER REFERENCES users(id) ON DELETE CASCADE, node_id INTEGER REFERENCES nodes(id) ON DELETE CASCADE, detail TEXT NOT NULL DEFAULT '')`,
		`CREATE INDEX IF NOT EXISTS events_at ON events(at)`,
		`CREATE TABLE IF NOT EXISTS presence (user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE, node_id INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE, conns INTEGER NOT NULL DEFAULT 0, ips INTEGER NOT NULL DEFAULT 0, seen_at TEXT, PRIMARY KEY (user_id, node_id))`,
		`ALTER TABLE users ADD COLUMN used_before INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE nodes ADD COLUMN fingerprint TEXT NOT NULL DEFAULT ''`,
		`CREATE TABLE IF NOT EXISTS plans (id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT NOT NULL, days INTEGER NOT NULL DEFAULT 0, traffic_limit INTEGER NOT NULL DEFAULT 0, max_ips INTEGER NOT NULL DEFAULT 0, speed_limit INTEGER NOT NULL DEFAULT 0, note TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL)`,
		`ALTER TABLE users ADD COLUMN plan_id INTEGER REFERENCES plans(id) ON DELETE SET NULL`,
		`ALTER TABLE users ADD COLUMN traffic_offset INTEGER NOT NULL DEFAULT 0`,
		`CREATE TABLE IF NOT EXISTS sub_aliases (kind TEXT NOT NULL, key TEXT NOT NULL, user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE, secret TEXT NOT NULL DEFAULT '', not_before TEXT, PRIMARY KEY (kind, key))`,
		`ALTER TABLE users ADD COLUMN last_seen TEXT`,
		`ALTER TABLE idempotency ADD COLUMN user_id INTEGER REFERENCES users(id) ON DELETE CASCADE`,
		// Каскад: вход — та же запись ноды со ссылкой на свой выход (cascade.go).
		// relay_ip — откуда вход сверяется с панелью: от этого адреса выход
		// верит заголовку с адресом покупателя.
		`ALTER TABLE nodes ADD COLUMN exit_id INTEGER REFERENCES nodes(id) ON DELETE CASCADE`,
		// fetch_ms — за сколько нода отдала покупателю пробную порцию (health.go).
		`ALTER TABLE node_reports ADD COLUMN fetch_ms INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE nodes ADD COLUMN relay_ip TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE node_invites ADD COLUMN exit_id INTEGER REFERENCES nodes(id) ON DELETE CASCADE`,
		// Ежемесячный сброс трафика (reset.go): у тарифа — включён ли, у
		// покупателя — от какого дня считать и когда следующий.
		`ALTER TABLE plans ADD COLUMN traffic_reset TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE users ADD COLUMN reset_anchor TEXT`,
		`ALTER TABLE users ADD COLUMN reset_at TEXT`,
		// Вид сброса у покупателя. В alpha.29 бывал только месячный, и
		// заведённым тогда вид проставляется задним числом.
		`ALTER TABLE users ADD COLUMN reset_kind TEXT NOT NULL DEFAULT ''`,
		`UPDATE users SET reset_kind = 'month' WHERE reset_anchor IS NOT NULL AND reset_kind = ''`,
		// Сколько соединений нода отклонила за час — только числа (guard.go).
		// Заморозка, сброс устройств и заметка продавца (freeze.go).
		`ALTER TABLE users ADD COLUMN frozen_at TEXT`,
		`ALTER TABLE users ADD COLUMN devices_from TEXT`,
		`ALTER TABLE users ADD COLUMN note TEXT NOT NULL DEFAULT ''`,
		`CREATE TABLE IF NOT EXISTS node_guard (node_id INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE, hour TEXT NOT NULL, by_rate INTEGER NOT NULL DEFAULT 0, by_open INTEGER NOT NULL DEFAULT 0, by_block INTEGER NOT NULL DEFAULT 0, PRIMARY KEY (node_id, hour))`,
	}

	for _, step := range steps {
		if _, err := db.Exec(step); err != nil {
			if strings.Contains(err.Error(), "duplicate column name") {
				continue
			}
			return fmt.Errorf("обновление схемы (%s): %w", step, err)
		}
	}

	if err := namedCountries(db); err != nil {
		return err
	}
	if err := forgetWhoWentWhere(db); err != nil {
		return err
	}
	return forgetWhichNodeWhen(db)
}

// forgetWhichNodeWhen убирает время связи по каждой ноде.
//
// Раньше «был на связи» хранился строкой на пару «покупатель — нода» и не
// исчезал, когда покупатель уходил: панель помнила, когда он в последний раз
// был на Финляндии, когда на Турции, и так по каждой ноде, где он бывал. Ни
// один экран этого не читал — показывалось только самое свежее время.
//
// Теперь время одно, у самого покупателя, а в presence остаются только те,
// кто на связи сейчас. Накопленное сворачивается в это одно время, и строки
// по нодам уходят. То же со временем обновления накопительного расхода: его
// никто не читал, а по нему восстанавливалось то же самое.
//
// Шаги безвредны при повторе и выполняются на каждом запуске: после первого
// им просто нечего делать.
func forgetWhichNodeWhen(db *sql.DB) error {
	steps := []string{
		`UPDATE users SET last_seen = (SELECT MAX(p.seen_at) FROM presence p WHERE p.user_id = users.id)
		 WHERE last_seen IS NULL
		   AND EXISTS (SELECT 1 FROM presence p WHERE p.user_id = users.id AND p.seen_at IS NOT NULL)`,
		`UPDATE presence SET seen_at = NULL WHERE seen_at IS NOT NULL`,
		`DELETE FROM presence WHERE conns = 0 AND ips = 0`,
		`UPDATE usage SET updated_at = '' WHERE updated_at <> ''`,
	}
	for _, step := range steps {
		if _, err := db.Exec(step); err != nil {
			return fmt.Errorf("свёртка времени связи (%s): %w", step, err)
		}
	}
	return nil
}

// forgetWhoWentWhere убирает человека из посуточной истории расхода.
//
// На панелях, стоявших до этой правки, usage_daily хранит строки вида «день,
// покупатель, нода, объём» — то есть посуточную запись, кто на какой ноде
// сидел. Ни один экран её не читал: и график за 30 дней, и разбивка по нодам
// суммируют по людям. А изъятие панели выдавало бы эту историю целиком, и
// панель изымается легче ноды — она стоит в юрисдикции продавца.
//
// Поэтому столбец не просто перестаёт заполняться: накопленное сворачивается
// по дням и нодам, и прежние строки исчезают вместе с ним. Цифры на графиках
// от этого не меняются — они и были суммами.
func forgetWhoWentWhere(db *sql.DB) error {
	var hasUser int
	err := db.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('usage_daily') WHERE name = 'user_id'`).Scan(&hasUser)
	if err != nil {
		return fmt.Errorf("проверка истории расхода: %w", err)
	}
	if hasUser == 0 {
		return nil
	}

	steps := []string{
		`CREATE TABLE usage_daily_new (
			day     TEXT    NOT NULL,
			node_id INTEGER NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
			up      INTEGER NOT NULL DEFAULT 0,
			down    INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (day, node_id)
		)`,
		// Строки исчезнувших нод отбрасываем. По уму их быть не может —
		// удаление ноды уносит их каскадом, — но обновление, которое падает
		// на неожиданной строке, оставляет продавца с неподнявшейся панелью.
		// Разбивка по нодам такие строки и так не показывает: она соединяется
		// с таблицей нод.
		`INSERT INTO usage_daily_new (day, node_id, up, down)
		 SELECT day, node_id, SUM(up), SUM(down) FROM usage_daily
		 WHERE node_id IN (SELECT id FROM nodes) GROUP BY day, node_id`,
		`DROP TABLE usage_daily`,
		`ALTER TABLE usage_daily_new RENAME TO usage_daily`,
		`CREATE INDEX IF NOT EXISTS usage_daily_day ON usage_daily(day)`,
	}
	for _, step := range steps {
		if _, err := db.Exec(step); err != nil {
			return fmt.Errorf("свёртка истории расхода (%s): %w", step, err)
		}
	}
	return nil
}

// Close закрывает базу.
func (s *Store) Close() error { return s.db.Close() }

// CreateUserParams — что нужно, чтобы завести подписчика.
type CreateUserParams struct {
	Label        string  `json:"label"`
	ExpiresAt    *Expiry `json:"expires_at,omitempty"`
	TrafficLimit int64   `json:"traffic_limit"`
	MaxIPs       int     `json:"max_ips"`
	MaxConns     int     `json:"max_conns"`
	SpeedLimit   int64   `json:"speed_limit"`

	// Kinds — какие наборы доступа выдать сразу: vp1, vless, trojan.
	// Пусто означает только vp1.
	//
	// Продавцу, переводящему покупателей с чужой панели, обычно нужны все
	// три: vless и trojan работают в приложениях, которые у людей уже стоят,
	// а vp1 — в нашем клиенте, когда они до него дойдут.
	Kinds []string `json:"kinds,omitempty"`

	// ExternalID — ключ покупателя в системе продавца, обычно telegram id.
	// Задан — повторная продажа тому же ключу вернёт существующего подписчика
	// вместо второго доступа за ту же оплату.
	ExternalID string `json:"external_id,omitempty"`

	// PlanID — завести по тарифу: срок и лимиты берутся из него. Вместе со
	// своими сроком или лимитами не задаётся, см. ErrPlanConflict.
	PlanID *int64 `json:"plan_id,omitempty"`

	// trafficReset подставляется из тарифа в applyPlan. Не из запроса: свой
	// сброс без тарифа боту пока задать негде, и поле в JSON обещало бы то,
	// чего панель не делает.
	trafficReset string
}

// Issued — выданный набор доступа. Поле Secret показывается ровно один раз.
type Issued struct {
	ID     int64  `json:"id"`
	Kind   string `json:"kind"`
	Secret string `json:"secret"`
}

// CreateUser заводит подписчика и выдаёт ему запрошенные наборы доступа.
func (s *Store) CreateUser(ctx context.Context, p CreateUserParams) (User, []Issued, error) {
	kinds := p.Kinds
	if len(kinds) == 0 {
		kinds = []string{CredVP1}
	}
	for _, kind := range kinds {
		if kind != CredVP1 && kind != CredVLESS && kind != CredTrojan {
			return User{}, nil, fmt.Errorf("%w: %q", ErrUnknownKind, kind)
		}
	}

	// Повторная продажа тому же ключу ничего не создаёт.
	//
	// Платёжные системы повторяют уведомление, когда бот не ответил, а бот мог
	// упасть ровно между вызовом панели и записью у себя. Без этой проверки
	// одна оплата давала бы два доступа: покупатель получил бы два ключа, а
	// продавец потерял бы месяц выручки и заметил бы это нескоро.
	if p.ExternalID != "" {
		existing, err := s.UserByExternalID(ctx, p.ExternalID)
		switch {
		case err == nil:
			return existing, nil, ErrAlreadyExists
		case !errors.Is(err, ErrNotFound):
			return User{}, nil, err
		}
	}

	subToken, err := NewToken()
	if err != nil {
		return User{}, nil, err
	}

	now := time.Now().UTC()
	if err := s.applyPlan(ctx, &p, now); err != nil {
		return User{}, nil, err
	}
	anchor, next, kind := resetSchedule(p.trafficReset, now)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, nil, err
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx,
		`INSERT INTO users (label, enabled, expires_at, traffic_limit, max_ips, max_conns, speed_limit, sub_token, created_at, external_id, plan_id, reset_anchor, reset_at, reset_kind)
		 VALUES (?, 1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.Label, nullTime(p.ExpiresAt.at()), p.TrafficLimit, p.MaxIPs, p.MaxConns, p.SpeedLimit, subToken, format(now),
		nullString(p.ExternalID), p.PlanID, anchor, next, kind)
	if err != nil {
		return User{}, nil, fmt.Errorf("создание пользователя: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return User{}, nil, err
	}

	// Всё в одной транзакции: подписчик без единого набора доступа —
	// бесполезная запись, которую продавцу пришлось бы чинить руками.
	var (
		issued []Issued
		creds  []Credential
	)
	for _, kind := range kinds {
		stored, shown, err := newSecret(kind)
		if err != nil {
			return User{}, nil, err
		}
		res, err := tx.ExecContext(ctx,
			`INSERT INTO credentials (user_id, kind, secret, label, created_at) VALUES (?, ?, ?, '', ?)`,
			id, kind, stored, format(now))
		if err != nil {
			return User{}, nil, fmt.Errorf("создание набора %s: %w", kind, err)
		}
		credID, err := res.LastInsertId()
		if err != nil {
			return User{}, nil, err
		}
		issued = append(issued, Issued{ID: credID, Kind: kind, Secret: shown})
		creds = append(creds, Credential{ID: credID, Kind: kind, Secret: stored, CreatedAt: now})
	}

	// Создание и его оповещение неделимы: бот не должен потерять продажу
	// при перезапуске панели сразу после выдачи доступа.
	if err := queueUserWebhookTx(ctx, tx, id, EventUserCreate); err != nil {
		return User{}, nil, err
	}
	if err := tx.Commit(); err != nil {
		return User{}, nil, err
	}

	user := User{
		ID: id, Label: p.Label, Enabled: true, ExpiresAt: p.ExpiresAt.at(),
		TrafficLimit: p.TrafficLimit, MaxIPs: p.MaxIPs, MaxConns: p.MaxConns, SpeedLimit: p.SpeedLimit,
		SubToken: subToken, CreatedAt: now, Credentials: creds, PlanID: p.PlanID,
	}
	if v, ok := next.(string); ok {
		at := parse(v)
		user.TrafficResetAt = &at
		user.TrafficReset = kind
	}
	return user, issued, nil
}

// UpdateUserParams — изменяемые поля. nil означает «не трогать».
type UpdateUserParams struct {
	Label        *string `json:"label,omitempty"`
	Enabled      *bool   `json:"enabled,omitempty"`
	ExpiresAt    *Expiry `json:"expires_at,omitempty"`
	TrafficLimit *int64  `json:"traffic_limit,omitempty"`
	MaxIPs       *int    `json:"max_ips,omitempty"`
	MaxConns     *int    `json:"max_conns,omitempty"`
	SpeedLimit   *int64  `json:"speed_limit,omitempty"`

	// NoExpiry снимает срок: доступ становится бессрочным. Отдельным полем,
	// а не пустым expires_at: пустая строка в сроке — почти всегда ошибка
	// бота, и молча раздать по ней вечный доступ было бы дорого.
	NoExpiry bool `json:"no_expiry,omitempty"`

	// Note — заметка продавца, не длиннее maxNote знаков.
	Note *string `json:"note,omitempty"`
}

// ErrExpiryConflict — в одном запросе и срок, и «бессрочно».
var ErrExpiryConflict = errors.New("срок и бессрочно вместе не задаются")

// UpdateUser меняет заданные поля подписчика.
func (s *Store) UpdateUser(ctx context.Context, id int64, p UpdateUserParams) (User, error) {
	sets := make([]string, 0, 6)
	args := make([]any, 0, 7)

	if p.Label != nil {
		sets = append(sets, "label = ?")
		args = append(args, *p.Label)
	}
	if p.Enabled != nil {
		sets = append(sets, "enabled = ?")
		args = append(args, boolInt(*p.Enabled))
	}
	if p.ExpiresAt != nil && p.NoExpiry {
		return User{}, ErrExpiryConflict
	}
	if p.Note != nil {
		if len([]rune(*p.Note)) > maxNote {
			return User{}, ErrNoteTooLong
		}
		sets = append(sets, "note = ?")
		args = append(args, strings.TrimSpace(*p.Note))
	}
	if p.ExpiresAt != nil {
		sets = append(sets, "expires_at = ?")
		args = append(args, format(p.ExpiresAt.Time.UTC()))
	}
	if p.NoExpiry {
		sets = append(sets, "expires_at = NULL")
	}
	if p.TrafficLimit != nil {
		sets = append(sets, "traffic_limit = ?")
		args = append(args, *p.TrafficLimit)
	}
	if p.MaxIPs != nil {
		sets = append(sets, "max_ips = ?")
		args = append(args, *p.MaxIPs)
	}
	if p.MaxConns != nil {
		sets = append(sets, "max_conns = ?")
		args = append(args, *p.MaxConns)
	}
	if p.SpeedLimit != nil {
		sets = append(sets, "speed_limit = ?")
		args = append(args, *p.SpeedLimit)
	}
	if len(sets) == 0 {
		return s.GetUser(ctx, id)
	}

	args = append(args, id)
	query := "UPDATE users SET " + join(sets, ", ") + " WHERE id = ?"
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, err
	}
	defer tx.Rollback()
	before, err := scanWebhookUser(tx.QueryRowContext(ctx, webhookUserSQL+` WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, err
	}
	// «Включить» замороженного — это разморозка: срок сдвигается на время
	// паузы, а пометка снимается (freeze.go).
	if p.Enabled != nil && *p.Enabled {
		if _, err := unfreezeTx(ctx, tx, id, time.Now()); err != nil {
			return User{}, err
		}
	}
	res, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return User{}, fmt.Errorf("обновление пользователя: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return User{}, ErrNotFound
	}
	if p.NoExpiry && before.ExpiresAt != nil {
		if _, err := tx.ExecContext(ctx, `DELETE FROM webhook_state WHERE key = ?`, fmt.Sprintf("%s:%d", WebhookUserExpired, id)); err != nil {
			return User{}, err
		}
	}
	if p.ExpiresAt != nil && (before.ExpiresAt == nil || !p.ExpiresAt.Equal(*before.ExpiresAt)) {
		if _, err := tx.ExecContext(ctx, `DELETE FROM webhook_state WHERE key = ?`, fmt.Sprintf("%s:%d", WebhookUserExpired, id)); err != nil {
			return User{}, err
		}
		if before.ExpiresAt != nil && p.ExpiresAt.After(*before.ExpiresAt) {
			if err := queueUserWebhookTx(ctx, tx, id, EventUserRenew); err != nil {
				return User{}, err
			}
		}
	}
	if p.TrafficLimit != nil && before.TrafficLimit != *p.TrafficLimit {
		if _, err := tx.ExecContext(ctx, `DELETE FROM webhook_state WHERE user_id = ? AND key IN (?, ?)`, id, fmt.Sprintf("%s:%d", WebhookTrafficLow, id), fmt.Sprintf("%s:%d", WebhookTrafficEmpty, id)); err != nil {
			return User{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return User{}, err
	}
	return s.GetUser(ctx, id)
}

// DeleteUser удаляет подписчика вместе с его ключами и статистикой.
//
// И с сохранёнными ответами на повторы оплаты: в них лежит тот же покупатель
// с меткой, ключом продавца и ссылками. Новые ответы привязаны к нему и уходят
// каскадом, а записанные до этой привязки находятся по области: продление и
// продление по тарифу помечены его номером, первая продажа — ключом продавца.
// Иначе «удалили» ещё неделю значило бы «удалили, но копия лежит».
func (s *Store) DeleteUser(ctx context.Context, id int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var external sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT external_id FROM users WHERE id = ?`, id).Scan(&external)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("удаление пользователя: %w", err)
	}

	extend, renew, create := fmt.Sprintf("extend:%d", id), fmt.Sprintf("renew:%d", id), ""
	if external.String != "" {
		create = "create:" + external.String
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM idempotency WHERE user_id = ? OR scope IN (?, ?) OR (? <> '' AND scope = ?)`,
		id, extend, renew, create, create); err != nil {
		return fmt.Errorf("удаление сохранённых ответов: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, id); err != nil {
		return fmt.Errorf("удаление пользователя: %w", err)
	}
	return tx.Commit()
}

// GetUser читает подписчика вместе с ключами и расходом.
func (s *Store) GetUser(ctx context.Context, id int64) (User, error) {
	list, err := s.queryUsers(ctx, `WHERE u.id = ?`, id)
	if err != nil {
		return User{}, err
	}
	if len(list) == 0 {
		return User{}, ErrNotFound
	}
	return list[0], nil
}

// RotateSubToken выдаёт подписчику новый токен подписки.
//
// Нужно, когда старый утёк: ссылку подписки покупатели раздают знакомым, она
// попадает в переписки и на форумы, а по ней отдаются и список нод, и секреты
// vless с trojan. Отзывать ради этого весь доступ — терять покупателя.
//
// Старая ссылка перестаёт работать сразу, новую надо переслать покупателю:
// приложение обновляет список нод по адресу, который у него сохранён, и само
// про смену не узнает.
//
// Вместе с ней гаснут и адреса подписки с прежней панели, если покупатель
// переехал. Продавец меняет ссылку, потому что она утекла, — а утечь могла
// как раз старая, которую покупатель годами пересылал знакомым.
func (s *Store) RotateSubToken(ctx context.Context, id int64) (User, error) {
	token, err := NewToken()
	if err != nil {
		return User{}, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, err
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, `UPDATE users SET sub_token = ? WHERE id = ?`, token, id)
	if err != nil {
		return User{}, fmt.Errorf("смена токена подписки: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return User{}, ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM sub_aliases WHERE user_id = ?`, id); err != nil {
		return User{}, fmt.Errorf("отзыв прежних адресов подписки: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return User{}, err
	}
	return s.GetUser(ctx, id)
}

// UserBySubToken находит подписчика по токену подписки.
func (s *Store) UserBySubToken(ctx context.Context, token string) (User, error) {
	list, err := s.queryUsers(ctx, `WHERE u.sub_token = ?`, token)
	if err != nil {
		return User{}, err
	}
	if len(list) == 0 {
		return User{}, ErrNotFound
	}
	return list[0], nil
}

// ListUsers возвращает всех подписчиков.
func (s *Store) ListUsers(ctx context.Context) ([]User, error) {
	return s.queryUsers(ctx, `ORDER BY u.id`)
}

func (s *Store) queryUsers(ctx context.Context, where string, args ...any) ([]User, error) {
	query := `
		SELECT u.id, u.label, u.enabled, u.expires_at, u.traffic_limit, u.max_ips,
		       u.max_conns, u.speed_limit, u.sub_token, u.created_at, u.external_id,
		       MAX(0, u.used_before + COALESCE((SELECT SUM(up + down) FROM usage WHERE user_id = u.id), 0) - u.traffic_offset),
		       u.plan_id,
		       COALESCE((SELECT SUM(p.conns) FROM presence p JOIN nodes n ON n.id = p.node_id
		                 WHERE p.user_id = u.id AND n.last_seen >= ?), 0),
		       COALESCE((SELECT SUM(p.ips) FROM presence p JOIN nodes n ON n.id = p.node_id
		                 WHERE p.user_id = u.id AND n.last_seen >= ?), 0),
		       u.last_seen, u.reset_at, u.reset_kind, u.frozen_at, u.note
		FROM users u ` + where

	// Присутствие считается только по нодам, которые выходили на связь
	// недавно: у замолчавшей ноды последний отчёт застыл бы в базе, и
	// продавец видел бы «на связи» у покупателя, чья нода выключена третий час.
	fresh := format(time.Now().Add(-presenceStale))
	args = append([]any{fresh, fresh}, args...)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("чтение пользователей: %w", err)
	}
	defer rows.Close()

	var list []User
	for rows.Next() {
		var (
			u         User
			enabled   int
			expires   sql.NullString
			createdAt string
			external  sql.NullString
			seen      sql.NullString
			plan      sql.NullInt64
			resetAt   sql.NullString
			frozenAt  sql.NullString
		)
		if err := rows.Scan(&u.ID, &u.Label, &enabled, &expires, &u.TrafficLimit,
			&u.MaxIPs, &u.MaxConns, &u.SpeedLimit, &u.SubToken, &createdAt, &external, &u.Used, &plan,
			&u.Online, &u.Devices, &seen, &resetAt, &u.TrafficReset, &frozenAt, &u.Note); err != nil {
			return nil, err
		}
		u.Enabled = enabled != 0
		u.LastSeen = parseNullTime(seen)
		u.TrafficResetAt = parseNullTime(resetAt)
		u.FrozenAt = parseNullTime(frozenAt)
		u.ExpiresAt = parseNullTime(expires)
		u.CreatedAt = parse(createdAt)
		u.ExternalID = external.String
		if plan.Valid {
			u.PlanID = &plan.Int64
		}
		list = append(list, u)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for i := range list {
		creds, err := s.credentials(ctx, list[i].ID)
		if err != nil {
			return nil, err
		}
		list[i].Credentials = creds
	}
	return list, nil
}

func (s *Store) credentials(ctx context.Context, userID int64) ([]Credential, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, kind, secret, label, created_at FROM credentials WHERE user_id = ? ORDER BY id`, userID)
	if err != nil {
		return nil, fmt.Errorf("чтение ключей: %w", err)
	}
	defer rows.Close()

	var list []Credential
	for rows.Next() {
		var (
			c         Credential
			createdAt string
		)
		if err := rows.Scan(&c.ID, &c.Kind, &c.Secret, &c.Label, &createdAt); err != nil {
			return nil, err
		}
		c.CreatedAt = parse(createdAt)
		list = append(list, c)
	}
	return list, rows.Err()
}

// CreateNodeParams — что нужно, чтобы завести ноду.
type CreateNodeParams struct {
	Name      string   `json:"name"`
	Country   string   `json:"country"`
	Address   string   `json:"address"`
	SNI       string   `json:"sni"`
	SNIExtra  []string `json:"sni_extra"`
	PublicKey string   `json:"public_key"`

	RealityPublicKey string `json:"reality_public_key"`
	RealityShortID   string `json:"reality_short_id"`
	WSPath           string `json:"ws_path"`
	QUIC             bool   `json:"quic"`
}

// CreateNode регистрирует ноду и выдаёт ей токен.
// Токен возвращается один раз, в базе лежит только его хеш.
func (s *Store) CreateNode(ctx context.Context, p CreateNodeParams) (Node, string, error) {
	p.Country = countryTitle(p.Country)
	if p.PublicKey != "" {
		if _, err := vp1.DecodeKey(p.PublicKey); err != nil {
			return Node{}, "", fmt.Errorf("публичный ключ ноды: %w", err)
		}
	}

	token, err := NewToken()
	if err != nil {
		return Node{}, "", err
	}
	now := time.Now().UTC()

	res, err := s.db.ExecContext(ctx,
		`INSERT INTO nodes (name, country, address, sni, sni_extra, public_key, reality_public_key, reality_short_id, ws_path, quic, token_hash, enabled, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?)`,
		p.Name, p.Country, p.Address, p.SNI, joinNames(p.SNIExtra), p.PublicKey, p.RealityPublicKey, p.RealityShortID, p.WSPath, boolInt(p.QUIC), HashToken(token), format(now))
	if err != nil {
		return Node{}, "", fmt.Errorf("создание ноды: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Node{}, "", err
	}

	return Node{ID: id, Name: p.Name, Country: p.Country, Address: p.Address, SNI: p.SNI,
		SNIExtra:  splitNames(joinNames(p.SNIExtra)),
		PublicKey: p.PublicKey, RealityPublicKey: p.RealityPublicKey, RealityShortID: p.RealityShortID,
		WSPath: p.WSPath, QUIC: p.QUIC, Enabled: true, CreatedAt: now}, token, nil
}

// ListNodes возвращает все ноды.
func (s *Store) ListNodes(ctx context.Context) ([]Node, error) {
	return s.queryNodes(ctx, "")
}

// GetNode возвращает одну ноду.
func (s *Store) GetNode(ctx context.Context, id int64) (Node, error) {
	list, err := s.queryNodes(ctx, "WHERE id = ?", id)
	if err != nil {
		return Node{}, err
	}
	if len(list) == 0 {
		return Node{}, ErrNotFound
	}
	return list[0], nil
}

func (s *Store) queryNodes(ctx context.Context, where string, args ...any) ([]Node, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, country, address, sni, sni_extra, public_key, reality_public_key, reality_short_id, ws_path, quic, enabled, last_seen, created_at, version, arch, upgrade_to, fingerprint, COALESCE(exit_id, 0)
		 FROM nodes `+where+` ORDER BY id`, args...)
	if err != nil {
		return nil, fmt.Errorf("чтение нод: %w", err)
	}
	defer rows.Close()

	var list []Node
	for rows.Next() {
		var (
			n         Node
			sniExtra  string
			quicOn    int
			enabled   int
			lastSeen  sql.NullString
			createdAt string
		)
		if err := rows.Scan(&n.ID, &n.Name, &n.Country, &n.Address, &n.SNI, &sniExtra, &n.PublicKey,
			&n.RealityPublicKey, &n.RealityShortID, &n.WSPath, &quicOn, &enabled, &lastSeen, &createdAt,
			&n.Version, &n.Arch, &n.UpgradeTo, &n.Fingerprint, &n.ExitID); err != nil {
			return nil, err
		}
		n.SNIExtra = splitNames(sniExtra)
		n.Enabled = enabled != 0
		n.QUIC = quicOn != 0
		n.LastSeen = parseNullTime(lastSeen)
		n.CreatedAt = parse(createdAt)
		list = append(list, n)
	}
	return list, rows.Err()
}

// splitNames разбирает имена прикрытия из строки через запятую.
//
// Пустые и повторы отбрасываются: продавец вводит их руками, а повтор молча
// перекосил бы случайный выбор на клиенте в сторону одного имени.
func splitNames(raw string) []string {
	var out []string
	seen := make(map[string]bool)
	for _, name := range strings.Split(raw, ",") {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}

// joinNames собирает имена обратно в строку для базы.
func joinNames(names []string) string {
	clean := splitNames(strings.Join(names, ","))
	return strings.Join(clean, ",")
}

// UpdateNodeParams — изменяемые поля ноды. nil означает «не трогать».
//
// Адрес и ключи здесь не меняются: они приходят от самой ноды при регистрации,
// и правка их руками означала бы ссылки, ведущие в никуда. Продавцу нужно
// другое — назвать сервер по-человечески и вывести его из работы.
type UpdateNodeParams struct {
	Name    *string `json:"name,omitempty"`
	Country *string `json:"country,omitempty"`
	Enabled *bool   `json:"enabled,omitempty"`

	// Address — адрес ноды, если панель определила его неверно.
	//
	// Обычно его определяет сама панель по тому, откуда пришла регистрация, и
	// это правильно: нода, которая называет адрес сама, может назвать чужой, и
	// тогда покупатели пойдут на чужой сервер.
	//
	// Но когда панель стоит за обратным прокси — Cloudflare и подобным, — она
	// видит адрес прокси, а не ноды, и записывает его. Клиенты после этого идут
	// к прокси вместо ноды. Поправить это может только человек, у которого есть
	// админский токен: он один знает, где стоит его сервер.
	Address *string `json:"address,omitempty"`

	// SNIExtra — запасные имена прикрытия. Пустой список их убирает.
	//
	// Правится руками, в отличие от адреса и ключей: имена задаёт продавец
	// флагом -reality-sni на самой ноде, и панель обязана раздавать клиентам
	// ровно тот же набор. Нода про панель здесь ничего не сообщает — она
	// вообще не знает, кому её раздают.
	SNIExtra *[]string `json:"sni_extra,omitempty"`

	// Fingerprint — чьё TLS-приветствие изображает клиент; пусто — Chrome.
	Fingerprint *string `json:"fingerprint,omitempty"`

	// ExitID — новый выход входа каскада. Проверяет его API (checkExit):
	// хранилищу не видно, вход ли это и годится ли выход.
	ExitID *int64 `json:"exit_id,omitempty"`
}

// UpdateNode меняет заданные поля ноды.
//
// Выключенная нода остаётся в базе: её расход по-прежнему виден, а из подписок
// покупателей она исчезает. Так подозрительный сервер выводится из работы одним
// переключателем, и решение можно отменить — в отличие от удаления, которое
// уносит с собой всю статистику.
func (s *Store) UpdateNode(ctx context.Context, id int64, p UpdateNodeParams) (Node, error) {
	sets := make([]string, 0, 3)
	args := make([]any, 0, 4)

	if p.Name != nil {
		name := strings.TrimSpace(*p.Name)
		if name == "" {
			return Node{}, errors.New("имя ноды не может быть пустым")
		}
		sets = append(sets, "name = ?")
		args = append(args, name)
	}
	if p.Country != nil {
		sets = append(sets, "country = ?")
		args = append(args, countryTitle(*p.Country))
	}
	if p.Enabled != nil {
		sets = append(sets, "enabled = ?")
		args = append(args, boolInt(*p.Enabled))
	}
	if p.Fingerprint != nil {
		fp, err := normalFingerprint(*p.Fingerprint)
		if err != nil {
			return Node{}, err
		}
		sets = append(sets, "fingerprint = ?")
		args = append(args, fp)
	}
	if p.SNIExtra != nil {
		// Основное имя из набора вычёркиваем: «запасные» — это те, что кроме
		// него. Клиент и так сложил бы их вместе, но продавец, глядя в список
		// нод, должен видеть, что у него добавлено, а не то, что он повторил.
		current, err := s.GetNode(ctx, id)
		if err != nil {
			return Node{}, err
		}
		extra := make([]string, 0, len(*p.SNIExtra))
		for _, name := range splitNames(strings.Join(*p.SNIExtra, ",")) {
			if name != current.SNI {
				extra = append(extra, name)
			}
		}
		sets = append(sets, "sni_extra = ?")
		args = append(args, joinNames(extra))
	}
	if p.Address != nil {
		address := strings.TrimSpace(*p.Address)
		// Проверяем форму: бессмысленный адрес уедет в ссылки всех покупателей
		// этой ноды, и узнают об этом они раньше продавца.
		if _, _, err := net.SplitHostPort(address); err != nil {
			return Node{}, fmt.Errorf("адрес ноды %q: нужен вид хост:порт", address)
		}
		sets = append(sets, "address = ?")
		args = append(args, address)
	}
	if p.ExitID != nil {
		sets = append(sets, "exit_id = ?")
		args = append(args, *p.ExitID)
	}
	if len(sets) == 0 {
		return s.GetNode(ctx, id)
	}

	args = append(args, id)
	res, err := s.db.ExecContext(ctx, "UPDATE nodes SET "+join(sets, ", ")+" WHERE id = ?", args...)
	if err != nil {
		return Node{}, fmt.Errorf("обновление ноды: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Node{}, ErrNotFound
	}
	return s.GetNode(ctx, id)
}

// DeleteNode удаляет ноду вместе с её статистикой.
func (s *Store) DeleteNode(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM nodes WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("удаление ноды: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// AuthenticateNode находит ноду по её токену и отмечает, что она на связи.
func (s *Store) AuthenticateNode(ctx context.Context, token string) (Node, error) {
	var (
		n         Node
		quicOn    int
		enabled   int
		lastSeen  sql.NullString
		createdAt string
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT id, name, country, address, sni, public_key, reality_public_key, reality_short_id, ws_path, quic, enabled, last_seen, created_at, version, arch, upgrade_to, COALESCE(exit_id, 0)
		 FROM nodes WHERE token_hash = ?`, HashToken(token)).
		Scan(&n.ID, &n.Name, &n.Country, &n.Address, &n.SNI, &n.PublicKey,
			&n.RealityPublicKey, &n.RealityShortID, &n.WSPath, &quicOn, &enabled, &lastSeen, &createdAt,
			&n.Version, &n.Arch, &n.UpgradeTo, &n.ExitID)
	if errors.Is(err, sql.ErrNoRows) {
		return Node{}, ErrNotFound
	}
	if err != nil {
		return Node{}, err
	}

	n.Enabled = enabled != 0
	n.QUIC = quicOn != 0
	n.LastSeen = parseNullTime(lastSeen)
	n.CreatedAt = parse(createdAt)

	if _, err := s.db.ExecContext(ctx, `UPDATE nodes SET last_seen = ? WHERE id = ?`,
		format(time.Now().UTC()), n.ID); err != nil {
		return Node{}, err
	}
	return n, nil
}

// ReportNodeBuild запоминает версию и разрядность, о которых сообщила нода.
//
// Дошедшая до просьбы нода эту просьбу снимает сама: продавцу не надо
// помнить, кого он просил, — в списке остаются только те, кто ещё в пути.
// Дошедшей считается и нода, ушедшая дальше просьбы: пока она обновлялась,
// мог выйти следующий релиз. Пустую версию не пишем: так отвечают ноды старше
// этой возможности, и затирать ею известное значение незачем.
//
// Отчёт приходит каждые четверть минуты, поэтому пишем, только если что-то
// изменилось.
func (s *Store) ReportNodeBuild(ctx context.Context, id int64, version, arch string) error {
	if version == "" {
		return nil
	}
	var was, wasArch, target string
	if err := s.db.QueryRowContext(ctx, `SELECT version, arch, upgrade_to FROM nodes WHERE id = ?`, id).
		Scan(&was, &wasArch, &target); err != nil {
		return err
	}
	reached := target != "" && !OlderVersion(version, target)
	if was == version && wasArch == arch && !reached {
		return nil
	}
	if reached {
		target = ""
	}
	_, err := s.db.ExecContext(ctx, `UPDATE nodes SET version = ?, arch = ?, upgrade_to = ? WHERE id = ?`,
		version, arch, target, id)
	return err
}

// RequestNodeUpgrade просит обновиться до target все включённые ноды, чья
// версия старше. Отдаёт, скольких попросили.
//
// Старше, а не «другая»: нода, поставленная из свежей сборки, бывает новее
// релиза, и просьба «обновись до релиза» откатила бы её назад. Выключенные не
// трогаем: продавец убрал их из работы, и тихо менять им бинарник, пока он не
// видит, — не наше дело. Ноды без версии старше этой возможности и просьбу не
// услышат: их обновляют один раз руками, и панель это показывает.
func (s *Store) RequestNodeUpgrade(ctx context.Context, target string) (int, error) {
	nodes, err := s.ListNodes(ctx)
	if err != nil {
		return 0, err
	}
	asked := 0
	for _, n := range nodes {
		if !n.Enabled || n.UpgradeTo == target || !OlderVersion(n.Version, target) {
			continue
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE nodes SET upgrade_to = ? WHERE id = ?`, target, n.ID); err != nil {
			return asked, err
		}
		asked++
	}
	return asked, nil
}

// OlderVersion сохраняет API панели, но порядок берёт из общего пакета:
// ноды и приложения должны одинаково замечать новый тестовый выпуск.
// Непонятные версии не обновляем автоматически. Тот же порядок для
// alpha/beta/rc — у older в web/index.html и в internal/updater/agent.sh.
func OlderVersion(a, b string) bool {
	return version.Older(a, b)
}

// NodeUsers собирает список пользователей для конкретной ноды в том же виде,
// в каком нода читает его из файла.
//
// Главная тонкость — общая квота при нескольких нодах. Каждая нода считает
// только свой трафик, поэтому лимит для неё уменьшается на то, что человек уже
// израсходовал на других. Иначе квоту в 100 ГБ можно было бы потратить на
// каждой ноде отдельно. Расход до переезда с чужой панели (used_before) — та
// же «другая нода», только прошлая.
func (s *Store) NodeUsers(ctx context.Context, nodeID int64) ([]users.User, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT u.id, u.enabled, u.expires_at, u.traffic_limit, u.max_ips, u.max_conns, u.speed_limit,
		       u.devices_from, c.kind, c.secret,
		       u.used_before + COALESCE((SELECT SUM(up + down) FROM usage WHERE user_id = u.id AND node_id <> ?), 0) - u.traffic_offset
		FROM users u
		JOIN credentials c ON c.user_id = u.id
		ORDER BY u.id, c.id`, nodeID)
	if err != nil {
		return nil, fmt.Errorf("сборка списка для ноды: %w", err)
	}
	defer rows.Close()

	var list []users.User
	for rows.Next() {
		var (
			userID    int64
			enabled   int
			expires   sql.NullString
			limit     int64
			maxIPs    int
			maxConns  int
			speed     int64
			devices   sql.NullString
			kind      string
			secret    string
			elsewhere int64
		)
		if err := rows.Scan(&userID, &enabled, &expires, &limit, &maxIPs, &maxConns, &speed,
			&devices, &kind, &secret, &elsewhere); err != nil {
			return nil, err
		}

		// Имени покупателя здесь нет намеренно: ноде оно для пропуска не
		// нужно, а изъятую или взломанную ноду оно превращало бы в список
		// клиентов продавца. Аккаунт — номер, и связать его с человеком
		// может только панель.
		u := users.User{
			Kind:     kind,
			Secret:   secret,
			Enabled:  enabled != 0,
			MaxIPs:   maxIPs,
			MaxConns: maxConns,
			// Потолок скорости общий на все ноды: он про то, как быстро, а
			// не про то, сколько всего, и делить его между нодами незачем —
			// клиент всё равно сидит на одной за раз.
			SpeedLimit: speed,
			// Все наборы одного подписчика попадают в один аккаунт: квота,
			// срок и лимит устройств у телефона, ноутбука и записи для
			// чужого приложения общие.
			Account: strconv.FormatInt(userID, 10),
		}
		if t := parseNullTime(expires); t != nil {
			u.ExpiresAt = *t
		}
		// Только время сброса устройств, не адреса: адресов у панели нет.
		if t := parseNullTime(devices); t != nil && maxIPs > 0 {
			u.DevicesFrom = *t
		}

		if limit > 0 {
			remaining := limit - elsewhere
			if remaining <= 0 {
				// Ноль в поле лимита означает «без ограничений», поэтому
				// исчерпавшего квоту выключаем целиком, а не шлём лимит 0.
				u.Enabled = false
				u.TrafficLimit = 0
			} else {
				u.TrafficLimit = remaining
			}
		}
		list = append(list, u)
	}
	return list, rows.Err()
}

// ReportUsage принимает от ноды её счётчики, разложенные по аккаунтам.
//
// Нода присылает накопленные с её стороны итоги, а не приращения: они
// переживают её перезапуск, и повторная доставка того же отчёта ничего не
// испортит. Панель просто складывает итоги всех нод.
func (s *Store) ReportUsage(ctx context.Context, nodeID int64, report map[string]users.Usage) error {
	if len(report) == 0 {
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	stamp := time.Now().UTC()
	now := format(stamp)
	day := stamp.Format("2006-01-02")

	for account, usage := range report {
		userID, err := strconv.ParseInt(account, 10, 64)
		if err != nil {
			// Отчёт не от нашей панели: нода работала по локальному файлу,
			// там идентификатор аккаунта — сам публичный ключ.
			continue
		}

		var exists int
		err = tx.QueryRowContext(ctx, `SELECT 1 FROM users WHERE id = ?`, userID).Scan(&exists)
		if errors.Is(err, sql.ErrNoRows) {
			// Пользователя удалили, пока нода копила отчёт. Не ошибка.
			continue
		}
		if err != nil {
			return err
		}

		// Сколько прошло с прошлого отчёта. Нода присылает накопительный итог,
		// поэтому история суток — это разницы между её отчётами.
		var prevUp, prevDown int64
		err = tx.QueryRowContext(ctx,
			`SELECT up, down FROM usage WHERE user_id = ? AND node_id = ?`, userID, nodeID).
			Scan(&prevUp, &prevDown)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}

		// Времени здесь нет намеренно: столбец updated_at остался от прежней
		// схемы и пишется пустым. По нему читалось бы, когда человек в
		// последний раз был именно на этой ноде, — см. forgetWhichNodeWhen.
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO usage (user_id, node_id, up, down, updated_at) VALUES (?, ?, ?, ?, '')
			ON CONFLICT (user_id, node_id) DO UPDATE SET up = excluded.up, down = excluded.down`,
			userID, nodeID, usage.Up, usage.Down); err != nil {
			return fmt.Errorf("запись расхода: %w", err)
		}

		up, down := delta(prevUp, usage.Up), delta(prevDown, usage.Down)
		if up == 0 && down == 0 {
			continue
		}

		// Прошедший трафик — тоже доказательство, что человек был на связи.
		//
		// Присутствие отмечалось только по отчёту с живыми соединениями, а
		// нода шлёт его раз в пятнадцать секунд. Сессия короче тика в такой
		// отчёт не попадала вовсе, и панель отвечала «не подключался ни
		// разу» про того, у кого расход уже шёл. Проверено на живой
		// установке: 20 КБ трафика и пустое «был на связи».
		//
		// Соединения здесь не трогаем: сейчас он уже мог уйти. Утверждаем
		// ровно то, что знаем, — что он был на связи, а на какой ноде, не
		// запоминаем.
		if _, err := tx.ExecContext(ctx, `UPDATE users SET last_seen = ? WHERE id = ?`, now, userID); err != nil {
			return fmt.Errorf("отметка присутствия по расходу: %w", err)
		}

		// В посуточную историю человек не попадает — только нода и объём.
		//
		// Раньше здесь стоял user_id, и панель накапливала посуточную запись
		// «кто на какой ноде сидел»: Артём каждый день сентября через
		// Финляндию, с четырнадцатого через Турцию. Ни один экран её не
		// читал — все они суммируют по людям, — а изъятие панели выдавало бы
		// эту историю целиком. Панель стоит в юрисдикции продавца и изымается
		// куда легче ноды.
		//
		// Что для этого нужно, осталось: общий расход человека и разделение
		// квоты между нодами считаются по таблице usage, где итог
		// накопительный и дат нет.
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO usage_daily (day, node_id, up, down) VALUES (?, ?, ?, ?)
			ON CONFLICT (day, node_id) DO UPDATE SET up = up + excluded.up, down = down + excluded.down`,
			day, nodeID, up, down); err != nil {
			return fmt.Errorf("запись расхода за сутки: %w", err)
		}
	}
	return tx.Commit()
}

// delta — сколько прибавилось между двумя отчётами ноды.
//
// Счётчик у ноды накопительный, но не вечный: после перезапуска она начинает с
// нуля, а после потери файла расхода — тоже. Тогда «новое минус старое» уходит
// в минус, и без этой проверки сутки получили бы отрицательный расход, а
// график — провал вниз. Считаем, что весь пришедший объём и есть прирост.
func delta(prev, now int64) int64 {
	if now < prev {
		return now
	}
	return now - prev
}

// presenceStale — сколько панель верит последнему отчёту ноды о том, кто
// на связи. Нода отчитывается раз в пятнадцать секунд; шесть пропущенных
// тиков — уже не задержка, а молчание.
const presenceStale = 90 * time.Second

// ReportPresence принимает от ноды срез «кто на связи прямо сейчас».
//
// Срез, а не журнал: строка есть только у того, кто на связи сейчас, и
// каждый отчёт её перезаписывает. Кого в отчёте нет, тот с этой ноды ушёл —
// его строка удаляется. Остаётся только время последней связи у самого
// покупателя: оно и есть ответ на «когда он был в последний раз», а на какой
// ноде — не нужно никому, кроме того, кто восстанавливает его маршрут.
func (s *Store) ReportPresence(ctx context.Context, nodeID int64, presence map[string]users.Presence) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `UPDATE presence SET conns = 0, ips = 0 WHERE node_id = ?`, nodeID); err != nil {
		return fmt.Errorf("сброс присутствия: %w", err)
	}

	now := format(time.Now().UTC())
	for account, p := range presence {
		userID, err := strconv.ParseInt(account, 10, 64)
		if err != nil {
			continue // нода с локальным файлом: там аккаунт — сам ключ
		}
		var exists int
		err = tx.QueryRowContext(ctx, `SELECT 1 FROM users WHERE id = ?`, userID).Scan(&exists)
		if errors.Is(err, sql.ErrNoRows) {
			continue // удалили, пока нода собирала отчёт
		}
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO presence (user_id, node_id, conns, ips) VALUES (?, ?, ?, ?)
			ON CONFLICT (user_id, node_id) DO UPDATE SET conns = excluded.conns, ips = excluded.ips`,
			userID, nodeID, p.Conns, p.IPs); err != nil {
			return fmt.Errorf("запись присутствия: %w", err)
		}
		if p.Conns > 0 {
			if _, err := tx.ExecContext(ctx, `UPDATE users SET last_seen = ? WHERE id = ?`, now, userID); err != nil {
				return fmt.Errorf("отметка последней связи: %w", err)
			}
		}
	}

	// Ушедших с этой ноды не помним вовсе.
	if _, err := tx.ExecContext(ctx, `DELETE FROM presence WHERE node_id = ? AND conns = 0 AND ips = 0`, nodeID); err != nil {
		return fmt.Errorf("уборка присутствия: %w", err)
	}
	return tx.Commit()
}

// PresenceRows — сколько строк присутствия в базе. Нужен тесту, который
// следит, что срез не разросся в журнал.
func (s *Store) PresenceRows(ctx context.Context) int {
	var n int
	_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM presence`).Scan(&n)
	return n
}

func format(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func parse(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

func parseNullTime(v sql.NullString) *time.Time {
	if !v.Valid || v.String == "" {
		return nil
	}
	t := parse(v.String)
	return &t
}

func nullTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return format(*t)
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func join(parts []string, sep string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += sep
		}
		out += p
	}
	return out
}

// ErrAlreadyExists — подписчик с таким ключом продавца уже заведён.
var ErrAlreadyExists = errors.New("подписчик с таким external_id уже есть")

// nullString превращает пустую строку в NULL.
//
// Для уникального индекса это принципиально: NULL в SQLite повторяться может,
// а пустая строка нет. Иначе второй подписчик без ключа продавца упёрся бы в
// уникальность на пустом месте.
func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// UserByExternalID находит подписчика по ключу продавца.
func (s *Store) UserByExternalID(ctx context.Context, externalID string) (User, error) {
	list, err := s.queryUsers(ctx, `WHERE u.external_id = ?`, externalID)
	if err != nil {
		return User{}, err
	}
	if len(list) == 0 {
		return User{}, ErrNotFound
	}
	return list[0], nil
}

// ExtendUser продлевает подписку на заданный срок.
//
// Считается от текущего окончания, а не от «сейчас»: покупатель, продливший за
// неделю до конца, не должен терять эту неделю. Если срок уже вышел или его не
// было вовсе — считаем от текущего момента.
//
// Чтение и запись идут одной транзакцией, и это не украшательство. Продление
// в три раздельных шага «прочитать, посчитать, записать» ломается на двух
// одновременных платежах: оба прочитали бы один и тот же срок, оба записали бы
// одно и то же значение, и один оплаченный месяц пропал бы бесследно.
func (s *Store) ExtendUser(ctx context.Context, id int64, d time.Duration) (User, error) {
	if d <= 0 {
		return User{}, errors.New("срок продления должен быть положительным")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, err
	}
	defer func() { _ = tx.Rollback() }()

	var current sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT expires_at FROM users WHERE id = ?`, id).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, err
	}

	// Считаем от большего из двух: текущего окончания и «сейчас». Первое —
	// чтобы не съесть остаток у того, кто продлевает заранее. Второе — чтобы
	// вернувшийся через полгода не купил месяц, истёкший пять месяцев назад.
	base := time.Now().UTC()
	if current.Valid {
		if t := parse(current.String); t.After(base) {
			base = t
		}
	}

	res, err := tx.ExecContext(ctx,
		`UPDATE users SET expires_at = ? WHERE id = ?`, format(base.Add(d)), id)
	if err != nil {
		return User{}, fmt.Errorf("продление подписки: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM webhook_state WHERE key = ?`, fmt.Sprintf("%s:%d", WebhookUserExpired, id)); err != nil {
		return User{}, err
	}
	if err := queueUserWebhookTx(ctx, tx, id, EventUserRenew); err != nil {
		return User{}, err
	}
	if err := tx.Commit(); err != nil {
		return User{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return User{}, ErrNotFound
	}

	list, err := s.queryUsers(ctx, `WHERE u.id = ?`, id)
	if err != nil {
		return User{}, err
	}
	if len(list) == 0 {
		return User{}, ErrNotFound
	}
	return list[0], nil
}

// Expiry — срок подписки в запросах к API.
//
// Принимает и дату в RFC3339, и «через сколько»: 30d, 12h. Второе — то, чем
// думает бот: он продаёт месяц, а не «до двадцать третьего сентября». Обратно
// всегда уезжает дата — «через месяц» в ответе было бы неправдой уже к моменту,
// когда бот его прочитает.
type Expiry struct{ time.Time }

// UnmarshalJSON разбирает обе записи срока.
func (e *Expiry) UnmarshalJSON(raw []byte) error {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return errors.New("срок подписки: нужна строка вида 30d либо дата RFC3339")
	}
	t, err := ParseExpiry(s)
	if err != nil {
		return err
	}
	if t == nil {
		return errors.New("срок подписки: пустая строка")
	}
	e.Time = *t
	return nil
}

// MarshalJSON отдаёт срок датой.
func (e Expiry) MarshalJSON() ([]byte, error) { return json.Marshal(e.Time) }

// at превращает срок в указатель на время, понимая отсутствие срока.
func (e *Expiry) at() *time.Time {
	if e == nil {
		return nil
	}
	t := e.Time.UTC()
	return &t
}

// DayUsage — сколько прошло за сутки.
type DayUsage struct {
	Day  string `json:"day"`
	Up   int64  `json:"up"`
	Down int64  `json:"down"`
}

// NodeUsage — сколько прошло через ноду за период.
type NodeUsage struct {
	NodeID  int64  `json:"node_id"`
	Name    string `json:"name"`
	Country string `json:"country,omitempty"`
	Up      int64  `json:"up"`
	Down    int64  `json:"down"`
}

// UsageByDay отдаёт расход по суткам за последние days дней.
//
// Дни без трафика возвращаются нулями, а не пропускаются: провал в графике —
// это тоже ответ, и рисовать его надо на своём месте, а не сдвигать соседние
// столбики.
func (s *Store) UsageByDay(ctx context.Context, days int) ([]DayUsage, error) {
	if days <= 0 {
		days = 30
	}

	from := time.Now().UTC().AddDate(0, 0, -(days - 1)).Format("2006-01-02")

	rows, err := s.db.QueryContext(ctx, `
		SELECT day, SUM(up), SUM(down) FROM usage_daily
		WHERE day >= ? GROUP BY day`, from)
	if err != nil {
		return nil, fmt.Errorf("расход по суткам: %w", err)
	}
	defer rows.Close()

	found := make(map[string]DayUsage, days)
	for rows.Next() {
		var d DayUsage
		if err := rows.Scan(&d.Day, &d.Up, &d.Down); err != nil {
			return nil, err
		}
		found[d.Day] = d
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]DayUsage, 0, days)
	for i := days - 1; i >= 0; i-- {
		day := time.Now().UTC().AddDate(0, 0, -i).Format("2006-01-02")
		if d, ok := found[day]; ok {
			out = append(out, d)
			continue
		}
		out = append(out, DayUsage{Day: day})
	}
	return out, nil
}

// UsageByNode отдаёт расход по нодам за последние days дней.
//
// Ноды, которых уже нет, сюда не попадают: их строки уходят вместе с ними по
// внешнему ключу. Это осознанно — статистика по удалённой ноде никому не
// поможет, а удаление и так предупреждает, что уносит учёт с собой.
func (s *Store) UsageByNode(ctx context.Context, days int) ([]NodeUsage, error) {
	if days <= 0 {
		days = 30
	}

	from := time.Now().UTC().AddDate(0, 0, -(days - 1)).Format("2006-01-02")

	rows, err := s.db.QueryContext(ctx, `
		SELECT n.id, n.name, n.country, SUM(u.up), SUM(u.down)
		FROM usage_daily u JOIN nodes n ON n.id = u.node_id
		WHERE u.day >= ?
		GROUP BY n.id, n.name, n.country
		ORDER BY SUM(u.up) + SUM(u.down) DESC`, from)
	if err != nil {
		return nil, fmt.Errorf("расход по нодам: %w", err)
	}
	defer rows.Close()

	var out []NodeUsage
	for rows.Next() {
		var n NodeUsage
		if err := rows.Scan(&n.NodeID, &n.Name, &n.Country, &n.Up, &n.Down); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// ForgetOldUsage выбрасывает историю старше keep дней.
//
// Без этого таблица растёт вечно: строка на ноду и день. Людей в ней нет, но
// вечная история — это база, которая только пухнет там, где её ежедневно
// копируют на свой компьютер.
func (s *Store) ForgetOldUsage(ctx context.Context, keep int) error {
	if keep <= 0 {
		keep = UsageKeepDays
	}

	edge := time.Now().UTC().AddDate(0, 0, -keep).Format("2006-01-02")
	if _, err := s.db.ExecContext(ctx, `DELETE FROM usage_daily WHERE day < ?`, edge); err != nil {
		return fmt.Errorf("очистка истории расхода: %w", err)
	}
	return nil
}

// DailyHistoryColumns перечисляет столбцы посуточной истории расхода.
//
// Нужна тесту: обещание «панель не хранит, кто на какой ноде сидел» проверяется
// по самой таблице, а не по коду, который в неё пишет. Код можно поменять и
// забыть про столбец.
func (s *Store) DailyHistoryColumns() ([]string, error) {
	rows, err := s.db.Query(`SELECT name FROM pragma_table_info('usage_daily')`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// TouchNode отмечает, что нода только что была на связи.
//
// В бою это делает сама нода, приходя за списком пользователей. Отдельный
// метод нужен тестам: поднимать ради отметки живую ноду незачем.
func (s *Store) TouchNode(ctx context.Context, id int64) error {
	return s.TouchNodeAt(ctx, id, time.Now())
}

// TouchNodeAt — то же, но задним числом: тестам нужно изобразить ноду,
// которая давно молчит.
func (s *Store) TouchNodeAt(ctx context.Context, id int64, at time.Time) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE nodes SET last_seen = ? WHERE id = ?`, format(at.UTC()), id)
	if err != nil {
		return fmt.Errorf("отметка ноды: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ExpiryAt собирает срок из времени — тому, кто зовёт панель из кода, а не
// по HTTP.
func ExpiryAt(t time.Time) *Expiry { return &Expiry{Time: t} }

// WithBackupKey задаёт публичный ключ для шифрования копий базы.
//
// Только публичный: приватный на сервере не нужен и быть его здесь не должно.
// Панель умеет зашифровать копию и не умеет её открыть — это и есть смысл.
func (s *Store) WithBackupKey(pub []byte) *Store {
	s.backupKey = pub
	return s
}
