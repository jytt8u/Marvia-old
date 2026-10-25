package mobile

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/jytt8u/marvia/internal/client"
	"github.com/jytt8u/marvia/internal/vp1"
)

// diagBackend — туннель для проверок: дозвон уходит туда, куда скажет тест.
// Указатель, а не значение: ядро сравнивает дозвоны, чтобы не приписать
// замер прежней ноды новой.
type diagBackend struct {
	client.Backend
	node  client.Node
	dial  func(context.Context, vp1.Address) (net.Conn, error)
	ping  func() (time.Duration, error)
	dials atomic.Int32
}

func (b *diagBackend) DialTarget(ctx context.Context, target vp1.Address) (net.Conn, error) {
	b.dials.Add(1)
	return b.dial(ctx, target)
}

func (b *diagBackend) Node() client.Node    { return b.node }
func (b *diagBackend) Nodes() []client.Node { return []client.Node{b.node} }
func (b *diagBackend) Ping(context.Context) (time.Duration, error) {
	return b.ping()
}

// toServer — дозвон туннеля, который всегда приходит на тестовый сервер.
func toServer(server *httptest.Server) func(context.Context, vp1.Address) (net.Conn, error) {
	return func(ctx context.Context, _ vp1.Address) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", server.Listener.Addr().String())
	}
}

func trace(ip string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "fl=1\nip="+ip+"\nloc=FI\n")
	}))
}

func decodeStep(t *testing.T, raw string) diagStep {
	t.Helper()
	var s diagStep
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		t.Fatalf("шаг не разобрался: %s: %v", raw, err)
	}
	return s
}

func allJudged() []diagStep {
	return []diagStep{
		judgeNet(true, 12), judgeNet(false, 0),
		judgeDNS(true, true, 5), judgeDNS(false, true, 0), judgeDNS(false, false, 0),
		judgeNode(3, 3, 40), judgeNode(2, 3, 40), judgeNode(3, 3, 450), judgeNode(0, 3, 0), judgeNode(0, 1, 0), judgeNode(0, 0, 0),
		judgeTunnel("connected", false), judgeTunnel("failed", false), judgeTunnel("failed", true),
		judgeTunnel("stalled", false), judgeTunnel("off", false), judgeTunnel("connecting", false),
		judgeWhere(true, "198.51.100.1", "fi"), judgeWhere(true, "", ""), judgeWhere(false, "", ""),
		judgeLeak(true, "198.51.100.1", "203.0.113.7"), judgeLeak(true, "198.51.100.1", "198.51.100.1"),
		judgeLeak(true, "", "203.0.113.7"), judgeLeak(false, "", ""),
		judgeSites(true, 8, 8), judgeSites(true, 5, 8), judgeSites(true, 0, 8), judgeSites(false, 0, 0),
	}
}

// Хороший шаг кнопок не получает, плохой — только из тех, что приложение
// умеет делать. Кнопка под зелёной галочкой — кнопка, которой нечего делать.
func TestOnlyTroubledStepsOfferActionsTheAppCanDo(t *testing.T) {
	for _, s := range allJudged() {
		if !slices.Contains(diagSteps, s.ID) {
			t.Errorf("шаг %q не из списка diagSteps", s.ID)
		}
		if !slices.Contains([]string{diagOK, diagWarn, diagBad, diagOff}, s.St) {
			t.Errorf("%s: незнакомое состояние %q", s.ID, s.St)
		}
		if s.St == diagOK && len(s.Act) > 0 {
			t.Errorf("%s: хороший шаг предлагает %v", s.ID, s.Act)
		}
		for _, a := range s.Act {
			if !slices.Contains(diagActs, a) {
				t.Errorf("%s: действие %q приложение не умеет", s.ID, a)
			}
		}
	}
	if got := strings.Split(DiagSteps(), ","); !slices.Equal(got, diagSteps) {
		t.Errorf("наружу уходят шаги %v, а не %v", got, diagSteps)
	}
}

// Где Marvia помочь не может, кнопки нет; включённое дробление второй раз
// не включишь, а сервер не сменишь, когда он один.
func TestNoButtonWhereThereIsNothingToPress(t *testing.T) {
	if s := judgeNet(false, 0); len(s.Act) != 0 {
		t.Errorf("нет интернета, а предлагается %v", s.Act)
	}
	if s := judgeNode(0, 1, 0); slices.Contains(s.Act, "nodes") {
		t.Error("сервер один, а предлагается сменить его на другой")
	}
	if s := judgeTunnel("failed", true); slices.Contains(s.Act, "fragment") {
		t.Error("дробление уже включено, а кнопка «включить» на месте")
	}
	if s := judgeTunnel("failed", false); !slices.Contains(s.Act, "fragment") {
		t.Error("туннель не поднялся без дробления, а включить его не предлагают")
	}
}

