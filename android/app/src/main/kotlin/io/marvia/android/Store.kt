package io.marvia.android

import android.content.Context
import android.graphics.Bitmap
import android.graphics.BitmapFactory
import io.marvia.mobile.Mobile
import java.io.File
import java.security.MessageDigest

/**
 * Store — то немногое, что приложение помнит между запусками.
 *
 * Ключ доступа лежит в обычных настройках приложения. Их защищает песочница
 * Android: другое приложение файл не прочитает, а на заблокированном телефоне
 * он зашифрован ключом владельца. От root-доступа это не спасает, но и
 * androidx.security ничего к этому не добавил бы — там тот же аппаратный
 * ключ, только через прослойку, которую сама Google перестала развивать.
 *
 * Рядом с настройками ядро держит кэш списка нод. Сам файл пишет и читает Go,
 * приложение знает про него ровно одно: когда ключ меняется, старый кэш надо
 * убрать. Ядро от чужого кэша защищено и само — оно сверяет отпечаток адреса
 * подписки, — но оставлять на диске адреса нод прошлого продавца незачем.
 */
class Store(context: Context) {

    private val app = context.applicationContext

    private val prefs = app.getSharedPreferences("veil", Context.MODE_PRIVATE)

    /** Ссылка вида marvia://… — её выдаёт бот продавца при оплате. */
    var accountLink: String
        get() = prefs.getString(KEY_ACCOUNT_LINK, "").orEmpty()
        set(value) {
            val link = value.trim()
            if (link != accountLink) {
                chosenNode = 0
                // Запоминаем день, а не саму ссылку: в настройках человек
                // видит, когда ключ появился, и понимает, тот ли он, что
                // прислал продавец на прошлой неделе.
                prefs.edit().putLong(KEY_ACCOUNT_SAVED, System.currentTimeMillis()).apply()
            }
            prefs.edit().putString(KEY_ACCOUNT_LINK, link).apply()
            // Ключ кладут и с онбординга, и нажатием marvia:// из чата.
            // Подхватываем его здесь, иначе добавленная позже вторая
            // подписка окажется в списке одна, а первая — нигде.
            if (link.isNotEmpty() && subscriptions.none { it.link == link }) {
                subscriptions = subscriptions + Subscription(defaultSubscriptionName(link), link)
            }
        }

    /**
     * chosenNode — нода, которую человек выбрал руками; ноль — автовыбор.
     *
     * Живёт здесь, а не в ядре: ядро поднимается с каждым туннелем заново и
     * своего выбора не помнит. Номер ноды принадлежит подписке, поэтому при
     * смене рабочей подписки выбор сбрасывается — чужой номер указал бы
     * не туда.
     */
    var chosenNode: Long
        get() = prefs.getLong(KEY_CHOSEN_NODE, 0)
        set(value) { prefs.edit().putLong(KEY_CHOSEN_NODE, value).apply() }

    /**
     * onboarded — второй шаг первого запуска (разрешения) пройден. Язык —
     * отдельно: его выбирают и из настроек, а разрешения спрашивают один раз.
     */
    var onboarded: Boolean
        get() = prefs.getBoolean(KEY_ONBOARDED, false)
        set(value) { prefs.edit().putBoolean(KEY_ONBOARDED, value).apply() }

    /** lastNode — через какую ноду шёл трафик в прошлый раз: главная показывает её и выключенной. */
    var lastNode: String
        get() = prefs.getString(KEY_LAST_NODE, "").orEmpty()
        set(value) { prefs.edit().putString(KEY_LAST_NODE, value).apply() }

    /** Когда ключ положили сюда. Ноль означает, что он появился до этой записи. */
    val accountSavedAt: Long
        get() = prefs.getLong(KEY_ACCOUNT_SAVED, 0)

    /**
     * Подписка: имя и ссылка от одного продавца.
     *
     * Их бывает несколько — у человека может быть доступ у двоих, — но
     * работает всегда одна. Так устроен и Hiddify, откуда взят образец, и так
     * это ничего не стоит: ядру по-прежнему отдаётся одна ссылка, а ключ
     * покупателя остаётся один на весь список нод.
     *
     * Несколько подписок одновременно означали бы свой ключ у каждой ноды и
     * переделку выбора ноды в ядре. Выгоды от этого нет: человек всё равно
     * выходит в интернет через одну страну за раз.
     */
    data class Subscription(val name: String, val link: String)

    /**
     * subscriptions — что человек добавил. Хранится строками «имя\nссылка», по
     * одной паре на запись: JSON ради двух полей — лишняя зависимость, а
     * перевод строки в имени и так не наберёшь.
     */
    var subscriptions: List<Subscription>
        get() = prefs.getStringSet(KEY_SUBSCRIPTIONS, emptySet()).orEmpty()
            .mapNotNull(::subscriptionOf)
            .sortedBy { it.name.lowercase() }
        set(value) {
            prefs.edit().putStringSet(KEY_SUBSCRIPTIONS, value.map(::rowOf).toSet()).apply()
            SubscriptionRefresh.sync(app, value.isNotEmpty())
        }

    /** addSubscription кладёт подписку и делает её рабочей. Повтор той же ссылки не двоится. */
    fun addSubscription(name: String, link: String) {
        val clean = link.trim()
        val title = name.trim().ifEmpty { defaultSubscriptionName(clean) }
        subscriptions = subscriptions.filter { it.link != clean } + Subscription(title, clean)
        accountLink = clean
    }

