// Команда marvia-node — нода Veil.
//
// Принимает соединения, маскируясь под обычный сайт на 443 порту: тот, кто
// пришёл без правильного ключа, получает настоящую веб-страницу, а не разрыв
// соединения. Внутри TLS работает протокол VP1, внутри него — логические
// потоки пользователя.
//
// Нода намеренно ничего не знает о людях. У неё есть публичные ключи, лимиты
// к ним и счётчики трафика. Кто куда ходил — не пишется никуда: изъятие
// сервера не должно выдавать пользователей.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/jytt8u/marvia/internal/connguard"
	"github.com/jytt8u/marvia/internal/envvar"
	"github.com/jytt8u/marvia/internal/fallback"
	"github.com/jytt8u/marvia/internal/nodesync"
	"github.com/jytt8u/marvia/internal/proxyproto"
	"github.com/jytt8u/marvia/internal/redact"
	"github.com/jytt8u/marvia/internal/transport"
	"github.com/jytt8u/marvia/internal/updater"
	"github.com/jytt8u/marvia/internal/users"
	"github.com/jytt8u/marvia/internal/vp1"
)

const (
	// requestTimeout — сколько ждём от клиента адрес назначения.
	requestTimeout = 10 * time.Second

	// dialTimeout — сколько ждём соединения с целевым хостом.
	dialTimeout = 15 * time.Second

	// usersReloadInterval — как часто проверяем, не изменился ли список
	// пользователей. Панель или скрипт продавца просто перезаписывают файл.
	usersReloadInterval = 5 * time.Second

	// usageFlushInterval — как часто расход выгружается на диск и сверяется
	// с квотой. Чаще — лишние записи на диск, реже — больше трафика можно
	// проскочить сверх лимита при внезапной перезагрузке.
	usageFlushInterval = 30 * time.Second

	// meterInterval — как часто счётчики соединения переносятся пользователю.
	// От него зависит, насколько быстро сработает исчерпание квоты.
	meterInterval = 5 * time.Second

	// panelSyncInterval — как часто нода сверяется с панелью: забирает список
	// и сдаёт расход. Отзыв доступа доезжает до ноды за это время.
	panelSyncInterval = 15 * time.Second

	// panelFirstFetchTimeout — сколько ждём панель на старте.
	panelFirstFetchTimeout = 30 * time.Second
)

// version подставляется при сборке: -ldflags "-X main.version=v0.1.0".
var version = "dev"

// deps — всё, что нужно обработчику одного соединения.
type deps struct {
	static   vp1.KeyPair
	guard    *vp1.ReplayGuard
	registry *users.Registry // nil — пускать любого, кто знает публичный ключ
	fallback *fallback.Handler
}

type serverOptions struct {
	listenAddr      string
	keyStr          string
	keyFile         string
	usersFile       string
	panelURL        string
	panelToken      string
	usageFile       string
	certFile        string
	keyPEMFile      string
	selfSigned      string
	realityDest     string
	realitySNI      string
	realityKey      string
	realityShortIDs string
	wsPath          string
	plain           bool
	coverSite       string
	coverTitle      string

	// quic поднимает второй слушатель на том же порту, но по UDP.
	quic     bool
	quicAddr string

	// relay — вход каскада: принимать и перекладывать на ноду, которую
	// назовёт панель (relay.go).
	relay bool
}

