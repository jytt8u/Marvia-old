package connguard

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// queue отдаёт подложенные соединения, а когда они кончились — errDrained.
// Настоящий TCP здесь не нужен: проверяются пределы, а не сокеты, а тысяча
// настоящих соединений с разных адресов на одной машине не собирается.
// Ошибка, а не ожидание: обёртка вернёт её наружу, и тест узнает, что всё
// подложенное разобрано — принято или отклонено.
type queue struct {
	mu    sync.Mutex
	conns []net.Conn
}

var errDrained = errors.New("подложенные соединения кончились")

func newQueue(conns ...net.Conn) *queue { return &queue{conns: conns} }

func (q *queue) push(conns ...net.Conn) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.conns = append(q.conns, conns...)
}

func (q *queue) Accept() (net.Conn, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.conns) == 0 {
		return nil, errDrained
	}
	c := q.conns[0]
	q.conns = q.conns[1:]
	return c, nil
}
func (q *queue) Close() error   { return nil }
func (q *queue) Addr() net.Addr { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 443} }

type fakeConn struct {
	net.Conn
	from   net.Addr
	closed atomic.Int32
}

func from(ip string) *fakeConn {
	return &fakeConn{from: &net.TCPAddr{IP: net.ParseIP(ip), Port: 40000}}
}

func (c *fakeConn) RemoteAddr() net.Addr { return c.from }
func (c *fakeConn) Close() error         { c.closed.Add(1); return nil }

// acceptAll принимает всё, что пропустит обёртка, пока подложенное не кончится.
func acceptAll(t *testing.T, l net.Listener) []net.Conn {
	t.Helper()
	var got []net.Conn
	for {
		c, err := l.Accept()
		if errors.Is(err, errDrained) {
			return got
		}
		if err != nil {
			t.Fatalf("accept: %v", err)
		}
		got = append(got, c)
	}
}

// guarded собирает обёртку с остановленными часами: пока время не идёт,
// ведро не пополняется, и запас виден ровно.
func guarded(inner net.Listener, cfg Config) *listener {
	l := Listen(inner, cfg).(*listener)
	at := time.Unix(1_700_000_000, 0)
	l.now = func() time.Time { return at }
	return l
}

// later переводит часы обёртки вперёд.
func later(l *listener, by time.Duration) time.Time {
	at := l.now().Add(by)
	l.now = func() time.Time { return at }
	return at
}

func remoteOf(c net.Conn) string { return c.RemoteAddr().(*net.TCPAddr).IP.String() }

// TestFloodFromOneAddressIsCutWhileAnotherStillConnects — адрес, открывающий
// соединения непрерывно, упирается в свой запас, а соседний адрес в это же
// время подключается как ни в чём не бывало.
func TestFloodFromOneAddressIsCutWhileAnotherStillConnects(t *testing.T) {
	inner := newQueue()
	var flood []*fakeConn
	for range 500 {
		c := from("203.0.113.7")
		flood = append(flood, c)
		inner.push(c)
	}
	inner.push(from("198.51.100.20"))
	l := guarded(inner, Config{Rate: 20, Burst: 200})
	defer l.Close()

	var fromFlood, fromBuyer int
	for _, c := range acceptAll(t, l) {
		switch remoteOf(c) {
		case "203.0.113.7":
			fromFlood++
		case "198.51.100.20":
			fromBuyer++
		}
	}
	if fromFlood != 200 {
		t.Errorf("с флудящего адреса пропущено %d соединений, запас — 200", fromFlood)
	}
	if fromBuyer != 1 {
		t.Errorf("соседний адрес не подключился во время флуда")
	}
	var dropped int
	for _, c := range flood[200:] {
		if c.closed.Load() == 1 {
			dropped++
		}
	}
	if dropped != 300 {
		t.Errorf("отклонённых закрыто %d из 300: остальные висят открытыми", dropped)
	}
}

// TestRateRefillsWithTime — упёршийся в предел адрес не заперт навсегда:
// через секунду ему снова доступно столько соединений, сколько велит частота.
func TestRateRefillsWithTime(t *testing.T) {
	inner := newQueue()
	l := guarded(inner, Config{Rate: 20, Burst: 200})
	defer l.Close()

	accepted := func(n int) int {
		for range n {
			inner.push(from("203.0.113.7"))
		}
		return len(acceptAll(t, l))
	}
	if got := accepted(300); got != 200 {
		t.Fatalf("первая партия: пропущено %d, ждали весь запас 200", got)
	}
	later(l, time.Second)
	if got := accepted(100); got != 20 {
		t.Fatalf("через секунду пропущено %d, частота — 20 в секунду", got)
	}
}

