package panel_test

import (
	"net/http"
	"strings"
	"testing"
)

type statusPageResponse struct {
	Enabled bool   `json:"enabled"`
	URL     string `json:"url"`
}

// statusPath — путь страницы из адреса, который отдала панель.
func statusPath(t *testing.T, url string) string {
	t.Helper()
	i := strings.Index(url, "/status/")
	if i < 0 {
		t.Fatalf("у страницы нет адреса: %q", url)
	}
	return url[i:]
}

func (h *harness) code(path string) int {
	h.t.Helper()
	resp, err := http.Get(h.server.URL + path)
	if err != nil {
		h.t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// TestStatusPageIsOffUntilTheSellerTurnsItOn: пока продавец не включил,
// панель не отвечает страницей ни по какому пути — как будто её нет.
func TestStatusPageIsOffUntilTheSellerTurnsItOn(t *testing.T) {
	h := newHarness(t)
	var got statusPageResponse
	if code := h.do(http.MethodGet, "/api/v1/status-page", adminToken, nil, &got); code != http.StatusOK || got.Enabled || got.URL != "" {
		t.Fatalf("по умолчанию: код %d, %+v", code, got)
	}
	if code := h.code("/status/anything"); code != http.StatusNotFound {
		t.Fatalf("выключенная страница ответила %d", code)
	}
}

// TestStatusPageShowsWhichNodesWorkButNotWhereTheyAre: живая нода —
// «работает», молчащая — «не отвечает»; адресов нод на странице нет.
func TestStatusPageShowsWhichNodesWorkButNotWhereTheyAre(t *testing.T) {
	h := newHarness(t)
	alive := h.createNode("helsinki")
	h.createNode("amsterdam")
	h.sync(alive.Token)

	var got statusPageResponse
	if code := h.do(http.MethodPut, "/api/v1/status-page", adminToken, map[string]any{"enabled": true}, &got); code != http.StatusOK || !got.Enabled {
		t.Fatalf("включение: код %d, %+v", code, got)
	}
	if !strings.HasPrefix(got.URL, "https://sub.example.com/status/") {
		t.Fatalf("адрес страницы %q не от адреса панели", got.URL)
	}
	page := h.fetch(statusPath(t, got.URL))

	row := func(name string) string {
		i := strings.Index(page, name)
		if i < 0 {
			t.Fatalf("ноды %s нет на странице", name)
		}
		end := strings.Index(page[i:], "</li>")
		return page[i : i+end]
	}
	if r := row("helsinki"); !strings.Contains(r, "работает") {
		t.Errorf("живая нода показана так: %s", r)
	}
	if r := row("amsterdam"); !strings.Contains(r, "не отвечает") {
		t.Errorf("молчащая нода показана так: %s", r)
	}
	if strings.Contains(page, ".example.com") {
		t.Error("на открытой странице адрес ноды")
	}
}

// TestNewStatusLinkRevokesTheOldOne: новая ссылка нужна, когда старую
// переслали не туда, — значит, старая перестаёт открываться.
func TestNewStatusLinkRevokesTheOldOne(t *testing.T) {
	h := newHarness(t)
	var first, second statusPageResponse
	h.do(http.MethodPut, "/api/v1/status-page", adminToken, map[string]any{"enabled": true}, &first)
	h.do(http.MethodPut, "/api/v1/status-page", adminToken, map[string]any{"enabled": true, "new_link": true}, &second)
	if first.URL == second.URL {
		t.Fatal("ссылка не сменилась")
	}
	if code := h.code(statusPath(t, first.URL)); code != http.StatusNotFound {
		t.Errorf("старая ссылка открывается: %d", code)
	}
	if code := h.code(statusPath(t, second.URL)); code != http.StatusOK {
		t.Errorf("новая ссылка не открывается: %d", code)
	}

	// Выключили — не открывается и новая.
	h.do(http.MethodPut, "/api/v1/status-page", adminToken, map[string]any{"enabled": false}, nil)
	if code := h.code(statusPath(t, second.URL)); code != http.StatusNotFound {
		t.Errorf("выключенная страница открывается: %d", code)
	}
}
