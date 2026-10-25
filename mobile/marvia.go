// Package mobile — то, что видит приложение на телефоне.
//
// Пакет собирается в библиотеку через gomobile bind, поэтому здесь намеренно
// нет ничего сложного в сигнатурах: только строки, числа и указатели на типы
// этого же пакета. Всё остальное gomobile переносить не умеет, а ловить это
// на этапе сборки под Android — худшее место для такого сюрприза.
//
// Разделение обязанностей такое. Приложение на Kotlin поднимает VpnService,
// получает от системы файловый дескриптор сетевого интерфейса и отдаёт его
// сюда. Дальше всё делает Go: разбирает пакеты, поднимает туннель, считает
// трафик. Логика не дублируется между платформами — на настольной машине тот
// же код работает под SOCKS вместо интерфейса.
package mobile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jytt8u/marvia/internal/client"
	"github.com/jytt8u/marvia/internal/foreign"
	"github.com/jytt8u/marvia/internal/redact"
	"github.com/jytt8u/marvia/internal/tunbridge"
	"github.com/jytt8u/marvia/internal/vp1"
)

// DefaultDNS — куда уходят перехваченные запросы имён.
//
// Только через туннель: запрос имени, ушедший мимо туннеля, выдаёт цензору
// весь список посещённых сайтов, даже когда сам трафик он расшифровать не
// может. Для известного резолвера — ещё и по HTTPS, чтобы этого списка не
// видела и нода (см. tunnelSettings.names).
const DefaultDNS = "1.1.1.1:53"

// CacheName — имя старого общего файла кэша подписки. Кэш теперь на каждую
// подписку свой (см. accountCachePath); имя осталось, чтобы стереть старый
// файл у тех, кто обновился.
const CacheName = "subscription.json"

// Сколько живёт сообщение о неудаче, прежде чем погаснуть.
//
// Минута: достаточно, чтобы человек успел прочитать, и мало, чтобы старая
// неудача не висела рядом с работающим туннелем.
const errorLifetime = time.Minute

// Сколько даём на замер всех нод и на переключение между ними.
//
// Замер идёт настоящими подключениями ко всем нодам разом, поэтому он не
// мгновенный; переключение — это обычное подключение, только к заданной ноде.
const (
	measureTimeout = 30 * time.Second
	selectTimeout  = 60 * time.Second
)

// Виды неудач.
//
// Текст ошибки из ядра точен и подробен, но покупателю он не говорит ничего:
// «dial tcp: lookup panel.example: no such host» — язык не для человека,
// который заплатил за интернет. Поэтому ядро сообщает вместе с подробностями
// ещё и вид неудачи, а человеческую фразу под него подбирает приложение.
//
// Разделение именно такое, а не «пусть ядро сразу пишет по-человечески»,
// потому что тогда русский текст интерфейса расползся бы по ядру. А он нужен
// будет ещё на фарси и на китайском — это те же страны, ради которых всё и
// затевалось.
//
// Вид едет первой строкой сообщения, подробности — следующими.
const (
	// FailAccount — ссылка доступа не та: испорчена при пересылке, обрезана,
	// выдана не нами.
	FailAccount = "account"

	// FailPanel — до панели продавца не достучались. Обычно это просто
	// отсутствие интернета, но может быть и блокировка самой панели.
	FailPanel = "panel"

	// FailNodes — панель ответила, но подключиться не вышло ни к одной ноде.
	FailNodes = "nodes"

	// FailSystem — не сложилось на стороне телефона: система не дала
	// интерфейс, не поднялся сетевой мост.
	FailSystem = "system"

	// FailExpired — кончился срок подписки.
	// FailQuota — кончился оплаченный трафик.
	//
	// Отдельно от FailNodes, и это главное: раньше кончившаяся подписка
	// показывалась как «ни один сервер не отвечает». Человек читал, что
	// сломались мы, шёл к продавцу с «у вас всё лежит», и продавец разбирался
	// с каждым таким вручную — хотя надо было одно слово «продли».
	FailExpired = "expired"
	FailQuota   = "quota"
)

// fail помечает ошибку видом: первая строка — вид, остальное — подробности.
func fail(kind string, err error) error {
	return fmt.Errorf("%s\n%w", kind, redact.Wrap(err))
}

func clientLog(format string, args ...any) {
	log.Print(redact.Secrets(fmt.Sprintf(format, args...)))
}