// У каждого шага и каждой кнопки есть слова на обоих языках приложения и
// обработчик в коде экрана: шаг без слов показал бы пустую строку, кнопка
// без обработчика — обещание, которое ничего не делает.
func TestEveryStepAndButtonHasWordsInBothLanguagesAndAHandler(t *testing.T) {
	res := filepath.Join("..", "android", "app", "src", "main", "res")
	dicts := map[string]string{}
	for _, dir := range []string{"values", "values-ru"} {
		raw, err := os.ReadFile(filepath.Join(res, dir, "diagnostics.xml"))
		if err != nil {
			t.Fatal(err)
		}
		dicts[dir] = string(raw)
	}
	code, err := os.ReadFile(filepath.Join("..", "android", "app", "src", "main", "kotlin", "io", "marvia", "android", "DiagnosticsScreen.kt"))
	if err != nil {
		t.Fatal(err)
	}
	need := map[string]bool{}
	for _, id := range diagSteps {
		need["diag_step_"+id] = true
	}
	for _, a := range diagActs {
		need["diag_act_"+a] = true
		if !strings.Contains(string(code), `"`+a+`" ->`) {
			t.Errorf("у кнопки %q нет обработчика в DiagnosticsScreen.kt", a)
		}
	}
	for _, s := range allJudged() {
		if s.St == diagWarn || s.St == diagBad {
			need["diag_"+s.ID+"_"+s.St] = true
		}
	}
	for name := range need {
		for dir, text := range dicts {
			if !strings.Contains(text, `name="`+name+`"`) {
				t.Errorf("в %s/diagnostics.xml нет строки %s", dir, name)
			}
		}
	}
}

// Без туннеля шаги, которым нужен туннель, — пропуск, и они не ходят никуда:
// ни через туннель, которого нет, ни мимо него «на всякий случай».
func TestStepsThatNeedTheTunnelAreSkippedWithoutIt(t *testing.T) {
	hit := atomic.Int32{}
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hit.Add(1) }))
	defer server.Close()
	defer swap(&whereURL, server.URL)()
	defer swap(&speedURL, server.URL)()

	d := NewDiagnostics("", t.TempDir(), "")
	d.NoTunnel("off")
	for _, id := range []string{"where", "leak", "sites"} {
		if s := decodeStep(t, d.Step(id)); s.St != diagOff || len(s.Act) > 0 {
			t.Errorf("%s без туннеля: %+v", id, s)
		}
	}
	if s := decodeStep(t, d.Step("tunnel")); s.St != diagOff || !slices.Contains(s.Act, "connect") {
		t.Errorf("без туннеля шаг подключения не предлагает подключиться: %+v", s)
	}
	if !strings.Contains(d.Where(), netNoTunnel) || !strings.Contains(d.Speed(), netNoTunnel) {
		t.Errorf("адрес или скорость без туннеля: %s, %s", d.Where(), d.Speed())
	}
	if hit.Load() != 0 {
		t.Fatalf("без туннеля проверка ушла в сеть %d раз", hit.Load())
	}
}

// Шаг утечки сравнивает адрес обычного пути приложения с адресом выхода
// туннеля, но ни один из них в итог не попадает: снимок экрана уходит
// продавцу, а настоящему адресу там не место.
func TestLeakStepComparesWithoutCarryingAnyAddress(t *testing.T) {
	tunnelSide := trace("198.51.100.1")
	defer tunnelSide.Close()
	direct := trace("203.0.113.7")
	defer direct.Close()
	defer swap(&whereURL, direct.URL+"/cdn-cgi/trace")()

	backend := &diagBackend{dial: toServer(tunnelSide)}
	d := NewDiagnostics("", t.TempDir(), "")
	d.UseTunnel(&Tunnel{dialer: backend, running: true}, "connected")
	raw := d.Step("leak")
	if s := decodeStep(t, raw); s.St != diagOK {
		t.Fatalf("адреса разные, а шаг %+v", s)
	}
	if strings.Contains(raw, "203.0.113.7") || strings.Contains(raw, "198.51.100.1") {
		t.Fatalf("в итоге шага утечки адрес: %s", raw)
	}

	// Выход ноды в той же сети, что телефон: сайты видят настоящий адрес.
	same := &diagBackend{dial: toServer(direct)}
	d.UseTunnel(&Tunnel{dialer: same, running: true}, "connected")
	raw = d.Step("leak")
	if s := decodeStep(t, raw); s.St != diagBad || !slices.Contains(s.Act, "nodes") || strings.Contains(raw, "203.0.113.7") {
		t.Fatalf("совпавший адрес не замечен или попал в итог: %s", raw)
	}
}

