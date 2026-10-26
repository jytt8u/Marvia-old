//go:build windows

package main

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"
	"time"

	"github.com/jytt8u/marvia/internal/client"
	"github.com/jytt8u/marvia/internal/foreign"
	"github.com/jytt8u/marvia/internal/netpath"
	"github.com/jytt8u/marvia/internal/routes"
	"github.com/jytt8u/marvia/internal/transport"
	"github.com/jytt8u/marvia/internal/tunbridge"
	"github.com/jytt8u/marvia/internal/vp1"
)

// Обход туннеля на компьютере: что идёт мимо и как.
//
// Решает мост на каждое соединение (tunbridge.Split), а не таблица маршрутов
// системы. Подробности и цена — там же.

// localNetwork — обычная сеть компьютера для соединений мимо туннеля.
//
// Тот же netpath, что держит соединения с нодой: сокет привязывается к
// внешнему интерфейсу, и его пакеты не попадают в собственный туннель по
// маршрутам /1. Интерфейс выбирается на каждое соединение заново, поэтому
// переход с Wi-Fi на кабель обход переживает сам.
var localNetwork = tunbridge.DialerFunc{
	Target: func(ctx context.Context, t vp1.Address) (net.Conn, error) {
		return netpath.Dialer().DialContext(ctx, "tcp", t.String())
	},
	// Подключённый UDP-сокет: один Read — одна датаграмма, как мост и ждёт.
	Datagrams: func(ctx context.Context, t vp1.Address) (net.Conn, error) {
		return netpath.Dialer().DialContext(ctx, "udp", t.String())
	},
}

// around — идти ли к адресу мимо туннеля.
func (c *Controller) around(ip netip.Addr) bool {
	return (c.lan.Load() && homeNetwork(ip)) || c.bypass.Load().Contains(ip)
}

// applyRussian ставит скачанный список в работу. Списка нет — обход ничего
// не уводит, и окно об этом скажет.
//
// Переключатель проверяется здесь, а не у того, кто зовёт: список качается в
// фоне секундами, и человек успевает выключить обход, пока он идёт. Без этой
// проверки опоздавшая загрузка включила бы его обратно молча.
func (c *Controller) applyRussian(dir string) int {
	c.mu.Lock()
	on := c.settings.BypassRussian
	c.mu.Unlock()
	list, _ := loadRuRoutes(dir)
	if !on || len(list) == 0 {
		c.bypass.Store(nil)
		return 0
	}
	set := routes.NewSet(list)
	c.bypass.Store(set)
	return set.Len()
}

// errForeignBypass — у чужой подписки списка взять неоткуда.
var errForeignBypass = errors.New("bypassForeign")

