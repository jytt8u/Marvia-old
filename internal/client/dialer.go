package client

import (
	"bytes"
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	mrand "math/rand/v2"
	"net"
	"sync/atomic"
	"time"

	"github.com/jytt8u/marvia/internal/transport"
	"github.com/jytt8u/marvia/internal/tunnel"
	"github.com/jytt8u/marvia/internal/vp1"
	utls "github.com/refraction-networking/utls"
)

// Dialer держит соединение до одной ноды и раздаёт потоки до целей.
type Dialer struct {
	node Node
	pool *tunnel.Pool

	// sub — подписка, с которой выбрана эта нода: срок и остаток трафика.
	//
	// Нужна не дозвону, а тому, кто рисует окно: приложение обязано уметь
	// ответить «до какого числа и сколько осталось», не спрашивая продавца.
	// Второй раз ходить за подпиской ради этого незачем — она уже в руках.
	sub Subscription

	// connectNS — сколько заняло последнее TCP-соединение до ноды, в
	// наносекундах. Это один круг по сети и есть — то, что человек называет
	// пингом. Полное время прогрева втрое больше: там ещё TLS и VP1.
	connectNS atomic.Int64
	// Обновление из экрана серверов не должно гоняться с чтением статуса.
	measurement atomic.Pointer[Measurement]
}

// Connect — круг по сети до ноды при последнем соединении. Ноль означает,
// что транспорт такого не сообщает (за CDN и по QUIC).
func (d *Dialer) Connect() time.Duration {
	return time.Duration(d.connectNS.Load())
}

// После потери сети старый отклик уже не описывает текущий туннель. Меняем
// только RTT: параллельный замер серверов не должен потерять свои результаты.
func (d *Dialer) recordRTT(rtt time.Duration) {
	for {
		previous := d.measurement.Load()
		next := Measurement{Node: d.Node()}
		if previous != nil {
			next = *previous
		}
		next.RTT = rtt
		next.dialer = nil
		if d.measurement.CompareAndSwap(previous, &next) {
			return
		}
	}
}

// quicAttempt — сколько ждём дозвона по UDP, прежде чем уйти на TCP.
//
// Четыре секунды: этого хватает на честное рукопожатие даже на плохой
// мобильной сети, но человек не успевает решить, что приложение зависло.
const quicAttempt = 4 * time.Second

// clockGrain — во что упирается точность замера.
//
// Windows меряет время грубо. Всё, что быстрее одной миллисекунды, для нас
// неотличимо, и притворяться, что мы видим микросекунды, значит выдавать шум
// за измерение.
const clockGrain = time.Millisecond

// Options — необязательные настройки дозвона.
//
// В бою RootCAs и InsecureSkipVerify пустые: доверяем системному хранилищу,
// как браузер. Нужны они для отладки — когда нода поднята с самоподписанным
// сертификатом.
type Options struct {
	// RootCAs — своё хранилище доверия вместо системного.
	RootCAs *x509.CertPool

	// InsecureSkipVerify отключает проверку сертификата. Только для отладки:
	// с ним любой, кто вклинится в соединение, становится нашей нодой.
	InsecureSkipVerify bool

	// Fragment режет TLS-приветствие так, чтобы имя из SNI не лежало целиком
	// ни в одном TCP-сегменте (transport.FragmentHello). Это решение человека,
	// а не ядра: есть ли у его провайдера фильтр по имени, клиент узнать не
	// может, а без фильтра дробление — лишняя примета.
	Fragment bool

	// NoQUIC — не ходить по UDP даже к ноде, которая его принимает. Откат на
	// TCP ловит только сеть, где UDP не проходит вовсе; где он проходит, но
	// его душат после первых мегабайт, QUIC дозванивается и потом еле
	// тянет. Узнать это сам клиент не может — решает человек. Нулевое
	// значение — как было: телефон этого поля не задаёт.
	NoQUIC bool

	// Fingerprint — чьё TLS-приветствие изображать вместо того, что выбрал
	// продавец для ноды. Пусто — как у продавца. Выбирает человек, руками:
	// сам клиент отпечаток не перебирает — смена сразу после заморозки адреса
	// её удлиняет (panel.Fingerprints). Но бывает сеть, где фильтр режет
	// именно приветствие Chrome, и продавец этого не видит, а человек —
	// видит, что с другим отпечатком открывается.
	Fingerprint string
}

