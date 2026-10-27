package io.marvia.android

import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.content.Intent
import android.content.pm.PackageManager
import android.content.pm.ServiceInfo
import android.net.InetAddresses
import android.net.IpPrefix
import android.net.VpnService
import android.os.Build
import android.os.Looper
import android.util.Log
import androidx.annotation.RequiresApi
import androidx.core.app.NotificationCompat
import io.marvia.mobile.Mobile
import io.marvia.mobile.ConnectionAttempt
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.cancel
import kotlinx.coroutines.delay
import kotlinx.coroutines.isActive
import kotlinx.coroutines.launch
import kotlinx.coroutines.channels.Channel
import io.marvia.mobile.Tunnel as Core

/**
 * MarviaVpnService — то, ради чего приложение существует.
 *
 * Обязанностей у него ровно две, и обе платформенные: выпросить у системы
 * сетевой интерфейс и не дать себя выгрузить из памяти. Всё остальное — выбор
 * ноды, рукопожатие, разбор пакетов, учёт — делает ядро на Go, то же самое,
 * что работает на сервере. Ни строчки сетевой логики здесь нет и быть не
 * должно: продублированная логика расходится, и расходится она молча.
 */
class MarviaVpnService : VpnService() {

    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.IO)

    /** Своя версия — сравнивать с той, что выложила панель продавца. */
    private val ownVersion: String by lazy {
        try {
            packageManager.getPackageInfo(packageName, 0).versionName.orEmpty()
        } catch (_: PackageManager.NameNotFoundException) {
            ""
        }
    }
    private var worker: Job? = null
    private var countryProbe: Job? = null
    private var networkMonitor: NetworkMonitor? = null
    private var networkWorker: Job? = null
    private var networkChanges: Channel<Boolean>? = null

    /** Останавливались ли мы уже. См. [shutdown] — там объяснено, зачем. */
    @Volatile private var finished = false
    private val lifecycle = Any()

    @Volatile
    private var core: Core? = null
    private var connectionAttempt: ConnectionAttempt? = null

    /** Есть ли сеть под туннелем. См. [Uplink]. */
    private val uplink by lazy { Uplink(this) }

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        if (intent?.action == ACTION_STOP) {
            shutdown(TunnelState.Off)
            return START_NOT_STICKY
        }
        if (worker?.isActive == true) return START_NOT_STICKY

        // Служба могла уже один раз остановиться и не успеть разрушиться —
        // тогда система отдаёт запуск тому же объекту. Без сброса признака
        // остановки такая служба больше никогда бы не остановилась: [shutdown]
        // молча выходил бы, считая, что всё уже сделано.
        synchronized(lifecycle) { finished = false }

        // Постоянное уведомление надо показать в первые секунды после запуска,
        // иначе система убьёт службу за нарушение правил. Подключение занимает
        // куда больше — поэтому уведомление сначала, работа потом.
        goForeground(Live.connecting(this))
        connect()

        // Не START_STICKY: система перезапускала бы службу с пустым намерением
        // после каждого убийства, и неудачное подключение превратилось бы в
        // бесконечный круг попыток, который человек не может остановить.
        return START_NOT_STICKY
    }

    private fun connect() {
        if (worker?.isActive == true) {
            return
        }

        val store = Store(this)
        val link = store.accountLink
        if (link.isBlank()) {
            // Вид пустой намеренно. С видом account сюда подставилась бы фраза
            // «ключ не подошёл», а ключа просто нет — это разные вещи, и
            // человека они ведут в разные стороны.
            shutdown(TunnelState.Failed("", getString(R.string.detail_no_key)))
            return
        }

        Journal.add(getString(R.string.log_tunnel_on))
        MarviaState.set(TunnelState.Connecting)
        val attempt = Mobile.newConnectionAttempt()
        synchronized(lifecycle) { connectionAttempt = attempt }

        val changes = Channel<Boolean>(Channel.CONFLATED)
        networkChanges = changes
        val monitor = NetworkMonitor(this) { changes.trySend(it) }
        networkMonitor = monitor
        try {
            monitor.start()
        } catch (t: Throwable) {
            shutdown(TunnelState.Failed(Mobile.FailSystem, reasonOf(t)))
            return
        }
        networkWorker = scope.launch {
            for (first in changes) {
                // onLost и новая default network часто приходят парой.
                // Один дозвон после короткого перехода экономит батарею.
                delay(300)
                var available = first
                while (true) { available = changes.tryReceive().getOrNull() ?: break }
                val active = core ?: continue
                active.networkChanged(available)
                val code = active.trouble()
                val next = snapshot(active, active.nodeName(), if (code.isEmpty()) "" else troubleText(code))
                synchronized(lifecycle) { if (isActive && !finished && core === active) {
                    publish(next)
                    Log.i(TAG, if (available) "внешняя сеть изменилась: восстанавливаем ноду" else "внешняя сеть потеряна")
                } }
            }
        }

        worker = scope.launch {
            val networkGeneration = monitor.snapshot().first

            val started = try {
                // Каталог под кэш списка нод. Путь к своим файлам знает только
                // Context — ядру его взять неоткуда, поэтому передаём руками.
                // Резолвер — тот, что выбрал человек. Внутрь туннеля запрос
                // уходит в любом случае; к известному резолверу — ещё и по
                // HTTPS, если человек не выключил шифрование имён
                // (plain_dns в tunnelSettings).
                // TUN появляется после дозвона. Пока панель или LTE молчит,
                // телефон сохраняет обычную сеть, а отмена прерывает и Go.
                attempt.connect(link, store.cacheDir(), store.chosenNode, store.tunnelSettings())
            } catch (t: Throwable) {
                shutdown(failureOf(t), attempt)
                return@launch
            }

            try {
                if (!attachInterface(started, store, attempt)) { started.stop(); return@launch }
            } catch (t: Throwable) {
                started.disconnect()
                shutdown(failureOf(t), attempt)
                started.stop()
                return@launch
            }
            val node = started.nodeName()
            val initial = snapshot(started, node)
            val attached = synchronized(lifecycle) {
                if (!isActive || finished || connectionAttempt !== attempt) false else {
                    core = started
                    MarviaState.hold(started)
                    Journal.add(getString(R.string.log_connected, node))
                    connectedAt = System.currentTimeMillis()
                    live.reset(started)
                    Traffic(this@MarviaVpnService).beginSession()
                    trafficHistory = TrafficHistory(this@MarviaVpnService)
                    SessionDetails.begin()
                    publish(initial)
                    goForeground(live.on(this@MarviaVpnService, node, 0))
                    uplink.start()
                    true
                }
            }
            if (!attached) { started.disconnect(); started.stop(); return@launch }
            // Смена сети во время блокирующего gomobile Start не должна
            // потеряться только потому, что ядро ещё не было опубликовано.
            val (generation, available) = monitor.snapshot()
            if (generation != networkGeneration) changes.trySend(available)
            detectExitCountry(started, store)

            // Российский список — сейчас, когда сеть точно есть. Приложение
            // исключено из туннеля, так что панель спрашивается напрямую, как
            // и до включения. Сработает со следующего подключения: маршруты
            // уже отданы системе.
            if (store.bypassRussian) RuRoutes.refreshInBackground(applicationContext, link)

            watch(started, node)
        }
    }

    /** Конфигурация интерфейса не удерживает замок отключения; только его выдача. */
    private fun attachInterface(started: Core, store: Store, attempt: ConnectionAttempt): Boolean {
        val builder = Builder()
            .setSession(getString(R.string.app_name))
            .setMtu(store.vpnMtu)
            .addAddress(ADDRESS_V4, PREFIX_V4)
            .addRoute("0.0.0.0", 0)
            // IPv6 заворачиваем в туннель, даже если у ноды его нет. Оставить
            // его снаружи — это утечка: часть трафика пойдёт мимо, и цензор
            // увидит именно ту часть, которую мы прячем. Пусть лучше
            // соединение не состоится внутри туннеля: приложения после этого
            // сами переходят на IPv4.
            .addAddress(ADDRESS_V6, PREFIX_V6)
            .addRoute("::", 0)
            // Адрес не важен: ядро перехватывает любой запрос имён по порту 53
            // и отвечает на него само, через туннель. Ставим тот, что выбрал
            // человек, на случай, если система решит показать его в
            // настройках сети — пусть там будет правда.
            .addDnsServer(store.dns)

        // Российские подсети мимо туннеля. Исключение маршрутов появилось в
        // Android 13; на старых остаётся исключение по приложениям, и обещать
        // человеку больше, чем умеет система, нельзя.
        if (store.bypassRussian && Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU) {
            var excluded = 0
            for (prefix in RuRoutes.load(this)) {
                try {
                    builder.excludeRoute(prefix)
                    excluded++
                } catch (t: IllegalArgumentException) {
                    // Одна битая строка в списке не повод остаться без туннеля.
                    Log.w(TAG, "подсеть не принята: " + t.message)
                }
            }
            Log.i(TAG, "мимо туннеля российских подсетей: " + excluded)
        }

        // Домашняя сеть мимо туннеля: принтер, телевизор, роутер. Их адреса
        // частные, за границу они не маршрутизируются в принципе, и внутри
        // туннеля до них просто не дойти. Тот же Android 13 и то же
        // исключение маршрутов.
        if (store.lanOutside && Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU) {
            for (prefix in lanPrefixes()) {
                builder.excludeRoute(prefix)
            }
        }

        // Свои адреса мимо туннеля — то же исключение маршрутов. Имена сайтов
        // разрешаются здесь, до поднятия туннеля, с пределом в пару секунд:
        // недоступный DNS не должен держать подключение.
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU) {
            val custom = CustomRoutes.parse(store.customBypass).entries
            if (custom.isNotEmpty()) {
                var excluded = 0
                for (prefix in CustomRoutes.prefixes(custom)) {
                    try {
                        builder.excludeRoute(prefix)
                        excluded++
                    } catch (t: IllegalArgumentException) {
                        Log.w(TAG, "свой адрес не принят: " + t.message)
                    }
                }
                Log.i(TAG, "мимо туннеля своих адресов: " + excluded)
            }
        }

        // Приложения. Три режима, и они исключают друг друга по устройству
        // Android: список «мимо туннеля» и список «только эти» в одном
        // интерфейсе не сочетаются, система бросит исключение.
        //
        // Удалённые пакеты пропускаем, но не расширяем список «только эти»:
        // пустой список Android трактует как весь телефон.
        when (store.bypassMode) {
            Store.BYPASS_EXCLUDE -> {
                // Свой трафик в собственный туннель не заворачиваем. Иначе
                // соединение до ноды пошло бы через интерфейс, который сам же
                // и ведёт к ноде.
                builder.addDisallowedApplication(packageName)
                // Госуслуги, банки, всё, что не отвечает на запросы из-за
                // границы. Система оставляет им обычную сеть, и на сервер
                // приходит их настоящий адрес.
                for (pkg in store.bypassed) {
                    try {
                        builder.addDisallowedApplication(pkg)
                    } catch (_: PackageManager.NameNotFoundException) {
                        Log.w(TAG, "мимо туннеля просили $pkg, но оно не установлено")
                    }
                }
            }
            Store.BYPASS_INCLUDE -> {
                // Только отмеченные идут через туннель, остальные — мимо. Себя
                // в список не добавляем, и этого достаточно: неотмеченное
                // система в туннель не пускает. Пустой список для Android
                // означает «все» — экран об этом предупреждает.
                val selected = store.bypassed
                val accepted = AppRouting.addIncluded(selected, packageName) { pkg ->
                    try {
                        builder.addAllowedApplication(pkg)
                        true
                    } catch (_: PackageManager.NameNotFoundException) {
                        Log.w(TAG, "в туннель просили $pkg, но оно не установлено")
                        false
                    }
                }
                if (!accepted) throw IllegalStateException(getString(R.string.error_missing_allowed_apps))
                if (selected.isEmpty()) {
                    builder.addDisallowedApplication(packageName)
                }
            }
            else -> builder.addDisallowedApplication(packageName)
        }

        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.Q) {
            // false наследует лимитность от исходной сети; true помечает VPN
            // лимитной всегда, чтобы приложения сами отложили крупные загрузки.
            builder.setMetered(store.meteredVpn)
        }

        return synchronized(lifecycle) {
            if (finished || connectionAttempt !== attempt) false else {
                val descriptor = builder.establish()
                    ?: throw IllegalStateException(getString(R.string.error_no_interface))
                // После detachFd ядро отвечает за закрытие, в том числе при
                // ошибке Attach. Двойное закрытие может задеть чужой сокет.
                descriptor.use { started.attach(it.detachFd().toLong(), store.dns + ":53") }
                true
            }
        }
    }

    /**
     * watch показывает ошибки отдельных соединений, не трогая туннель.
     *
     * Разница между «ничего не работает» и «не открывается один сайт» для
     * человека огромна, а изнутри ядра она видна сразу.
     *
     * Имя ноды перечитываем на каждом круге, а не запоминаем при подключении.
     * Ядро меняет ноду само, когда прежняя замолчала, и раньше об этом здесь
     * не узнавали: на экране навсегда оставалась та нода, через которую трафик
     * давно не идёт. Удачный переезд выглядел как «ничего не произошло» — и
     * человек, глядя на мёртвое имя и на ошибки под ним, шёл переподключаться
     * руками. Ровно это и случилось на первой живой проверке.
     */
    private suspend fun watch(started: Core, node: String) {
        var shownNode = node
        var previousWarning = ""
        var lastPingAt = 0L
        val power = getSystemService(android.os.PowerManager::class.java)
        val settings = Store(this)
        while (scope.isActive && started.running()) {
            // Экран погашен — смотреть на скорость некому. Интервал можно
            // настроить: он меняет только счётчики и уведомление, не туннель.
            // Системное энергосбережение замедляет опрос до профиля «Батарея»
            // (MonitoringProfiles.pace).
            val (seconds, pingEvery) = MonitoringProfiles.pace(
                settings.liveRefreshSeconds, settings.idleRefreshSeconds, settings.pingIntervalSeconds,
                interactive = power?.isInteractive != false,
                saver = power?.isPowerSaveMode == true,
            )
            delay(seconds * 1000L)

            // Отклик мерим независимо от опроса счётчиков: если человек
            // снизил частоту обновления ради батареи, проверка не учащается.
            val nowPing = android.os.SystemClock.elapsedRealtime()
            if (pingEvery > 0 && (lastPingAt == 0L || nowPing - lastPingAt >= pingEvery * 1000L)) {
                started.ping()
                lastPingAt = nowPing
            }
            if (!kotlin.coroutines.coroutineContext.isActive || finished || core !== started) return

            // Держащаяся беда важнее разовой ошибки и показывается вместо неё.
            //
            // Из ядра приезжает код, а не фраза: оно не знает языка интерфейса.
            // Раньше оттуда приходило русское предложение, и англоязычный
            // покупатель читал его как есть.
            // Нет сети — главное, что надо знать: остальные ошибки в это
            // время только её следствие.
            val trouble = started.trouble()
            val offline = uplink.offline
            val last = when {
                offline -> getString(R.string.trouble_offline)
                trouble.isEmpty() -> started.lastError()
                else -> troubleText(trouble)
            }
            if (last.isNotBlank() && last != previousWarning) Journal.add(last, Journal.Level.WARN)
            previousWarning = last

            val now = started.nodeName()
            if (now != shownNode) {
                shownNode = now
                Journal.add(getString(R.string.log_moved, now), Journal.Level.WARN)
                detectExitCountry(started, settings)
            }

            // Пересобираем целиком, а не только имя ноды: остаток трафика
            // тает на глазах, и показывать число с момента подключения значит
            // отвечать на «сколько осталось» вчерашним ответом.
            val next = snapshot(started, shownNode, last)
            synchronized(lifecycle) {
                // Опрос JNI мог закончиться уже после отключения. Последняя
                // проверка стоит у публикации, чтобы не вернуть «Подключено».
                if (!kotlin.coroutines.coroutineContext.isActive || finished || core !== started) return
                publish(next)
                goForeground(live.on(this, shownNode, next.state.ms, offline))
            }
        }
    }

    /**
     * snapshot собирает всё, что экран показывает про работающий туннель.
     *
     * Список нод спрашиваем здесь же: из него видно и время отклика текущей
     * ноды, и то, совпадает ли она с выбранной руками. Сети это не касается —
     * ядро отдаёт то, что уже знает.
     */
    private var trafficHistory: TrafficHistory? = null
    private var trafficCountry = ""
    private data class CoreSnapshot(val state: TunnelState.On, val received: Long, val sent: Long, val country: String)

    /** One bounded lookup per unknown node, only when it actually becomes active. */
    private fun detectExitCountry(started: Core, store: Store) {
        val link = store.accountLink
        val row = runCatching { NodeRow.parse(started.nodes()).firstOrNull { it.current } }.getOrNull() ?: return
        if (row.country.isNotBlank() || CountryGuess.fromName(row.name).isNotEmpty() ||
            store.exitCountry(link, row.id, row.name, row.endpoint).isNotEmpty()
        ) return
        countryProbe?.cancel()
        countryProbe = scope.launch {
            val code = runCatching { started.exitCountry() }.getOrDefault("")
            if (!isActive || core !== started || store.accountLink != link || code.isEmpty()) return@launch
            val stillCurrent = runCatching { NodeRow.parse(started.nodes()).firstOrNull { it.current } }.getOrNull()
            if (stillCurrent?.id != row.id || stillCurrent.name != row.name || stillCurrent.endpoint != row.endpoint) return@launch
            store.rememberExitCountry(link, row.id, row.name, row.endpoint, code)
            MarviaState.countryDetected.value += 1
        }
    }

    private fun snapshot(started: Core, node: String, warning: String = ""): CoreSnapshot {
        // Вызовы JNI выполняются без замка остановки. Снимок применяется
        // только после проверки владельца: старый опрос не меняет новую сессию.
        val received = started.receivedBytes()
        val sent = started.sentBytes()
        val store = Store(this)
        val rows = NodeRow.parse(started.nodes()) { id, name, endpoint ->
            store.exitCountry(store.accountLink, id, name, endpoint)
        }
        val current = rows.firstOrNull { it.current }
        val chosen = rows.firstOrNull { it.chosen }

        // Ядро отдаёт последний замер именно текущей ноды.
        val ms = current?.ms ?: 0

        return CoreSnapshot(TunnelState.On(
            node = node,
            warning = warning,
            subscription = TunnelState.Subscription(
                until = started.until(),
                limitBytes = started.trafficLimit(),
                leftBytes = started.trafficLeft(),
                updateVersion = started.updateVersion(ownVersion),
                updateUrl = started.updateURL(ownVersion),
            ),
            ms = ms,
            chosen = chosen?.title.orEmpty(),
        ), received, sent, current?.group.orEmpty())
    }

    private fun publish(snapshot: CoreSnapshot) {
        SessionDetails.sample(snapshot.received, snapshot.sent)
        trafficCountry = snapshot.country
        MarviaState.traffic.value = trafficHistory!!.sample(snapshot.received + snapshot.sent, snapshot.country)
        MarviaState.set(snapshot.state)
    }

    /** troubleText подбирает фразу под код беды из ядра. */
    private fun troubleText(code: String): String = when (code) {
        "no-node" -> getString(R.string.trouble_no_node)
        "network-changing" -> getString(R.string.trouble_network_changing)
        "network-offline" -> getString(R.string.trouble_network_offline)
        // Незнакомый код — не повод молчать: покажем как есть, чтобы поломка
        // была видна, а не спрятана за пустой строкой.
        else -> code
    }

    /**
     * shutdown останавливает туннель и объявляет итоговое состояние.
     *
     * Останавливаемся ровно один раз, и вот почему. После stopSelf система
     * зовёт onDestroy, а тот тоже останавливается — и объявлял бы обычное
     * «отключено» поверх только что записанной причины обрыва. На экране
     * оставалось бы «Отключено. Трафик идёт напрямую», хотя подключение
     * только что провалилось. Это худший вид ошибки: человек видит, что не
     * работает, и не видит почему.
     */
    private fun shutdown(state: TunnelState, attempt: ConnectionAttempt? = null) {
        // Ошибка дозвона приходит с IO, а команды Android — с главного потока.
        // Одно место завершения не даёт старой попытке остановить новую службу.
        if (Looper.myLooper() != Looper.getMainLooper()) {
            scope.launch(Dispatchers.Main.immediate) { shutdown(state, attempt) }
            return
        }
        val started: Core?
        val pending: ConnectionAttempt?
        synchronized(lifecycle) {
            if (finished || (attempt != null && connectionAttempt !== attempt)) return
            finished = true
            started = core
            pending = connectionAttempt
            core = null
            connectionAttempt = null
        }
        pending?.cancel()
        started?.disconnect()
        networkMonitor?.close()
        networkMonitor = null
        networkWorker?.cancel()
        networkWorker = null
        networkChanges?.close()
        networkChanges = null

        if (state is TunnelState.Failed) {
            // В журнал — чтобы причину можно было достать с чужого телефона,
            // где экран уже закрыли и пересказывают по памяти.
            Log.w(TAG, "туннель не поднялся (${state.kind}): ${state.detail}")
            Journal.add(getString(R.string.log_failed, state.kind, state.detail), Journal.Level.ERROR)
        }

        worker?.cancel()
        worker = null
        uplink.stop()
        countryProbe?.cancel()
        countryProbe = null
        started?.let {
            // Последний отсчёт мог прийти после предыдущего опроса. Он нужен
            // и для итогов завершённой сессии, и для суточного расхода.
            runCatching { started.receivedBytes() to started.sentBytes() }.getOrNull()?.let { (received, sent) ->
                SessionDetails.sample(received, sent)
                Traffic(this).note(received + sent, trafficCountry, persist = false)
            }
        }
        val session = if (started != null) SessionDetails.finish() else null
        trafficHistory = null
        // Экрану стран больше не у кого спрашивать: туннеля нет.
        MarviaState.hold(null)
        MarviaState.set(state)
        shown = null
        stopForeground(STOP_FOREGROUND_REMOVE)
        stopSelf()
        // onDestroy отменит scope службы, но освобождение ядра и запись
        // уже отделённой сессии обязаны закончиться после её уничтожения.
        val context = applicationContext
        cleanupScope.launch {
            runCatching { started?.stop() }
        }
        cleanupScope.launch {
            if (session != null) SessionDetails.save(context, session)
            Traffic(context).flush()
        }
    }

    /** Система отобрала право на VPN — обычно потому, что включили другой. */
    override fun onRevoke() {
        shutdown(TunnelState.Off)
    }

    override fun onCreate() {
        super.onCreate()
        MarviaState.serviceAlive.value = true
    }

    override fun onDestroy() {
        shutdown(TunnelState.Off)
        scope.cancel()
        super.onDestroy()
        MarviaState.serviceAlive.value = false
    }

    /** Когда туннель поднялся: от этой отметки уведомление ведёт часы сессии. */
    private var connectedAt = 0L

    /** Скорость для уведомления — по двум направлениям, из счётчиков ядра. */
    private val live = Live()

    /**
     * Live — что уведомление говорит про работающий туннель.
     *
     * Раньше там было «Подключено. Нода: …» и больше ничего: чтобы узнать,
     * идёт ли трафик и какой отклик, приходилось открывать приложение.
     * Теперь — страна, скорость вниз и вверх, отклик и часы сессии, и всё
     * это обновляется, пока туннель жив. Часы ведёт сама система
     * (setUsesChronometer): перерисовывать уведомление каждую секунду ради
     * них незачем и накладно.
     */
    class Live {
        private var rx = 0L
        private var tx = 0L
        private var at = 0L

        data class Text(val title: String, val text: String, val big: String, val on: Boolean)

        fun reset(core: Core) {
            rx = core.receivedBytes()
            tx = core.sentBytes()
            at = android.os.SystemClock.elapsedRealtime()
            this.core = core
        }

        private var core: Core? = null

        fun on(context: android.content.Context, node: String, ms: Long, offline: Boolean = false): Text {
            val c = core
            val now = android.os.SystemClock.elapsedRealtime()
            var down = 0.0
            var up = 0.0
            if (c != null) {
                val r = c.receivedBytes()
                val t = c.sentBytes()
                val dt = (now - at).coerceAtLeast(1) / 1000.0
                if (at != 0L && now > at) {
                    down = (r - rx).coerceAtLeast(0) / dt
                    up = (t - tx).coerceAtLeast(0) / dt
                }
                rx = r; tx = t; at = now
            }
            val place = node.split('·').map { it.trim() }.filter { it.isNotEmpty() }.take(2).joinToString(" · ")
            if (offline) {
                // Скорость и отклик без сети — нули, которые выглядят как
                // поломка туннеля. Говорим, что именно случилось и что делать.
                val title = context.getString(R.string.notification_offline, place.ifEmpty { node })
                val detail = context.getString(R.string.notification_offline_detail)
                return Text(title, detail, detail + "\n" + node, on = true)
            }
            val title = context.getString(R.string.notification_on, place.ifEmpty { node })
            val speed = context.getString(R.string.notification_speed, rate(context, down), rate(context, up))
            val ping = if (ms > 0) context.getString(R.string.node_ms, ms) else ""
            val text = listOf(speed, ping).filter { it.isNotEmpty() }.joinToString("  ·  ")
            val today = context.getString(R.string.notification_today, Format.size(context, MarviaState.traffic.value.today))
            return Text(title, text, text + "\n" + today + "\n" + node, on = true)
        }

        companion object {
            fun connecting(context: android.content.Context) = Text(
                context.getString(R.string.status_connecting),
                context.getString(R.string.detail_connecting),
                context.getString(R.string.detail_connecting),
                on = false,
            )

            /** rate — байты в секунду человеческим числом: 850 Кбит/с, 12,4 Мбит/с. */
            fun rate(context: android.content.Context, bytesPerSecond: Double): String {
                val bits = bytesPerSecond * 8
                val locale = context.resources.configuration.locales[0]
                return if (bits >= 1_000_000) {
                    context.getString(R.string.stats_mbps, String.format(locale, "%.1f", bits / 1_000_000))
                } else {
                    context.getString(R.string.stats_kbps, String.format(locale, "%.0f", bits / 1_000))
                }
            }
        }
    }

    /**
     * mark — прозрачный знак для уведомления в цвет темы. Подложка вокруг
     * него на Samsung рисовалась белым диском и перекрывала сам знак.
     */
    private var markCache: Pair<Int, android.graphics.Bitmap>? = null

    private fun mark(): android.graphics.Bitmap {
        val t = Look.theme(Store(this).look)
        // Уведомление обновляется каждые две секунды, а знак меняется только
        // со сменой темы: собирать картинку заново каждый раз незачем. Серый
        // запасной не кэшируем — через миг в кэше будет покрашенный.
        markCache?.let { (acc, bmp) -> if (acc == t.acc) return bmp }
        val size = (64 * resources.displayMetrics.density).toInt()
        val out = android.graphics.Bitmap.createBitmap(size, size, android.graphics.Bitmap.Config.ARGB_8888)
        val canvas = android.graphics.Canvas(out)
        val paint = android.graphics.Paint(android.graphics.Paint.ANTI_ALIAS_FLAG or android.graphics.Paint.FILTER_BITMAP_FLAG)
        // Прозрачный фон: залитый круг превращался у Samsung в белую плашку
        // слева от текста уведомления.
        val tinted = LogoAtlas.peek(this, t.acc) {}
        val logo = tinted ?: LogoAtlas.stock(this)
        val w = size * 0.8f
        val h = w * logo.height / logo.width
        canvas.drawBitmap(logo, null, android.graphics.RectF((size - w) / 2, (size - h) / 2, (size + w) / 2, (size + h) / 2), paint)
        if (tinted != null) markCache = t.acc to out
        return out
    }

    /** Что уведомление показывает сейчас: одинаковое второй раз не отправляем. */
    private var shown: Live.Text? = null
    private val notificationStore by lazy { Store(this) }

    private fun goForeground(content: Live.Text) {
        val display = if (content.on && notificationStore.compactNotification) {
            val status = getString(R.string.notification_compact_status)
            Live.Text(status, getString(R.string.notification_compact_detail), status, true)
        } else content
        // Простаивающий туннель даёт «0 Кбит/с» каждые две секунды, и каждое
        // такое обновление — будить системный процесс уведомлений впустую.
        if (display == shown) return
        shown = display
        val manager = getSystemService(NotificationManager::class.java)
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
            val channel = NotificationChannel(
                CHANNEL,
                getString(R.string.notification_channel),
                NotificationManager.IMPORTANCE_LOW,
            )
            channel.description = getString(R.string.notification_channel_desc)
            channel.setShowBadge(false)
            manager.createNotificationChannel(channel)
        }

        val flags = PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT
        val open = PendingIntent.getActivity(
            this,
            0,
            Intent(this, MainActivity::class.java),
            flags,
        )
        val stop = PendingIntent.getService(
            this,
            1,
            Intent(this, MarviaVpnService::class.java).setAction(ACTION_STOP),
            flags,
        )

        val t = Look.theme(Store(this).look)
        val builder = NotificationCompat.Builder(this, CHANNEL)
            .setSmallIcon(R.drawable.ic_stat_marvia)
            .setLargeIcon(mark())
            .setColor(t.acc)
            .setContentTitle(display.title)
            .setContentText(display.text)
            .setStyle(NotificationCompat.BigTextStyle().bigText(display.big))
            .setContentIntent(open)
            .setOngoing(true)
            // Обновляется каждые пару секунд — звука и вибрации на каждом
            // обновлении быть не должно.
            .setOnlyAlertOnce(true)
            .setCategory(NotificationCompat.CATEGORY_SERVICE)
            .setPriority(NotificationCompat.PRIORITY_LOW)
            .setForegroundServiceBehavior(NotificationCompat.FOREGROUND_SERVICE_IMMEDIATE)
            .addAction(R.drawable.ic_stat_marvia, getString(R.string.notification_stop), stop)
        if (display.on && connectedAt > 0) {
            builder.setShowWhen(true).setWhen(connectedAt).setUsesChronometer(true)
        } else {
            builder.setShowWhen(false)
        }
        val notification = builder.build()

        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.UPSIDE_DOWN_CAKE) {
            startForeground(
                NOTIFICATION_ID,
                notification,
                ServiceInfo.FOREGROUND_SERVICE_TYPE_SPECIAL_USE,
            )
        } else {
            startForeground(NOTIFICATION_ID, notification)
        }
    }

    companion object {

        private val cleanupScope = CoroutineScope(SupervisorJob() + Dispatchers.IO)
        const val ACTION_STOP = "io.marvia.android.action.STOP"

        private const val TAG = "Veil"
        private const val CHANNEL = "veil.tunnel"
        private const val NOTIFICATION_ID = 1
        // Адреса внутри туннеля. Наружу они не выходят и ни с чем не спорят:
        // это частные диапазоны, видимые только сетевому стеку телефона.
        private const val ADDRESS_V4 = "10.19.84.2"
        private const val PREFIX_V4 = 32
        private const val ADDRESS_V6 = "fdfe:dcba:9876::2"
        private const val PREFIX_V6 = 126

        /**
         * Частные диапазоны домашней сети — те, что не уходят за роутер.
         * Адрес самого туннеля (10.19.84.2) лежит внутри первого, но
         * исключение маршрута его не трогает: адрес интерфейса — не маршрут.
         *
         * Функция, а не поле: конструктор IpPrefix появился в Android 13, и
         * поле в companion создавалось бы при загрузке класса на любом
         * телефоне — вместе с падением на старых.
         */
        @RequiresApi(Build.VERSION_CODES.TIRAMISU)
        private fun lanPrefixes(): List<IpPrefix> = listOf(
            IpPrefix(InetAddresses.parseNumericAddress("10.0.0.0"), 8),
            IpPrefix(InetAddresses.parseNumericAddress("172.16.0.0"), 12),
            IpPrefix(InetAddresses.parseNumericAddress("192.168.0.0"), 16),
        )

        fun reasonOf(t: Throwable): String {
            val message = t.message
            return if (message.isNullOrBlank()) t.toString() else message
        }

        /**
         * failureOf разбирает ошибку ядра на вид и подробности.
         *
         * Ядро складывает их в одно сообщение — иначе не получится: наружу
         * gomobile отдаёт обычное исключение, и приложить к нему что-то ещё,
         * кроме текста, некуда. Вид едет первой строкой.
         */
        fun failureOf(t: Throwable): TunnelState.Failed {
            val raw = reasonOf(t)
            val cut = raw.indexOf('\n')
            if (cut <= 0) {
                // Вида нет — покажем как есть, это лучше, чем выдумывать.
                return TunnelState.Failed("", raw)
            }
            return TunnelState.Failed(raw.substring(0, cut), raw.substring(cut + 1).trim())
        }
    }
}
