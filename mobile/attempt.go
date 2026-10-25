package mobile

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/jytt8u/marvia/internal/client"
	"github.com/jytt8u/marvia/internal/tunbridge"
)

// ConnectionAttempt отменяет дозвон из Android, где отмена корутины не
// прерывает вызов JNI. До успешного Connect приложение не создаёт TUN:
// медленная панель тогда не перехватывает весь интернет телефона.
type ConnectionAttempt struct {
	mu     sync.Mutex
	ctx    context.Context
	cancel context.CancelFunc
	used   bool
	tunnel *Tunnel
}

func NewConnectionAttempt() *ConnectionAttempt {
	ctx, cancel := context.WithCancel(context.Background())
	return &ConnectionAttempt{ctx: ctx, cancel: cancel}
}

// Cancel не ждёт закрытия сетевого ядра: только отменяет дозвон и освобождает
// уже присоединённый интерфейс. Владельцу результата остаётся вызвать Stop.
func (a *ConnectionAttempt) Cancel() {
	a.cancel()
	a.mu.Lock()
	t := a.tunnel
	a.mu.Unlock()
	if t != nil {
		t.Disconnect()
	}
}

// Connect используется один раз, чтобы поздний результат отменённой попытки
// нельзя было принять за новое подключение на том же объекте.
func (a *ConnectionAttempt) Connect(accountLink, cacheDir string, prefer int64, settings string) (*Tunnel, error) {
	a.mu.Lock()
	if err := a.ctx.Err(); err != nil {
		a.mu.Unlock()
		return nil, err
	}
	if a.used {
		a.mu.Unlock()
		return nil, errors.New("попытка подключения уже использована")
	}
	a.used = true
	a.mu.Unlock()
	t := &Tunnel{running: true, settings: parseSettings(settings)}
	dialer, err := connectContext(a.ctx, accountLink, cacheDir, prefer, client.Events{OnSwitch: t.switched, OnTrouble: t.trouble, OnRecovered: t.recovered}, t.settings)
	if err != nil {
		if canceled := a.ctx.Err(); canceled != nil {
			err = canceled
		}
		a.cancel()
		return nil, err
	}
	name := dialer.Node().Title()
	t.mu.Lock()
	t.dialer = dialer
	t.nodeName = name
	t.mu.Unlock()
	a.mu.Lock()
	err = a.ctx.Err()
	if err == nil {
		a.tunnel = t
	}
	a.mu.Unlock()
	if err != nil {
		_ = t.Stop()
		return nil, err
	}
	return t, nil
}

// Attach принимает дескриптор насовсем, даже при ошибке. Только локальный
// стек создаётся под замком: ожидание сети уже закончилось в Connect, а
// Disconnect не должен разминуться с появлением нового интерфейса.
func (t *Tunnel) Attach(tunFD int, dns string) error {
	if tunFD <= 0 {
		return fail(FailSystem, errors.New("не передан дескриптор сетевого интерфейса"))
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.running || t.dialer == nil || t.bridge != nil {
		tunbridge.CloseFD(tunFD)
		return fail(FailSystem, errors.New("туннель остановлен или интерфейс уже присоединён"))
	}
	if strings.TrimSpace(dns) == "" {
		dns = DefaultDNS
	}
	tunnel := tunbridge.Metered(t.dialer, &t.up, &t.down)
	bridge, err := tunbridge.Start(tunbridge.Config{FD: tunFD, Dialer: tunnel, DNS: dns, Names: t.settings.names(dns, tunnel), NoIPv6: t.settings.NoIPv6, OnError: t.note})
	if err != nil {
		return fail(FailSystem, fmt.Errorf("сетевой мост: %w", err))
	}
	t.bridge = bridge
	t.dns = dns
	return nil
}
