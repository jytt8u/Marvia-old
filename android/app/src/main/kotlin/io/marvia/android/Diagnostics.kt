package io.marvia.android

import android.content.Context
import android.os.Build
import org.json.JSONObject
import java.text.SimpleDateFormat
import java.util.Date
import java.util.Locale

/**
 * Diagnostics — то, что экран «Диагностика» знает, без самого экрана:
 * разбор ответов ядра, слова под коды, история замеров и отчёт продавцу.
 *
 * Отдельно от вьюх ради тестов: здесь нет ни ядра, ни разметки, и
 * обещания — «у каждого шага есть слова», «в отчёте нет адреса» —
 * проверяются без телефона.
 */
object Diagnostics {

    /** Шаг «Проверить всё» так, как его отдало ядро (mobile/diag.go). */
    data class Step(
        val id: String,
        val st: String,
        val ms: Long = 0,
        val n: Int = 0,
        val of: Int = 0,
        val ip: String = "",
        val country: String = "",
        val act: List<String> = emptyList(),
    )

    /** Качество связи; samples == 0 — ядро ещё не мерило. */
    data class Quality(val pingMs: Long, val jitterMs: Long, val lossPct: Int, val samples: Int)

    /** Устройство соединения — только то, что знает ядро. */
    data class Route(
        val protocol: String,
        val transport: String,
        val security: String,
        val sni: String,
        val spareSni: Int,
        val fingerprint: String,
    )

    fun step(json: String): Step? = runCatching {
        val o = JSONObject(json)
        val acts = o.optJSONArray("act")
        Step(
            id = o.getString("id"),
            st = o.getString("st"),
            ms = o.optLong("ms"),
            n = o.optInt("n"),
            of = o.optInt("of"),
            ip = o.optString("ip"),
            country = o.optString("country"),
            act = if (acts == null) emptyList() else List(acts.length()) { acts.getString(it) },
        )
    }.getOrNull()

    fun quality(json: String): Quality? = runCatching {
        val o = JSONObject(json)
        Quality(o.optLong("ping_ms"), o.optLong("jitter_ms"), o.optInt("loss_pct"), o.optInt("samples"))
    }.getOrNull()?.takeIf { it.samples > 0 }

    fun route(json: String): Route? = runCatching {
        val o = JSONObject(json)
        Route(
            o.optString("protocol"), o.optString("transport"), o.optString("security"),
            o.optString("sni"), o.optInt("spare_sni"), o.optString("fingerprint"),
        )
    }.getOrNull()?.takeIf { it.protocol.isNotEmpty() }

    /** Адрес и страна выхода; пусто — ядро не узнало или туннеля нет. */
    data class Where(val ip: String, val country: String)

    fun where(json: String): Where? = runCatching {
        val o = JSONObject(json)
        Where(o.optString("ip"), o.optString("country"))
    }.getOrNull()?.takeIf { it.ip.isNotEmpty() }

    /** Скорость в Мбит/с; null — замер не удался. */
    fun speed(json: String): Double? = runCatching {
        JSONObject(json).optDouble("mbps", 0.0)
    }.getOrNull()?.takeIf { it > 0 }

    // ------------------------------------------------------------ слова

    /** Название шага; null — ядро прислало код, о котором приложение не знает. */
    fun title(id: String): Int? = when (id) {
        "net" -> R.string.diag_step_net
        "dns" -> R.string.diag_step_dns
        "node" -> R.string.diag_step_node
        "tunnel" -> R.string.diag_step_tunnel
        "where" -> R.string.diag_step_where
        "leak" -> R.string.diag_step_leak
        "sites" -> R.string.diag_step_sites
        else -> null
    }

