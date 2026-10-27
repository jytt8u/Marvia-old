package io.marvia.android

import android.content.ClipboardManager
import android.content.Intent
import android.content.res.ColorStateList
import android.view.LayoutInflater
import android.view.View
import android.widget.EditText
import android.widget.LinearLayout
import android.widget.TextView
import android.widget.Toast
import androidx.activity.result.contract.ActivityResultContracts
import androidx.appcompat.app.AppCompatActivity
import androidx.core.graphics.ColorUtils
import androidx.core.view.isVisible
import androidx.core.widget.ImageViewCompat
import androidx.lifecycle.lifecycleScope
import io.marvia.android.databinding.ItemNodeBinding
import io.marvia.android.databinding.ItemProviderBinding
import io.marvia.android.databinding.ScreenServersBinding
import io.marvia.mobile.Mobile
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.flow.collect
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext
import org.json.JSONObject

/**
 * ServersScreen — серверы по провайдерам, как в макете.
 *
 * Провайдер — это подписка: у человека их бывает две, от разных продавцов,
 * и у каждой свои ноды, свой срок и свой остаток. Работает всегда одна:
 * ключ покупателя один на весь список нод, и выбор ноды в чужой подписке
 * означает сначала переключиться на неё.
 *
 * Главное, что этот экран обязан не соврать: выбранная нода и та, через
 * которую идёт трафик, — разные вещи. Человек выбрал ОАЭ, нода замолчала,
 * ядро увезло его в Финляндию и продолжает работать. Написать в этот момент
 * одно «выбрана ОАЭ» значит показать картинку, которой нет.
 *
 * Список нод виден и без туннеля: он приходит из подписки, а не из
 * подключения, и по возможности из кэша — чтобы не ходить в панель зря.
 */
