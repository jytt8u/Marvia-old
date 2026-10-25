package mobile

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/jytt8u/marvia/internal/client"
	"github.com/jytt8u/marvia/internal/foreign"
	"github.com/jytt8u/marvia/internal/tunbridge"
	"github.com/jytt8u/marvia/internal/vp1"
)

// Шаги «Проверить всё» на живом телефоне; что из замеров следует — в diag.go.
//
// Мимо туннеля здесь ходят ровно три вещи: TCP-соединение без данных для
// «интернет без VPN» и для «серверы отвечают» (само приложение и так
// подключается к нодам мимо туннеля) и один запрос адреса для сравнения в
// шаге утечки. Адрес, который вернёт этот запрос, — настоящий адрес
// телефона; он сравнивается здесь и наружу не выходит никогда.

// diagDialTimeout — сколько ждать одно TCP-соединение. Дольше четырёх
// секунд адрес для человека уже «не отвечает».
const diagDialTimeout = 4 * time.Second

// diagNodeLimit — сколько нод стучать в шаге «Серверы отвечают». Вывод —
// «отвечают ли вообще и какая ближе» — ясен по первым, а у продавца их бывает
// три десятка.
const diagNodeLimit = 6

// diagNetTargets — адреса для «интернет без VPN»: два разных хозяина и голые
// IP, чтобы шаг не зависел от DNS — его проверяет следующий.
var diagNetTargets = []string{"1.1.1.1:443", "8.8.8.8:443"}

// diagNames — имена для шага DNS. Те, что держат всегда, у двух хозяев.
var diagNames = []string{"www.google.com", "www.cloudflare.com"}

// Пути мимо туннеля. Подменяются в тестах: настоящая сеть в тесте — это
// тест, который падает в самолёте.
var (
	directDial   = (&net.Dialer{}).DialContext
	systemLookup = net.DefaultResolver.LookupHost
	directHTTP   = func() *http.Client { return &http.Client{Transport: &http.Transport{Proxy: nil}} }
)

// udpOnly — протоколы чужих ссылок, у которых на адресе ноды нет TCP:
// стук по TCP сказал бы «не отвечает» про живую ноду.
var udpOnly = []string{"hysteria2", "wireguard"}

// Diagnostics — одна проверка с экрана «Диагностика». Живёт, пока экран
// открыт: Cancel обрывает всё, что ещё идёт, — человек ушёл, и замер скорости
// не должен докачивать тридцать мегабайт в пустоту.
type Diagnostics struct {
	ctx    context.Context
	cancel context.CancelFunc

	link, cacheDir string
	fragment       bool

	mu     sync.Mutex
	tunnel *Tunnel
	state  string
}

// NewDiagnostics готовит проверку. accountLink и cacheDir — рабочий ключ и
// каталог кэша, как у Start; settings — те же настройки туннеля JSON.
func NewDiagnostics(accountLink, cacheDir, settings string) *Diagnostics {
	ctx, cancel := context.WithCancel(context.Background())
	return &Diagnostics{ctx: ctx, cancel: cancel, link: accountLink, cacheDir: cacheDir, fragment: parseSettings(settings).Fragment, state: "off"}
}

// UseTunnel — туннель поднят; state — "connected" или "stalled" (нода
// молчит, а переехать не на что).
func (d *Diagnostics) UseTunnel(t *Tunnel, state string) {
	d.mu.Lock()
	d.tunnel, d.state = t, state
	d.mu.Unlock()
}

// NoTunnel — туннеля нет; state — "off", "connecting" или "failed".
// Отдельно от UseTunnel, а не nil в нём: через gomobile пустой указатель
// ходит плохо.
func (d *Diagnostics) NoTunnel(state string) {
	d.mu.Lock()
	d.tunnel, d.state = nil, state
	d.mu.Unlock()
}

// Cancel обрывает всё, что идёт.
func (d *Diagnostics) Cancel() { d.cancel() }

// up — поднят ли туннель, через который можно спрашивать.
func (d *Diagnostics) up() (*Tunnel, string, bool) {
	d.mu.Lock()
	t, state := d.tunnel, d.state
	d.mu.Unlock()
	ok := t != nil && (state == "connected" || state == "stalled") && t.tunnelDial() != nil
	return t, state, ok
}

// Step проходит один шаг и отдаёт итог JSON. Незнакомый код — пропуск, а не
// ошибка: старое приложение с новым ядром не должно падать на шаге, о
// котором не знает.
func (d *Diagnostics) Step(id string) string {
	out, _ := json.Marshal(d.step(id))
	return string(out)
}

