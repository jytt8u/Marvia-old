// Package tunbridge превращает пакеты сетевого интерфейса в соединения
// туннеля.
//
// Это то, без чего на телефоне не бывает системного VPN. Android даёт
// приложению файловый дескриптор интерфейса TUN, в который операционная
// система пишет сырые IP-пакеты всех приложений. Пакеты — не соединения:
// чтобы отправить их в наш туннель, кто-то должен собрать из них TCP-потоки,
// то есть выполнить работу сетевого стека в пространстве пользователя.
//
// Свой стек TCP мы не пишем по той же причине, по которой не писали свой
// TLS: цена ошибки высокая, а выигрыша нет. Берём проверенный gvisor через
// обёртку tun2socks и подключаем к нему свой обработчик.
package tunbridge

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xjasonlyu/tun2socks/v2/core"
	"github.com/xjasonlyu/tun2socks/v2/core/adapter"
	"github.com/xjasonlyu/tun2socks/v2/core/device/fdbased"
	"github.com/xjasonlyu/tun2socks/v2/core/device/iobased"
	"gvisor.dev/gvisor/pkg/tcpip/stack"

	"github.com/jytt8u/marvia/internal/relay"
	"github.com/jytt8u/marvia/internal/vp1"
)

const (
	// DefaultMTU — размер, который выставляет большинство клиентов.
	// Меньше обычного из-за накладных расходов туннеля.
	DefaultMTU = 1500

	// dialTimeout — сколько ждём открытия потока до цели.
	dialTimeout = 15 * time.Second

	// dnsPort — порт, на котором мы перехватываем запросы имён.
	dnsPort = 53

	// httpsPort — порт, на котором ждём TLS-приветствия ради правил по
	// сайтам. Только он: на остальных TLS редок, а ждать первого пакета на
	// чужом протоколе — значит задерживать его зря.
	httpsPort = 443

	// udpIdleTimeout — сколько держим поток датаграмм без единого пакета.
	//
	// У UDP нет конца разговора, закрывать поток некому. Полторы минуты: QUIC
	// подтверждает жизнь куда чаще, так что живое соединение сюда не попадёт,
	// а брошенное не будет висеть до отключения человека от туннеля.
	udpIdleTimeout = 90 * time.Second
)

// Dialer открывает поток до цели через туннель.
type Dialer interface {
	DialTarget(ctx context.Context, target vp1.Address) (net.Conn, error)

	// DialDatagrams открывает поток датаграмм до цели. Границы в нём
	// сохраняются: один Read — одна датаграмма, один Write — одна датаграмма.
	DialDatagrams(ctx context.Context, target vp1.Address) (net.Conn, error)
}

