package io.marvia.android

import android.content.ClipData
import android.content.ClipboardManager
import android.graphics.Typeface
import android.view.Gravity
import android.view.View
import android.widget.LinearLayout
import android.widget.ScrollView
import android.widget.TextView
import android.widget.Toast
import androidx.appcompat.app.AppCompatActivity
import androidx.lifecycle.Lifecycle
import androidx.lifecycle.lifecycleScope
import androidx.lifecycle.repeatOnLifecycle
import io.marvia.mobile.Mobile
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.delay
import kotlinx.coroutines.isActive
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext
import io.marvia.mobile.Diagnostics as CoreDiagnostics
import io.marvia.mobile.Tunnel as Core

/**
 * DiagnosticsScreen — «Диагностика» в настройках: где ты в интернете,
 * качество связи, «Проверить всё», скорость и отчёт продавцу. То же, что
 * раздел «Диагностика» в окне на компьютере.
 *
 * Человек с «не работает» видит зелёное «Подключено» и не знает, куда
 * смотреть дальше. Здесь на это есть ответ по шагам, и у плохого шага —
 * кнопка, которая что-то делает прямо отсюда. Логика шагов и вердикты — в
 * ядре (mobile/diag.go): там же они у окна на компьютере, и второй копии
 * правил в Kotlin не нужно. Отсюда — только слова и кнопки.
 *
 * Всё, что спрашивает сеть, идёт через ядро туннеля: приложение исключено
 * из собственного VPN, и его обычный HTTP показал бы настоящий адрес.
 */
