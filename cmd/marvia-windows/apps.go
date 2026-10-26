package main

import (
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"

	"github.com/jytt8u/marvia/internal/tunbridge"
)

// Правила по программам: эта — всегда через VPN, эта — всегда мимо.
//
// Программу называют именем файла, как в диспетчере задач: telegram.exe,
// steam.exe. Путь не нужен и не принимается: один и тот же Telegram стоит у
// людей в разных папках, а правило должно пережить переустановку.

const maxApps = 50

// errBadApp — имя программы не годится; текст — ключ словаря.
var errBadApp = errors.New("appBad")

// normalizeApps приводит имена: нижний регистр, без пути, с .exe на конце.
// Пустые и повторы отбрасываются.
func normalizeApps(in []string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, raw := range in {
		name := strings.ToLower(strings.TrimSpace(raw))
		if name == "" {
			continue
		}
		if strings.ContainsAny(name, `/\:*?"<>|`) || len(name) > 100 {
			return nil, errBadApp
		}
		if filepath.Ext(name) != ".exe" {
			name += ".exe"
		}
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	if len(out) > maxApps {
		return nil, errBadApp
	}
	return out, nil
}

// appRules — действующие правила; меняются на ходу, читаются на каждое
// соединение, поэтому под атомарным указателем, а не под замком.
type appRules struct {
	sets atomic.Pointer[appSets]
}

type appSets struct {
	direct, tunnel map[string]bool
}

// Set ставит списки. Пустые оба — правил нет, и мост даже не спрашивает
// систему, чья программа: таблица соединений на каждом соединении стоит
// денег, а тем, у кого правил нет, платить незачем.
func (r *appRules) Set(direct, tunnel []string) {
	if len(direct)+len(tunnel) == 0 {
		r.sets.Store(nil)
		return
	}
	s := &appSets{direct: map[string]bool{}, tunnel: map[string]bool{}}
	for _, n := range direct {
		s.direct[n] = true
	}
	for _, n := range tunnel {
		s.tunnel[n] = true
	}
	r.sets.Store(s)
}

func (r *appRules) active() bool { return r.sets.Load() != nil }

// route — куда вести соединение программы exe (имя файла).
func (r *appRules) route(exe string) tunbridge.Route {
	s := r.sets.Load()
	if s == nil || exe == "" {
		return tunbridge.RouteAny
	}
	exe = strings.ToLower(exe)
	switch {
	case s.tunnel[exe]:
		return tunbridge.RouteTunnel
	case s.direct[exe]:
		return tunbridge.RouteDirect
	}
	return tunbridge.RouteAny
}

// placeApps ставит списки программ из правки окна: как и сайт, программа
// живёт ровно в одном списке.
func (s *pcSettings) placeApps(direct, tunnel *[]string) error {
	next := *s
	if direct != nil {
		list, err := normalizeApps(*direct)
		if err != nil {
			return err
		}
		next.AppsDirect = list
		next.AppsTunnel = withoutSites(next.AppsTunnel, list)
	}
	if tunnel != nil {
		list, err := normalizeApps(*tunnel)
		if err != nil {
			return err
		}
		next.AppsTunnel = list
		next.AppsDirect = withoutSites(next.AppsDirect, list)
	}
	*s = next
	return nil
}
