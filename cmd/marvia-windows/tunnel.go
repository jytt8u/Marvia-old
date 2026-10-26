//go:build windows

package main

import (
	"context"
	"errors"
	"fmt"
	"github.com/jytt8u/marvia/internal/netpath"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jytt8u/marvia/internal/client"
	"github.com/jytt8u/marvia/internal/foreign"
	"github.com/jytt8u/marvia/internal/killswitch"
	"github.com/jytt8u/marvia/internal/redact"
	"github.com/jytt8u/marvia/internal/routes"
	"github.com/jytt8u/marvia/internal/securedns"
	"github.com/jytt8u/marvia/internal/tunbridge"
	"github.com/jytt8u/marvia/internal/vp1"
	"github.com/jytt8u/marvia/internal/wintun"
)

// State — что сейчас с туннелем.
type State string

const (
	StateIdle       State = "idle"
	StateConnecting State = "connecting"
	StateConnected  State = "connected"
	StateFailed     State = "failed"

	// StateStalled — туннель поднят, но нода перестала отвечать.
	//
	// Отдельное состояние, а не «не подключилось»: туннель на месте, маршруты
	// стоят, и разница для человека принципиальная — «нажми ещё раз» против
	// «сейчас ничего не работает и вот почему».
	StateStalled State = "stalled"

	// StateDisconnecting — туннель снимается, а сеть возвращается как была.
	//
	// Раньше окно писало «Отключено» сразу, а уборка шла ещё секунды: человек
	// жал «Подключиться», программа молча отказывала — уборка не кончилась, —
	// и он жал снова, не понимая, включено или нет.
	StateDisconnecting State = "disconnecting"
)

// Status — всё, что показывает окно.
type Status struct {
	State   State  `json:"state"`
	Node    string `json:"node"`
	Address string `json:"address"`
	PingMS  int64  `json:"ping_ms"`
	// Transport — чем идём до ноды: tls, reality, ws или quic. Пусто без
	// туннеля. Окно показывает его в обзоре — «как именно я подключён» —
	// вопрос, на который раньше отвечал только журнал.
	Transport string `json:"transport,omitempty"`
	Up        int64  `json:"up"`
	Down      int64  `json:"down"`
	Reason    string `json:"reason,omitempty"`
	// Step — чем подключение занято прямо сейчас: «проверяю страны»,
	// «поднимаю туннель». Одно «Подключаюсь» на десять секунд выглядит как
	// зависание, и человек жмёт кнопку снова — то есть отменяет.
	Step string `json:"step,omitempty"`
	// Quality — отклик, джиттер и потери текущей ноды вживую.
	Quality QualityView `json:"quality"`
	// MovedFrom — с какой ноды надзор уехал сам за последние полминуты:
	// окно говорит «Финляндия перестала отвечать — перешёл на Германию».
	MovedFrom string `json:"moved_from,omitempty"`
	// Conflicts — мешающие программы, уже словами на языке окна.
	Conflicts []string `json:"conflicts,omitempty"`
	// Check — что нашла проверка после подключения, словами; пусто — всё
	// открывается или проверки ещё не было.
	Check string `json:"check,omitempty"`

	// Подписка: до какого числа и сколько трафика осталось. Пустые значения
	// означают, что продавец не поставил ни срока, ни квоты, — врать про
	// «безлимит» в этом случае нельзя.
	Until      string `json:"until,omitempty"`
	LimitBytes int64  `json:"limit_bytes,omitempty"`
	LeftBytes  int64  `json:"left_bytes,omitempty"`
	HasAccount bool   `json:"has_account"`
	Elevated   bool   `json:"elevated"`

	// Since — когда поднялся туннель, unix-секунды. Окно считает по нему
	// длительность сессии само: слать готовую строку раз в секунду — значит
	// собирать её на языке окна в программе, которой это знать незачем.
	Since int64 `json:"since,omitempty"`

	// History — байты по минутам за последний час, от старой к текущей.
	History []int64         `json:"history"`
	Traffic trafficSnapshot `json:"traffic"`

	Version string `json:"version"`

	// Update — версия, которую панель продавца выложила и которая новее
	// нашей; пусто, если обновляться не на что. Ссылка — с домена продавца:
	// магазины и github в России отваливаются раньше всего остального.
	Update    string `json:"update,omitempty"`
	UpdateURL string `json:"update_url,omitempty"`

	// Proxy — предупреждение о системном прокси, если он есть.
	//
	// По наличию прокси нельзя определить его внешний маршрут. Показываем
	// конфликт настроек для проверки, не объявляя доказанную утечку.
	Proxy      string `json:"proxy,omitempty"`
	ProxyOwner string `json:"proxy_owner,omitempty"`
	// ProxyServer — адрес отдельно: в обзоре настроек плитка узкая, и
	// целая фраза из Proxy в ней обрезалась на «В системе прописан …».
	ProxyServer string `json:"proxy_server,omitempty"`
	ProxyEnv    bool   `json:"proxy_env,omitempty"`

	// Route — как именно идём до ноды: протокол, транспорт, имя прикрытия,
	// отпечаток. Только то, с чем дозванивается ядро; без туннеля — пусто.
	// «Диагностика» показывает это человеку, который спрашивает продавца
	// «у меня не работает»: без этих слов продавец гадает.
	Route *RouteView `json:"route,omitempty"`

	// Seller — поддержка, «Продлить», объявление и напоминание о сроке; см.
	// seller.go. Есть и без туннеля: нужнее всего, когда подключиться нельзя.
	Seller SellerView `json:"seller"`
}

