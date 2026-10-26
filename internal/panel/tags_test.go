package panel_test

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// Метки сохраняются без повторов и пробелов, а запятая внутри метки не
// превращает одну метку в две.
func TestTagsAreCleanAndCommaIsRefused(t *testing.T) {
	h := newHarness(t)
	created := h.createUser(0)
	path := "/api/v1/users/" + itoa(created.User.ID)
	h.do(http.MethodPatch, path, adminToken, map[string]any{"tags": []string{" опт ", "Семья", "ОПТ", ""}}, nil)
	if got := h.getUser(created.User.ID).Tags; len(got) != 2 || got[0] != "опт" || got[1] != "Семья" {
		t.Fatalf("метки: %q", got)
	}
	if code := h.do(http.MethodPatch, path, adminToken, map[string]any{"tags": []string{"опт,семья"}}, nil); code != http.StatusBadRequest {
		t.Fatalf("метка с запятой принята: код %d", code)
	}
	h.do(http.MethodPatch, path, adminToken, map[string]any{"tags": []string{}}, nil)
	if got := h.getUser(created.User.ID).Tags; len(got) != 0 {
		t.Fatalf("пустой список не снял метки: %q", got)
	}
}

// Выгрузка открывается в русском Excel по столбцам и без кракозябр, а имя,
// похожее на формулу, формулой не становится.
func TestClientExportOpensInExcelAndRunsNoFormulas(t *testing.T) {
	h := newHarness(t)
	created := h.createUser(0)
	h.do(http.MethodPatch, "/api/v1/users/"+itoa(created.User.ID), adminToken, map[string]any{"label": "=HYPERLINK(\"x\")", "tags": []string{"опт"}}, nil)

	req, _ := http.NewRequest(http.MethodGet, h.server.URL+"/api/v1/users/export.csv", nil)
	req.Header.Set("Authorization", "Bearer "+adminToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	body := string(raw)
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(body, "\xef\xbb\xbf") {
		t.Fatalf("код %d, начало %q", resp.StatusCode, body[:min(len(body), 10)])
	}
	if !strings.Contains(body, "номер;имя;метки") || !strings.Contains(body, ";опт;") {
		t.Fatalf("столбцы не через точку с запятой: %q", body)
	}
	if !strings.Contains(body, "'=HYPERLINK") {
		t.Fatalf("имя-формула не обезврежено: %q", body)
	}
}

// Выгрузка — вся база клиентов разом: боту её не отдаём.
func TestBotCannotExportClients(t *testing.T) {
	h := newHarness(t)
	var key struct {
		Secret string `json:"secret"`
	}
	h.do(http.MethodPost, "/api/v1/keys", adminToken, map[string]any{"name": "бот", "scopes": []string{"users", "read"}}, &key)
	req, _ := http.NewRequest(http.MethodGet, h.server.URL+"/api/v1/users/export.csv", nil)
	req.Header.Set("Authorization", "Bearer "+key.Secret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("бот выгрузил клиентов: код %d", resp.StatusCode)
	}
}
