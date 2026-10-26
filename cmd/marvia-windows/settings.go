package main

import (
	"encoding/json"
	"errors"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jytt8u/marvia/internal/securedns"
	"github.com/jytt8u/marvia/internal/tunbridge"
)

// pcSettings — то, что человек выбрал в настройках подключения.
//
// Один файл JSON, а не по файлу на значение, как язык окна: эти настройки
// читает подключение целиком и разом, и половина, записанная без второй
// половины, давала бы туннель, которого человек не выбирал.
//
// Незнакомое поле при чтении пропускается, пропущенное значит «как было по
// умолчанию»: файл переживает и откат на старую версию, и обновление.
type pcSettings struct {
	// BypassRussian — российские сайты мимо туннеля, по списку подсетей от
	// панели продавца. Выключено по умолчанию, как на телефоне: решение,
	// что банк должен видеть настоящий адрес, принимает человек.
	BypassRussian bool `json:"bypass_russian,omitempty"`

	// LANOutside — частные адреса (домашняя сеть) мимо туннеля. Выключено по
	// умолчанию по той же причине, что на телефоне: в чужом Wi-Fi «домашняя
	// сеть» — это чужая сеть, и пускать туда трафик мимо туннеля человек
	// решает сам.
	LANOutside bool `json:"lan_outside,omitempty"`

	// DNS — чей резолвер отвечает на запросы имён: адрес IPv4 без порта.
	// Пусто — тот, что задан флагом -dns (по умолчанию Cloudflare). Сами
	// запросы в любом случае идут через туннель; выбор только в том, кто на
	// другом конце: у кого-то есть фильтр рекламы, у кого-то нет.
	DNS string `json:"dns,omitempty"`

	// DNSPlain — имена как раньше: открытым DNS через туннель, и нода их
	// видит. По умолчанию имена к известному резолверу уходят по HTTPS (см.
	// internal/securedns). Хранится как «открыто», чтобы умолчание —
	// шифровать — было нулём и в файле, записанном прежней версией.
	DNSPlain bool `json:"dns_plain,omitempty"`

	// Fragment — резать TLS-приветствие к нодам так, чтобы имя из SNI не
	// лежало целиком ни в одном TCP-сегменте. Против фильтров по имени.
	// Выключено по умолчанию, как на телефоне: без такого фильтра у
	// провайдера это лишние пакеты и своя примета.
	Fragment bool `json:"fragment,omitempty"`

	// TCPOnly — к нодам Marvia только по TCP, без пробы QUIC. Нужен там, где
	// UDP не режут, а душат: QUIC дозванивается и потом еле тянет, и сам
	// клиент этого не отличит от медленной ноды (client.Options.NoQUIC).
	// Выключено по умолчанию: где UDP живой, QUIC ровнее на потерях.
	TCPOnly bool `json:"tcp_only,omitempty"`

	// Fingerprint — чьё TLS-приветствие изображать к нодам Marvia вместо
	// выбранного продавцом (client.Options.Fingerprint). Пусто — как у
	// продавца. Только руками: сам клиент отпечаток не перебирает.
	Fingerprint string `json:"fingerprint,omitempty"`

	// IPv6Off — не пускать IPv6 через туннель вовсе. Мимо он при этом тоже не
	// уходит: маршрут IPv6 остаётся в туннеле, мост отвечает «адресов IPv6
	// нет», и программы идут по IPv4 (tunbridge.Config.NoIPv6). Нужен, когда
	// сайты путают страну: IPv6-адрес ноды их базы порой относят не туда, куда
	// IPv4, и один сервис видит человека то здесь, то там. Хранится как
	// «выключен», чтобы умолчание — IPv6 через туннель — было нулём.
	IPv6Off bool `json:"ipv6_off,omitempty"`

	// ReportsOff — не отправлять продавцу замеры нод при подключении. Только
	// для ключа Marvia: чужим панелям отчёты не шлются вовсе. Хранится как
	// «выключено», чтобы умолчание — отправлять, как на телефоне, — было нулём.
	ReportsOff bool `json:"reports_off,omitempty"`
	// KillSwitch — пока туннель включён, мимо него не идёт ничего, даже если
	// он лёг: см. internal/killswitch. Выключен по умолчанию: закрытый
	// интернет при замолчавшей ноде — то, что человек должен выбрать сам и
	// знать, что выбрал, иначе это выглядит как поломка.
	KillSwitch bool `json:"kill_switch,omitempty"`

	// AutoFixOff — не убирать помехи самим: не отключать WARP на время
	// подключения, не снимать брошенный прокси, не поднимать пропавший
	// адаптер. Хранится как «выключено», чтобы умолчание — убирать — было
	// нулём: приходят к нам как раз те, кто сам этого не сделает.
	AutoFixOff bool `json:"auto_fix_off,omitempty"`

	// MTU — размер пакета на адаптере; ноль — как в программе по умолчанию.
	// Нужен там, где провайдер или чужой туннель снаружи режет крупные
	// пакеты: сайты открываются наполовину, а меньший MTU это лечит.
	MTU uint32 `json:"mtu,omitempty"`

	// SitesDirect — сайты, которые идут мимо туннеля, по имени: банк,
	// госуслуги, всё, что не пускает из-за границы. SitesTunnel — сайты,
	// которые идут через туннель, даже если их адрес в российском списке.
	// Имя действует вместе с поддоменами. Решает мост: tunbridge.Sites, там
	// же — как он узнаёт имя соединения и где это не получается. Пустые по
	// умолчанию: новичку правила не нужны, а российский обход — один
	// переключатель.
	SitesDirect []string `json:"sites_direct,omitempty"`
	SitesTunnel []string `json:"sites_tunnel,omitempty"`

	// AppsDirect и AppsTunnel — программы по имени файла: всегда мимо и
	// всегда через туннель (apps.go). Сильнее правил по сайтам.
	AppsDirect []string `json:"apps_direct,omitempty"`
	AppsTunnel []string `json:"apps_tunnel,omitempty"`
}

