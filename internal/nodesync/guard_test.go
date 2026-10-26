package nodesync

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sync"
	"testing"

	"github.com/jytt8u/marvia/internal/connguard"
	"github.com/jytt8u/marvia/internal/users"
)

// fakeGuard запоминает пределы и отдаёт заданные счётчики.
type fakeGuard struct {
	net.Listener
	mu    sync.Mutex
	got   []connguard.Limits
	stats connguard.Stats
}

func (g *fakeGuard) Update(l connguard.Limits) { g.mu.Lock(); g.got = append(g.got, l); g.mu.Unlock() }
func (g *fakeGuard) Take() connguard.Stats {
	g.mu.Lock()
	defer g.mu.Unlock()
	s := g.stats
	g.stats = connguard.Stats{}
	return s
}

// Продавец ужесточил пределы и закрыл сеть — нода применяет это со
// следующей синхронизацией. Опечатка в одном адресе не роняет ни список
// покупателей, ни остальные закрытые адреса.
func TestNodeTakesFloodLimitsFromThePanel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"users":[],"guard":{"rate":5,"burst":50,"max_open":3000,"blocked":["192.0.2.0/24","198.51.100.7","мусор"]}}`))
	}))
	defer srv.Close()
	g := &fakeGuard{}
	c := New(srv.URL, "t")
	c.Guard(g)
	if _, err := c.FetchUsers(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(g.got) != 1 {
		t.Fatalf("пределы применены %d раз", len(g.got))
	}
	lim := g.got[0]
	if lim.Rate != 5 || lim.Burst != 50 || lim.MaxOpen != 3000 {
		t.Fatalf("пределы: %+v", lim)
	}
	want := []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24"), netip.MustParsePrefix("198.51.100.7/32")}
	if len(lim.Blocked) != len(want) || lim.Blocked[0] != want[0] || lim.Blocked[1] != want[1] {
		t.Fatalf("закрытые адреса: %v", lim.Blocked)
	}
}

// Панель постарше пределов не шлёт — нода не трогает свои, а не сбрасывает
// их в ноль.
func TestOldPanelLeavesNodeLimitsAlone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"users":[]}`))
	}))
	defer srv.Close()
	g := &fakeGuard{}
	c := New(srv.URL, "t")
	c.Guard(g)
	if _, err := c.FetchUsers(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(g.got) != 0 {
		t.Fatalf("без пределов от панели нода получила %+v", g.got)
	}
}

// Нода сообщает панели, сколько отбила, — числами, без единого адреса.
func TestNodeReportsRejectedCountsWithoutAddresses(t *testing.T) {
	var raw string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&body)
		raw = string(body["guard"])
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	g := &fakeGuard{stats: connguard.Stats{ByRate: 120, ByOpen: 3, ByBlock: 40}}
	c := New(srv.URL, "t").WithBuild("v0.13.0", "amd64")
	c.Guard(g)
	if err := c.ReportUsage(context.Background(), map[string]users.Usage{}, nil); err != nil {
		t.Fatal(err)
	}
	var got connguard.Stats
	if err := json.Unmarshal([]byte(raw), &got); err != nil || got != (connguard.Stats{ByRate: 120, ByOpen: 3, ByBlock: 40}) {
		t.Fatalf("панель получила %q", raw)
	}
	var fields map[string]any
	_ = json.Unmarshal([]byte(raw), &fields)
	for k, v := range fields {
		if _, isNumber := v.(float64); !isNumber {
			t.Fatalf("в отчёте не число: %s = %v", k, v)
		}
	}
}

// Память из /proc/meminfo — в байтах, а не в килобайтах, как она там
// записана: иначе панель показала бы сервер в тысячу раз меньше.
func TestMeminfoIsReadInBytes(t *testing.T) {
	total, avail := parseMeminfo([]byte("MemTotal:        2014320 kB\nMemFree:          112204 kB\nMemAvailable:    1203640 kB\n"))
	if total != 2014320*1024 || avail != 1203640*1024 {
		t.Fatalf("память: всего %d, свободно %d", total, avail)
	}
}
