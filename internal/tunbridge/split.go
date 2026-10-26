package tunbridge

import (
	"context"
	"net"
	"net/netip"

	"github.com/jytt8u/marvia/internal/vp1"
)

// Split — дозвон, который часть адресов ведёт мимо туннеля.
//
// На Android мимо туннеля адреса уводит система: VpnService умеет исключать
// маршруты, и до моста такие пакеты просто не доходят. На Windows так не
// выйдет без правки таблицы маршрутов всей машины — восемь тысяч записей,
// которые переживут падение программы и сломаются при первой смене Wi-Fi.
// Поэтому там все пакеты приходят в мост, а решение принимается здесь, на
// каждое соединение: адрес, для которого direct ответил true, получает
// соединение через local — обычную сеть компьютера, — остальные идут в
// туннель.
//
// Цена: обходящий трафик проходит через стек моста в пространстве
// пользователя, а не мимо него. Для сайтов банков и госуслуг это не заметно,
// а выигрыш — ни одного следа в системе после выхода.
//
// direct спрашивается на каждое соединение, поэтому его можно менять на ходу:
// включённый человеком обход действует на новые соединения сразу, без
// переподключения.
//
// sites — правила по именам сайтов (см. sites.go); nil — только по адресам.
// Правило сильнее адреса: «через туннель» оставляет в нём и сайт из
// российского списка, «мимо» уводит и заграничный.
func Split(tunnel, local Dialer, direct func(netip.Addr) bool, sites *Sites) Dialer {
	return &splitDialer{tunnel: tunnel, local: local, direct: direct, sites: sites}
}

type splitDialer struct {
	tunnel, local Dialer
	direct        func(netip.Addr) bool
	sites         *Sites
	apps          AppRoute // правила по программам; см. apps.go
}

// siteRules отдаёт мосту правила: по ним он решает, ждать ли приветствия, и
// в их память складывает ответы на запросы имён. Мост берёт их у развилки, а
// не из своих настроек, чтобы не было двух источников правды: правила без
// развилки ничего не решали бы, а мост ждал бы приветствий зря.
func (d *splitDialer) siteRules() *Sites { return d.sites }

func (d *splitDialer) DialTarget(ctx context.Context, target vp1.Address) (net.Conn, error) {
	if d.around(ctx, target) {
		return d.local.DialTarget(ctx, target)
	}
	return d.tunnel.DialTarget(ctx, target)
}

func (d *splitDialer) DialDatagrams(ctx context.Context, target vp1.Address) (net.Conn, error) {
	if d.around(ctx, target) {
		return d.local.DialDatagrams(ctx, target)
	}
	return d.tunnel.DialDatagrams(ctx, target)
}

// around — идти ли к цели мимо туннеля.
//
// Запросы имён — никогда, даже к российскому резолверу: запрос имени в
// открытую выдаёт цензору весь список посещённых сайтов, и ради этого
// обещания мост и существует (см. Config.DNS). Человек, выбравший резолвер
// Яндекса, получит его ответы — но через туннель.
func (d *splitDialer) around(ctx context.Context, target vp1.Address) bool {
	if target.Port == dnsPort {
		return false
	}
	if src, ok := sourceOf(ctx); ok && d.apps != nil {
		switch d.apps(src.port, src.udp) {
		case RouteDirect:
			return true
		case RouteTunnel:
			return false
		}
	}
	ip, err := netip.ParseAddr(target.Host)
	if err != nil {
		// Имя, а не адрес: так в мост цели не приходят, но решать по имени
		// здесь нечем — пусть идёт в туннель, как всё неизвестное.
		return false
	}
	ip = ip.Unmap()
	switch d.sites.Route(siteOf(ctx), ip) {
	case RouteDirect:
		return true
	case RouteTunnel:
		return false
	}
	return d.direct != nil && d.direct(ip)
}

// DialerFunc собирает Dialer из двух функций — для обычной сети компьютера,
// которую мосту даёт тот, кто знает, как к ней привязаться.
type DialerFunc struct {
	Target    func(ctx context.Context, target vp1.Address) (net.Conn, error)
	Datagrams func(ctx context.Context, target vp1.Address) (net.Conn, error)
}

func (f DialerFunc) DialTarget(ctx context.Context, target vp1.Address) (net.Conn, error) {
	return f.Target(ctx, target)
}

func (f DialerFunc) DialDatagrams(ctx context.Context, target vp1.Address) (net.Conn, error) {
	return f.Datagrams(ctx, target)
}