// refreshRussian скачивает российский список у панели, если он устарел
// (force — не глядя на возраст), и ставит его в работу.
//
// Качает ядро тем же путём, что подписку: по адресам панели из ссылки, если
// они там есть. Туннель при этом может быть поднят — тогда запрос уйдёт
// через него, и это не беда: панель видна и оттуда.
//
// Одна загрузка за раз: подключение и окно просят список одновременно, а две
// записи через один временный файл мешают друг другу.
func (c *Controller) refreshRussian(account string, force bool) error {
	c.ruMu.Lock()
	defer c.ruMu.Unlock()
	dir, err := settingsDir()
	if err != nil {
		return err
	}
	if _, fetched := loadRuRoutes(dir); !force && !ruRoutesStale(fetched, time.Now()) {
		c.applyRussian(dir)
		return nil
	}
	if foreign.IsForeign(account) {
		c.setRuErr(say("bypassForeign"))
		return errForeignBypass
	}
	parsed, err := client.ParseAccountLink(account)
	if err != nil {
		c.setRuErr(err.Error())
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	prefixes, err := client.FetchBypass(ctx, parsed.SubscriptionURL, parsed.PanelIPs)
	if err == nil {
		err = saveRuRoutes(dir, prefixes)
	}
	if err != nil {
		// Старый список, если был, продолжает работать: месячной давности
		// подсети лучше, чем никаких.
		c.applyRussian(dir)
		c.setRuErr(err.Error())
		c.log.add("%s", sayf("logBypassFailed", err))
		return err
	}
	c.setRuErr("")
	n := c.applyRussian(dir)
	c.log.add("%s", sayf("logBypassCount", n))
	return nil
}

func (c *Controller) setRuErr(reason string) {
	c.mu.Lock()
	c.ruErr = reason
	c.mu.Unlock()
}

// Settings отдаёт настройки подключения.
func (c *Controller) Settings() SettingsView {
	c.mu.Lock()
	defer c.mu.Unlock()
	var killInUse *bool
	if c.adapter != nil {
		killInUse = boolPtr(c.guard != nil)
	}
	return SettingsView{
		BypassRussian:    c.settings.BypassRussian,
		RuCount:          c.bypass.Load().Len(),
		RuError:          c.ruErr,
		LANOutside:       c.settings.LANOutside,
		SitesDirect:      displaySites(c.settings.SitesDirect),
		SitesTunnel:      displaySites(c.settings.SitesTunnel),
		AppsDirect:       append([]string{}, c.settings.AppsDirect...),
		AppsTunnel:       append([]string{}, c.settings.AppsTunnel...),
		DNS:              dnsHost(c.settings.resolver(c.dns)),
		DNSInUse:         c.dnsInUse,
		DNSSecure:        !c.settings.DNSPlain,
		DNSSecureNow:     c.settings.encrypted(c.settings.resolver(c.dns)),
		DNSSecureInUse:   c.dnsSecureInUse,
		Fragment:         c.settings.Fragment,
		FragmentInUse:    c.fragmentInUse,
		TCPOnly:          c.settings.TCPOnly,
		TCPOnlyInUse:     c.tcpOnlyInUse,
		Fingerprint:      c.settings.Fingerprint,
		FingerprintInUse: c.fingerprintInUse,
		IPv6:             !c.settings.IPv6Off,
		IPv6InUse:        c.ipv6InUse,
		Reports:          !c.settings.ReportsOff,
		MTU:              c.settings.MTU,
		MTUDefault:       c.mtu,
		MTUInUse:         c.mtuInUse,
		KillSwitch:       c.settings.KillSwitch,
		KillSwitchInUse:  killInUse,
		KillSwitchError:  c.guardErr,
		AutoFix:          !c.settings.AutoFixOff,
	}
}

// UpdateSettings меняет настройки и сразу их применяет.
//
// Обход действует на новые соединения без переподключения: мост спрашивает
// его на каждое. Уже открытые соединения доживают по старому пути — рвать
// их ради этого незачем.
func (c *Controller) UpdateSettings(p SettingsPatch) (SettingsView, error) {
	dir, err := settingsDir()
	if err != nil {
		return c.Settings(), err
	}
	c.mu.Lock()
	next := c.settings
	if p.BypassRussian != nil {
		next.BypassRussian = *p.BypassRussian
	}
	if p.LANOutside != nil {
		next.LANOutside = *p.LANOutside
	}
	if p.Fragment != nil {
		next.Fragment = *p.Fragment
	}
	if p.TCPOnly != nil {
		next.TCPOnly = *p.TCPOnly
	}
	if p.Fingerprint != nil {
		// Только из списка, который знает ядро: опечатка иначе молча стала
		// бы Chrome, и человек думал бы, что выбрал другое.
		if _, known := transport.Fingerprint(*p.Fingerprint); *p.Fingerprint != "" && !known {
			c.mu.Unlock()
			return c.Settings(), errors.New(say("badRequest"))
		}
		next.Fingerprint = *p.Fingerprint
	}
	if p.IPv6 != nil {
		next.IPv6Off = !*p.IPv6
	}
	if p.Reports != nil {
		next.ReportsOff = !*p.Reports
	}
	if p.KillSwitch != nil {
		next.KillSwitch = *p.KillSwitch
	}
	if p.AutoFix != nil {
		next.AutoFixOff = !*p.AutoFix
	}
	if p.DNSSecure != nil {
		next.DNSPlain = !*p.DNSSecure
	}
	if p.MTU != nil {
		if err := checkMTU(*p.MTU); err != nil {
			c.mu.Unlock()
			return c.Settings(), errors.New(say(err.Error()))
		}
		next.MTU = *p.MTU
	}
	if p.DNS != nil {
		want := strings.TrimSpace(*p.DNS)
		if want != "" {
			if err := checkDNS(want); err != nil {
				c.mu.Unlock()
				return c.Settings(), errors.New(say(err.Error()))
			}
		}
		next.DNS = want
	}
	if err := next.placeSites(p.SitesDirect, p.SitesTunnel); err != nil {
		c.mu.Unlock()
		return c.Settings(), errors.New(siteError(err))
	}
	if err := next.placeApps(p.AppsDirect, p.AppsTunnel); err != nil {
		c.mu.Unlock()
		return c.Settings(), errors.New(say(err.Error()))
	}
	account := c.account
	c.mu.Unlock()

	if err := saveSettings(dir, next); err != nil {
		return c.Settings(), err
	}
	c.mu.Lock()
	c.settings = next
	c.mu.Unlock()
	c.lan.Store(next.LANOutside)
	// Правила по сайтам, как и обход, действуют на новые соединения сразу.
	c.sites.Set(next.SitesDirect, next.SitesTunnel)
	c.apps.Set(next.AppsDirect, next.AppsTunnel)

	// Kill switch и домашняя сеть в его правилах действуют сразу: ждать
	// переподключения ради запрета, который человек только что включил,
	// значило бы оставить его без защиты, думая, что она есть.
	if p.KillSwitch != nil || p.LANOutside != nil {
		c.reguard()
	}

	if p.BypassRussian != nil {
		if !next.BypassRussian {
			c.bypass.Store(nil)
			c.setRuErr("")
			c.log.add("%s", say("logBypassOff"))
		} else {
			// Ошибку скачивания не возвращаем как ошибку сохранения:
			// переключатель включён, а причина видна под ним.
			_ = c.refreshRussian(account, false)
		}
	}
	return c.Settings(), nil
}

// siteError — почему список сайтов не принят, на языке окна и с той
// строкой, которая не подошла.
func siteError(err error) string {
	var bad *tunbridge.BadSiteError
	if errors.As(err, &bad) {
		return sayf("siteBad", bad.Site)
	}
	if errors.Is(err, tunbridge.ErrTooManySites) {
		return say("siteTooMany")
	}
	return err.Error()
}

// dnsHost — адрес резолвера без порта, как его показывает окно.
func dnsHost(hostPort string) string {
	host, _, err := net.SplitHostPort(hostPort)
	if err != nil {
		return hostPort
	}
	return host
}

// displaySites — список для окна: всегда массив и с кириллицей, как вписано.
// Окно присылает его обратно целиком, и NormalizeSites вернёт те же имена.
// Здесь, а не в settings.go: зовёт его только окно Windows.
func displaySites(list []string) []string {
	out := make([]string, 0, len(list))
	for _, name := range list {
		out = append(out, tunbridge.DisplaySite(name))
	}
	return out
}
