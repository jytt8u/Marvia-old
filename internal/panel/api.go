package panel

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jytt8u/marvia/internal/connguard"
	"github.com/jytt8u/marvia/internal/look"
	"github.com/jytt8u/marvia/internal/redact"
	"github.com/jytt8u/marvia/internal/routes"
	"github.com/jytt8u/marvia/internal/seller"
	"github.com/jytt8u/marvia/internal/users"
)

// API — HTTP-интерфейс панели.
//
// Четыре круга доступа:
//   - админский токен — полный доступ и выпуск ключей. Им продавец входит в
//     панель, и больше он не должен попадать никуда;
//   - ключ доступа — права из числа users, nodes, read. Это то, что получает
//     бот продавца: отзывается отдельно и не даёт трогать ноды;
//   - токен ноды — только свой список пользователей и отправка статистики;
//   - подписка по токену — без авторизации, токен и есть секрет.
type API struct {
	store            *Store
	scheduledBackups *ScheduledBackups
	adminToken       string
	subBase          string // базовый адрес подписок, например https://sub.example.com
	distDir          string // где лежат бинарники для раздачи новым нодам

	// restorePath и restart — восстановление из копии (restore.go).
	restorePath string
	restart     func()

	// panelIPs вписываются в ссылку доступа, чтобы клиент не спрашивал имя
	// домена подписки у резолвера провайдера. Подробности — в links.go.
	panelIPs []string

	// version — версия сборки. Отдаётся только с авторизацией: в /healthz,
	// который открыт всему интернету, точная версия говорит сканеру, какие
	// дыры пробовать, и заодно опознаёт панель как нашу.
	version string

	// home — каталог панели: туда кладётся просьба к службе обновления.
	home string

	// releases — что известно о последнем релизе; см. updates.go.
	releases releaseCache
	// auto — что ночное автообновление уже попросило; см. autoupdate.go.
	auto autoState

	// geo — когда по какой ноде спрашивали страну по адресу; см. geo.go.
	geo geoTries

	// flood — пределы против потока запросов, floodNow — их часы; см.
	// flood.go.
	flood    floodLimits
	floodNow func() time.Time
}

// NewAPI собирает обработчики панели.
func NewAPI(store *Store, adminToken, subBase, distDir string) *API {
	return &API{store: store, adminToken: adminToken, subBase: strings.TrimRight(subBase, "/"), distDir: distDir, flood: floodDefaults}
}

// Handler возвращает готовый маршрутизатор.
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()

	// Управление: пользователи. Это всё, что нужно боту продавца, и ровно
	// столько прав ему и достаётся.
	mux.HandleFunc("GET /api/v1/users", a.scoped(ScopeRead, a.listUsers))
	mux.HandleFunc("GET /api/v1/users/export.csv", a.admin(a.exportUsers))
	mux.HandleFunc("POST /api/v1/users", a.scoped(ScopeUsers, a.createUser))
	mux.HandleFunc("GET /api/v1/users/{id}", a.scoped(ScopeRead, a.getUser))
	mux.HandleFunc("PATCH /api/v1/users/{id}", a.scoped(ScopeUsers, a.updateUser))
	mux.HandleFunc("DELETE /api/v1/users/{id}", a.scoped(ScopeUsers, a.deleteUser))
	mux.HandleFunc("GET /api/v1/users/{id}/links", a.scoped(ScopeUsers, a.userLinks))
	mux.HandleFunc("POST /api/v1/users/{id}/credentials", a.scoped(ScopeUsers, a.addCredential))
	mux.HandleFunc("POST /api/v1/users/{id}/sub-token", a.scoped(ScopeUsers, a.rotateSubToken))
	mux.HandleFunc("POST /api/v1/users/{id}/renew", a.scoped(ScopeUsers, a.renewUser))
	mux.HandleFunc("POST /api/v1/users/{id}/reset-traffic", a.scoped(ScopeUsers, a.resetTraffic))
	mux.HandleFunc("POST /api/v1/users/{id}/freeze", a.scoped(ScopeUsers, a.freezeUser))
	mux.HandleFunc("POST /api/v1/users/{id}/unfreeze", a.scoped(ScopeUsers, a.unfreezeUser))
	mux.HandleFunc("POST /api/v1/users/{id}/reset-devices", a.scoped(ScopeUsers, a.resetDevices))
	mux.HandleFunc("POST /api/v1/users/{id}/promo", a.scoped(ScopeUsers, a.redeemPromo))
	mux.HandleFunc("GET /api/v1/promos", a.admin(a.listPromos))
	mux.HandleFunc("POST /api/v1/promos", a.admin(a.createPromo))
	mux.HandleFunc("DELETE /api/v1/promos/{id}", a.admin(a.deletePromo))
	mux.HandleFunc("PUT /api/v1/referral", a.admin(a.setReferral))
	mux.HandleFunc("GET /api/v1/report", a.scoped(ScopeRead, a.monthReport))
	mux.HandleFunc("GET /api/v1/night-speed", a.scoped(ScopeRead, a.getNightSpeed))
	mux.HandleFunc("PUT /api/v1/night-speed", a.admin(a.setNightSpeed))
	mux.HandleFunc("GET /api/v1/bot-texts", a.scoped(ScopeRead, a.getBotTexts))
	mux.HandleFunc("PUT /api/v1/bot-texts", a.admin(a.setBotTexts))
	mux.HandleFunc("POST /api/v1/qr", a.scoped(ScopeUsers, a.qrCode))
	mux.HandleFunc("POST /api/v1/users/bulk", a.scoped(ScopeUsers, a.bulkUsers))
	mux.HandleFunc("GET /api/v1/users.csv", a.scoped(ScopeRead, a.usersCSV))

	// Тарифы. Бот их читает, чтобы продавать то же, что видит продавец, а
	// меняет только админ: тариф задаёт, сколько получат за деньги все
	// следующие покупатели, и утёкший ключ бота не должен раздать «год даром».
	mux.HandleFunc("GET /api/v1/plans", a.scoped(ScopeRead, a.listPlans))
	mux.HandleFunc("POST /api/v1/plans", a.admin(a.createPlan))
	mux.HandleFunc("PATCH /api/v1/plans/{id}", a.admin(a.updatePlan))
	mux.HandleFunc("DELETE /api/v1/plans/{id}", a.admin(a.deletePlan))
	mux.HandleFunc("POST /api/v1/credentials/{id}/rotate", a.scoped(ScopeUsers, a.rotateCredential))
	mux.HandleFunc("DELETE /api/v1/credentials/{id}", a.scoped(ScopeUsers, a.deleteCredential))

	// Управление: ноды. Боту сюда не надо.
	mux.HandleFunc("GET /api/v1/nodes", a.scoped(ScopeRead, a.listNodes))
	mux.HandleFunc("POST /api/v1/nodes", a.admin(a.createNode))
	mux.HandleFunc("PATCH /api/v1/nodes/{id}", a.scoped(ScopeNodes, a.updateNode))
	mux.HandleFunc("DELETE /api/v1/nodes/{id}", a.scoped(ScopeNodes, a.deleteNode))
	mux.HandleFunc("POST /api/v1/nodes/{id}/check", a.scoped(ScopeNodes, a.checkNode))
	mux.HandleFunc("GET /api/v1/nodes/{id}/geo", a.scoped(ScopeNodes, a.nodeGeo))
	mux.HandleFunc("POST /api/v1/nodes/invite", a.admin(a.createNodeInvite))

	// Ключи доступа. Только по админскому токену: ключ, умеющий выпускать
	// ключи, ничем не отличается от админского — и разделение теряет смысл.
	mux.HandleFunc("GET /api/v1/keys", a.admin(a.listKeys))
	mux.HandleFunc("POST /api/v1/keys", a.admin(a.createKey))
	mux.HandleFunc("DELETE /api/v1/keys/{id}", a.admin(a.revokeKey))

	// Копия базы. Тоже только админским: в ней лежит вся панель целиком.
	mux.HandleFunc("GET /api/v1/backup", a.admin(a.downloadBackup))
	mux.HandleFunc("GET /api/v1/backups", a.admin(a.getBackupSettings))
	mux.HandleFunc("PUT /api/v1/backups", a.admin(a.setBackupSettings))
	mux.HandleFunc("POST /api/v1/backups/run", a.admin(a.runBackup))

	// Оповещения — только админским токеном: в настройках лежит токен бота
	// продавца, а им можно писать от его имени кому угодно.
	// Журнал событий — только админским токеном. Это летопись действий над
	// панелью, и боту с правом read в ней делать нечего.
	mux.HandleFunc("GET /api/v1/events", a.admin(a.listEvents))

	mux.HandleFunc("GET /api/v1/alerts", a.admin(a.getAlerts))
	mux.HandleFunc("GET /api/v1/guard", a.admin(a.getGuard))
	mux.HandleFunc("PUT /api/v1/guard", a.admin(a.setGuard))

	// Обновления: только админ. Ключ бота обновлять серверы не должен —
	// ему хватает подписчиков.
	mux.HandleFunc("GET /api/v1/updates", a.admin(a.getUpdates))
	mux.HandleFunc("POST /api/v1/updates/panel", a.admin(a.upgradePanel))
	mux.HandleFunc("POST /api/v1/updates/nodes", a.admin(a.upgradeNodes))
	mux.HandleFunc("POST /api/v1/updates/all", a.admin(a.upgradeAll))
	mux.HandleFunc("GET /api/v1/updates/auto", a.admin(a.getAutoUpdate))
	mux.HandleFunc("PUT /api/v1/updates/auto", a.admin(a.setAutoUpdate))
	mux.HandleFunc("PUT /api/v1/alerts", a.admin(a.setAlerts))
	// Получатели видят метки покупателей, а подпись даёт доверие ботам:
	// ни адреса доставки, ни секрет не может менять ключ бота.
	mux.HandleFunc("GET /api/v1/webhooks", a.admin(a.getWebhooks))
	mux.HandleFunc("PUT /api/v1/webhooks", a.admin(a.setWebhooks))

	// Связь с покупателем: поддержка, «Продлить», объявление. Читать можно
	// ключом с правом read — бот продавца шлёт покупателю ту же ссылку
	// продления, а секрета в ней нет: она и так уходит в каждой подписке.
	// Менять — только админским токеном: эти ссылки ведут покупателей к
	// деньгам, и утёкший ключ бота не должен уметь подменить страницу оплаты
	// на чужую у всех покупателей разом.
	mux.HandleFunc("GET /api/v1/seller", a.scoped(ScopeRead, a.getSeller))
	mux.HandleFunc("PUT /api/v1/seller", a.admin(a.setSeller))

	// Установка ноды одной командой. Приглашение стоит в адресе, потому что
	// команду продавец вставляет целиком, не разбираясь в заголовках.
	mux.HandleFunc("GET /install/{token}", a.installScript)
	mux.HandleFunc("GET /install/{token}/{name}", a.installBinary)
	mux.HandleFunc("POST /api/v1/nodes/register", a.registerNode)

	// Ноды забирают свой список и сдают статистику.
	mux.HandleFunc("GET /api/v1/node/users", a.node(a.nodeUsers))
	mux.HandleFunc("POST /api/v1/node/usage", a.node(a.nodeUsage))

	// Подписка. Токен в адресе и есть авторизация.
	mux.HandleFunc("GET /sub/{token}", a.subscription)

	// Отчёты о доступности от клиентов. Тем же токеном: админского на
	// устройстве покупателя быть не должно ни при каких обстоятельствах.
	mux.HandleFunc("POST /sub/{token}/report", a.report)

	// Приложения покупателям раздаёт сама панель — с домена продавца.
	// Подробности и причина в apps.go.
	mux.HandleFunc("GET /sub/{token}/app/{name}", a.appDownload)

	// Российские подсети для байпаса. По токену подписки: список не секрет, но
	// и раздавать его всему интернету с домена продавца незачем.
	mux.HandleFunc("GET /sub/{token}/bypass", a.bypassRoutes)

	// Подписка по пути прежней панели, если он был не /sub/. Все пути выше
	// конкретнее этого шаблона и выигрывают у него сами.
	//
	// Без метода в шаблоне — намеренно. С «GET» маршрутизатор отвечал бы на
	// POST по любому пути из двух частей 405 вместо 404, и сканер отличал бы
	// панель по этой мелочи. Метод проверяет сам обработчик.
	mux.HandleFunc("/{prefix}/{token}", a.legacySubscription)
	mux.HandleFunc("GET /api/v1/apps", a.scoped(ScopeRead, a.listApps))
	mux.HandleFunc("GET /api/v1/stats", a.scoped(ScopeRead, a.stats))

	// Веб-интерфейс. Только по точному корню: всё остальное — 404, чтобы
	// панель не отвечала страницей на случайные пути сканеров.
	mux.HandleFunc("GET /{$}", a.ServeApp)
	mux.HandleFunc("GET /fonts/{name}", look.ServeFont)
	mux.HandleFunc("GET /assets/{name}", look.ServeAsset)

	// Версия — с авторизацией. Продавцу она нужна, когда он пишет в поддержку;
	// постороннему сканеру знать её незачем.
	mux.HandleFunc("GET /api/v1/me", a.me)
	mux.HandleFunc("POST /api/v1/restore", a.admin(a.restoreBackup))
	mux.HandleFunc("POST /api/v1/login", a.login)
	mux.HandleFunc("GET /api/v1/version", a.scoped(ScopeRead, func(w http.ResponseWriter, _ *http.Request) {
		version := a.version
		if version == "" {
			version = "неизвестна"
		}
		ok(w, map[string]any{"version": version})
	}))

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	// Пределы — перед всеми маршрутами сразу: флуд дешевле отсечь до
	// разбора пути, чем учить каждый обработчик ему отказывать.
	return newFloodGuard(mux, a.flood, a.floodNow)
}

