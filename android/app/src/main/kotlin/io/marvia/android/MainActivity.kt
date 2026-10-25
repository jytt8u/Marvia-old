package io.marvia.android

import android.content.ClipboardManager
import android.content.Intent
import android.net.VpnService
import android.os.Bundle
import android.view.View
import android.view.inputmethod.InputMethodManager
import android.widget.ImageView
import android.widget.TextView
import android.widget.Toast
import androidx.activity.OnBackPressedCallback
import androidx.activity.enableEdgeToEdge
import androidx.activity.result.contract.ActivityResultContracts
import androidx.appcompat.app.AppCompatActivity
import androidx.appcompat.app.AppCompatDelegate
import androidx.core.content.ContextCompat
import androidx.core.view.ViewCompat
import androidx.core.view.WindowCompat
import androidx.core.view.doOnLayout
import androidx.core.view.WindowInsetsCompat
import androidx.core.view.isVisible
import androidx.core.widget.doAfterTextChanged
import androidx.core.view.updatePadding
import androidx.core.view.updateLayoutParams
import androidx.lifecycle.Lifecycle
import androidx.lifecycle.lifecycleScope
import androidx.lifecycle.repeatOnLifecycle
import io.marvia.android.databinding.ActivityMainBinding
import io.marvia.android.databinding.ScreenConnectBinding
import io.marvia.mobile.Mobile
import kotlinx.coroutines.flow.first
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext
import kotlinx.coroutines.withTimeoutOrNull

/**
 * MainActivity — все экраны приложения.
 *
 * Они лежат друг на друге в одной активности: подключение, выбор страны,
 * тема, «ещё» (соединение, приложения, логи), ключ доступа и выбор языка.
 * Отдельных активностей нет намеренно —
 * состояние туннеля живёт в процессе, и переключение вкладок не должно
 * пересобирать экран и терять то, что человек уже видел.
 *
 * Ключ добавляется на «Серверах»: оформление и настройки доступны до
 * первого подключения.
 */
class MainActivity : AppCompatActivity() {

    private enum class Screen { LANGUAGE, PERMS, KEY, CONNECT, INSIGHTS, SERVERS, STATS, THEME, MORE }

    private lateinit var ui: ActivityMainBinding
    private lateinit var store: Store
    private lateinit var servers: ServersScreen
    private lateinit var stats: StatsScreen
    private lateinit var insights: InsightsScreen
    private lateinit var more: MoreScreen
    private lateinit var themeScreen: ThemeScreen
    private lateinit var language: LanguageScreen
    private lateinit var perms: PermsScreen

    /** Откуда открыт выбор языка: с первого запуска возврат ведёт дальше, из настроек — назад. */
    private var languageFromSettings = false

    /** Тема на сейчас. Пересчитывается по выбору и красит всё дерево вьюх. */
    private var theme: Theme = Look.theme(Look.Choice())

    private var screen = Screen.CONNECT

    /** Был ли туннель поднят на прошлой отрисовке: по смене обновляем список стран. */
    private var wasRunning = false

    /** Системное окно «разрешить приложению создавать VPN». */
    private val consent = registerForActivityResult(
        ActivityResultContracts.StartActivityForResult(),
    ) { result ->
        if (result.resultCode == RESULT_OK) {
            launchService()
        } else {
            // Вид пустой: фразу мы уже написали сами, переводить нечего.
            MarviaState.set(TunnelState.Failed("", getString(R.string.consent_denied)))
        }
    }

    /**
     * Выбор фото под фон. GetContent, а не разрешение на «все файлы»: система
     * сама показывает выбор и отдаёт нам один файл, доступа к галерее целиком
     * не нужно. Сохраняем его к себе и перекрашиваем всё приложение.
     */
    private val pickBackdrop = registerForActivityResult(
        ActivityResultContracts.GetContent(),
    ) { uri ->
        if (uri != null && store.saveBackdrop(uri)) {
            repaint()
            // Сразу — выбрать кадр: фото редко ложится как надо само, и искать
            // потом, где это настраивается, никто не станет.
            themeScreen.editFrame()
        } else if (uri != null) {
            Toast.makeText(this, R.string.theme_backdrop_bad, Toast.LENGTH_LONG).show()
        }
    }

    /**
     * Ключ с картинки QR-кода из галереи. Распознавание — не на главном
     * потоке: большой скриншот разбирается десятки миллисекунд, и экран не
     * должен на это время замирать.
     */
    private val pickQr = registerForActivityResult(
        ActivityResultContracts.GetContent(),
    ) { uri ->
        if (uri == null) return@registerForActivityResult
        lifecycleScope.launch {
            val text = withContext(Dispatchers.Default) {
                runCatching { QrImport.fromUri(this@MainActivity, uri) }.getOrNull()
            }
            if (text.isNullOrEmpty()) {
                Toast.makeText(this@MainActivity, R.string.key_qr_missing, Toast.LENGTH_LONG).show()
            } else {
                ui.keyScreen.keyInput.setText(text)
            }
        }
    }

    private val scanQr = registerForActivityResult(ActivityResultContracts.StartActivityForResult()) { result ->
        if (result.resultCode == RESULT_OK) {
            result.data?.getStringExtra(QrCameraActivity.EXTRA_KEY)?.let { ui.keyScreen.keyInput.setText(it) }
        }
    }

    /** Своя картинка под значок в шапке — тем же путём, что фон. */
    private val pickLogo = registerForActivityResult(
        ActivityResultContracts.GetContent(),
    ) { uri ->
        if (uri != null && store.saveLogo(uri)) {
            store.logo = Store.LOGO_CUSTOM
            repaint()
        } else if (uri != null) {
            Toast.makeText(this, R.string.theme_backdrop_bad, Toast.LENGTH_LONG).show()
        }
    }