// Config описывает мост.
type Config struct {
	// FD — дескриптор интерфейса от VpnService. Взаимоисключающ с Device.
	//
	// Дескриптор переходит мосту во владение: мост закроет его и при неудачном
	// запуске, и при остановке. Приложение поэтому отдаёт его через detachFd
	// и больше к нему не прикасается. Иначе выйдет двойное закрытие, а это на
	// Linux означает, что вторым разом мы закроем чужой сокет, успевший занять
	// освободившийся номер.
	FD int

	// Device — интерфейс в виде потока байтов. Нужен для проверок на
	// настольной машине, где никакого TUN нет.
	// Если поток умеет Close, владение им тоже переходит мосту: закрытие
	// канального endpoint само по себе не прерывает заблокированный Read.
	Device io.ReadWriter

	// Endpoint — уже открытый сетевой интерфейс.
	//
	// Так приходит TUN на Windows: там его создаёт драйвер wintun, никакого
	// файлового дескриптора не бывает, и открывать его должен тот, кто знает
	// про эту платформу. Мост про Windows знать не обязан — ему достаточно
	// готового интерфейса.
	//
	// Правило владения то же, что и у дескриптора: интерфейс переходит мосту,
	// и закроет его мост. Закрывать его ещё и снаружи не надо — но снять
	// маршруты, которые под него настраивали, придётся тому, кто их ставил.
	Endpoint stack.LinkEndpoint

	// MTU интерфейса.
	MTU uint32

	// Dialer — как добраться до цели.
	Dialer Dialer

	// DNS — куда отправлять запросы имён, если нода не умеет датаграммы.
	//
	// Обычно имена идут по UDP, как им и положено. Но нода у продавца может
	// быть старой версии, а без работающих имён телефон бесполезен — поэтому
	// остался и запасной путь: тот же запрос по TCP внутрь туннеля.
	//
	// Мимо туннеля запросы имён не уходят ни в одном из случаев: запрос имени
	// в открытую выдаёт цензору весь список посещённых сайтов.
	DNS string

	// Names — кто отвечает на перехваченные запросы имён; nil — как раньше:
	// открытый DNS через туннель к DNS.
	//
	// Открытый запрос через туннель прячет имена от провайдера, но не от
	// ноды: она расшифровывает VP1 и отправляет запрос дальше таким, каким
	// его написало приложение. С Names запрос к резолверу уходит по HTTPS
	// (internal/securedns), и нода видит только соединение с резолвером. Мост
	// при этом отвечает на любой UDP и TCP на порт 53 сам: к ноде не уходит
	// ни одного открытого запроса, какой бы адрес приложение себе ни
	// прописало.
	Names Names

	// NoIPv6 — не пускать IPv6 вовсе, какой бы ни была нода; см. ipv6.go.
	// Маршрут IPv6 при этом остаётся в туннеле: иначе IPv6 ушёл бы мимо.
	NoIPv6 bool

	// OnError вызывается на ошибках отдельных соединений. Может быть nil.
	OnError func(error)
}

// Names отвечает на запросы имён вместо открытого DNS.
type Names interface {
	// Exchange отвечает на сообщение DNS. udp — ответ поедет датаграммой и
	// обязан влезть в размер, который объявил запрос.
	Exchange(ctx context.Context, query []byte, udp bool) ([]byte, error)

	// Reset забывает всё, что относилось к прежней ноде.
	Reset()
}

// Bridge — работающий мост.
type Bridge struct {
	device         stack.LinkEndpoint
	guard          *decoyGuard
	stream         io.Closer
	stack          *stack.Stack
	handler        *handler
	disconnectOnce sync.Once
	closeOnce      sync.Once
}

// Start поднимает мост и начинает разбирать пакеты.
func Start(cfg Config) (*Bridge, error) {
	if cfg.Dialer == nil {
		return nil, errors.New("не задан способ дозвона до цели")
	}
	if cfg.DNS == "" {
		return nil, errors.New("не задан адрес для запросов имён")
	}
	mtu := cfg.MTU
	if mtu == 0 {
		mtu = DefaultMTU
	}

	dev, err := openDevice(cfg, mtu)
	if err != nil {
		// Вернуть дескриптор уже некому: на той стороне от него отказались
		// насовсем. Не закроем — интерфейс останется висеть до конца работы
		// приложения, и следующая попытка подключения упрётся в него же.
		CloseFD(cfg.FD)
		return nil, err
	}
	// Обманки zapret и GoodbyeDPI умирают здесь, до стека; см. decoy.go.
	guard := guardDecoys(dev)
	var stream io.Closer
	if cfg.Endpoint == nil && cfg.Device != nil {
		stream, _ = cfg.Device.(io.Closer)
	}

	handler := &handler{
		dialer:  cfg.Dialer,
		dns:     cfg.DNS,
		names:   cfg.Names,
		onError: cfg.OnError,
	}
	if s, ok := cfg.Dialer.(interface{ siteRules() *Sites }); ok {
		handler.sites = s.siteRules()
	}
	handler.v6.never = cfg.NoIPv6
	handler.v6.reset()

	st, err := core.CreateStack(&core.Config{
		LinkEndpoint:     guard,
		TransportHandler: handler,
	})
	if err != nil {
		if stream != nil {
			_ = stream.Close()
		}
		guard.Close()
		return nil, fmt.Errorf("сетевой стек: %w", err)
	}
	handler.acceptTCP(st)
	guard.ready.Store(true)

	return &Bridge{device: guard, guard: guard, stream: stream, stack: st, handler: handler}, nil
}

