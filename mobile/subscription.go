package mobile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/jytt8u/marvia/internal/client"
	"github.com/jytt8u/marvia/internal/foreign"
	"github.com/jytt8u/marvia/internal/vp1"
)

// SubscriptionView — подписка глазами экрана «Серверы»: ноды, срок, остаток.
//
// Экран показывает серверы и до подключения — это список того, что человеку
// продали, а не того, что сейчас работает. Поэтому подписка читается отдельно
// от туннеля, и по возможности из кэша: каждый поход в панель — это запрос
// имени её домена, а он уходит провайдеру открытым текстом.
type SubscriptionView struct {
	Nodes []NodeView `json:"nodes"`

	// Until — до какого числа оплачено, «2026-09-27»; пусто — без срока.
	Until string `json:"until,omitempty"`
	// Limit и Left — байты; ноль в Limit — без ограничения.
	Limit int64 `json:"limit"`
	Left  int64 `json:"left"`

	// FetchedAt — когда список получен из панели, unix-секунды. Экран пишет
	// по нему «обновлено 1 ч назад», а не выдумывает свежесть.
	FetchedAt int64 `json:"fetched_at"`
	// Stale — список из протухшего кэша: панель не ответила, показываем
	// вчерашний. Человеку это надо видеть, а не догадываться.
	Stale bool `json:"stale"`

	// SupportURL, RenewURL, Announce — поддержка, «Продлить» и объявление
	// продавца; пусто — не задано. Ядро их уже проверило: только https:// и
	// tg://, текст без управляющих символов и символов направления письма.
	// Приложению остаётся отдать ссылку системе как есть.
	SupportURL string `json:"support_url,omitempty"`
	RenewURL   string `json:"renew_url,omitempty"`
	Announce   string `json:"announce,omitempty"`

	// Remind — о чём напомнить: "expiry" (RemindValue — дней до конца) или
	// "traffic" (RemindValue — процентов трафика осталось) или "forecast"
	// (RemindValue — примерно дней, на сколько хватит трафика); пусто — не о
	// чем. RemindKey приложение запоминает, когда показало, и тот же ключ
	// второй раз не показывает. Подробности — client.Reminder.
	Remind      string `json:"remind,omitempty"`
	RemindValue int64  `json:"remind_value,omitempty"`
	RemindKey   string `json:"remind_key,omitempty"`
}

// Subscription отдаёт подписку по ссылке доступа: из свежего кэша, иначе из
// панели; refresh — идти в панель, не глядя на кэш. Если панель молчит, а
// кэш есть, отдаётся кэш с пометкой Stale. Ошибка — только когда показать
// нечего совсем.
func Subscription(accountLink, cacheDir string, refresh bool) (string, error) {
	return subscription(accountLink, cacheDir, refresh, nil)
}

func subscription(accountLink, cacheDir string, refresh bool, httpClient *http.Client) (string, error) {
	if isForeign(accountLink) {
		if httpClient == nil {
			return foreignView(accountLink, cacheDir, refresh)
		}
		sub, fetched, stale, err := foreign.LoadWithClient(accountLink, foreign.CachePath(cacheDir, accountLink), refresh, httpClient)
		if err != nil {
			return "", fail(FailPanel, err)
		}
		return viewJSON(foreign.View(sub), fetched, stale)
	}
	account, err := client.ParseAccountLink(accountLink)
	if err != nil {
		return "", fail(FailAccount, fmt.Errorf("ссылка доступа: %w", err))
	}
	path := accountCachePath(cacheDir, account.SubscriptionURL)

	var cached client.CachedSubscription
	if path != "" {
		cached, err = client.LoadCache(path, account.SubscriptionURL)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			cached = client.CachedSubscription{}
		}
	}
	if !refresh && cached.Fresh() {
		return viewJSON(cached.Subscription, cached.FetchedAt, false)
	}

	ctx, cancel := context.WithTimeout(context.Background(), subscriptionTimeout)
	defer cancel()
	var sub client.Subscription
	if httpClient == nil {
		sub, err = client.FetchSubscription(ctx, account.SubscriptionURL, account.PanelIPs)
	} else {
		sub, err = client.FetchSubscriptionWithClient(ctx, account.SubscriptionURL, httpClient)
	}
	if err != nil {
		if len(cached.Nodes()) > 0 {
			return viewJSON(cached.Subscription, cached.FetchedAt, true)
		}
		return "", fail(FailPanel, err)
	}
	if path != "" {
		// Не сохранился кэш — не беда: в следующий раз сходим в панель снова.
		_ = client.SaveCache(path, account.SubscriptionURL, sub)
	}
	// Прогноз «хватит примерно на N дней» — по прошлому замеру из кэша
	// (client.Forecast); напоминание срока или остатка его перекрывает.
	var forecast client.Reminder
	if !cached.FetchedAt.IsZero() {
		prev := cached.Subscription
		forecast = client.Forecast(prev.Remaining(), prev.TrafficLimit, prev.ExpiresAt, cached.FetchedAt, sub, time.Now())
	}
	return viewJSONWith(sub, time.Now(), false, forecast)
}