// placeSites ставит списки сайтов из правки окна.
//
// Сайт живёт ровно в одном списке: только что вписанный в один пропадает из
// другого. Иначе человек видел бы «сбербанк» и в «мимо», и в «через», и
// гадал бы, какой из них главный. Неверное имя в правке не меняет ничего —
// ни одного из списков.
func (s *pcSettings) placeSites(direct, tunnel *[]string) error {
	next := *s
	if direct != nil {
		list, err := tunbridge.NormalizeSites(*direct)
		if err != nil {
			return err
		}
		next.SitesDirect = list
		next.SitesTunnel = withoutSites(next.SitesTunnel, list)
	}
	if tunnel != nil {
		list, err := tunbridge.NormalizeSites(*tunnel)
		if err != nil {
			return err
		}
		next.SitesTunnel = list
		next.SitesDirect = withoutSites(next.SitesDirect, list)
	}
	*s = next
	return nil
}

// withoutSites — список без имён из drop; новый срез, чтобы не задеть
// настройки, с которых он скопирован.
func withoutSites(list, drop []string) []string {
	gone := make(map[string]bool, len(drop))
	for _, name := range drop {
		gone[name] = true
	}
	var out []string
	for _, name := range list {
		if !gone[name] {
			out = append(out, name)
		}
	}
	return out
}

// mtuMin и mtuMax — пределы своего MTU. Ниже 1280 IPv6 по стандарту не
// работает вовсе, выше 1500 — уже не обычная сеть, а то, чего у
// домашнего провайдера не бывает.
const (
	mtuMin = 1280
	mtuMax = 1500
)

// checkMTU — ноль (по умолчанию) или число в пределах.
func checkMTU(v uint32) error {
	if v != 0 && (v < mtuMin || v > mtuMax) {
		return errors.New("mtuRange")
	}
	return nil
}

// mtu — действующий размер: свой или программы.
func (s pcSettings) mtu(def uint32) uint32 {
	if s.MTU != 0 {
		return s.MTU
	}
	return def
}