    fun removeSubscription(link: String) {
        subscriptions = subscriptions.filter { it.link != link }
        dropSubscriptionCache(link)
        val prefix = exitCountryPrefix(link)
        val edit = prefs.edit()
        prefs.all.keys.filter { it.startsWith(prefix) }.forEach { edit.remove(it) }
        edit.remove(remindedKey(link))
        edit.apply()
        // Убрали рабочую — остаёмся без ключа, а не с чужим втихую.
        if (accountLink == link) accountLink = ""
    }

    /** Country of this node's exit IP, learned once through the active tunnel. */
    fun exitCountry(link: String, nodeId: Long, name: String, endpoint: String): String {
        val parts = prefs.getString(exitCountryKey(link, nodeId, name, endpoint), null)?.split('|') ?: return ""
        if (parts.size != 2) return ""
        val savedAt = parts[1].toLongOrNull() ?: return ""
        if (System.currentTimeMillis() - savedAt !in 0..EXIT_COUNTRY_MAX_AGE) return ""
        return Flags.countryOf(parts[0].lowercase()).orEmpty()
    }

    fun rememberExitCountry(link: String, nodeId: Long, name: String, endpoint: String, code: String) {
        if (Flags.countryOf(code.lowercase()) == null) return
        prefs.edit().putString(
            exitCountryKey(link, nodeId, name, endpoint),
            "${code.lowercase()}|${System.currentTimeMillis()}",
        ).apply()
    }

    /**
     * reminded — ключи напоминаний о сроке, которые по этой подписке уже
     * показали, от старых к новым. Строкой, а не набором: набор порядка не
     * хранит, а выбрасывать надо самые старые.
     */
    fun reminded(link: String): List<String> =
        prefs.getString(remindedKey(link), "").orEmpty().split('\n').filter { it.isNotEmpty() }

    fun setReminded(link: String, keys: List<String>) {
        prefs.edit().putString(remindedKey(link), keys.joinToString("\n")).apply()
    }

    private fun remindedKey(link: String) = "reminded_${hash(link)}"

    private fun exitCountryPrefix(link: String) = "exit_country_${hash(link)}_"
    private fun exitCountryKey(link: String, nodeId: Long, name: String, endpoint: String) =
        exitCountryPrefix(link) + hash("$nodeId\n$name\n$endpoint")

    private fun hash(value: String): String = MessageDigest.getInstance("SHA-256")
        .digest(value.toByteArray(Charsets.UTF_8)).take(12).joinToString("") { "%02x".format(it.toInt() and 0xff) }

    /**
     * defaultSubscriptionName — имя, когда человек его не ввёл: домен подписки.
     *
     * Он и отличает продавцов друг от друга, а «Подписка 1» и «Подписка 2»
     * не отличают ничего.
     */
    private fun defaultSubscriptionName(link: String): String =
        nameFromLink(link, app.getString(R.string.servers_sub_untitled))

    /**
     * bypassed — приложения, которые ходят мимо туннеля.
     *
     * Госуслуги, банки и всё государственное не отвечают на запросы с
     * зарубежных адресов, а нода стоит за границей — отсюда и берётся «включил
     * VPN, и госуслуги не открываются». Исключённому приложению система
     * оставляет обычную сеть, и на сервер приходит нормальный российский адрес.
     *
     * Значок VPN в шторке при этом остаётся: его рисует Android, пока поднят
     * туннель, и убрать его нельзя ничем. Но ломает госуслуги не значок, а
     * адрес, и вот его исключение чинит.
     */
    var bypassed: Set<String>
        // Копию отдаём намеренно: SharedPreferences возвращает своё множество,
        // и правка его на месте молча не сохранилась бы.
        get() = prefs.getStringSet(KEY_BYPASSED, emptySet())?.toSet().orEmpty()
        set(value) {
            prefs.edit().putStringSet(KEY_BYPASSED, value.toSet()).apply()
        }

    /**
     * bypassMode — что делать со списком приложений.
     *
     * «Мимо туннеля»: отмеченные ходят напрямую, остальные — через туннель.
     * «Только эти»: наоборот, в туннель идут одни отмеченные — так живут те,
     * кому туннель нужен ради двух приложений, а банк и такси должны видеть
     * настоящий адрес. «Выключено»: список остаётся, но не применяется —
     * чтобы отметки не пропали, когда человек на день выключает исключения.
     */
    var bypassMode: String
        get() = prefs.getString(KEY_BYPASS_MODE, BYPASS_EXCLUDE).let {
            if (it == BYPASS_INCLUDE || it == BYPASS_OFF) it else BYPASS_EXCLUDE
        }
        set(value) {
            prefs.edit().putString(KEY_BYPASS_MODE, value).apply()
        }

    /**
     * dns — кто отвечает на запросы имён. Только адрес, без порта.
     *
     * Запрос в любом случае уходит внутрь туннеля — это решает ядро, а не
     * человек. Выбор здесь только в том, чей резолвер стоит на другом конце:
     * у кого-то из них есть фильтр рекламы, у кого-то — нет. Шифровать ли
     * запрос от ноды — отдельная настройка, dnsSecure. Свой адрес
     * принимается, только если прошёл checkDns: сохранённый раньше мусор
     * откатывается на резолвер по умолчанию, а не ломает имена.
     */
    var dns: String
        get() = prefs.getString(KEY_DNS, null)?.takeIf { it in DNS_CHOICES || checkDns(it) == DnsCheck.OK }
            ?: DNS_CHOICES.first()
        set(value) {
            prefs.edit().putString(KEY_DNS, value).apply()
        }

