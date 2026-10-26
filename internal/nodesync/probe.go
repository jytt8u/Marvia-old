package nodesync

import (
	"context"
	"net"
	"sync"
	"time"
)

// Проверка выхода из страны покупателя.
//
// Вход каскада стоит там же, где покупатели, — в российской сети. Если
// адрес выхода заблокировали, первым это видит именно он, раньше любого
// покупателя. Поэтому вход по расписанию стучится до своего выхода и
// сообщает панели, дошёл ли и за сколько. Это проверка без клиентов: ни
// ключей, ни трафика покупателей в ней нет — только TCP до адреса, который
// вход и так знает (AGENTS.md, правило 5).
//
// Чего она не видит: замедления. Блокировка по адресу рвёт соединение, а
// дросселирование пропускает рукопожатие и душит данные, — это видно уже
// по отчётам приложений (fetch_ms).

// Probe — последняя проверка выхода.
type Probe struct {
	OK    bool  `json:"ok"`
	TCPMS int64 `json:"tcp_ms"`
}

// probeTimeout — сколько ждать соединения. Пять секунд: живой выход
// отвечает за десятки миллисекунд, а блокировка — тишиной.
const probeTimeout = 5 * time.Second

type probeState struct {
	mu   sync.Mutex
	last *Probe
}

// ProbeExit проверяет выход каждые every, пока не отменён ctx. Только у
// входа: обычной ноде проверять некого.
func (c *Client) ProbeExit(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if to := c.RelayTo(); to != "" {
			p := probeTCP(ctx, to)
			c.probe.mu.Lock()
			c.probe.last = &p
			c.probe.mu.Unlock()
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func probeTCP(ctx context.Context, addr string) Probe {
	start := time.Now()
	d := net.Dialer{Timeout: probeTimeout}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return Probe{}
	}
	_ = conn.Close()
	return Probe{OK: true, TCPMS: time.Since(start).Milliseconds()}
}

// lastProbe — последняя проверка для отчёта; nil — ещё не проверяли.
func (c *Client) lastProbe() *Probe {
	c.probe.mu.Lock()
	defer c.probe.mu.Unlock()
	return c.probe.last
}