    override fun onCreate(savedInstanceState: Bundle?) {
        // Режим ночи — по темноте пресета. Он нужен не нам, а Material:
        // диалоги и системные виджеты берут цвета оттуда, и светлый диалог
        // над «Полночью» выглядел бы дырой в экране.
        store = Store(this)
        // Прежние образы ушли из выбора: один раз меняем их на ближайшие новые.
        AppearanceProfiles.migrate(store)
        theme = Look.theme(store.look)
        AppCompatDelegate.setDefaultNightMode(nightModeFor(theme))
        enableEdgeToEdge()
        super.onCreate(savedInstanceState)

        ui = ActivityMainBinding.inflate(layoutInflater)
        setContentView(ui.root)

        applyInsets()
        wireNav()
        wireConnect()
        wireKey()

        servers = ServersScreen(
            host = this,
            ui = ui.serversScreen,
            theme = { theme },
            store = store,
            onSubscriptionChanged = { restartTunnel() },
        )
        stats = StatsScreen(host = this, ui = ui.statsScreen, theme = { theme }, traffic = Traffic(this), store = store)
        insights = InsightsScreen(this, ui.insightsScreen, { theme }) { show(Screen.CONNECT) }
        themeScreen = ThemeScreen(this, ui.themeScreen, store, { pickBackdrop.launch("image/*") }, { pickLogo.launch("image/*") }) { repaint() }
        language = LanguageScreen(this, ui.languageScreen, store, { theme }) { afterLanguage() }
        perms = PermsScreen(this, ui.permsScreen, { theme }) {
            store.onboarded = true
            show(Screen.CONNECT)
        }
        more = MoreScreen(
            host = this,
            ui = ui.moreScreen,
            theme = { theme },
            store = store,
            onLanguage = {
                languageFromSettings = true
                show(Screen.LANGUAGE)
            },
            onRoutesChanged = { forceRoutes ->
                if (store.bypassRussian && forceRoutes) refreshRoutes(force = true)
                more.showPendingConnectionChange()
            },
            // IPv6 и дробление тоже читаются при подключении, но к маршрутам
            // отношения не имеют: звать ради них российский список у панели —
            // лишний запрос на каждое касание переключателя.
            onNextConnect = {
                more.showPendingConnectionChange()
            },
            onReset = { resetAll() },
            onAppearanceChanged = { repaint() },
            onTheme = { show(Screen.THEME) },
            onConnect = {
                val state = MarviaState.state.value
                if (state !is TunnelState.On && state !is TunnelState.Connecting) toggle()
            },
            onReconnect = { reconnect() },
            onServers = { show(Screen.SERVERS) },
        )

        lifecycleScope.launch {
            repeatOnLifecycle(Lifecycle.State.STARTED) {
                launch { MarviaState.state.collect { render(it) } }
            }
        }

        onBackPressedDispatcher.addCallback(this, back)

        repaint()
        show(firstScreen())
        refreshRoutes()
        acceptLinkFrom(intent)
    }

    override fun onResume() {
        super.onResume()
        // Вернулись из системного окна разрешений — карточки могли поменяться.
        if (screen == Screen.PERMS) perms.open()
        // Включён «мимо туннеля», а списка нет — пробуем скачать, не дожидаясь,
        // пока человек переключит тумблер ещё раз: раньше единственная попытка
        // была в момент включения, и неудачная оставалась навсегда.
        if (store.bypassRussian && !RuRoutes.ready(this)) refreshRoutes(force = false)
        if (screen == Screen.CONNECT) loadHomePlan()
    }

    /** Срок и остаток рабочей подписки для главной; null — ключа нет или подписка не читается. */
    private var homePlan: HomeBlocks.Plan? = null

    /**
     * loadHomePlan читает подписку из кэша ядра — того же, что у «Серверов».
     * Без refresh: обновлять подписку при каждом заходе на главную значило бы
     * ходить к продавцу на каждое переключение вкладок.
     */
    private fun loadHomePlan() {
        val link = store.accountLink
        if (link.isBlank()) {
            homePlan = null
            render(MarviaState.state.value)
            return
        }
        val name = store.subscriptions.firstOrNull { it.link == link }?.name.orEmpty()
            .ifEmpty { getString(R.string.home_plan_title) }
        lifecycleScope.launch {
            val json = withContext(Dispatchers.IO) {
                runCatching {
                    MarviaState.core?.subscription(link, store.cacheDir(), false)
                        ?: Mobile.subscription(link, store.cacheDir(), false)
                }.getOrDefault("")
            }
            // Пока читали, ключ могли сменить — чужой срок показывать нельзя.
            if (link != store.accountLink) return@launch
            homePlan = if (json.isEmpty()) null else HomeBlocks.parsePlan(name, json)
            render(MarviaState.state.value)
        }
    }

    /**
     * После выбора языка показываем главное пространство. Ключ добавляется
     * явной кнопкой: оформление можно выбрать ещё до подключения.
     */
    private fun firstScreen(): Screen = when {
        store.language.isEmpty() -> Screen.LANGUAGE
        !store.onboarded -> Screen.PERMS
        else -> Screen.CONNECT
    }

    /** afterLanguage — язык выбран: назад в настройки или дальше по первому запуску. */
    private fun afterLanguage() {
        if (languageFromSettings) {
            languageFromSettings = false
            show(Screen.MORE)
        } else {
            show(firstScreen())
        }
    }

    /** Возврат с любого экрана ведёт на подключение, а с него — из приложения. */
    private val back = object : OnBackPressedCallback(true) {
        override fun handleOnBackPressed() {
            // Из выбора языка, открытого из настроек, — назад в настройки.
            if (screen == Screen.LANGUAGE && languageFromSettings) {
                languageFromSettings = false
                show(Screen.MORE)
                return
            }
            // С подэкрана настроек (приложения, журнал) — назад в настройки.
            if (screen == Screen.MORE && more.back()) return
            if (screen == Screen.CONNECT || screen == Screen.LANGUAGE || screen == Screen.PERMS) {
                isEnabled = false
                onBackPressedDispatcher.onBackPressed()
                isEnabled = true
                return
            }
            show(Screen.CONNECT)
        }
    }

    // ---------------------------------------------------------------- тема