    /**
     * dnsSecure — шифровать ли запросы имён: к известному резолверу они
     * уходят по HTTPS внутри туннеля, и нода видит только соединение с ним.
     * Включено по умолчанию. Выключенное — открытый DNS через туннель, как
     * раньше: нода видит каждое имя. Свой адрес шифровать нечем при любом
     * значении — ядро не знает, как с ним говорить по HTTPS.
     */
    var dnsSecure: Boolean
        get() = prefs.getBoolean(KEY_DNS_SECURE, true)
        set(value) {
            prefs.edit().putBoolean(KEY_DNS_SECURE, value).apply()
        }

    /**
     * dnsSecureNow — пойдут ли имена по HTTPS при нынешнем выборе резолвера.
     * Какие резолверы шифруются, знает только ядро — спрашиваем его.
     */
    val dnsSecureNow: Boolean
        get() = dnsSecure && Mobile.namesEncrypted(dns)

    /**
     * fragment — резать ли TLS-приветствие к нодам, чтобы имя из SNI не
     * лежало целиком ни в одном пакете. Выключено по умолчанию: без фильтра
     * по имени у провайдера это лишние пакеты и своя примета.
     */
    var fragment: Boolean
        get() = prefs.getBoolean(KEY_FRAGMENT, false)
        set(value) {
            prefs.edit().putBoolean(KEY_FRAGMENT, value).apply()
        }

    /**
     * ipv6 — пускать ли IPv6 через туннель. Выключенный не уходит и мимо:
     * маршрут остаётся в туннеле, ядро отвечает «адресов IPv6 нет», и
     * приложения идут по IPv4.
     */
    var ipv6: Boolean
        get() = prefs.getBoolean(KEY_IPV6, true)
        set(value) {
            prefs.edit().putBoolean(KEY_IPV6, value).apply()
        }

    /** MTU интерфейса Android: уменьшение помогает сетям, где крупные пакеты теряются. */
    var vpnMtu: Int
        get() = prefs.getInt(KEY_VPN_MTU, 1500).takeIf { it in MTU_CHOICES } ?: 1500
        set(value) { prefs.edit().putInt(KEY_VPN_MTU, value.takeIf { it in MTU_CHOICES } ?: 1500).apply() }

    /** Как часто обновлять скорость и счётчики при открытом экране. */
    var liveRefreshSeconds: Int
        get() = prefs.getInt(KEY_LIVE_REFRESH, 2).takeIf { it in LIVE_REFRESH_CHOICES } ?: 2
        set(value) { prefs.edit().putInt(KEY_LIVE_REFRESH, value.takeIf { it in LIVE_REFRESH_CHOICES } ?: 2).apply() }

    /** С погашенным экраном счётчики нужны значительно реже. */
    var idleRefreshSeconds: Int
        get() = prefs.getInt(KEY_IDLE_REFRESH, 30).takeIf { it in IDLE_REFRESH_CHOICES } ?: 30
        set(value) { prefs.edit().putInt(KEY_IDLE_REFRESH, value.takeIf { it in IDLE_REFRESH_CHOICES } ?: 30).apply() }

    /** Интервал фоновой проверки отклика; ноль отключает только повторный замер. */
    var pingIntervalSeconds: Int
        get() = prefs.getInt(KEY_PING_INTERVAL, 30).takeIf { it in PING_INTERVAL_CHOICES } ?: 30
        set(value) { prefs.edit().putInt(KEY_PING_INTERVAL, value.takeIf { it in PING_INTERVAL_CHOICES } ?: 30).apply() }

    /**
     * Последние замеры скорости из «Диагностики», новые первыми. Живут в
     * настройках, а не в ядре: ядро живёт, пока поднят туннель, а «вчера было
     * 80, сегодня 8» нужно помнить между подключениями.
     */
    var speedHistory: List<Diagnostics.SpeedHistory.Record>
        get() = Diagnostics.SpeedHistory.decode(prefs.getString(KEY_SPEED_HISTORY, "").orEmpty())
        set(value) { prefs.edit().putString(KEY_SPEED_HISTORY, Diagnostics.SpeedHistory.encode(value)).apply() }

    /** true помечает VPN лимитной сетью; false наследует признак от исходной сети. */
    var meteredVpn: Boolean
        get() = prefs.getBoolean(KEY_METERED_VPN, false)
        set(value) { prefs.edit().putBoolean(KEY_METERED_VPN, value).apply() }

    /** Давать ли панели продавца результаты замеров нод после подключения. */
    var sendNodeReports: Boolean
        get() = prefs.getBoolean(KEY_SEND_NODE_REPORTS, true)
        set(value) { prefs.edit().putBoolean(KEY_SEND_NODE_REPORTS, value).apply() }

    /** Оставить в обязательном уведомлении статус, скрыв название ноды и скорость. */
    var compactNotification: Boolean
        get() = prefs.getBoolean(KEY_COMPACT_NOTIFICATION, false)
        set(value) { prefs.edit().putBoolean(KEY_COMPACT_NOTIFICATION, value).apply() }

    /**
     * tunnelSettings — настройки туннеля для ядра (Mobile.start и
     * Mobile.measureNodes), одной строкой JSON. Замер нод получает те же:
     * нода, до которой доходит только разрезанное приветствие, без
     * дробления показалась бы мёртвой.
     */
    fun tunnelSettings(): String =
        org.json.JSONObject()
            .put("fragment", fragment)
            .put("no_ipv6", !ipv6)
            .put("disable_reports", !sendNodeReports)
            .put("plain_dns", !dnsSecure)
            .toString()

