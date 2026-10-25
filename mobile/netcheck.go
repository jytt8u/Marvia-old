package mobile

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jytt8u/marvia/internal/tunbridge"
	"github.com/jytt8u/marvia/internal/vp1"
)

// Проверки через туннель для экрана «Диагностика»: адрес выхода, главные
// сайты и скорость. Те же, что в окне на компьютере (cmd/marvia-windows/
// netcheck.go), и по той же причине все идут через ядро туннеля: ответ
// должен быть про туннель, а не про сеть телефона. Здесь это ещё важнее —
// приложение исключено из собственного VPN, и его обычный HTTP-клиент ушёл
// бы мимо, показав настоящий адрес и страну телефона.

// whereURL отдаёт адрес и страну того, кто спрашивает, обычным текстом.
// Через туннель спрашивающий — нода, и Cloudflare видит её адрес.
var whereURL = "https://www.cloudflare.com/cdn-cgi/trace"

// speedURL — сколько попросить байт, сервер отдаёт ровно столько.
var speedURL = "https://speed.cloudflare.com/__down?bytes=" + strconv.Itoa(speedBytes)

// speedBytes и speedBudget — потолок замера: трафик продавца не бесконечен,
// а за пять секунд скорость уже понятна. Тот же, что в окне.
const (
	speedBytes  = 30 << 20
	speedBudget = 5 * time.Second
)

// probeTimeout — сколько ждать одну страницу: дольше восьми секунд для
// человека это уже «не открывается».
const probeTimeout = 8 * time.Second

// siteScheme подменяется в тестах: у тестового сервера нет сертификата.
var siteScheme = "https://"

// siteChecks — то, без чего человек считает интернет сломанным: поиск,
// видео, мессенджеры, соцсети, ChatGPT и GitHub. Список тот же, что в окне;
// домен — тот, что открывает браузер.
var siteChecks = []struct{ Name, Host string }{
	{"Google", "www.google.com"},
	{"YouTube", "www.youtube.com"},
	{"Telegram", "web.telegram.org"},
	{"WhatsApp", "web.whatsapp.com"},
	{"Instagram", "www.instagram.com"},
	{"ChatGPT", "chatgpt.com"},
	{"X", "x.com"},
	{"GitHub", "github.com"},
}

// Коды ошибок наружу. Слова под них подбирает приложение.
const (
	netNoTunnel = "no-tunnel"
	netFailed   = "failed"
)

// whereView — адрес и страна, под которыми человек виден в интернете.
type whereView struct {
	IP      string `json:"ip,omitempty"`
	Country string `json:"country,omitempty"`
	Error   string `json:"error,omitempty"`
}

// siteView — открылся ли сайт через туннель и за сколько.
type siteView struct {
	Name string `json:"name"`
	OK   bool   `json:"ok"`
	MS   int64  `json:"ms,omitempty"`
}

// speedView — скорость приёма через туннель.
type speedView struct {
	Mbps  float64 `json:"mbps,omitempty"`
	Bytes int64   `json:"bytes,omitempty"`
	Error string  `json:"error,omitempty"`
}

// tunnelDial — дозвон через ядро туннеля с учётом трафика сессии: замер
// скорости тратит настоящие мегабайты продавца, и счётчик обязан их видеть.
// nil — туннеля нет.
func (t *Tunnel) tunnelDial() tunbridge.Dialer {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.running || t.dialer == nil {
		return nil
	}
	return tunbridge.Metered(t.dialer, &t.up, &t.down)
}

// tunnelHTTP — клиент, который ходит только через туннель. Имя цели уходит
// ноде вместе с запросом: системный резолвер телефона не участвует.
func tunnelHTTP(d tunbridge.Dialer) *http.Client {
	return &http.Client{Transport: &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, _, address string) (net.Conn, error) {
			host, portText, err := net.SplitHostPort(address)
			if err != nil {
				return nil, err
			}
			port, err := strconv.ParseUint(portText, 10, 16)
			if err != nil {
				return nil, err
			}
			target, err := vp1.AddressFromHostPort(host, uint16(port))
			if err != nil {
				return nil, err
			}
			return d.DialTarget(ctx, target)
		},
		DisableKeepAlives: true,
	}}
}