// scoped пускает по админскому токену или по ключу с нужным правом.
//
// Админский токен проверяется первым и за постоянное время: он лежит в памяти,
// и поход в базу за ним не нужен. Ключ ищется по хешу, и база заодно отмечает,
// что им воспользовались.
func (a *API) scoped(scope string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := bearer(r)
		if token == "" {
			fail(w, http.StatusUnauthorized, "нужен токен: админский или ключ доступа")
			return
		}
		if TokensEqual(token, a.adminToken) {
			next(w, withSecrets(withActor(r, ActorAdmin), true))
			return
		}

		key, err := a.store.AuthenticateAPIKey(r.Context(), token)
		if err != nil {
			fail(w, http.StatusUnauthorized, "неизвестный токен")
			return
		}
		if !key.Allows(scope) {
			// Говорим, чего именно не хватает. «Доступ запрещён» без объяснения
			// заставляет выпустить ключ со всеми правами — и разделение, ради
			// которого всё затевалось, пропадает.
			fail(w, http.StatusForbidden, "ключу «"+key.Name+"» не хватает права "+scope)
			return
		}
		// Имя ключа, а не сам ключ: по журналу должно быть видно, чей бот
		// наделал дел, и при этом журнал не должен становиться местом,
		// откуда утекают токены.
		next(w, withSecrets(withActor(r, key.Name), key.Allows(ScopeUsers)))
	}
}

// admin проверяет админский токен.
//
// Живой ключ бота здесь получает 403, а не 401. Это разные ответы: 401 — «такого
// токена нет», и страница панели на нём выходит из учётной записи; 403 — «токен
// есть, но это ему не положено». Раньше отвечали 401 всем, и поддержка,
// вошедшая ключом с правом чтения, вылетала при первой же загрузке: страница
// спрашивает список ключей, а он только для админа.
func (a *API) admin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := bearer(r)
		if TokensEqual(token, a.adminToken) {
			next(w, withActor(r, ActorAdmin))
			return
		}
		if token != "" {
			if key, err := a.store.AuthenticateAPIKey(r.Context(), token); err == nil {
				fail(w, http.StatusForbidden, "ключу «"+key.Name+"» это не положено: нужен админский токен")
				return
			}
		}
		fail(w, http.StatusUnauthorized, "нужен админский токен")
	}
}

// nodeContext передаёт опознанную ноду обработчику.
type nodeHandler func(http.ResponseWriter, *http.Request, Node)

// node проверяет токен ноды.
func (a *API) node(next nodeHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := bearer(r)
		if token == "" {
			fail(w, http.StatusUnauthorized, "нужен токен ноды")
			return
		}
		n, err := a.store.AuthenticateNode(r.Context(), token)
		if err != nil {
			fail(w, http.StatusUnauthorized, "неизвестный токен ноды")
			return
		}
		if !n.Enabled {
			fail(w, http.StatusForbidden, "нода отключена")
			return
		}
		next(w, r, n)
	}
}

