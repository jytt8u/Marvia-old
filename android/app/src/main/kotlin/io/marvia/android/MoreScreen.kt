package io.marvia.android

import android.content.ActivityNotFoundException
import android.content.ClipData
import android.content.ClipboardManager
import android.content.Intent
import android.content.res.ColorStateList
import android.content.pm.PackageManager
import android.graphics.Typeface
import android.net.Uri
import android.provider.Settings as AndroidSettings
import android.view.Gravity
import android.view.LayoutInflater
import android.view.View
import android.view.ViewGroup
import android.widget.LinearLayout
import android.widget.TextView
import android.widget.Toast
import androidx.appcompat.app.AlertDialog
import androidx.appcompat.app.AppCompatActivity
import androidx.core.view.isVisible
import androidx.lifecycle.lifecycleScope
import androidx.recyclerview.widget.LinearLayoutManager
import androidx.recyclerview.widget.RecyclerView
import io.marvia.android.databinding.ItemAppBinding
import io.marvia.android.databinding.ScreenMoreBinding
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext

/**
 * MoreScreen — «Настройки»: одна страница по макету, с подэкранами
 * приложений, дополнительных параметров, журнала и «О нас».
 *
 * Здесь нет ручек протокола:
 * ни числа соединений, ни выбора мультиплексора, ни отпечатка TLS. Такие
 * настройки бывают у оболочек над чужим движком, которому надо объяснить, к
 * какому серверу он подключается. У нас клиент и нода — одна система, и все
 * эти решения приняты внутри; вынести их на экран значило бы дать человеку
 * сломать себе связь, не понимая чем.
 *
 * Остаётся то, что зависит от него и от его сети, а ядру не видно: когда
 * включаться, кто отвечает на запросы имён, что пускать мимо, нужен ли
 * IPv6, как учитывать лимитную сеть и что показать продавцу. И одна ручка из
 * маскировки — дробление приветствия: режет ли провайдер соединения по имени
 * сайта, клиент узнать не может, а без такого фильтра дробление вредно.
 *
 * Из макета намеренно нет: kill switch (это системный «постоянный VPN», и
 * туда ведёт строка), «переподключаться при смене сети» (надзор делает это
 * сам, выключать нечего), транспорта и DNS через туннель (решает ядро),
 * уведомлений по отдельности и переноса настроек — за такими строками пока
 * нет действия, а нарисованная ручка — обещание.
 */
