package nodesync

import (
	"net/netip"
	"strings"
	"sync"

	"github.com/jytt8u/marvia/internal/connguard"
)

// Защита от флуда: пределы на приёме задаёт продавец в панели, а нода
// применяет их на ходу (internal/connguard). Обратно нода сообщает только,
// сколько соединений отклонила, — адресов нет ни в отчёте, ни на диске.

// guardConfig — пределы в ответе панели. Панель постарше их не шлёт, и
// нода тогда работает со значениями по умолчанию, как до этой настройки.
type guardConfig struct {
	Rate    float64  `json:"rate"`
	Burst   float64  `json:"burst"`
	MaxOpen int      `json:"max_open"`
	Blocked []string `json:"blocked"`
}

func (g guardConfig) limits() connguard.Limits {
	lim := connguard.Limits{Rate: g.Rate, Burst: g.Burst, MaxOpen: g.MaxOpen}
	for _, s := range g.Blocked {
		if p, ok := parseBlocked(s); ok {
			lim.Blocked = append(lim.Blocked, p)
		}
	}
	return lim
}

// parseBlocked понимает и сеть, и одиночный адрес. Непонятную строку
// пропускаем, а не роняем синхронизацию: из-за опечатки в одном адресе
// нода не должна перестать получать список покупателей.
func parseBlocked(s string) (netip.Prefix, bool) {
	s = strings.TrimSpace(s)
	if p, err := netip.ParsePrefix(s); err == nil {
		return p.Masked(), true
	}
	if a, err := netip.ParseAddr(s); err == nil {
		a = a.Unmap()
		return netip.PrefixFrom(a, a.BitLen()), true
	}
	return netip.Prefix{}, false
}

// guards — приёмы, которым нода раздаёт пределы. Обычно один; у ноды за
// CDN или у входа каскада — тоже один, но список дешевле, чем угадывать.
type guards struct {
	mu   sync.Mutex
	list []connguard.Guard
}

// Guard подключает приём к пределам из панели и к отчёту об отклонённых.
func (c *Client) Guard(g connguard.Guard) {
	c.guards.mu.Lock()
	c.guards.list = append(c.guards.list, g)
	c.guards.mu.Unlock()
}

func (c *Client) applyGuard(cfg *guardConfig) {
	if cfg == nil {
		return
	}
	lim := cfg.limits()
	c.guards.mu.Lock()
	defer c.guards.mu.Unlock()
	for _, g := range c.guards.list {
		g.Update(lim)
	}
}

// takeGuardStats собирает отклонённые со всех приёмов с прошлого отчёта.
func (c *Client) takeGuardStats() connguard.Stats {
	c.guards.mu.Lock()
	defer c.guards.mu.Unlock()
	var total connguard.Stats
	for _, g := range c.guards.list {
		s := g.Take()
		total.ByRate += s.ByRate
		total.ByOpen += s.ByOpen
		total.ByBlock += s.ByBlock
	}
	return total
}