// Controller держит туннель и знает, как его включить и выключить.
//
// Всё состояние собрано в одном месте под одним замком. Разложить его по
// разным местам было бы соблазнительно, но кнопку в окне человек нажимает
// когда захочет, в том числе посреди подключения, — и тогда две половины
// состояния начали бы расходиться.
type Controller struct {
	mu            sync.Mutex
	tunnelMu      sync.Mutex
	disconnectMu  sync.Mutex
	disconnecting bool
	// leaving — сколько отключений идёт прямо сейчас, вместе с возвратом
	// помех. Пока не ноль, окно показывает «Отключаюсь», а новое
	// подключение ждёт: поднимать туннель посреди уборки старого нельзя.
	leaving int
	// step — шаг подключения для окна; см. Status.Step.
	step string
	// where — адрес и страна через туннель, до конца сессии; см. netcheck.go.
	where WhereView
	// quality — отклик текущей ноды за последние минуты; см. quality.go.
	quality qualityRing
	// movedFrom и movedAt — откуда надзор уехал сам, чтобы окно сказало
	// почему; manualAt — когда ноду выбрал человек: такой переезд не беда.
	movedFrom         string
	movedAt, manualAt time.Time
	connectCtx        context.Context
	state             State
	node              client.Node
	mtuInUse          uint32
	ping              time.Duration
	reason            string

	// Подписка, с которой выбрана нода: её показывает окно.
	until      string
	limitBytes int64
	leftBytes  int64
	update     client.AppOffer
	account    string

	dialer  client.Backend
	adapter *wintun.Adapter
	bridge  *tunbridge.Bridge
	cancel  context.CancelFunc

	// guard — фильтр kill switch, пока туннель поднят и он включён; nil —
	// выключен или не встал. guardErr — почему не встал: окно говорит это
	// под переключателем, а не показывает включённый тумблер без защиты.
	guard    *killswitch.Guard
	guardErr string

	up, down         atomic.Int64
	baseUp, baseDown int64

	// proxy — что нашлось в системных настройках прокси.
	//
	// Проверяем при включении и, пока туннель поднят, раз в proxyRecheck: другой
	// клиент (Hiddify) ставит прокси и после нашего подключения, и тогда
	// браузер теряет сайты без единого слова от нас. На каждый опрос окна не
	// проверяем: перебор слушающих сокетов раз в секунду — расточительство.
	proxy   wintun.Proxy
	proxyAt time.Time

	// adapterAt — когда последний раз проверяли, что адаптер ещё в системе;
	// adapterLost — что пропажу уже заметили и убираем туннель.
	adapterAt   time.Time
	adapterLost bool

	// conflicts — мешающие программы, найденные при последней проверке.
	// Ищутся в фоне (conflictsBusy): спросить WARP, подключён ли он, — это
	// запуск его утилиты, и держать ради неё замок окна нельзя.
	conflicts     []string
	conflictsAt   time.Time
	conflictsBusy bool

	// fixer — помехи, остановленные на время подключения; см. autofix.go.
	fixer *pauser

	// recoveredAt — когда последний раз сами поднимали пропавший адаптер.
	recoveredAt time.Time

	// measuring, measured — замер нод для экрана «Серверы»: идёт ли он и что
	// успело прийти. Замер в фоне, окно спрашивает список, пока он идёт; см.
	// StartMeasure.
	measuring bool
	measured  map[int64]client.Measurement
	// fresh — какие ноды ответили в идущем замере. Прежние числа на это время
	// остаются на экране: раньше замер стирал их, и список мигал «меряю…»
	// так, будто ничего не мерилось.
	fresh map[int64]bool

	// check — итог проверки после подключения: пусто — всё хорошо или ещё
	// не проверяли, иначе ключ сообщения; checkNode — нода, о которой он.
	check     string
	checkNode string

	dns string
	mtu uint32
	log *journal

	// since — момент подъёма туннеля; нулевой, пока туннеля нет.
	since   time.Time
	hist    history
	traffic trafficStats

	// settings — настройки подключения из окна; см. settings.go.
	settings pcSettings

	// keys — все сохранённые ключи; рабочий из них — account. См. keys.go.
	keys []savedKey

	// bypass — российские подсети, которые сейчас идут мимо туннеля; nil —
	// обход выключен или списка нет. Указатель, а не поле под замком: его
	// читает мост на каждое новое соединение, и ждать ради этого замок
	// контроллера, который держат подключение и окно, нельзя. Он же даёт
	// включить и выключить обход на ходу, без переподключения.
	bypass atomic.Pointer[routes.Set]

	// lan — домашняя сеть мимо туннеля; атомарно по той же причине, что bypass.
	lan atomic.Bool

	// sites — правила по сайтам и память «адрес → имя». Одна на всю работу
	// программы, а не на подключение: кэш имён Windows переживает
	// переподключение, и приложение пойдёт к адресу, не спрашивая заново.
	sites *tunbridge.Sites

	// apps — правила по программам (apps.go); как и сайты, на всю работу.
	apps appRules

	// dnsInUse — резолвер поднятого туннеля; пусто, пока туннеля нет.
	dnsInUse string

	// dnsSecureInUse — идут ли имена поднятого туннеля по HTTPS; nil, пока
	// туннеля нет.
	dnsSecureInUse *bool

	// fragmentInUse — режется ли приветствие у поднятого туннеля; nil, пока
	// туннеля нет.
	fragmentInUse *bool

	// tcpOnlyInUse — без QUIC ли поднят туннель; nil, пока туннеля нет или ключ
	// чужой.
	tcpOnlyInUse *bool

	// fingerprintInUse — с каким отпечатком поднят туннель («» — как у
	// продавца); nil, пока туннеля нет или ключ чужой.
	fingerprintInUse *string

	// ipv6InUse — пускает ли поднятый туннель IPv6; nil, пока туннеля нет.
	ipv6InUse *bool

	// ruErr — почему российский список не скачался в последний раз; пусто
	// — скачался или не пробовали. Окно показывает его под переключателем:
	// включённый тумблер без списка ничего не уводит, и человек должен это
	// видеть, а не гадать, почему банк всё ещё видит заграницу.
	ruErr string
	ruMu  sync.Mutex

	// known — последняя известная подписка ключа, из кэша; пока туннель
	// поднят, окно смотрит на живую. reminded — ключ напоминания, которое
	// человек уже закрыл.
	known    client.Subscription
	reminded string
}

// NewController готовит управление, подхватывая сохранённую ссылку доступа.
func NewController(dns string, mtu uint32, log *journal) *Controller {
	c := &Controller{state: StateIdle, dns: dns, mtu: mtu, log: log, sites: tunbridge.NewSites()}
	c.account = readAccount()
	adoptLegacyCache(c.account)
	c.known = knownSubscription(c.account)
	c.reminded = readUISetting(remindedSetting)
	if dir, err := settingsDir(); err == nil {
		c.keys = loadKeys(dir, c.account)
	}
	if path, err := accountPath(); err == nil {
		c.traffic.load(filepath.Join(filepath.Dir(path), "traffic.json"), time.Now())
	}
	pausedPath := ""
	if dir, err := settingsDir(); err == nil {
		pausedPath = filepath.Join(dir, "paused.json")
	}
	c.fixer = newPauser(pausedPath, func(s string) { c.log.add("%s", s) })
	// Остатки прошлого запуска: программа упала или компьютер выключили
	// посреди подключения, и WARP так и стоит отключённым.
	if c.fixer.pending() {
		go c.fixer.restore()
	}
	if dir, err := settingsDir(); err == nil {
		c.settings = loadSettings(dir)
		c.lan.Store(c.settings.LANOutside)
		c.sites.Set(c.settings.SitesDirect, c.settings.SitesTunnel)
		c.apps.Set(c.settings.AppsDirect, c.settings.AppsTunnel)
		if c.settings.BypassRussian {
			c.applyRussian(dir)
		}
	}
	go c.keepHistory()
	return c
}