    /**
     * repaint пересчитывает тему по выбору и красит всё приложение.
     *
     * Дерево вьюх красится по тегам, состояние туннеля — своей отрисовкой,
     * экраны с динамикой — своими paint. Если пресет сменил тёмное на светлое
     * или обратно, режим ночи переключаем тут же: uiMode объявлен в манифесте,
     * и система экран не пересоздаёт, а значит, красить всё равно нам —
     * ранний выход здесь оставлял светлую тему на тёмных карточках до
     * следующего запуска.
     */
    private fun repaint() {
        theme = Look.theme(store.look)

        val night = nightModeFor(theme)
        if (AppCompatDelegate.getDefaultNightMode() != night) {
            AppCompatDelegate.setDefaultNightMode(night)
        }

        Paint.style = Paint.Style(
            pattern = store.pattern, font = store.font, photo = store.hasBackdrop(),
            motion = !store.reduceMotion,
            appearanceStyle = store.appearanceStyle, vibe = AppearanceProfiles.detail(store), home = store.homeView,
        )
        ConnectionAppearance.apply(ui.connectScreen, store.appearanceStyle, theme)
        ui.connectScreen.powerAction.motionEnabled = !store.reduceMotion
        if (store.reduceMotion) {
            listOf(ui.connectScreen.root, ui.serversScreen.root, ui.statsScreen.root,
                ui.themeScreen.root, ui.moreScreen.root, ui.insightsScreen.root,
                ui.connectScreen.nodeLine, ui.connectScreen.todayCard, ui.connectScreen.tilesRow,
                ui.connectScreen.statusText, ui.connectScreen.heroName).forEach {
                it.animate().cancel()
                it.alpha = 1f
                it.translationX = 0f
                it.translationY = 0f
            }
        }
        Paint.apply(ui.root, theme)
        // Зерно и виньетка «Плёнки» — поверх всего, как на ПК; у прочих образов ничего.
        applyBackdrop()
        paintLogo()
        paintNav()
        servers.paint()
        stats.paint(theme)
        insights.paint()
        themeScreen.paint(theme)
        more.paint()
        render(MarviaState.state.value)

        // Часы и кнопки системы над нашим фоном: тёмные на светлой теме,
        // светлые на тёмной. Иначе на «Бумаге» строка состояния пропадает.
        val bars = WindowCompat.getInsetsController(window, ui.root)
        bars.isAppearanceLightStatusBars = !theme.dark
        bars.isAppearanceLightNavigationBars = !theme.dark
    }

    /** paintLogo — что в шапке главной: знак, своя картинка или ничего. */
    private fun paintLogo() {
        val c = ui.connectScreen
        val mode = store.logo
        val custom = if (mode == Store.LOGO_CUSTOM) store.logoBitmap() else null
        c.heroMarkButton.isVisible = mode != Store.LOGO_NONE
        c.heroMark.isVisible = mode == Store.LOGO_MARVIA || (mode == Store.LOGO_CUSTOM && custom == null)
        c.heroCustom.isVisible = custom != null
        if (custom != null) {
            c.heroCustom.setImageBitmap(custom)
            c.heroCustom.clipToOutline = true
            c.heroCustom.outlineProvider = object : android.view.ViewOutlineProvider() {
                override fun getOutline(view: View, outline: android.graphics.Outline) {
                    outline.setRoundRect(0, 0, view.width, view.height, 9 * resources.displayMetrics.density)
                }
            }
        }
    }

    private fun nightModeFor(t: Theme): Int =
        if (t.dark) AppCompatDelegate.MODE_NIGHT_YES else AppCompatDelegate.MODE_NIGHT_NO

    /**
     * applyBackdrop кладёт свой фон поверх фона темы. Paint уже поставил корню
     * Backdrop(t); если у человека выбрано фото — заменяем им. Пелена цветом
     * фона темы держит читаемость, как и в панели.
     */
    private fun applyBackdrop() {
        val stamp = store.backdropStamp()
        if (stamp == 0L) {
            backdropBitmap?.recycle()
            backdropBitmap = null
            backdropStamp = 0L
            return
        }
        // Снимок декодируем один раз на файл, а не на каждую перерисовку:
        // ползунок затемнения зовёт repaint на каждом шаге, и читать JPEG в
        // две тысячи точек на каждый — это рывки вместо плавной пелены.
        if (backdropBitmap == null || backdropStamp != stamp) {
            backdropBitmap?.recycle()
            backdropBitmap = store.backdropBitmap()
            backdropStamp = stamp
        }
        val bmp = backdropBitmap ?: return
        val veil = colorWithAlpha(theme.bg, store.backdropDim)
        ui.root.background = BackdropImage(bmp, store.backdropFit, theme.bg, veil, store.backdropFrame)
    }

    private var backdropBitmap: android.graphics.Bitmap? = null
    private var backdropStamp = 0L

    /** colorWithAlpha — цвет фона темы с долей непрозрачности из процента затемнения. */
    private fun colorWithAlpha(color: Int, dimPercent: Int): Int {
        val a = (dimPercent.coerceIn(0, 95) * 255 / 100)
        return (a shl 24) or (color and 0x00FFFFFF)
    }

    // -------------------------------------------------------------- экраны