// NodeChanged сообщает мосту, что трафик пошёл через другую ноду.
//
// Нужно ровно ради одного: снять запрет датаграмм. Вывод «нода не умеет
// датаграммы» относится к конкретной ноде, а не к туннелю, и переносить его на
// новую ноду — значит наказывать её за чужую старость.
//
// Хуже того, сам вывод мог быть неверным: умирающая нода обрывает поток там
// же, где старая отвечает отказом. На живой проверке это и случилось — после
// смерти ноды датаграммы выключались до конца сессии, и человеку оставалось
// только переподключиться руками, что он и сделал.
//
// Резолверу — тоже: его соединения жили в потоках через прежнюю ноду, а
// ответы из кэша подбирались под её страну.
func (b *Bridge) NodeChanged() {
	if b != nil && b.handler != nil {
		b.handler.noUDP.Store(false)
		b.handler.v6.reset()
		if b.handler.names != nil {
			b.handler.names.Reset()
		}
	}
}

// openDevice отдаёт интерфейс канального уровня.
//
// Источников три, и все нужны. Дескриптор — то, что даёт VpnService на
// телефоне. Готовый интерфейс — то, что получается на Windows, где TUN создаёт
// драйвер и файлового дескриптора не бывает вовсе. Поток байтов — способ
// проверить мост там, где никакого TUN нет.
func openDevice(cfg Config, mtu uint32) (stack.LinkEndpoint, error) {
	switch {
	case cfg.Endpoint != nil:
		return cfg.Endpoint, nil
	case cfg.Device != nil:
		dev, err := iobased.New(cfg.Device, mtu, 0)
		if err != nil {
			return nil, fmt.Errorf("интерфейс из потока: %w", err)
		}
		return dev, nil
	case cfg.FD > 0:
		dev, err := fdbased.Open(strconv.Itoa(cfg.FD), mtu, 0)
		if err != nil {
			return nil, fmt.Errorf("интерфейс по дескриптору %d: %w", cfg.FD, err)
		}
		return dev, nil
	default:
		return nil, errors.New("не задан ни готовый интерфейс, ни дескриптор, ни поток")
	}
}

// Disconnect освобождает интерфейс раньше очистки стека: на Android именно
// закрытие TUN возвращает обычные маршруты. Ожидание потоков до мёртвой ноды
// не должно оставлять весь телефон без интернета. Once защищает номер
// дескриптора от повторного закрытия после его выдачи новому соединению.
func (b *Bridge) Disconnect() {
	if b == nil {
		return
	}
	b.disconnectOnce.Do(func() {
		if b.stream != nil {
			_ = b.stream.Close()
		}
		if b.device != nil {
			b.device.Close()
		}
	})
}

// Close останавливает мост после освобождения интерфейса.
func (b *Bridge) Close() error {
	if b == nil {
		return nil
	}
	b.Disconnect()
	b.closeOnce.Do(func() {
		if b.stack != nil {
			b.stack.Close()
		}
	})
	return nil
}

// handler получает от стека готовые соединения.
type handler struct {
	dialer  Dialer
	dns     string
	names   Names
	onError func(error)

	// sites — правила по сайтам у развилки (Split); nil — правил нет. Мост
	// кладёт им в память ответы на запросы имён и достаёт имя из TLS.
	sites *Sites

	// noUDP — нода отказалась от датаграмм. Ставится один раз за сессию,
	// чтобы не платить лишний круг на каждом новом потоке.
	noUDP atomic.Bool

	// v6 — есть ли у ноды IPv6; см. ipv6.go.
	v6 v6State
}

func (h *handler) fail(err error) {
	if h.onError != nil && err != nil {
		h.onError(err)
	}
}