// fingerprintFor — какой отпечаток изображать: выбранный человеком, иначе
// продавцом. Незнакомое имя оставляет Chrome: панель могла выучить новое
// раньше, чем клиент обновился, и подключиться важнее, чем изобразить.
func fingerprintFor(node Node, opts Options) utls.ClientHelloID {
	name := node.Fingerprint
	if opts.Fingerprint != "" {
		name = opts.Fingerprint
	}
	id, _ := transport.Fingerprint(name)
	return id
}

// NewDialer готовит дозвон до ноды. Соединение поднимается лениво, при первом
// обращении: на телефоне включение VPN не должно упираться в сеть.
func NewDialer(node Node, key vp1.KeyPair, opts Options) (*Dialer, error) {
	if node.Address == "" {
		return nil, errors.New("у ноды нет адреса")
	}
	serverPub, err := vp1.DecodeKey(node.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("публичный ключ ноды %s: %w", node.Name, err)
	}

	name := node.SNI
	if name == "" {
		host, _, splitErr := net.SplitHostPort(node.Address)
		if splitErr != nil {
			return nil, fmt.Errorf("адрес ноды %q: %w", node.Address, splitErr)
		}
		name = host
	}

	// Dialer заводим раньше дозвона: замер круга пишется прямо в него.
	d := &Dialer{node: node}
	dial, err := transportDialer(node, name, opts, func(rtt time.Duration) {
		d.connectNS.Store(int64(rtt))
	})
	if err != nil {
		return nil, err
	}

	pool := tunnel.NewPool(func(ctx context.Context) (net.Conn, error) {
		raw, err := dial(ctx)
		if err != nil {
			return nil, err
		}
		// VP1 принимает net.Conn, а не context: отмена внешней сети должна
		// закрыть и этот этап, не дожидаясь десятисекундного тайм-аута.
		stop := context.AfterFunc(ctx, func() { _ = raw.Close() })
		conn, err := vp1.ClientHandshake(raw, key, serverPub)
		stopped := stop()
		if err != nil {
			_ = raw.Close()
			return nil, err
		}
		if !stopped || ctx.Err() != nil {
			_ = conn.Close()
			return nil, ctx.Err()
		}
		return conn, nil
	}, 0, 0)

	d.pool = pool
	return d, nil
}

// transportDialer выбирает внешний слой под то, как настроена нода.
func transportDialer(node Node, serverName string, opts Options, onConnect func(time.Duration)) (func(context.Context) (net.Conn, error), error) {
	// QUIC пробуем первым и откатываемся на TCP, если не вышло.
	//
	// Только для обычного TLS: под REALITY нода не может держать QUIC (там
	// нет своего сертификата), а за CDN адрес принадлежит не ноде, и UDP до
	// неё не дойдёт.
	if node.QUIC && !opts.NoQUIC && node.Transport() == TransportTLS {
		tcp, err := tcpDialer(node, serverName, opts, onConnect)
		if err != nil {
			return nil, err
		}
		return quicFirst(node, serverName, opts, tcp), nil
	}

	return tcpDialer(node, serverName, opts, onConnect)
}