    /**
     * Что сказать под шагом. У хорошего — ничего: число справа уже ответ.
     * Под «подключение» плохое бывает двух видов, и слова разные: «не
     * поднялось» и «сервер замолчал» ведут к разным кнопкам.
     */
    fun say(s: Step): Int? = when (s.id to s.st) {
        "net" to "bad" -> R.string.diag_net_bad
        "dns" to "bad" -> R.string.diag_dns_bad
        "node" to "warn" -> R.string.diag_node_warn
        "node" to "bad" -> R.string.diag_node_bad
        "node" to "off" -> R.string.diag_node_off
        "tunnel" to "warn" -> R.string.diag_tunnel_warn
        "tunnel" to "bad" -> if ("reconnect" in s.act) R.string.diag_tunnel_stalled else R.string.diag_tunnel_bad
        "tunnel" to "off" -> R.string.diag_tunnel_off
        "where" to "bad" -> R.string.diag_where_bad
        "leak" to "warn" -> R.string.diag_leak_warn
        "leak" to "bad" -> R.string.diag_leak_bad
        "sites" to "warn" -> R.string.diag_sites_warn
        "sites" to "bad" -> R.string.diag_sites_bad
        else -> null
    }

    /** Подпись кнопки; null — кнопки не рисуем, раз делать по ней нечего. */
    fun act(code: String): Int? = when (code) {
        "connect" -> R.string.diag_act_connect
        "reconnect" -> R.string.diag_act_reconnect
        "nodes" -> R.string.diag_act_nodes
        "fragment" -> R.string.diag_act_fragment
        "dns" -> R.string.diag_act_dns
        else -> null
    }

    fun glyph(st: String): String = when (st) {
        "ok" -> "✓"
        "warn" -> "!"
        "bad" -> "✕"
        else -> "–"
    }

    /** Число справа от шага: время, «3 из 4», страна. */
    fun value(ctx: Context, s: Step): String = when (s.id) {
        "net", "dns" -> if (s.ms > 0) ctx.getString(R.string.node_ms, s.ms) else ""
        "node" -> if (s.of == 0) "" else ctx.getString(R.string.diag_of, s.n, s.of) +
            if (s.ms > 0) " · " + ctx.getString(R.string.diag_best, s.ms) else ""
        "where" -> listOf(s.ip, countryName(ctx, s.country)).filter { it.isNotEmpty() }.joinToString(" · ")
        "leak" -> if (s.st == "ok") ctx.getString(R.string.diag_hidden) else ""
        "sites" -> if (s.of == 0) "" else ctx.getString(R.string.diag_of, s.n, s.of)
        else -> ""
    }.ifEmpty { if (s.st == "off" && s.id in NEED_TUNNEL) ctx.getString(R.string.diag_need_up) else "" }

    /** Шаги, которым нужен туннель: без него у них пропуск, а не беда. */
    val NEED_TUNNEL = setOf("where", "leak", "sites")

    /** Страна по двухбуквенному коду на языке интерфейса; пусто — кода нет. */
    fun countryName(ctx: Context, code: String): String {
        if (code.length != 2) return ""
        val locale = ctx.resources.configuration.locales[0]
        val region = code.uppercase(Locale.ROOT)
        // Builder, а не конструктор: тот устарел, а Builder заодно отвергает
        // не код страны — тогда показываем сами две буквы.
        return runCatching { Locale.Builder().setRegion(region).build().getDisplayCountry(locale) }
            .getOrNull().orEmpty().ifEmpty { region }
    }

    // ------------------------------------------------------ история скорости

    /**
     * SpeedHistory — последние замеры скорости. Пять: этого хватает, чтобы
     * увидеть «вчера было 80, сегодня 8», и мало, чтобы список стал отчётом
     * о том, когда человек пользовался телефоном.
     *
     * Строкой с табуляцией, а не JSON: так историю можно разобрать и в
     * тесте без Android, а имя ноды с табуляцией внутри мы чистим сами.
     */
    object SpeedHistory {
        const val KEEP = 5

        data class Record(val at: Long, val mbps: Double, val node: String)

        fun add(history: List<Record>, record: Record): List<Record> = (listOf(record) + history).take(KEEP)

        fun encode(history: List<Record>): String = history.take(KEEP).joinToString("\n") {
            it.at.toString() + "\t" + it.mbps + "\t" + it.node.replace(Regex("[\\t\\r\\n]"), " ")
        }

