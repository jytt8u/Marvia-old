package client

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

type closeCounter struct {
	net.Conn
	closed *atomic.Int32
}

func (c closeCounter) Close() error { c.closed.Add(1); return nil }

func dialAfter(d time.Duration, closed *atomic.Int32, err error) func(context.Context) (net.Conn, error) {
	return func(ctx context.Context) (net.Conn, error) {
		select {
		case <-time.After(d):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if err != nil {
			return nil, err
		}
		a, _ := net.Pipe()
		return closeCounter{a, closed}, nil
	}
}

// Где TCP быстрее UDP, туннель едет по TCP, а не по формально рабочему,
// но медленному QUIC; где быстрее QUIC — по нему.
func TestFasterPathWinsTheRace(t *testing.T) {
	var closed atomic.Int32
	var viaUDP bool
	conn, err := race(context.Background(),
		dialAfter(200*time.Millisecond, &closed, nil), dialAfter(10*time.Millisecond, &closed, nil),
		func(u bool) { viaUDP = u })
	if err != nil || conn == nil || viaUDP {
		t.Fatalf("медленный QUIC против быстрого TCP: udp=%v, %v", viaUDP, err)
	}
	conn, err = race(context.Background(),
		dialAfter(10*time.Millisecond, &closed, nil), dialAfter(200*time.Millisecond, &closed, nil),
		func(u bool) { viaUDP = u })
	if err != nil || conn == nil || !viaUDP {
		t.Fatalf("быстрый QUIC против медленного TCP: udp=%v, %v", viaUDP, err)
	}
}

// Без UDP туннель встаёт по TCP; не встал никто — ошибка TCP, понятная
// человеку.
func TestRaceFallsBackAndReportsTheTCPError(t *testing.T) {
	var closed atomic.Int32
	udpErr, tcpErr := errors.New("udp молчит"), errors.New("tcp отказал")
	conn, err := race(context.Background(),
		dialAfter(time.Millisecond, &closed, udpErr), dialAfter(20*time.Millisecond, &closed, nil), func(bool) {})
	if err != nil || conn == nil {
		t.Fatalf("без UDP: %v", err)
	}
	if _, err := race(context.Background(),
		dialAfter(time.Millisecond, &closed, udpErr), dialAfter(time.Millisecond, &closed, tcpErr), func(bool) {}); !errors.Is(err, tcpErr) {
		t.Fatalf("ошибка: %v", err)
	}
}