// quicFirst выбирает между UDP и TCP тем, кто быстрее, а без UDP навсегда
// остаётся на TCP.
//
// Первое соединение идёт наперегонки: QUIC и TCP набираются разом, кто
// первым собрался, тот и путь, а проигравший закрывается. Раньше QUIC шёл
// первым всегда, и там, где UDP медленнее TCP (перегруженный оператор, шейпер
// на UDP), туннель годами ездил по худшей дороге только потому, что она
// формально работала. Цена гонки — один лишний дозвон на старте.
//
// «Навсегда» важнее, чем кажется. UDP режут не по одному пакету, а целой
// сетью: у оператора, в офисе, в белом списке. Там первая попытка не просто
// не удастся — она будет молча висеть до тайм-аута, и так на каждом новом
// соединении. Один раз выяснили, что дороги нет, и больше туда не ходим.
func quicFirst(node Node, serverName string, opts Options, tcp func(context.Context) (net.Conn, error)) func(context.Context) (net.Conn, error) {
	const (
		undecided int32 = iota
		viaQUIC
		viaTCP
	)
	var chosen atomic.Int32

	quic := func(ctx context.Context) (net.Conn, error) {
		quicCtx, cancel := context.WithTimeout(ctx, quicAttempt)
		defer cancel()
		return transport.DialQUIC(quicCtx, node.Address, transport.QUICDialConfig{
			TLS: transport.ClientConfig{
				ServerName:         serverName,
				RootCAs:            opts.RootCAs,
				InsecureSkipVerify: opts.InsecureSkipVerify,
			},
		})
	}

	return func(ctx context.Context) (net.Conn, error) {
		switch chosen.Load() {
		case viaTCP:
			return tcp(ctx)
		case viaQUIC:
			conn, err := quic(ctx)
			if err == nil {
				return conn, nil
			}
			// Отмена самим человеком — не приговор дороге: он просто нажал
			// «отключиться», и в следующий раз UDP надо пробовать снова.
			if ctx.Err() == nil {
				chosen.Store(viaTCP)
			}
			return tcp(ctx)
		}
		return race(ctx, quic, tcp, func(viaUDP bool) {
			if viaUDP {
				chosen.Store(viaQUIC)
			} else {
				chosen.Store(viaTCP)
			}
		})
	}
}

// race набирает QUIC и TCP разом и отдаёт первое собравшееся соединение;
// второе, если соберётся, закрывается. won сообщает, кто выиграл. Не
// собралось ни одно — ошибка TCP: она понятнее человеку, чем ошибка UDP.
func race(ctx context.Context, quic, tcp func(context.Context) (net.Conn, error), won func(viaUDP bool)) (net.Conn, error) {
	type result struct {
		conn net.Conn
		err  error
		udp  bool
	}
	raceCtx, cancel := context.WithCancel(ctx)
	results := make(chan result, 2)
	go func() { c, err := quic(raceCtx); results <- result{c, err, true} }()
	go func() { c, err := tcp(raceCtx); results <- result{c, err, false} }()

	var tcpErr error
	for i := 0; i < 2; i++ {
		r := <-results
		if r.err == nil {
			won(r.udp)
			cancel()
			// Проигравший, если всё же соберётся, — лишний: закрываем.
			if i == 0 {
				go func() {
					if late := <-results; late.conn != nil {
						_ = late.conn.Close()
					}
				}()
			}
			return r.conn, nil
		}
		if !r.udp {
			tcpErr = r.err
		}
	}
	cancel()
	return nil, tcpErr
}

// tcpDialer собирает дозвон по TCP — тот, что был до появления QUIC.
func tcpDialer(node Node, serverName string, opts Options, onConnect func(time.Duration)) (func(context.Context) (net.Conn, error), error) {
	fingerprint := fingerprintFor(node, opts)
	tlsCfg := transport.ClientConfig{
		ServerName:         serverName,
		RootCAs:            opts.RootCAs,
		InsecureSkipVerify: opts.InsecureSkipVerify,
		OnConnect:          onConnect,
		Fragment:           opts.Fragment,
		Fingerprint:        fingerprint,
	}

	switch node.Transport() {
	case TransportWS:
		cfg := transport.WSDialConfig{Host: serverName, Path: node.WSPath, TLS: tlsCfg}
		return func(ctx context.Context) (net.Conn, error) {
			return transport.DialWS(ctx, node.Address, cfg)
		}, nil

	case TransportReality:
		pub, err := vp1.DecodeKey(node.RealityPublicKey)
		if err != nil {
			return nil, fmt.Errorf("публичный ключ REALITY ноды %s: %w", node.Name, err)
		}
		// Имя выбирается на каждое соединение, а не один раз на ноду.
		//
		// Только здесь это и возможно: под REALITY подлинность ноды
		// подтверждает её ключ, а не сертификат, поэтому в SNI годится любое
		// из имён, которые нода принимает. При обычном TLS и за CDN имя
		// обязано совпадать с сертификатом, и выбирать там не из чего.
		//
		// Зачем. Одно имя на ноду — это одна строка в списке блокировок:
		// зарезали домен, и нода умерла целиком, хотя её адрес жив. С набором
		// имён блокировка одного отнимает у неё долю соединений, а не все.
		// Сессии живут 20…60 минут, так что имя меняется само по себе
		// несколько раз в сутки.
		names := node.serverNames(serverName)
		return func(ctx context.Context) (net.Conn, error) {
			return transport.DialReality(ctx, node.Address, transport.RealityDialConfig{
				ServerName:  names[mrand.IntN(len(names))],
				PublicKey:   pub,
				ShortID:     node.RealityShortID,
				OnConnect:   onConnect,
				Fragment:    opts.Fragment,
				Fingerprint: fingerprint,
			})
		}, nil

	default:
		return func(ctx context.Context) (net.Conn, error) {
			return transport.Dial(ctx, node.Address, tlsCfg)
		}, nil
	}
}