// MeasureNodes меряет ноды подписки без туннеля, настоящим подключением к
// каждой, и отдаёт тот же список с временами. Занимает секунды.
//
// Когда туннель поднят, пользоваться этим не надо: замер уйдёт через сам
// туннель и покажет не то. Для поднятого есть Tunnel.Measure.
//
// settings — те же настройки, что уходят в Start: нода, до которой доходит
// только разрезанное приветствие, без дробления показалась бы мёртвой.
func MeasureNodes(accountLink, cacheDir, settings string) (string, error) {
	set := parseSettings(settings)
	if isForeign(accountLink) {
		return foreignMeasure(accountLink, cacheDir, set)
	}
	account, err := client.ParseAccountLink(accountLink)
	if err != nil {
		return "", fail(FailAccount, fmt.Errorf("ссылка доступа: %w", err))
	}
	key, err := vp1.KeyPairFromPrivate(account.PrivateKey)
	if err != nil {
		return "", fail(FailAccount, fmt.Errorf("личный ключ: %w", err))
	}

	raw, err := Subscription(accountLink, cacheDir, false)
	if err != nil {
		return "", err
	}
	var view SubscriptionView
	if err := json.Unmarshal([]byte(raw), &view); err != nil {
		return "", err
	}

	// Ноды с адресами есть только в кэше: наружу они не отдаются, в списке
	// экрана им делать нечего.
	cached, err := client.LoadCache(accountCachePath(cacheDir, account.SubscriptionURL), account.SubscriptionURL)
	if err != nil {
		return "", fail(FailPanel, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), measureTimeout)
	defer cancel()
	byID := make(map[int64]client.Measurement)
	for _, m := range client.MeasureAll(ctx, cached.Nodes(), key, set.dial()) {
		byID[m.Node.ID] = m
	}
	for i := range view.Nodes {
		m, ok := byID[view.Nodes[i].ID]
		if !ok {
			continue
		}
		view.Nodes[i].Alive = m.OK()
		view.Nodes[i].MS = m.PingMS()
		if m.OK() {
			view.Nodes[i].SetupMS = max(1, m.Latency.Milliseconds())
		}
	}
	out, err := json.Marshal(view)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func viewJSON(sub client.Subscription, fetched time.Time, stale bool) (string, error) {
	return viewJSONWith(sub, fetched, stale, client.Reminder{})
}

// viewJSONWith — то же, с запасным напоминанием: его показывают, только
// если своих поводов у подписки нет.
func viewJSONWith(sub client.Subscription, fetched time.Time, stale bool, fallback client.Reminder) (string, error) {
	// Напоминание считается на «сейчас», а не на момент, когда список
	// пришёл: из кэша суточной давности «осталось 3 дня» было бы неправдой.
	remind := sub.Reminder(time.Now())
	if remind.Kind == "" {
		remind = fallback
	}
	view := SubscriptionView{
		Nodes:       make([]NodeView, 0, len(sub.Nodes)),
		Limit:       sub.TrafficLimit,
		Left:        sub.Remaining(),
		FetchedAt:   fetched.Unix(),
		Stale:       stale,
		SupportURL:  sub.SupportURL,
		RenewURL:    sub.RenewURL,
		Announce:    sub.Announce,
		Remind:      remind.Kind,
		RemindValue: remind.Value,
		RemindKey:   remind.Key,
	}
	if until, set := sub.Until(); set {
		view.Until = until.Local().Format("2006-01-02")
	}
	for _, n := range sub.Nodes {
		view.Nodes = append(view.Nodes, NodeView{ID: n.ID, Name: n.Name, Country: n.Country, Endpoint: n.Address})
	}
	out, err := json.Marshal(view)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// accountCachePath — свой файл кэша на каждую подписку; см. client.CacheFile.
func accountCachePath(dir, subURL string) string {
	return client.CacheFile(dir, subURL)
}

// ForgetSubscription стирает кэш подписки: человек удалил ключ, и список
// его нод на телефоне оставаться не должен.
func ForgetSubscription(accountLink, cacheDir string) {
	if isForeign(accountLink) {
		foreign.ForgetCache(cacheDir, accountLink)
		return
	}
	account, err := client.ParseAccountLink(accountLink)
	if err != nil {
		return
	}
	if path := accountCachePath(cacheDir, account.SubscriptionURL); path != "" {
		_ = os.Remove(path)
	}
}

const subscriptionTimeout = 20 * time.Second

// BypassRoutes забирает у панели российские подсети — по строке на подсеть.
//
// Идёт тем же путём, что подписка: по адресам панели из ссылки, если они там
// есть. Ошибка — с понятной причиной, её приложение покажет под
// переключателем, а не «панель была недоступна» на всё подряд.
func BypassRoutes(accountLink string) (string, error) {
	if isForeign(accountLink) {
		// Список отдаёт панель Marvia; у чужой подписки его взять неоткуда.
		// Приложения мимо туннеля по-прежнему работают — там список не нужен.
		return "", fail(FailPanel, errors.New("этот список даёт только панель Marvia, а подписка чужая"))
	}
	account, err := client.ParseAccountLink(accountLink)
	if err != nil {
		return "", fail(FailAccount, fmt.Errorf("ссылка доступа: %w", err))
	}
	prefixes, err := client.FetchBypass(context.Background(), account.SubscriptionURL, account.PanelIPs)
	if err != nil {
		return "", fail(FailPanel, err)
	}
	return strings.Join(prefixes, "\n"), nil
}