func (d *Diagnostics) step(id string) diagStep {
	ctx := d.ctx
	t, state, up := d.up()
	switch id {
	case "net":
		n, best := fastest(ctx, diagNetTargets)
		return judgeNet(n > 0, millis(best))

	case "dns":
		start := time.Now()
		ok := false
		for _, name := range diagNames {
			var err error
			if up {
				err = t.lookupThrough(ctx, name)
			} else {
				err = lookupSystem(ctx, name)
			}
			if err == nil {
				ok = true
				break
			}
		}
		return judgeDNS(ok, up, millis(time.Since(start)))

	case "node":
		addresses := d.nodeAddresses(t, up)
		n, best := fastest(ctx, addresses)
		return judgeNode(n, len(addresses), millis(best))

	case "tunnel":
		return judgeTunnel(state, d.fragment)

	case "where":
		if !up {
			return judgeWhere(false, "", "")
		}
		w := t.where(ctx)
		return judgeWhere(true, w.IP, w.Country)

	case "leak":
		if !up {
			return judgeLeak(false, "", "")
		}
		tunnel := t.where(ctx)
		// Обычным путём приложения — мимо туннеля, из которого оно
		// исключено. Этот ответ и есть настоящий адрес; дальше этой строки он
		// не уходит.
		direct := fetchWhere(ctx, directHTTP())
		return judgeLeak(true, tunnel.IP, direct.IP)

	case "sites":
		if !up {
			return judgeSites(false, 0, 0)
		}
		dialer := t.tunnelDial()
		if dialer == nil {
			return judgeSites(false, 0, 0)
		}
		opened := 0
		for _, s := range checkSites(ctx, dialer) {
			if s.OK {
				opened++
			}
		}
		return judgeSites(true, opened, len(siteChecks))
	}
	return diagStep{ID: id, St: diagOff}
}

// Where — адрес и страна выхода через туннель, JSON: {"ip","country"} или
// {"error"}. Коды ошибок: no-tunnel, failed.
func (d *Diagnostics) Where() string {
	t, _, up := d.up()
	v := whereView{Error: netNoTunnel}
	if up {
		v = t.where(d.ctx)
	}
	out, _ := json.Marshal(v)
	return string(out)
}

// Speed меряет скорость приёма через туннель, JSON: {"mbps","bytes"} или
// {"error"}. Только по кнопке: трафик продавца тратится настоящий.
func (d *Diagnostics) Speed() string {
	v := speedView{Error: netNoTunnel}
	if t, _, up := d.up(); up {
		if dialer := t.tunnelDial(); dialer != nil {
			var err error
			v, err = measureSpeed(d.ctx, tunnelHTTP(dialer))
			if err != nil {
				v = speedView{Error: netFailed}
			}
		}
	}
	out, _ := json.Marshal(v)
	return string(out)
}

// nodeAddresses — кого стучать: текущую ноду первой (о ней человек и
// спрашивает), потом остальные. Без туннеля — из кэша подписки, без похода в
// панель: проверка не должна сама слать запрос имени панели оператору.
func (d *Diagnostics) nodeAddresses(t *Tunnel, up bool) []string {
	var addresses []string
	add := func(a string) {
		if a != "" && !slices.Contains(addresses, a) {
			addresses = append(addresses, a)
		}
	}
	if isForeign(d.link) {
		sub, _ := foreign.Cached(d.link, foreign.CachePath(d.cacheDir, d.link))
		tcp := map[string]bool{}
		for _, l := range sub.Links {
			if !slices.Contains(udpOnly, l.Protocol) {
				tcp[net.JoinHostPort(l.Host, strconv.Itoa(l.Port))] = true
			}
		}
		if up {
			if a := t.currentNode().Address; tcp[a] {
				add(a)
			}
		}
		for _, l := range sub.Links {
			if a := net.JoinHostPort(l.Host, strconv.Itoa(l.Port)); tcp[a] {
				add(a)
			}
		}
	} else {
		var nodes []client.Node
		if up {
			nodes = append([]client.Node{t.currentNode()}, t.allNodes()...)
		} else if account, err := client.ParseAccountLink(d.link); err == nil {
			if cached, err := client.LoadCache(accountCachePath(d.cacheDir, account.SubscriptionURL), account.SubscriptionURL); err == nil {
				nodes = cached.Nodes()
			}
		}
		for _, n := range nodes {
			add(n.Address)
		}
	}
	return addresses[:min(len(addresses), diagNodeLimit)]
}