    /**
     * lanOutside — оставлять ли домашнюю сеть мимо туннеля.
     *
     * Принтер, телевизор, роутер: их адреса частные и за границу не
     * маршрутизируются, внутри туннеля до них не дойти никак. Выключено по
     * умолчанию не из вредности: в чужом Wi-Fi «домашняя сеть» — это чужая
     * сеть, и пускать туда трафик мимо туннеля человек должен сам.
     */
    var lanOutside: Boolean
        get() = prefs.getBoolean(KEY_LAN_OUTSIDE, false)
        set(value) {
            prefs.edit().putBoolean(KEY_LAN_OUTSIDE, value).apply()
        }

    /**
     * bypassRussian — вести ли российские сайты мимо туннеля.
     *
     * Отдельно от списка приложений: приложения человек выбирает сам, а это
     * один переключатель на всю страну. Работает с Android 13, где система
     * научилась исключать маршруты.
     */
    var bypassRussian: Boolean
        get() = prefs.getBoolean(KEY_BYPASS_RU, false)
        set(value) {
            prefs.edit().putBoolean(KEY_BYPASS_RU, value).apply()
        }

    /** Спрашивали ли уже про российские сайты. Второй раз не навязываемся. */
    var bypassAsked: Boolean
        get() = prefs.getBoolean(KEY_BYPASS_ASKED, false)
        set(value) {
            prefs.edit().putBoolean(KEY_BYPASS_ASKED, value).apply()
        }

    /**
     * autoStart — поднимать ли туннель после перезагрузки телефона.
     *
     * Телефон перезагружается сам: от обновления, от разряда, от зависания.
     * Человек замечает это не сразу, а когда открывает нужный сайт, — и
     * винит не телефон, а нас.
     */
    var autoStart: Boolean
        get() = prefs.getBoolean(KEY_AUTOSTART, false)
        set(value) {
            prefs.edit().putBoolean(KEY_AUTOSTART, value).apply()
        }

    /**
     * look — выбранная тема: пресет, акцент, плотность.
     *
     * До этого тем было две — светлая и тёмная, — и выбор лежал под ключом
     * theme числом режима AppCompatDelegate. Тот, кто выбирал светлую, получает
     * «Дневную»: это ближайший пресет, и человек не должен обнаружить, что его
     * выбор молча стёрли обновлением.
     */
    /** hasLook — выбирал ли человек тему хоть раз: свежая установка получает образ из коробки. */
    fun hasLook(): Boolean = prefs.contains(KEY_PRESET) || prefs.contains(KEY_THEME)

    var look: Look.Choice
        get() {
            val d = Look.DEFAULT
            val preset = prefs.getString(KEY_PRESET, null)
                ?: if (prefs.getInt(KEY_THEME, -1) == LEGACY_LIGHT) "daylight" else d.preset
            // Незнакомое или битое значение поле за полем заменит normalize:
            // одна битая плотность не должна сбрасывать ещё и цвет.
            return Look.normalize(
                Look.Choice(
                    preset = preset,
                    accent = prefs.getInt(KEY_ACCENT, 0),
                    kind = prefs.getString(KEY_KIND, null) ?: d.kind,
                    dir = prefs.getString(KEY_DIR, null) ?: d.dir,
                    depth = prefs.getFloat(KEY_DEPTH, d.depth.toFloat()).toDouble(),
                    tint = prefs.getInt(KEY_TINT, 0),
                    radius = prefs.getString(KEY_RADIUS, null) ?: d.radius,
                    density = prefs.getString(KEY_DENSITY, null) ?: d.density,
                    btn = prefs.getString(KEY_BTN, null) ?: d.btn,
                    glow = prefs.getString(KEY_GLOW, null) ?: d.glow,
                    card = prefs.getString(KEY_CARD, null) ?: d.card,
                ),
            )
        }
        set(value) {
            val v = Look.normalize(value)
            prefs.edit()
                .putString(KEY_PRESET, v.preset)
                .putInt(KEY_ACCENT, v.accent)
                .putString(KEY_KIND, v.kind)
                .putString(KEY_DIR, v.dir)
                .putFloat(KEY_DEPTH, v.depth.toFloat())
                .putInt(KEY_TINT, v.tint)
                .putString(KEY_RADIUS, v.radius)
                .putString(KEY_DENSITY, v.density)
                .putString(KEY_BTN, v.btn)
                .putString(KEY_GLOW, v.glow)
                .putString(KEY_CARD, v.card)
                .apply()
        }

    /**
     * Ручки Android: композиция экрана, узор фона, шрифт, значок в шапке.
     *
     * В код темы они не входят намеренно: код общий с панелью и окном, и
     * узор с фотографией-значком там не значат ничего. Живут на телефоне;
     * сохранённые профили запоминают только композицию рядом с кодом цветов.
     */
    // Композиция экрана выбирается явно: плоская палитра сама по себе не
    // должна превращать другую тему в точечную. Общий код цветов не меняется.
    var appearanceStyle: AppearanceStyle
        get() = AppearanceStyle.entries.firstOrNull { it.key == prefs.getString(KEY_APPEARANCE_STYLE, null) }
            ?: AppearanceStyle.CLASSIC
        set(value) { prefs.edit().putString(KEY_APPEARANCE_STYLE, value.key).apply() }

    /**
     * Последний выбранный образ. Его фирменные детали — металл кнопки, жёсткие
     * тени, зерно — не выводятся из палитры: та же тема с другим акцентом
     * остаётся «Хромом», и подстройка не должна стирать то, по чему образ
     * узнают. Пустая строка — образа ещё не выбирали.
     */
    var vibe: String
        get() = prefs.getString(KEY_VIBE, null).orEmpty()
        set(value) { prefs.edit().putString(KEY_VIBE, value).apply() }

