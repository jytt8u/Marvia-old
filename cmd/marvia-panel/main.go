// Команда marvia-panel — управляющий слой платформы.
//
// Через неё продавец заводит подписчиков, продлевает и отключает их, а ноды
// забирают свои списки и сдают статистику. Пользовательский трафик через
// панель не идёт никогда: её домен светится только в момент обновления
// подписки, и это делает её сравнительно живучей.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/crypto/acme/autocert"

	"github.com/jytt8u/marvia/internal/envvar"
	"github.com/jytt8u/marvia/internal/panel"
	"github.com/jytt8u/marvia/internal/redact"
	"github.com/jytt8u/marvia/internal/updater"
	"github.com/jytt8u/marvia/internal/vp1"
)

const shutdownGrace = 10 * time.Second

// version подставляется при сборке: -ldflags "-X main.version=v0.1.0".
//
// Без неё на вопрос «что у тебя стоит» продавец ответить не может, а поддержка
// не может понять, чинили ли уже его беду. Поэтому версия и в -version, и в
// первой строке журнала: в обращения обычно попадает именно журнал.
var version = "dev"

type options struct {
	listen     string
	dbPath     string
	adminToken string
	subBase    string
	distDir    string

	certFile string
	keyFile  string

	acmeDomain string
	acmeEmail  string
	acmeCache  string
	acmeHTTP   string
	panelIPs   string

	backupDir   string
	backupKeep  int
	backupEvery time.Duration

	// backupKey — публичный ключ для шифрования копий. Только публичный:
	// приватному на сервере делать нечего.
	backupKey string
}

