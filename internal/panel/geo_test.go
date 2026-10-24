package panel_test

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/jytt8u/marvia/internal/panel"
)

// TestNodeWithoutCountryGetsItFromItsAddress: нода, которой страну не
// назвали, получает её по адресу сама — покупатель видит «Финляндия» с
// флагом, а не голое имя машины. Написанное продавцом не трогается.
func TestNodeWithoutCountryGetsItFromItsAddress(t *testing.T) {
	defer panel.SetGeoLookup(func(context.Context, string) (string, error) { return "fi", nil })()
	h := newHarness(t)
	bare := h.createNode("bare")
	named := h.createNode("named")
	h.do(http.MethodPatch, "/api/v1/nodes/"+itoa(named.Node.ID), adminToken, map[string]any{"country": "Нидерланды"}, nil)

	h.nodeUsers(bare.Token)
	h.do(http.MethodPost, "/api/v1/node/usage", bare.Token, map[string]any{"usage": map[string]any{}}, nil)
	h.do(http.MethodPost, "/api/v1/node/usage", named.Token, map[string]any{"usage": map[string]any{}}, nil)

	country := func(id int64) string {
		var out struct {
			Nodes []panel.Node `json:"nodes"`
		}
		h.do(http.MethodGet, "/api/v1/nodes", adminToken, nil, &out)
		for _, n := range out.Nodes {
			if n.ID == id {
				return n.Country
			}
		}
		return ""
	}
	deadline := time.Now().Add(3 * time.Second)
	for country(bare.Node.ID) == "" && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := country(bare.Node.ID); got != "Финляндия" {
		t.Fatalf("страна по адресу: %q", got)
	}
	if got := country(named.Node.ID); got != "Нидерланды" {
		t.Fatalf("страну продавца перезаписали: %q", got)
	}

	var geo struct {
		Country string `json:"country"`
	}
	if code := h.do(http.MethodGet, "/api/v1/nodes/"+itoa(named.Node.ID)+"/geo", adminToken, nil, &geo); code != http.StatusOK || geo.Country != "Финляндия" {
		t.Fatalf("кнопка «По IP»: код %d, %q", code, geo.Country)
	}
}

// TestCountryCodeBecomesItsName: страна, записанная кодом, уходит
// покупателю названием — «Финляндия · fi-1», а не «FI · fi-1».
func TestCountryCodeBecomesItsName(t *testing.T) {
	h := newHarness(t)
	n := h.createNode("fi-1")
	var out struct {
		Node panel.Node `json:"node"`
	}
	h.do(http.MethodPatch, "/api/v1/nodes/"+itoa(n.Node.ID), adminToken, map[string]any{"country": "FI"}, &out)
	if out.Node.Country != "Финляндия" {
		t.Fatalf("код страны остался: %q", out.Node.Country)
	}
	h.do(http.MethodPatch, "/api/v1/nodes/"+itoa(n.Node.ID), adminToken, map[string]any{"country": "Атлантида"}, &out)
	if out.Node.Country != "Атлантида" {
		t.Fatalf("незнакомую страну переписали: %q", out.Node.Country)
	}
}

// TestCountryCodesWrittenBeforeAreNamedOnRestart: ноды, которым страну
// раньше записали кодом, после перезапуска панели называются по-людски.
func TestCountryCodesWrittenBeforeAreNamedOnRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "panel.db")
	store, err := panel.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	node, _, err := store.CreateNode(context.Background(), panel.CreateNodeParams{Name: "ae-1", Address: "203.0.113.1:443"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Exec(context.Background(), `UPDATE nodes SET country = 'AE' WHERE id = ?`, node.ID); err != nil {
		t.Fatal(err)
	}
	_ = store.Close()

	store, err = panel.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	got, err := store.GetNode(context.Background(), node.ID)
	if err != nil || got.Country != "ОАЭ" {
		t.Fatalf("после перезапуска страна %q, %v", got.Country, err)
	}
}

// TestUnrecognisedCountryIsReplacedByTheAddressCountry: слово, в котором
// страну не узнать («Пиюсн»), панель заменяет страной по адресу — иначе у
// ноды вместо флага «··». Узнаваемая страна, вписанная руками, остаётся.
func TestUnrecognisedCountryIsReplacedByTheAddressCountry(t *testing.T) {
	defer panel.SetGeoLookup(func(context.Context, string) (string, error) { return "nl", nil })()
	h := newHarness(t)
	weird := h.createNode("marvia-185")
	h.do(http.MethodPatch, "/api/v1/nodes/"+itoa(weird.Node.ID), adminToken, map[string]any{"country": "Пиюсн"}, nil)
	kept := h.createNode("fi-1")
	h.do(http.MethodPatch, "/api/v1/nodes/"+itoa(kept.Node.ID), adminToken, map[string]any{"country": "Финляндия"}, nil)
	h.do(http.MethodPost, "/api/v1/node/usage", weird.Token, map[string]any{"usage": map[string]any{}}, nil)
	h.do(http.MethodPost, "/api/v1/node/usage", kept.Token, map[string]any{"usage": map[string]any{}}, nil)

	country := func(id int64) string {
		var out struct {
			Nodes []panel.Node `json:"nodes"`
		}
		h.do(http.MethodGet, "/api/v1/nodes", adminToken, nil, &out)
		for _, n := range out.Nodes {
			if n.ID == id {
				return n.Country
			}
		}
		return ""
	}
	deadline := time.Now().Add(3 * time.Second)
	for country(weird.Node.ID) == "Пиюсн" && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := country(weird.Node.ID); got != "Нидерланды" {
		t.Fatalf("неузнаваемая страна осталась: %q", got)
	}
	if got := country(kept.Node.ID); got != "Финляндия" {
		t.Fatalf("страну продавца заменили: %q", got)
	}
}