    private fun show(next: Screen) {
        val was = screen
        if (was == Screen.MORE && next != Screen.MORE) more.hidden()
        screen = next

        ui.keyScreen.root.isVisible = next == Screen.KEY
        ui.connectScreen.root.isVisible = next == Screen.CONNECT
        ui.insightsScreen.root.isVisible = next == Screen.INSIGHTS
        ui.serversScreen.root.isVisible = next == Screen.SERVERS
        ui.statsScreen.root.isVisible = next == Screen.STATS
        ui.moreScreen.root.isVisible = next == Screen.MORE
        ui.themeScreen.root.isVisible = next == Screen.THEME
        ui.languageScreen.root.isVisible = next == Screen.LANGUAGE
        ui.permsScreen.root.isVisible = next == Screen.PERMS
        // Экран поднимается снизу и проявляется, как .rise в макете: смена
        // вкладки без движения выглядит как сбой отрисовки, а не как переход.
        if (was != next && !store.reduceMotion) {
            val root = when (next) {
                Screen.CONNECT -> ui.connectScreen.root
                Screen.INSIGHTS -> ui.insightsScreen.root
                Screen.SERVERS -> ui.serversScreen.root
                Screen.STATS -> ui.statsScreen.root
                Screen.THEME -> ui.themeScreen.root
                Screen.MORE -> ui.moreScreen.root
                Screen.KEY -> ui.keyScreen.root
                Screen.LANGUAGE -> ui.languageScreen.root
                Screen.PERMS -> ui.permsScreen.root
            }
            root.alpha = 0f
            root.translationY = 12 * resources.displayMetrics.density
            root.animate().alpha(1f).translationY(0f).setDuration(420).setInterpolator(android.view.animation.DecelerateInterpolator(2f)).start()
            // На главной карточки внизу догоняют с шагом: экран собирается
            // сверху вниз, а не падает целиком.
            if (next == Screen.CONNECT) {
                val c = ui.connectScreen
                listOf(c.infoRow, c.todayCard, c.tilesRow).forEachIndexed { i, v ->
                    v.alpha = 0f
                    v.translationY = 18 * resources.displayMetrics.density
                    v.animate().alpha(1f).translationY(0f).setStartDelay(60L + 70L * i).setDuration(460)
                        .setInterpolator(android.view.animation.DecelerateInterpolator(2.2f)).start()
                }
            }
        }

        // Панель есть и без ключа: подписка добавляется на «Серверах», а тему
        // можно выбрать до подключения. На выборе языка её нет — это экран
        // одного действия.
        ui.nav.root.isVisible = next != Screen.LANGUAGE && next != Screen.PERMS && next != Screen.INSIGHTS
        paintNav()

        when (next) {
            Screen.SERVERS -> servers.open()
            Screen.STATS -> stats.open()
            Screen.MORE -> more.open()
            Screen.THEME -> themeScreen.paint(theme)
            Screen.LANGUAGE -> language.open()
            Screen.PERMS -> perms.open()
            Screen.KEY -> openKey()
            Screen.CONNECT -> loadHomePlan()
            Screen.INSIGHTS -> Unit
        }

        // Панель то появляется, то нет — а отступ под системной навигацией
        // должен остаться в обоих случаях.
        ViewCompat.requestApplyInsets(ui.root)
    }

    private fun wireNav() {
        ui.nav.navConnect.setOnClickListener { show(Screen.CONNECT) }
        ui.nav.navServers.setOnClickListener { show(Screen.SERVERS) }
        ui.nav.navStats.setOnClickListener { show(Screen.STATS) }
        ui.nav.navTheme.setOnClickListener { show(Screen.THEME) }
        ui.nav.navMore.setOnClickListener { show(Screen.MORE) }
    }

    private fun paintNav() {
        val n = ui.nav
        paintTab(n.navConnectPill, n.navConnectIcon, n.navConnectLabel, screen == Screen.CONNECT)
        paintTab(n.navServersPill, n.navServersIcon, n.navServersLabel, screen == Screen.SERVERS)
        paintTab(n.navStatsPill, n.navStatsIcon, n.navStatsLabel, screen == Screen.STATS)
        paintTab(n.navThemePill, n.navThemeIcon, n.navThemeLabel, screen == Screen.THEME)
        paintTab(n.navMorePill, n.navMoreIcon, n.navMoreLabel, screen == Screen.MORE)
    }

    /**
     * Активная вкладка отмечена заливкой акцента и контрастным значком.
     * Без внешнего ripple: широкое пятно закрывало соседние подписи.
     */
    private fun paintTab(pill: View, icon: ImageView, label: TextView, active: Boolean) {
        Paint.navigationTab(pill, icon, label, active, theme)
    }

    // --------------------------------------------------------- подключение

    private fun wireConnect() {
        if (MarviaState.state.value !is TunnelState.On) MarviaState.traffic.value = TrafficHistory(this).saved()
        val c = ui.connectScreen
        c.powerAction.setOnClickListener { toggle() }
        c.accessAction.setOnClickListener { servers.askWhereFrom() }
        c.techText.setOnClickListener { more.openLogs(); show(Screen.MORE) }
        c.nodeLine.setOnClickListener { show(Screen.SERVERS) }
        c.statusText.setOnClickListener {
            if (store.accountLink.isBlank()) show(Screen.SERVERS)
        }
        c.downCard.setOnClickListener { show(Screen.INSIGHTS); insights.open(InsightsScreen.Mode.SPEED) }
        c.upCard.setOnClickListener { show(Screen.INSIGHTS); insights.open(InsightsScreen.Mode.SPEED) }
        c.powerHint.setOnClickListener {
            if (store.accountLink.isBlank()) show(Screen.SERVERS)
            else if (MarviaState.state.value is TunnelState.On) { show(Screen.INSIGHTS); insights.open(InsightsScreen.Mode.SESSIONS) }
        }
        c.todayCard.setOnClickListener { show(Screen.STATS) }
        c.placeCard.setOnClickListener { show(Screen.SERVERS) }
        c.planCard.setOnClickListener { show(Screen.SERVERS) }
        c.planRenew.setOnClickListener { homePlan?.renew?.let { SellerChannel.open(this, it) } }
        c.consoleAction.setOnClickListener {
            if (store.accountLink.isBlank()) servers.askWhereFrom() else toggle()
        }

        // Нажатие на знак позволяет свернуть или вернуть подпись бренда.
        c.heroTagline.text = getString(R.string.hero_subtitle)
        c.heroName.isVisible = false
        c.heroWord.setTag(R.id.keep_font, true)
        // Имя — металлом, как знак: сверху светлое, книзу в приглушённый.
        c.heroWord.doOnLayout {
            c.heroWord.paint.shader = android.graphics.LinearGradient(
                0f, 0f, 0f, c.heroWord.height.toFloat(),
                intArrayOf(theme.fg, theme.fg, theme.dim), floatArrayOf(0f, 0.42f, 1f), android.graphics.Shader.TileMode.CLAMP,
            )
            c.heroWord.invalidate()
        }
        c.heroMarkButton.setOnClickListener {
            // Тридцать нажатий подряд — змейка в цветах темы.
            if (SnakeGame.tap()) {
                SnakeGame.show(this, theme, store)
                return@setOnClickListener
            }
            c.heroName.animate().cancel()
            val show = !c.heroName.isVisible
            if (store.reduceMotion) {
                c.heroName.isVisible = show
                ConnectionAppearance.header(c, store.appearanceStyle)
            } else if (show) {
                c.heroName.alpha = 0f
                c.heroName.translationX = -10 * resources.displayMetrics.density
                c.heroName.isVisible = true
                ConnectionAppearance.header(c, store.appearanceStyle)
                c.heroName.animate().alpha(1f).translationX(0f).setDuration(450).setInterpolator(android.view.animation.DecelerateInterpolator(2f)).start()
            } else {
                c.heroName.animate().alpha(0f).setDuration(200).withEndAction {
                    c.heroName.isVisible = false
                    ConnectionAppearance.header(c, store.appearanceStyle)
                }.start()
            }
        }
        lifecycleScope.launch {
            repeatOnLifecycle(Lifecycle.State.STARTED) {
                MarviaState.traffic.collect { t ->
                    c.todayTotal.text = Format.size(this@MainActivity, t.today)
                    c.todayBars.hours = t.hours
                    showSpeed(c, t)
                    if (screen == Screen.INSIGHTS) insights.refresh()
                }
            }
        }
    }