// DialTarget открывает поток до цели через туннель.
func (d *Dialer) DialTarget(ctx context.Context, target vp1.Address) (net.Conn, error) {
	// Ранний старт: ответа ноды не ждём, см. early.go.
	stream, err := d.pool.Open(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", vp1.ErrNodeUnreachable, err)
	}
	release := armStream(ctx, stream)
	defer release()
	if err := vp1.WriteRequestOf(stream, target, vp1.KindTCP); err != nil {
		_ = stream.Close()
		return nil, fmt.Errorf("%w: запрос на %s: %w", vp1.ErrNodeUnreachable, target, streamError(ctx, err))
	}
	return newEarlyConn(stream, target), nil
}

// DialDatagrams открывает поток датаграмм до цели.
//
// Отдельный метод, а не флаг в DialTarget: вернувшееся соединение живёт по
// другим правилам — границы датаграмм в нём сохраняются, и обращаться с ним
// как с потоком байтов нельзя.
func (d *Dialer) DialDatagrams(ctx context.Context, target vp1.Address) (net.Conn, error) {
	stream, err := d.open(ctx, target, vp1.KindUDP, nil)
	if err != nil {
		return nil, err
	}
	return vp1.Datagrams(stream), nil
}

// MeasureFetch узнаёт, за сколько нода отдаёт порцию данных.
//
// Возвращает время целиком: от просьбы до последнего байта, вместе с кругом до
// ноды и разгоном TCP. Именно это человек и ждёт, открывая страницу.
//
// Отсчёт нарочно не с первого байта, хотя так казалось точнее. Первая попытка
// так и делала — и на живой ноде показала 884 Мбит/с при канале в 155. Пока мы
// доходили до чтения, нода уже успевала прислать всю порцию, и та лежала в
// буфере ядра: часы мерили не сеть, а скорость памяти. Замер с начала запроса
// такого обмана не допускает.
//
// Цель в запросе не участвует — наружу нода не пойдёт, отдаст своё, — но адрес
// в протоколе обязателен, поэтому шлём заведомо пустой.
func (d *Dialer) MeasureFetch(ctx context.Context, size int) (time.Duration, error) {
	// Размер уходит вместе с запросом, до ответа ноды. Отправлять его после
	// статуса нельзя: обе стороны встанут ждать друг друга.
	var ask bytes.Buffer
	if err := vp1.RequestSample(&ask, size); err != nil {
		return 0, err
	}

	start := time.Now()

	stream, err := d.open(ctx, vp1.Address{Type: vp1.AtypIPv4, Host: "0.0.0.0", Port: 0}, vp1.KindProbe, ask.Bytes())
	if err != nil {
		return 0, err
	}
	defer stream.Close()
	release := armStream(ctx, stream)
	defer release()

	granted, err := vp1.ReadGrant(stream)
	if err != nil {
		return 0, streamError(ctx, err)
	}

	read, err := io.CopyN(io.Discard, stream, int64(granted))
	if err != nil {
		return 0, fmt.Errorf("замер оборван: %w", streamError(ctx, err))
	}
	if read <= 0 {
		return 0, errors.New("замер пустой")
	}

	took := time.Since(start)

	// Часы у Windows грубые: всё, что быстрее тика, для нас неотличимо, и
	// притворяться, что мы видим микросекунды, значит выдавать шум за замер.
	return max(took, clockGrain), nil
}