class ServersScreen(
    private val host: AppCompatActivity,
    private val ui: ScreenServersBinding,
    /** Тема на сейчас: цвета выбора, пинга и карточек берутся из неё. */
    private val theme: () -> Theme,
    /** Хранилище подписок: их список, рабочая и выбранная руками нода. */
    private val store: Store,
    /** Подписка сменилась: ключ другой, туннель надо поднимать заново. */
    private val onSubscriptionChanged: () -> Unit,
) {

    /** Что известно про провайдера: ноды, срок, остаток и когда получено. */
    private class Provider(val sub: Store.Subscription) {
        var rows: List<NodeRow> = emptyList()
        var until = ""
        var limit = 0L
        var left = 0L
        var fetchedAt = 0L
        var stale = false
        /** Поддержка, «Продлить» и объявление продавца; пусто — не задано. */
        var support = ""
        var renew = ""
        var announce = ""
        var error = ""
        var loaded = false
        var measuring = false
        var refreshing = false
    }

    private val providers = LinkedHashMap<String, Provider>()

    /** Какие провайдеры развёрнуты. По умолчанию — только рабочий. */
    private val open = HashSet<String>()
    private var openDefaulted = false

    private val dp = host.resources.displayMetrics.density

    init {
        ui.refreshAll.setOnClickListener { refreshAll() }
        ui.autoRow.setOnClickListener { pickAuto() }
        ui.addSubscription.setOnClickListener { askWhereFrom() }
        host.lifecycleScope.launch {
            MarviaState.countryDetected.collect {
                for (p in providers.values) {
                    p.rows = p.rows.map { row ->
                        row.copy(exitCountry = store.exitCountry(p.sub.link, row.id, row.name, row.endpoint))
                    }
                }
                render()
            }
        }
    }

    // --------------------------------------------------------- подписки

    /**
     * Ключ приходит строкой, картинкой или показан на другом экране.
     * Все способы доступны и после добавления первой подписки.
     */
    fun askWhereFrom() {
        val items = listOf(
            host.getString(R.string.servers_add_clipboard),
            host.getString(R.string.servers_add_qr),
            host.getString(R.string.key_camera),
            host.getString(R.string.servers_add_manual),
        )
        ChoiceSheet.show(host, theme(), host.getString(R.string.servers_add_key), items) { which ->
            when (which) {
                0 -> fromClipboard()
                1 -> pickQr.launch("image/*")
                2 -> scanQr.launch(Intent(host, QrCameraActivity::class.java))
                else -> byHand()
            }
        }
    }

    private val scanQr = host.registerForActivityResult(ActivityResultContracts.StartActivityForResult()) { result ->
        if (result.resultCode == AppCompatActivity.RESULT_OK) {
            result.data?.getStringExtra(QrCameraActivity.EXTRA_KEY)?.let { add("", it) }
        }
    }

    /**
     * Вторую подписку тоже присылают QR-кодом. Экран ключа с кнопкой «QR»
     * виден только до первого ключа, так что без этого пункта у человека с
     * ключом картинку было бы не прочитать вовсе.
     */
    private val pickQr = host.registerForActivityResult(ActivityResultContracts.GetContent()) { uri ->
        if (uri == null) return@registerForActivityResult
        host.lifecycleScope.launch {
            val text = withContext(Dispatchers.Default) {
                runCatching { QrImport.fromUri(host, uri) }.getOrNull()
            }
            if (text.isNullOrEmpty()) {
                Toast.makeText(host, R.string.key_qr_missing, Toast.LENGTH_LONG).show()
            } else {
                add("", text)
            }
        }
    }

    private fun fromClipboard() {
        val clip = host.getSystemService(ClipboardManager::class.java)
            ?.primaryClip?.takeIf { it.itemCount > 0 }
            ?.getItemAt(0)?.text?.toString().orEmpty()
        if (clip.isBlank()) {
            Toast.makeText(host, R.string.key_clipboard_empty, Toast.LENGTH_SHORT).show()
            return
        }
        add("", clip)
    }

    /** byHand — имя и ссылка. Имя необязательно: без него возьмём домен. */
    private fun byHand() {
        val pad = (18 * dp).toInt()
        val box = LinearLayout(host).apply {
            orientation = LinearLayout.VERTICAL
            setPadding(pad, (8 * dp).toInt(), pad, 0)
        }
        val name = EditText(host).apply {
            id = View.generateViewId()
            setHint(R.string.servers_sub_name)
            setSingleLine()
        }
        val link = EditText(host).apply {
            id = View.generateViewId()
            setHint(R.string.appearance_access_hint)
            setSingleLine()
        }
        fun label(text: Int, field: EditText) = TextView(host).apply {
            setText(text)
            textSize = 13f
            labelFor = field.id
            setTextColor(theme().dim)
            setPadding(0, (10 * dp).toInt(), 0, 0)
        }
        box.addView(label(R.string.appearance_access_name, name))
        box.addView(name)
        box.addView(label(R.string.appearance_access_link, link))
        box.addView(link)

        val t = theme()
        for (field in listOf(name, link)) {
            field.setTextColor(t.fg); field.setHintTextColor(t.dim)
            field.backgroundTintList = ColorStateList.valueOf(t.acc)
            field.minHeight = (54 * dp).toInt()
        }
        link.inputType = android.text.InputType.TYPE_CLASS_TEXT or android.text.InputType.TYPE_TEXT_VARIATION_URI or android.text.InputType.TYPE_TEXT_FLAG_NO_SUGGESTIONS
        ChoiceSheet.form(host, t, host.getString(R.string.servers_add_manual), box, host.getString(R.string.servers_add)) {
            val clean = link.text.toString().trim()
            val valid = runCatching { Mobile.checkAccountLink(clean) }.isSuccess
            if (!valid) link.error = host.getString(R.string.servers_sub_bad)
            else add(name.text.toString(), clean)
            valid
        }
    }

    /**
     * add принимает ссылку, проверив её ядром.
     *
     * Проверяем до сохранения: положить мусор и узнать об этом при следующем
     * подключении значит объяснять человеку поломку через час после того,
     * как он её устроил.
     */
    private fun add(name: String, link: String) {
        val clean = link.trim()
        val good = runCatching { Mobile.checkAccountLink(clean) }.isSuccess
        if (!good) {
            Toast.makeText(host, R.string.servers_sub_bad, Toast.LENGTH_LONG).show()
            return
        }

        val was = store.accountLink
        store.addSubscription(name, clean)
        Toast.makeText(host, R.string.servers_sub_added, Toast.LENGTH_SHORT).show()
        open.add(clean)
        sync()
        providers[clean]?.let { load(it, refresh = false) }
        if (store.accountLink != was) onSubscriptionChanged()
    }

    /** use делает подписку рабочей: ключ другой, туннель поднимается заново. */
    private fun use(sub: Store.Subscription) {
        if (sub.link == store.accountLink) return
        store.accountLink = sub.link
        open.add(sub.link)
        render()
        onSubscriptionChanged()
    }

    private fun forget(sub: Store.Subscription) {
        ThemedDialogs.builder(host, theme())
            .setMessage(host.getString(R.string.servers_sub_remove_ask, sub.name))
            .setPositiveButton(R.string.servers_sub_remove) { _, _ ->
                val was = store.accountLink
                store.removeSubscription(sub.link)
                providers.remove(sub.link)
                render()
                if (store.accountLink != was) onSubscriptionChanged()
            }
            .setNegativeButton(android.R.string.cancel, null)
            .show()
    }

    /** more — действия с подпиской: сделать рабочей, перенести, убрать. */
    private fun more(p: Provider) {
        val active = p.sub.link == store.accountLink
        val actions = buildList<Pair<String, () -> Unit>> {
            if (!active) add(host.getString(R.string.servers_use) to { use(p.sub) })
            add(host.getString(R.string.servers_sub_qr) to { showQr(p.sub) })
            add(host.getString(R.string.servers_sub_remove) to { forget(p.sub) })
        }
        ChoiceSheet.show(host, theme(), p.sub.name, actions.map { it.first }) { which -> actions[which].second() }
    }

    /**
     * showQr показывает ключ QR-кодом — переезд на новый телефон без
     * пересылки ключа самому себе в чат, где он остался бы навсегда.
     *
     * В ключе личный ключ покупателя: кто отсканирует, тот ходит по его
     * доступу. Поэтому окно говорит об этом прямо и запрещает снимок
     * экрана (FLAG_SECURE) — скриншот QR и есть тот самый ключ в галерее.
     */
    private fun showQr(sub: Store.Subscription) {
        val side = (240 * host.resources.displayMetrics.density).toInt()
        val pixels = runCatching { QrImport.encode(sub.link, side) }.getOrNull() ?: return
        val image = android.widget.ImageView(host).apply {
            setImageBitmap(android.graphics.Bitmap.createBitmap(pixels, side, side, android.graphics.Bitmap.Config.ARGB_8888))
            adjustViewBounds = true
            val pad = (16 * host.resources.displayMetrics.density).toInt()
            setPadding(pad, pad, pad, 0)
        }
        val dialog = ThemedDialogs.builder(host, theme())
            .setTitle(R.string.servers_qr_title)
            .setMessage(R.string.servers_qr_body)
            .setView(image)
            .setPositiveButton(android.R.string.ok, null)
            .create()
        dialog.window?.addFlags(android.view.WindowManager.LayoutParams.FLAG_SECURE)
        dialog.show()
    }

    // ----------------------------------------------------------- данные

    /** sync сверяет список провайдеров с хранилищем, не теряя загруженного. */
    private fun sync() {
        val subs = store.subscriptions
        val keep = subs.map { it.link }.toSet()
        providers.keys.retainAll(keep)
        for (sub in subs) providers.getOrPut(sub.link) { Provider(sub) }
        if (!openDefaulted && store.accountLink.isNotEmpty()) {
            open.add(store.accountLink)
            openDefaulted = true
        }
        render()
    }

    /**
     * open зовётся при каждом показе экрана: подписки и туннель могли смениться.
     *
     * Подписки, которых ещё не видели, читаются из кэша или панели. Рабочую
     * при первом заходе за подключение меряем сами: экран, где вместо времён
     * стоят прочерки, выбрать не помогает.
     */
    fun open() {
        sync()
        for (p in providers.values) {
            if (!p.loaded && !p.refreshing) load(p, refresh = false)
        }
        val active = providers[store.accountLink] ?: return
        if (MarviaState.core != null && !MarviaState.anyMeasured() && !active.measuring) measure(active)
        else if (MarviaState.core != null) load(active, refresh = false)
    }

    /**
     * load читает подписку: ноды, срок, остаток. С поднятым туннелем ноды
     * рабочей подписки берутся у ядра — у него есть текущая и выбранная.
     */
    private fun load(p: Provider, refresh: Boolean) {
        if (p.refreshing) return
        p.refreshing = refresh
        renderProvider(p)
        host.lifecycleScope.launch {
            val core = MarviaState.core
            val active = p.sub.link == store.accountLink
            val json = withContext(Dispatchers.IO) {
                try {
                    if (core == null) Mobile.subscription(p.sub.link, store.cacheDir(), refresh)
                    else core.subscription(p.sub.link, store.cacheDir(), refresh)
                } catch (t: Throwable) {
                    p.error = human(t)
                    ""
                }
            }
            if (json.isNotEmpty()) take(p, json)
            if (active && core != null) {
                val live = withContext(Dispatchers.IO) { runCatching { core.nodes() }.getOrDefault("[]") }
                merge(p, parseRows(p, live))
            }
            p.loaded = true
            p.refreshing = false
            render()
        }
    }

    /**
     * measure меряет ноды провайдера настоящим подключением к каждой.
     *
     * Рабочую подписку с поднятым туннелем меряет ядро: замер снаружи ушёл бы
     * через сам туннель и показал бы не то. Остальные — ядро подписки, без
     * туннеля.
     */
    private fun measure(p: Provider) {
        if (p.measuring) return
        p.measuring = true
        open.add(p.sub.link)
        renderProvider(p)
        host.lifecycleScope.launch {
            val core = MarviaState.core
            val active = p.sub.link == store.accountLink
            val json = withContext(Dispatchers.IO) {
                try {
                    if (active && core != null) core.measure() else Mobile.measureNodes(p.sub.link, store.cacheDir(), store.tunnelSettings())
                } catch (t: Throwable) {
                    p.error = human(t)
                    ""
                }
            }
            if (json.isNotEmpty()) {
                if (active && core != null) {
                    val rows = parseRows(p, json)
                    MarviaState.remember(rows)
                    merge(p, rows)
                } else {
                    take(p, json)
                }
            }
            p.loaded = true
            p.measuring = false
            render()
        }
    }

    /** refreshAll — все подписки заново из панели, рабочую — ещё и замерить. */
    private fun refreshAll() {
        for (p in providers.values) load(p, refresh = true)
    }

    /** take разбирает ответ ядра про подписку. */
    private fun take(p: Provider, json: String) {
        val o = try { JSONObject(json) } catch (_: Throwable) { return }
        val rows = parseRows(p, o.optJSONArray("nodes")?.toString() ?: "[]")
        // Замеры с прошлого раза не теряем: ответ без времён — не повод
        // превращать список в прочерки.
        val known = p.rows.associateBy { it.id }
        p.rows = rows.map { r ->
            val k = known[r.id]
            if (r.ms == 0L && k != null && k.ms > 0) r.copy(ms = k.ms, alive = k.alive, setupMs = k.setupMs) else r
        }
        p.until = o.optString("until", "")
        p.limit = o.optLong("limit", 0)
        p.left = o.optLong("left", 0)
        p.fetchedAt = o.optLong("fetched_at", 0)
        p.stale = o.optBoolean("stale", false)
        p.support = o.optString("support_url", "")
        p.renew = o.optString("renew_url", "")
        p.announce = o.optString("announce", "")
        p.error = ""
        SellerChannel.remind(
            host, store, p.sub.name, p.sub.link,
            o.optString("remind", ""), o.optLong("remind_value", 0), o.optString("remind_key", ""), p.renew,
        )
        SellerChannel.announce(host, store, p.sub.name, p.sub.link, p.announce)
    }

    private fun parseRows(p: Provider, json: String): List<NodeRow> = NodeRow.parse(json) { id, name, endpoint ->
        store.exitCountry(p.sub.link, id, name, endpoint)
    }

    /** merge накладывает ответ ядра (текущая, выбранная, времена) на список подписки. */
    private fun merge(p: Provider, live: List<NodeRow>) {
        if (live.isEmpty()) return
        val byId = live.associateBy { it.id }
        p.rows = if (p.rows.isEmpty()) live else p.rows.map { r ->
            val l = byId[r.id] ?: return@map r.copy(current = false, chosen = false)
            r.copy(
                current = l.current, chosen = l.chosen,
                ms = if (l.ms > 0) l.ms else MarviaState.ping(r.id)?.ms ?: r.ms,
                alive = if (l.ms > 0 || l.current) l.alive else MarviaState.ping(r.id)?.alive ?: r.alive,
                setupMs = if (l.setupMs > 0) l.setupMs else r.setupMs,
            )
        }
        publish(p.rows)
    }

    /** publish переносит текущую и выбранную ноду в общее состояние: главный экран узнаёт о переезде отсюда. */
    private fun publish(rows: List<NodeRow>) {
        val on = MarviaState.state.value as? TunnelState.On ?: return
        val current = rows.firstOrNull { it.current }
        val chosen = rows.firstOrNull { it.chosen }
        MarviaState.set(
            on.copy(
                node = current?.title ?: on.node,
                ms = current?.ms ?: 0,
                chosen = chosen?.title.orEmpty(),
            ),
        )
    }

    // ------------------------------------------------------------ выбор

    /**
     * pick — нода выбрана руками. В чужой подписке — сначала она становится
     * рабочей, и туннель поднимается заново уже на выбранной ноде.
     */
    private fun pick(p: Provider, row: NodeRow) {
        if (p.sub.link != store.accountLink) {
            store.accountLink = p.sub.link
            store.chosenNode = row.id
            render()
            onSubscriptionChanged()
            return
        }
        select(p, row.id)
    }

    private fun pickAuto() {
        val p = providers[store.accountLink] ?: return
        select(p, AUTO)
    }

    /**
     * select переводит туннель на ноду. Ноль — обратно к автовыбору.
     *
     * Ядро при этом заново договаривается с нодой: это поход в сеть, и он
     * может не получиться. Молча вернуть человека к прежней стране нельзя —
     * он решит, что нажатие не сработало. Без туннеля выбор просто
     * запоминается: следующий подъём начнётся с него.
     */
    private fun select(p: Provider, id: Long) {
        val core = MarviaState.core
        if (core == null) {
            store.chosenNode = id
            p.rows = p.rows.map { it.copy(chosen = it.id == id) }
            render()
            return
        }
        if (p.measuring) return
        p.measuring = true
        renderProvider(p)
        host.lifecycleScope.launch {
            val failure = withContext(Dispatchers.IO) {
                try { core.selectNode(id); "" } catch (t: Throwable) { MarviaVpnService.reasonOf(t) }
            }
            if (failure.isNotEmpty()) {
                Toast.makeText(host, host.getString(R.string.servers_failed, failure), Toast.LENGTH_LONG).show()
            } else {
                store.chosenNode = id
            }
            p.measuring = false
            load(p, refresh = false)
        }
    }

    // ------------------------------------------------------------ вид

    /** paint перекрашивает экран в новую тему по тому, что уже показано. */
    fun paint() = render()

    private fun render() {
        val t = theme()
        val list = providers.values.toList()
        val active = providers[store.accountLink]
        val nodes = list.sumOf { it.rows.size }

        ui.serversSubtitle.text = if (list.isEmpty()) "" else listOf(
            host.resources.getQuantityString(R.plurals.servers_providers, list.size, list.size),
            host.resources.getQuantityString(R.plurals.servers_count, nodes, nodes),
        ).joinToString(" · ")

        ui.serversEmpty.isVisible = list.isEmpty()
        ui.countryDetectionNote.isVisible = list.isNotEmpty()
        ui.serversEmpty.setText(R.string.servers_no_subs)
        ui.autoRow.isVisible = active != null
        ui.refreshSpinner.isVisible = list.any { it.refreshing }

        // Автовыбор действует, пока человек ничего не выбрал руками.
        val manual = store.chosenNode != 0L || active?.rows?.any { it.chosen } == true
        ui.autoRow.background = card(t, if (manual) t.line else t.acc)
        ui.autoIconBox.background = Paint.rounded(t.accSoft, 12, dp)
        ui.autoMark.background = Paint.circle(t.acc)
        ui.autoMark.isVisible = !manual
        ImageViewCompat.setImageTintList(ui.autoMarkCheck, ColorStateList.valueOf(t.accFg))
        val fastest = active?.rows?.filter { it.alive && it.ms > 0 }?.minByOrNull { it.ms }
        ui.autoNote.text = if (fastest == null) host.getString(R.string.servers_auto_idle)
        // У одиночной чужой ссылки имя подписки и ноды одно и то же — дважды
        // его не пишем.
        else host.getString(R.string.servers_auto_fastest, listOf(fastest.group, fastest.name).filter { it.isNotEmpty() }.distinct().joinToString(" · "))

        // Карточки провайдеров собираются заново: их единицы, а состояние у
        // каждой своё, и точечно обновлять дешевле не выйдет.
        ui.providerList.removeAllViews()
        val inflater = LayoutInflater.from(host)
        for (p in list) {
            val item = ItemProviderBinding.inflate(inflater, ui.providerList, false)
            item.root.tag = null
            item.root.background = card(t, t.line)
            (item.root.layoutParams as? android.view.ViewGroup.MarginLayoutParams)?.topMargin = (t.gap * dp).toInt()
            fill(item, p)
            ui.providerList.addView(item.root)
        }
    }

    /** renderProvider — перерисовать только одного, пока он занят. */
    private fun renderProvider(p: Provider) = render()

    private fun fill(item: ItemProviderBinding, p: Provider) {
        val t = theme()
        val active = p.sub.link == store.accountLink
        val isOpen = p.sub.link in open
        // Сначала общая покраска тегами, потом своё: полоса и кнопки — не по тегу.
        Paint.apply(item.root, t)

        item.providerName.text = p.sub.name
        item.providerMeta.text = buildList {
            add(host.resources.getQuantityString(R.plurals.servers_count, p.rows.size, p.rows.size))
            if (active) add(host.getString(R.string.servers_sub_active))
            if (!isOpen) add(host.getString(R.string.servers_collapsed))
        }.joinToString(" · ")
        item.providerChevron.rotation = if (isOpen) 90f else 0f
        item.providerHead.setOnClickListener {
            if (isOpen) open.remove(p.sub.link) else open.add(p.sub.link)
            render()
        }
        item.providerAge.text = when {
            p.measuring -> host.getString(R.string.servers_measuring)
            p.refreshing -> "…"
            else -> age(p.fetchedAt)
        }
        item.providerSpinner.isVisible = p.measuring
        for (b in listOf(item.providerRefresh, item.providerMeasure, item.providerMore)) {
            b.background = Paint.circle(t.surf2)
        }
        item.providerRefresh.setOnClickListener { load(p, refresh = true) }
        item.providerMeasure.setOnClickListener { measure(p) }
        item.providerMore.setOnClickListener { more(p) }

        // Срок и остаток. Ни того ни другого — строка не нужна: продавец не ставил.
        val days = if (p.until.isEmpty()) null else Format.daysLeft(p.until)
        val quota = p.limit > 0
        item.providerQuota.isVisible = p.loaded && (days != null || quota || p.until.isNotEmpty())
        item.providerDays.text = when {
            days != null -> host.getString(R.string.servers_days_left, host.resources.getQuantityString(R.plurals.days_left, days, days))
            p.until.isNotEmpty() -> host.getString(R.string.sub_until_only, Format.day(host, p.until))
            else -> host.getString(R.string.servers_no_term)
        }
        item.providerTrack.background = Paint.rounded(t.shade, 999, dp)
        item.providerTrack.clipToOutline = true
        item.providerFill.background = Paint.rounded(t.acc, 999, dp)
        item.providerFill.alpha = if (quota) 1f else 0.35f
        item.providerLeft.text = if (quota) host.getString(R.string.servers_quota, Format.size(host, p.left), Format.size(host, p.limit))
        else host.getString(R.string.servers_unlimited)
        item.providerTrack.post {
            val share = if (quota) (p.left.toFloat() / p.limit).coerceIn(0f, 1f) else 1f
            item.providerFill.layoutParams = item.providerFill.layoutParams.apply { width = (item.providerTrack.width * share).toInt() }
        }

        // Связь с продавцом. Кнопка без ссылки — пустое обещание, поэтому
        // показываем только те, что продавец оставил.
        item.providerAnnounce.isVisible = p.announce.isNotEmpty()
        item.providerAnnounce.text = p.announce
        val renew = SellerChannel.openable(p.renew)
        val support = SellerChannel.openable(p.support)
        item.providerSeller.isVisible = renew || support
        item.providerRenew.isVisible = renew
        item.providerSupport.isVisible = support
        item.providerRenew.setOnClickListener { SellerChannel.open(host, p.renew) }
        item.providerSupport.setOnClickListener { SellerChannel.open(host, p.support) }

        item.providerNote.isVisible = p.error.isNotEmpty() || p.stale
        item.providerNote.text = p.error.ifEmpty { host.getString(R.string.servers_stale) }
        item.providerNote.setTextColor(if (p.error.isNotEmpty()) t.warn else t.dim)
        item.providerRule.isVisible = isOpen && p.rows.isNotEmpty()

        item.providerNodes.removeAllViews()
        if (isOpen) {
            val inflater = LayoutInflater.from(host)
            val current = p.rows.firstOrNull { it.current }
            p.rows.forEachIndexed { i, row ->
                item.providerNodes.addView(nodeView(inflater, item.providerNodes, p, row, current, i == 0, active))
            }
        }
        item.root.background = card(t, t.line)
    }

    private fun nodeView(inflater: LayoutInflater, parent: LinearLayout, p: Provider, row: NodeRow, current: NodeRow?, first: Boolean, active: Boolean): View {
        val t = theme()
        val item = ItemNodeBinding.inflate(inflater, parent, false)
        Paint.apply(item.root, t)
        val measured = row.ms > 0 || (active && MarviaState.ping(row.id) != null)
        val ping = if (row.ms > 0) MarviaState.Ping(row.ms, row.alive, row.setupMs) else if (active) MarviaState.ping(row.id) else null
        val down = measured && ping?.alive == false
        val chosen = row.chosen || (active && store.chosenNode == row.id && MarviaState.core == null)

        val flag = Flags.of(row.group)
        item.nodeFlag.text = flag
        item.nodeFlag.isVisible = flag.isNotEmpty()
        item.nodeTitle.text = row.group.ifEmpty { row.name }
        item.nodeTitle.setTextColor(if (down) t.dim else t.fg)

        // Город и хост, затем — что с нодой: молчит, выбрана, через неё трафик.
        val place = row.country.substringAfter('·', "").trim()
        item.nodeNote.text = buildList {
            if (place.isNotEmpty()) add(place)
            // Без страны имя ноды уже стоит заголовком — второй раз подписью оно
            // только занимает строку.
            if (row.group.isNotEmpty()) add(row.name)
            if (row.countryFromExit) add(host.getString(R.string.country_from_exit_ip))
            when {
                down -> add(host.getString(R.string.node_down))
                chosen && row.current -> add(host.getString(R.string.node_chosen_current))
                chosen && current != null -> add(host.getString(R.string.node_chosen_silent, current.group.ifEmpty { current.name }))
                chosen -> add(host.getString(R.string.node_picked))
                row.current -> add(host.getString(R.string.node_traffic_now))
            }
        }.joinToString(" · ")

        item.nodeBars.lit = SignalBars.of(ping?.ms ?: 0, ping?.alive == true)
        item.nodeBars.on = t.acc
        item.nodeBars.off = t.line
        item.nodePing.text = when {
            p.measuring -> "…"
            ping == null || ping.ms <= 0 -> "—"
            else -> host.getString(R.string.node_ms, ping.ms)
        }
        item.nodePing.setTextColor(when {
            down || ping == null -> t.dim
            row.current -> t.fg
            else -> pingColor(t, ping.ms)
        })

        item.root.background = when {
            row.current -> Paint.rounded(ColorUtils.setAlphaComponent(t.acc, 26), 0, dp)
            else -> null
        }
        if (!first) {
            // Тонкая линия между строками — не рамка, а разделитель внутри карточки.
            val rule = View(host).apply { setBackgroundColor(ColorUtils.setAlphaComponent(t.line, 160)) }
            parent.addView(rule, LinearLayout.LayoutParams(LinearLayout.LayoutParams.MATCH_PARENT, 1))
        }
        item.root.alpha = if (down) 0.7f else 1f
        item.root.setOnClickListener { if (!down) pick(p, row) }
        return item.root
    }

    /** Карточка с рамкой заданного цвета; у плоских карточек рамки нет, но выбор обводим всегда. */
    private fun card(t: Theme, stroke: Int) = Paint.card(t, dp, stroke = stroke).apply {
        if (stroke != t.line) setStroke((1.5f * dp).toInt(), stroke)
    }

    /**
     * human — неудача словами человека, а не ядра: у ошибки первой строкой
     * идёт вид, и под него есть фраза. Без вида — как есть, это честнее выдумки.
     */
    private fun human(t: Throwable): String {
        val f = MarviaVpnService.failureOf(t)
        val text = when (f.kind) {
            Mobile.FailAccount -> R.string.fail_account
            Mobile.FailPanel -> R.string.fail_panel
            Mobile.FailExpired -> R.string.fail_expired
            Mobile.FailQuota -> R.string.fail_quota
            else -> null
        }
        return if (text == null) f.detail else host.getString(text)
    }

    /** age — как давно список получен из панели: «только что», «1 ч», «3 дн». */
    private fun age(fetchedAt: Long): String {
        if (fetchedAt <= 0) return ""
        val s = System.currentTimeMillis() / 1000 - fetchedAt
        return when {
            s < 90 -> host.getString(R.string.servers_age_now)
            s < 3600 -> host.getString(R.string.servers_age_min, (s / 60).toInt())
            s < 86_400 -> host.getString(R.string.servers_age_hours, (s / 3600).toInt())
            else -> host.getString(R.string.servers_age_days, (s / 86_400).toInt())
        }
    }

    private companion object {
        /** Ноль в selectNode означает «выбирай сам». */
        const val AUTO = 0L
    }
}

/**
 * pingColor — быстро акцентом, терпимо янтарным, плохо красным.
 *
 * Пороги одни на оба экрана: список стран и главный экран не должны спорить,
 * хорошие ли это сорок миллисекунд.
 */
fun pingColor(t: Theme, ms: Long): Int = when {
    ms < 70 -> t.acc
    ms < 250 -> t.warn
    else -> t.fail
}