    private fun render(state: TunnelState) {
        val c = ui.connectScreen
        val hasKey = store.accountLink.isNotBlank()
        c.accessAction.isVisible = !hasKey && state != TunnelState.Connecting
        c.todayCard.isVisible = store.homeStats && hasKey
        c.tilesRow.isVisible = store.homeStats && hasKey
        c.connectionBadge.setText(when (state) {
            TunnelState.Off -> R.string.connect_badge_off
            TunnelState.Connecting -> R.string.connect_badge_wait
            is TunnelState.On -> if (state.warning.isBlank()) R.string.connect_badge_on else R.string.connect_badge_error
            is TunnelState.Failed -> R.string.connect_badge_error
        })
        c.connectionBadge.setTextColor(when (state) {
            is TunnelState.Failed -> theme.fail
            is TunnelState.On -> if (state.warning.isBlank()) theme.acc else theme.warn
            else -> theme.dim
        })
        if (state !is TunnelState.On) more.clearPendingConnectionChange()

        c.techText.isVisible = false
        c.powerAction.state = when (state) {
            TunnelState.Off -> PowerButton.State.OFF
            TunnelState.Connecting -> PowerButton.State.CONNECTING
            is TunnelState.On -> PowerButton.State.ON
            is TunnelState.Failed -> PowerButton.State.FAILED
        }
        // Строка под состоянием: нода, а пока её нет — что делаем. Нажатие
        // ведёт к выбору сервера, поэтому строка есть и без туннеля.
        c.nodeLine.text = when (state) {
            TunnelState.Connecting -> getString(R.string.connect_choosing)
            is TunnelState.On -> listOf(state.node, if (state.ms > 0) getString(R.string.node_ms, state.ms) else "")
                .filter { it.isNotEmpty() }.joinToString(" · ")
            else -> lastNode
        }
        if (state is TunnelState.On) lastNode = state.node
        c.nodeLine.isVisible = c.nodeLine.text.isNotEmpty()
        c.powerHint.setText(when (state) {
            TunnelState.Off -> if (hasKey) R.string.power_hint_start else R.string.connect_signal_add
            TunnelState.Connecting -> R.string.power_hint_cancel
            is TunnelState.On -> R.string.power_hint_stop
            is TunnelState.Failed -> R.string.connect_retry
        })
        // Под «Подключено» — время сессии: плитка «Сессия» уступила место
        // скорости в обе стороны, а время человек ищет рядом с состоянием.
        if (state is TunnelState.On) c.powerHint.text = sessionLine(MarviaState.traffic.value.seconds)
        c.statusText.isClickable = !hasKey
        c.statusText.isFocusable = !hasKey

        when (state) {
            TunnelState.Off -> {
                status(if (hasKey) R.string.status_off else R.string.connect_signal_ready, theme.fg)
                c.powerAction.contentDescription = getString(if (hasKey) R.string.action_connect else R.string.connect_add_key)
                paintPower(theme.acc)
                c.nodeNote.text = ""
            }

            TunnelState.Connecting -> {
                status(R.string.status_connecting, theme.dim)
                c.powerAction.contentDescription = getString(R.string.status_connecting)
                paintPower(theme.acc)
                c.nodeNote.text = ""
            }

            is TunnelState.On -> {
                // Без сети «Подключено» — неправда, которую человек проверяет
                // открытым сайтом и списывает на VPN.
                if (state.warning == getString(R.string.trouble_offline)) {
                    status(R.string.status_offline, theme.warn)
                } else {
                    status(R.string.status_on, if (theme.dark) theme.fg else theme.acc)
                }
                c.powerAction.contentDescription = getString(R.string.connect_disconnect)
                paintPower(theme.acc)
                c.nodeNote.text = choiceText(state)

                // Ошибка отдельного соединения туннель не роняет, но молчать о
                // ней нельзя: иначе человек видит «подключено» при наполовину
                // живой ноде.
                if (state.warning.isNotBlank()) {
                    // Две беды называем как есть: «нода молчит» и «нет сети»
                    // объясняют всё сами, и «некоторые запросы не прошли»
                    // вместо них только запутало бы.
                    val plain = state.warning == getString(R.string.trouble_no_node) ||
                        state.warning == getString(R.string.trouble_offline)
                    c.techText.text = if (plain) state.warning else getString(R.string.connection_warning_details)
                    c.techText.setTextColor(theme.warn)
                    c.techText.isVisible = true
                }

            }

            is TunnelState.Failed -> {
                status(R.string.status_failed, theme.fail)
                c.powerAction.contentDescription = getString(R.string.connect_retry)
                paintPower(theme.fail)

                val human = humanReasonFor(state.kind)
                if (human == null) {
                    // Вида нет — значит фраза уже человеческая, показываем её.
                    c.nodeNote.setText(R.string.connect_error_hint)
                    c.techText.setText(R.string.connection_warning_details)
                    c.techText.isVisible = state.detail.isNotBlank()
                } else {
                    c.nodeNote.setText(human)
                    c.techText.setText(R.string.connection_warning_details)
                    c.techText.setTextColor(theme.dim)
                    c.techText.isVisible = state.detail.isNotBlank()
                }
            }
        }
        // Пустая строка — это не строка: место под неё занимать незачем.
        c.nodeNote.isVisible = c.nodeNote.text.isNotEmpty()
        renderHome(state, hasKey)

        renderSubscription(state)

        // Список стран живёт внутри туннеля: он появляется и пропадает вместе
        // с ним, и открытый экран стран должен это заметить.
        val running = state is TunnelState.On
        if (running != wasRunning) {
            wasRunning = running
            if (screen == Screen.SERVERS) {
                servers.open()
            }
        }
    }