// Адрес, сайты и скорость спрашиваются только через туннель, а имя цели
// уходит ноде: обычный путь приложения — это настоящий адрес телефона.
func TestTunnelChecksNeverGoAroundTheTunnel(t *testing.T) {
	body := strings.Repeat("x", 512<<10)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/cdn-cgi/trace" {
			_, _ = io.WriteString(w, "ip=198.51.100.1\nloc=FI\n")
			return
		}
		_, _ = io.WriteString(w, body)
	}))
	defer server.Close()
	defer swap(&whereURL, "http://where.invalid/cdn-cgi/trace")()
	defer swap(&speedURL, "http://speed.invalid/__down")()
	defer swap(&siteScheme, "http://")()
	defer swapHTTP(func() *http.Client {
		t.Error("проверка через туннель пошла обычным путём")
		return &http.Client{}
	})()

	// Сайты проверяются разом, и дозвоны идут из нескольких горутин.
	var (
		mu    sync.Mutex
		hosts []string
	)
	backend := &diagBackend{}
	backend.dial = func(ctx context.Context, target vp1.Address) (net.Conn, error) {
		if target.Type != vp1.AtypDomain {
			t.Errorf("имя %v разрешилось на телефоне, а не у ноды", target)
		}
		mu.Lock()
		hosts = append(hosts, target.Host)
		mu.Unlock()
		return toServer(server)(ctx, target)
	}
	tunnel := &Tunnel{dialer: backend, running: true}
	d := NewDiagnostics("", t.TempDir(), "")
	d.UseTunnel(tunnel, "connected")

	if s := decodeStep(t, d.Step("where")); s.St != diagOK || s.IP != "198.51.100.1" || s.Country != "fi" {
		t.Fatalf("адрес через туннель: %+v", s)
	}
	if s := decodeStep(t, d.Step("sites")); s.St != diagOK || s.N != len(siteChecks) {
		t.Fatalf("сайты через туннель: %+v", s)
	}
	var speed speedView
	if err := json.Unmarshal([]byte(d.Speed()), &speed); err != nil || speed.Mbps <= 0 {
		t.Fatalf("скорость через туннель: %+v, %v", speed, err)
	}
	if tunnel.down.Load() < int64(len(body)) {
		t.Fatal("замер скорости не учтён в трафике сессии")
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Contains(hosts, "where.invalid") || !slices.Contains(hosts, "web.telegram.org") {
		t.Fatalf("через туннель ушли не те имена: %v", hosts)
	}
}

// С туннелем шаг DNS спрашивает резолвер из настроек тем же путём, каким
// его получают приложения, — через туннель, а не резолвером телефона.
func TestDNSStepAsksTheChosenResolverThroughTheTunnel(t *testing.T) {
	defer swapLookup(func(context.Context, string) ([]string, error) {
		t.Error("с туннелем имя спросили у резолвера телефона")
		return nil, errors.New("мимо туннеля")
	})()
	var asked vp1.Address
	backend := &diagBackend{dial: func(_ context.Context, target vp1.Address) (net.Conn, error) {
		asked = target
		client, server := net.Pipe()
		go answerOverTCP(t, server)
		return client, nil
	}}
	tunnel := &Tunnel{dialer: backend, running: true, dns: "9.9.9.9:53", settings: tunnelSettings{PlainDNS: true}}
	d := NewDiagnostics("", t.TempDir(), "")
	d.UseTunnel(tunnel, "connected")
	if s := decodeStep(t, d.Step("dns")); s.St != diagOK {
		t.Fatalf("резолвер ответил, а шаг %+v", s)
	}
	if asked.Host != "9.9.9.9" || asked.Port != 53 {
		t.Fatalf("спросили %+v, а не резолвер из настроек", asked)
	}
}

func answerOverTCP(t *testing.T, conn net.Conn) {
	defer conn.Close()
	var size [2]byte
	if _, err := io.ReadFull(conn, size[:]); err != nil {
		return
	}
	query := make([]byte, binary.BigEndian.Uint16(size[:]))
	if _, err := io.ReadFull(conn, query); err != nil {
		return
	}
	var p dnsmessage.Parser
	h, err := p.Start(query)
	if err != nil {
		t.Error(err)
		return
	}
	q, err := p.Question()
	if err != nil {
		t.Error(err)
		return
	}
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: h.ID, Response: true, RecursionAvailable: true})
	_ = b.StartQuestions()
	_ = b.Question(q)
	_ = b.StartAnswers()
	_ = b.AResource(dnsmessage.ResourceHeader{Name: q.Name, Class: dnsmessage.ClassINET, TTL: 60}, dnsmessage.AResource{A: [4]byte{198, 51, 100, 2}})
	answer, _ := b.Finish()
	binary.BigEndian.PutUint16(size[:], uint16(len(answer)))
	_, _ = conn.Write(append(size[:], answer...))
}

