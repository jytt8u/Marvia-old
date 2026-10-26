package tunbridge

import (
	"context"
	"net/netip"
	"time"
)

// Ручки для проверок снаружи пакета: имя сайта приходит к развилке от моста,
// а снаружи его подсунуть больше неоткуда.

func WithSiteForTest(name string) context.Context { return withSite(context.Background(), name) }

func LearnForTest(s *Sites, name string, ip netip.Addr) { s.learn(name, ip, time.Hour) }

func WithSourceForTest(port uint16, udp bool) context.Context {
	return withSource(context.Background(), port, udp)
}