    /**
     * renderHome — блоки главной под раскладку образа: «где вы», подписка,
     * строки консоли и её кнопка. Без ключа рассказывать не о чем — остаётся
     * одна кнопка «Добавить ключ».
     */
    private fun renderHome(state: TunnelState, hasKey: Boolean) {
        val c = ui.connectScreen
        val layout = HomeLayout.of(store.homeView)
        val on = state as? TunnelState.On
        val place = HomeBlocks.Place(
            title = on?.node ?: lastNode,
            ms = on?.ms ?: 0,
            live = on != null,
            manual = on != null && HomeBlocks.pickedHere(on.chosen, on.node),
        )
        c.placeCard.isVisible = hasKey && layout.cards
        if (c.placeCard.isVisible) HomeBlocks.place(c, place, theme)
        HomeBlocks.plan(c, if (hasKey) homePlan else null, theme, layout)
        c.nodeLine.isVisible = layout == HomeLayout.MARVIA && c.nodeLine.text.isNotEmpty()
        c.tilesRow.isVisible = store.homeStats && hasKey && layout != HomeLayout.CONSOLE
        c.consoleAction.isVisible = layout == HomeLayout.CONSOLE
        c.readout.isVisible = layout == HomeLayout.CONSOLE && hasKey
        if (layout == HomeLayout.CONSOLE) {
            c.accessAction.isVisible = false
            c.consoleAction.text = HomeBlocks.consoleLabel(this, state, hasKey)
            c.consoleAction.contentDescription = c.consoleAction.text
            if (hasKey) HomeBlocks.readout(c, consoleRows(place), theme)
        }
    }

    /** Строки консоли: нода, срок, трафик по подписке и скорость; «сегодня» — в карточке с часами ниже. */
    private fun consoleRows(place: HomeBlocks.Place): List<Pair<String, String>> = buildList {
        add(getString(R.string.home_r_node) to when {
            place.title.isEmpty() -> getString(R.string.home_place_auto)
            place.live && place.ms > 0 -> place.title + "  " + getString(R.string.node_ms, place.ms)
            else -> place.title
        })
        homePlan?.let { p ->
            val days = if (p.until.isEmpty()) null else Format.daysLeft(p.until)
            add(getString(R.string.home_r_term) to (days?.let { resources.getQuantityString(R.plurals.days_left, it.coerceAtLeast(0), it.coerceAtLeast(0)) }
                ?: getString(R.string.servers_no_term)))
            add(getString(R.string.home_r_quota) to (if (p.limit > 0) getString(R.string.home_plan_left, Format.size(this@MainActivity, p.left), Format.size(this@MainActivity, p.limit))
                else getString(R.string.home_plan_unlimited)))
        }
        if (MarviaState.state.value is TunnelState.On) {
            val last = SessionDetails.snapshot().samples.lastOrNull()
            fun mbps(b: Double) = String.format(java.util.Locale.getDefault(), "%.1f", b * 8 / 1_000_000)
            add(getString(R.string.home_r_speed) to getString(R.string.home_r_speed_value, mbps(last?.down ?: 0.0), mbps(last?.up ?: 0.0)))
        }
    }

    /**
     * choiceText — кто выбрал ноду, через которую идёт трафик.
     *
     * Выбор человека и текущая нода расходятся, когда выбранная замолчала и
     * ядро уехало на живую. Написать в этот момент «выбрана ОАЭ» над словом
     * «Финляндия» значило бы соврать в единственном месте, где человек
     * проверить нас не может.
     */
    private fun choiceText(state: TunnelState.On): String = when {
        // Автовыбор — это молчание: он и так по умолчанию, и сообщать о нём
        // нечего. Строка появляется, только когда есть что сказать: человек
        // выбрал страну сам, или выбрал одну, а ядро уехало на другую.
        state.chosen.isEmpty() -> ""
        // Карточка «где вы» уже пишет «выбрана вами» — второй раз то же
        // под состоянием только шумит. Без карточки (консоль, табло) строка нужна.
        state.chosen == state.node -> if (ui.connectScreen.placeCard.isVisible) "" else getString(R.string.connect_manual)
        else -> getString(R.string.connect_manual_moved, state.chosen)
    }

    /** Последняя нода, через которую шёл трафик: её показываем и выключенными — и после перезапуска. */
    private var lastNode: String
        get() = store.lastNode
        set(value) { store.lastNode = value }

    /** Состояние — одной крупной строкой под кнопкой, цветом состояния. */
    /** Состояние крупно; новое слово проявляется, а не подменяет прежнее рывком. */
    private fun status(text: Int, color: Int) {
        val v = ui.connectScreen.statusText
        val changed = v.text.toString() != getString(text)
        v.setText(text)
        v.setTextColor(color)
        if (changed && v.isAttachedToWindow && v.isShown && !store.reduceMotion) {
            v.alpha = 0f
            v.translationY = 6 * resources.displayMetrics.density
            v.animate().alpha(1f).translationY(0f).setDuration(360).setInterpolator(android.view.animation.DecelerateInterpolator(2f)).start()
        }
    }

    /** Скорость в обе стороны: цифра — последний замер, линия — полминуты. */
    private fun showSpeed(c: ScreenConnectBinding, t: TrafficSnapshot) {
        val samples = SessionDetails.snapshot().samples
        val last = samples.lastOrNull()
        val on = MarviaState.state.value is TunnelState.On
        fun mbps(bytes: Double) = getString(R.string.stats_mbps, String.format(java.util.Locale.getDefault(), "%.1f", bytes * 8 / 1_000_000))
        c.downValue.text = mbps(if (on) last?.down ?: 0.0 else 0.0)
        c.upValue.text = mbps(if (on) last?.up ?: 0.0 else 0.0)
        c.downSpark.show(if (on) samples.map { it.down } else emptyList())
        c.upSpark.show(if (on) samples.map { it.up } else emptyList())
        if (on) c.powerHint.text = sessionLine(t.seconds)
    }