// Tunnel — работающее подключение.
type Tunnel struct {
	mu sync.Mutex

	// Не просто дозвон, а надзор над ним: он сам меняет ноду, когда текущая
	// замолчала, и подменяет её под мостом. Мост об этом не знает.
	dialer   client.Backend
	bridge   *tunbridge.Bridge
	settings tunnelSettings

	nodeName string
	running  bool

	// Последняя неудача и когда она случилась.
	//
	// Время нужно, чтобы забывать: отдельный неудавшийся поток — обычное дело
	// в интернете, а не поломка туннеля. Цель могла быть недоступна, заблокирована
	// или просто не иметь IPv6, до которого ноде не дотянуться. Показывать такое
	// рядом с надписью «Подключено» и не стирать значит приучить не читать эту
	// строку вовсе — а она понадобится, когда сломается что-то настоящее.
	lastError string
	lastErrAt time.Time

	// troubleCode — держащаяся беда: нода молчит, и переехать не на что.
	// Живёт до тех пор, пока надзор не скажет, что связь вернулась.
	troubleCode string

	// up и down — байты этой сессии. Счёт по дням ведёт приложение: ядро
	// живёт ровно столько, сколько поднят туннель, и хранить в нём историю
	// было бы обещанием, которого оно не выполнит.
	up, down atomic.Int64

	// quality — последние замеры отклика текущей ноды; см. quality.go.
	// whereCache — адрес выхода через туннель, до смены ноды; см. where.
	// dns — резолвер, отданный мосту: шаг DNS в «Диагностике» спрашивает
	// его тем же путём, что и приложения в туннеле.
	quality    qualityRing
	whereCache whereView
	dns        string
}

// Start поднимает туннель поверх сетевого интерфейса, полученного от системы.
//
// accountLink — ссылка, которую покупатель получил от бота.
// tunFD — дескриптор от VpnService.
// dns — адрес резолвера; пусто означает значение по умолчанию. Для
// известного резолвера он же выбирает, к кому идти по HTTPS.
// cacheDir — каталог приложения под кэш подписки; пусто означает работу без
// кэша, как было раньше.
// prefer — нода, выбранная человеком руками; ноль — автовыбор. Выбор живёт
// в приложении и переживает перезапуск туннеля: иначе страна, выбранная
// вчера, молча сменилась бы на «самую быструю» после перезагрузки.
// settings — настройки туннеля из приложения, JSON; см. tunnelSettings.
//
// Каталог передаёт приложение, а не выясняет ядро: на Android путь к своим
// файлам знает только Context, и достать его из Go неоткуда.
func Start(accountLink string, tunFD int, dns string, cacheDir string, prefer int64, settings string) (*Tunnel, error) {
	if tunFD <= 0 {
		return nil, fail(FailSystem, errors.New("не передан дескриптор сетевого интерфейса"))
	}

	attempt := NewConnectionAttempt()
	defer attempt.cancel()
	t, err := attempt.Connect(accountLink, cacheDir, prefer, settings)
	if err != nil {
		tunbridge.CloseFD(tunFD)
		return nil, err
	}
	if err := t.Attach(tunFD, dns); err != nil {
		_ = t.Stop()
		return nil, err
	}
	return t, nil
}

// connect делает всё, что не касается сетевого интерфейса: разбирает ссылку,
// забирает список нод и выбирает лучшую.
//
// Вынесено отдельно не ради красоты: так эту часть можно проверить тестом, не
// выдумывая дескриптор интерфейса. Выдуманный дескриптор в тесте — это номер,
// который на Linux принадлежит чему-то настоящему.
func connect(accountLink, cacheDir string, prefer int64, events client.Events, set tunnelSettings) (client.Backend, error) {
	return connectContext(context.Background(), accountLink, cacheDir, prefer, events, set)
}

