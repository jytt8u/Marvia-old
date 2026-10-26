package panel_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/jytt8u/marvia/internal/panel"
)

// Вход пишется в журнал — кем, из какого браузера и откуда, — а
// перезагрузка страницы (/me) входом не считается.
func TestPanelSignInIsJournaledButReloadIsNot(t *testing.T) {
	h := newHarness(t)
	req, _ := http.NewRequest(http.MethodPost, h.server.URL+"/api/v1/login", nil)
	req.Header.Set("Authorization", "Bearer "+adminToken)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/130.0 Safari/537.36")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	h.do(http.MethodGet, "/api/v1/me", adminToken, nil, nil)

	var out struct {
		Events []panel.Event `json:"events"`
	}
	h.do(http.MethodGet, "/api/v1/events?limit=50", adminToken, nil, &out)
	logins := 0
	for _, e := range out.Events {
		if e.Action == panel.EventPanelLogin {
			logins++
			if e.Actor != panel.ActorAdmin || !strings.Contains(e.Detail, "Chrome, Windows") || !strings.Contains(e.Detail, "127.0.0.1") {
				t.Fatalf("запись о входе: %+v", e)
			}
		}
	}
	if logins != 1 {
		t.Fatalf("входов в журнале %d, ждали 1", logins)
	}
	if code := h.do(http.MethodPost, "/api/v1/login", "не-тот-токен-0000000000", nil, nil); code != http.StatusUnauthorized {
		t.Fatalf("вход чужим токеном: код %d", code)
	}
}

// Помощник с ключом «только чтение» узнаёт, что он только смотрит, и
// сервер не даёт ему ничего поменять, что бы ни нарисовала страница.
func TestViewOnlyHelperSeesButCannotChange(t *testing.T) {
	h := newHarness(t)
	created := h.createUser(0)
	var key struct {
		Secret string `json:"secret"`
	}
	h.do(http.MethodPost, "/api/v1/keys", adminToken, map[string]any{"name": "помощник", "scopes": []string{"read"}}, &key)
	var me struct {
		Me panel.Me `json:"me"`
	}
	if code := h.do(http.MethodGet, "/api/v1/me", key.Secret, nil, &me); code != http.StatusOK || me.Me.Admin || me.Me.Name != "помощник" {
		t.Fatalf("me: код %d, %+v", code, me.Me)
	}
	if code := h.do(http.MethodGet, "/api/v1/users", key.Secret, nil, nil); code != http.StatusOK {
		t.Fatalf("помощник не видит клиентов: %d", code)
	}
	if code := h.do(http.MethodPatch, "/api/v1/users/"+itoa(created.User.ID), key.Secret, map[string]any{"label": "x"}, nil); code != http.StatusForbidden {
		t.Fatalf("помощник поменял клиента: %d", code)
	}
}
