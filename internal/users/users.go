// Package users ведёт учёт клиентов ноды: кого пускать, сколько ему можно и
// сколько он уже израсходовал.
//
// Нода намеренно не знает, кто такой пользователь. У неё есть публичный ключ,
// лимиты к нему и счётчики. Ни имени, ни телефона, ни истории посещений:
// изъятие сервера не должно выдавать людей.
package users

import (
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// Причины отказа. Наружу они не уходят: клиент в любом случае получает
// сайт-прикрытие, иначе разница в реакции сама становится признаком.
var (
	ErrUnknown       = errors.New("ключ не зарегистрирован")
	ErrDisabled      = errors.New("доступ отключён")
	ErrExpired       = errors.New("срок подписки истёк")
	ErrQuotaExceeded = errors.New("исчерпана квота трафика")
	ErrTooManyIPs    = errors.New("превышено число устройств")
	ErrTooManyConns  = errors.New("превышено число соединений")
)

// User — запись о клиенте, как её задаёт владелец ноды или панель.
type User struct {
	// Kind — вид учётных данных: vp1, vless или trojan.
	// Пусто означает vp1: это умолчание и основной протокол ноды.
	Kind string `json:"kind,omitempty"`

	// Secret — сам секрет: публичный ключ для vp1, UUID для vless,
	// пароль для trojan.
	Secret string `json:"secret,omitempty"`

	// PublicKey — прежнее имя поля Secret, оставлено для совместимости с уже
	// написанными файлами и примерами в документации.
	PublicKey string `json:"public_key,omitempty"`

	// Label — человекочитаемая пометка для владельца ноды. На работу не влияет.
	Label string `json:"label,omitempty"`

	// Enabled — выключенный пользователь не пускается, но остаётся в списке
	// вместе со статистикой. Для «приостановить до оплаты».
	Enabled bool `json:"enabled"`

	// ExpiresAt — конец подписки. Нулевое значение означает «бессрочно».
	ExpiresAt time.Time `json:"expires_at,omitempty"`

	// TrafficLimit — сколько байт всего можно. 0 означает «без лимита».
	TrafficLimit int64 `json:"traffic_limit,omitempty"`

	// MaxIPs — со скольких разных адресов можно подключаться в пределах окна.
	// 0 означает «без ограничения».
	//
	// Это ответ на главную боль продавца: один купил, раздал конфиг десяти
	// друзьям. Считать устройства напрямую нельзя — все они предъявляют один
	// и тот же ключ, — а вот разные IP видно.
	MaxIPs int `json:"max_ips,omitempty"`

	// MaxConns — сколько одновременных соединений до ноды разрешено.
	// 0 означает «без ограничения». С мультиплексированием одно устройство
	// открывает до четырёх, так что ставить сюда единицу нельзя.
	MaxConns int `json:"max_conns,omitempty"`

	// SpeedLimit — потолок скорости в байтах в секунду. 0 означает «без
	// ограничения».
	//
	// Квота по объёму отвечает на «сколько всего», а этот — на «как быстро»:
	// без него дешёвый тариф и дорогой отличаются только цифрой в гигабайтах,
	// и продать «100 Мбит/с» отдельно нельзя. Потолок общий на аккаунт, а не
	// на соединение: клиент держит пул из нескольких сессий, и лимит на
	// каждую означал бы кратно больший потолок на деле.
	//
	// Считается по тем же байтам, что и квота, — по тому, что реально ушло в
	// сеть вместе с добивкой. Иначе потолок «100 Мбит/с» на проводе давал бы
	// заметно больше.
	SpeedLimit int64 `json:"speed_limit,omitempty"`

	// Account связывает несколько ключей в один аккаунт.
	//
	// У человека обычно телефон и ноутбук, и у каждого свой ключ: так их видно
	// по отдельности и любой отзывается отдельно. Но квота, срок и лимит
	// устройств у них общие — иначе достаточно выпустить второй ключ, чтобы
	// удвоить себе трафик. Пусто означает «ключ сам себе аккаунт».
	Account string `json:"account,omitempty"`

	// DevicesFrom — адреса, с которых приходили раньше этого времени, в
	// лимит устройств не считаются. Это «сбросить устройства» из панели:
	// человек сменил телефон и упёрся в лимит, а старый адрес забылся бы
	// сам только через час. Нода получает только время, не адреса.
	DevicesFrom time.Time `json:"devices_from,omitempty"`
}

// Normalized приводит запись к каноническому виду: подставляет умолчание для
// вида и переносит секрет из устаревшего поля.
func (u User) Normalized() User {
	if u.Kind == "" {
		u.Kind = KindVP1
	}
	if u.Secret == "" {
		u.Secret = u.PublicKey
	}
	if u.PublicKey == "" && u.Kind == KindVP1 {
		u.PublicKey = u.Secret
	}
	return u
}

// AccountID возвращает идентификатор аккаунта, к которому относится ключ.
//
// Пусто означает «ключ сам себе аккаунт» — так работает локальный файл, где
// никаких аккаунтов нет. Панель проставляет сюда идентификатор подписчика,
// и тогда телефон, ноутбук и запись для чужого клиента делят одну квоту.
func (u User) AccountID() string {
	if u.Account != "" {
		return u.Account
	}
	return u.Kind + "|" + u.Secret
}

// Usage — накопленный расход.
type Usage struct {
	Up   int64 `json:"up"`
	Down int64 `json:"down"`
}

// Total — сколько всего израсходовано.
func (u Usage) Total() int64 { return u.Up + u.Down }

// Presence — кто на связи прямо сейчас: сколько соединений у аккаунта и со
// скольких адресов он приходил за окно.
//
// Это не журнал, а срез: нода и так держит эти два числа ради лимитов
// соединений и устройств, и панели они уезжают как есть — без адресов и
// без истории. Адреса считаются только у тех, кому задан лимит устройств:
// остальным нода их не запоминает вовсе, и IPs у них всегда ноль.
type Presence struct {
	Conns int `json:"conns"`
	IPs   int `json:"ips"`
}

// ipWindow — сколько помним адрес, с которого подключались.
//
// Час выбран как компромисс: меньше — и человек, переехавший с Wi-Fi на
// мобильную сеть, упрётся в лимит на ровном месте; больше — и раздача
// конфига друзьям слишком долго остаётся незамеченной.
const ipWindow = time.Hour

// account — состояние одного аккаунта в памяти ноды. Ключей у аккаунта может
// быть несколько, а счётчики, соединения и адреса общие.
type account struct {
	id    string
	user  User
	usage Usage
	conns int
	ips   map[string]time.Time

	// limiter — потолок скорости, общий на все соединения аккаунта.
	// nil означает «без ограничения».
	limiter *rate.Limiter
}

// burstFor — размер ведра под заданную скорость.
//
// Четверть секунды запаса: с меньшим ведром страница грузится рывками, потому
// что каждый кадр ждёт токенов, с большим — первые секунды идут мимо потолка
// и человек видит скорость, которой не покупал.
//
// Нижняя граница — не про отказ, а про рывки: ведро меньше кадра VP1 (16 КиБ)
// заставляло бы ждать токенов каждый отдельный кадр. Просить больше ведра за
// раз безопасно: metered.Conn берёт частями по его размеру.
func burstFor(bytesPerSec int64) int {
	burst := bytesPerSec / 4
	if burst < minBurst {
		burst = minBurst
	}
	return int(burst)
}

// minBurst — ведро не меньше 64 КиБ: вчетверо больше самого крупного кадра.
const minBurst = 64 << 10

// limiterFor собирает ограничитель под скорость аккаунта.
func limiterFor(bytesPerSec int64) *rate.Limiter {
	if bytesPerSec <= 0 {
		return nil
	}
	return rate.NewLimiter(rate.Limit(bytesPerSec), burstFor(bytesPerSec))
}

// Registry хранит аккаунты и их расход.
type Registry struct {
	mu sync.Mutex

	// byKey — публичный ключ клиента (сырые байты) → аккаунт. Несколько
	// ключей одного человека указывают на одну и ту же запись.
	byKey map[string]*account

	// byAccount — идентификатор аккаунта → та же запись. Нужен, чтобы
	// восстанавливать расход и отдавать статистику.
	byAccount map[string]*account

	now func() time.Time
}

// NewRegistry собирает реестр из списка пользователей.
func NewRegistry(list []User) (*Registry, error) {
	r := &Registry{
		byKey:     make(map[string]*account, len(list)),
		byAccount: make(map[string]*account, len(list)),
		now:       time.Now,
	}
	if err := r.Replace(list); err != nil {
		return nil, err
	}
	return r, nil
}

// Replace заменяет список пользователей, сохраняя накопленную статистику тех,
// кто остался. Вызывается при перечитывании файла и при обновлении с панели.
func (r *Registry) Replace(list []User) error {
	nextKeys := make(map[string]*account, len(list))
	nextAccounts := make(map[string]*account, len(list))

	for i, raw := range list {
		u := raw.Normalized()

		identity, err := Identity(u.Kind, u.Secret)
		if err != nil {
			return fmt.Errorf("пользователь %d (%s): %w", i+1, u.Label, err)
		}

		id := u.AccountID()
		acc, ok := nextAccounts[id]
		if !ok {
			acc = &account{id: id, user: u, ips: make(map[string]time.Time), limiter: limiterFor(u.SpeedLimit)}
			nextAccounts[id] = acc
		}
		nextKeys[credentialKey(u.Kind, identity)] = acc
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	// Переносим расход, живые соединения и известные адреса к новым записям.
	for id, old := range r.byAccount {
		fresh, ok := nextAccounts[id]
		if !ok {
			continue
		}
		fresh.usage = old.usage
		fresh.conns = old.conns
		// Адреса переносим, только пока по ним есть что считать. Лимит
		// устройств сняли — и адреса уходят сразу, а не доживают до конца
		// окна: держать их нода вправе лишь ради этого лимита.
		if fresh.user.MaxIPs > 0 {
			fresh.ips = old.ips
			for ip, seen := range fresh.ips {
				if seen.Before(fresh.user.DevicesFrom) {
					delete(fresh.ips, ip)
				}
			}
		}
		// Ограничитель переносим вместе с накопленными токенами, пока
		// скорость не изменилась: новый начал бы с полного ведра, и человек
		// на каждое перечитывание списка получал бы бесплатный рывок.
		if old.limiter != nil && fresh.limiter != nil && old.limiter.Limit() == fresh.limiter.Limit() {
			fresh.limiter = old.limiter
		}
	}
	r.byKey = nextKeys
	r.byAccount = nextAccounts
	return nil
}

// Session — принятое соединение конкретного пользователя.
type Session struct {
	registry *Registry
	id       string
	closed   bool

	// limiter — потолок скорости аккаунта, общий на все его соединения.
	limiter *rate.Limiter
}

// Admit решает, пускать ли соединение, и открывает сессию.
//
// identity — то, что нода вычитала из провода: публичный ключ клиента для VP1,
// UUID для VLESS, двоичный хеш пароля для Trojan.
func (r *Registry) Admit(kind string, identity []byte, remote net.Addr) (*Session, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	acc, ok := r.byKey[credentialKey(kind, identity)]
	if !ok {
		return nil, ErrUnknown
	}
	if !acc.user.Enabled {
		return nil, ErrDisabled
	}

	now := r.now()
	if !acc.user.ExpiresAt.IsZero() && now.After(acc.user.ExpiresAt) {
		return nil, ErrExpired
	}
	if acc.user.TrafficLimit > 0 && acc.usage.Total() >= acc.user.TrafficLimit {
		return nil, ErrQuotaExceeded
	}
	if acc.user.MaxConns > 0 && acc.conns >= acc.user.MaxConns {
		return nil, ErrTooManyConns
	}

	if acc.user.MaxIPs > 0 {
		host := hostOf(remote)
		acc.pruneIPs(now)
		if _, seen := acc.ips[host]; !seen && len(acc.ips) >= acc.user.MaxIPs {
			return nil, ErrTooManyIPs
		}
		acc.ips[host] = now
	}

	acc.conns++
	return &Session{registry: r, id: acc.id, limiter: acc.limiter}, nil
}

// Limiter отдаёт потолок скорости аккаунта или nil, если его нет.
//
// Один объект на все соединения аккаунта: клиент держит пул сессий, и
// отдельный потолок на каждую означал бы кратно больший потолок на деле.
func (s *Session) Limiter() *rate.Limiter {
	if s == nil {
		return nil
	}
	return s.limiter
}

// Add записывает израсходованные байты и сообщает, не пора ли отключать.
func (s *Session) Add(up, down int64) (overQuota bool) {
	if s == nil {
		return false
	}
	s.registry.mu.Lock()
	defer s.registry.mu.Unlock()

	acc, ok := s.registry.byAccount[s.id]
	if !ok {
		// Пользователя удалили из списка, пока он был подключён.
		return true
	}
	acc.usage.Up += up
	acc.usage.Down += down

	return acc.user.TrafficLimit > 0 && acc.usage.Total() >= acc.user.TrafficLimit
}

// Label возвращает пометку пользователя — для журнала.
func (s *Session) Label() string {
	if s == nil {
		return ""
	}
	s.registry.mu.Lock()
	defer s.registry.mu.Unlock()
	if acc, ok := s.registry.byAccount[s.id]; ok {
		return acc.user.Label
	}
	return ""
}

// Valid сообщает, можно ли продолжать обслуживать уже открытую сессию.
//
// Проверять только при подключении недостаточно. Сессия живёт часами: человек
// подключился утром, днём продавец отключил его за неоплату — и до вечера
// ничего не изменится, потому что переподключаться клиенту незачем. Ровно так
// утекает выручка. Нода обязана перепроверять состояние периодически.
func (s *Session) Valid() error {
	if s == nil {
		return nil
	}
	s.registry.mu.Lock()
	defer s.registry.mu.Unlock()

	acc, ok := s.registry.byAccount[s.id]
	if !ok {
		// Пользователя удалили или отозвали все его ключи.
		return ErrUnknown
	}
	if !acc.user.Enabled {
		return ErrDisabled
	}
	if !acc.user.ExpiresAt.IsZero() && s.registry.now().After(acc.user.ExpiresAt) {
		return ErrExpired
	}
	if acc.user.TrafficLimit > 0 && acc.usage.Total() >= acc.user.TrafficLimit {
		return ErrQuotaExceeded
	}
	return nil
}

// Close закрывает сессию. Безопасно вызывать несколько раз.
func (s *Session) Close() {
	if s == nil || s.closed {
		return
	}
	s.closed = true

	s.registry.mu.Lock()
	defer s.registry.mu.Unlock()
	if acc, ok := s.registry.byAccount[s.id]; ok && acc.conns > 0 {
		acc.conns--
	}
}

// Stat — сводка по одному аккаунту.
type Stat struct {
	Account string `json:"account"`
	Label   string `json:"label,omitempty"`
	Usage   Usage  `json:"usage"`
	Conns   int    `json:"conns"`
	IPs     int    `json:"ips"`
}

// Stats отдаёт срез состояния — для журнала, выгрузки и, позже, панели.
func (r *Registry) Stats() []Stat {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := r.now()
	out := make([]Stat, 0, len(r.byAccount))
	for _, acc := range r.byAccount {
		acc.pruneIPs(now)
		out = append(out, Stat{
			Account: acc.id,
			Label:   acc.user.Label,
			Usage:   acc.usage,
			Conns:   acc.conns,
			IPs:     len(acc.ips),
		})
	}
	return out
}

// HasCredential сообщает, знает ли нода такие учётные данные.
//
// Нужен диспетчеру: у VLESS и VP1 первый байт может совпадать, и различить их
// удаётся только поиском UUID в списке. Ответ не говорит, пустят ли клиента —
// это решает Admit, где проверяются срок, квота и лимиты.
func (r *Registry) HasCredential(kind string, identity []byte) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.byKey[credentialKey(kind, identity)]
	return ok
}

// Len возвращает число зарегистрированных аккаунтов.
func (r *Registry) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.byAccount)
}

// RestoreUsage возвращает счётчики на место после перезапуска ноды.
func (r *Registry) RestoreUsage(saved map[string]Usage) {
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, acc := range r.byAccount {
		if u, ok := saved[acc.id]; ok {
			acc.usage = u
		}
	}
}

func (a *account) pruneIPs(now time.Time) {
	deadline := now.Add(-ipWindow)
	for ip, seen := range a.ips {
		if seen.Before(deadline) {
			delete(a.ips, ip)
		}
	}
}

// hostOf достаёт адрес без порта: подключения с одного устройства приходят
// с разных портов, и считать их за разные устройства нельзя.
func hostOf(addr net.Addr) string {
	if addr == nil {
		return ""
	}
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		return addr.String()
	}
	return host
}
