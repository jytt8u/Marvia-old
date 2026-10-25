package mobile

import (
	"encoding/json"
	"net"
	"sort"

	"github.com/jytt8u/marvia/internal/client"
	"github.com/jytt8u/marvia/internal/foreign"
	"github.com/jytt8u/marvia/internal/redact"
	"github.com/jytt8u/marvia/internal/transport"
)

// Качество связи и устройство соединения для экрана «Диагностика».
//
// «Отклик 32 мс» с момента подключения ничего не говорит про сейчас: нода
// могла начать терять ответы или тормозить. Поэтому ядро помнит последние
// замеры отклика — те, что приложение и так делает по расписанию, — и
// отдаёт медиану, разброс и долю потерь. Новых пакетов ради этого не шлётся.

// qualityKeep — сколько замеров помнить: хватает, чтобы увидеть, что связь
// «поплыла», и старая беда не висит в числах вечно. Как в окне на компьютере.
const qualityKeep = 60

// qualityView — качество связи для экрана. Пусто (Samples == 0) — ядро ещё
// ничего не мерило, и экран чисел не показывает, а не рисует нули.
type qualityView struct {
	// PingMS — медиана последних удачных замеров: одиночный всплеск не
	// должен перекрашивать число.
	PingMS int64 `json:"ping_ms"`
	// JitterMS — средний разброс между соседними удачными замерами.
	JitterMS int64 `json:"jitter_ms"`
	// LossPct — доля замеров без ответа, в процентах.
	LossPct int `json:"loss_pct"`
	// Samples — сколько замеров легло в эти числа.
	Samples int `json:"samples"`
}

type qualityRing struct {
	samples []int64
}

// add записывает замер; ноль — ответа не было.
func (q *qualityRing) add(ms int64) {
	q.samples = append(q.samples, max(0, ms))
	if len(q.samples) > qualityKeep {
		q.samples = q.samples[len(q.samples)-qualityKeep:]
	}
}

func (q *qualityRing) reset() { q.samples = nil }

func (q *qualityRing) view() qualityView {
	if len(q.samples) == 0 {
		return qualityView{}
	}
	v := qualityView{Samples: len(q.samples)}
	var ok []int64
	lost, jitter, pairs := 0, int64(0), 0
	prev := int64(0)
	for _, s := range q.samples {
		if s <= 0 {
			lost++
			prev = 0
			continue
		}
		if prev > 0 {
			d := s - prev
			if d < 0 {
				d = -d
			}
			jitter += d
			pairs++
		}
		prev = s
		ok = append(ok, s)
	}
	v.LossPct = lost * 100 / len(q.samples)
	if pairs > 0 {
		v.JitterMS = jitter / int64(pairs)
	}
	if n := len(ok); n > 0 {
		recent := append([]int64(nil), ok[max(0, n-5):]...)
		sort.Slice(recent, func(i, j int) bool { return recent[i] < recent[j] })
		v.PingMS = recent[len(recent)/2]
	}
	return v
}

// Quality — качество связи с текущей нодой, JSON; см. qualityView.
func (t *Tunnel) Quality() string {
	t.mu.Lock()
	v := t.quality.view()
	t.mu.Unlock()
	out, _ := json.Marshal(v)
	return string(out)
}

// routeView — как идём до ноды. Только то, с чем дозванивается ядро: чего
// оно не знает, того здесь нет.
type routeView struct {
	// Protocol — vp1 у ключа Marvia, иначе протокол чужой ссылки.
	Protocol string `json:"protocol,omitempty"`
	// Transport — у VP1: tls, reality, ws или quic; у чужих — транспорт
	// Xray: raw, ws, grpc, xhttp…
	Transport string `json:"transport,omitempty"`
	// Security — у чужих: tls, reality или none.
	Security string `json:"security,omitempty"`
	// SNI — имя прикрытия; SpareSNI — сколько запасных у REALITY.
	SNI      string `json:"sni,omitempty"`
	SpareSNI int    `json:"spare_sni,omitempty"`
	// Fingerprint — чьё TLS-приветствие изображается; у QUIC своего
	// приветствия uTLS нет, и поле пустое.
	Fingerprint string `json:"fingerprint,omitempty"`
}

// Route — устройство текущего соединения, JSON; "{}" — туннеля нет.
func (t *Tunnel) Route() string {
	t.mu.Lock()
	dialer := t.dialer
	t.mu.Unlock()
	var r routeView
	switch d := dialer.(type) {
	case nil:
	case *foreign.Supervisor:
		fr := d.Route()
		r = routeView{Protocol: fr.Protocol, Transport: fr.Network, Security: fr.Security, SNI: fr.SNI, Fingerprint: fr.Fingerprint}
	default:
		if n := d.Node(); n.Address != "" {
			r = vp1Route(n)
		}
	}
	out, _ := json.Marshal(r)
	return string(out)
}

// vp1Route — то же для ноды Marvia, теми же правилами, что у дозвона
// (client.NewDialer): без SNI имя — хост адреса, незнакомый отпечаток —
// Chrome, QUIC — только у обычного TLS. Своего отпечатка и «только TCP» на
// телефоне человек не выбирает, так что берётся то, что задал продавец.
func vp1Route(n client.Node) routeView {
	r := routeView{Protocol: "vp1", Transport: string(n.Transport()), SNI: n.SNI}
	if n.QUIC && n.Transport() == client.TransportTLS {
		r.Transport = "quic"
	}
	if r.SNI == "" {
		r.SNI, _, _ = net.SplitHostPort(n.Address)
	}
	if n.Transport() == client.TransportReality {
		r.SpareSNI = len(n.SNIExtra)
	}
	if r.Transport != "quic" {
		r.Fingerprint = "chrome"
		if _, known := transport.Fingerprint(n.Fingerprint); known {
			r.Fingerprint = n.Fingerprint
		}
	}
	return r
}

// Redact закрывает в тексте адреса и секреты ссылок — тем же правилом, что
// журналы серверов. Отчёт уходит продавцу, а иногда и в публичный чат, и
// приложению незачем держать вторую, свою копию этого правила.
func Redact(text string) string { return redact.Text(text) }