func connectContext(ctx context.Context, accountLink, cacheDir string, prefer int64, events client.Events, set tunnelSettings) (client.Backend, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if isForeign(accountLink) {
		return connectForeign(ctx, accountLink, cacheDir, prefer, events, set)
	}
	account, err := client.ParseAccountLink(accountLink)
	if err != nil {
		return nil, fail(FailAccount, fmt.Errorf("ссылка доступа: %w", err))
	}

	key, err := vp1.KeyPairFromPrivate(account.PrivateKey)
	if err != nil {
		return nil, fail(FailAccount, fmt.Errorf("личный ключ: %w", err))
	}

	// Ноду выбираем замерами с самого устройства, а не берём первую из
	// списка: панель стоит за границей и видит ноду живой ровно тогда, когда
	// для телефона в Иркутске она уже мертва.
	//
	// Список нод при этом по возможности берём из кэша: каждый поход в панель
	// — это запрос имени её домена, а он с недавних пор уходит провайдеру
	// открытым текстом. Подробности в internal/client/cache.go.
	// Supervise, а не Connect: подключение то же самое, но за нодой дальше
	// следят и меняют её, если она замолчала. Без этого туннель, чью ноду
	// заблокировали, не рвётся — он глохнет, и телефон продолжает показывать
	// «Подключено», пока у человека наполовину не грузится всё подряд.
	//
	// Журнал ядра прокидываем обязательно.
	//
	// Без него надзор на телефоне нем: ни «нода не отвечает», ни «переехали»,
	// ни «переехать не удалось» наружу не выходят, и живая проверка не может
	// отличить «сторож не сработал» от «сторож сработал и не смог». Ровно на
	// этом застряла первая проверка переезда.
	dialer, measurements, err := client.Supervise(ctx, client.ConnectConfig{
		Account:   account,
		Key:       key,
		CachePath: accountCachePath(cacheDir, account.SubscriptionURL),
		Dial:      set.dial(),
		Prefer:    prefer,
		Log:       clientLog,
	}, events)

	// По умолчанию продавец узнаёт и о неудачном дозвоне. Человек может
	// оставить результаты только на телефоне: выбор ноды от этого не меняется.
	if !set.DisableReports && ctx.Err() == nil {
		go func() {
			if err := client.SendReports(context.Background(), account.SubscriptionURL, client.ReportsFrom(measurements)); err != nil {
				// Панель недоступна — не повод не работать. Туннель от неё не
				// зависит, список нод уже получен.
				_ = err
			}
		}()
	}

	if err != nil {
		// Вид неудачи человека ведёт в разные стороны: до панели не
		// достучались — проверь интернет, ноды молчат — пиши продавцу,
		// подписка кончилась — продли, и никуда писать не надо.
		switch {
		case errors.Is(err, client.ErrExpired):
			return nil, fail(FailExpired, err)
		case errors.Is(err, client.ErrQuota):
			return nil, fail(FailQuota, err)
		case errors.Is(err, client.ErrPanel):
			// Без «список нод:» сверху: ошибка и так начинается с «панель
			// недоступна», а три слоя пояснений подряд читать невозможно.
			return nil, fail(FailPanel, err)
		}
		return nil, fail(FailNodes, err)
	}
	return dialer, nil
}

// note запоминает последнюю ошибку, чтобы приложение могло её показать.
//
// Ошибки отдельных соединений не должны ронять туннель: одна недоступная цель
// — это норма, а не повод обрывать человеку весь интернет.
func (t *Tunnel) note(err error) {
	if err == nil {
		return
	}
	t.mu.Lock()
	t.lastError = redact.Secrets(err.Error())
	t.lastErrAt = time.Now()
	t.mu.Unlock()
}

// switched зовётся надзором после переезда на другую ноду.
//
// Имя переписываем обязательно: иначе окно продолжит показывать ноду, через
// которую трафик давно не идёт, и человек, глядя на «Финляндия», будет думать
// про Финляндию, когда он уже в Дубае. Прежнюю ошибку заодно стираем —
// переезд её и лечил.
func (t *Tunnel) switched(n client.Node) {
	t.mu.Lock()
	t.nodeName = n.Title()
	t.lastError = ""
	t.troubleCode = ""
	// Отклик и адрес выхода принадлежат прежней ноде: смешать их с новой
	// значило бы показать человеку джиттер и страну, которых у него нет.
	t.quality.reset()
	t.whereCache = whereView{}
	bridge := t.bridge
	t.mu.Unlock()

	// Мосту говорим отдельно: он мог выключить датаграммы, решив, что нода их
	// не умеет, а на самом деле нода в тот миг умирала. Новая нода за это не
	// отвечает.
	bridge.NodeChanged()
}