// failDial сообщает о несостоявшемся дозвоне — кроме отказов по цели.
//
// Отказ по цели — «сайт не отвечает», «у ноды нет пути в его сеть» — это не
// беда туннеля, и показывать его человеку значит пугать зря: на живом
// телефоне журнал состоял из таких строк целиком, и человек решил, что
// ничего не работает, хотя работало всё, кроме одного сайта. Беда туннеля —
// нода не отвечает, — по-прежнему сообщается.
func (h *handler) failDial(err error) {
	var refused *vp1.RefusedError
	if errors.As(err, &refused) {
		return
	}
	h.fail(err)
}

// HandleTCP обслуживает соединение приложения.
func (h *handler) HandleTCP(conn adapter.TCPConn) {
	defer conn.Close()

	// Имена по TCP — тот же вопрос, что по UDP: так их спрашивают, когда ответ
	// не влез в датаграмму. Пропусти мы их к ноде как обычный поток, шифрование
	// имён обходилось бы любым приложением, которому захотелось TCP.
	if conn.ID().LocalPort == dnsPort && h.names != nil {
		h.serveNamesTCP(conn)
		return
	}

	target := targetOf(conn.ID().LocalAddress.String(), conn.ID().LocalPort)
	v6 := target.Type == vp1.AtypIPv6
	if v6 && h.v6.off.Load() {
		// Нода IPv6 не умеет: закрываем сразу, приложение уйдёт на IPv4, не
		// дожидаясь круга до ноды.
		return
	}

	// Имя сайта — для правил по сайтам; см. sites.go, там же цена ожидания.
	site := ""
	if conn.ID().LocalPort == httpsPort && h.sites.active() {
		head, name := readHello(conn, helloWait)
		if len(head) > 0 {
			conn = &peekedConn{TCPConn: conn, head: head}
		}
		site = name
		if ip, err := netip.ParseAddr(target.Host); err == nil && site != "" {
			// QUIC к этому же сайту пойдёт по UDP, где имени не увидеть, —
			// пусть решится так же, как это соединение.
			h.sites.learn(site, ip, seenFloor)
		}
	}

	ctx, cancel := context.WithTimeout(withSource(withSite(context.Background(), site), conn.ID().RemotePort, false), dialTimeout)
	stream, err := h.dialer.DialTarget(ctx, target)
	cancel()
	if v6 {
		h.v6.observe(err)
	}
	if err != nil {
		h.failDial(fmt.Errorf("поток до %s: %w", target, err))
		return
	}
	defer stream.Close()
	// Поток мог открыться раньше ответа ноды (ранний старт). Отказ по цели
	// и недоступность ноды тогда приходят первым чтением — учитываем их
	// там же, где раньше учитывали ошибку дозвона.
	if early, ok := stream.(interface{ OnStatus(func(error)) }); ok {
		early.OnStatus(func(err error) {
			if v6 {
				h.v6.observe(err)
			}
			if err != nil {
				h.failDial(fmt.Errorf("поток до %s: %w", target, err))
			}
		})
	}

	// Обрыв уже начатого соединения человеку не показываем, и вот почему.
	//
	// Отличить «приложение само закрыло соединение на полуслове» от «нода
	// умерла посреди передачи» на этом уровне невозможно: адаптер gvisor
	// делает errors.New(err.String()) и выбрасывает тип, оставляя голый
	// текст. А сообщение, которое не различает эти два случая, хуже молчания:
	// оно приписывает «endpoint is closed for send» к совершенно здоровому
	// туннелю каждый раз, когда человек листает ленту и обрывает загрузку.
	//
	// Сигнал при этом не теряется. Если нода действительно умерла, следующее
	// же соединение не откроется — а вот это выше сообщается.
	_ = relay.Bidirectional(conn, stream)
}