// keepHistory снимает счётчики трафика для графика за час.
//
// Раз в несколько секунд, а не на каждый опрос окна: окно может быть
// закрыто, а график за час должен остаться честным, когда его откроют.
func (c *Controller) keepHistory() {
	for now := range time.Tick(historyTick) {
		c.hist.sample(now, c.up.Load()+c.down.Load())
		c.traffic.sample(now, c.up.Load(), c.down.Load())
	}
}

const historyTick = 5 * time.Second

// Status отдаёт снимок состояния для окна.
// proxyRecheck — как часто перепроверять системный прокси при поднятом туннеле.
const proxyRecheck = 10 * time.Second

// conflictRecheck — как часто искать мешающие программы. Снимок процессов
// дешёвый, но раз в секунду окну он не нужен: программы не появляются так часто.
const conflictRecheck = 15 * time.Second

// processList — список процессов; подменяется в тестах.
var processList = runningProcesses

// adapterRecheck — как часто проверять, что адаптер туннеля ещё в системе.
const adapterRecheck = 5 * time.Second

// adapterPresent — проверка адаптера; подменяется в тестах.
var adapterPresent = func(a *wintun.Adapter) bool { return a.Present() }

// recoverGap — реже этого пропавший адаптер сами не поднимаем.
const recoverGap = 2 * time.Minute

// loseAdapter поднимает туннель заново, когда адаптер пропал.
//
// Один раз — сами: адаптер чаще всего удаляет программа, которую уже
// закрыли, и человеку незачем узнавать об этом из ошибки. Пропал снова за
// пару минут — значит, его удаляет то, что работает сейчас, и подъём по
// кругу дал бы окно, мигающее «подключено — потеряно» без конца. Тогда
// отключаемся и говорим почему: трафик в это время шёл бы мимо туннеля.
func (c *Controller) loseAdapter() {
	c.mu.Lock()
	again := !c.settings.AutoFixOff && time.Since(c.recoveredAt) >= recoverGap
	if again {
		c.recoveredAt = time.Now()
	}
	c.mu.Unlock()

	if again {
		c.log.add("%s", say("logAdapterLost"))
		c.teardown()
		c.mu.Lock()
		c.adapterLost = false
		c.mu.Unlock()
		_ = c.Connect()
		return
	}

	c.log.add("%s", say("adapterGone"))
	c.Disconnect()
	c.mu.Lock()
	c.state = StateFailed
	c.reason = say("adapterGone")
	c.adapterLost = false
	c.mu.Unlock()
}

// systemProxy — чтение настроек прокси; подменяется в тестах.
var systemProxy = wintun.SystemProxy

// disableProxy — снятие системного прокси; подменяется в тестах.
var disableProxy = wintun.DisableSystemProxy

// dropDeadProxy снимает прокси, который никто не слушает.
//
// Чужой прокси сами не трогаем (см. DropProxy), но мёртвый — не чей-то
// выбор, а мусор: закрытая не той кнопкой программа оставила адрес, на
// котором больше никого нет, и браузер с ним не открывает ничего. Вернуть
// его после отключения тоже нельзя — вернули бы поломку. Звать с отпущенным
// замком: чтение таблицы сокетов — не для окна, ждущего ответа.
func (c *Controller) dropDeadProxy() bool {
	p := systemProxy()
	if !p.Dead() {
		return false
	}
	if err := disableProxy(); err != nil {
		c.log.add("%s", sayf("logCleanup", err))
		return false
	}
	c.log.add("%s", sayf("logDeadProxy", p.Server))
	c.mu.Lock()
	c.proxy = systemProxy()
	c.proxyAt = time.Now()
	c.mu.Unlock()
	return true
}

// refreshConflicts ищет мешающие программы в фоне.
//
// WARP мешает, только когда подключён: его служба работает всегда, пока он
// установлен, и называть помехой выключенный WARP значило бы гнать человека
// удалять то, что ничему не мешает.
func (c *Controller) refreshConflicts() {
	found := conflictsAmong(processList())
	if i := slices.Index(found, "conflictWarp"); i >= 0 && !warpConnected() {
		found = slices.Delete(found, i, i+1)
	}
	c.mu.Lock()
	c.conflicts = found
	c.conflictsAt = time.Now()
	c.conflictsBusy = false
	c.mu.Unlock()
}

func (c *Controller) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.conflictsBusy && time.Since(c.conflictsAt) >= conflictRecheck {
		c.conflictsBusy = true
		go c.refreshConflicts()
	}
	if c.state == StateConnected && time.Since(c.proxyAt) >= proxyRecheck {
		c.proxy = systemProxy()
		c.proxyAt = time.Now()
		// Прокси, поставленный уже после подключения и тут же брошенный, —
		// тот же мусор, что и до него.
		if c.proxy.Dead() && !c.settings.AutoFixOff {
			go c.dropDeadProxy()
		}
	}
	// Адаптер мог исчезнуть: другая программа на Wintun удаляет его, и
	// трафик идёт мимо туннеля, а окно продолжало бы писать «подключено».
	if c.state == StateConnected && c.adapter != nil && !c.adapterLost && time.Since(c.adapterAt) >= adapterRecheck {
		c.adapterAt = time.Now()
		if !adapterPresent(c.adapter) {
			c.adapterLost = true
			go c.loseAdapter()
		}
	}
	pingMS := int64(0)
	if c.dialer != nil {
		pingMS = c.dialer.Measurement().PingMS()
	}
	// Живой отклик важнее замера при подключении: тот был минуту назад.
	quality := c.quality.view()
	if quality.PingMS > 0 {
		pingMS = quality.PingMS
	}

	state := c.state
	if c.disconnecting || c.leaving > 0 {
		state = StateDisconnecting
	}
	step := ""
	if state == StateConnecting {
		step = c.step
	}
	movedFrom := ""
	if time.Since(c.movedAt) < 30*time.Second {
		movedFrom = c.movedFrom
	}

	return Status{
		State:       state,
		Node:        c.node.Title(),
		Address:     c.node.Address,
		Transport:   transportOf(c.node, c.tcpOnlyInUse != nil && *c.tcpOnlyInUse),
		Route:       c.routeLocked(),
		PingMS:      pingMS,
		Up:          max(0, c.up.Load()-c.baseUp),
		Down:        max(0, c.down.Load()-c.baseDown),
		Reason:      c.reason,
		Step:        step,
		Quality:     quality,
		MovedFrom:   movedFrom,
		Until:       c.until,
		LimitBytes:  c.limitBytes,
		LeftBytes:   c.leftBytes,
		HasAccount:  c.account != "",
		Elevated:    elevated(),
		Since:       sinceUnix(c.since),
		History:     c.hist.minutes(time.Now()),
		Traffic:     c.traffic.snapshot(time.Now()),
		Version:     version,
		Update:      c.update.Version,
		UpdateURL:   c.update.URL,
		Conflicts:   conflictTexts(c.conflicts),
		Check:       c.checkText(),
		Proxy:       c.proxy.Describe(),
		ProxyOwner:  c.proxy.Owner,
		ProxyServer: c.proxy.Server,
		ProxyEnv:    c.proxy.FromEnv,
		Seller:      sellerView(c.subscriptionLocked(), c.reminded, time.Now()),
	}
}