// Без туннеля ноды берутся из кэша подписки, а не из панели, и у чужих
// ссылок по UDP их не стучат по TCP: живая нода Hysteria2 выглядела бы
// мёртвой.
func TestNodeStepSkipsUDPOnlyLinksAndNeedsNoPanel(t *testing.T) {
	var dialed []string
	defer swapDial(func(_ context.Context, _, address string) (net.Conn, error) {
		dialed = append(dialed, address)
		return nil, errors.New("молчит")
	})()
	link := "vless://00000000-0000-0000-0000-000000000000@a.example:443?security=tls&type=tcp#A\nhysteria2://secret@b.example:443#B"
	d := NewDiagnostics(link, t.TempDir(), "")
	d.NoTunnel("off")
	s := decodeStep(t, d.Step("node"))
	if s.Of != 1 || !slices.Equal(dialed, []string{"a.example:443"}) {
		t.Fatalf("стучали %v, шаг %+v", dialed, s)
	}
	if s.St != diagBad || len(s.Act) != 0 {
		t.Fatalf("единственная нода молчит, а шаг %+v", s)
	}
}

// Качество — из тех замеров, что приложение и так делает: неответ считается
// потерей, а замер прежней ноды не смешивается с новой.
func TestQualityCountsLossAndJitterOfTheCurrentNodeOnly(t *testing.T) {
	answers := []time.Duration{40 * time.Millisecond, 60 * time.Millisecond, 0, 50 * time.Millisecond}
	i := 0
	backend := &diagBackend{ping: func() (time.Duration, error) {
		d := answers[i]
		i++
		if d == 0 {
			return 0, errors.New("нет ответа")
		}
		return d, nil
	}}
	tunnel := &Tunnel{dialer: backend, running: true}
	for range answers {
		tunnel.Ping()
	}
	var q qualityView
	if err := json.Unmarshal([]byte(tunnel.Quality()), &q); err != nil {
		t.Fatal(err)
	}
	if q.Samples != 4 || q.LossPct != 25 || q.JitterMS != 20 || q.PingMS != 50 {
		t.Fatalf("качество %+v", q)
	}
	tunnel.switched(client.Node{Name: "другая"})
	if err := json.Unmarshal([]byte(tunnel.Quality()), &q); err != nil || q.Samples != 0 {
		t.Fatalf("после переезда остались замеры прежней ноды: %+v", q)
	}
}

// Устройство соединения — ровно то, с чем дозванивается ядро.
func TestRouteSaysWhatTheDialerActuallyUses(t *testing.T) {
	cases := []struct {
		node client.Node
		want routeView
	}{
		{client.Node{Address: "n.example:443"}, routeView{Protocol: "vp1", Transport: "tls", SNI: "n.example", Fingerprint: "chrome"}},
		{client.Node{Address: "n.example:443", QUIC: true, SNI: "cdn.example"}, routeView{Protocol: "vp1", Transport: "quic", SNI: "cdn.example"}},
		{client.Node{Address: "203.0.113.7:443", RealityPublicKey: "k", SNI: "www.example.com", SNIExtra: []string{"a", "b"}, Fingerprint: "firefox"},
			routeView{Protocol: "vp1", Transport: "reality", SNI: "www.example.com", SpareSNI: 2, Fingerprint: "firefox"}},
		{client.Node{Address: "n.example:443", Fingerprint: "netscape"}, routeView{Protocol: "vp1", Transport: "tls", SNI: "n.example", Fingerprint: "chrome"}},
	}
	for _, c := range cases {
		if got := vp1Route(c.node); got != c.want {
			t.Errorf("%+v: %+v, а не %+v", c.node, got, c.want)
		}
	}
	if (&Tunnel{}).Route() != "{}" {
		t.Error("без туннеля устройство соединения не пустое")
	}
}

// В отчёте продавцу адреса закрыты: и выход ноды, и любой адрес из журнала.
func TestReportForTheSellerHasNoAddresses(t *testing.T) {
	text := Redact("выход 198.51.100.1, нода 203.0.113.7:443, v6 [2001:db8::1]:443, ключ marvia://secret@panel.example/sub/abc")
	for _, leak := range []string{"198.51.100.1", "203.0.113.7", "2001:db8::1", "secret"} {
		if strings.Contains(text, leak) {
			t.Errorf("в отчёте осталось %q: %s", leak, text)
		}
	}
}

func swap[T any](p *T, v T) func() {
	old := *p
	*p = v
	return func() { *p = old }
}

func swapHTTP(f func() *http.Client) func() { return swap(&directHTTP, f) }

func swapLookup(f func(context.Context, string) ([]string, error)) func() {
	return swap(&systemLookup, f)
}

func swapDial(f func(context.Context, string, string) (net.Conn, error)) func() {
	return swap(&directDial, f)
}