// HandleUDP обслуживает датаграммы.
//
// Раньше здесь проходил только порт имён, а всё остальное отбрасывалось — с
// расчётом, что приложение поймёт и перейдёт на TCP. Расчёт оказался неверным:
// видео в TikTok, YouTube и Instagram ходит по QUIC, то есть по UDP на 443, и
// приложение не «понимает», а ждёт таймаута, пробует ещё раз, и только потом
// откатывается. На живом телефоне это выглядело как «крутится и подгружается»
// на каждом новом ролике.
// HandleUDP — точка, где нельзя задерживаться.
//
// gvisor вызывает этот обработчик прямо из разбора пакета, синхронно, и это
// не то же самое, что с соединениями: для TCP он заводит горутину сам, а для
// датаграмм — нет. Всё, что мы здесь просидим, стек не разбирает пакеты. Ни
// свои, ни чужие: очередь одна.
//
// Так уже было и стоило дорого. Запрос имени обслуживался прямо здесь, и
// каждый такой запрос останавливал разбор пакетов на всё время похода в
// туннель. Приложение, открывающее десяток адресов подряд — а видео открывает
// именно так, — само себе устраивало заикание. Теперь работа уходит в
// горутину, как это делает и сам tun2socks у себя.
func (h *handler) HandleUDP(conn adapter.UDPConn) {
	go h.serveUDP(conn)
}

// serveUDP уносит датаграммы приложения в туннель.
//
// Раньше сюда проходил только порт имён, а всё остальное отбрасывалось — с
// расчётом, что приложение поймёт и перейдёт на TCP. Расчёт неверный: видео в
// TikTok, YouTube и Instagram ходит по QUIC, то есть по UDP на 443, и
// приложение не «понимает», а ждёт таймаута и пробует снова.
func (h *handler) serveUDP(conn adapter.UDPConn) {
	defer conn.Close()

	target := targetOf(conn.ID().LocalAddress.String(), conn.ID().LocalPort)
	isDNS := conn.ID().LocalPort == dnsPort

	// Первую датаграмму читаем здесь, а не внутри: если придётся откатываться
	// на запасной путь, перечитать её будет уже неоткуда — она одна.
	firstBuf := vp1.DatagramBuffer()
	defer vp1.PutDatagramBuffer(firstBuf)
	first := (*firstBuf)[:vp1.MaxDatagram]
	_ = conn.SetReadDeadline(time.Now().Add(udpIdleTimeout))
	n, from, err := conn.ReadFrom(first)
	if err != nil {
		h.fail(fmt.Errorf("чтение датаграммы до %s: %w", target, err))
		return
	}
	// Раньше проверки IPv6: на вопрос, заданный IPv6-резолверу, мост ответит
	// сам, и до ноды он не дойдёт — а значит, и её IPv6 тут ни при чём.
	if isDNS && h.names != nil {
		h.serveNames(conn, first[:n], from)
		return
	}
	if target.Type == vp1.AtypIPv6 && h.v6.off.Load() {
		return
	}
	if isDNS {
		if answer, ok := h.v6.localAnswer(first[:n]); ok {
			_, _ = conn.WriteTo(answer, from)
			return
		}
	}

	// Ноды обновляются не разом с клиентами: продавец ставит их сам, и часть
	// стоит со старой версией, которая про датаграммы не знает. Такая нода
	// отвечает отказом на первый же запрос — тогда мы запоминаем это на всю
	// сессию, чтобы не платить лишний круг на каждом потоке.
	if !h.noUDP.Load() {
		err := h.pipeUDP(conn, target, first[:n], from)
		if err == nil {
			return
		}
		if target.Type == vp1.AtypIPv6 {
			h.v6.observe(err)
		}
		if errors.Is(err, vp1.ErrDatagramsUnsupported) {
			h.noUDP.Store(true)
		} else {
			h.failDial(fmt.Errorf("датаграммы до %s: %w", target, err))
		}
	}

	// Запасной путь только для имён, и он обязан быть: без работающих имён
	// телефон бесполезен, что бы ни случилось с датаграммами.
	if !isDNS {
		return
	}
	if err := h.serveDNS(conn, first[:n], from); err != nil {
		h.failDial(fmt.Errorf("запрос имени: %w", err))
	}
}