func main() {
	var opts serverOptions

	flag.StringVar(&opts.listenAddr, "listen", ":8443", "адрес и порт для приёма соединений")
	flag.StringVar(&opts.keyStr, "key", "", "приватный ключ ноды в base64")
	flag.StringVar(&opts.keyFile, "key-file", "", "файл с приватным ключом ноды")
	flag.StringVar(&opts.usersFile, "users", "", "файл пользователей: JSON с лимитами либо просто список ключей (пусто — пускать всех)")
	flag.StringVar(&opts.usageFile, "usage", "", "файл для расхода трафика (по умолчанию — рядом с файлом пользователей)")
	flag.StringVar(&opts.panelURL, "panel", "", "адрес панели, например https://panel.example.com (вместо -users)")
	flag.StringVar(&opts.panelToken, "panel-token", "", "токен этой ноды (по умолчанию — из MARVIA_NODE_TOKEN)")

	flag.StringVar(&opts.certFile, "tls-cert", "", "файл сертификата PEM")
	flag.StringVar(&opts.keyPEMFile, "tls-key", "", "файл приватного ключа сертификата PEM")
	flag.StringVar(&opts.selfSigned, "tls-self-signed", "", "выпустить самоподписанный сертификат на это имя (только для отладки)")
	flag.BoolVar(&opts.plain, "plain", false, "работать без TLS-маскировки (для отладки)")

	flag.StringVar(&opts.realityDest, "reality-dest", "", "настоящий сайт для маскировки REALITY, например www.samsung.com:443")
	flag.StringVar(&opts.realitySNI, "reality-sni", "", "имена в SNI через запятую (по умолчанию — хост из -reality-dest)")
	flag.StringVar(&opts.realityKey, "reality-key", "", "приватный ключ REALITY в base64 (по умолчанию — из MARVIA_REALITY_KEY)")
	flag.StringVar(&opts.realityShortIDs, "reality-short-id", "", "короткие идентификаторы клиентов через запятую, шестнадцатеричные")

	flag.StringVar(&opts.wsPath, "ws-path", "", "путь туннеля WebSocket, например /assets/app.js (режим для работы за CDN)")

	flag.StringVar(&opts.coverSite, "cover", "", "адрес настоящего сайта для неопознанных гостей, например https://example.org")
	flag.BoolVar(&opts.quic, "quic", false, "принимать ещё и по QUIC на том же порту (нужен -tls-cert)")
	flag.StringVar(&opts.quicAddr, "quic-listen", "", "отдельный адрес для QUIC (по умолчанию — тот же, что у -listen)")

	flag.StringVar(&opts.coverTitle, "cover-title", "", "если сайт-прикрытие не задан, отдавать заглушку с таким заголовком")
	flag.BoolVar(&opts.relay, "relay", false, "работать входом каскада: перекладывать соединения на ноду, которую назовёт панель (нужен -panel)")

	showVersion := flag.Bool("version", false, "показать версию и выйти")
	installUpdater := flag.Bool("install-updater", false, "поставить службу обновления по кнопке в панели и выйти (нужен root)")

	flag.Parse()

	if *showVersion {
		fmt.Println("marvia-node", version)
		return
	}
	if *installUpdater {
		if err := updater.Install(); err != nil {
			fmt.Fprintf(os.Stderr, "служба обновления: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if opts.relay {
		if err := runRelay(opts); err != nil {
			fmt.Fprintf(os.Stderr, "ошибка: %v\n", err)
			os.Exit(1)
		}
		return
	}
	if err := run(opts); err != nil {
		fmt.Fprintf(os.Stderr, "ошибка: %v\n", err)
		os.Exit(1)
	}
}

func run(opts serverOptions) error {
	static, err := loadStaticKey(opts.keyStr, opts.keyFile)
	if err != nil {
		return err
	}

	cover, err := loadFallback(opts.coverSite, opts.coverTitle)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	registry, usagePath, panel, err := setupUsers(ctx, opts)
	if err != nil {
		return err
	}

	ln, err := listenTCP(opts.listenAddr)
	if err != nil {
		return fmt.Errorf("прослушивание %s: %w", opts.listenAddr, err)
	}
	defer ln.Close()

	// Пределы на приём стоят первыми, над сырым TCP: флуд соединениями
	// отсекается раньше, чем нода потратит на него чтение заголовка или
	// рукопожатие TLS. Входы каскада от предела частоты освобождены — за
	// адресом входа приходят все его покупатели. Поэтому обёртка стоит до
	// proxyproto: после него адрес входа уже подменён адресом покупателя, и
	// вход было бы не узнать.
	//
	// За CDN (-ws-path) от предела частоты освобождены все: каждый покупатель
	// приходит с адреса узла CDN, и предел одного адреса резал бы сотни людей
	// за этим узлом. Флуд там принимает на себя CDN, а ноде остаётся общий
	// предел открытых соединений.
	var exempt func(net.IP) bool
	switch {
	case opts.wsPath != "":
		exempt = func(net.IP) bool { return true }
	case panel != nil:
		exempt = panel.TrustsRelay
	}
	guard := connguard.Listen(ln, connguard.Config{Exempt: exempt})
	if panel != nil {
		// Пределы и закрытые адреса задаёт продавец в панели; без панели —
		// значения по умолчанию.
		panel.Guard(guard)
	}
	ln = guard

	// Адрес покупателя, пришедшего через вход каскада, нода узнаёт из
	// заголовка, который вход шлёт первым (internal/proxyproto). Читается он
	// до маскировки: вход перекладывает сырой поток, и TLS начинается уже за
	// заголовком. Каким адресам верить, говорит панель.
	if panel != nil {
		ln = proxyproto.Listen(ln, panel.TrustsRelay)
	}

	addr := ln.Addr()
	switch {
	case opts.wsPath != "":
		ln, err = listenWS(ln, opts, cover)
		if err != nil {
			return err
		}
	case opts.plain:
		// маскировки нет
	case opts.realityDest != "":
		ln, err = listenReality(ln, opts)
		if err != nil {
			return err
		}
	default:
		cert, err := loadCertificate(opts)
		if err != nil {
			return err
		}
		ln = transport.Listen(ln, transport.ServerConfig{Certificate: cert})
	}

	log.Printf("marvia-node %s слушает %s", version, addr)
	log.Printf("публичный ключ ноды: %s", vp1.EncodeKey(static.Public))
	if opts.plain {
		log.Printf("ВНИМАНИЕ: режим -plain, маскировки нет — трафик опознаётся DPI")
	}
	if registry == nil {
		log.Printf("ВНИМАНИЕ: список пользователей не задан, пускаем любого, кто знает публичный ключ")
	} else {
		log.Printf("пользователей: %d, расход пишется в %s", registry.Len(), usagePath)
	}
	if cover == nil && opts.realityDest == "" && opts.wsPath == "" {
		log.Printf("ВНИМАНИЕ: сайт-прикрытие не задан — неопознанные соединения будут рваться, что заметно сканерам")
	}
	if cover == nil && opts.realityDest != "" {
		// Под REALITY прикрытие обеспечивает сам транспорт: чужой гость
		// уходит на настоящий сайт, не доходя до нашего кода. Флаг -cover
		// здесь нужен только для редкого случая, когда клиент прошёл
		// проверку REALITY, но говорит внутри что-то неопознанное.
		log.Printf("прикрытие обеспечивает REALITY: неопознанные гости уходят на %s", opts.realityDest)
	}

	d := deps{static: static, guard: vp1.NewReplayGuard(vp1.ClockSkew), registry: registry, fallback: cover}

	// QUIC — рядом, а не вместо.
	//
	// Тот же порт, но по UDP, те же клиенты и тот же протокол внутри. Смысл в
	// двух вещах: на сети с потерями один потерянный пакет не тормозит весь
	// туннель разом, а при переходе из вайфая в мобильный интернет соединение
	// переезжает вместо того, чтобы умереть и переподключаться на глазах.
	//
	// Включается отдельно и TCP не заменяет: UDP режут целыми сетями, а в
	// белых списках его нет вовсе. Кто не дозвонится по UDP, придёт по TCP.
	quicLn, err := listenQUIC(opts)
	if err != nil {
		return err
	}
	if quicLn != nil {
		defer quicLn.Close()
		log.Printf("quic слушает %s по UDP", quicLn.Addr())

		go func() {
			for {
				conn, err := quicLn.Accept()
				if err != nil {
					return
				}
				go serve(conn, d)
			}
		}()
	}

	// Закрытие слушателей по сигналу разблокирует Accept.
	go func() {
		<-ctx.Done()
		log.Printf("завершаем работу")
		_ = ln.Close()
		if quicLn != nil {
			_ = quicLn.Close()
		}
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			log.Printf("accept: %v", err)
			continue
		}
		go serve(conn, d)
	}
}

// setupUsers поднимает реестр пользователей и фоновые задачи вокруг него.
//
// Источник списка — либо панель, либо локальный файл. Панель приоритетнее:
// если она задана, файл не читается вовсе.
func setupUsers(ctx context.Context, opts serverOptions) (*users.Registry, string, *nodesync.Client, error) {
	if opts.panelURL != "" {
		return setupPanelUsers(ctx, opts)
	}
	if opts.usersFile == "" {
		return nil, "", nil, nil
	}

	list, err := users.LoadUsers(opts.usersFile)
	if err != nil {
		return nil, "", nil, err
	}
	registry, err := users.NewRegistry(list)
	if err != nil {
		return nil, "", nil, err
	}

	usagePath := opts.usageFile
	if usagePath == "" {
		usagePath = opts.usersFile + ".usage.json"
	}

	saved, err := users.LoadUsage(usagePath)
	if err != nil {
		// Битый файл расхода — не повод не запускаться: лучше потерять
		// статистику, чем оставить всех клиентов без связи.
		log.Printf("расход не восстановлен (%v), счётчики начнутся с нуля", err)
	} else {
		registry.RestoreUsage(saved)
	}

	go registry.WatchUsers(ctx, opts.usersFile, usersReloadInterval, func(err error, count int) {
		if err != nil {
			log.Printf("список пользователей не перечитан, работаем по старому: %v", err)
			return
		}
		log.Printf("список пользователей перечитан: %d записей", count)
	})

	go registry.PersistUsage(ctx, usagePath, usageFlushInterval, func(err error) {
		log.Printf("не удалось сохранить расход: %v", err)
	})

	return registry, usagePath, nil, nil
}

// setupPanelUsers берёт список пользователей у панели.
//
// Локальный снимок расхода нужен и в этом режиме: между перезапуском ноды и
// первой успешной синхронизацией она должна знать, кто сколько израсходовал,
// иначе после каждой перезагрузки квоты начинались бы заново.
func preparePanelUsers(ctx context.Context, opts serverOptions) (*users.Registry, string, *nodesync.Client, error) {
	token := opts.panelToken
	if token == "" {
		token = envvar.Get("MARVIA_NODE_TOKEN")
	}
	if token == "" {
		return nil, "", nil, errors.New("не задан токен ноды: укажи -panel-token или MARVIA_NODE_TOKEN")
	}

	// Имена прикрытия нода сообщает панели сама: иначе один и тот же список
	// пришлось бы держать руками и здесь флагом, и там полем, а разойдясь,
	// они молча ломают подключение — клиент стучится именем, которого нода
	// уже не принимает.
	usagePath := opts.usageFile
	if usagePath == "" {
		usagePath = "veil-node.usage.json"
	}
	saved, usageErr := users.LoadUsage(usagePath)
	if usageErr != nil {
		return nil, "", nil, fmt.Errorf("расход не восстановлен: %w", usageErr)
	}
	client := nodesync.New(opts.panelURL, token).WithSnapshot(usagePath+".access").
		WithCoverNames(splitList(opts.realitySNI)).
		WithBuild(version, runtime.GOARCH)

	// Первый список забираем синхронно: стартовать, не зная пользователей,
	// значит на несколько секунд открыть ноду для всех подряд.
	first, cancel := context.WithTimeout(ctx, panelFirstFetchTimeout)
	list, restored, offline, err := client.Bootstrap(first, saved)
	cancel()

	switch {
	case errors.Is(err, nodesync.ErrNodeDisabled):
		// Продавец выключил ноду в панели — это решение, а не сбой, и он вправе
		// отменить его одним нажатием. Поэтому поднимаемся с пустым списком:
		// никого не пускаем, но живём и продолжаем спрашивать панель. Включат
		// обратно — начнём обслуживать сами, без похода по SSH.
		//
		// Раньше нода на этом ответе выходила с ошибкой, systemd поднимал её
		// заново, и всё повторялось по кругу. На живой машине счётчик дошёл до
		// семи с лишним тысяч перезапусков, и каждый бил запросом по панели.
		log.Printf("нода выключена в панели: поднимаюсь, никого не пускаю, жду включения")
		list = nil

	case err != nil:
		return nil, "", nil, fmt.Errorf("панель %s: %w", redact.Secrets(opts.panelURL), redact.Wrap(err))
	}

	registry, err := users.NewRegistry(list)
	if err != nil {
		return nil, "", nil, err
	}

	registry.RestoreUsage(restored)
	if offline {
		log.Printf("панель недоступна: восстановлен снимок доступа, действующий не более 24 часов")
	}
	if err := client.SnapshotError(); err != nil {
		log.Printf("снимок доступа не сохранён: %v", err)
	}
	return registry, usagePath, client, nil
}

func setupPanelUsers(ctx context.Context, opts serverOptions) (*users.Registry, string, *nodesync.Client, error) {
	registry, usagePath, client, err := preparePanelUsers(ctx, opts)
	if err != nil {
		return nil, "", nil, err
	}
	go client.Run(ctx, registry, panelSyncInterval, panelEvents())

	go registry.PersistUsage(ctx, usagePath, usageFlushInterval, func(err error) {
		log.Printf("не удалось сохранить расход: %v", err)
	})

	return registry, usagePath, client, nil
}

// panelEvents — что нода пишет в журнал о синхронизации с панелью и как
// передаёт просьбу обновиться. Общее для ноды и входа каскада.
func panelEvents() nodesync.Events {
	return nodesync.Events{
		OnUsers: func(count int) {
			if count == 0 {
				log.Printf("панель вернула пустой список пользователей")
			}
		},
		OnError: func(err error) {
			logPanelSyncError(err)
		},
		OnDisabled: func() {
			// Выключили в середине жизни — та же реакция, что и на старте:
			// реестр уже очищен, никого не пускаем, ждём включения. Строка одна
			// на переход, а не каждые 15 секунд.
			log.Printf("нода выключена в панели: очищаю список, никого не пускаю, жду включения")
		},
		OnEnabled: func() {
			log.Printf("нода снова включена в панели: возобновляю обслуживание")
		},
		// release, а не target: в коде ноды target — это куда шёл покупатель,
		// и проверка журнала (internal/redact) такие имена не пропускает.
		OnUpgrade: func(release string) {
			// Просьба кладётся рядом с бинарником: это каталог ноды, куда у
			// неё есть право писать, и его же сторожит служба обновления.
			home := nodeHome()
			if err := updater.Request(home); err != nil {
				log.Printf("панель просит обновиться до %s, но просьба не легла в %s: %v", release, home, err)
				return
			}
			log.Printf("панель просит обновиться до %s: просьба передана службе обновления", release)
		},
	}
}

func logPanelSyncError(err error) {
	log.Printf("синхронизация с панелью не удалась, работаем по последнему списку: %s", redact.Secrets(err.Error()))
}

// nodeHome — каталог ноды: там, где лежит её бинарник. Туда у ноды есть
// право писать, и его же сторожит служба обновления.
func nodeHome() string {
	exe, err := os.Executable()
	if err != nil {
		return "."
	}
	return filepath.Dir(exe)
}

// listenWS поднимает транспорт WebSocket — режим для работы за CDN.
//
// Смысл режима не в самом WebSocket, а в том, что настоящий адрес ноды
// перестаёт существовать для цензора: пользователь подключается к адресам
// CDN, за которыми живут миллионы обычных сайтов. Ковровая блокировка
// диапазонов хостингов такую ноду не задевает.
//
// Сертификат нужен не всегда. CDN сам занимается TLS перед пользователем, а
// до ноды может ходить как по HTTPS, так и по обычному HTTP — второе выбирают,
// когда нода стоит в закрытом периметре или туннеле до CDN.
func listenWS(inner net.Listener, opts serverOptions, cover *fallback.Handler) (net.Listener, error) {
	if opts.realityDest != "" {
		return nil, errors.New("-ws-path и -reality-dest вместе не работают: CDN расшифровывает TLS у себя, и REALITY через него невозможен")
	}

	cfg := transport.WSConfig{Path: opts.wsPath}
	if cover != nil {
		cfg.Cover = cover.Handler()
	}

	switch {
	case opts.plain:
		log.Printf("транспорт WebSocket на пути %s, без TLS — только за CDN или обратным прокси", opts.wsPath)
	default:
		cert, err := loadCertificate(opts)
		if err != nil {
			return nil, err
		}
		cfg.Certificate = &cert
		log.Printf("транспорт WebSocket на пути %s, поверх TLS", opts.wsPath)
	}

	listener, err := transport.ListenWS(inner, cfg)
	if err != nil {
		return nil, fmt.Errorf("транспорт WebSocket: %w", err)
	}
	if cover == nil {
		log.Printf("ВНИМАНИЕ: сайт-прикрытие не задан — на все прочие пути нода отвечает 404, что заметно при переборе")
	}
	return listener, nil
}

// listenReality поднимает маскировку REALITY.
//
// В этом режиме нода не предъявляет собственный сертификат: для всех, кто не
// доказал право входа, она становится прозрачным зеркалом чужого сайта. Свой
// домен и сертификат при этом не нужны вовсе.
func listenReality(inner net.Listener, opts serverOptions) (net.Listener, error) {
	if opts.certFile != "" || opts.selfSigned != "" {
		return nil, errors.New("-reality-dest и обычный TLS-сертификат вместе не работают: выбери что-то одно")
	}

	keyStr := opts.realityKey
	if keyStr == "" {
		keyStr = envvar.Get("MARVIA_REALITY_KEY")
	}
	if keyStr == "" {
		return nil, errors.New("не задан ключ REALITY: укажи -reality-key или MARVIA_REALITY_KEY (выпустить: marvia-keygen -reality)")
	}
	key, err := vp1.DecodeKey(strings.TrimSpace(keyStr))
	if err != nil {
		return nil, fmt.Errorf("ключ REALITY: %w", err)
	}

	names := splitList(opts.realitySNI)
	if len(names) == 0 {
		// Умолчание безопасное: имя берём из самого адреса прикрытия, ровно
		// то, что клиент и напишет в SNI.
		host, _, splitErr := net.SplitHostPort(opts.realityDest)
		if splitErr != nil {
			return nil, fmt.Errorf("не разобрать -reality-dest %q: %w", opts.realityDest, splitErr)
		}
		names = []string{host}
	}

	listener, err := transport.ListenReality(inner, transport.RealityConfig{
		Dest:        opts.realityDest,
		ServerNames: names,
		PrivateKey:  key,
		ShortIDs:    splitList(opts.realityShortIDs),
	})
	if err != nil {
		return nil, fmt.Errorf("маскировка REALITY: %w", err)
	}

	pair, err := vp1.KeyPairFromPrivate(key)
	if err != nil {
		return nil, err
	}
	log.Printf("маскировка REALITY: прикрытие %s, SNI %s", opts.realityDest, strings.Join(names, ", "))
	log.Printf("публичный ключ REALITY (pbk для клиентов): %s", vp1.EncodeKey(pair.Public))

	return listener, nil
}

// splitList разбирает список, заданный через запятую.
func splitList(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func loadCertificate(opts serverOptions) (tls.Certificate, error) {
	switch {
	case opts.certFile != "" && opts.keyPEMFile != "":
		return transport.LoadCertificate(opts.certFile, opts.keyPEMFile)
	case opts.selfSigned != "":
		log.Printf("ВНИМАНИЕ: самоподписанный сертификат на %s — только для отладки", opts.selfSigned)
		return transport.SelfSignedCertificate(opts.selfSigned)
	default:
		return tls.Certificate{}, errors.New("нужен сертификат: укажи -tls-cert и -tls-key, либо -tls-self-signed для отладки, либо -plain чтобы работать без маскировки")
	}
}

func loadFallback(site, title string) (*fallback.Handler, error) {
	if site != "" {
		return fallback.NewReverseProxy(site)
	}
	if title != "" {
		return fallback.NewStatic(title, "This site is under construction."), nil
	}
	return nil, nil
}

// loadStaticKey достаёт приватный ключ из флага, файла или переменной
// окружения MARVIA_SERVER_KEY.
//
// Порядок такой не случайно: ключ во флаге виден в выводе ps любому
// пользователю машины. Для боевой ноды — файл или переменная окружения.
func loadStaticKey(keyStr, keyFile string) (vp1.KeyPair, error) {
	if keyStr == "" {
		keyStr = envvar.Get("MARVIA_SERVER_KEY")
	}
	if keyStr == "" && keyFile != "" {
		raw, err := os.ReadFile(keyFile)
		if err != nil {
			return vp1.KeyPair{}, fmt.Errorf("чтение %s: %w", keyFile, err)
		}
		keyStr = strings.TrimSpace(string(raw))
	}
	if keyStr == "" {
		return vp1.KeyPair{}, errors.New("не задан приватный ключ: укажи -key, -key-file или MARVIA_SERVER_KEY (сгенерировать: marvia-keygen)")
	}

	priv, err := vp1.DecodeKey(strings.TrimSpace(keyStr))
	if err != nil {
		return vp1.KeyPair{}, fmt.Errorf("приватный ключ: %w", err)
	}
	return vp1.KeyPairFromPrivate(priv)
}

// listenQUIC поднимает слушателя QUIC, если продавец его включил.
//
// Возвращает nil без ошибки, когда QUIC не просили: это не поломка, а обычный
// случай — большинство нод живут по TCP под REALITY.
func listenQUIC(opts serverOptions) (*transport.QUICListener, error) {
	if !opts.quic {
		return nil, nil
	}

	// Настоящий сертификат обязателен, и вот почему.
	//
	// В QUIC нет чужого рукопожатия, за которое можно спрятаться: REALITY
	// зеркалит TCP-хендшейк настоящего сайта, а здесь зеркалить нечего. Значит
	// нода предъявляет свой сертификат сама, и самоподписанный виден сканеру
	// как объявление «тут не сайт». Пусть лучше QUIC не включится, чем
	// включится и выдаст ноду.
	if opts.certFile == "" || opts.keyPEMFile == "" {
		return nil, errors.New("для -quic нужен настоящий сертификат: -tls-cert и -tls-key. Самоподписанный выдаёт ноду сканеру, а прятаться в QUIC не за что")
	}

	cert, err := transport.LoadCertificate(opts.certFile, opts.keyPEMFile)
	if err != nil {
		return nil, err
	}

	addr := opts.quicAddr
	if addr == "" {
		addr = opts.listenAddr
	}
	return transport.ListenQUIC(addr, cert)
}