// where — адрес выхода через туннель. Запоминается до смены ноды: адрес
// выхода за минуту не меняется, а экран спрашивает его при каждом открытии.
func (t *Tunnel) where(ctx context.Context) whereView {
	t.mu.Lock()
	cached, dialer := t.whereCache, t.dialer
	t.mu.Unlock()
	if cached.IP != "" {
		return cached
	}
	d := t.tunnelDial()
	if d == nil {
		return whereView{Error: netNoTunnel}
	}
	v := fetchWhere(ctx, tunnelHTTP(d))
	if v.IP != "" {
		t.mu.Lock()
		if t.dialer == dialer {
			t.whereCache = v
		}
		t.mu.Unlock()
	}
	return v
}

func fetchWhere(ctx context.Context, hc *http.Client) whereView {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, whereURL, nil)
	if err != nil {
		return whereView{Error: netFailed}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return whereView{Error: netFailed}
	}
	defer resp.Body.Close()
	var v whereView
	sc := bufio.NewScanner(io.LimitReader(resp.Body, 4096))
	for sc.Scan() {
		k, val, ok := strings.Cut(sc.Text(), "=")
		if !ok {
			continue
		}
		switch k {
		case "ip":
			v.IP = val
		case "loc":
			v.Country = strings.ToLower(val)
		}
	}
	if v.IP == "" {
		v.Error = netFailed
	}
	return v
}

// checkSites проверяет сайты через туннель разом: TLS до сайта и ответ на
// запрос главной. Любой HTTP-ответ — «открывается»: редирект на вход или
// капча — это уже сайт, а не блокировка.
func checkSites(ctx context.Context, d tunbridge.Dialer) []siteView {
	hc := tunnelHTTP(d)
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	out := make([]siteView, len(siteChecks))
	var wg sync.WaitGroup
	for i, s := range siteChecks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i] = checkSite(ctx, hc, s.Name, s.Host)
		}()
	}
	wg.Wait()
	return out
}

func checkSite(ctx context.Context, hc *http.Client, name, host string) siteView {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, siteScheme+host+"/", nil)
	if err != nil {
		return siteView{Name: name}
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Linux; Android 14)")
	start := time.Now()
	resp, err := hc.Do(req)
	if err != nil {
		return siteView{Name: name}
	}
	resp.Body.Close()
	return siteView{Name: name, OK: true, MS: max(1, time.Since(start).Milliseconds())}
}

// measureSpeed качает до speedBytes, но не дольше speedBudget, и считает
// скорость по тому, что успело прийти. Время — от первого байта:
// рукопожатие до сервера замера — это отклик, а не скорость.
func measureSpeed(ctx context.Context, hc *http.Client) (speedView, error) {
	ctx, cancel := context.WithTimeout(ctx, speedBudget+probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, speedURL, nil)
	if err != nil {
		return speedView{}, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return speedView{}, err
	}
	defer resp.Body.Close()
	buf := make([]byte, 64<<10)
	var got int64
	var start time.Time
	deadline := time.Now().Add(speedBudget)
	for time.Now().Before(deadline) {
		n, err := resp.Body.Read(buf)
		if n > 0 && start.IsZero() {
			start = time.Now()
		}
		got += int64(n)
		if err != nil {
			break
		}
	}
	// Меньше четверти мегабайта — это не замер, а случайность: одно окно TCP.
	if start.IsZero() || got < 256<<10 {
		return speedView{}, errors.New("слишком мало данных для замера")
	}
	took := max(time.Since(start), time.Millisecond)
	return speedView{Mbps: float64(got*8) / took.Seconds() / 1e6, Bytes: got}, nil
}