// Granted — сколько байт нода согласилась отдать на последний замер.
//
// Нужно проверке потолка: клиент может попросить сколько угодно, а решает нода.
func (d *Dialer) Granted(ctx context.Context, size int) (int, error) {
	var ask bytes.Buffer
	if err := vp1.RequestSample(&ask, size); err != nil {
		return 0, err
	}

	stream, err := d.open(ctx, vp1.Address{Type: vp1.AtypIPv4, Host: "0.0.0.0", Port: 0}, vp1.KindProbe, ask.Bytes())
	if err != nil {
		return 0, err
	}
	defer stream.Close()

	release := armStream(ctx, stream)
	defer release()
	granted, err := vp1.ReadGrant(stream)
	return granted, streamError(ctx, err)
}

// open открывает поток нужного вида.
//
// Хвост дописывается сразу за запросом, до чтения статуса: если чего-то ждёт
// нода, а мы уже сели ждать её ответа, встанут обе стороны.
func (d *Dialer) open(ctx context.Context, target vp1.Address, kind vp1.Kind, tail []byte) (net.Conn, error) {
	// Всё, что не доехало до ответа ноды, помечаем как недоступность самой
	// ноды. Дальше по этому признаку принимают два решения: надзор — менять
	// ли ноду немедленно, мост — выключать ли датаграммы навсегда. Оба до
	// появления признака ошибались, и оба дорого.
	stream, err := d.pool.Open(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", vp1.ErrNodeUnreachable, err)
	}
	release := armStream(ctx, stream)
	defer release()

	if err := vp1.WriteRequestOf(stream, target, kind); err != nil {
		_ = stream.Close()
		return nil, fmt.Errorf("%w: запрос на %s: %w", vp1.ErrNodeUnreachable, target, streamError(ctx, err))
	}
	if len(tail) > 0 {
		if _, err := stream.Write(tail); err != nil {
			_ = stream.Close()
			return nil, fmt.Errorf("%w: запрос на %s: %w", vp1.ErrNodeUnreachable, target, streamError(ctx, err))
		}
	}

	status, err := vp1.ReadStatus(stream)
	if err != nil {
		err = streamError(ctx, err)
		_ = stream.Close()
		// Старая нода не знает про датаграммы: она видит незнакомый тип
		// адреса и закрывает поток, не ответив. Обрыв ровно здесь и ровно на
		// запросе датаграмм — это она, а не сеть.
		//
		// Ровно так же обрывается поток у ноды, которую в этот миг выключили,
		// поэтому вывод «нода старая» здесь только предположение — и он несёт
		// на себе признак недоступности, чтобы тот, кто выключает датаграммы
		// до конца сессии, мог отличить одно от другого.
		if kind == vp1.KindUDP && closedEarly(err) {
			return nil, fmt.Errorf("%w: %w", vp1.ErrNodeUnreachable, vp1.ErrDatagramsUnsupported)
		}
		return nil, fmt.Errorf("%w: ответ ноды по %s: %w", vp1.ErrNodeUnreachable, target, err)
	}
	if status != vp1.StatusOK {
		_ = stream.Close()
		return nil, &vp1.RefusedError{Target: target.String(), Status: status}
	}
	return stream, nil
}

// Node возвращает ноду, к которой подключён этот дозвон.
func (d *Dialer) Node() Node { return d.node }

// Close закрывает все соединения до ноды.
func (d *Dialer) Close() error { return d.pool.Close() }

// Subscription — срок и квота, с которыми выбрана нода.
func (d *Dialer) Subscription() Subscription { return d.sub }

// withSubscription запоминает подписку в дозвоне.
func (d *Dialer) withSubscription(s Subscription) *Dialer {
	if d != nil {
		d.sub = s
	}
	return d
}

// closedEarly отличает «собеседник закрыл поток, не ответив» от прочих бед.
func closedEarly(err error) bool {
	return errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, net.ErrClosed) ||
		errors.Is(err, io.ErrClosedPipe)
}