class DiagnosticsScreen(
    private val host: AppCompatActivity,
    private val scroll: ScrollView,
    private val rows: LinearLayout,
    private val store: Store,
    private val theme: () -> Theme,
    private val version: String,
    private val actions: Actions,
) {
    /** Настоящие действия приложения, на которые ведут кнопки шагов. */
    class Actions(
        val connect: () -> Unit,
        val reconnect: () -> Unit,
        val servers: () -> Unit,
        val chooseDns: () -> Unit,
    )

    private val dp = host.resources.displayMetrics.density

    /** Проверка ядра; живёт, пока экран открыт. */
    private var diag: CoreDiagnostics? = null
    private var stepIds: List<String> = emptyList()

    /** Итоги шагов по порядку; null — шаг ещё идёт. */
    private val steps = LinkedHashMap<String, Diagnostics.Step?>()
    private var checking = false
    private var where: Diagnostics.Where? = null
    private var quality: Diagnostics.Quality? = null
    private var route: Diagnostics.Route? = null
    private var measuring = false
    private var speedFailed = false

    private var live: Job? = null
    private var check: Job? = null
    private var speed: Job? = null

    private lateinit var whereBox: LinearLayout
    private lateinit var qualityBox: LinearLayout
    private lateinit var checkBox: LinearLayout
    private lateinit var speedBox: LinearLayout

    fun open() {
        if (diag == null) diag = Mobile.newDiagnostics(store.accountLink, store.cacheDir(), store.tunnelSettings())
        if (stepIds.isEmpty()) stepIds = Mobile.diagSteps().split(',').filter { it.isNotEmpty() }
        render()
        watch()
    }

    /**
     * close — человек ушёл: всё, что ещё идёт, обрывается. Замер скорости не
     * должен докачивать тридцать мегабайт продавца в пустоту.
     */
    fun close() {
        live?.cancel()
        check?.cancel()
        speed?.cancel()
        diag?.cancel()
        diag = null
        checking = false
        measuring = false
        steps.entries.removeAll { it.value == null }
    }

    /**
     * watch держит «где ты» и «качество» живыми, пока экран на виду: раз в
     * пять секунд один пинг ноды — тот же служебный кадр, что шлёт служба
     * по расписанию. Без него джиттер и потери считались бы по замерам раз
     * в полминуты и ничего не говорили бы про «сейчас».
     */
    private fun watch() {
        live?.cancel()
        live = host.lifecycleScope.launch {
            host.repeatOnLifecycle(Lifecycle.State.STARTED) {
                while (isActive && scroll.isShown) {
                    val core = sync()
                    if (core == null) {
                        where = null
                        quality = null
                        route = null
                    } else {
                        val (q, r) = withContext(Dispatchers.IO) {
                            runCatching { core.ping() }
                            Diagnostics.quality(core.quality()) to Diagnostics.route(core.route())
                        }
                        quality = q
                        route = r
                        fillQuality()
                        if (where == null) {
                            val d = diag
                            where = if (d == null) null else withContext(Dispatchers.IO) { Diagnostics.where(d.where()) }
                        }
                    }
                    fillWhere()
                    fillQuality()
                    if (!checking) fillSpeed()
                    delay(LIVE_MS)
                }
            }
        }
    }

    /**
     * sync говорит ядру, поднят ли туннель. Состояние живёт в приложении
     * (MarviaState), а не в ядре: проверка без туннеля тоже идёт, и шагам
     * надо знать, «не подключено» это или «не поднялось».
     */
    private fun sync(): Core? {
        val d = diag ?: return null
        val core = MarviaState.core
        val state = when (MarviaState.state.value) {
            is TunnelState.On -> if (core == null) "connecting" else {
                val trouble = runCatching { core.trouble() }.getOrDefault("")
                when {
                    trouble.isEmpty() -> "connected"
                    // Сеть телефона меняется: туннель вот-вот переподнимется,
                    // спрашивать через него сейчас — мерить переходный миг.
                    trouble.startsWith("network-") -> "connecting"
                    else -> "stalled"
                }
            }
            TunnelState.Connecting -> "connecting"
            is TunnelState.Failed -> "failed"
            TunnelState.Off -> "off"
        }
        return if (core != null && (state == "connected" || state == "stalled")) {
            d.useTunnel(core, state)
            core
        } else {
            d.noTunnel(state)
            null
        }
    }

    private val up: Boolean get() = MarviaState.state.value is TunnelState.On && MarviaState.core != null

    fun render() {
        val position = scroll.scrollY
        rows.removeAllViews()

        group(R.string.diag_where_title)
        whereBox = card()
        group(R.string.diag_quality_title)
        qualityBox = card()
        group(R.string.diag_check_title)
        checkBox = card()
        group(R.string.diag_speed_title)
        speedBox = card()

        rows.addView(button(host.getString(R.string.diag_report), lead = false) { copyReport() },
            LinearLayout.LayoutParams(-1, px(48)).apply { topMargin = px(14) })

        fillWhere()
        fillQuality()
        fillCheck()
        fillSpeed()
        Paint.apply(rows, theme())
        scroll.post { scroll.scrollTo(0, position) }
    }

    // --------------------------------------------------------- где ты

    private fun fillWhere() {
        if (!::whereBox.isInitialized) return
        val box = whereBox
        box.removeAllViews()
        val state = MarviaState.state.value as? TunnelState.On
        if (!up || state == null) {
            box.addView(dim(host.getString(R.string.diag_need_up)))
        } else {
            val w = where
            if (w != null && w.country.isNotEmpty()) {
                val name = Diagnostics.countryName(host, w.country)
                box.addView(TextView(host).apply {
                    text = listOf(Flags.of(w.country), name).filter { it.isNotEmpty() }.joinToString("  ")
                    textSize = 18f
                    setTypeface(typeface, Typeface.BOLD)
                    tag = "fg"
                }, LinearLayout.LayoutParams(-1, -2).apply { bottomMargin = px(4) })
            }
            box.addView(pair(R.string.diag_ip, w?.ip ?: host.getString(R.string.diag_quality_wait)))
            box.addView(pair(R.string.diag_server, state.node))
        }
        Paint.apply(box, theme())
    }

    // ------------------------------------------------------- качество

    private fun fillQuality() {
        if (!::qualityBox.isInitialized) return
        val box = qualityBox
        val t = theme()
        box.removeAllViews()
        if (!up) {
            box.addView(dim(host.getString(R.string.diag_need_up)))
            Paint.apply(box, t)
            return
        }
        val q = quality
        if (q == null) {
            box.addView(pair(R.string.diag_ping, host.getString(R.string.diag_quality_wait)))
        } else {
            box.addView(pair(R.string.diag_ping, host.getString(R.string.node_ms, q.pingMs), when {
                q.pingMs in 1L..120L -> t.acc
                q.pingMs in 121L..300L -> t.warn
                else -> t.fail
            }))
            box.addView(pair(R.string.diag_jitter, host.getString(R.string.node_ms, q.jitterMs)))
            box.addView(pair(R.string.diag_loss, host.getString(R.string.diag_loss_value, q.lossPct), when {
                q.lossPct == 0 -> null
                q.lossPct < 20 -> t.warn
                else -> t.fail
            }))
        }
        route?.let { r ->
            box.addView(pair(R.string.diag_protocol, r.protocol.uppercase()))
            val transport = listOf(r.transport, r.security).filter { it.isNotEmpty() && it != "none" }.joinToString(" · ") { it.uppercase() }
            if (transport.isNotEmpty()) box.addView(pair(R.string.diag_transport, transport))
            if (r.sni.isNotEmpty()) {
                box.addView(pair(R.string.diag_sni, if (r.spareSni > 0) host.getString(R.string.diag_spare, r.sni, r.spareSni) else r.sni))
            }
            if (r.fingerprint.isNotEmpty()) box.addView(pair(R.string.diag_fp, r.fingerprint))
        }
        Paint.apply(box, t)
    }

    // -------------------------------------------------- проверить всё

    private fun runCheck() {
        val d = diag ?: return
        if (checking) return
        checking = true
        steps.clear()
        for (id in stepIds) steps[id] = null
        fillCheck()
        check = host.lifecycleScope.launch {
            for (id in stepIds) {
                sync()
                val raw = withContext(Dispatchers.IO) { d.step(id) }
                if (!isActive) return@launch
                steps[id] = Diagnostics.step(raw) ?: Diagnostics.Step(id, "off")
                fillCheck()
            }
            checking = false
            fillCheck()
        }
    }

    private fun fillCheck() {
        if (!::checkBox.isInitialized) return
        val box = checkBox
        val t = theme()
        box.removeAllViews()

        val done = steps.values.filterNotNull()
        val troubled = done.count { it.st == "warn" || it.st == "bad" }
        if (done.isNotEmpty() && !checking) {
            box.addView(TextView(host).apply {
                text = if (troubled == 0) host.getString(R.string.diag_all_ok) else host.getString(R.string.diag_found, troubled)
                textSize = 15f
                setTypeface(typeface, Typeface.BOLD)
                setTextColor(if (troubled == 0) t.acc else t.warn)
            }, LinearLayout.LayoutParams(-1, -2).apply { bottomMargin = px(6) })
        }

        for ((id, s) in steps) box.addView(stepRow(id, s, t))

        val label = when {
            checking -> R.string.diag_running
            steps.isEmpty() -> R.string.diag_run
            else -> R.string.diag_again
        }
        box.addView(button(host.getString(label), lead = true, enabled = !checking) { runCheck() },
            LinearLayout.LayoutParams(-1, px(44)).apply { topMargin = px(if (steps.isEmpty()) 0 else 10) })
        Paint.apply(box, t)
    }

    private fun stepRow(id: String, s: Diagnostics.Step?, t: Theme): View {
        val row = LinearLayout(host).apply {
            orientation = LinearLayout.VERTICAL
            setPadding(0, px(7), 0, px(7))
        }
        val line = LinearLayout(host).apply { orientation = LinearLayout.HORIZONTAL; gravity = Gravity.CENTER_VERTICAL }
        line.addView(TextView(host).apply {
            text = if (s == null) "…" else Diagnostics.glyph(s.st)
            textSize = 15f
            setTypeface(typeface, Typeface.BOLD)
            gravity = Gravity.CENTER
            setTextColor(when (s?.st) {
                "ok" -> t.acc
                "warn" -> t.warn
                "bad" -> t.fail
                else -> t.dim
            })
        }, LinearLayout.LayoutParams(px(22), -2))
        line.addView(TextView(host).apply {
            text = Diagnostics.title(id)?.let(host::getString) ?: id
            textSize = 14.5f
            tag = "fg"
        }, LinearLayout.LayoutParams(0, -2, 1f).apply { marginStart = px(6) })
        if (s != null) {
            val value = Diagnostics.value(host, s)
            if (value.isNotEmpty()) line.addView(TextView(host).apply {
                text = value
                textSize = 13f
                gravity = Gravity.END
                tag = "dim"
            }, LinearLayout.LayoutParams(-2, -2).apply { marginStart = px(8) })
        }
        row.addView(line)
        if (s == null) return row

        Diagnostics.say(s)?.let {
            row.addView(TextView(host).apply {
                setText(it)
                textSize = 12.5f
                tag = "dim"
            }, LinearLayout.LayoutParams(-1, -2).apply { marginStart = px(28); topMargin = px(2) })
        }
        // Кнопки — только те, за которыми у приложения есть дело; код, о
        // котором приложение не знает, кнопки не получает.
        val acts = s.act.filter { Diagnostics.act(it) != null }
        if (acts.isNotEmpty()) {
            val strip = LinearLayout(host).apply { orientation = LinearLayout.HORIZONTAL }
            acts.forEachIndexed { i, code ->
                strip.addView(TextView(host).apply {
                    setText(Diagnostics.act(code)!!)
                    textSize = 12.5f
                    isClickable = true
                    isFocusable = true
                    setPadding(px(12), px(7), px(12), px(7))
                    tag = if (i == 0) "btn" else "chip"
                    setOnClickListener { perform(code) }
                }, LinearLayout.LayoutParams(-2, -2).apply { if (i > 0) marginStart = px(7) })
            }
            row.addView(strip, LinearLayout.LayoutParams(-2, -2).apply { marginStart = px(28); topMargin = px(7) })
        }
        return row
    }

    /**
     * perform — кнопка плохого шага. Каждая делает то, что обещает: ведёт на
     * «Серверы», открывает выбор DNS, включает дробление или поднимает
     * туннель заново. Итоги прежней проверки после этого стираются: они уже
     * про другое подключение.
     */
    private fun perform(code: String) {
        when (code) {
            "connect" -> actions.connect()
            "reconnect" -> actions.reconnect()
            "nodes" -> actions.servers()
            "fragment" -> {
                // Дробление ядро читает при подключении, поэтому сразу и
                // переподключаемся: иначе кнопка ничего бы не изменила.
                store.fragment = true
                actions.reconnect()
            }
            "dns" -> actions.chooseDns()
            else -> return
        }
        if (code != "nodes" && code != "dns") {
            check?.cancel()
            checking = false
            steps.clear()
            where = null
            fillCheck()
        }
    }

    // --------------------------------------------------------- скорость

    private fun runSpeed() {
        val d = diag ?: return
        if (measuring || !up) return
        measuring = true
        speedFailed = false
        fillSpeed()
        speed = host.lifecycleScope.launch {
            val node = (MarviaState.state.value as? TunnelState.On)?.node.orEmpty()
            val mbps = Diagnostics.speed(withContext(Dispatchers.IO) { d.speed() })
            if (!isActive) return@launch
            measuring = false
            if (mbps == null) {
                speedFailed = true
            } else {
                store.speedHistory = Diagnostics.SpeedHistory.add(
                    store.speedHistory, Diagnostics.SpeedHistory.Record(System.currentTimeMillis(), mbps, node),
                )
            }
            fillSpeed()
        }
    }

    private fun fillSpeed() {
        if (!::speedBox.isInitialized) return
        val box = speedBox
        val t = theme()
        box.removeAllViews()
        val history = store.speedHistory
        val last = history.firstOrNull()
        if (last != null) {
            box.addView(TextView(host).apply {
                text = Diagnostics.mbps(host, last.mbps)
                textSize = 22f
                setTypeface(typeface, Typeface.BOLD)
                tag = "fg"
            })
            box.addView(dim(listOf(Diagnostics.time(last.at), last.node).filter { it.isNotEmpty() }.joinToString(" · ")))
        }
        if (speedFailed) box.addView(TextView(host).apply {
            setText(R.string.diag_speed_failed)
            textSize = 12.5f
            tag = "fail"
        })
        for (r in history.drop(1)) {
            box.addView(pair(listOf(Diagnostics.time(r.at), r.node).filter { it.isNotEmpty() }.joinToString(" · "), Diagnostics.mbps(host, r.mbps)))
        }
        // Замер идёт только через туннель. Без него кнопки нет: нажать её
        // было бы нечем, а нарисованная кнопка — обещание.
        if (up) {
            box.addView(button(host.getString(if (measuring) R.string.diag_speed_running else R.string.diag_speed_run), lead = true, enabled = !measuring) { runSpeed() },
                LinearLayout.LayoutParams(-1, px(44)).apply { if (box.childCount > 0) topMargin = px(10) })
        } else {
            box.addView(dim(host.getString(R.string.diag_need_up)), LinearLayout.LayoutParams(-1, -2).apply { if (box.childCount > 0) topMargin = px(6) })
        }
        Paint.apply(box, t)
    }

    // ------------------------------------------------------------ отчёт

    private fun copyReport() {
        val state = MarviaState.state.value as? TunnelState.On
        val snapshot = Diagnostics.Snapshot(
            connected = state != null,
            server = state?.node.orEmpty(),
            where = where,
            quality = quality,
            route = route,
            steps = steps.values.filterNotNull(),
            speed = store.speedHistory.firstOrNull(),
        )
        val text = Diagnostics.report(host, snapshot, Journal.report(host), version) { Mobile.redact(it) }
        host.getSystemService(ClipboardManager::class.java)?.setPrimaryClip(ClipData.newPlainText("marvia", text))
        Toast.makeText(host, R.string.diag_report_copied, Toast.LENGTH_SHORT).show()
    }

    // ------------------------------------------------------------ вьюхи

    private fun group(label: Int) {
        rows.addView(TextView(host).apply {
            setText(label)
            textSize = 10f
            letterSpacing = 0.16f
            tag = "dim"
            setPadding(px(5), px(18), 0, px(8))
        })
    }

    private fun card(): LinearLayout {
        val card = LinearLayout(host).apply {
            orientation = LinearLayout.VERTICAL
            tag = "card"
            setPadding(px(15), px(12), px(15), px(12))
        }
        rows.addView(card, LinearLayout.LayoutParams(-1, -2))
        return card
    }

    private fun pair(label: Int, value: String, color: Int? = null): View = pair(host.getString(label), value, color)

    private fun pair(label: String, value: String, color: Int? = null): View {
        val line = LinearLayout(host).apply {
            orientation = LinearLayout.HORIZONTAL
            gravity = Gravity.CENTER_VERTICAL
            setPadding(0, px(5), 0, px(5))
        }
        line.addView(TextView(host).apply {
            text = label
            textSize = 14f
            tag = "dim"
        }, LinearLayout.LayoutParams(0, -2, 1f))
        line.addView(TextView(host).apply {
            text = value
            textSize = 14f
            gravity = Gravity.END
            if (color == null) tag = "fg" else setTextColor(color)
        }, LinearLayout.LayoutParams(-2, -2).apply { marginStart = px(12) })
        return line
    }

    private fun dim(text: String) = TextView(host).apply {
        this.text = text
        textSize = 12.5f
        tag = "dim"
    }

    private fun button(label: String, lead: Boolean, enabled: Boolean = true, onClick: () -> Unit) = TextView(host).apply {
        text = label
        textSize = 14f
        gravity = Gravity.CENTER
        isClickable = enabled
        isFocusable = enabled
        isEnabled = enabled
        alpha = if (enabled) 1f else 0.6f
        setTypeface(typeface, Typeface.BOLD)
        tag = if (lead) "btn" else "chip"
        setOnClickListener { onClick() }
    }

    private fun px(value: Int) = (value * dp).toInt()

    private companion object {
        /** Раз в сколько обновлять «качество», пока экран на виду. */
        const val LIVE_MS = 5_000L
    }
}
