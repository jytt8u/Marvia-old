package tunbridge_test

import (
	"context"
	"net"
	"testing"

	"github.com/jytt8u/marvia/internal/tunbridge"
	"github.com/jytt8u/marvia/internal/vp1"
)

type markDialer struct{ name string }

func (d markDialer) DialTarget(context.Context, vp1.Address) (net.Conn, error) {
	return nil, &net.OpError{Op: d.name}
}
func (d markDialer) DialDatagrams(context.Context, vp1.Address) (net.Conn, error) {
	return nil, &net.OpError{Op: d.name}
}

func pathOf(err error) string {
	if op, ok := err.(*net.OpError); ok {
		return op.Op
	}
	return "?"
}

// Правило программы сильнее российского списка: «браузер только через VPN»
// уводит в туннель и российский сайт, а «игра мимо» — и заграничный.
func TestAppRuleBeatsTheAddressRule(t *testing.T) {
	d := tunbridge.Split(markDialer{"tunnel"}, markDialer{"local"}, nil, nil)
	d = tunbridge.WithApps(d, func(port uint16, udp bool) tunbridge.Route {
		switch port {
		case 1111:
			return tunbridge.RouteDirect
		case 2222:
			return tunbridge.RouteTunnel
		}
		return tunbridge.RouteAny
	})
	target := vp1.Address{Type: vp1.AtypIPv4, Host: "93.184.216.34", Port: 443}
	if _, err := d.DialTarget(tunbridge.WithSourceForTest(1111, false), target); pathOf(err) != "local" {
		t.Fatalf("программа «мимо» пошла %s", pathOf(err))
	}
	if _, err := d.DialTarget(tunbridge.WithSourceForTest(2222, false), target); pathOf(err) != "tunnel" {
		t.Fatalf("программа «через VPN» пошла %s", pathOf(err))
	}
	if _, err := d.DialDatagrams(tunbridge.WithSourceForTest(1111, true), target); pathOf(err) != "local" {
		t.Fatalf("датаграммы программы «мимо» пошли %s", pathOf(err))
	}
	// Запросы имён — всегда через туннель, что бы ни сказала программа.
	dns := vp1.Address{Type: vp1.AtypIPv4, Host: "1.1.1.1", Port: 53}
	if _, err := d.DialDatagrams(tunbridge.WithSourceForTest(1111, true), dns); pathOf(err) != "tunnel" {
		t.Fatalf("запрос имени программы «мимо» пошёл %s", pathOf(err))
	}
}