    fun profileStyle(slot: Int): AppearanceStyle = AppearanceStyle.entries.firstOrNull {
        it.key == prefs.getString("$KEY_PROFILE_STYLE$slot", null)
    } ?: AppearanceStyle.CLASSIC

    fun saveProfileStyle(slot: Int) { prefs.edit().putString("$KEY_PROFILE_STYLE$slot", appearanceStyle.key).apply() }

    var pattern: String
        get() = prefs.getString(KEY_PATTERN, PATTERN_DOTS)?.takeIf { it in KNOWN_PATTERNS } ?: PATTERN_DOTS
        set(value) { prefs.edit().putString(KEY_PATTERN, value.takeIf { it in KNOWN_PATTERNS } ?: PATTERN_DOTS).apply() }

    var reduceMotion: Boolean
        get() = prefs.getBoolean(KEY_REDUCE_MOTION, false)
        set(value) { prefs.edit().putBoolean(KEY_REDUCE_MOTION, value).apply() }

    /** snakeBest — рекорд в змейке; живёт только на телефоне. */
    var snakeBest: Int
        get() = prefs.getInt(KEY_SNAKE_BEST, 0)
        set(value) { prefs.edit().putInt(KEY_SNAKE_BEST, value).apply() }

    /** customBypass — свои адреса мимо туннеля, как человек их вписал; разбирает CustomRoutes. */
    var customBypass: String
        get() = prefs.getString(KEY_CUSTOM_BYPASS, "").orEmpty()
        set(value) { prefs.edit().putString(KEY_CUSTOM_BYPASS, value.take(20_000)).apply() }

    /**
     * showSystemApps — показывать ли в списке приложений системные. По
     * умолчанию нет: камера, звонилка и сервисы производителя занимают
     * половину списка, а уводить их мимо туннеля почти никому не нужно.
     */
    var showSystemApps: Boolean
        get() = prefs.getBoolean(KEY_SHOW_SYSTEM_APPS, false)
        set(value) { prefs.edit().putBoolean(KEY_SHOW_SYSTEM_APPS, value).apply() }

    /** homeView — вид главной, ключ из HomeLayout; незнакомый читается как главная Marvia. */
    var homeView: String
        get() = HomeLayout.of(prefs.getString(KEY_HOME_VIEW, null).orEmpty()).key
        set(value) { prefs.edit().putString(KEY_HOME_VIEW, HomeLayout.of(value).key).apply() }

    var homeStats: Boolean
        get() = prefs.getBoolean(KEY_HOME_STATS, true)
        set(value) { prefs.edit().putBoolean(KEY_HOME_STATS, value).apply() }

    var font: String
        get() = prefs.getString(KEY_FONT, FONT_ONEST)?.takeIf { it in FONTS } ?: FONT_ONEST
        set(value) { prefs.edit().putString(KEY_FONT, value.takeIf { it in FONTS } ?: FONT_ONEST).apply() }

    /** logo — что в шапке главной: знак Marvia, своя картинка или ничего. */
    var logo: String
        get() = prefs.getString(KEY_LOGO, LOGO_MARVIA)?.takeIf { it in LOGOS } ?: LOGO_MARVIA
        set(value) { prefs.edit().putString(KEY_LOGO, value.takeIf { it in LOGOS } ?: LOGO_MARVIA).apply() }

    private fun logoFile(): File = File(app.filesDir, LOGO_FILE)

    fun hasLogo(): Boolean = logoFile().exists()

    /** saveLogo кладёт свою картинку под значок, уменьшив: в шапке она в 34 dp. */
    fun saveLogo(uri: android.net.Uri): Boolean {
        val bmp = try {
            app.contentResolver.openInputStream(uri)?.use { BitmapFactory.decodeStream(it) }
        } catch (_: Exception) {
            null
        } ?: return false
        val scale = LOGO_MAX_SIDE.toFloat() / maxOf(bmp.width, bmp.height, 1)
        val small = if (scale < 1f) Bitmap.createScaledBitmap(bmp, (bmp.width * scale).toInt().coerceAtLeast(1), (bmp.height * scale).toInt().coerceAtLeast(1), true) else bmp
        return try {
            val tmp = File(app.filesDir, "$LOGO_FILE.tmp")
            tmp.outputStream().use { small.compress(Bitmap.CompressFormat.PNG, 100, it) }
            tmp.renameTo(logoFile())
        } catch (_: Exception) {
            false
        } finally {
            if (small !== bmp) small.recycle()
            bmp.recycle()
        }
    }

    fun logoBitmap(): Bitmap? = try { BitmapFactory.decodeFile(logoFile().absolutePath) } catch (_: Exception) { null }

    /** profiles — три сохранённых вида кодами; пустой слот — null. */
    var profiles: List<String?>
        get() = (0 until 3).map { i ->
            prefs.getString(KEY_PROFILE + i, null)?.takeIf { Look.decode(it) != null }
        }
        set(value) {
            val e = prefs.edit()
            for (i in 0 until 3) {
                val code = value.getOrNull(i)
                if (code == null) e.remove(KEY_PROFILE + i) else e.putString(KEY_PROFILE + i, code)
            }
            e.apply()
        }

    /**
     * language — язык приложения: ru, en или пусто, пока не выбран.
     *
     * Пустота означает первый запуск: тогда первым открывается экран выбора.
     * Сам язык применяет AppCompat и хранит его отдельно; здесь запоминается
     * только факт выбора и то, что показать отмеченным в настройках.
     */
    var language: String
        get() = prefs.getString(KEY_LANGUAGE, "").orEmpty()
        set(value) {
            prefs.edit().putString(KEY_LANGUAGE, value).apply()
        }

