package main

import (
	"testing"

	"github.com/jytt8u/marvia/internal/tunbridge"
)

// Программу называют именем файла как угодно — «Telegram», «TELEGRAM.EXE»,
// — а путь не принимается: правило должно пережить переустановку в другую
// папку.
func TestAppNamesAreFileNamesWithoutPaths(t *testing.T) {
	got, err := normalizeApps([]string{" Telegram ", "TELEGRAM.EXE", "steam.exe", ""})
	if err != nil || len(got) != 2 || got[0] != "telegram.exe" || got[1] != "steam.exe" {
		t.Fatalf("имена: %v, %v", got, err)
	}
	if _, err := normalizeApps([]string{`C:\Program Files\x.exe`}); err == nil {
		t.Fatal("путь принят как имя программы")
	}
}

// «Через VPN» сильнее «мимо», если программа каким-то образом оказалась в
// обоих, а без правил мост даже не спрашивает систему.
func TestAppRulesRouteByFileName(t *testing.T) {
	var r appRules
	if r.active() {
		t.Fatal("без правил мост спрашивал бы систему о каждом соединении")
	}
	r.Set([]string{"game.exe", "both.exe"}, []string{"telegram.exe", "both.exe"})
	cases := map[string]tunbridge.Route{
		"Game.exe": tunbridge.RouteDirect, "telegram.exe": tunbridge.RouteTunnel,
		"both.exe": tunbridge.RouteTunnel, "other.exe": tunbridge.RouteAny,
	}
	for exe, want := range cases {
		if got := r.route(exe); got != want {
			t.Errorf("%s: %v, ждали %v", exe, got, want)
		}
	}
	var s pcSettings
	direct, tunnel := []string{"steam"}, []string{"steam.exe"}
	if err := s.placeApps(&direct, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.placeApps(nil, &tunnel); err != nil {
		t.Fatal(err)
	}
	if len(s.AppsDirect) != 0 || len(s.AppsTunnel) != 1 {
		t.Fatalf("программа в двух списках: %v / %v", s.AppsDirect, s.AppsTunnel)
	}
}
