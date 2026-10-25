// Package connguard не даёт нескольким адресам положить сервер потоком
// новых соединений.
//
// Объёмный DDoS — гигабиты мусора в канал — программой на самом сервере не
// лечится: канал забит раньше, чем пакет дошёл до кода. Это дело хостера с
// защитой, CDN и запасных нод. Здесь другое, дешёвое и частое: десяток
// машин открывает тысячи соединений в секунду, и нода тратит память и
// горутины на рукопожатия, которые никогда не закончатся. Против этого
// хватает двух пределов на приёме, до любой криптографии:
//
//   - частота новых соединений с одного адреса. Именно частота, а не число
//     открытых: за одним адресом мобильного оператора (CGNAT) сидят сотни
//     покупателей, и каждый держит свои соединения часами. Предел на
//     открытые срезал бы их всех; предел на частоту видит только того, кто
//     открывает непрерывно;
//   - общее число открытых соединений. Оно держит память сервера в рамках,
//     когда адресов много и каждый по отдельности вежлив.
//
// Отклонённое соединение закрывается сразу, без чтения и без ответа: любой
// ответ — работа, а именно её флуд и добивается.
package connguard

import (
	"log"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// DefaultRate — новых соединений в секунду с одного адреса, в среднем.
	// Клиент при подключении открывает несколько соединений (замер нод,
	// сам туннель, переподключение после смены сети), и за адресом CGNAT
	// таких клиентов сотни. Двадцать в секунду — тысяча двести в минуту —
	// с запасом покрывает утро большого оператора и при этом в сотни раз
	// меньше того, что выдаёт простейший флудер.
	DefaultRate = 20

	// DefaultBurst — сколько соединений адрес может открыть разом, прежде
	// чем начнёт действовать частота. Запас нужен на пики: оператор после
	// аварии возвращает в сеть всех абонентов одновременно, и они
	// переподключаются в одну секунду.
	DefaultBurst = 200

	// DefaultMaxOpen — сколько соединений открыто одновременно. На каждое —
	// горутина и буферы, порядка десятков килобайт; двадцать тысяч — сотни
	// мегабайт, предел машины с гигабайтом памяти. Живых покупателей у одной
	// ноды обычно на порядки меньше.
	DefaultMaxOpen = 20000

	// DefaultMaxTracked — сколько адресов помнит таблица частоты. Запись —
	// около сотни байт, так что таблица не больше нескольких мегабайт, как бы
	// много адресов ни пришло.
	DefaultMaxTracked = 65536

	// reportEvery — как часто в журнал уходит счётчик отклонённых.
	reportEvery = time.Minute

	// sweepEvery — как часто таблица чистится от адресов, которые давно не
	// приходили. Запись, чьё ведро успело наполниться, ничем не отличается
	// от отсутствующей, и держать её незачем.
	sweepEvery = time.Minute
)

// Config — пределы. Нулевые поля означают значения по умолчанию.
type Config struct {
	Rate       float64 // новых соединений в секунду с адреса
	Burst      float64 // запас на пик
	MaxOpen    int     // открытых соединений всего
	MaxTracked int     // адресов в таблице частоты

	// Exempt — адреса, которых частота не касается. Это входы каскада и узлы
	// CDN: за одним таким адресом приходят все покупатели входа или узла, и
	// предел одного адреса резал бы их всех разом. Общий предел открытых на
	// них действует, как на всех.
	Exempt func(net.IP) bool
}

// Limits — то, что продавец меняет из панели на ходу, без перезапуска ноды.
// Нулевые поля — значения по умолчанию, как в Config.
type Limits struct {
	Rate    float64
	Burst   float64
	MaxOpen int

	// Blocked — адреса и сети, которым нода не отвечает вовсе: соединение
	// закрывается до чтения, как отклонённое за частоту. Это ответ на
	// «нас долбят вот с этих адресов» — продавец видит их у хостера или в
	// своём журнале и закрывает, не заходя на сервер.
	Blocked []netip.Prefix
}

// Stats — сколько соединений отклонено с прошлого вызова Take. Только
// числа: адреса нода не копит и панели не отдаёт.
type Stats struct {
	ByRate  int64 `json:"by_rate"`
	ByOpen  int64 `json:"by_open"`
	ByBlock int64 `json:"by_block"`
}

// Guard — приём с пределами, которые можно менять на ходу.
type Guard interface {
	net.Listener
	// Update ставит новые пределы. Уже открытые соединения не трогаются:
	// новый предел открытых действует на следующие.
	Update(Limits)
	// Take отдаёт счётчики отклонённых и обнуляет их.
	Take() Stats
}