func (t *Tunnel) currentNode() client.Node {
	t.mu.Lock()
	dialer := t.dialer
	t.mu.Unlock()
	if dialer == nil {
		return client.Node{}
	}
	return dialer.Node()
}

func (t *Tunnel) allNodes() []client.Node {
	t.mu.Lock()
	dialer := t.dialer
	t.mu.Unlock()
	if dialer == nil {
		return nil
	}
	return dialer.Nodes()
}

// lookupThrough спрашивает имя тем же путём, каким его получают приложения
// в туннеле: через резолвер по HTTPS, если ядро умеет до него, иначе
// открытым DNS по TCP до резолвера из настроек — тоже через туннель.
func (t *Tunnel) lookupThrough(ctx context.Context, name string) error {
	dialer := t.tunnelDial()
	if dialer == nil {
		return errors.New("туннель не поднят")
	}
	t.mu.Lock()
	dns, settings := t.dns, t.settings
	t.mu.Unlock()
	if dns == "" {
		dns = DefaultDNS
	}
	query, err := aQuery(name)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, diagDialTimeout)
	defer cancel()
	var answer []byte
	if names := settings.names(dns, dialer); names != nil {
		answer, err = names.Exchange(ctx, query, false)
	} else {
		answer, err = exchangePlain(ctx, dialer, dns, query)
	}
	if err != nil {
		return err
	}
	return answered(answer)
}

func exchangePlain(ctx context.Context, d tunbridge.Dialer, dns string, query []byte) ([]byte, error) {
	host, portText, err := net.SplitHostPort(dns)
	if err != nil {
		host, portText = dns, "53"
	}
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil {
		return nil, err
	}
	target, err := vp1.AddressFromHostPort(host, uint16(port))
	if err != nil {
		return nil, err
	}
	stream, err := d.DialTarget(ctx, target)
	if err != nil {
		return nil, err
	}
	defer stream.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = stream.SetDeadline(deadline)
	}
	return tunbridge.ExchangeOverTCP(stream, query)
}

// lookupSystem — имя через резолвер телефона, как без VPN.
func lookupSystem(ctx context.Context, name string) error {
	ctx, cancel := context.WithTimeout(ctx, diagDialTimeout)
	defer cancel()
	_, err := systemLookup(ctx, name)
	return err
}

// aQuery — вопрос «адрес IPv4 для name» с флагом рекурсии.
func aQuery(name string) ([]byte, error) {
	fqdn, err := dnsmessage.NewName(name + ".")
	if err != nil {
		return nil, err
	}
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: uint16(time.Now().UnixNano()), RecursionDesired: true})
	b.EnableCompression()
	if err := b.StartQuestions(); err != nil {
		return nil, err
	}
	if err := b.Question(dnsmessage.Question{Name: fqdn, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}); err != nil {
		return nil, err
	}
	return b.Finish()
}

// answered — ответ без ошибки и хотя бы с одной записью. Пустой «успех»
// тоже не годится: имена, которые держат всегда, без адреса не бывают.
func answered(raw []byte) error {
	var p dnsmessage.Parser
	h, err := p.Start(raw)
	if err != nil {
		return err
	}
	if h.RCode != dnsmessage.RCodeSuccess {
		return errors.New("резолвер ответил ошибкой " + h.RCode.String())
	}
	if err := p.SkipAllQuestions(); err != nil {
		return err
	}
	answers, err := p.AllAnswers()
	if err != nil {
		return err
	}
	if len(answers) == 0 {
		return errors.New("в ответе нет адресов")
	}
	return nil
}

// fastest стучится по всем адресам разом мимо туннеля и отдаёт, сколько
// ответило и за сколько ответил самый быстрый. Соединение без данных: ни
// запроса, ни имени сайта в нём нет.
func fastest(ctx context.Context, addresses []string) (int, time.Duration) {
	var (
		mu    sync.Mutex
		count int
		best  time.Duration
		wg    sync.WaitGroup
	)
	for _, a := range addresses {
		wg.Add(1)
		go func() {
			defer wg.Done()
			dctx, cancel := context.WithTimeout(ctx, diagDialTimeout)
			defer cancel()
			start := time.Now()
			conn, err := directDial(dctx, "tcp", a)
			if err != nil {
				return
			}
			took := time.Since(start)
			_ = conn.Close()
			mu.Lock()
			count++
			if best == 0 || took < best {
				best = took
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	return count, best
}

func millis(d time.Duration) int64 {
	if d <= 0 {
		return 0
	}
	return max(1, d.Milliseconds())
}