// SettingsView — настройки подключения и правда о них для окна.
type SettingsView struct {
	// MTU — выбранный (ноль — по умолчанию), MTUDefault — какой это, MTUInUse —
	// с каким поднят адаптер: размер ставится вместе с ним и меняется со
	// следующего подключения.
	MTU           uint32 `json:"mtu"`
	MTUDefault    uint32 `json:"mtu_default"`
	MTUInUse      uint32 `json:"mtu_in_use,omitempty"`
	BypassRussian bool   `json:"bypass_russian"`

	// RuCount — сколько российских подсетей сейчас уводится мимо туннеля;
	// RuError — почему список не скачался. Оба нужны под переключателем.
	RuCount int    `json:"ru_count"`
	RuError string `json:"ru_error,omitempty"`

	LANOutside bool `json:"lan_outside"`

	// SitesDirect и SitesTunnel — списки сайтов; всегда массивы, а не null:
	// окну проще рисовать пустой список, чем проверять его наличие.
	SitesDirect []string `json:"sites_direct"`
	SitesTunnel []string `json:"sites_tunnel"`
	AppsDirect  []string `json:"apps_direct"`
	AppsTunnel  []string `json:"apps_tunnel"`

	// DNS — выбранный резолвер; DNSInUse — тот, с которым поднят туннель.
	// Разные — значит выбор применится при следующем подключении: адрес
	// резолвера стоит на адаптере и в политике имён системы, а их меняют
	// только вместе с адаптером.
	DNS      string `json:"dns"`
	DNSInUse string `json:"dns_in_use,omitempty"`

	// DNSSecure — человек хочет шифровать имена; DNSSecureNow — получится ли
	// это с выбранным резолвером: свой адрес шифровать нечем, и окно должно
	// сказать это прямо, а не показывать включённый переключатель, который
	// ничего не делает. DNSSecureInUse — шифрует ли поднятый туннель.
	DNSSecure      bool  `json:"dns_secure"`
	DNSSecureNow   bool  `json:"dns_secure_now"`
	DNSSecureInUse *bool `json:"dns_secure_in_use,omitempty"`

	// Fragment — выбранное; FragmentInUse — с каким поднят туннель. Дробление
	// решается при дозвоне до ноды, поэтому новое действует со следующего
	// подключения.
	Fragment      bool  `json:"fragment"`
	FragmentInUse *bool `json:"fragment_in_use,omitempty"`

	// TCPOnly — выбранное; TCPOnlyInUse — так ли поднят туннель. nil — туннеля
	// нет или ключ чужой: у чужих протоколов QUIC от Marvia нет вовсе.
	TCPOnly      bool  `json:"tcp_only"`
	TCPOnlyInUse *bool `json:"tcp_only_in_use,omitempty"`

	// Fingerprint — выбранное («» — как у продавца); FingerprintInUse — с
	// каким поднят туннель (nil — туннеля нет или ключ чужой).
	Fingerprint      string  `json:"fingerprint"`
	FingerprintInUse *string `json:"fingerprint_in_use,omitempty"`

	// IPv6 — пускать ли IPv6 через туннель; IPv6InUse — как у поднятого.
	IPv6      bool  `json:"ipv6"`
	IPv6InUse *bool `json:"ipv6_in_use,omitempty"`

	// Reports — отправлять ли замеры нод панели продавца.
	Reports bool `json:"reports"`

	// KillSwitch — выбранное; KillSwitchInUse — стоит ли фильтр у поднятого
	// туннеля (nil — туннеля нет); KillSwitchError — почему не встал.
	KillSwitch      bool   `json:"kill_switch"`
	KillSwitchInUse *bool  `json:"kill_switch_in_use,omitempty"`
	KillSwitchError string `json:"kill_switch_error,omitempty"`

	// AutoFix — убирать ли помехи самим; см. pcSettings.AutoFixOff.
	AutoFix bool `json:"auto_fix"`
}

// SettingsPatch — что окно просит поменять; nil — не трогать.
type SettingsPatch struct {
	MTU           *uint32 `json:"mtu"`
	BypassRussian *bool   `json:"bypass_russian"`
	LANOutside    *bool   `json:"lan_outside"`
	DNS           *string `json:"dns"`
	DNSSecure     *bool   `json:"dns_secure"`
	Fragment      *bool   `json:"fragment"`
	TCPOnly       *bool   `json:"tcp_only"`
	Fingerprint   *string `json:"fingerprint"`
	IPv6          *bool   `json:"ipv6"`
	Reports       *bool   `json:"reports"`
	KillSwitch    *bool   `json:"kill_switch"`
	AutoFix       *bool   `json:"auto_fix"`

	// SitesDirect и SitesTunnel — список целиком, а не «добавь одно»: окно
	// присылает ровно то, что показывает, и правка не зависит от того, что
	// программа помнит между запросами.
	SitesDirect *[]string `json:"sites_direct"`
	SitesTunnel *[]string `json:"sites_tunnel"`
	AppsDirect  *[]string `json:"apps_direct"`
	AppsTunnel  *[]string `json:"apps_tunnel"`
}

// settingsFile — где лежат настройки подключения.
func settingsFile(dir string) string { return filepath.Join(dir, "settings.json") }

// loadSettings читает настройки. Нет файла или он испорчен — умолчания:
// туннель должен подниматься и тогда, когда настройки потерялись.
func loadSettings(dir string) pcSettings {
	var s pcSettings
	if dir == "" {
		return s
	}
	raw, err := os.ReadFile(settingsFile(dir))
	if err != nil || len(raw) > 64<<10 {
		return s
	}
	if json.Unmarshal(raw, &s) != nil {
		return pcSettings{}
	}
	return s
}