// TestRelayEntryIsNotLimitedByAddress — вход каскада приводит всех своих
// покупателей с одного адреса, и предел частоты его не касается.
func TestRelayEntryIsNotLimitedByAddress(t *testing.T) {
	inner := newQueue()
	for range 1000 {
		inner.push(from("203.0.113.5"))
	}
	exempt := func(ip net.IP) bool { return ip.Equal(net.ParseIP("203.0.113.5")) }
	l := guarded(inner, Config{Rate: 20, Burst: 200, Exempt: exempt})
	defer l.Close()

	if got := len(acceptAll(t, l)); got != 1000 {
		t.Fatalf("от входа каскада пропущено %d соединений из 1000", got)
	}
}

// TestNeighboursInOneIPv6NetworkShareALimit — адреса одной сети /64
// считаются одним абонентом: иначе флудер обходил бы предел, меняя адрес
// внутри своей сети.
func TestNeighboursInOneIPv6NetworkShareALimit(t *testing.T) {
	inner := newQueue()
	for i := range 300 {
		inner.push(from(fmt.Sprintf("2001:db8:1:2::%x", i+1)))
	}
	l := guarded(inner, Config{Rate: 20, Burst: 200})
	defer l.Close()

	if got := len(acceptAll(t, l)); got != 200 {
		t.Fatalf("из одной сети /64 пропущено %d соединений, запас — 200", got)
	}
}

// TestOpenConnectionsNeverExceedTheLimitAndCounterDoesNotLeak — открытых
// соединений не больше предела; закрытое освобождает место ровно один раз,
// сколько бы раз его ни закрывали.
func TestOpenConnectionsNeverExceedTheLimitAndCounterDoesNotLeak(t *testing.T) {
	inner := newQueue()
	l := guarded(inner, Config{MaxOpen: 3})
	defer l.Close()

	// next подкладывает одно соединение и говорит, пропущено ли оно.
	next := func(ip string) (net.Conn, *fakeConn) {
		raw := from(ip)
		inner.push(raw)
		got := acceptAll(t, l)
		if len(got) == 0 {
			return nil, raw
		}
		return got[0], raw
	}

	var open []net.Conn
	for i := range 3 {
		c, _ := next(fmt.Sprintf("198.51.100.%d", i+1))
		if c == nil {
			t.Fatalf("соединение %d не пропущено при свободном месте", i+1)
		}
		open = append(open, c)
	}
	extra, raw := next("198.51.100.9")
	if extra != nil {
		t.Fatal("четвёртое соединение пропущено сверх предела в три")
	}
	if raw.closed.Load() != 1 {
		t.Fatal("соединение сверх предела не закрыто")
	}

	// Двойное закрытие — обычное дело: его закрывают и relay, и обработчик.
	_ = open[0].Close()
	_ = open[0].Close()
	if n := l.open.Load(); n != 2 {
		t.Fatalf("после закрытия одного открыто по счётчику %d, ждали 2", n)
	}
	again, _ := next("198.51.100.10")
	if again == nil {
		t.Fatal("освободившееся место не досталось новому соединению")
	}
	if extra, _ := next("198.51.100.11"); extra != nil {
		t.Fatal("двойное закрытие освободило лишнее место")
	}
	_ = again.Close()
	for _, c := range open[1:] {
		_ = c.Close()
	}
	if n := l.open.Load(); n != 0 {
		t.Fatalf("все закрыты, а по счётчику открыто %d", n)
	}
}

// TestAddressTableStaysBounded — сколько бы адресов ни пришло, таблица не
// растёт выше предела, а не уместившиеся адреса пропускаются, а не
// запираются: иначе тот, у кого много адресов, закрыл бы ноду от всех.
func TestAddressTableStaysBounded(t *testing.T) {
	inner := newQueue()
	for i := range 100 {
		inner.push(from(fmt.Sprintf("198.51.100.%d", i+1)))
	}
	l := guarded(inner, Config{MaxTracked: 10})
	defer l.Close()

	if got := len(acceptAll(t, l)); got != 100 {
		t.Fatalf("пропущено %d соединений со ста разных адресов", got)
	}
	if n := l.table.size(); n > 10 {
		t.Fatalf("в таблице %d адресов при пределе 10", n)
	}

	// Через минуту вёдра наполнились, и таблица чистится до пустой.
	at := later(l, sweepEvery)
	if !l.table.take(keyFor(t, "192.0.2.1"), at) {
		t.Fatal("новый адрес не пропущен после чистки")
	}
	if n := l.table.size(); n != 1 {
		t.Fatalf("после чистки в таблице %d адресов, ждали один новый", n)
	}
}

func keyFor(t *testing.T, ip string) netip.Addr {
	t.Helper()
	k, ok := keyOf(net.ParseIP(ip))
	if !ok {
		t.Fatalf("адрес %s не разобран", ip)
	}
	return k
}