// transportOf — способ связи с нодой для обзора. QUIC у ноды с TLS — первый
// выбор дозвона; при закрытом UDP он сам уходит на TCP, и это видно в журнале.
func transportOf(n client.Node, tcpOnly bool) string {
	if n.Address == "" {
		return ""
	}
	if n.QUIC && !tcpOnly && n.Transport() == client.TransportTLS {
		return "quic"
	}
	return string(n.Transport())
}

// OpenUpdate открывает ссылку на новую версию в браузере.
//
// Только ту, что пришла из подписки: страница окна не выбирает адрес сама,
// иначе любая ошибка в разметке превращалась бы в открытие чего угодно.
func (c *Controller) OpenUpdate() error {
	c.mu.Lock()
	url := c.update.URL
	c.mu.Unlock()
	if url == "" {
		return errors.New("обновляться не на что")
	}
	return openLink(url)
}

// DropProxy снимает системный прокси и перепроверяет, что получилось.
//
// Только по нажатию человека. Прокси мог быть поставлен осознанно — рабочим,
// родительским контролем, другим клиентом, которым он пользуется, — и снять
// его молча значило бы сломать то, чего мы не понимаем.
func (c *Controller) DropProxy() error {
	if err := wintun.DisableSystemProxy(); err != nil {
		return err
	}

	c.mu.Lock()
	c.proxy = systemProxy()
	c.proxyAt = time.Now()
	left := c.proxy
	c.mu.Unlock()

	c.log.add("%s", say("logProxyGone"))

	// Переменные окружения остаются жить в уже запущенных программах: они
	// прочитали их при старте, и наша правка реестра до них не дойдёт.
	if left.Found() && left.FromEnv {
		return errors.New(say("proxyInEnv"))
	}
	return nil
}

// Account отдаёт сохранённую ссылку доступа.
func (c *Controller) Account() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.account
}

// SetAccount проверяет ссылку и запоминает её.
//
// Проверяем тем же разбором, каким потом будем подключаться: сказать «ссылка
// не та» сразу при вставке гораздо лучше, чем после неудачной попытки.
func (c *Controller) SetAccount(link string) error {
	return c.AddKey("", link)
}

// AddKey проверяет ссылку, кладёт её в список ключей и делает рабочей —
// как «+» на экране «Серверы» телефона. Имя необязательно: без него берётся
// метка из ссылки или домен продавца.
func (c *Controller) AddKey(name, link string) error {
	link = strings.TrimSpace(link)
	// Чужая ссылка ноды проверяется сразу, адрес чужой подписки — при первом
	// походе: что по нему лежит, не узнать, не сходив.
	if foreign.IsForeign(link) {
		if foreign.IsLink(foreign.FirstLine(link)) {
			if _, err := foreign.Parse(foreign.FirstLine(link)); err != nil {
				return err
			}
		}
	} else if _, err := client.ParseAccountLink(link); err != nil {
		return err
	}

	dir, err := settingsDir()
	if err != nil {
		return err
	}
	c.mu.Lock()
	keys := withKey(c.keys, name, link)
	changed := link != c.account
	up := c.state != StateIdle && c.state != StateFailed
	c.mu.Unlock()
	if err := saveKeys(dir, keys); err != nil {
		return err
	}
	if err := writeAccount(link); err != nil {
		return err
	}
	c.mu.Lock()
	c.keys = keys
	c.account = link
	c.mu.Unlock()
	// Кнопки продавца — от нового ключа, а не от прежнего: «Продлить» чужого
	// магазина хуже, чем никакого.
	c.rememberSubscription(link)

	// Новый ключ стал рабочим — туннель переподнимается на нём, как при
	// «Сделать рабочим»: держать трафик на прежнем, показывая в окне новый,
	// значило бы врать, через чьи серверы он идёт.
	if changed && up {
		c.teardown()
		return c.Connect()
	}
	return nil
}

// Connect поднимает туннель.
func (c *Controller) Connect() error {
	c.mu.Lock()
	// StateStalled сюда же: туннель поднят, просто нода молчит. Поднимать
	// второй поверх первого нельзя — адаптер и маршруты уже заняты.
	if c.disconnecting || c.leaving > 0 || c.state == StateConnecting || c.state == StateConnected || c.state == StateStalled {
		c.mu.Unlock()
		return nil
	}
	if c.account == "" {
		c.mu.Unlock()
		return errors.New(say("noAccount"))
	}
	if !elevated() {
		c.mu.Unlock()
		return errors.New(say("needAdmin"))
	}

	account := c.account
	c.proxy = systemProxy()
	c.proxyAt = time.Now()
	c.state = StateConnecting
	c.reason = ""
	c.step = ""
	c.baseUp, c.baseDown = c.up.Load(), c.down.Load()
	c.traffic.resetRates(time.Now(), c.baseUp, c.baseDown)

	ctx, cancel := context.WithCancel(context.Background())
	c.cancel = cancel
	c.connectCtx = ctx
	bypass := c.settings.BypassRussian
	// Настройки дозвона снимаем здесь, под замком и разом: правка в окне
	// посреди подключения не должна дать половину старого и половину нового.
	set := c.settings
	c.mu.Unlock()

	// Устаревший список обновляем рядом с подключением, а не перед ним:
	// подключение не должно ждать панель. Свежий список начнёт действовать
	// на новых соединениях, как только скачается.
	if bypass {
		go c.refreshRussian(account, false)
	}

	go c.connect(ctx, account, set)
	return nil
}

func (c *Controller) connect(ctx context.Context, link string, set pcSettings) {
	// Помехи убираем до дозвона: второй VPN перехватывает и само соединение
	// с нодой.
	if !set.AutoFixOff {
		c.setStep(ctx, "prepare")
		c.fixer.pause()
		c.dropDeadProxy()
	}
	if err := c.raise(ctx, link, set); err != nil {
		if ctx.Err() != nil {
			// Человек нажал «отключиться», не дождавшись. Это не ошибка.
			c.finish(ctx, StateIdle, "")
			return
		}
		c.log.add("%s", sayf("logNoConnect", err))
		// Попытка успела обновить кэш до отказа: кончившийся доступ — ровно
		// тот случай, когда нужна свежая ссылка «Продлить».
		c.rememberSubscription(link)
		c.finish(ctx, StateFailed, err.Error())
		// Без туннеля отключённый WARP только отнимает у человека его VPN:
		// возвращаем всё сразу, а не по кнопке «Отключить».
		c.fixer.restore()
	}
}

