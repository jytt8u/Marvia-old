package panel_test

import (
	"net/http"
	"testing"
	"time"
)

// Ноды обновляются по одной: следующая получает просьбу, только когда
// предыдущая сообщила новую версию. Сломанная сборка не кладёт весь парк.
func TestNodesUpgradeOneAtATime(t *testing.T) {
	h := newHarness(t)
	a, b, c := h.createNode("a"), h.createNode("b"), h.createNode("c")
	report := func(token, version string) {
		t.Helper()
		h.do(http.MethodPost, "/api/v1/node/usage", token, map[string]any{"usage": map[string]any{}, "version": version, "arch": "amd64"}, nil)
	}
	for _, n := range []createNodeResponse{a, b, c} {
		report(n.Token, "v0.11.0")
	}
	defer h.fakeRelease("v0.12.0")()
	var asked struct {
		Asked int `json:"asked"`
	}
	h.do(http.MethodPost, "/api/v1/updates/nodes", adminToken, nil, &asked)
	if asked.Asked != 3 {
		t.Fatalf("в очереди %d нод, ждали 3", asked.Asked)
	}
	upgradeTo := func(n createNodeResponse) string {
		var out struct {
			UpgradeTo string `json:"upgrade_to"`
		}
		h.do(http.MethodGet, "/api/v1/node/users", n.Token, nil, &out)
		return out.UpgradeTo
	}
	asking := func() int {
		count := 0
		for _, n := range []createNodeResponse{a, b, c} {
			if upgradeTo(n) != "" {
				count++
			}
		}
		return count
	}
	if got := asking(); got != 1 {
		t.Fatalf("просьбу получили %d нод разом, ждали одну", got)
	}
	report(a.Token, "v0.12.0")
	if upgradeTo(a) != "" || upgradeTo(b) != "v0.12.0" || upgradeTo(c) != "" {
		t.Fatalf("после первой: a=%q b=%q c=%q", upgradeTo(a), upgradeTo(b), upgradeTo(c))
	}
	report(b.Token, "v0.12.0")
	report(c.Token, "v0.12.0")
	if got := asking(); got != 0 {
		t.Fatalf("после всех просьбы остались у %d", got)
	}
}

// Ночью — без потолка скорости, днём — как в тарифе.
func TestNightLiftsTheSpeedCap(t *testing.T) {
	h := newHarness(t)
	node := h.createNode("fi-1")
	created := h.createUser(0)
	h.do(http.MethodPatch, "/api/v1/users/"+itoa(created.User.ID), adminToken, map[string]any{"speed_limit": 6250000}, nil)
	msk := time.FixedZone("MSK", 3*60*60)
	hour := time.Now().In(msk).Hour()
	h.do(http.MethodPut, "/api/v1/night-speed", adminToken, map[string]any{"enabled": true, "from": hour, "until": (hour + 1) % 24}, nil)
	if got := h.nodeUsers(node.Token)[0].SpeedLimit; got != 0 {
		t.Fatalf("в ночном окне потолок %d", got)
	}
	h.do(http.MethodPut, "/api/v1/night-speed", adminToken, map[string]any{"enabled": true, "from": (hour + 2) % 24, "until": (hour + 3) % 24}, nil)
	if got := h.nodeUsers(node.Token)[0].SpeedLimit; got != 6250000 {
		t.Fatalf("вне окна потолок %d", got)
	}
	if code := h.do(http.MethodPut, "/api/v1/night-speed", adminToken, map[string]any{"enabled": true, "from": 3, "until": 3}, nil); code != http.StatusBadRequest {
		t.Fatalf("пустое окно принято: %d", code)
	}
}

// Нагрузка машины ноды доходит до панели и видна продавцу.
func TestNodeLoadReachesThePanel(t *testing.T) {
	h := newHarness(t)
	node := h.createNode("fi-1")
	h.do(http.MethodPost, "/api/v1/node/usage", node.Token, map[string]any{
		"usage": map[string]any{}, "version": "v0.13.0", "arch": "amd64",
		"sys": map[string]any{"load": 1.5, "cpus": 2, "mem_total": 2 << 30, "mem_avail": 1 << 29},
	}, nil)
	var out struct {
		Nodes []struct {
			Sys *struct {
				Load float64 `json:"load"`
				CPUs int     `json:"cpus"`
			} `json:"sys"`
		} `json:"nodes"`
	}
	h.do(http.MethodGet, "/api/v1/nodes", adminToken, nil, &out)
	if len(out.Nodes) != 1 || out.Nodes[0].Sys == nil || out.Nodes[0].Sys.Load != 1.5 || out.Nodes[0].Sys.CPUs != 2 {
		t.Fatalf("нагрузка: %+v", out.Nodes)
	}
}

// Вход сообщает о своём выходе; неудачи считаются подряд, а успех их
// обнуляет — одна моргнувшая сеть не превращается в «заблокирован».
func TestEntryProbeFailuresCountInARow(t *testing.T) {
	h := newHarness(t)
	entry := h.createNode("ru-entry")
	send := func(ok bool) {
		h.do(http.MethodPost, "/api/v1/node/usage", entry.Token, map[string]any{
			"usage": map[string]any{}, "version": "v0.13.0", "probe": map[string]any{"ok": ok, "tcp_ms": 40},
		}, nil)
	}
	probe := func() (bool, int) {
		var out struct {
			Nodes []struct {
				Probe *struct {
					OK    bool `json:"ok"`
					Fails int  `json:"fails"`
				} `json:"probe"`
			} `json:"nodes"`
		}
		h.do(http.MethodGet, "/api/v1/nodes", adminToken, nil, &out)
		if out.Nodes[0].Probe == nil {
			t.Fatal("проверки нет")
		}
		return out.Nodes[0].Probe.OK, out.Nodes[0].Probe.Fails
	}
	send(false)
	send(false)
	if ok, fails := probe(); ok || fails != 2 {
		t.Fatalf("после двух неудач: ok=%v, подряд %d", ok, fails)
	}
	send(true)
	if ok, fails := probe(); !ok || fails != 0 {
		t.Fatalf("после успеха: ok=%v, подряд %d", ok, fails)
	}
}