    /**
     * Свой фон: фото за интерфейсом. Файл лежит в приватном каталоге
     * приложения и никуда не уходит — как ключ и настройки. Видео нет: его
     * пришлось бы декодировать своим плеером, а это ExoPlayer и почти
     * удвоение пакета ради фона. В код темы фон не входит: код передают, а
     * файл вместе с ним не унесёшь.
     *
     * fit — как вписать: cover (заполнить) или contain (целиком). dim —
     * затемнение в процентах: пелена цветом фона темы поверх снимка, чтобы
     * светлая тема на тёмном фото осталась читаемой.
     */
    var backdropFit: String
        get() = prefs.getString(KEY_BG_FIT, "cover").let { if (it == "contain") it else "cover" }
        set(value) {
            prefs.edit().putString(KEY_BG_FIT, if (value == "contain") "contain" else "cover").apply()
        }

    var backdropDim: Int
        get() = prefs.getInt(KEY_BG_DIM, 55).coerceIn(0, 95)
        set(value) {
            prefs.edit().putInt(KEY_BG_DIM, value.coerceIn(0, 95)).apply()
        }

    /** backdropFrame — как лежит снимок: приближение, середина, поворот. */
    var backdropFrame: BackdropFrame
        get() = BackdropFrame.decode(prefs.getString(KEY_BG_FRAME, null))
        set(value) {
            prefs.edit().putString(KEY_BG_FRAME, value.encode()).apply()
        }

    fun hasBackdrop(): Boolean = backdropFile().exists()

    /** backdropStamp — «версия» файла фона: ноль, если его нет. По ней кэшируют декодированный снимок. */
    fun backdropStamp(): Long {
        val f = backdropFile()
        if (!f.exists()) return 0L
        return f.lastModified().let { if (it == 0L) f.length() else it }
    }

    private fun backdropFile(): File = File(app.filesDir, BACKDROP_FILE)

    /**
     * saveBackdrop берёт выбранный файл и кладёт его к себе, уменьшив до
     * разумного размера. Полноразмерный снимок с камеры — это десятки
     * мегапикселей, которые незачем держать и рисовать под интерфейсом: на
     * фоне телефона больше пары тысяч точек по длинной стороне не видно, а
     * память они съедают всерьёз.
     *
     * Возвращает false, если файл не картинка или не читается: тогда фон
     * просто не меняется, а не роняет приложение.
     */
    fun saveBackdrop(uri: android.net.Uri): Boolean {
        val bounds = BitmapFactory.Options().apply { inJustDecodeBounds = true }
        try {
            app.contentResolver.openInputStream(uri)?.use { BitmapFactory.decodeStream(it, null, bounds) }
        } catch (_: Exception) {
            return false
        }
        if (bounds.outWidth <= 0 || bounds.outHeight <= 0) return false

        val opts = BitmapFactory.Options().apply {
            inSampleSize = sampleSize(bounds.outWidth, bounds.outHeight, BACKDROP_MAX_SIDE)
        }
        val decoded = try {
            app.contentResolver.openInputStream(uri)?.use { BitmapFactory.decodeStream(it, null, opts) }
        } catch (_: Exception) {
            null
        } ?: return false

        // Камера пишет снимок как держала матрицу, а как держали телефон —
        // отдельной пометкой EXIF. BitmapFactory её не читает, и портретное
        // фото ложилось под интерфейс боком. Поворачиваем здесь, один раз,
        // чтобы на диске лежал уже ровный снимок.
        val bmp = upright(decoded, uri)
        // Новый снимок — новый кадр: приближение и сдвиг от прошлого фото к
        // этому отношения не имеют.
        prefs.edit().remove(KEY_BG_FRAME).apply()

        return try {
            // Во временный файл, потом переименование: прерванная запись не
            // оставит наполовину сохранённый фон, который потом не прочитается.
            val tmp = File(app.filesDir, "$BACKDROP_FILE.tmp")
            tmp.outputStream().use { bmp.compress(Bitmap.CompressFormat.JPEG, 88, it) }
            tmp.renameTo(backdropFile())
        } catch (_: Exception) {
            false
        } finally {
            bmp.recycle()
        }
    }

    /** upright поворачивает снимок так, как его держали, по пометке EXIF. */
    private fun upright(bmp: Bitmap, uri: android.net.Uri): Bitmap {
        val orientation = try {
            app.contentResolver.openInputStream(uri)?.use {
                android.media.ExifInterface(it).getAttributeInt(
                    android.media.ExifInterface.TAG_ORIENTATION,
                    android.media.ExifInterface.ORIENTATION_NORMAL,
                )
            } ?: android.media.ExifInterface.ORIENTATION_NORMAL
        } catch (_: Exception) {
            android.media.ExifInterface.ORIENTATION_NORMAL
        }
        val m = android.graphics.Matrix()
        when (orientation) {
            android.media.ExifInterface.ORIENTATION_ROTATE_90 -> m.postRotate(90f)
            android.media.ExifInterface.ORIENTATION_ROTATE_180 -> m.postRotate(180f)
            android.media.ExifInterface.ORIENTATION_ROTATE_270 -> m.postRotate(270f)
            android.media.ExifInterface.ORIENTATION_FLIP_HORIZONTAL -> m.postScale(-1f, 1f)
            android.media.ExifInterface.ORIENTATION_FLIP_VERTICAL -> m.postScale(1f, -1f)
            android.media.ExifInterface.ORIENTATION_TRANSPOSE -> { m.postRotate(90f); m.postScale(-1f, 1f) }
            android.media.ExifInterface.ORIENTATION_TRANSVERSE -> { m.postRotate(270f); m.postScale(-1f, 1f) }
            else -> return bmp
        }
        return try {
            Bitmap.createBitmap(bmp, 0, 0, bmp.width, bmp.height, m, true).also { if (it !== bmp) bmp.recycle() }
        } catch (_: OutOfMemoryError) {
            bmp
        }
    }

