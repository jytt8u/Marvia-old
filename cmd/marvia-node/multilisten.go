package main

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
)

// Запасные порты.
//
// Блокируют не всегда весь адрес: бывает, что режут один порт, а соседние
// живы. Нода слушает ещё и на запасных портах (-alt-listen 2053,8880) тем
// же конвейером — с теми же пределами, маскировкой и ключами, — а панель,
// увидев, что основной порт перестал проходить, переводит подписки на
// запасной (internal/panel/ports.go). Покупатели переезжают со следующим
// обновлением подписки, а не ждут, пока продавец заметит и переставит ноду.

// parseAltPorts разбирает «2053,8880». Порт основного слушателя и повторы
// отбрасываются.
func parseAltPorts(raw, mainAddr string) ([]int, error) {
	_, mainPortText, _ := net.SplitHostPort(mainAddr)
	mainPort, _ := strconv.Atoi(mainPortText)
	var out []int
	seen := map[int]bool{mainPort: true}
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		port, err := strconv.Atoi(part)
		if err != nil || port < 1 || port > 65535 {
			return nil, fmt.Errorf("запасной порт %q: нужно число от 1 до 65535", part)
		}
		if !seen[port] {
			seen[port] = true
			out = append(out, port)
		}
	}
	if len(out) > 8 {
		return nil, errors.New("запасных портов не больше восьми")
	}
	return out, nil
}

// multiListener сводит несколько слушателей в один: Accept отдаёт
// соединение с любого. Адрес — основного, чтобы журнал и всё, что
// спрашивает Addr, видели прежнее.
type multiListener struct {
	main  net.Listener
	all   []net.Listener
	conns chan acceptResult
	done  chan struct{}
	once  sync.Once
}

type acceptResult struct {
	conn net.Conn
	err  error
}

func listenMulti(main net.Listener, extra []net.Listener) net.Listener {
	if len(extra) == 0 {
		return main
	}
	m := &multiListener{main: main, all: append([]net.Listener{main}, extra...), conns: make(chan acceptResult), done: make(chan struct{})}
	for _, l := range m.all {
		go m.pump(l)
	}
	return m
}

func (m *multiListener) pump(l net.Listener) {
	for {
		c, err := l.Accept()
		if err != nil && errors.Is(err, net.ErrClosed) {
			return
		}
		select {
		case m.conns <- acceptResult{c, err}:
		case <-m.done:
			if c != nil {
				_ = c.Close()
			}
			return
		}
	}
}

func (m *multiListener) Accept() (net.Conn, error) {
	select {
	case r := <-m.conns:
		return r.conn, r.err
	case <-m.done:
		return nil, net.ErrClosed
	}
}

func (m *multiListener) Close() error {
	var err error
	m.once.Do(func() {
		close(m.done)
		for _, l := range m.all {
			err = errors.Join(err, l.Close())
		}
	})
	return err
}

func (m *multiListener) Addr() net.Addr { return m.main.Addr() }