// trouble — нода замолчала, а переехать не на что.
//
// Единственный случай, когда человеку про переезд надо сказать. Удачный он
// замечать не должен: в этом весь смысл. А вот «сейчас не работает ничего»
// лучше прочитать у нас, чем выяснять самому, почему интернет наполовину.
//
// Кладём в отдельное поле, а не в общую строку ошибки, по двум причинам.
//
// Первая: беда должна держаться, пока не кончится, а разовая неудача одного
// потока — гаснуть через минуту. В одном поле это несовместимо, и раньше
// сообщение о беде мигало: надзор ставил его заново раз в минуту с лишним, а
// гасло оно ровно через минуту.
//
// Вторая: здесь код, а не фраза. Фразу подбирает приложение на своём языке —
// иначе английский интерфейс показывает русское предложение из ядра.
func (t *Tunnel) trouble(code string) {
	t.mu.Lock()
	t.troubleCode = code
	t.mu.Unlock()
}

// recovered — нода снова отвечает после того, как её объявили молчащей.
//
// Без этого надпись про беду не гасла бы никогда: надзор ставит её, а снять
// было некому. Человек смотрел бы на «ничего не работает» после того, как всё
// заработало.
func (t *Tunnel) recovered() {
	t.mu.Lock()
	t.troubleCode = ""
	t.lastError = ""
	t.mu.Unlock()
}

// Trouble отдаёт код беды: пусто — беды нет.
//
// Приложение подбирает под код свою надпись. Разовые неудачи отдельных
// соединений сюда не попадают — они в LastError и гаснут сами.
func (t *Tunnel) Trouble() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.troubleCode
}

// NetworkChanged передаёт смену внешней сети, сохраняя TUN и учёт сессии.
// Закрытая служба не должна запускать дозвон из позднего Android callback.
func (t *Tunnel) NetworkChanged(available bool) {
	t.mu.Lock()
	if !t.running {
		t.mu.Unlock()
		return
	}
	dialer := t.dialer
	if available {
		t.troubleCode = "network-changing"
	} else {
		t.troubleCode = "network-offline"
	}
	t.lastError = ""
	t.mu.Unlock()
	if aware, ok := dialer.(interface{ NetworkChanged(bool) }); ok {
		aware.NetworkChanged(available)
	}
}

// Disconnect возвращает телефону обычную сеть, не ожидая очистки дозвона.
// Приложение затем вызывает Stop в фоне; эта часть может ждать сторожа ноды.
func (t *Tunnel) Disconnect() {
	t.mu.Lock()
	t.running = false
	bridge := t.bridge
	t.mu.Unlock()
	bridge.Disconnect()
}

// Stop закрывает туннель. Безопасно вызывать несколько раз, в том числе
// после Disconnect: признак running уже снят, но ресурсы ещё надо освободить.
func (t *Tunnel) Stop() error {
	t.Disconnect()
	t.mu.Lock()
	t.running = false
	bridge, dialer := t.bridge, t.dialer
	t.bridge, t.dialer = nil, nil
	t.mu.Unlock()

	var first error
	if bridge != nil {
		if err := bridge.Close(); err != nil {
			first = err
		}
	}
	if dialer != nil {
		if err := dialer.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// Running сообщает, поднят ли туннель.
func (t *Tunnel) Running() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.running
}

// NodeName — имя ноды, через которую идёт трафик.
func (t *Tunnel) NodeName() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.nodeName
}

// LastError — последняя ошибка отдельного соединения, для показа в интерфейсе.
// Пустая строка означает, что ошибок не было.
func (t *Tunnel) LastError() string {
	t.mu.Lock()
	defer t.mu.Unlock()

	// Забываем старое. Настоящая беда о себе напомнит: надзор, не сумевший
	// переехать, ставит её заново каждые двадцать секунд, и строка не гаснет.
	// А единственный неудавшийся поток погаснет — ему там и место.
	if t.lastError != "" && time.Since(t.lastErrAt) > errorLifetime {
		t.lastError = ""
	}
	return t.lastError
}

// Экран выбора страны.
//
// Наружу отдаём JSON строкой, а не список объектов, и это не лень. gomobile
// умеет переносить только строки, числа и указатели на типы этого же пакета:
// список пришлось бы отдавать обёрткой со счётчиком и обращением по номеру, и
// приложение читало бы его в цикле через границу языков. JSON пересекает её
// один раз.