    private fun sessionLine(seconds: Long) = getString(R.string.session_running,
        String.format(java.util.Locale.US, "%02d:%02d:%02d", seconds / 3600, seconds / 60 % 60, seconds % 60))

    /** Цвет ленты — личный выбор; состояние передаём дугой и строкой. */
    private fun paintPower(color: Int) {
        val c = ui.connectScreen
        c.todayBars.theme = theme
        c.downSpark.theme = theme
        c.upSpark.theme = theme
        c.halo.theme = theme
        c.halo.isVisible = !c.powerAction.dottedPower
        c.halo.lit = MarviaState.state.value is TunnelState.On
        c.powerAction.theme = theme.copy(acc = color)
    }

    /**
     * renderSubscription — срок и остаток живут на «Серверах», у своего
     * провайдера. Здесь от подписки остаётся одно: есть ли новая версия.
     */
    private fun renderSubscription(state: TunnelState) {
        more.showUpdate((state as? TunnelState.On)?.subscription)
    }

    // --------------------------------------------------------------- ключ

    private fun wireKey() {
        val k = ui.keyScreen
        k.pasteButton.setOnClickListener { paste() }
        k.qrButton.setOnClickListener { pickQr.launch("image/*") }
        k.cameraButton.setOnClickListener { scanQr.launch(Intent(this, QrCameraActivity::class.java)) }
        k.keySaveButton.setOnClickListener { onKeyButton() }
        // Ошибка относится к тексту, который в поле был, а не к новому: после
        // вставки или QR красная строка про прежний ключ только путает.
        k.keyInput.doAfterTextChanged { k.keyError.isVisible = false }
    }

    private fun openKey() {
        val k = ui.keyScreen
        k.keyInput.setText(store.accountLink)
        k.keyError.isVisible = false
        // Туннель уже работает — кнопка сохраняет ключ, а не подключает: два
        // разных действия под одной надписью человек нажимает наугад.
        k.keySaveButton.setText(
            if (MarviaState.state.value is TunnelState.On) R.string.action_save else R.string.action_connect,
        )
    }

    /**
     * paste берёт ссылку из буфера обмена.
     *
     * Ключ приходит в чате, и до приложения он почти всегда доезжает именно
     * так: человек скопировал и не знает, куда вставить.
     */
    private fun paste() {
        val clipboard = getSystemService(ClipboardManager::class.java)
        val text = clipboard?.primaryClip
            ?.takeIf { it.itemCount > 0 }
            ?.getItemAt(0)
            ?.coerceToText(this)
            ?.toString()
            ?.trim()
            .orEmpty()

        if (text.isEmpty()) {
            Toast.makeText(this, R.string.key_clipboard_empty, Toast.LENGTH_SHORT).show()
            return
        }
        ui.keyScreen.keyInput.setText(text)
    }

    private fun onKeyButton() {
        if (!saveKey()) {
            return
        }

        // Работающий туннель на новый ключ не переводим сами: это обрыв связи
        // посреди чужого дела, и решать про него человеку.
        val running = MarviaState.state.value is TunnelState.On
        // Через firstScreen, а не сразу на подключение: ключ мог приехать
        // ссылкой при самом первом запуске, и язык ещё не выбран.
        show(firstScreen())
        if (!running) {
            toggle()
        }
    }

    private fun saveKey(): Boolean {
        val k = ui.keyScreen
        val link = k.keyInput.text?.toString()?.trim().orEmpty()
        if (link.isEmpty()) {
            showKeyError(getString(R.string.key_empty))
            return false
        }

        // Разбираем ссылку ядром, а не своим кодом на Kotlin. Правило, что
        // считать правильной ссылкой, живёт в одном месте — иначе приложение
        // и сервер однажды разойдутся во мнениях, и разбираться в этом будет
        // человек, который просто хотел включить интернет.
        try {
            Mobile.checkAccountLink(link)
        } catch (t: Throwable) {
            showKeyError(MarviaVpnService.reasonOf(t))
            return false
        }

        k.keyError.isVisible = false
        store.accountLink = link
        hideKeyboard()
        Toast.makeText(this, R.string.key_saved, Toast.LENGTH_SHORT).show()
        refreshRoutes()
        return true
    }

    private fun showKeyError(text: String) {
        ui.keyScreen.keyError.text = text
        ui.keyScreen.keyError.isVisible = true
    }

    /**
     * acceptLinkFrom подставляет ключ из нажатой ссылки marvia://.
     *
     * Отправить её может любое приложение — телеграм, браузер, что угодно.
     * Поэтому замена уже стоящего ключа спрашивается: подменённая ссылка увела
     * бы весь трафик покупателя на серверы того, кто её подсунул, а заметить
     * это ему нечем. Когда ключа ещё нет, спрашивать не о чем — это обычная
     * первая настройка, ради которой ссылка и придумана.
     */
    private fun acceptLinkFrom(intent: Intent) {
        val link = intent.data?.toString()?.trim().orEmpty()
        if (link.isEmpty()) {
            return
        }

        // Ссылку из намерения убираем сразу: иначе возврат из системного окна
        // разрешения или смена темы подставит её заново.
        intent.data = null

        if (store.accountLink.isBlank() || store.accountLink == link) {
            takeLink(link)
            return
        }

        ThemedDialogs.builder(this, theme)
            .setTitle(R.string.key_replace_title)
            .setMessage(R.string.key_replace_body)
            .setPositiveButton(R.string.key_replace_yes) { _, _ -> takeLink(link) }
            .setNegativeButton(R.string.key_replace_no, null)
            .show()
    }

    private fun takeLink(link: String) {
        show(Screen.KEY)
        ui.keyScreen.keyInput.setText(link)
        if (saveKey()) {
            show(firstScreen())
        }
    }