// Listen оборачивает слушатель пределами cfg.
//
// Ставится первым, над сырым TCP: до заголовка PROXY, до TLS и REALITY.
// Каждый слой выше тратит на соединение больше, и отсечь флуд надо раньше,
// чем он до них дошёл.
func Listen(inner net.Listener, cfg Config) Guard {
	if cfg.MaxTracked <= 0 {
		cfg.MaxTracked = DefaultMaxTracked
	}
	l := &listener{
		Listener: inner,
		exempt:   cfg.Exempt,
		now:      time.Now,
		table: table{
			max:     cfg.MaxTracked,
			buckets: make(map[netip.Addr]bucket),
		},
		done: make(chan struct{}),
	}
	l.Update(Limits{Rate: cfg.Rate, Burst: cfg.Burst, MaxOpen: cfg.MaxOpen})
	go l.report()
	return l
}

func (l *listener) Update(lim Limits) {
	if lim.Rate <= 0 {
		lim.Rate = DefaultRate
	}
	if lim.Burst < 1 {
		lim.Burst = DefaultBurst
	}
	if lim.MaxOpen <= 0 {
		lim.MaxOpen = DefaultMaxOpen
	}
	l.table.mu.Lock()
	l.table.rate, l.table.burst = lim.Rate, lim.Burst
	l.table.mu.Unlock()
	l.maxOpen.Store(int64(lim.MaxOpen))
	blocked := append([]netip.Prefix(nil), lim.Blocked...)
	l.blocked.Store(&blocked)
}

func (l *listener) Take() Stats {
	return Stats{ByRate: l.totRate.Swap(0), ByOpen: l.totOpen.Swap(0), ByBlock: l.totBlock.Swap(0)}
}

type listener struct {
	net.Listener
	exempt  func(net.IP) bool
	maxOpen atomic.Int64
	blocked atomic.Pointer[[]netip.Prefix]
	now     func() time.Time
	table   table

	open    atomic.Int64 // открыто сейчас
	byRate  atomic.Int64 // отклонено за частоту с начала минуты
	byOpen  atomic.Int64 // отклонено сверх предела открытых
	byBlock atomic.Int64 // отклонено по списку закрытых адресов

	// Те же счётчики для панели: журнал обнуляет свои раз в минуту, а
	// панель забирает свои с каждым отчётом, и делить одни на двоих значило
	// бы, что один из них недосчитается.
	totRate, totOpen, totBlock atomic.Int64

	done chan struct{}
	once sync.Once
}

// Accept отдаёт следующее соединение, прошедшее оба предела. Отклонённые
// закрываются здесь же: проверка — одна запись в таблице под замком, так что
// цикл приёма не задерживается и под флудом.
func (l *listener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if l.isBlocked(c.RemoteAddr()) {
			l.byBlock.Add(1)
			l.totBlock.Add(1)
			drop(c)
			continue
		}
		if !l.allow(c.RemoteAddr()) {
			l.byRate.Add(1)
			l.totRate.Add(1)
			drop(c)
			continue
		}
		if l.open.Add(1) > l.maxOpen.Load() {
			l.open.Add(-1)
			l.byOpen.Add(1)
			l.totOpen.Add(1)
			drop(c)
			continue
		}
		return &conn{Conn: c, release: func() { l.open.Add(-1) }}, nil
	}
}