// NodeView — одна нода так, как её видит покупатель.
type NodeView struct {
	SetupMS int64 `json:"setup_ms"`
	ID      int64 `json:"id"`

	// Name и Country пишет продавец. Покупателю говорит страна: имя сервера
	// вроде vm-4823917 не значит для него ничего.
	Name    string `json:"name"`
	Country string `json:"country,omitempty"`
	// Endpoint identifies the server for a local exit-country cache. It is
	// already present in the subscription and is never shown as a country.
	Endpoint string `json:"endpoint,omitempty"`

	// MS — последний отклик внутри готового туннеля с этого телефона.
	// Ноль означает, что успешного замера отклика нет.
	MS    int64 `json:"ms"`
	Alive bool  `json:"alive"`

	// Current — через неё идёт трафик прямо сейчас.
	// Chosen — её выбрал человек руками.
	//
	// Это разные вещи, и именно поэтому оба поля здесь. Выбранная нода могла
	// замолчать, надзор уехал на живую, и показывать в этом случае одну
	// «выбранную» значило бы врать про то, куда идёт трафик.
	Current bool `json:"current"`
	Chosen  bool `json:"chosen"`
}

// Nodes отдаёт список нод без замера — тем, что известно с подключения.
func (t *Tunnel) Nodes() string {
	t.mu.Lock()
	dialer := t.dialer
	t.mu.Unlock()
	if dialer == nil {
		return "[]"
	}
	return viewsJSON(dialer, dialer.Nodes(), []client.Measurement{dialer.Measurement()})
}

// Measure меряет все ноды заново и отдаёт тот же список с временами.
//
// Зовётся по нажатию «Обновить» и занимает секунды: ноды опрашиваются разом,
// но каждая — настоящим подключением, иначе число было бы выдумкой.
func (t *Tunnel) Measure() string {
	t.mu.Lock()
	dialer := t.dialer
	t.mu.Unlock()
	if dialer == nil {
		return "[]"
	}

	ctx, cancel := context.WithTimeout(context.Background(), measureTimeout)
	defer cancel()

	return viewsJSON(dialer, dialer.Nodes(), dialer.Measure(ctx))
}

// Ping меряет отклик текущей ноды внутри туннеля, в миллисекундах; ноль —
// не вышло. Дёшево: один запрос-ответ по открытой сессии, поэтому приложение
// зовёт его само раз в несколько секунд, и число на экране и в уведомлении
// живое, а не снятое при подключении.
func (t *Tunnel) Ping() int64 {
	t.mu.Lock()
	dialer := t.dialer
	t.mu.Unlock()
	if dialer == nil {
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), pingTimeout)
	defer cancel()
	rtt, err := dialer.Ping(ctx)
	ms := int64(0)
	if err == nil && rtt > 0 {
		ms = max(1, rtt.Milliseconds())
	}
	// Неответ тоже замер: из них складываются потери в «Качестве связи».
	// Пишем, только если нода всё та же — замер прежней не про текущую.
	t.mu.Lock()
	if t.dialer == dialer {
		t.quality.add(ms)
	}
	t.mu.Unlock()
	return ms
}

// pingTimeout — дольше не ждём: отклик в три секунды человеку уже всё равно
// что «нет ответа», а зовут замер каждые несколько секунд.
const pingTimeout = 3 * time.Second

// SelectNode переводит туннель на выбранную ноду. Ноль — обратно к автовыбору.
func (t *Tunnel) SelectNode(id int64) error {
	t.mu.Lock()
	dialer := t.dialer
	t.mu.Unlock()
	if dialer == nil {
		return errors.New("туннель не поднят")
	}

	ctx, cancel := context.WithTimeout(context.Background(), selectTimeout)
	defer cancel()

	if err := dialer.Select(ctx, id); err != nil {
		return err
	}

	t.mu.Lock()
	t.nodeName = dialer.Node().Title()
	t.mu.Unlock()
	return nil
}

// SelectedNode — что выбрано руками. Ноль означает автовыбор.
func (t *Tunnel) SelectedNode() int64 {
	t.mu.Lock()
	dialer := t.dialer
	t.mu.Unlock()
	if dialer == nil {
		return 0
	}
	return dialer.Selected()
}