        fun decode(raw: String): List<Record> = raw.lineSequence().mapNotNull { line ->
            val parts = line.split('\t', limit = 3)
            val at = parts.getOrNull(0)?.toLongOrNull() ?: return@mapNotNull null
            val mbps = parts.getOrNull(1)?.toDoubleOrNull()?.takeIf { it > 0 && it.isFinite() } ?: return@mapNotNull null
            Record(at, mbps, parts.getOrNull(2).orEmpty())
        }.take(KEEP).toList()
    }

    fun mbps(ctx: Context, value: Double): String =
        ctx.getString(R.string.stats_mbps, String.format(ctx.resources.configuration.locales[0], "%.1f", value))

    // ------------------------------------------------------------- отчёт

    /** Всё, что экран видел к моменту нажатия «Скопировать отчёт». */
    data class Snapshot(
        val connected: Boolean,
        val server: String,
        val where: Where?,
        val quality: Quality?,
        val route: Route?,
        val steps: List<Step>,
        val speed: SpeedHistory.Record?,
    )

    /**
     * report — отчёт продавцу: то же, что на экране, словами интерфейса, и
     * хвост журнала ниже.
     *
     * Адреса выхода в нём нет вовсе, только страна: продавец знает адреса
     * своих нод и так, а отчёт иногда уходит в публичный чат. Всё остальное —
     * строки журнала в первую очередь — проходит redact (internal/redact в
     * ядре), который закрывает любой адрес и ключ.
     */
    fun report(ctx: Context, s: Snapshot, journal: String, version: String, redact: (String) -> String): String {
        val text = buildString {
            append(ctx.getString(R.string.diag_report_header)).append(" · ").append(version).append('\n')
            append(Build.MANUFACTURER).append(' ').append(Build.MODEL).append(", Android ").append(Build.VERSION.RELEASE).append('\n')
            append('\n')
            append(ctx.getString(R.string.diag_server)).append(": ")
                .append(if (s.connected) s.server else ctx.getString(R.string.diag_report_off)).append('\n')
            s.where?.let { countryName(ctx, it.country) }?.takeIf { it.isNotEmpty() }?.let {
                append(ctx.getString(R.string.diag_country)).append(": ").append(it).append('\n')
            }
            s.quality?.let { q ->
                append(ctx.getString(R.string.diag_ping)).append(": ").append(ctx.getString(R.string.node_ms, q.pingMs))
                append(" · ").append(ctx.getString(R.string.diag_jitter)).append(": ").append(ctx.getString(R.string.node_ms, q.jitterMs))
                append(" · ").append(ctx.getString(R.string.diag_loss)).append(": ").append(ctx.getString(R.string.diag_loss_value, q.lossPct))
                append('\n')
            }
            s.route?.let { r ->
                append(listOf(r.protocol, r.transport, r.security, r.sni, r.fingerprint).filter { it.isNotEmpty() }.joinToString(" · ")).append('\n')
            }
            s.speed?.let { append(ctx.getString(R.string.diag_speed)).append(": ").append(mbps(ctx, it.mbps)).append('\n') }
            if (s.steps.isNotEmpty()) {
                append('\n')
                for (step in s.steps) {
                    val title = title(step.id)?.let(ctx::getString) ?: step.id
                    // Адрес выхода не пишем и здесь: в шаге «тебя видят как»
                    // остаётся только страна.
                    val value = if (step.id == "where") countryName(ctx, step.country) else value(ctx, step)
                    append(glyph(step.st)).append(' ').append(title)
                    if (value.isNotEmpty()) append(" — ").append(value)
                    say(step)?.let { append(". ").append(ctx.getString(it)) }
                    append('\n')
                }
            }
            append('\n').append(journal)
        }
        return redact(text)
    }

    fun time(at: Long): String = SimpleDateFormat("dd.MM HH:mm", Locale.US).format(Date(at))
}