// raise делает всю работу подключения по шагам.
//
// Путь до подключения у ключа Marvia и у чужой подписки разный, а дальше —
// адаптер, маршруты, мост — общий: обоим нужно одно и то же, поток до цели.
func (c *Controller) raise(ctx context.Context, link string, set pcSettings) error {
	netpath.Enable()
	var (
		dialer       client.Backend
		measurements []client.Measurement

		err error
	)
	c.setStep(ctx, "nodes")
	if foreign.IsForeign(link) {
		dialer, measurements, err = c.raiseForeign(ctx, link, set)
	} else {
		dialer, measurements, err = c.raiseMarvia(ctx, link, set)
	}
	if err != nil {
		return err
	}
	return c.raiseTunnel(ctx, dialer, measurements, set)
}

// raiseForeign — чужая подписка: VLESS, VMess, Trojan и прочее через Xray.
//
// Имена нод разрешаются здесь, до адаптера, и адреса закрепляются в
// настройках: иначе Xray разрешал бы имя ноды при каждом соединении, и
// запрос ушёл бы в туннель, который он же и держит. Подробнее — foreign.Pin.
func (c *Controller) raiseForeign(ctx context.Context, link string, set pcSettings) (client.Backend, []client.Measurement, error) {
	dir, _ := settingsDir()
	sub, _, _, err := foreign.Load(link, foreign.CachePath(dir, link), false)
	if err != nil {
		return nil, nil, err
	}
	if !sub.Expire.IsZero() && time.Now().After(sub.Expire) {
		return nil, nil, client.ErrExpired
	}
	if sub.Total > 0 && sub.Remaining() == 0 {
		return nil, nil, client.ErrQuota
	}
	pinned, _, err := foreign.Resolve(ctx, sub)
	if err != nil {
		return nil, nil, err
	}
	dialer, measurements, err := foreign.Supervise(ctx, pinned, 0, c.events(ctx), foreign.Options{Fragment: set.Fragment})
	if err != nil {
		return nil, measurements, err
	}
	return dialer, measurements, nil
}

// raiseMarvia — свой ключ: VP1 и панель Marvia.
func (c *Controller) raiseMarvia(ctx context.Context, link string, set pcSettings) (client.Backend, []client.Measurement, error) {
	account, err := client.ParseAccountLink(link)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", say("accountLink"), err)
	}
	key, err := vp1.KeyPairFromPrivate(account.PrivateKey)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", say("privateKey"), err)
	}

	// Список нод по возможности берём из кэша: каждый поход в панель — это
	// запрос имени её домена, а он с недавних пор уходит провайдеру открытым
	// текстом. Подробности в internal/client/cache.go.
	// Supervise, а не Connect: за нодой дальше следят и меняют её, если она
	// замолчала. Свой сторож здесь больше не нужен — он умел заметить разрыв,
	// но не умел его вылечить.
	dialer, measurements, err := client.Supervise(ctx, client.ConnectConfig{
		Account:   account,
		Key:       key,
		CachePath: cachePathFor(account.SubscriptionURL),
		Dial:      client.Options{Fragment: set.Fragment, NoQUIC: set.TCPOnly, Fingerprint: set.Fingerprint},
		Log:       c.log.add,
	}, c.events(ctx))

	// По умолчанию продавец узнаёт и о неудачном дозвоне — так панель
	// ставит выше ноды, до которых люди доходят. Человек может оставить
	// замеры на компьютере: выбор ноды от этого не меняется.
	if !set.ReportsOff {
		go func() {
			_ = client.SendReports(context.Background(), account.SubscriptionURL, client.ReportsFrom(measurements))
		}()
	}

	if err != nil {
		return nil, measurements, err
	}

	return dialer, measurements, nil
}

// raiseTunnel поднимает адаптер, маршруты и мост поверх готового подключения.
func (c *Controller) raiseTunnel(ctx context.Context, dialer client.Backend, measurements []client.Measurement, set pcSettings) error {
	// Старый адаптер должен сняться раньше нового. Отмена при этом немедленно
	// закрывает контекст: ждать освобождения маршрутов для отмены незачем.
	c.tunnelMu.Lock()
	defer c.tunnelMu.Unlock()
	c.mu.Lock()
	current := c.connectCtx == ctx && ctx.Err() == nil
	c.mu.Unlock()
	if !current {
		_ = dialer.Close()
		return context.Canceled
	}
	node := dialer.Node()
	ping := latencyOf(measurements, node)
	c.log.add("%s", sayf("logNodePicked", node.Name, ping.Milliseconds()))

	address, err := netip.ParsePrefix(tunnelAddress)
	if err != nil {
		_ = dialer.Close()
		return err
	}
	// Резолвер берём из настроек окна на момент подключения; флаг -dns —
	// запасной, для поддержки и отладки.
	c.mu.Lock()
	dns := c.settings.resolver(c.dns)
	mtu := c.settings.mtu(c.mtu)
	kill := c.settings.KillSwitch
	c.mu.Unlock()
	dnsAddr, err := dnsAddress(dns)
	if err != nil {
		_ = dialer.Close()
		return err
	}

	c.setStep(ctx, "adapter")
	c.log.add("%s", say("logAdapter"))
	adapter, err := wintun.Open(wintun.Config{
		Name:    adapterName,
		MTU:     mtu,
		Address: address,
		DNS:     dnsAddr,
	})
	if err != nil {
		_ = dialer.Close()
		return err
	}

	// Счётчик стоит внутри развилки, а не снаружи: мимо туннеля идёт
	// обычная сеть компьютера, и в расход туннеля она попадать не должна —
	// на телефоне такие подсети до моста не доходят вовсе и тоже не
	// считаются.
	tunnel := tunbridge.Metered(dialer, &c.up, &c.down)
	names, err := c.names(dns, set, tunnel)
	if err != nil {
		_ = adapter.Close()
		_ = dialer.Close()
		return err
	}
	bridge, err := tunbridge.Start(tunbridge.Config{
		Endpoint: adapter.Endpoint(),
		MTU:      mtu,
		Dialer:   tunbridge.WithApps(tunbridge.Split(tunnel, localNetwork, c.around, c.sites), c.appRoute),
		DNS:      dns,
		Names:    names,
		NoIPv6:   set.IPv6Off,
		OnError:  func(err error) { c.log.add("%s", sayf("logConn", err)) },
	})
	if err != nil {
		_ = adapter.Close()
		_ = dialer.Close()
		return fmt.Errorf("%s: %w", say("bridge"), err)
	}
	guard, guardErr := c.raiseGuard(adapter.LUID(), kill)

	c.mu.Lock()
	if c.connectCtx != ctx || ctx.Err() != nil {
		c.mu.Unlock()
		_ = bridge.Close()
		_ = adapter.Close()
		_ = dialer.Close()
		_ = guard.Close()
		return context.Canceled
	}
	c.dialer, c.adapter, c.bridge = dialer, adapter, bridge
	c.guard, c.guardErr = guard, guardErr
	c.node, c.ping = node, ping
	c.dnsInUse = dnsAddr.String()
	c.mtuInUse = mtu
	c.dnsSecureInUse = boolPtr(names != nil)
	c.fragmentInUse = &set.Fragment
	if _, theirs := dialer.(*foreign.Supervisor); !theirs {
		c.tcpOnlyInUse = &set.TCPOnly
		c.fingerprintInUse = &set.Fingerprint
	}
	c.ipv6InUse = boolPtr(!set.IPv6Off)
	c.until, c.limitBytes, c.leftBytes = subscriptionOf(dialer)
	c.known = dialer.Subscription()
	// Про обновление узнаём здесь же: подписка приходит при подключении, а
	// ходить за ней отдельно ради версии — лишний запрос к панели в день.
	if offer, ok := dialer.Subscription().Update("windows", version); ok {
		c.update = offer
	}
	c.state = StateConnected
	c.reason = ""
	c.since = time.Now()
	c.mu.Unlock()

	c.log.add("%s", sayf("logTunnelUp", node.Name))
	go c.selfCheck(ctx, dialer)
	go c.watchQuality(ctx, dialer)
	// Список стран меряется сам, чуть позже: человек, открывший «Серверы»,
	// видит числа сразу. С одной нодой выбирать не из чего — не меряем.
	if len(dialer.Nodes()) > 1 {
		go func() {
			select {
			case <-time.After(4 * time.Second):
				c.StartMeasure()
			case <-ctx.Done():
			}
		}()
	}

	return nil
}