    /** backdropBitmap — фон для отрисовки или null, если его нет либо файл битый. */
    fun backdropBitmap(): Bitmap? {
        val f = backdropFile()
        if (!f.exists()) return null
        return try {
            BitmapFactory.decodeFile(f.absolutePath)
        } catch (_: Exception) {
            null
        }
    }

    fun clearBackdrop() {
        backdropFile().delete()
    }

    private fun sampleSize(w: Int, h: Int, max: Int): Int {
        var s = 1
        while (w / (s * 2) >= max || h / (s * 2) >= max) s *= 2
        return s
    }

    /**
     * resetAll стирает всё: подписки с их кэшем, тему с фоном и значком,
     * настройки и учёт трафика. Язык тоже — после сброса приложение
     * начинается с первого экрана, как после установки.
     */
    fun resetAll() {
        for (sub in subscriptions) runCatching { Mobile.forgetSubscription(sub.link, cacheDir()) }
        File(app.filesDir, Mobile.CacheName).delete()
        backdropFile().delete()
        logoFile().delete()
        prefs.edit().clear().apply()
        SubscriptionRefresh.sync(app, false)
    }

    /** Каталог, который приложение отдаёт ядру под кэш подписки. */
    fun cacheDir(): String = app.filesDir.absolutePath

    private fun dropSubscriptionCache(link: String) {
        // Кэш у каждой подписки свой — стираем его; старый общий файл, если
        // остался от прежних версий, — заодно. Не удалилось — не беда: ядро
        // сверит отпечаток и чужой кэш использовать не станет.
        runCatching { Mobile.forgetSubscription(link, cacheDir()) }
        File(app.filesDir, Mobile.CacheName).delete()
    }

    /** DnsCheck — что сказал checkDns про свой резолвер. */
    enum class DnsCheck { OK, BAD, LOCAL }