// viewsJSON собирает список для приложения.
func viewsJSON(dialer client.Backend, nodes []client.Node, measured []client.Measurement) string {
	byID := make(map[int64]client.Measurement, len(measured))
	for _, m := range measured {
		byID[m.Node.ID] = m
	}

	current := dialer.Node().ID
	chosen := dialer.Selected()

	views := make([]NodeView, 0, len(nodes))
	for _, n := range nodes {
		v := NodeView{
			ID:       n.ID,
			Name:     n.Name,
			Country:  n.Country,
			Endpoint: n.Address,
			Current:  n.ID == current,
			Chosen:   chosen != 0 && n.ID == chosen,
		}
		if m, ok := byID[n.ID]; ok {
			v.Alive = m.OK()
			v.MS = m.PingMS()
			if m.OK() {
				v.SetupMS = max(1, m.Latency.Milliseconds())
			}
		} else if n.ID == current {
			// Текущую ноду мы не мерили, но знаем точно: через неё прямо
			// сейчас идёт трафик.
			v.Alive = true
		}
		views = append(views, v)
	}

	out, err := json.Marshal(views)
	if err != nil {
		return "[]"
	}
	return string(out)
}

// SentBytes и ReceivedBytes — сколько байт прошло через туннель с его
// подъёма, со стороны человека: отправил он и получил он.
//
// Накопительно и с нуля при каждом подключении: приложение снимает их
// раз в несколько секунд и кладёт разницу в свой счёт по дням. Меньше
// прошлого значит, что туннель подняли заново.
func (t *Tunnel) SentBytes() int64 { return t.up.Load() }

func (t *Tunnel) ReceivedBytes() int64 { return t.down.Load() }

// CheckAccountLink проверяет ссылку, ничего не подключая.
//
// Нужно интерфейсу: сказать «ссылка не та» сразу при вставке гораздо лучше,
// чем после неудачной попытки соединения.
func CheckAccountLink(accountLink string) error {
	if isForeign(accountLink) {
		// Ссылку ноды проверяем сразу — ошибка в ней видна без сети. Адрес
		// чужой подписки проверится при первом походе: что по нему лежит,
		// не узнать, не сходив.
		if foreign.IsLink(firstLine(accountLink)) {
			if _, err := foreign.Parse(firstLine(accountLink)); err != nil {
				return err
			}
		}
		return nil
	}
	_, err := client.ParseAccountLink(accountLink)
	return err
}

// Until — до какого числа оплачена подписка, в виде «2026-09-27».
// Пустая строка означает «без ограничения по сроку».
//
// Приложение обязано уметь ответить на «сколько у меня осталось» само.
// Иначе на этот вопрос отвечает продавец — каждому и вручную.
func (t *Tunnel) Until() string {
	t.mu.Lock()
	dialer := t.dialer
	t.mu.Unlock()

	if dialer == nil {
		return ""
	}
	until, set := dialer.Subscription().Until()
	if !set {
		return ""
	}
	return until.Local().Format("2006-01-02")
}

// UpdateVersion — версия приложения новее нашей, которую выложила панель
// продавца; пустая строка — обновляться не на что. current — versionName
// приложения: ядро своей версии не знает, она у пакета.
func (t *Tunnel) UpdateVersion(current string) string {
	offer, _ := t.update(current)
	return offer.Version
}

// UpdateURL — откуда скачать эту версию: с домена продавца, а не из магазина.
func (t *Tunnel) UpdateURL(current string) string {
	offer, _ := t.update(current)
	return offer.URL
}

func (t *Tunnel) update(current string) (client.AppOffer, bool) {
	t.mu.Lock()
	dialer := t.dialer
	t.mu.Unlock()

	if dialer == nil {
		return client.AppOffer{}, false
	}
	return dialer.Subscription().Update("android", current)
}

// TrafficLimit — сколько байт оплачено. Ноль означает «без ограничения».
func (t *Tunnel) TrafficLimit() int64 {
	t.mu.Lock()
	dialer := t.dialer
	t.mu.Unlock()

	if dialer == nil {
		return 0
	}
	return dialer.Subscription().TrafficLimit
}

// TrafficLeft — сколько байт осталось. Ноль означает либо «без ограничения»,
// либо «всё выбрано»; отличать по TrafficLimit.
func (t *Tunnel) TrafficLeft() int64 {
	t.mu.Lock()
	dialer := t.dialer
	t.mu.Unlock()

	if dialer == nil {
		return 0
	}
	return dialer.Subscription().Remaining()
}