// names — кто ответит на запросы имён: резолвер по HTTPS или nil — открытый
// DNS через туннель, как раньше.
//
// Резолверу отдаётся дозвон туннеля, а не развилка: соединение с ним мимо
// туннеля — это DoH, видный провайдеру, а его блокируют; да и адрес
// резолвера в российском списке оказаться может.
func (c *Controller) names(resolver string, set pcSettings, tunnel tunbridge.Dialer) (tunbridge.Names, error) {
	if !set.encrypted(resolver) {
		c.log.add("%s", say("logNamesPlain"))
		return nil, nil
	}
	servers, _ := securedns.For(resolver)
	r, err := securedns.New(securedns.Config{Servers: servers, Dial: tunbridge.DialAddr(tunnel)})
	if err != nil {
		return nil, err
	}
	c.log.add("%s", sayf("logNamesSecure", servers[0].Name))
	return r, nil
}

// Disconnect убирает туннель и всё, что под него настраивалось, и
// возвращает помехи, остановленные на время подключения.
func (c *Controller) Disconnect() {
	c.mu.Lock()
	c.leaving++
	c.mu.Unlock()
	c.leave()
}

// StartDisconnect — то же, но не дожидаясь уборки: окно сразу видит
// «Отключаюсь», а не висит на запросе, пока снимается адаптер.
func (c *Controller) StartDisconnect() {
	c.mu.Lock()
	c.leaving++
	c.mu.Unlock()
	go c.leave()
}

func (c *Controller) leave() {
	defer func() {
		c.mu.Lock()
		c.leaving--
		c.mu.Unlock()
	}()
	c.teardown()
	c.fixer.restore()
}

// teardown убирает туннель, не возвращая помех: переподключению они снова
// помешали бы, а включать WARP на секунду между двумя туннелями незачем.
func (c *Controller) teardown() {
	c.disconnectMu.Lock()
	defer c.disconnectMu.Unlock()
	c.mu.Lock()
	c.disconnecting = true
	c.connectCtx = nil
	if c.cancel != nil {
		c.cancel()
		c.cancel = nil
	}
	c.mu.Unlock()
	c.tunnelMu.Lock()
	defer c.tunnelMu.Unlock()
	c.mu.Lock()
	bridge, adapter, dialer, guard := c.bridge, c.adapter, c.dialer, c.guard
	if dialer != nil {
		// Подписка за сессию могла обновиться: кнопки продавца остаются от
		// последней, а не от той, что была при подключении.
		c.known = dialer.Subscription()
	}
	c.bridge, c.adapter, c.dialer = nil, nil, nil
	c.guard, c.guardErr = nil, ""
	c.state = StateIdle
	c.reason = ""
	c.node = client.Node{}
	c.dnsInUse = ""
	c.mtuInUse = 0
	c.dnsSecureInUse = nil
	c.fragmentInUse = nil
	c.tcpOnlyInUse = nil
	c.fingerprintInUse = nil
	c.ipv6InUse = nil
	c.until, c.limitBytes, c.leftBytes = "", 0, 0
	c.since = time.Time{}
	c.check, c.checkNode = "", ""
	c.where = WhereView{}
	c.quality.reset()
	c.movedFrom = ""
	c.measured = nil
	c.mu.Unlock()

	// Порядок обратный сборке: сначала перестаём разбирать пакеты, потом
	// снимаем маршруты и убираем адаптер, и только затем рвём туннель.
	// Сколько обманок zapret мост выбросил за сеанс: видно, что он рядом и
	// туннель с ним уживается, — и с чего начинать, если YouTube всё-таки
	// не открылся.
	if n := bridge.Decoys(); n > 0 {
		c.log.add("%s", sayf("logDecoys", n))
	}
	if bridge != nil {
		_ = bridge.Close()
	}
	if adapter != nil {
		if err := adapter.Close(); err != nil {
			c.log.add("%s", sayf("logCleanup", err))
		}
	}
	if dialer != nil {
		_ = dialer.Close()
	}
	// Фильтр — последним: пока снимаются маршруты и адаптер, трафику уже
	// некуда идти, кроме как напрямую, и запрет держит его до конца уборки.
	if guard != nil {
		if err := guard.Close(); err != nil {
			c.log.add("%s", sayf("logCleanup", err))
		}
		c.log.add("%s", say("logKillOff"))
	}
	c.traffic.flush(time.Now(), c.up.Load(), c.down.Load())
	if bridge != nil || adapter != nil {
		c.log.add("%s", say("logTunnelDown"))
	}
	c.mu.Lock()
	c.disconnecting = false
	c.mu.Unlock()
}