func (a *API) listUsers(w http.ResponseWriter, r *http.Request) {
	// Поиск по ключу продавца: боту надо по telegram id понять, кто перед ним,
	// и не держать ради этого вторую базу соответствий.
	if external := strings.TrimSpace(r.URL.Query().Get("external_id")); external != "" {
		user, err := a.store.UserByExternalID(r.Context(), external)
		if err != nil {
			respondStoreErr(w, err)
			return
		}
		ok(w, map[string]any{"users": visible(r, []User{user})})
		return
	}

	list, err := a.store.ListUsers(r.Context())
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	ok(w, map[string]any{"users": visible(r, list)})
}

func (a *API) createUser(w http.ResponseWriter, r *http.Request) {
	var p CreateUserParams
	if !decode(w, r, &p) {
		return
	}

	// Ключ идемпотентности защищает и первую продажу, не только продление.
	//
	// Платёжные системы и телеграм повторяют уведомление, пока бот не ответил,
	// а бот может упасть ровно между выдачей доступа и ответом. Повтор той
	// оплаты, которая завела покупателя, без этого выдавал бы второй срок за
	// одни деньги: ErrAlreadyExists ловит только повтор по external_id, а
	// продавец, продающий без него, не защищён ничем.
	//
	// Область — вместе с покупателем: тот же ключ, пришедший на другого,
	// означает ошибку в боте, и лучше ответить 409, чем молча выдать первому.
	scope := "create:" + strings.TrimSpace(p.ExternalID)
	if handled := a.replayed(w, r, scope); handled {
		return
	}

	user, issued, err := a.store.CreateUser(r.Context(), p)

	// Такой покупатель уже заведён — значит уведомление об оплате пришло
	// повторно. Отвечаем успехом и говорим, что ничего не создали: бот на
	// повторе не должен ни падать, ни выдавать второй доступ за ту же оплату.
	if errors.Is(err, ErrAlreadyExists) {
		ok(w, map[string]any{
			"user":    noteFor(r, user),
			"created": false,
			"issued":  []Issued{},
			"links":   a.links(r.Context(), user, nil),
		})
		return
	}
	if err != nil {
		if errors.Is(err, ErrPlanConflict) || errors.Is(err, ErrNoSuchPlan) || errors.Is(err, ErrBadTags) {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
		if errors.Is(err, ErrUnknownKind) || errors.Is(err, ErrTrialNeedsKey) {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
		if errors.Is(err, ErrTrialUsed) {
			fail(w, http.StatusConflict, err.Error())
			return
		}
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Запоминаем ответ без секретов, хотя отдаём с ними.
	//
	// Приватную часть vp1 панель не хранит нигде — в этом весь смысл: утечка
	// её базы не даёт доступа ни к одному покупателю нашего протокола. Сложить
	// секрет в таблицу повторов ради удобства бота значило бы разменять это
	// свойство на сутки хранения. Поэтому повтор получает того же покупателя,
	// created=false и ссылки без ключа: доступ выдан один раз, и если бот его
	// потерял, выдаётся новый набор, а не старый.
	a.remember(r, scope, user.ID, map[string]any{
		"user":    user,
		"created": false,
		"issued":  []Issued{},
		"links":   a.links(r.Context(), user, nil),
	})

	ok(w, map[string]any{
		"user":    user,
		"created": true,
		// Секреты отдаются ровно здесь и больше нигде. Для vp1 панель не
		// хранит приватную часть вовсе; для vless и trojan хранит, но
		// повторно через API не отдаёт. Бот обязан сразу переслать ссылки
		// покупателю.
		"issued": issued,
		"links":  a.links(r.Context(), user, issued),
	})

	a.record(r, EventUserCreate, Event{UserID: user.ID})
}

// links собирает готовые к отправке ссылки.
//
// Бот не должен ничего склеивать сам: одна ошибка в параметрах ссылки — и
// покупатель приходит в поддержку с «не работает», а продавец не понимает,
// в чём дело.
func (a *API) links(ctx context.Context, user User, issued []Issued) map[string]any {
	out := map[string]any{"subscription": a.subURL(user.SubToken)}

	// Ссылки на приложения — с домена панели. Магазины приложений и чужие
	// файлохостинги отваливаются первыми, и покупатель застревает на шаге
	// «скачай», уже заплатив.
	if apps := a.appLinks(user.SubToken); apps != nil {
		out["apps"] = apps
	}

	stock := make([]Credential, 0, len(issued))
	for _, i := range issued {
		switch i.Kind {
		case CredVP1:
			out["account"] = AccountLink(a.subBase, i.Secret, user.SubToken, user.Label, a.panelIPs)
		default:
			stock = append(stock, Credential{Kind: i.Kind, Secret: i.Secret})
		}
	}

	if len(stock) == 0 {
		return out
	}

	nodes, err := a.store.SubscriptionNodes(ctx, user.NodeGroup)
	if err != nil {
		// Ссылки на ноды не собрались, но подписку отдать всё равно можно:
		// клиент возьмёт список оттуда.
		log.Printf("сборка ссылок: %v", err)
		return out
	}
	if links := StockLinks(nodes, stock, user.Label); len(links) > 0 {
		out["stock"] = links
	}
	return out
}

// subURL — адрес подписки для чужих клиентов.
func (a *API) subURL(token string) string {
	base := a.subBase
	if base == "" {
		base = "https://ПОДСТАВЬ-АДРЕС-ПАНЕЛИ"
	}
	return base + "/sub/" + token
}

func (a *API) getUser(w http.ResponseWriter, r *http.Request) {
	id, okID := pathID(w, r)
	if !okID {
		return
	}
	user, err := a.store.GetUser(r.Context(), id)
	if err != nil {
		respondStoreErr(w, err)
		return
	}
	ok(w, map[string]any{"user": visible(r, []User{user})[0]})
}

func (a *API) updateUser(w http.ResponseWriter, r *http.Request) {
	id, okID := pathID(w, r)
	if !okID {
		return
	}
	// ExtendBy отдельно от остальных полей: это не «поставить срок», а
	// «добавить к тому, что есть». Разница видна на покупателе, который
	// продлевает за неделю до конца: с абсолютным сроком он эту неделю теряет.
	var p struct {
		UpdateUserParams
		ExtendBy string `json:"extend_by,omitempty"`
	}
	if !decode(w, r, &p) {
		return
	}
	if p.ExtendBy != "" && (p.ExpiresAt != nil || p.NoExpiry) {
		fail(w, http.StatusBadRequest, "extend_by вместе с expires_at или no_expiry не работает: либо продлить, либо назначить срок")
		return
	}
	if p.ExpiresAt != nil && p.NoExpiry {
		fail(w, http.StatusBadRequest, ErrExpiryConflict.Error())
		return
	}

	// Ключ идемпотентности защищает продление: повтор уведомления от платёжной
	// системы иначе добавит срок дважды. Подробности в idempotency.go.
	scope := fmt.Sprintf("extend:%d", id)
	if handled := a.replayed(w, r, scope); handled {
		return
	}

	if p.ExtendBy != "" {
		d, err := ParseDuration(p.ExtendBy)
		if err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
		if _, err := a.store.ExtendUser(r.Context(), id, d); err != nil {
			respondStoreErr(w, err)
			return
		}
	}

	user, err := a.store.UpdateUser(r.Context(), id, p.UpdateUserParams)
	if errors.Is(err, ErrNoteTooLong) || errors.Is(err, ErrExpiryConflict) || errors.Is(err, ErrBadTags) {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if err != nil {
		respondStoreErr(w, err)
		return
	}

	user = noteFor(r, user)
	a.remember(r, scope, user.ID, map[string]any{"user": user})
	a.record(r, EventUserUpdate, Event{UserID: user.ID, Detail: updateDetail(p.UpdateUserParams, p.ExtendBy)})
	ok(w, map[string]any{"user": user})
}

// replayed отдаёт сохранённый ответ, если запрос с этим ключом уже проходил.
//
// true означает, что ответ уже отправлен и обработчику делать нечего: либо это
// повтор, либо ключ занят другой операцией, либо он негоден.
func (a *API) replayed(w http.ResponseWriter, r *http.Request, scope string) bool {
	key := strings.TrimSpace(r.Header.Get(idempotencyHeader))
	if key == "" {
		return false
	}
	if len(key) > maxIdempotencyKey {
		fail(w, http.StatusBadRequest, "ключ идемпотентности длиннее допустимого")
		return true
	}

	stored, found, err := a.store.RememberedResponse(r.Context(), key, scope)
	if errors.Is(err, ErrIdempotencyScope) {
		fail(w, http.StatusConflict, err.Error())
		return true
	}
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return true
	}
	if !found {
		return false
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Idempotent-Replay", "true")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(stored))
	return true
}

// remember запоминает ответ под ключом идемпотентности, если он был задан.
// userID — о ком ответ: с удалением покупателя уходит и он.
func (a *API) remember(r *http.Request, scope string, userID int64, payload any) {
	key := strings.TrimSpace(r.Header.Get(idempotencyHeader))
	if key == "" {
		return
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return
	}
	// Неудачу записи глотаем намеренно: операция уже прошла, и отвечать
	// продавцу ошибкой из-за незапомненного ответа хуже, чем рискнуть
	// повтором. Худший случай здесь — ровно то поведение, что было раньше.
	_ = a.store.RememberResponse(r.Context(), key, scope, string(body), userID)
}

func (a *API) deleteUser(w http.ResponseWriter, r *http.Request) {
	id, okID := pathID(w, r)
	if !okID {
		return
	}
	// Запись делаем ДО удаления: внешний ключ с каскадом унесёт её вместе с
	// покупателем, и это нарочно — «удалили значит удалили». Здесь она нужна
	// лишь затем, чтобы сам факт удаления не пропал бесследно из ответа.
	if err := a.store.DeleteUser(r.Context(), id); err != nil {
		respondStoreErr(w, err)
		return
	}
	a.record(r, EventUserDelete, Event{Detail: "вместе со всеми его записями"})
	w.WriteHeader(http.StatusNoContent)
}

// userLinks отдаёт ссылки уже заведённого подписчика.
//
// Нужно для самого частого обращения в поддержку: «потерял конфиг, пришлите
// заново». Для vless и trojan ссылку можно собрать снова — секрет хранится в
// базе. Для vp1 нельзя: приватной части у панели нет, и это не недоработка,
// а осознанное свойство. Такому подписчику выпускают новый ключ.
func (a *API) userLinks(w http.ResponseWriter, r *http.Request) {
	id, okID := pathID(w, r)
	if !okID {
		return
	}

	user, err := a.store.GetUser(r.Context(), id)
	if err != nil {
		respondStoreErr(w, err)
		return
	}
	nodes, err := a.store.SubscriptionNodes(r.Context(), user.NodeGroup)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}

	answer := map[string]any{
		"subscription": a.subURL(user.SubToken),
		"stock":        StockLinks(nodes, user.Credentials, user.Label),
	}
	if apps := a.appLinks(user.SubToken); apps != nil {
		answer["apps"] = apps
	}
	ok(w, answer)
}

func (a *API) addCredential(w http.ResponseWriter, r *http.Request) {
	id, okID := pathID(w, r)
	if !okID {
		return
	}
	var body struct {
		Kind  string `json:"kind"`
		Label string `json:"label"`
	}
	if r.ContentLength > 0 && !decode(w, r, &body) {
		return
	}

	cred, secret, err := a.store.AddCredential(r.Context(), id, body.Kind, body.Label)
	if err != nil {
		if errors.Is(err, ErrUnknownKind) {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
		respondStoreErr(w, err)
		return
	}
	user, err := a.store.GetUser(r.Context(), id)
	if err != nil {
		respondStoreErr(w, err)
		return
	}

	issued := []Issued{{ID: cred.ID, Kind: cred.Kind, Secret: secret}}
	a.record(r, EventCredAdd, Event{UserID: user.ID, Detail: "выдан доступ " + cred.Kind})
	ok(w, map[string]any{
		"credential": cred,
		"issued":     issued,
		"links":      a.links(r.Context(), user, issued),
	})
}

// rotateCredential меняет ключ набора, оставляя сам набор на месте.
//
// Отвечает тем же, чем выдача нового: ссылками. Секрет показывается один раз
// — панель его не хранит для vp1 и не показывает второй раз ни для одного
// вида.
func (a *API) rotateCredential(w http.ResponseWriter, r *http.Request) {
	id, okID := pathID(w, r)
	if !okID {
		return
	}

	cred, secret, err := a.store.RotateCredential(r.Context(), id)
	if err != nil {
		respondStoreErr(w, err)
		return
	}
	user, err := a.store.UserByCredential(r.Context(), cred.ID)
	if err != nil {
		respondStoreErr(w, err)
		return
	}

	issued := []Issued{{ID: cred.ID, Kind: cred.Kind, Secret: secret}}
	a.record(r, EventCredRotate, Event{UserID: user.ID, Detail: "сменён ключ " + cred.Kind})
	ok(w, map[string]any{
		"credential": cred,
		"issued":     issued,
		"links":      a.links(r.Context(), user, issued),
	})
}

func (a *API) deleteCredential(w http.ResponseWriter, r *http.Request) {
	id, okID := pathID(w, r)
	if !okID {
		return
	}
	// Хозяина набора узнаём до удаления: после него спрашивать будет не у кого.
	owner, ownerErr := a.store.UserByCredential(r.Context(), id)

	if err := a.store.DeleteCredential(r.Context(), id); err != nil {
		respondStoreErr(w, err)
		return
	}
	if ownerErr == nil {
		a.record(r, EventCredDelete, Event{UserID: owner.ID})
	}
	w.WriteHeader(http.StatusNoContent)
}

// report принимает от клиента отчёты о доступности нод.
//
// Ответ всегда 204, даже если часть отчётов не про наши ноды: подписка на
// устройстве могла устареть, и превращать это в ошибку незачем.
func (a *API) report(w http.ResponseWriter, r *http.Request) {
	user, err := a.store.UserBySubToken(r.Context(), r.PathValue("token"))
	if err != nil {
		// Как и в подписке, не подсказываем, существует ли токен.
		http.NotFound(w, r)
		return
	}

	var body struct {
		Reports []Report `json:"reports"`
	}
	if !decode(w, r, &body) {
		return
	}
	if len(body.Reports) > maxReportsPerRequest {
		fail(w, http.StatusBadRequest, "слишком много отчётов в одном запросе")
		return
	}

	if err := a.store.SaveReports(r.Context(), user.ID, body.Reports); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// maxReportsPerRequest — верхняя граница на один запрос. Нод у продавца
// десятки, а не тысячи; всё сверх этого — попытка нагрузить панель.
const maxReportsPerRequest = 256

func (a *API) listNodes(w http.ResponseWriter, r *http.Request) {
	list, err := a.store.ListNodes(r.Context())
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Продавцу нужна не просто «нода включена», а «сколько людей до неё не
	// доходит». Без этого он узнаёт о блокировке из обращений в поддержку.
	health, err := a.store.NodeHealth(r.Context())
	if err != nil {
		log.Printf("сводка доступности: %v", err)
		health = map[int64]Health{}
	}

	type view struct {
		Node
		Health    Health `json:"health"`
		Degraded  bool   `json:"degraded"`
		Throttled bool   `json:"throttled"`
		FetchKbps int64  `json:"fetch_kbps,omitempty"`
	}

	views := make([]view, 0, len(list))
	for _, n := range list {
		h := health[n.ID]
		views = append(views, view{Node: n, Health: h, Degraded: h.Degraded(), Throttled: h.Throttled(), FetchKbps: h.FetchKbps()})
	}

	ok(w, map[string]any{"nodes": views})
}

func (a *API) createNode(w http.ResponseWriter, r *http.Request) {
	var p CreateNodeParams
	if !decode(w, r, &p) {
		return
	}
	if p.Address == "" {
		fail(w, http.StatusBadRequest, "нужен адрес ноды в виде host:port")
		return
	}

	node, token, err := a.store.CreateNode(r.Context(), p)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	a.record(r, EventNodeCreate, Event{NodeID: node.ID})
	ok(w, map[string]any{
		"node": node,
		// Токен ноды тоже отдаётся один раз: в базе только его хеш.
		"token": token,
	})
}

// updateNode переименовывает ноду, проставляет ей страну и выключает её.
//
// Выключение — это то, что делают в первую очередь при подозрении на
// блокировку: нода уходит из подписок, но её статистика остаётся, и решение
// отменяется обратным запросом.
func (a *API) updateNode(w http.ResponseWriter, r *http.Request) {
	id, okID := pathID(w, r)
	if !okID {
		return
	}
	var p UpdateNodeParams
	if !decode(w, r, &p) {
		return
	}
	if p.Name != nil && strings.TrimSpace(*p.Name) == "" {
		fail(w, http.StatusBadRequest, "имя ноды не может быть пустым")
		return
	}
	if p.Fingerprint != nil {
		if _, err := normalFingerprint(*p.Fingerprint); err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if p.ExitID != nil {
		// Выход меняют только у входа: обычная нода, ставшая «входом»
		// правкой, осталась бы с ключами и покупателями, которых вход
		// знать не должен.
		current, err := a.store.GetNode(r.Context(), id)
		if err != nil {
			respondStoreErr(w, err)
			return
		}
		if current.ExitID == 0 {
			fail(w, http.StatusBadRequest, "выход меняют только у входа каскада")
			return
		}
		if *p.ExitID == id {
			fail(w, http.StatusBadRequest, "вход не может вести сам в себя")
			return
		}
		if err := a.checkExit(r.Context(), *p.ExitID); err != nil {
			fail(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	node, err := a.store.UpdateNode(r.Context(), id, p)
	var bad inputError
	if errors.As(err, &bad) {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	if err != nil {
		respondStoreErr(w, err)
		return
	}
	a.record(r, EventNodeUpdate, Event{NodeID: node.ID, Detail: nodeUpdateDetail(p)})
	ok(w, map[string]any{"node": node})
}

func (a *API) deleteNode(w http.ResponseWriter, r *http.Request) {
	id, okID := pathID(w, r)
	if !okID {
		return
	}
	if err := a.store.DeleteNode(r.Context(), id); err != nil {
		respondStoreErr(w, err)
		return
	}
	a.record(r, EventNodeDelete, Event{Detail: "вместе с её статистикой"})
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) nodeUsers(w http.ResponseWriter, r *http.Request, n Node) {
	if n.ExitID != 0 {
		a.entrySync(w, r, n)
		return
	}
	list, err := a.store.NodeUsers(r.Context(), n.ID)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	relays, err := a.store.RelayIPs(r.Context(), n.ID)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	guard, err := a.store.GuardSettings(r.Context())
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	// upgrade_to — просьба обновиться, если продавец её дал. Нода передаёт
	// её службе обновления; что ставить, та решает сама (internal/updater).
	// relays — адреса входов каскада, от которых верить адресу покупателя.
	// guard — пределы на приёме и закрытые адреса (guard.go).
	ok(w, map[string]any{"users": list, "upgrade_to": n.UpgradeTo, "relays": relays, "guard": nodeGuard(guard)})
}

// maxCoverNames — сколько имён прикрытия панель принимает от ноды за раз.
const maxCoverNames = 16

func (a *API) nodeUsage(w http.ResponseWriter, r *http.Request, n Node) {
	var body struct {
		Usage map[string]users.Usage `json:"usage"`

		// Presence — кто на связи прямо сейчас. Пустое означает «никого»: нода
		// шлёт только тех, у кого есть соединения, а панель обнуляет остальных.
		Presence map[string]users.Presence `json:"presence"`

		// SNIExtra — имена прикрытия, которые нода принимает на самом деле.
		// Она сообщает их сама, чтобы один и тот же список не приходилось
		// держать руками и на ноде, и в панели: разойдясь, они молча ломают
		// подключение — клиент стучится именем, которого нода не знает.
		SNIExtra []string `json:"sni_extra"`

		// Version и Arch — сборка ноды: по ним панель видит, какие ноды
		// отстали от релиза.
		Version string `json:"version"`
		Arch    string `json:"arch"`

		// Guard — сколько соединений нода отклонила с прошлого отчёта:
		// только числа, адресов нода не присылает.
		Guard connguard.Stats `json:"guard"`

		// Sys — нагрузка машины ноды (fleet.go).
		Sys NodeSys `json:"sys"`
	}
	if !decodeFromNode(w, r, &body) {
		return
	}
	if err := a.store.ReportGuard(r.Context(), n.ID, body.Guard, time.Now()); err != nil {
		log.Printf("отклонённые нодой %d не сохранились: %v", n.ID, err)
	}
	if countryUnknown(n.Country) {
		a.fillCountry(n.ID, n.Country, n.Address)
	}
	if err := a.store.ReportNodeBuild(r.Context(), n.ID, clip(body.Version, 40), clip(body.Arch, 16)); err != nil {
		log.Printf("версия ноды %d не сохранилась: %v", n.ID, err)
	}
	// Нода могла только что дообновиться — очередь идёт дальше сразу, а не
	// через минуту (fleet.go).
	if body.Version != "" {
		if _, err := a.store.RollNodeUpgrades(r.Context(), time.Now()); err != nil {
			log.Printf("очередь обновления нод: %v", err)
		}
	}
	if err := a.store.ReportNodeSys(r.Context(), n.ID, body.Sys, time.Now()); err != nil {
		log.Printf("нагрузка ноды %d не сохранилась: %v", n.ID, err)
	}

	// Пишем, только когда список и правда изменился: отчёт приходит каждые
	// несколько секунд, и запись на каждый из них ни к чему.
	// Список уходит в подписку каждого покупателя, а присылает его нода — то
	// есть кто угодно с её токеном. Режем до разумного: имя в DNS не длиннее
	// 253 знаков, а живой ноде хватает пары десятков имён.
	var reported []string
	for _, name := range body.SNIExtra {
		if len(reported) == maxCoverNames {
			break
		}
		if len(name) <= 253 {
			reported = append(reported, name)
		}
	}
	if !sameNames(n.SNIExtra, reported, n.SNI) {
		names := reported
		if _, err := a.store.UpdateNode(r.Context(), n.ID, UpdateNodeParams{SNIExtra: &names}); err != nil {
			log.Printf("имена прикрытия ноды %d не сохранились: %v", n.ID, err)
		}
	}

	if err := a.store.ReportUsage(r.Context(), n.ID, body.Usage); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := a.store.ReportPresence(r.Context(), n.ID, body.Presence); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// subscription отдаёт список нод по токену подписки.
//
// Формат — как у всех: строки со ссылками, свёрнутые в base64. Плюс заголовок
// subscription-userinfo, по которому клиенты рисуют остаток трафика и дату
// окончания. Мелочь, но именно её продавцу приходится объяснять покупателям
// голосом, если её нет.
func (a *API) subscription(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	user, err := a.store.UserBySubToken(r.Context(), token)
	if errors.Is(err, ErrNotFound) {
		// Покупатель, переехавший с Marzban или 3x-ui, приходит со старым
		// адресом: его приложение обновляет подписку само и про переезд не
		// знает. Путь /sub/ у обеих панелей тот же, что у нас.
		user, err = a.store.UserByLegacySubToken(r.Context(), token)
	}
	if err != nil {
		// Не подсказываем, существует ли токен: перебор подписок — обычное
		// занятие тех, кто ищет чужие ноды.
		http.NotFound(w, r)
		return
	}
	a.serveSubscription(w, r, user)
}

// legacySubscription отдаёт подписку по пути прежней панели, когда он был не
// /sub/: у 3x-ui его часто делают случайным. Любой другой путь — 404, как и
// раньше.
func (a *API) legacySubscription(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.NotFound(w, r)
		return
	}
	if !a.store.IsLegacySubPath(r.Context(), r.PathValue("prefix")) {
		http.NotFound(w, r)
		return
	}
	user, err := a.store.UserByLegacySubToken(r.Context(), r.PathValue("token"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	a.serveSubscription(w, r, user)
}

func (a *API) serveSubscription(w http.ResponseWriter, r *http.Request, user User) {
	// Адрес один, ответы разные: прокси не должен сохранить страницу вместо
	// подписки, а браузер — хранить токен и одноразовый nonce на диске.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Vary", "Accept, User-Agent, Accept-Language, Sec-Fetch-Mode, Sec-Fetch-Dest")
	if wantsBuyerPage(r) {
		a.buyerPage(w, r, user)
		return
	}

	nodes, err := a.store.SubscriptionNodes(r.Context(), user.NodeGroup)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Ноды идут в том порядке, в каком их стоит пробовать: сверху те, на
	// которые не жалуются и которые отвечают быстрее. Ничего не выбрасываем —
	// отчёты приходят от недоверенных клиентов, и один вредитель не должен
	// лишать остальных рабочей ноды.
	if health, err := a.store.NodeHealth(r.Context()); err == nil {
		nodes = RankNodes(nodes, health)
	} else {
		log.Printf("сводка доступности: %v", err)
	}

	w.Header().Set("Subscription-Userinfo", userInfoHeader(user))
	w.Header().Set("Profile-Update-Interval", "12")

	// Поддержка, «Продлить» и объявление — для чужих приложений заголовками,
	// для нашего клиента ещё и полями JSON. Не прочиталось — подписка всё
	// равно уходит: без кнопок жить можно, без нод нельзя.
	info, err := a.store.SellerInfo(r.Context())
	if err != nil {
		log.Printf("настройки связи с покупателем: %v", err)
	}
	// Имя профиля — метка доступа: её покупатель и так видит в имени каждой
	// ноды (см. StockLinks), и в приложении профиль зовётся так же. Без метки
	// Hiddify берёт последний сегмент адреса — токен подписки. Проверяем уже
	// очищенный заголовок: одни пробелы или управляющие тоже не дают имени.
	info.WriteHeader(w.Header(), user.Label)
	if w.Header().Get(seller.HeaderTitle) == "" {
		w.Header().Set(seller.HeaderTitle, seller.Encode("Marvia"))
	}

	if r.URL.Query().Get("format") == "json" {
		a.subscriptionJSON(w, user, nodes, info)
		return
	}

	// Готовые конфиги для sing-box и Clash — только по явной просьбе, см.
	// formats.go. Нет ни VLESS, ни Trojan — честно говорим, чего не хватает:
	// пустой конфиг приложение приняло бы молча и не подключилось.
	switch r.URL.Query().Get("format") {
	case "singbox", "sing-box", "clash", "mihomo":
		eps := stockEndpoints(nodes, user.Credentials, user.Label)
		var (
			body []byte
			err  error
			kind = "application/json; charset=utf-8"
		)
		if f := r.URL.Query().Get("format"); f == "clash" || f == "mihomo" {
			body, err = clashConfig(eps)
			kind = "text/yaml; charset=utf-8"
		} else {
			body, err = singBoxConfig(eps)
		}
		if err != nil {
			http.Error(w, "у этого доступа нет VLESS или Trojan: попросите продавца выдать их в панели", http.StatusConflict)
			return
		}
		w.Header().Set("Content-Type", kind)
		_, _ = w.Write(body)
		return
	}

	// По умолчанию отдаём то, что понимают чужие приложения. Наши ссылки
	// сюда не попадают намеренно: на незнакомой схеме часть клиентов
	// спотыкается и не принимает подписку целиком.
	body := strings.Join(StockLinks(nodes, user.Credentials, user.Label), "\n")

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if r.URL.Query().Get("format") == "raw" {
		_, _ = w.Write([]byte(body))
		return
	}
	_, _ = w.Write([]byte(base64.StdEncoding.EncodeToString([]byte(body))))
}

// subscriptionJSON — подписка для нашего клиента.
//
// Секретов здесь нет и быть не должно: свой ключ клиент получил один раз в
// ссылке аккаунта, а список нод он обновляет постоянно и по открытому каналу.
func (a *API) subscriptionJSON(w http.ResponseWriter, user User, nodes []Node, info seller.Info) {
	// Транспорт передаётся явно: нашему клиенту нужно знать, как именно
	// подключаться, а разбирать это из ссылки — лишний источник расхождений
	// между тем, что собрала панель, и тем, что понял клиент.
	type nodeView struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`

		// Country — то, что продавец написал руками. Флаг клиент подбирает
		// сам: держать картинки на стороне панели незачем.
		Country string `json:"country,omitempty"`
		Address string `json:"address"`
		SNI     string `json:"sni,omitempty"`

		// SNIExtra — запасные имена прикрытия. Клиент выбирает из них на
		// каждое соединение; клиенты постарше поля не знают и ходят по SNI,
		// как раньше.
		SNIExtra  []string `json:"sni_extra,omitempty"`
		PublicKey string   `json:"public_key"`
		Link      string   `json:"link"`

		WSPath           string `json:"ws_path,omitempty"`
		QUIC             bool   `json:"quic,omitempty"`
		RealityPublicKey string `json:"reality_public_key,omitempty"`
		RealityShortID   string `json:"reality_short_id,omitempty"`

		// Fingerprint — чьё приветствие изображать. Клиенты постарше поля не
		// знают и остаются на Chrome, как раньше.
		Fingerprint string `json:"fingerprint,omitempty"`
	}

	views := make([]nodeView, 0, len(nodes))
	for _, n := range nodes {
		if !n.Enabled {
			continue
		}
		views = append(views, nodeView{
			ID: n.ID, Name: n.Name, Country: n.Country, Address: n.Address, SNI: n.SNI,
			SNIExtra:  n.SNIExtra,
			PublicKey: n.PublicKey, Link: NodeLink(n),
			WSPath:           n.WSPath,
			QUIC:             n.QUIC,
			RealityPublicKey: n.RealityPublicKey,
			RealityShortID:   n.RealityShortID,
			Fingerprint:      n.Fingerprint,
		})
	}

	answer := map[string]any{
		"nodes":         views,
		"traffic_limit": user.TrafficLimit,
		"used":          user.Used,
		"expires_at":    user.ExpiresAt,
	}
	// Приложения с версиями — чтобы клиент сам заметил, что устарел. Play
	// может не показывать приложение в РФ, а панель раздаёт его молча.
	if apps := a.appOffers(user.SubToken); apps != nil {
		answer["apps"] = apps
	}
	// Пустые поля не отдаём вовсе: клиент постарше их не знает, а новый
	// читает отсутствие как «выключено».
	if info.SupportURL != "" {
		answer["support_url"] = info.SupportURL
	}
	if info.RenewURL != "" {
		answer["renew_url"] = info.RenewURL
	}
	if info.Announce != "" {
		answer["announce"] = info.Announce
	}
	ok(w, answer)
}

// userInfoHeader формирует заголовок с остатком квоты.
func userInfoHeader(u User) string {
	parts := []string{
		"upload=0",
		fmt.Sprintf("download=%d", u.Used),
		fmt.Sprintf("total=%d", u.TrafficLimit),
	}
	if u.ExpiresAt != nil {
		parts = append(parts, fmt.Sprintf("expire=%d", u.ExpiresAt.Unix()))
	}
	return strings.Join(parts, "; ")
}

func bearer(r *http.Request) string {
	head := r.Header.Get("Authorization")
	if after, found := strings.CutPrefix(head, "Bearer "); found {
		return strings.TrimSpace(after)
	}
	return ""
}

func pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		fail(w, http.StatusBadRequest, "идентификатор должен быть числом")
		return 0, false
	}
	return id, true
}

func decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	return decodeBody(w, r, dst, true)
}

// decodeFromNode разбирает отчёт ноды, не споткнувшись о незнакомые поля.
//
// Строгость decode хороша для людей и ботов: опечатка в имени поля должна
// стать ошибкой, а не молча пропасть. Ноде она вредит. Ноды обновляются не
// разом с панелью, и нода новее панели шлёт поля, которых панель ещё не
// знает; строгий разбор отвечал бы ей 400 на каждый отчёт, и расход, «на
// связи» и имена прикрытия переставали бы доходить — ровно так было с полем
// version. Поэтому в /node/* поля только добавляются, а панель лишнее
// пропускает.
func decodeFromNode(w http.ResponseWriter, r *http.Request, dst any) bool {
	return decodeBody(w, r, dst, false)
}

func decodeBody(w http.ResponseWriter, r *http.Request, dst any, strict bool) bool {
	defer r.Body.Close()
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if strict {
		dec.DisallowUnknownFields()
	}
	if err := dec.Decode(dst); err != nil {
		fail(w, http.StatusBadRequest, "не разобрал тело запроса: "+err.Error())
		return false
	}
	return true
}

func ok(w http.ResponseWriter, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		// Ошибка записи в оборванное соединение несёт оба его конца: «write
		// tcp 192.0.2.1:443->203.0.113.7:51234: broken pipe». Второй — это
		// покупатель, пришедший за подпиской.
		log.Printf("отправка ответа: %s", redact.Error(err))
	}
}

func fail(w http.ResponseWriter, code int, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}

func respondStoreErr(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrNotFound) {
		fail(w, http.StatusNotFound, "не найдено")
		return
	}
	fail(w, http.StatusInternalServerError, err.Error())
}

// ParseExpiry разбирает срок подписки, заданный человеком: либо дата, либо
// «через сколько», как «30d» или «12h». Второе удобнее боту: он продаёт месяц,
// а не «до 23 сентября».
func ParseExpiry(s string) (*time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	if d, err := time.ParseDuration(strings.Replace(s, "d", "0h", 1)); err == nil && strings.HasSuffix(s, "d") {
		days, convErr := strconv.Atoi(strings.TrimSuffix(s, "d"))
		if convErr == nil {
			t := time.Now().UTC().Add(time.Duration(days) * 24 * time.Hour)
			return &t, nil
		}
		t := time.Now().UTC().Add(d)
		return &t, nil
	}
	if d, err := time.ParseDuration(s); err == nil {
		t := time.Now().UTC().Add(d)
		return &t, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return nil, fmt.Errorf("не понял срок %q: нужна дата в формате RFC3339 либо длительность вида 30d, 12h", s)
	}
	utc := t.UTC()
	return &utc, nil
}

// ParseDuration разбирает срок продления: «30d», «12h», «90m».
//
// time.ParseDuration дней не знает, а бот продаёт именно месяцы и дни, а не
// семьсот двадцать часов.
func ParseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil || n <= 0 {
			return 0, fmt.Errorf("не понял срок %q: нужно число дней, например 30d", s)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("не понял срок %q: нужно 30d, 12h или 90m", s)
	}
	return d, nil
}

// WithPanelIPs вписывает адреса панели в выдаваемые ссылки доступа.
//
// Отдельным вызовом, а не ещё одним доводом конструктора: адреса не нужны для
// работы панели и появляются позже остального — их выясняет команда запуска.
func (a *API) WithPanelIPs(ips []string) *API {
	a.panelIPs = ips
	return a
}

// WithVersion сообщает панели её версию сборки.
func (a *API) WithVersion(v string) *API {
	a.version = v
	return a
}

// listKeys перечисляет живые ключи доступа.
func (a *API) listKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := a.store.ListAPIKeys(r.Context())
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	ok(w, map[string]any{"keys": keys})
}

// createKey выпускает ключ. Секрет уходит в ответ один раз и больше нигде не
// появляется — в базе от него только хеш.
func (a *API) createKey(w http.ResponseWriter, r *http.Request) {
	var p struct {
		Name   string   `json:"name"`
		Scopes []string `json:"scopes"`
	}
	if !decode(w, r, &p) {
		return
	}

	key, secret, err := a.store.CreateAPIKey(r.Context(), p.Name, p.Scopes)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}

	// В журнал уходит имя ключа и его права, но не сам ключ.
	a.record(r, EventKeyCreate, Event{Detail: "«" + key.Name + "», права: " + strings.Join(key.Scopes, ", ")})
	ok(w, map[string]any{
		"key":    key,
		"secret": secret,
		"note":   "секрет показывается один раз: в базе лежит только его хеш",
	})
}

// revokeKey отзывает ключ.
func (a *API) revokeKey(w http.ResponseWriter, r *http.Request) {
	id, okID := pathID(w, r)
	if !okID {
		return
	}
	if err := a.store.RevokeAPIKey(r.Context(), id); err != nil {
		if errors.Is(err, ErrKeyNotFound) {
			fail(w, http.StatusNotFound, "ключа с таким номером нет или он уже отозван")
			return
		}
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	a.record(r, EventKeyRevoke, Event{})
	w.WriteHeader(http.StatusNoContent)
}

// rotateSubToken меняет ссылку подписки, не трогая доступ.
//
// Ссылку подписки покупатели раздают знакомым: она попадает в переписки, на
// форумы и в чужие приложения, а по ней отдаются список нод и секреты vless с
// trojan. Продавцу нужен ответ мягче, чем отзыв всего доступа, — иначе за одну
// утёкшую ссылку он теряет покупателя.
//
// Старая ссылка умирает сразу — вместе с ней перестаёт работать и ссылка
// доступа покупателя: в ней зашит путь /sub/<токен>, по которому приложение
// забирает список нод. Поэтому смена адреса подписки всегда идёт в паре с
// выдачей нового набора доступа: POST /users/{id}/credentials.
//
// И это не отменяет отзыва утёкших наборов. Тот, кто успел прочитать старую
// подписку, унёс из неё секреты vless и trojan, и они действуют, пока их не
// отозвали: смена адреса закрывает будущие чтения, а не прошлые.
func (a *API) rotateSubToken(w http.ResponseWriter, r *http.Request) {
	id, okID := pathID(w, r)
	if !okID {
		return
	}

	user, err := a.store.RotateSubToken(r.Context(), id)
	if err != nil {
		respondStoreErr(w, err)
		return
	}

	a.record(r, EventSubToken, Event{UserID: user.ID, Detail: "сменён"})
	ok(w, map[string]any{
		"user": noteFor(r, user),
		// Секретов здесь нет: меняется только адрес подписки, наборы доступа
		// остаются прежними, и заново их панель не выдаёт.
		"links": a.links(r.Context(), user, nil),
	})
}

// stats отдаёт историю расхода: по суткам и по нодам.
//
// Числа считает панель, а не браузер: у продавца может быть тысяча покупателей
// и год истории, и тащить это в страницу целиком ради трёх графиков — способ
// подвесить его ноутбук.
func (a *API) stats(w http.ResponseWriter, r *http.Request) {
	days := 30
	if raw := strings.TrimSpace(r.URL.Query().Get("days")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			fail(w, http.StatusBadRequest, "days: нужно положительное число дней")
			return
		}
		// Год с запасом. Больше — не отказ, а тихое обрезание: график за пять
		// лет всё равно нечитаем, а запрос на такую выборку легко сделать
		// случайно.
		days = min(n, 400)
	}

	byDay, err := a.store.UsageByDay(r.Context(), days)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	byNode, err := a.store.UsageByNode(r.Context(), days)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}

	var total int64
	for _, d := range byDay {
		total += d.Up + d.Down
	}

	ok(w, map[string]any{
		"days": days,
		// Пустой список отдаём списком, а не null.
		//
		// В Go пустой срез превращается в JSON как null, и на свежей панели
		// вся вкладка статистики падала на попытке узнать его длину: продавец
		// в первый же заход видел ошибку вместо «трафика пока нет». Разница
		// между «пусто» и «сломалось» тут стоит одной строки.
		"by_day":  emptyIfNil(byDay),
		"by_node": emptyIfNil(byNode),
		"total":   total,
	})
}

// emptyIfNil заменяет несуществующий список пустым.
//
// Клиент не должен различать «поля нет», «поле null» и «поле пустое»: это три
// написания одного и того же, и каждое надо было бы проверять отдельно.
func emptyIfNil[T any](list []T) []T {
	if list == nil {
		return []T{}
	}
	return list
}

// bypassRoutes отдаёт российские подсети, которые клиент ведёт мимо туннеля.
//
// Зачем это нужно. Нода стоит за границей, и для госуслуг, банков и всего
// государственного человек оказывается иностранцем — они просто не отвечают.
// Клиент исключает эти подсети из туннеля, и такие сайты открываются с
// настоящего адреса.
//
// Список отдаёт панель, а не приложение носит его в себе: он меняется раз в
// месяц, а обновление приложения у покупателя упирается в магазин, который в
// нужный момент как раз и не работает.
func (a *API) bypassRoutes(w http.ResponseWriter, r *http.Request) {
	if _, err := a.store.UserBySubToken(r.Context(), r.PathValue("token")); err != nil {
		// Как и подписка: не подсказываем, существует ли токен.
		http.NotFound(w, r)
		return
	}

	prefixes, err := routes.RussianPrefixes()
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}

	w.Header().Set("Cache-Control", "public, max-age=86400")
	ok(w, map[string]any{"prefixes": prefixes})
}

// getAlerts отдаёт настройки оповещений без токена бота.
//
// Токен наружу не уходит даже админу: страница его не показывает, а отдать —
// значит положить его в историю браузера и в любой перехват ответа. Вместо
// него признак «задан»: страница по нему рисует «токен сохранён» и предлагает
// заменить, а не показать.
func (a *API) getAlerts(w http.ResponseWriter, r *http.Request) {
	cfg, err := a.store.AlertSettings(r.Context())
	if err != nil {
		respondStoreErr(w, err)
		return
	}
	ok(w, map[string]any{
		"enabled":   cfg.Enabled,
		"chat_id":   cfg.ChatID,
		"has_token": cfg.BotToken != "",
	})
}

// setAlerts сохраняет настройки оповещений.
//
// Пустой токен в запросе означает «оставить прежний», а не «стереть»: страница
// его не показывает, и отправить обратно то, чего она не знает, она не может.
// Стирается токен вместе с выключением оповещений.
func (a *API) setAlerts(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Enabled  bool   `json:"enabled"`
		BotToken string `json:"bot_token"`
		ChatID   string `json:"chat_id"`
	}
	if !decode(w, r, &body) {
		return
	}

	current, err := a.store.AlertSettings(r.Context())
	if err != nil {
		respondStoreErr(w, err)
		return
	}

	next := AlertSettings{
		Enabled:  body.Enabled,
		BotToken: strings.TrimSpace(body.BotToken),
		ChatID:   strings.TrimSpace(body.ChatID),
	}
	if next.BotToken == "" {
		next.BotToken = current.BotToken
	}
	if !next.Enabled {
		// Выключили — значит и токену бота в базе делать нечего.
		next.BotToken = ""
	}

	if next.Enabled && (next.BotToken == "" || next.ChatID == "") {
		fail(w, http.StatusBadRequest, "для оповещений нужны и токен бота, и адрес чата")
		return
	}

	if err := a.store.SetAlertSettings(r.Context(), next); err != nil {
		respondStoreErr(w, err)
		return
	}
	// Токена бота в журнале нет и быть не может: только сам факт правки.
	if next.Enabled {
		a.record(r, EventAlertsUpdate, Event{Detail: "оповещения включены"})
	} else {
		a.record(r, EventAlertsUpdate, Event{Detail: "оповещения выключены"})
	}
	w.WriteHeader(http.StatusNoContent)
}

// sameNames сравнивает списки имён без оглядки на порядок и повторы.
//
// Нода присылает то, что ей задали флагом, а панель хранит уже очищенное:
// без пустых, без повторов и без основного имени. Сравнивать их построчно
// значило бы переписывать запись на каждом отчёте.
func sameNames(have, got []string, primary string) bool {
	var clean []string
	for _, n := range splitNames(strings.Join(got, ",")) {
		// Основное имя нода шлёт вместе с запасными — оно у неё в том же
		// флаге. Панель хранит их отдельно, и сравнивать надо с тем же
		// набором, который она сохранит.
		if n != primary {
			clean = append(clean, n)
		}
	}
	if len(clean) != len(have) {
		return false
	}
	seen := make(map[string]bool, len(have))
	for _, n := range have {
		seen[n] = true
	}
	for _, n := range clean {
		if !seen[n] {
			return false
		}
	}
	return true
}

// actorKey — под каким ключом в контексте запроса лежит тот, кто действует.
//
// Своим типом, а не строкой: строковый ключ в context — это общая яма, куда
// два пакета однажды кладут разное под одним именем.
type actorKeyType struct{}

var actorKey actorKeyType

// withActor помечает запрос тем, кто его сделал: админ или имя ключа бота.
func withActor(r *http.Request, actor string) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), actorKey, actor))
}

// secretsKeyType — может ли тот, кто спрашивает, видеть секреты покупателей.
type secretsKeyType struct{}

var secretsKey secretsKeyType

// withSecrets помечает запрос правом видеть токены подписок и секреты vless
// с trojan. Это право users: кто может выдать покупателю доступ, тот может и
// посмотреть выданный.
func withSecrets(r *http.Request, allowed bool) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), secretsKey, allowed))
}

// visible прячет секреты покупателей от тех, кому положено только смотреть.
//
// Право read раздают доске с показателями и второму человеку — тем, кому
// менять ничего не надо. А токен подписки и секрет vless или trojan — это
// сам доступ: по токену отдаются ссылки с секретами, по секрету пускает
// нода. Без этого ключ «только смотреть» уносил бы доступ всех покупателей
// разом, хотя ссылки (GET /users/{id}/links) ему закрыты правом users.
//
// Публичный ключ vp1 остаётся: секретом он не является, а страница по нему
// отличает наборы друг от друга.
//
// Нет пометки — значит запрос пришёл мимо scoped, и считаем его чужим:
// показать лишнее хуже, чем не показать.
// noteFor убирает заметку продавца из ответа всем, кроме админа.
func noteFor(r *http.Request, u User) User {
	if actorOf(r) != ActorAdmin {
		u.Note = ""
	}
	return u
}

func visible(r *http.Request, list []User) []User {
	// Заметка — для продавца в панели, а не для бота: «должен за прошлый
	// месяц» не должно уезжать в чужие системы вместе со списком
	// покупателей, даже ключу с правом на секреты.
	if actorOf(r) != ActorAdmin {
		stripped := make([]User, len(list))
		for i, u := range list {
			stripped[i] = noteFor(r, u)
		}
		list = stripped
	}
	if allowed, _ := r.Context().Value(secretsKey).(bool); allowed {
		return list
	}
	out := make([]User, len(list))
	for i, u := range list {
		u.SubToken = ""
		creds := make([]Credential, len(u.Credentials))
		for j, c := range u.Credentials {
			if c.Kind != CredVP1 {
				c.Secret = ""
			}
			creds[j] = c
		}
		if u.Credentials == nil {
			creds = nil
		}
		u.Credentials = creds
		out[i] = u
	}
	return out
}

// actorOf достаёт того, кто действует. Пусто не бывает у ручек, закрытых
// проверкой токена, но на всякий случай считаем такое действие админским:
// потерять запись хуже, чем записать её без имени.
func actorOf(r *http.Request) string {
	if v, ok := r.Context().Value(actorKey).(string); ok && v != "" {
		return v
	}
	return ActorAdmin
}

// record кладёт запись в журнал от имени того, кто сделал запрос.
func (a *API) record(r *http.Request, action string, e Event) {
	// Правка без пояснения — это PATCH, который ничего человеческого не
	// поменял. Журнал из таких строк только тяжелеет.
	//
	// У остальных действий пустое пояснение законно: «заведён покупатель»
	// исчерпывающе описано самим видом события, и дописывать к нему «заведён»
	// значит повторяться дважды в одной строке.
	if e.Detail == "" && (action == EventUserUpdate || action == EventNodeUpdate) {
		return
	}
	e.Action = action
	e.Actor = actorOf(r)
	a.store.Record(r.Context(), e)
}

// updateDetail описывает правку подписчика словами для журнала.
//
// Именно словами, а не дампом полей: журнал читают глазами, и «продлён на
// 30 дней» полезнее, чем JSON с семью null.
func updateDetail(p UpdateUserParams, extend string) string {
	var parts []string
	if extend != "" {
		parts = append(parts, "продлён на "+extend)
	}
	if p.Enabled != nil {
		if *p.Enabled {
			parts = append(parts, "включён")
		} else {
			parts = append(parts, "отключён")
		}
	}
	if p.ExpiresAt != nil {
		parts = append(parts, "назначен срок")
	}
	if p.NoExpiry {
		parts = append(parts, "срок снят")
	}
	if p.TrafficLimit != nil {
		parts = append(parts, "изменена квота")
	}
	if p.SpeedLimit != nil {
		parts = append(parts, "изменён потолок скорости")
	}
	if p.MaxIPs != nil || p.MaxConns != nil {
		parts = append(parts, "изменены лимиты устройств")
	}
	if p.Label != nil {
		parts = append(parts, "переименован")
	}
	if p.Note != nil {
		parts = append(parts, "изменена заметка")
	}
	if p.Tags != nil {
		parts = append(parts, "изменены метки")
	}
	if len(parts) == 0 {
		return "правка без изменений"
	}
	return strings.Join(parts, ", ")
}

// nodeUpdateDetail — то же самое для ноды.
func nodeUpdateDetail(p UpdateNodeParams) string {
	var parts []string
	if p.Enabled != nil {
		if *p.Enabled {
			parts = append(parts, "включена")
		} else {
			parts = append(parts, "выключена")
		}
	}
	if p.Name != nil {
		parts = append(parts, "переименована")
	}
	if p.Country != nil {
		parts = append(parts, "изменена страна")
	}
	if p.Address != nil {
		parts = append(parts, "изменён адрес")
	}
	if p.Fingerprint != nil {
		parts = append(parts, "изменён отпечаток TLS")
	}
	if p.ExitID != nil {
		parts = append(parts, "сменён выход")
	}
	// Имена прикрытия нода присылает сама, каждым отчётом. Писать о них в
	// журнал значило бы забивать его строками, которых человек не делал.
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, ", ")
}

// listEvents отдаёт последние записи журнала.
func (a *API) listEvents(w http.ResponseWriter, r *http.Request) {
	limit := 200
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}

	list, err := a.store.Events(r.Context(), limit)
	if err != nil {
		respondStoreErr(w, err)
		return
	}
	if list == nil {
		list = []Event{}
	}
	ok(w, map[string]any{"events": list})
}