    /**
     * onNewIntent ловит ссылку, когда приложение уже открыто.
     *
     * У активности launchMode=singleTask, и второй раз onCreate не позовут: без
     * этого нажатие на ссылку при открытом приложении не делало бы ничего, и
     * человек решил бы, что ссылка нерабочая.
     */
    override fun onNewIntent(intent: Intent) {
        super.onNewIntent(intent)
        setIntent(intent)
        acceptLinkFrom(intent)
    }

    // ------------------------------------------------------------- прочее

    /** refreshRoutes подтягивает список подсетей с панели продавца. */
    private fun refreshRoutes(force: Boolean = true) {
        val link = store.accountLink.takeIf { it.isNotBlank() } ?: return
        lifecycleScope.launch {
            withContext(Dispatchers.IO) { RuRoutes.refresh(applicationContext, link, force) }
            // Строка под переключателем говорит, скачан ли список, — обновить её.
            more.paint()
        }
    }

    /**
     * applyInsets отодвигает содержимое от системных панелей.
     *
     * С Android 15 приложение рисуется под строкой состояния и панелью
     * навигации, хочет оно того или нет. Без этого заголовок уезжает под часы,
     * а нижняя панель — под системную навигацию.
     */
    private fun applyInsets() {
        val pad = ui.nav.navBar.paddingBottom
        ViewCompat.setOnApplyWindowInsetsListener(ui.root) { _, insets ->
            val bars = insets.getInsets(WindowInsetsCompat.Type.systemBars())
            val ime = insets.getInsets(WindowInsetsCompat.Type.ime())

            ui.nav.navBar.updatePadding(bottom = pad)
            ui.nav.root.updateLayoutParams<android.view.ViewGroup.MarginLayoutParams> {
                bottomMargin = bars.bottom + (8 * resources.displayMetrics.density).toInt()
            }
            // Когда панели нет, её отступ забирает содержимое — иначе экран
            // ключа упирается в системную навигацию.
            val below = if (ui.nav.root.isVisible) 0 else bars.bottom
            ui.content.updatePadding(top = bars.top, bottom = maxOf(below, ime.bottom))
            insets
        }
    }

    private fun toggle() {
        val state = MarviaState.state.value
        if (state is TunnelState.On || state is TunnelState.Connecting) {
            startService(
                Intent(this, MarviaVpnService::class.java).setAction(MarviaVpnService.ACTION_STOP),
            )
            return
        }

        if (store.accountLink.isBlank()) {
            servers.askWhereFrom()
            return
        }

        // Система спрашивает разрешение один раз на установку — но спросить
        // всё равно надо каждый раз: его могли отозвать, включив другой VPN.
        val intent = VpnService.prepare(this)
        if (intent == null) {
            launchService()
        } else {
            consent.launch(intent)
        }
    }

    /**
     * restartTunnel — подписка сменилась, а с ней и ключ покупателя.
     *
     * Ядро держит ключ на всё время работы, поэтому поднятый туннель надо
     * ронять: иначе человек сменил продавца, а трафик продолжает идти через
     * прежнего — и нигде это не написано.
     */
    /**
     * resetAll — «сбросить всё»: туннель вниз, настройки в ноль, экран заново.
     * Пересоздаём активность, а не чистим экраны по одному: первый запуск и
     * так умеет начинать с пустого места, и второго пути быть не должно.
     */
    private fun resetAll() {
        if (MarviaState.state.value is TunnelState.On || MarviaState.state.value is TunnelState.Connecting) {
            startService(Intent(this, MarviaVpnService::class.java).setAction(MarviaVpnService.ACTION_STOP))
        }
        store.resetAll()
        recreate()
    }

    private fun restartTunnel() {
        if (MarviaState.state.value !is TunnelState.On) return
        startService(
            Intent(this, MarviaVpnService::class.java).setAction(MarviaVpnService.ACTION_STOP),
        )
    }

    /**
     * reconnect — «Переподключиться» из «Диагностики»: туннель заново с тем,
     * что сейчас выбрано. Без него дробление, включённое кнопкой рядом, ждало
     * бы, пока человек сам догадается нажать «Отключить» и «Подключить».
     *
     * Новую службу запускаем только после того, как прежняя разрушилась:
     * запуск, пришедший до её onDestroy, достался бы остановленной службе, и
     * onDestroy оборвал бы новое подключение.
     */
    private fun reconnect() {
        val state = MarviaState.state.value
        if (state !is TunnelState.On && state !is TunnelState.Connecting) {
            toggle()
            return
        }
        startService(Intent(this, MarviaVpnService::class.java).setAction(MarviaVpnService.ACTION_STOP))
        lifecycleScope.launch {
            withTimeoutOrNull(RECONNECT_WAIT_MS) { MarviaState.serviceAlive.first { !it } }
            val now = MarviaState.state.value
            if (now !is TunnelState.On && now !is TunnelState.Connecting) toggle()
        }
    }

    private fun launchService() {
        ContextCompat.startForegroundService(this, Intent(this, MarviaVpnService::class.java))
    }

    /**
     * humanReasonFor подбирает фразу под вид неудачи, названный ядром.
     *
     * Ядро говорит точно: «dial tcp: lookup panel.example: no such host». Для
     * разбора это незаменимо, для покупателя — пустой звук. Поэтому наверху
     * стоит фраза с указанием, что делать, а точный текст остаётся ниже
     * мелким: его покупатель и снимет вместе с экраном для продавца.
     *
     * null означает, что вида нет и подбирать нечего.
     */
    private fun humanReasonFor(kind: String): Int? = when (kind) {
        Mobile.FailAccount -> R.string.fail_account
        Mobile.FailPanel -> R.string.fail_panel
        Mobile.FailNodes -> R.string.fail_nodes
        Mobile.FailSystem -> R.string.fail_system
        Mobile.FailExpired -> R.string.fail_expired
        Mobile.FailQuota -> R.string.fail_quota
        else -> null
    }

    private fun hideKeyboard() {
        val manager = getSystemService(InputMethodManager::class.java)
        manager?.hideSoftInputFromWindow(ui.keyScreen.keyInput.windowToken, 0)
        ui.keyScreen.keyInput.clearFocus()
    }
}

/** Сколько ждать, пока прежняя служба VPN разрушится, прежде чем поднять новую. */
private const val RECONNECT_WAIT_MS = 10_000L