// saveSettings пишет настройки через временный файл: оборванная запись не
// должна оставить файл, который потом не прочитается.
func saveSettings(dir string, s pcSettings) error {
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp := settingsFile(dir) + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, settingsFile(dir)); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// Российский список на диске.
//
// Тот же список, что качает телефон, и с того же адреса /sub/{токен}/bypass:
// панель отдаёт его всем своим приложениям, и подсети меняются раз в месяц.
// Скачанное лежит рядом с ключом: включение туннеля не должно ждать сети, а
// панель бывает недоступна ровно тогда, когда VPN и нужен.
const (
	ruRoutesFile = "ru-routes.txt"

	// ruRoutesMaxAge — как часто спрашиваем панель. Чаще раза в неделю
	// незачем: каждый поход — это запрос имени её домена.
	ruRoutesMaxAge = 7 * 24 * time.Hour
)

func ruRoutesPath(dir string) string { return filepath.Join(dir, ruRoutesFile) }

// loadRuRoutes читает скачанный список и когда он скачан. Нет файла — пусто.
func loadRuRoutes(dir string) ([]string, time.Time) {
	path := ruRoutesPath(dir)
	info, err := os.Stat(path)
	if err != nil || info.Size() > 16<<20 {
		return nil, time.Time{}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, time.Time{}
	}
	return strings.Fields(string(raw)), info.ModTime()
}

// ruRoutesStale — пора ли спросить панель заново.
func ruRoutesStale(fetched, now time.Time) bool {
	return fetched.IsZero() || now.Sub(fetched) >= ruRoutesMaxAge
}

// saveRuRoutes кладёт список целиком и разом: половина списка молча
// оставила бы часть российских сайтов в туннеле.
func saveRuRoutes(dir string, prefixes []string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp := ruRoutesPath(dir) + ".tmp"
	if err := os.WriteFile(tmp, []byte(strings.Join(prefixes, "\n")+"\n"), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, ruRoutesPath(dir)); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// homeNetwork — адрес из домашней сети: частные диапазоны IPv4 (10/8,
// 172.16/12, 192.168/16) — те же, что исключает телефон, — и их IPv6-пара
// fc00::/7.
//
// Свою подсеть Windows и без того ведёт мимо туннеля: её маршрут точнее
// половин /1, которыми туннель забирает трафик. Переключатель нужен для
// остального частного — соседней подсети за роутером, сетевого диска,
// камеры, — которое иначе ушло бы на ноду за границей и там пропало.
func homeNetwork(ip netip.Addr) bool {
	return ip.IsPrivate()
}

var (
	errDNSBad   = errors.New("dnsBad")
	errDNSLocal = errors.New("dnsLocal")
)

// checkDNS проверяет свой адрес резолвера — те же правила, что у телефона.
//
// Опечатка здесь означает «интернет не работает» без единой подсказки,
// почему, поэтому строго: четыре числа без ведущих нулей (010 одни читают
// восьмеричным, другие десятичным). Локальный адрес отвергается отдельно и с
// причиной: запрос имени уходит в туннель, и роутер 192.168.1.1 оттуда не
// виден, а нода к частным адресам не ходит. IPv6 не берём: при ноде без
// IPv6 такой резолвер молча перестал бы отвечать.
func checkDNS(address string) error {
	parts := strings.Split(strings.TrimSpace(address), ".")
	if len(parts) != 4 {
		return errDNSBad
	}
	var n [4]int
	for i, p := range parts {
		if p == "" || len(p) > 3 || (len(p) > 1 && p[0] == '0') {
			return errDNSBad
		}
		for _, r := range p {
			if r < '0' || r > '9' {
				return errDNSBad
			}
			n[i] = n[i]*10 + int(r-'0')
		}
		if n[i] > 255 {
			return errDNSBad
		}
	}
	a, b := n[0], n[1]
	local := a == 0 || a == 10 || a == 127 || a >= 224 ||
		(a == 100 && b >= 64 && b <= 127) ||
		(a == 169 && b == 254) ||
		(a == 172 && b >= 16 && b <= 31) ||
		(a == 192 && b == 168)
	if local {
		return errDNSLocal
	}
	return nil
}

// resolver — куда слать запросы имён при подключении, в виде host:port.
// Испорченный выбор в файле не ломает имена, а откатывается на флаг.
func (s pcSettings) resolver(fallback string) string {
	if s.DNS != "" && checkDNS(s.DNS) == nil {
		return net.JoinHostPort(strings.TrimSpace(s.DNS), "53")
	}
	return fallback
}

// encrypted — пойдут ли имена по HTTPS при этих настройках к этому резолверу
// (host:port). Нет — если человек выбрал «как раньше» или резолвер свой: для
// своего адреса мы не знаем ни имени сертификата, ни пути DoH.
func (s pcSettings) encrypted(resolver string) bool {
	if s.DNSPlain {
		return false
	}
	_, ok := securedns.For(resolver)
	return ok
}