// pipeUDP гоняет датаграммы между приложением и целью.
//
// Поток заведён на одну цель, поэтому адрес в датаграммах не нужен: куда
// слать наружу, знает нода, а кому отдавать ответы — сказано в from.
func (h *handler) pipeUDP(conn adapter.UDPConn, target vp1.Address, first []byte, from net.Addr) error {
	ctx, cancel := context.WithTimeout(withSource(context.Background(), conn.ID().RemotePort, true), dialTimeout)
	stream, err := h.dialer.DialDatagrams(ctx, target)
	cancel()
	if err != nil {
		return err
	}
	defer stream.Close()

	if _, err := stream.Write(first); err != nil {
		return fmt.Errorf("отправка датаграммы: %w", err)
	}

	done := make(chan struct{})

	// Ответы — обратно приложению.
	go func() {
		defer close(done)
		b := vp1.DatagramBuffer()
		defer vp1.PutDatagramBuffer(b)
		buf := (*b)[:vp1.MaxDatagram]
		for {
			_ = stream.SetReadDeadline(time.Now().Add(udpIdleTimeout))
			n, err := stream.Read(buf)

			// Датаграмма нулевой длины — это датаграмма, а не «ничего не
			// пришло»: в UDP такие законны, ими проверяют, что путь жив.
			if err == nil || n > 0 {
				if target.Port == dnsPort {
					h.sites.learnAnswer(buf[:n])
				}
				if _, err := conn.WriteTo(buf[:n], from); err != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	out := vp1.DatagramBuffer()
	defer vp1.PutDatagramBuffer(out)
	buf := (*out)[:vp1.MaxDatagram]
	isDNS := target.Port == dnsPort
	for {
		_ = conn.SetReadDeadline(time.Now().Add(udpIdleTimeout))
		n, _, err := conn.ReadFrom(buf)
		if err == nil || n > 0 {
			// Резолвер шлёт A и AAAA из одного гнезда: вторая датаграмма
			// приходит уже сюда, и фильтр должен видеть и её.
			if isDNS {
				if answer, ok := h.v6.localAnswer(buf[:n]); ok {
					_, _ = conn.WriteTo(answer, from)
					continue
				}
			}
			if _, err := stream.Write(buf[:n]); err != nil {
				break
			}
		}
		if err != nil {
			break
		}
	}

	// Закрываем поток, чтобы отпустить чтение ответов, и дожидаемся его.
	_ = stream.Close()
	<-done
	return nil
}

// serveDNS переводит запрос имени с UDP на TCP и уносит его в туннель.
//
// Запрос и обратный адрес приходят снаружи: первая датаграмма уже прочитана,
// и второй раз её не будет.
func (h *handler) serveDNS(conn adapter.UDPConn, query []byte, addr net.Addr) error {
	_ = conn.SetDeadline(time.Now().Add(dialTimeout))

	n := len(query)

	target, err := vp1.AddressFromHostPort(hostOf(h.dns), portOf(h.dns))
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), dialTimeout)
	stream, err := h.dialer.DialTarget(ctx, target)
	cancel()
	if err != nil {
		return fmt.Errorf("поток до %s: %w", h.dns, err)
	}
	defer stream.Close()

	answer, err := ExchangeOverTCP(stream, query[:n])
	if err != nil {
		return err
	}
	h.sites.learnAnswer(answer)
	if _, err := conn.WriteTo(answer, addr); err != nil {
		return fmt.Errorf("отправка ответа: %w", err)
	}
	return nil
}

// targetOf собирает адрес назначения из данных стека.
//
// LocalAddress и LocalPort здесь — это адрес, к которому обращалось
// приложение: с точки зрения стека, перехватившего пакет, он локальный.
func targetOf(host string, port uint16) vp1.Address {
	addr, err := vp1.AddressFromHostPort(host, port)
	if err != nil {
		// Стек не отдаёт кривых адресов, но на всякий случай не роняем мост.
		return vp1.Address{Type: vp1.AtypDomain, Host: host, Port: port}
	}
	return addr
}

func hostOf(hostPort string) string {
	host, _, err := net.SplitHostPort(hostPort)
	if err != nil {
		return hostPort
	}
	return host
}

func portOf(hostPort string) uint16 {
	_, port, err := net.SplitHostPort(hostPort)
	if err != nil {
		return dnsPort
	}
	value, err := strconv.Atoi(port)
	if err != nil {
		return dnsPort
	}
	return uint16(value)
}