    companion object {
        private const val EXIT_COUNTRY_MAX_AGE = 30L * 24 * 60 * 60 * 1000
        const val LANG_RU = "ru"
        const val LANG_EN = "en"

        const val BYPASS_EXCLUDE = "exclude"
        const val BYPASS_INCLUDE = "include"
        const val BYPASS_OFF = "off"

        /**
         * Резолверы, из которых выбирают. Первый — по умолчанию, он же
         * Mobile.DefaultDNS без порта. Свой адрес тоже можно, но через
         * checkDns.
         */
        val DNS_CHOICES = listOf("1.1.1.1", "8.8.8.8", "9.9.9.9", "94.140.14.14")
        val MTU_CHOICES = listOf(1280, 1400, 1500)
        val LIVE_REFRESH_CHOICES = listOf(2, 5, 10)
        val IDLE_REFRESH_CHOICES = listOf(10, 30, 60)
        val PING_INTERVAL_CHOICES = listOf(0, 10, 30, 60)

        /**
         * checkDns — годится ли адрес в свои резолверы.
         *
         * Только IPv4 и только публичный. Опечатка в адресе резолвера — это
         * «интернет не работает» без единой подсказки, почему, поэтому
         * проверяем строго: четыре числа, без ведущих нулей (010 в разных
         * местах читают то восьмеричным, то десятичным). Адрес локальной
         * сети отвергается отдельно и с причиной: запрос имени уходит в
         * туннель, и роутер 192.168.1.1 оттуда не виден, а нода к частным
         * адресам не ходит вовсе. IPv6 не берём: при выключенном IPv6 такой
         * резолвер молча перестал бы отвечать.
         */
        fun checkDns(address: String): DnsCheck {
            val parts = address.trim().split('.')
            if (parts.size != 4) return DnsCheck.BAD
            val n = parts.map { p ->
                if (p.isEmpty() || p.length > 3 || !p.all { it in '0'..'9' } || (p.length > 1 && p[0] == '0')) {
                    return DnsCheck.BAD
                }
                p.toInt().takeIf { it <= 255 } ?: return DnsCheck.BAD
            }
            val (a, b) = n[0] to n[1]
            val local = a == 0 || a == 10 || a == 127 || a >= 224 ||
                (a == 100 && b in 64..127) ||
                (a == 169 && b == 254) ||
                (a == 172 && b in 16..31) ||
                (a == 192 && b == 168)
            return if (local) DnsCheck.LOCAL else DnsCheck.OK
        }

        private const val KEY_ACCOUNT_LINK = "account_link"
        private const val KEY_ACCOUNT_SAVED = "account_saved_at"
        private const val KEY_CHOSEN_NODE = "chosen_node"
        private const val KEY_ONBOARDED = "onboarded"
        private const val KEY_LAST_NODE = "last_node"
        private const val KEY_SUBSCRIPTIONS = "subscriptions"

        /**
         * rowOf и subscriptionOf — подписка одной строкой настроек.
         *
         * Пара полей, а не JSON: зависимость ради двух строк не нужна, а
         * перевод строки в имени всё равно не наберёшь.
         */
        fun rowOf(sub: Subscription): String = sub.name + "\n" + sub.link

        fun subscriptionOf(row: String): Subscription? {
            val at = row.indexOf('\n')
            return if (at <= 0) null else Subscription(row.take(at), row.substring(at + 1))
        }

        /** nameFromLink — домен панели из ссылки; порт в имя не тащим. */
        /**
         * nameFromLink — имя подписки, если человек своё не дал.
         *
         * У ссылки чужой ноды имя уже есть — то, что после #, его и берём:
         * «🇳🇱 Amsterdam» говорит больше, чем адрес. У адреса подписки и у
         * ключа Marvia — домен панели: он и отличает продавцов друг от друга.
         */
        fun nameFromLink(link: String, fallback: String): String {
            val clean = link.trim().lineSequence().firstOrNull()?.trim().orEmpty()
            val tag = clean.substringAfter('#', "").let {
                try {
                    java.net.URLDecoder.decode(it.replace("+", "%2B"), "UTF-8").trim()
                } catch (_: Exception) {
                    it.trim()
                }
            }
            if (tag.isNotEmpty() && !clean.startsWith("marvia://")) return tag
            if (clean.startsWith("http://") || clean.startsWith("https://")) {
                return clean.substringAfter("://").substringBefore('/').substringBefore('?').substringBefore(':').ifEmpty { fallback }
            }
            return Regex("@([^/?#]+)").find(clean)?.groupValues?.get(1)?.substringBefore(':')?.removePrefix("[")
                ?: fallback
        }
        private const val KEY_BYPASSED = "bypassed_apps"
        private const val KEY_BYPASS_MODE = "bypass_mode"
        private const val KEY_DNS = "dns"
        private const val KEY_DNS_SECURE = "dns_secure"
        private const val KEY_FRAGMENT = "fragment"
        private const val KEY_IPV6 = "ipv6"
        private const val KEY_VPN_MTU = "vpn_mtu"
        private const val KEY_LIVE_REFRESH = "live_refresh_seconds"
        private const val KEY_IDLE_REFRESH = "idle_refresh_seconds"
        private const val KEY_PING_INTERVAL = "ping_interval_seconds"
        private const val KEY_SPEED_HISTORY = "speed_history"
        private const val KEY_METERED_VPN = "metered_vpn"
        private const val KEY_SEND_NODE_REPORTS = "send_node_reports"
        private const val KEY_COMPACT_NOTIFICATION = "compact_notification"
        private const val KEY_LAN_OUTSIDE = "lan_outside"
        private const val KEY_BYPASS_RU = "bypass_russian"
        private const val KEY_BYPASS_ASKED = "bypass_asked"
        private const val KEY_AUTOSTART = "autostart"
        private const val KEY_THEME = "theme"
        private const val KEY_LANGUAGE = "language"
        private const val KEY_PRESET = "look_preset"
        private const val KEY_ACCENT = "look_accent"
        private const val KEY_DENSITY = "look_density"
        private const val KEY_KIND = "look_kind"
        private const val KEY_DIR = "look_dir"
        private const val KEY_DEPTH = "look_depth"
        private const val KEY_TINT = "look_tint"
        private const val KEY_RADIUS = "look_radius"
        private const val KEY_BTN = "look_btn"
        private const val KEY_GLOW = "look_glow"
        private const val KEY_CARD = "look_card"
        private const val KEY_PROFILE = "look_profile_"
        private const val KEY_BG_FIT = "backdrop_fit"
        private const val KEY_BG_DIM = "backdrop_dim"
        private const val KEY_BG_FRAME = "backdrop_frame"

        /** Имя файла фона в приватном каталоге и потолок его длинной стороны. */
        private const val BACKDROP_FILE = "backdrop.jpg"
        private const val BACKDROP_MAX_SIDE = 2048

        private const val KEY_APPEARANCE_STYLE = "look_appearance_style"
        private const val KEY_VIBE = "look_vibe"
        private const val KEY_PROFILE_STYLE = "look_profile_style_"
        private const val KEY_PATTERN = "look_pattern"
        private const val KEY_REDUCE_MOTION = "reduce_motion"
        private const val KEY_HOME_STATS = "home_stats"
        private const val KEY_HOME_VIEW = "home_view"
        private const val KEY_CUSTOM_BYPASS = "custom_bypass"
        private const val KEY_SNAKE_BEST = "snake_best"
        private const val KEY_SHOW_SYSTEM_APPS = "show_system_apps"
        private const val KEY_FONT = "look_font"
        private const val KEY_LOGO = "look_logo"
        private const val LOGO_FILE = "logo.png"
        private const val LOGO_MAX_SIDE = 256

        const val PATTERN_NONE = "none"
        const val PATTERN_DOTS = "dots"
        val PATTERNS = listOf(PATTERN_NONE, PATTERN_DOTS, "grid", "rings", "lines")

        /**
         * Что можно хранить: узоры из строки выбора и сцены образов. Сцены в
         * строку не попадают — это картины, а не узор, и приходят вместе с
         * образом; тринадцать плиток в строку выбора и не уместились бы.
         */
        val KNOWN_PATTERNS = PATTERNS + Scenes.keys

        const val FONT_ONEST = "onest"
        val FONTS = listOf(FONT_ONEST, "manrope", "geologica")

        const val LOGO_MARVIA = "marvia"
        const val LOGO_CUSTOM = "custom"
        const val LOGO_NONE = "none"
        val LOGOS = listOf(LOGO_MARVIA, LOGO_CUSTOM, LOGO_NONE)

        /** AppCompatDelegate.MODE_NIGHT_NO — так хранилась светлая тема. */
        private const val LEGACY_LIGHT = 1
    }
}