// isBlocked — адрес в списке закрытых. Список действует и на входы каскада
// с узлами CDN: закрыть их — решение продавца, а не случайность частоты.
func (l *listener) isBlocked(addr net.Addr) bool {
	list := l.blocked.Load()
	if list == nil || len(*list) == 0 {
		return false
	}
	tcp, ok := addr.(*net.TCPAddr)
	if !ok {
		return false
	}
	a, ok := netip.AddrFromSlice(tcp.IP)
	if !ok {
		return false
	}
	a = a.Unmap()
	for _, p := range *list {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

func (l *listener) allow(addr net.Addr) bool {
	tcp, ok := addr.(*net.TCPAddr)
	if !ok {
		return true
	}
	if l.exempt != nil && l.exempt(tcp.IP) {
		return true
	}
	key, ok := keyOf(tcp.IP)
	if !ok {
		return true
	}
	return l.table.take(key, l.now())
}

func (l *listener) Close() error {
	l.once.Do(func() { close(l.done) })
	return l.Listener.Close()
}

// report раз в минуту пишет, сколько отклонено. Только числа: адреса в
// журнал не идут, как и везде в проекте (docs/privacy.md). Продавцу,
// который разбирается с жалобами, хватает знать, что флуд был и сколько.
//
// Заодно чистится таблица. Чистка при приёме есть и так, но на затихшей
// ноде приёма нет, а обещание «адрес забывается через минуту-две» должно
// держаться и тогда.
func (l *listener) report() {
	t := time.NewTicker(reportEvery)
	defer t.Stop()
	for {
		select {
		case <-l.done:
			return
		case now := <-t.C:
			l.flush()
			l.table.expire(now)
		}
	}
}

func (l *listener) flush() {
	rate, open, block := l.byRate.Swap(0), l.byOpen.Swap(0), l.byBlock.Swap(0)
	if rate+open+block == 0 {
		return
	}
	log.Printf("за минуту отклонено соединений: %d — слишком часто с одного адреса, %d — сверх предела в %d открытых, %d — из закрытых адресов",
		rate, open, l.maxOpen.Load(), block)
}

// drop закрывает отклонённое соединение сбросом, а не вежливым FIN. После
// FIN сокет ещё минуту висит у ядра в TIME_WAIT, и под флудом таких
// набираются десятки тысяч; сброс освобождает его сразу. Честному клиенту,
// упёршемуся в предел, всё равно: соединения не было, он попробует снова
// или другую ноду.
func drop(c net.Conn) {
	if tcp, ok := c.(*net.TCPConn); ok {
		_ = tcp.SetLinger(0)
	}
	_ = c.Close()
}

// keyOf — чей это адрес. IPv6 считается по сети /64: её получает один
// абонент, и внутри неё он меняет адреса когда угодно. Считай мы по полному
// адресу, флудер с одним роутером обходил бы предел перебором адресов.
func keyOf(ip net.IP) (netip.Addr, bool) {
	a, ok := netip.AddrFromSlice(ip)
	if !ok {
		return netip.Addr{}, false
	}
	a = a.Unmap()
	if a.Is6() {
		p, err := a.Prefix(64)
		if err != nil {
			return netip.Addr{}, false
		}
		return p.Addr(), true
	}
	return a, true
}

// table — вёдра токенов по адресам.
type table struct {
	mu      sync.Mutex
	rate    float64
	burst   float64
	max     int
	buckets map[netip.Addr]bucket
	swept   time.Time
}

type bucket struct {
	tokens float64
	at     time.Time
}

// take списывает одно соединение с ведра адреса.
//
// Когда таблица полна и чистить в ней нечего, новый адрес пропускается без
// учёта. Выбор между «пустить» и «отказать» тут не симметричный: отказ
// означал бы, что тот, у кого шестьдесят пять тысяч адресов, запирает ноду
// от всех остальных. Пропуск оставляет его общему пределу открытых.
func (t *table) take(key netip.Addr, now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if now.Sub(t.swept) >= sweepEvery {
		t.sweep(now)
	}
	b, ok := t.buckets[key]
	if ok {
		b.tokens = min(t.burst, b.tokens+now.Sub(b.at).Seconds()*t.rate)
	} else {
		if len(t.buckets) >= t.max {
			// Чистка проходит всю таблицу; под потоком новых адресов чаще
			// раза в секунду её не повторяем, иначе она съест сам приём.
			if now.Sub(t.swept) >= time.Second {
				t.sweep(now)
			}
			if len(t.buckets) >= t.max {
				return true
			}
		}
		b.tokens = t.burst
	}
	b.at = now
	if b.tokens < 1 {
		t.buckets[key] = b
		return false
	}
	b.tokens--
	t.buckets[key] = b
	return true
}

// sweep убирает адреса, чьи вёдра уже полны.
func (t *table) sweep(now time.Time) {
	for key, b := range t.buckets {
		if b.tokens+now.Sub(b.at).Seconds()*t.rate >= t.burst {
			delete(t.buckets, key)
		}
	}
	t.swept = now
}

func (t *table) expire(now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.sweep(now)
}

func (t *table) size() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.buckets)
}

// conn — принятое соединение. Его закрытие освобождает место под общим
// пределом.
type conn struct {
	net.Conn
	once    sync.Once
	release func()
}

// Close освобождает место ровно один раз: соединение закрывают и relay, и
// TLS, и обработчик, каждый на всякий случай, и счётчик, уменьшенный
// дважды, со временем пустил бы сверх предела.
func (c *conn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
}

// CloseWrite пробрасывается к сокету. Без него половинное закрытие в relay
// стало бы полным, а REALITY (github.com/xtls/reality) приводит соединение к
// интерфейсу с CloseWrite без проверки и упал бы на первом же покупателе.
func (c *conn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return c.Close()
}