// moved — ядро переехало на другую ноду.
//
// Имя переписываем обязательно: иначе окно продолжит показывать ноду, через
// которую трафик давно не идёт. Состояние возвращаем в «подключено» — если до
// этого висело «связь потеряна», то она уже нашлась.
func (c *Controller) moved(ctx context.Context, node client.Node) {
	c.mu.Lock()
	if c.connectCtx != ctx || ctx.Err() != nil {
		c.mu.Unlock()
		return
	}
	if time.Since(c.manualAt) > 20*time.Second && c.node.Address != "" {
		c.movedFrom, c.movedAt = c.node.Title(), time.Now()
	}
	c.node = node
	c.ping = 0
	c.quality.reset()
	// Нода другая — и адрес другой.
	c.where = WhereView{}
	if c.state == StateStalled {
		c.state = StateConnected
		c.reason = ""
	}
	bridge := c.bridge
	c.mu.Unlock()

	// Мост мог выключить датаграммы, решив, что нода их не умеет. Умирающая
	// нода обрывает поток там же, где старая отвечает отказом, так что вывод
	// мог быть про смерть, а не про старость. Новая нода за это не отвечает.
	bridge.NodeChanged()

	c.log.add("%s", sayf("logMoved", node.Title()))
	go c.selfCheck(ctx, c.currentDialer())
}

// stall — нода замолчала, а переехать не на что.
//
// Туннель при этом намеренно не разбираем. Соблазн был — всё равно не
// работает, — но тогда снимутся маршруты, и трафик пойдёт мимо туннеля
// открыто ровно в тот момент, когда человек об этом не знает. Для средства
// обхода блокировок это хуже, чем отсутствие связи.
func (c *Controller) stall(ctx context.Context, code string) {
	// Из ядра приезжает код, а не фраза: оно не знает, на каком языке говорит
	// окно. Фразу подбираем здесь, из своего словаря.
	reason := say("nodeSilent")
	if code != client.TroubleNoNode {
		reason = code
	}

	c.mu.Lock()
	if c.connectCtx != ctx || ctx.Err() != nil {
		c.mu.Unlock()
		return
	}
	if c.state == StateConnected {
		c.state = StateStalled
		c.reason = reason
	}
	c.mu.Unlock()

	c.log.add("%s", reason)
}

// NodeView — одна нода так, как её видит покупатель в окне.
type NodeView struct {
	SetupMS int64  `json:"setup_ms"`
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	Country string `json:"country,omitempty"`
	MS      int64  `json:"ms"`
	Alive   bool   `json:"alive"`

	// Current и Chosen — разные вещи, и обе нужны. Выбранная страна могла
	// замолчать, надзор уехал на живую, и показывать в этом случае одну
	// «выбранную» значило бы врать про то, куда идёт трафик.
	Current bool `json:"current"`
	Chosen  bool `json:"chosen"`

	// Pending — замер идёт, а до этой ноды ещё не дошёл: «меряю», а не
	// «не отвечает».
	Pending bool `json:"pending,omitempty"`

	// Failed — ноду мерили, и она не ответила. Без этого окно путало её с
	// той, которую ещё не мерили, и писало «не мерили» о мёртвой.
	Failed bool `json:"failed,omitempty"`
}

// NodesView — список нод для окна и идёт ли сейчас замер.
type NodesView struct {
	Nodes     []NodeView `json:"nodes"`
	Measuring bool       `json:"measuring"`
}

// Nodes отдаёт список нод с тем, что уже намерено.
func (c *Controller) Nodes() NodesView {
	c.mu.Lock()
	dialer := c.dialer
	measuring := c.measuring
	got := make([]client.Measurement, 0, len(c.measured)+1)
	have := make(map[int64]bool, len(c.measured))
	for id, m := range c.measured {
		got = append(got, m)
		have[id] = true
	}
	c.mu.Unlock()
	if dialer == nil {
		return NodesView{}
	}
	// Текущая нода известна и без замера — по живому соединению.
	if cur := dialer.Measurement(); !have[cur.Node.ID] {
		got = append(got, cur)
		have[cur.Node.ID] = true
	}
	views := nodeViews(dialer, dialer.Nodes(), got)
	if measuring {
		c.mu.Lock()
		for i := range views {
			views[i].Pending = !c.fresh[views[i].ID]
		}
		c.mu.Unlock()
	}
	return NodesView{Nodes: views, Measuring: measuring}
}

// measureLimit — дольше замер списка не идёт: нода, не ответившая за это
// время, для человека и так «не отвечает».
const measureLimit = 20 * time.Second

// streamMeasurer — ядро, которое отдаёт замеры по одному, а не скопом.
type streamMeasurer interface {
	MeasureEach(ctx context.Context, each func(client.Measurement))
}

// StartMeasure запускает замер нод в фоне.
//
// Раньше окно ждало ответа на «Перемерить», пока не доберутся все ноды и
// не замерится скорость, — до пятнадцати секунд «меряю…» без единого
// числа. Теперь каждая нода появляется в списке, как только ответила, а
// окно спрашивает список, пока замер идёт. Повторное нажатие во время
// замера ничего не делает: второй замер поверх первого — это лишние
// рукопожатия к тем же адресам.
func (c *Controller) StartMeasure() {
	c.mu.Lock()
	dialer, base := c.dialer, c.connectCtx
	if dialer == nil || base == nil || c.measuring {
		c.mu.Unlock()
		return
	}
	c.measuring = true
	if c.measured == nil {
		c.measured = map[int64]client.Measurement{}
	}
	c.fresh = map[int64]bool{}
	c.mu.Unlock()

	go func() {
		ctx, cancel := context.WithTimeout(base, measureLimit)
		defer cancel()
		keep := func(m client.Measurement) {
			c.mu.Lock()
			if c.dialer == dialer && c.measured != nil {
				c.measured[m.Node.ID] = m
				c.fresh[m.Node.ID] = true
			}
			c.mu.Unlock()
		}
		if s, ok := dialer.(streamMeasurer); ok {
			s.MeasureEach(ctx, keep)
		} else {
			for _, m := range dialer.Measure(ctx) {
				keep(m)
			}
		}
		c.mu.Lock()
		// Кто не ответил за отведённое время — «не ответила», а не старое
		// число, которое уже неправда.
		if c.dialer == dialer && c.measured != nil {
			for _, n := range dialer.Nodes() {
				if !c.fresh[n.ID] {
					c.measured[n.ID] = client.Measurement{Node: n, Err: context.DeadlineExceeded}
				}
			}
		}
		c.measuring = false
		c.mu.Unlock()
	}()
}

// SelectNode переводит туннель на выбранную ноду. Ноль — обратно к автовыбору.
func (c *Controller) SelectNode(ctx context.Context, id int64) error {
	c.mu.Lock()
	dialer := c.dialer
	c.mu.Unlock()
	if dialer == nil {
		return errors.New(say("notConnected"))
	}

	c.mu.Lock()
	c.manualAt = time.Now()
	c.mu.Unlock()

	if err := dialer.Select(ctx, id); err != nil {
		return err
	}

	c.mu.Lock()
	c.node = dialer.Node()
	c.quality.reset()
	c.mu.Unlock()

	c.log.add("%s", sayf("logChosen", dialer.Node().Title()))
	return nil
}