// TestJournalCountsRejectionsWithoutAddresses — в журнал уходит число
// отклонённых, а адреса — нет: проект не пишет адреса покупателей никуда.
func TestJournalCountsRejectionsWithoutAddresses(t *testing.T) {
	var buf bytes.Buffer
	was := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(was) })

	inner := newQueue()
	for range 250 {
		inner.push(from("203.0.113.7"))
	}
	l := guarded(inner, Config{Burst: 200})
	defer l.Close()
	acceptAll(t, l)
	l.flush()

	journal := buf.String()
	if !strings.Contains(journal, "отклонено соединений: 50 ") {
		t.Errorf("в журнале нет числа отклонённых: %q", journal)
	}
	if strings.Contains(journal, "203.0.113") {
		t.Errorf("адрес попал в журнал: %q", journal)
	}
}

// TestWrappedConnectionKeepsCloseWrite — обёртка сохраняет половинное
// закрытие: REALITY без него падает, а relay обрывает недописанный ответ.
func TestWrappedConnectionKeepsCloseWrite(t *testing.T) {
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	l := Listen(raw, Config{}).(*listener)
	defer l.Close()

	client, err := net.Dial("tcp", raw.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	server, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	// Тот же вид, к которому REALITY приводит соединение без проверки.
	half, ok := server.(interface {
		net.Conn
		CloseWrite() error
	})
	if !ok {
		t.Fatal("принятое соединение не умеет CloseWrite")
	}
	if _, err := half.Write([]byte("ответ")); err != nil {
		t.Fatal(err)
	}
	if err := half.CloseWrite(); err != nil {
		t.Fatalf("CloseWrite: %v", err)
	}
	_ = client.SetReadDeadline(time.Now().Add(5 * time.Second))
	got, err := io.ReadAll(client)
	if err != nil || string(got) != "ответ" {
		t.Fatalf("клиент прочёл %q, %v: ждали ответ и конец потока", got, err)
	}
	// Вторая половина жива: клиент ещё может договорить.
	if _, err := client.Write([]byte("ещё")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 16)
	_ = server.SetReadDeadline(time.Now().Add(5 * time.Second))
	n, err := server.Read(buf)
	if err != nil || string(buf[:n]) != "ещё" {
		t.Fatalf("после CloseWrite сервер прочёл %q, %v", buf[:n], err)
	}
	if n := l.open.Load(); n != 1 {
		t.Fatalf("половинное закрытие изменило счётчик открытых: %d", n)
	}
}

// TestSellerTightensLimitsWithoutRestartingTheNode — продавец ужесточает
// предел из панели посреди атаки, и нода режет по новому сразу: перезапуск
// сбросил бы всех живых покупателей ради того, чтобы отбиться от флуда.
func TestSellerTightensLimitsWithoutRestartingTheNode(t *testing.T) {
	inner := newQueue()
	l := guarded(inner, Config{Rate: 20, Burst: 200})
	defer l.Close()
	l.Update(Limits{Rate: 5, Burst: 10})
	for range 100 {
		inner.push(from("203.0.113.9"))
	}
	if got := len(acceptAll(t, l)); got != 10 {
		t.Fatalf("после ужесточения пропущено %d, новый запас — 10", got)
	}
}

// TestBlockedNetworkGetsNoAnswerEvenAsRelay — закрытая продавцом сеть не
// получает ни одного соединения, даже если её адрес — вход каскада: закрыть
// его — решение продавца, и частотная поблажка входа его не отменяет.
func TestBlockedNetworkGetsNoAnswerEvenAsRelay(t *testing.T) {
	inner := newQueue()
	blocked := from("192.0.2.77")
	inner.push(blocked, from("198.51.100.20"))
	exempt := func(net.IP) bool { return true }
	l := guarded(inner, Config{Exempt: exempt})
	defer l.Close()
	l.Update(Limits{Blocked: []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}})

	got := acceptAll(t, l)
	if len(got) != 1 || remoteOf(got[0]) != "198.51.100.20" {
		t.Fatalf("пропущены %d соединений, ждали одно — не из закрытой сети", len(got))
	}
	if blocked.closed.Load() != 1 {
		t.Fatal("соединение из закрытой сети не закрыто")
	}
	if s := l.Take(); s.ByBlock != 1 {
		t.Fatalf("в счётчике закрытых %d, ждали 1", s.ByBlock)
	}
}

// TestPanelGetsItsOwnCountsWhateverTheJournalDid — панель забирает свои
// счётчики отдельно от журнала: минутная запись в журнал не съедает числа,
// которые нода ещё не успела отправить.
func TestPanelGetsItsOwnCountsWhateverTheJournalDid(t *testing.T) {
	inner := newQueue()
	for range 30 {
		inner.push(from("203.0.113.7"))
	}
	l := guarded(inner, Config{Rate: 1, Burst: 10})
	defer l.Close()
	acceptAll(t, l)
	l.flush()
	if s := l.Take(); s.ByRate != 20 {
		t.Fatalf("панель получила %d отклонённых за частоту, ждали 20", s.ByRate)
	}
	if s := l.Take(); s != (Stats{}) {
		t.Fatalf("повторный Take вернул %+v, а не нули", s)
	}
}