func main() {
	var opts options

	flag.StringVar(&opts.listen, "listen", "127.0.0.1:8080", "адрес HTTP-интерфейса")
	flag.StringVar(&opts.dbPath, "db", "marvia-panel.db", "файл базы данных")
	flag.StringVar(&opts.adminToken, "admin-token", "", "админский токен (по умолчанию — из MARVIA_ADMIN_TOKEN)")
	flag.StringVar(&opts.subBase, "sub-base", "", "внешний адрес панели для ссылок подписки, например https://sub.example.com")
	flag.StringVar(&opts.distDir, "dist", "dist", "каталог с бинарниками ноды: панель раздаёт их установщику")

	flag.StringVar(&opts.certFile, "tls-cert", "", "файл сертификата PEM (без него — голый HTTP за обратным прокси)")
	flag.StringVar(&opts.keyFile, "tls-key", "", "файл приватного ключа сертификата PEM")

	flag.StringVar(&opts.acmeDomain, "acme-domain", "", "получать сертификат самой на этот домен")
	flag.StringVar(&opts.acmeEmail, "acme-email", "", "почта для писем Let's Encrypt об истечении")
	flag.StringVar(&opts.acmeCache, "acme-cache", "acme", "каталог для полученных сертификатов")
	flag.StringVar(&opts.acmeHTTP, "acme-http", ":80", "адрес для ответов на проверку ACME (пусто — не поднимать)")
	flag.StringVar(&opts.panelIPs, "panel-ip", "", "адреса панели через запятую для ссылок доступа (пусто — выяснить по домену)")

	flag.StringVar(&opts.backupDir, "backup-dir", "", "каталог для копий базы (по умолчанию — backup рядом с базой)")
	flag.IntVar(&opts.backupKeep, "backup-keep", panel.BackupKeep, "сколько копий держать")
	flag.StringVar(&opts.backupKey, "backup-key", "", "публичный ключ для шифрования копий базы (приватный держи вне сервера)")
	flag.DurationVar(&opts.backupEvery, "backup-every", panel.BackupEvery, "как часто снимать копию (0 — не снимать)")

	newToken := flag.Bool("new-token", false, "выпустить админский токен и выйти")
	showVersion := flag.Bool("version", false, "показать версию и выйти")
	verifyBackup := flag.String("verify-backup", "", "проверить парольную копию и выйти (пароль из stdin или MARVIA_BACKUP_PASSWORD)")
	backupOutput := flag.String("backup-output", "", "при проверке сохранить восстановленную базу в новый файл")
	installUpdater := flag.Bool("install-updater", false, "поставить службу обновления по кнопке в панели и выйти (нужен root)")

	var imp importOptions
	flag.StringVar(&imp.source, "import", "", "перенести покупателей с прежней панели: marzban или 3x-ui (без -import-apply — только отчёт)")
	flag.StringVar(&imp.from, "import-db", "", "база прежней панели: /var/lib/marzban/db.sqlite3 или /etc/x-ui/x-ui.db")
	flag.StringVar(&imp.xray, "import-xray", "", "xray_config.json Marzban (по умолчанию — рядом с базой)")
	flag.StringVar(&imp.subPath, "import-sub-path", "", "путь подписки прежней панели, если он не /sub/")
	flag.BoolVar(&imp.apply, "import-apply", false, "записать перенос в базу панели, а не только показать отчёт")

	flag.Parse()
	if *verifyBackup != "" {
		if err := runVerifyBackup(os.Stdout, os.Stdin, *verifyBackup, *backupOutput); err != nil {
			fmt.Fprintf(os.Stderr, "проверка копии: %v\n", err)
			os.Exit(1)
		}
		return
	}
	if *backupOutput != "" {
		fmt.Fprintln(os.Stderr, "-backup-output работает только с -verify-backup")
		os.Exit(1)
	}

	if *showVersion {
		fmt.Println("marvia-panel", version)
		return
	}
	if *installUpdater {
		if err := updater.Install(); err != nil {
			fmt.Fprintf(os.Stderr, "служба обновления: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if imp.source != "" {
		if err := runImport(os.Stdout, imp, opts.dbPath); err != nil {
			fmt.Fprintf(os.Stderr, "перенос: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if *newToken {
		token, err := panel.NewToken()
		if err != nil {
			fmt.Fprintf(os.Stderr, "ошибка: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(token)
		return
	}

	if err := run(opts); err != nil {
		fmt.Fprintf(os.Stderr, "ошибка: %v\n", err)
		os.Exit(1)
	}
}

func run(opts options) error {
	if opts.adminToken == "" {
		opts.adminToken = envvar.Get("MARVIA_ADMIN_TOKEN")
	}
	if opts.adminToken == "" {
		return errors.New("не задан админский токен: укажи -admin-token или MARVIA_ADMIN_TOKEN (выпустить: marvia-panel -new-token)")
	}
	if len(strings.TrimSpace(opts.adminToken)) < 16 {
		return errors.New("админский токен слишком короткий: нужен хотя бы 16 символов, лучше выпустить через -new-token")
	}
	if (opts.certFile == "") != (opts.keyFile == "") {
		return errors.New("-tls-cert и -tls-key задаются только вместе")
	}
	if opts.acmeDomain != "" && opts.certFile != "" {
		return errors.New("-acme-domain и -tls-cert вместе не работают: сертификат либо получает панель, либо даёшь его ты")
	}

	// Копия, подготовленная кнопкой «Восстановить» (internal/panel/restore.go),
	// встаёт на место базы до её открытия.
	if done, err := panel.ApplyStagedRestore(opts.dbPath, time.Now()); err != nil {
		return err
	} else if done {
		log.Printf("база восстановлена из копии")
	}

	store, err := panel.Open(opts.dbPath)
	if err != nil {
		return err
	}
	defer store.Close()

	// Копии базы уезжают с сервера, и там их читает кто угодно. Публичный
	// ключ делает их нечитаемыми; приватного здесь нет и быть не должно —
	// иначе изъятие сервера выдавало бы и копии, и ключ к ним.
	if opts.backupKey != "" {
		pub, err := vp1.DecodeKey(opts.backupKey)
		if err != nil {
			return fmt.Errorf("ключ для копий: %w", err)
		}
		store.WithBackupKey(pub)
		log.Printf("копии базы шифруются; открыть их можно только приватным ключом")
	} else if opts.backupEvery > 0 {
		log.Printf("ВНИМАНИЕ: копии базы не шифруются — задай -backup-key, если увозишь их с сервера")
	}

	// Всё, что хранится ограниченный срок, убирается сразу при запуске: сроки
	// и их причины — в internal/panel/retention.go.
	if err := store.Forget(context.Background()); err != nil {
		log.Printf("уборка старых записей: %v", err)
	}

	// Адреса выясняются один раз, при запуске, и уезжают в ссылки доступа.
	// Печатаем их вслух: продавец, поставивший панель за Cloudflare, должен
	// видеть, что в ссылки попал Cloudflare, а не прежний адрес сервера —
	// иначе он узнает об этом в тот день, когда сервер заблокируют.
	addresses := panelAddresses(opts)
	if len(addresses) > 0 {
		log.Printf("ссылки доступа подсказывают адреса панели: %s", strings.Join(addresses, ", "))
	}

	// Оповещения. Пока их не включили в панели, наружу они не
	// ходят вовсе — и панель не светит свой адрес телеграму.
	alerts := panel.NewAlerts(store)
	backupDir := opts.backupDir
	if backupDir == "" {
		backupDir = filepath.Join(filepath.Dir(opts.dbPath), "backup")
	}
	scheduled := panel.NewScheduledBackups(store, alerts, filepath.Join(backupDir, "scheduled"))

	api := panel.NewAPI(store, opts.adminToken, opts.subBase, opts.distDir).
		WithPanelIPs(addresses).
		WithVersion(version).
		WithScheduledBackups(scheduled).
		WithHome(filepath.Dir(opts.dbPath))
	server := newServer(opts.listen, api.Handler())

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// Перезапуск после восстановления — выходом с ошибкой: служба стоит с
	// Restart=on-failure и поднимет панель, а та при запуске подменит базу.
	api.WithRestore(opts.dbPath, func() {
		log.Printf("копия подготовлена, перезапускаюсь для подмены базы")
		os.Exit(3)
	})

	// Уборка по сроку идёт и дальше, а не только при запуске: панель
	// работает месяцами, и разовая чистка на старте означала бы, что
	// обещанные сроки хранения соблюдаются лишь у тех, кто часто её
	// перезапускает.
	go store.KeepForgetting(ctx, panel.ForgetEvery, func(err error) {
		log.Printf("уборка старых записей: %v", err)
	})

	// Копии базы снимает сама панель. Продавец, которому надо помнить про
	// cron, однажды про него не вспомнит — а panel.db это все его покупатели
	// и все оплаченные ими месяцы.
	if opts.backupEvery > 0 {
		log.Printf("копии базы: каждые %s в %s, храним %d",
			opts.backupEvery, backupDir, opts.backupKeep)
		go store.KeepBackups(ctx, backupDir, opts.backupKeep, opts.backupEvery, func(path string, err error) {
			// О несделанной копии узнают обычно в тот день, когда она понадобилась.
			// Поэтому не только в журнал, но и сообщением.
			alerts.BackupFailed(ctx, err)
			if err != nil {
				log.Printf("копия базы не снялась: %v", err)
				return
			}
			log.Printf("копия базы: %s", path)
		})
	}

	go alerts.Watch(ctx, 0)
	go scheduled.Watch(ctx, func(err error) {
		log.Printf("резервная копия не создана или не доставлена: %v", err)
	})
	go panel.NewWebhooks(store).Watch(ctx)
	// Ночное автообновление: само ничего не делает, пока продавец его не
	// включит на странице «Обновления»; см. internal/panel/autoupdate.go.
	go api.KeepUpToDate(ctx)
	// Ежемесячный сброс трафика у купивших по тарифу с ним; см.
	// internal/panel/reset.go.
	go api.KeepTrafficReset(ctx)

	go func() {
		<-ctx.Done()
		log.Printf("завершаем работу")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	log.Printf("marvia-panel %s слушает %s, база %s", version, opts.listen, opts.dbPath)
	if opts.subBase == "" {
		log.Printf("ВНИМАНИЕ: не задан -sub-base, ссылки подписки будут с заглушкой вместо адреса")
	}

	switch {
	case opts.acmeDomain != "":
		return serveACME(server, opts)

	case opts.certFile != "":
		log.Printf("панель отдаёт HTTPS, сертификат %s", opts.certFile)
		if err := server.ListenAndServeTLS(opts.certFile, opts.keyFile); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("HTTPS-сервер: %w", err)
		}
		return nil

	default:
		// Панель обязана стоять за HTTPS: по её API ходят токены, а в ответах
		// уезжают приватные ключи подписчиков. Клиент это и не обсуждает —
		// адрес подписки в ссылке доступа он всегда читает как https.
		log.Printf("панель отдаёт голый HTTP: ставь её только за обратным прокси с TLS")
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("HTTP-сервер: %w", err)
		}
		return nil
	}
}

// newServer собирает HTTP-сервер панели.
func newServer(listen string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              listen,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		// Тела запросов к панели не больше мегабайта, и минуты на них хватает
		// с запасом. Без предела медленный клиент держит соединение и
		// горутину сколько захочет — так панель кладут, не зная ни одного
		// токена. WriteTimeout не ставим: приложения покупателю отдаются
		// отсюда же, и на плохом мобильном десятки мегабайт идут дольше.
		ReadTimeout: time.Minute,
		IdleTimeout: 2 * time.Minute,
		TLSConfig:   &tls.Config{MinVersion: tls.VersionTLS12},
		// Свой журнал ошибок, а не стандартный. Стандартный net/http пишет
		// «TLS handshake error from 203.0.113.7:51234» — а к панели за
		// подпиской ходят покупатели, и журнал превращался в список их
		// адресов. Строка остаётся, адрес заменяется пометкой.
		ErrorLog: redact.Logger(),
	}
}

// serveACME поднимает панель с сертификатом, который она получает сама.
//
// Своё ACME появилось не от недоверия к certbot, а потому что продавец,
// поднявший панель на отдельной машине, не должен ради одной подписки
// осваивать ещё и nginx с расписанием обновления. Сертификат тут расходник:
// берётся при первом обращении и продлевается сам, без чужих служб.
func serveACME(server *http.Server, opts options) error {
	manager := &autocert.Manager{
		// Хранилище обязано переживать перезапуск. Без него каждый рестарт
		// заказывал бы новый сертификат, а у Let's Encrypt на домен всего пять
		// штук в неделю: после пятого перезапуска панель осталась бы без
		// сертификата на неделю, а вместе с ней все покупатели — без
		// обновления списка нод.
		Cache:      autocert.DirCache(opts.acmeCache),
		Prompt:     autocert.AcceptTOS,
		HostPolicy: autocert.HostWhitelist(opts.acmeDomain),
		Email:      opts.acmeEmail,
	}

	server.TLSConfig = manager.TLSConfig()
	server.TLSConfig.MinVersion = tls.VersionTLS12

	if opts.acmeHTTP != "" {
		// Let's Encrypt проверяет владение доменом одним из двух способов: по
		// http на 80-м порту или по TLS на 443-м. Второй обычно занят нодой, а
		// нода на незнакомого гостя отвечает сайтом прикрытия — проверка уйдёт
		// в Samsung и не вернётся. Поэтому держим 80-й.
		go func() {
			challenge := &http.Server{
				Addr:              opts.acmeHTTP,
				Handler:           manager.HTTPHandler(nil),
				ReadHeaderTimeout: 10 * time.Second,
				ErrorLog:          redact.Logger(),
			}
			if err := challenge.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Printf("ВНИМАНИЕ: не занять %s для проверки ACME: %v", opts.acmeHTTP, err)
				log.Printf("пока порт занят, сертификат получить не выйдет")
			}
		}()
	}

	log.Printf("панель берёт сертификат сама: домен %s, хранилище %s", opts.acmeDomain, opts.acmeCache)
	log.Printf("первое обращение по https займёт несколько секунд — в этот момент заказывается сертификат")

	if err := server.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("HTTPS-сервер: %w", err)
	}
	return nil
}

// panelAddresses выясняет, какие адреса вписывать в ссылки доступа.
//
// Заданы вручную — берём их. Не заданы — спрашиваем DNS про собственный домен
// один раз при запуске. Это правильный ответ в обоих случаях: без CDN домен
// ведёт прямо на нас, за CDN — на его адреса, а покупателю нужны ровно те, по
// которым он и будет ходить.
//
// Спрашиваем здесь, а не у покупателя: один запрос при запуске панели вместо
// запроса у каждого покупателя при каждом включении.
func panelAddresses(opts options) []string {
	if manual := strings.TrimSpace(opts.panelIPs); manual != "" {
		out := []string{}
		for _, part := range strings.Split(manual, ",") {
			if part = strings.TrimSpace(part); part != "" {
				out = append(out, part)
			}
		}
		return out
	}

	host := opts.acmeDomain
	if host == "" {
		host = strings.TrimPrefix(strings.TrimPrefix(strings.TrimRight(opts.subBase, "/"), "https://"), "http://")
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
	}
	if host == "" {
		return nil
	}

	ips, err := net.LookupIP(host)
	if err != nil {
		log.Printf("не выяснил адреса %s, ссылки доступа пойдут без подсказки: %s",
			redact.Secrets(host), redact.Secrets(err.Error()))
		return nil
	}

	out := make([]string, 0, len(ips))
	for _, ip := range ips {
		if v4 := ip.To4(); v4 != nil {
			out = append(out, v4.String())
		}
	}
	return out
}