class MoreScreen(
    private val host: AppCompatActivity,
    private val ui: ScreenMoreBinding,
    private val store: Store,
    private val theme: () -> Theme,
    /** Открыть выбор языка: он общий с первым запуском. */
    private val onLanguage: () -> Unit,
    /** Что-то из этого применяется только на следующем подключении. */
    private val onRoutesChanged: (Boolean) -> Unit,
    /** Настройка ядра, не маршрутов, — тоже со следующего подключения. */
    private val onNextConnect: () -> Unit,
    /** Человек сбросил всё: выключить туннель и начать с чистого листа. */
    private val onReset: () -> Unit,
    private val onAppearanceChanged: () -> Unit,
    private val onTheme: () -> Unit,
    /** «Диагностика»: подключиться, переподключиться, открыть «Серверы». */
    private val onConnect: () -> Unit,
    private val onReconnect: () -> Unit,
    private val onServers: () -> Unit,
) {

    enum class Section { CONN, ADVANCED, APPS, LOGS, ABOUT, DIAG }

    private var section = Section.CONN

    /** Фильтр логов: null — все уровни. */
    private var logFilter: Journal.Level? = null

    private val apps = AppsAdapter()
    private val iconCache = android.util.LruCache<String, android.graphics.drawable.Drawable>(64)
    private val dp = host.resources.displayMetrics.density
    private val advanced by lazy {
        AdvancedVpnSettings(host, ui.sectionAdvanced, ui.advancedRows, store, theme, onNextConnect)
    }
    private val appearance by lazy {
        AppearanceSettings(host, ui.appearanceRows, store, theme, onAppearanceChanged)
    }
    private val diagnostics by lazy {
        DiagnosticsScreen(
            host, ui.sectionDiag, ui.diagRows, store, theme, version,
            DiagnosticsScreen.Actions(
                connect = onConnect,
                reconnect = onReconnect,
                servers = onServers,
                chooseDns = { chooseDns() },
            ),
        )
    }

    private val version: String = try {
        host.packageManager.getPackageInfo(host.packageName, 0).versionName.orEmpty()
    } catch (_: PackageManager.NameNotFoundException) {
        ""
    }

    /** Строка поиска по приложениям; пусто — все. */
    private var query = ""

    init {
        ui.moreBack.setOnClickListener { show(Section.CONN) }
        ui.rowApps.setOnClickListener { show(Section.APPS) }
        ui.rowLogs.setOnClickListener { show(Section.LOGS) }
        ui.rowDiag.setOnClickListener { show(Section.DIAG) }
        ui.rowTheme.setOnClickListener { onTheme() }

        wireConnection()
        wireApps()
        wireLogs()
    }

    /** open зовётся при каждом показе: настройки меняются и из других мест. */
    fun open() {
        show(section)
    }

    /** paint перекрашивает то, что красится кодом: таблетки и строки логов. */
    fun paint() {
        val t = theme()
        paintTitleSize()
        ui.appearanceValue.text = AppearanceProfiles.selected(store)?.let { host.getString(it.name) }
            ?: host.getString(R.string.theme_look_custom)
        paintModes()
        renderConnection()
        if (section == Section.ADVANCED) advanced.render()
        if (section == Section.DIAG) diagnostics.render()
        ui.searchBox.background = Paint.rounded(t.surf, minOf(t.r, 12), dp)
        ui.appList.background = Paint.rounded(t.surf, t.r, dp)
        ui.appsSpinner.indeterminateTintList = ColorStateList.valueOf(t.acc)
        if (section == Section.LOGS) {
            renderLogs()
        }
        apps.notifyDataSetChanged()
    }

    fun openLogs() { section = Section.LOGS }

    /** Экран настроек ушёл с глаз: «Диагностика» обрывает то, что ещё идёт. */
    fun hidden() {
        if (section == Section.DIAG) diagnostics.close()
    }

    /** Подсказка внутри экрана вместо системного Toast поверх навигации. */
    fun showPendingConnectionChange() {
        val state = MarviaState.state.value
        ui.pendingConnectionNotice.isVisible = state is TunnelState.On || state is TunnelState.Connecting
    }

    fun clearPendingConnectionChange() {
        ui.pendingConnectionNotice.isVisible = false
    }

    /** back уводит с подэкрана на страницу настроек; false — мы уже на ней. */
    fun back(): Boolean {
        if (section == Section.CONN) return false
        show(Section.CONN)
        return true
    }

    private fun paintTitleSize() {
        val signal = store.appearanceStyle == AppearanceStyle.SIGNAL
        ui.moreTitle.textSize = if (section == Section.CONN) {
            if (signal) 32f else 26f
        } else if (signal) 24f else 21f
    }

    private fun show(next: Section) {
        // Уходя из «Диагностики», обрываем то, что там ещё идёт: замер
        // скорости не должен докачивать мегабайты продавца в пустоту.
        if (section == Section.DIAG && next != Section.DIAG) diagnostics.close()
        section = next
        val state = MarviaState.state.value
        if (state !is TunnelState.On && state !is TunnelState.Connecting) ui.pendingConnectionNotice.isVisible = false
        ui.sectionConn.isVisible = next == Section.CONN
        ui.sectionAdvanced.isVisible = next == Section.ADVANCED
        ui.sectionApps.isVisible = next == Section.APPS
        ui.sectionLogs.isVisible = next == Section.LOGS
        ui.sectionAbout.isVisible = next == Section.ABOUT
        ui.sectionDiag.isVisible = next == Section.DIAG

        ui.moreBack.isVisible = next != Section.CONN
        // Подэкран — заголовок помельче: «Прокси по приложениям» в 26 не влезает.
        paintTitleSize()
        ui.moreTitle.setText(
            when (next) {
                Section.CONN -> R.string.more_title
                Section.ADVANCED -> R.string.advanced_title
                Section.APPS -> R.string.more_apps_card
                Section.LOGS -> R.string.more_logs_row
                Section.ABOUT -> R.string.settings_about
                Section.DIAG -> R.string.diag_title
            },
        )
        ui.moreSub.text = when (next) {
            Section.CONN -> host.getString(R.string.more_sub, version)
            Section.ADVANCED -> host.getString(R.string.advanced_subtitle)
            Section.APPS -> appsSummary()
            Section.LOGS -> logsSub()
            Section.ABOUT -> host.getString(R.string.about_intro)
            Section.DIAG -> host.getString(R.string.diag_sub)
        }

        when (next) {
            Section.CONN -> renderConnection()
            Section.ADVANCED -> advanced.render()
            Section.APPS -> openApps()
            Section.LOGS -> renderLogs()
            Section.ABOUT -> ui.aboutVersion.text = host.getString(R.string.about_version, version)
            Section.DIAG -> diagnostics.open()
        }
    }

    /** pill красит таблетку: активная — акцентом, остальные — второй поверхностью. */
    private fun pill(view: TextView, t: Theme, on: Boolean) {
        view.background = Paint.rounded(if (on) t.acc else t.surf2, minOf(t.r, 14), dp)
        view.setTextColor(if (on) t.accFg else t.dim)
    }

    // ---------------------------------------------------------- соединение

    private fun wireConnection() {
        ui.rowAutostart.setOnClickListener {
            store.autoStart = !store.autoStart
            renderConnection()
            Toast.makeText(
                host,
                if (store.autoStart) {
                    R.string.settings_autostart_saved
                } else {
                    R.string.settings_autostart_off_saved
                },
                Toast.LENGTH_SHORT,
            ).show()
        }

        ui.rowAlwaysOn.setOnClickListener { openAlwaysOn() }
        ui.rowAdvanced.setOnClickListener { show(Section.ADVANCED) }
        ui.rowDns.setOnClickListener { chooseDns() }

        ui.rowLan.setOnClickListener {
            store.lanOutside = !store.lanOutside
            renderConnection()
            onRoutesChanged(false)
        }

        ui.rowRussian.setOnClickListener {
            store.bypassRussian = !store.bypassRussian
            renderConnection()
            onRoutesChanged(store.bypassRussian)
        }

        // Обе настройки ядро читает при подключении: работающему туннелю
        // они не передаются, и onNextConnect говорит об этом человеку.
        ui.rowIpv6.setOnClickListener {
            store.ipv6 = !store.ipv6
            renderConnection()
            onNextConnect()
        }
        ui.rowFragment.setOnClickListener {
            store.fragment = !store.fragment
            renderConnection()
            onNextConnect()
        }
        ui.rowDnsSecure.setOnClickListener {
            store.dnsSecure = !store.dnsSecure
            renderConnection()
            onNextConnect()
        }

        ui.rowLanguage.setOnClickListener { onLanguage() }
        ui.rowAbout.setOnClickListener { show(Section.ABOUT) }
        ui.aboutPrivacy.setOnClickListener {
            try {
                host.startActivity(Intent(Intent.ACTION_VIEW, Uri.parse("https://github.com/jytt8u/marvia/blob/main/docs/privacy.md")))
            } catch (_: ActivityNotFoundException) {
                Toast.makeText(host, R.string.about_privacy_unavailable, Toast.LENGTH_LONG).show()
            }
        }
        ui.rowReset.setOnClickListener { askReset() }

        // Исключать маршруты умеет только Android 13 и новее. На старых
        // строк нет вовсе: переключатель обещал бы то, чего система не
        // сделает, а человек считал бы, что госуслуги уже починены.
        val routes = RuRoutes.supported()
        ui.rowLan.isVisible = routes
        ui.dividerLan.isVisible = routes
        ui.rowRussian.isVisible = routes
    }

    /**
     * askReset — «сбросить всё»: подписки, тема, настройки. Спрашиваем один
     * раз и прямо: после этого ссылку доступа придётся добавлять заново, а
     * её у человека может уже не быть под рукой.
     */
    private fun askReset() {
        ThemedDialogs.builder(host, theme())
            .setMessage(R.string.more_reset_ask)
            .setPositiveButton(R.string.more_reset_yes) { _, _ -> onReset() }
            .setNegativeButton(android.R.string.cancel, null)
            .show()
    }

    private fun renderConnection() {
        appearance.render()
        ui.switchAutostart.isChecked = store.autoStart
        ui.switchLan.isChecked = store.lanOutside
        ui.switchRussian.isChecked = store.bypassRussian
        ui.switchIpv6.isChecked = store.ipv6
        ui.switchFragment.isChecked = store.fragment
        // Под переключателем — правда о списке: включённый тумблер без
        // скачанных подсетей ничего не уводит, и человек должен это видеть,
        // а не гадать, почему Яндекс всё ещё идёт через туннель.
        val routes = if (store.bypassRussian) RuRoutes.count(host) else -1
        ui.russianSub.text = when {
            routes < 0 -> host.getString(R.string.settings_russian_sub)
            // Причина — та, что сказало ядро: «панель недоступна» на всё
            // подряд прятало и неверную ссылку, и отказ панели.
            routes == 0 && RuRoutes.lastError.isNotEmpty() -> host.getString(R.string.settings_russian_failed, RuRoutes.lastError)
            routes == 0 -> host.getString(R.string.settings_russian_none)
            else -> host.getString(R.string.settings_russian_count, routes)
        }
        ui.dnsValue.text = dnsName(store.dns)
        ui.switchDnsSecure.isChecked = store.dnsSecure
        // Свой адрес шифровать нечем, и переключатель не должен делать вид,
        // что шифрует: под ним — правда о том, что будет.
        ui.dnsSecureSub.setText(
            if (store.dnsSecure && !store.dnsSecureNow) R.string.conn_dns_secure_own else R.string.conn_dns_secure_sub,
        )
        ui.languageValue.text = host.getString(LanguageScreen.label(store.language))

        ui.appsSummary.text = appsSummary()
        ui.aboutSub.text = host.getString(R.string.about_sub, version)
    }

    /**
     * chooseDns — кто отвечает на запросы имён: известные резолверы и свой
     * адрес последней строкой. Свой, если он уже стоит, показан отмеченным
     * — иначе человек не увидел бы в списке того, что выбрано.
     */
    private fun chooseDns() {
        val choices = Store.DNS_CHOICES
        val labels = choices.map { dnsName(it) + "  ·  " + it }.toMutableList()
        var current = choices.indexOf(store.dns)
        val custom = store.dns.takeIf { it !in choices }
        if (custom != null) {
            labels += host.getString(R.string.dns_custom_current, custom)
            current = labels.lastIndex
        }
        labels += host.getString(R.string.dns_custom)

        ChoiceSheet.show(host, theme(), host.getString(R.string.conn_dns), labels, current) { which ->
            when {
                which < choices.size -> {
                    store.dns = choices[which]
                    renderConnection()
                    onRoutesChanged(false)
                }
                which == labels.lastIndex -> askDns(custom.orEmpty())
            }
        }
    }

    /**
     * askDns — свой резолвер. Адрес проверяется до сохранения, с причиной
     * под полем: узнать про опечатку при следующем подключении значит
     * узнать о ней как «интернет не работает».
     */
    private fun askDns(was: String) {
        val t = theme()
        val field = android.widget.EditText(host).apply {
            setHint(R.string.dns_custom_hint)
            setText(was)
            setSingleLine()
            // Цифровая клавиатура с точкой: адрес — это четыре числа, и
            // буквенная раскладка только подсовывала бы опечатки.
            inputType = android.text.InputType.TYPE_CLASS_NUMBER or android.text.InputType.TYPE_NUMBER_FLAG_DECIMAL
            keyListener = android.text.method.DigitsKeyListener.getInstance("0123456789.")
            setTextColor(t.fg)
            setHintTextColor(t.dim)
            backgroundTintList = ColorStateList.valueOf(t.acc)
            minHeight = (54 * dp).toInt()
        }
        ChoiceSheet.form(host, t, host.getString(R.string.dns_custom_title), field, host.getString(R.string.dns_custom_save)) {
            val address = field.text.toString().trim()
            when (Store.checkDns(address)) {
                Store.DnsCheck.OK -> {
                    store.dns = address
                    renderConnection()
                    onRoutesChanged(false)
                    true
                }
                Store.DnsCheck.LOCAL -> {
                    field.error = host.getString(R.string.dns_custom_local)
                    false
                }
                Store.DnsCheck.BAD -> {
                    field.error = host.getString(R.string.dns_custom_bad)
                    false
                }
            }
        }
    }

    private fun dnsName(address: String): String = when (address) {
        "1.1.1.1" -> "Cloudflare"
        "8.8.8.8" -> "Google"
        "9.9.9.9" -> "Quad9"
        "94.140.14.14" -> "AdGuard"
        else -> address
    }

    /**
     * openAlwaysOn отправляет в системный «постоянный VPN».
     *
     * Он надёжнее нашего автозапуска: система поднимает туннель раньше нас,
     * переживает наше падение и умеет не пускать трафик мимо, пока туннель не
     * встал, — это и есть kill switch, только настоящий. Прятать это от
     * человека ради того, чтобы наша галочка выглядела главной, — нечестно.
     */
    private fun openAlwaysOn() {
        try {
            host.startActivity(Intent(AndroidSettings.ACTION_VPN_SETTINGS))
        } catch (_: ActivityNotFoundException) {
            Toast.makeText(host, R.string.settings_always_on_missing, Toast.LENGTH_LONG).show()
        }
    }

    /**
     * showUpdate показывает строку «есть новая версия», когда панель продавца
     * её выложила. Ссылка открывается браузером: скачивать apk лучше тем,
     * чему человек уже доверяет, а ставить — системе.
     */
    fun showUpdate(sub: TunnelState.Subscription?) {
        // Туннель выключили — подписки в состоянии нет, но строка остаётся:
        // человек увидел «есть новая» и вправе вернуться к ней позже.
        if (sub == null) return
        val version = sub.updateVersion
        val url = sub.updateUrl
        val show = version.isNotEmpty() && url.isNotEmpty()
        ui.rowUpdate.isVisible = show
        ui.rowUpdateLine.isVisible = show
        if (!show) return
        ui.updateTitle.text = host.getString(R.string.settings_update, version)
        ui.rowUpdate.setOnClickListener {
            try {
                host.startActivity(Intent(Intent.ACTION_VIEW, Uri.parse(url)))
            } catch (_: ActivityNotFoundException) {
                Toast.makeText(host, url, Toast.LENGTH_LONG).show()
            }
        }
    }

    // ---------------------------------------------------------- приложения

    private fun wireApps() {
        ui.appList.layoutManager = LinearLayoutManager(host)
        ui.appList.adapter = apps
        ui.appList.itemAnimator = null

        ui.appSearch.addTextChangedListener(object : android.text.TextWatcher {
            override fun beforeTextChanged(s: CharSequence?, a: Int, b: Int, c: Int) = Unit
            override fun onTextChanged(s: CharSequence?, a: Int, b: Int, c: Int) = Unit
            override fun afterTextChanged(s: android.text.Editable?) {
                query = s?.toString()?.trim().orEmpty()
                apps.refilter()
            }
        })
        ui.modeExclude.setOnClickListener { setMode(Store.BYPASS_EXCLUDE) }
        ui.modeInclude.setOnClickListener { setMode(Store.BYPASS_INCLUDE) }
        ui.modeOff.setOnClickListener { setMode(Store.BYPASS_OFF) }
        ui.appsSystem.setOnClickListener {
            store.showSystemApps = !store.showSystemApps
            paintModes()
            apps.refilter()
        }
        ui.rowCustom.setOnClickListener { editCustom() }
        // Блокировку без VPN и постоянный VPN включает только сама система:
        // приложению Android этого не доверяет. Ведём человека ровно туда.
        ui.rowLockdown.setOnClickListener {
            try {
                host.startActivity(Intent(AndroidSettings.ACTION_VPN_SETTINGS))
            } catch (_: ActivityNotFoundException) {
                Toast.makeText(host, R.string.lockdown_unavailable, Toast.LENGTH_LONG).show()
            }
        }
        ui.rowCustom.isVisible = RuRoutes.supported()
        paintCustom()
    }

    /** paintCustom — подпись строки своих адресов: сколько и какие первые. */
    private fun paintCustom() {
        val parsed = CustomRoutes.parse(store.customBypass)
        val names = parsed.entries.map {
            when (it) {
                is CustomRoutes.Entry.Host -> it.name
                is CustomRoutes.Entry.Net -> if (it.length == (if (it.v6) 128 else 32)) it.address else it.address + "/" + it.length
            }
        }
        ui.customSub.text = if (names.isEmpty()) host.getString(R.string.custom_bypass_none)
        else host.resources.getQuantityString(R.plurals.custom_bypass_count, names.size, names.size) +
            " · " + names.take(3).joinToString(", ") + if (names.size > 3) "…" else ""
    }

    /**
     * editCustom — поле со своими адресами. Нераспознанные строки не
     * выбрасываем молча: показываем их, иначе человек решит, что правило
     * действует.
     */
    private fun editCustom() {
        val t = theme()
        val dp = host.resources.displayMetrics.density
        val field = android.widget.EditText(host).apply {
            setText(store.customBypass)
            hint = host.getString(R.string.custom_bypass_hint)
            minLines = 4
            maxLines = 10
            gravity = Gravity.TOP or Gravity.START
            inputType = android.text.InputType.TYPE_CLASS_TEXT or android.text.InputType.TYPE_TEXT_FLAG_MULTI_LINE or
                android.text.InputType.TYPE_TEXT_FLAG_NO_SUGGESTIONS
            textSize = 14f
            setTextColor(t.fg)
            setHintTextColor(t.dim)
        }
        val note = TextView(host).apply {
            setText(R.string.custom_bypass_note)
            textSize = 12f
            setTextColor(t.dim)
            setPadding(0, (8 * dp).toInt(), 0, 0)
        }
        val box = LinearLayout(host).apply {
            orientation = LinearLayout.VERTICAL
            setPadding((20 * dp).toInt(), (8 * dp).toInt(), (20 * dp).toInt(), 0)
            addView(field)
            addView(note)
        }
        ThemedDialogs.builder(host, t)
            .setTitle(R.string.custom_bypass)
            .setView(box)
            .setPositiveButton(R.string.custom_bypass_save) { _, _ ->
                val parsed = CustomRoutes.parse(field.text.toString())
                store.customBypass = field.text.toString().trim()
                paintCustom()
                // Нераспознанное и не влезшее в предел — разными словами: человеку
                // важно, исправить строку или просто убрать лишнее.
                val said = listOfNotNull(
                    parsed.bad.takeIf { it.isNotEmpty() }?.let { host.getString(R.string.custom_bypass_bad, it.take(3).joinToString(", ")) },
                    parsed.skipped.takeIf { it > 0 }?.let { host.getString(R.string.custom_bypass_skipped, CustomRoutes.MAX, it) },
                )
                if (said.isNotEmpty()) Toast.makeText(host, said.joinToString("\n"), Toast.LENGTH_LONG).show()
                onRoutesChanged(false)
            }
            .setNegativeButton(android.R.string.cancel, null)
            .show()
    }

    private fun setMode(mode: String) {
        if (store.bypassMode == mode) {
            return
        }
        store.bypassMode = mode
        afterAppsChanged()
    }

    private fun afterAppsChanged() {
        paintModes()
        ui.moreSub.text = appsSummary()
        ui.appsSummary.text = appsSummary()
        onRoutesChanged(false)
    }

    private fun paintModes() {
        val t = theme()
        val mode = store.bypassMode
        pill(ui.modeExclude, t, mode == Store.BYPASS_EXCLUDE)
        pill(ui.modeInclude, t, mode == Store.BYPASS_INCLUDE)
        pill(ui.modeOff, t, mode == Store.BYPASS_OFF)
        pill(ui.appsSystem, t, store.showSystemApps)
        ui.modeNote.setText(
            when (mode) {
                Store.BYPASS_INCLUDE -> R.string.apps_note_include
                Store.BYPASS_OFF -> R.string.apps_note_off
                else -> R.string.apps_note_exclude
            },
        )
    }

    /**
     * appsSummary — подпись: сколько и в какую сторону, с именами первых
     * трёх, когда они уже известны. Имена берём из прочитанного списка, а
     * читать его ради подписи не идём: это секунда на каждое открытие.
     */
    private fun appsSummary(): String {
        val n = store.bypassed.size
        val names = apps.all.filter { it.pkg in store.bypassed }.map { it.label }
        return when (store.bypassMode) {
            Store.BYPASS_OFF -> host.getString(R.string.apps_summary_off)
            Store.BYPASS_INCLUDE ->
                if (n == 0) host.getString(R.string.apps_summary_include_none)
                else host.resources.getQuantityString(R.plurals.apps_summary_include, n, n)
            else -> when {
                n == 0 -> host.getString(R.string.bypass_none)
                names.isNotEmpty() -> host.getString(R.string.apps_summary_names, n, names.take(3).joinToString(", ") + if (names.size > 3) "…" else "")
                else -> host.resources.getQuantityString(R.plurals.apps_summary_exclude, n, n)
            }
        }
    }

    /**
     * openApps читает список установленного в фоне и показывает отмеченные
     * сверху: человек открывает этот экран второй раз, чтобы посмотреть или
     * снять то, что уже отметил, а не искать заново.
     */
    private fun openApps() {
        paintModes()
        if (apps.entries.isNotEmpty()) {
            apps.notifyDataSetChanged()
            return
        }
        ui.appsLoading.isVisible = true
        ui.appsSpinner.indeterminateTintList = android.content.res.ColorStateList.valueOf(theme().acc)
        host.lifecycleScope.launch {
            val entries = withContext(Dispatchers.IO) { Bypass.installed(host) }
            val chosen = store.bypassed
            apps.all = entries.sortedWith(
                compareByDescending<Bypass.Entry> { it.pkg in chosen }.thenBy { it.label.lowercase() },
            )
            apps.refilter()
            ui.appsLoading.isVisible = false
            ui.appsSummary.text = appsSummary()
            ui.moreSub.text = appsSummary()
        }
    }

    private inner class AppsAdapter : RecyclerView.Adapter<AppHolder>() {
        /** all — всё установленное; entries — то, что прошло через поиск. */
        var all: List<Bypass.Entry> = emptyList()
        var entries: List<Bypass.Entry> = emptyList()

        fun refilter() {
            val q = query.lowercase()
            // Системные — только по просьбе; выбранные видны всегда, иначе
            // правило действовало бы на невидимое приложение.
            val chosen = store.bypassed
            val visible = if (store.showSystemApps) all else all.filter { !it.system || it.pkg in chosen }
            entries = if (q.isEmpty()) visible else visible.filter { it.label.lowercase().contains(q) || it.pkg.lowercase().contains(q) }
            notifyDataSetChanged()
        }

        override fun getItemCount() = entries.size

        override fun onCreateViewHolder(parent: ViewGroup, viewType: Int): AppHolder {
            val binding = ItemAppBinding.inflate(LayoutInflater.from(parent.context), parent, false)
            return AppHolder(binding)
        }

        override fun onBindViewHolder(holder: AppHolder, position: Int) {
            holder.bind(entries[position])
        }
    }

    private inner class AppHolder(private val b: ItemAppBinding) : RecyclerView.ViewHolder(b.root) {
        fun bind(entry: Bypass.Entry) {
            val t = theme()
            Paint.apply(b.root, t)

            b.appName.text = entry.label
            b.appPackage.text = entry.pkg
            b.appIcon.tag = entry.pkg
            val cachedIcon = iconCache.get(entry.pkg)
            b.appIcon.setImageDrawable(cachedIcon ?: host.packageManager.defaultActivityIcon)
            if (cachedIcon == null) host.lifecycleScope.launch {
                val icon = withContext(Dispatchers.IO) {
                    runCatching { host.packageManager.getApplicationIcon(entry.pkg) }.getOrNull()
                }
                if (icon != null) iconCache.put(entry.pkg, icon)
                if (b.appIcon.tag == entry.pkg && icon != null) b.appIcon.setImageDrawable(icon)
            }

            val on = entry.pkg in store.bypassed
            b.appSwitch.isChecked = on
            // Список остаётся и в выключенном режиме, но не применяется —
            // переключатели гаснут, чтобы это было видно.
            val enabled = store.bypassMode != Store.BYPASS_OFF
            b.root.isEnabled = enabled
            b.root.alpha = if (enabled) 1f else 0.5f

            b.root.setOnClickListener {
                val chosen = store.bypassed.toMutableSet()
                if (!chosen.remove(entry.pkg)) {
                    chosen.add(entry.pkg)
                }
                store.bypassed = chosen
                b.appSwitch.isChecked = entry.pkg in chosen
                afterAppsChanged()
            }
        }
    }

    // ---------------------------------------------------------------- логи

    private fun wireLogs() {
        ui.logAll.setOnClickListener { filterLogs(null) }
        ui.logInfo.setOnClickListener { filterLogs(Journal.Level.INFO) }
        ui.logWarn.setOnClickListener { filterLogs(Journal.Level.WARN) }
        ui.logErr.setOnClickListener { filterLogs(Journal.Level.ERROR) }

        ui.logsCopy.setOnClickListener {
            val text = visibleLogs().joinToString("\n") { it.time + "  " + it.text }
            if (text.isEmpty()) {
                Toast.makeText(host, R.string.logs_empty, Toast.LENGTH_SHORT).show()
                return@setOnClickListener
            }
            host.getSystemService(ClipboardManager::class.java)
                ?.setPrimaryClip(ClipData.newPlainText("marvia", text))
            Toast.makeText(host, R.string.logs_copied, Toast.LENGTH_SHORT).show()
        }

        // Одна кнопка «отправить» заменяет переписку «а что у тебя написано
        // на экране» — самую бесполезную часть любой поддержки. Уходит
        // отчёт целиком, с моделью телефона, а не отфильтрованный кусок.
        ui.logsSend.setOnClickListener {
            val send = Intent(Intent.ACTION_SEND).apply {
                type = "text/plain"
                putExtra(Intent.EXTRA_TEXT, Journal.report(host))
            }
            host.startActivity(Intent.createChooser(send, host.getString(R.string.journal_send)))
        }
    }

    private fun logsSub(): String {
        val n = Journal.entries().size
        return if (n == 0) host.getString(R.string.more_logs_sub_empty) else host.resources.getQuantityString(R.plurals.more_logs_sub, n, n)
    }

    private fun filterLogs(level: Journal.Level?) {
        logFilter = level
        renderLogs()
    }

    private fun visibleLogs(): List<Journal.Entry> {
        val all = Journal.entries()
        val level = logFilter ?: return all
        return all.filter { it.level == level }
    }

    /**
     * renderLogs собирает строки вьюхами, а не одним TextView: у каждой
     * строки своя метка уровня своим цветом, и предупреждение должно
     * читаться как предупреждение, а не как ещё одна серая строка.
     */
    private fun renderLogs() {
        val t = theme()
        pill(ui.logAll, t, logFilter == null)
        pill(ui.logInfo, t, logFilter == Journal.Level.INFO)
        pill(ui.logWarn, t, logFilter == Journal.Level.WARN)
        pill(ui.logErr, t, logFilter == Journal.Level.ERROR)

        val rows = ui.logRows
        rows.removeAllViews()
        val shown = visibleLogs()
        ui.logsCount.text = host.resources.getQuantityString(R.plurals.logs_count, shown.size, shown.size)
        ui.moreSub.text = logsSub()

        if (shown.isEmpty()) {
            rows.addView(
                TextView(host).apply {
                    setText(R.string.logs_empty)
                    setTextColor(t.dim)
                    textSize = 13f
                },
            )
            return
        }

        for ((i, e) in shown.withIndex()) {
            rows.addView(logRow(t, e), LinearLayout.LayoutParams(MATCH, WRAP).apply {
                if (i > 0) topMargin = (9 * dp).toInt()
            })
        }
        ui.logsScroll.post { ui.logsScroll.fullScroll(View.FOCUS_DOWN) }
    }

    private fun logRow(t: Theme, e: Journal.Entry): View {
        val row = LinearLayout(host).apply { orientation = LinearLayout.HORIZONTAL }

        row.addView(
            TextView(host).apply {
                text = e.time
                typeface = Typeface.MONOSPACE
                textSize = 10f
                setTextColor(t.dim)
            },
            LinearLayout.LayoutParams(WRAP, WRAP),
        )

        val (tag, color) = when (e.level) {
            Journal.Level.INFO -> "INF" to t.acc
            Journal.Level.WARN -> "WRN" to t.warn
            Journal.Level.ERROR -> "ERR" to t.fail
        }
        row.addView(
            TextView(host).apply {
                text = tag
                typeface = Typeface.MONOSPACE
                textSize = 9f
                gravity = Gravity.CENTER
                setTextColor(color)
                background = Paint.rounded(Look.withAlpha(color, 0.14), 4, dp)
                setPadding((5 * dp).toInt(), (1 * dp).toInt(), (5 * dp).toInt(), (1 * dp).toInt())
            },
            LinearLayout.LayoutParams(WRAP, WRAP).apply { marginStart = (8 * dp).toInt() },
        )

        row.addView(
            TextView(host).apply {
                text = e.text
                typeface = Typeface.MONOSPACE
                textSize = 11f
                setTextColor(t.fg)
            },
            LinearLayout.LayoutParams(0, WRAP, 1f).apply { marginStart = (8 * dp).toInt() },
        )
        return row
    }

    private companion object {
        const val MATCH = LinearLayout.LayoutParams.MATCH_PARENT
        const val WRAP = LinearLayout.LayoutParams.WRAP_CONTENT
    }
}