func nodeViews(dialer client.Backend, nodes []client.Node, measured []client.Measurement) []NodeView {
	byID := make(map[int64]client.Measurement, len(measured))
	for _, m := range measured {
		byID[m.Node.ID] = m
	}

	current := dialer.Node().ID
	chosen := dialer.Selected()

	out := make([]NodeView, 0, len(nodes))
	for _, n := range nodes {
		v := NodeView{
			ID:      n.ID,
			Name:    n.Name,
			Country: n.Country,
			Current: n.ID == current,
			Chosen:  chosen != 0 && n.ID == chosen,
		}
		if m, ok := byID[n.ID]; ok {
			v.Alive = m.OK()
			v.Failed = !m.OK()
			v.MS = m.PingMS()
			if m.OK() {
				v.SetupMS = max(1, m.Latency.Milliseconds())
			}
		} else if n.ID == current {
			// Текущую не мерили, но знаем точно: через неё идёт трафик.
			v.Alive = true
		}
		out = append(out, v)
	}
	return out
}

// recovered — нода снова отвечает после того, как её объявили молчащей.
//
// Без этого из «связь потеряна» не было выхода, кроме переезда. А самый
// обычный случай — не мёртвая нода, а пропавшая сеть: тогда переезжать некуда,
// и надпись оставалась навсегда, в том числе после возвращения сети. Хуже
// того, кнопка «Подключиться» в этом состоянии тоже не помогала: туннель
// считался поднятым.
func (c *Controller) recovered(ctx context.Context) {
	c.mu.Lock()
	if c.connectCtx != ctx || ctx.Err() != nil {
		c.mu.Unlock()
		return
	}
	if c.state == StateStalled {
		c.state = StateConnected
		c.reason = ""
	}
	c.mu.Unlock()

	c.log.add("%s", say("logNodeBack"))
}

// setStep отмечает шаг подключения — только своего: отменённое подключение
// не должно переписывать шаг следующего.
func (c *Controller) setStep(ctx context.Context, step string) {
	c.mu.Lock()
	if c.connectCtx == ctx {
		c.step = step
	}
	c.mu.Unlock()
}

func (c *Controller) finish(ctx context.Context, state State, reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.connectCtx != ctx {
		return
	}
	c.state = state
	c.reason = redact.Secrets(reason)
	if c.cancel != nil {
		c.cancel()
	}
	c.cancel = nil
	c.connectCtx = nil
}

func (c *Controller) events(ctx context.Context) client.Events {
	return client.Events{
		OnSwitch:    func(node client.Node) { c.moved(ctx, node) },
		OnTrouble:   func(reason string) { c.stall(ctx, reason) },
		OnRecovered: func() { c.recovered(ctx) },
	}
}

// latencyOf достаёт задержку выбранной ноды из замеров.
func latencyOf(measurements []client.Measurement, node client.Node) time.Duration {
	for _, m := range measurements {
		if m.Node.ID == node.ID && m.OK() {
			return time.Duration(m.PingMS()) * time.Millisecond
		}
	}
	return 0
}

// settingsDir — каталог настроек: %APPDATA%\Marvia.
func settingsDir() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "Marvia"), nil
}

// legacyAccountPath — где ключ лежал до переименования.
//
// Ключ доступа хранится у покупателя на диске, а не у нас в базе. Просто
// сменить имя каталога значило бы, что после обновления человек открывает окно
// и видит пустое поле там, где вчера был рабочий доступ, — и идёт к продавцу с
// «у меня всё пропало».
func legacyAccountPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "Veil", "account"), nil
}

// accountPath — где лежит сохранённая ссылка доступа.
func accountPath() (string, error) {
	dir, err := settingsDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "account"), nil
}

// cachePathFor — кэш подписки, рядом со ссылкой доступа, свой у каждого
// ключа: смена рабочего ключа туда-обратно не должна стоить похода в панель.
//
// Пустая строка означает, что каталога настроек нет: тогда подключаемся без
// кэша, как раньше, — это медленнее и заметнее, но работает.
func cachePathFor(subURL string) string {
	dir, err := settingsDir()
	if err != nil {
		return ""
	}
	return client.CacheFile(dir, subURL)
}

// adoptLegacyCache переносит кэш подписки из общего файла прежних версий
// в свой файл рабочего ключа — один раз, при первом запуске новой версии.
//
// Без этого после обновления кнопок продавца в окне не было бы до первого
// подключения, а само подключение сходило бы в панель за списком, который
// уже лежит на диске. LoadCache сверяет отпечаток адреса подписки, так что
// кэш другого продавца под чужое имя не переедет.
func adoptLegacyCache(link string) {
	if link == "" || foreign.IsForeign(link) {
		return
	}
	account, err := client.ParseAccountLink(link)
	if err != nil {
		return
	}
	dir, err := settingsDir()
	if err != nil {
		return
	}
	legacy := filepath.Join(dir, "subscription.json")
	fresh := cachePathFor(account.SubscriptionURL)
	if _, err := os.Stat(fresh); err == nil {
		return
	}
	if _, err := client.LoadCache(legacy, account.SubscriptionURL); err != nil {
		return
	}
	_ = os.Rename(legacy, fresh)
}

func readAccount() string {
	path, err := accountPath()
	if err != nil {
		return ""
	}
	if raw, err := os.ReadFile(path); err == nil {
		return strings.TrimSpace(string(raw))
	}

	// Нового ключа нет — смотрим, не остался ли он от прежнего имени. Найдя,
	// сразу перекладываем, чтобы этот путь понадобился ровно один раз.
	legacy, err := legacyAccountPath()
	if err != nil {
		return ""
	}
	raw, err := os.ReadFile(legacy)
	if err != nil {
		return ""
	}
	link := strings.TrimSpace(string(raw))
	_ = writeAccount(link)
	return link
}

// writeAccount сохраняет ссылку доступа только для владельца.
//
// В ней личный ключ покупателя. Права 600 на Windows работают иначе, чем на
// Unix, но каталог настроек пользователя и так закрыт от других учётных
// записей — а вот от случайного чужого взгляда в общей папке это защищает.
func writeAccount(link string) error {
	path, err := accountPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(link), 0o600)
}

// subscriptionOf достаёт из дозвона то, что показывает окно: до какого числа
// оплачено и сколько трафика осталось.
//
// Человек заплатил и вправе это видеть, не спрашивая продавца. А продавец
// вправе не отвечать на такое вручную каждому.
func subscriptionOf(dialer client.Backend) (until string, limit, left int64) {
	if dialer == nil {
		return "", 0, 0
	}
	sub := dialer.Subscription()
	if at, set := sub.Until(); set {
		until = at.Local().Format("02.01.2006")
	}
	return until, sub.TrafficLimit, sub.Remaining()
}

// sinceUnix — время в секундах для окна; ноль, если туннеля нет.
func sinceUnix(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

// boolPtr — адрес копии значения: состояние «настройка ещё не применялась» —
// это nil, а не false.
func boolPtr(v bool) *bool { return &v }
