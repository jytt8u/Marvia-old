package io.marvia.android

import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import java.util.concurrent.ConcurrentHashMap
import io.marvia.mobile.Tunnel as Core

/** Что сейчас с туннелем. */
sealed interface TunnelState {

    /** Туннеля нет, трафик идёт напрямую. */
    data object Off : TunnelState

    /** Идёт замер нод и подключение. */
    data object Connecting : TunnelState

    /**
     * Туннель поднят.
     *
     * warning — последняя ошибка отдельного соединения. Туннель от неё не
     * падает: одна недоступная цель это норма. Но показать её стоит, иначе
     * человек видит только «подключено» при наполовину живой ноде.
     */
    data class On(
        val node: String,
        val warning: String = "",
        /** Подписка: до какого числа и сколько трафика осталось. */
        val subscription: Subscription = Subscription(),
        /** Время отклика текущей ноды. Ноль означает, что её не мерили. */
        val ms: Long = 0,
        /**
         * chosen — нода, которую человек выбрал руками. Пусто значит автовыбор.
         *
         * Хранится отдельно от [node] намеренно. Выбранная нода могла
         * замолчать, и ядро уехало на живую: тогда эти два имени разные, и
         * показать надо оба. Одно «выбрана ОАЭ» при трафике через Финляндию —
         * это неправда, которую человеку нечем проверить.
         */
        val chosen: String = "",
    ) : TunnelState

    /**
     * Subscription — то, за что человек заплатил.
     *
     * Без этого на вопрос «сколько у меня осталось» отвечает продавец —
     * каждому и вручную. Пустые значения означают «без ограничения»: так
     * бывает, когда продавец не поставил ни срока, ни квоты.
     */
    data class Subscription(
        val until: String = "",
        val limitBytes: Long = 0,
        val leftBytes: Long = 0,
        /**
         * Версия новее нашей, которую выложила панель продавца, и откуда её
         * взять. Пусто — обновляться не на что. Ссылка ведёт на домен
         * продавца: магазин в России может не открыться вовсе.
         */
        val updateVersion: String = "",
        val updateUrl: String = "",
    ) {
        val known: Boolean get() = until.isNotEmpty() || limitBytes > 0
    }

    /**
     * Подняться не удалось.
     *
     * kind — вид неудачи, названный ядром. По нему подбирается фраза, которую
     * покупатель поймёт. Пустой вид означает, что причину сформулировало само
     * приложение и подбирать под неё нечего.
     *
     * detail — подробности из ядра как есть. Покупателю они не говорят ничего,
     * но именно они нужны продавцу, когда покупатель присылает ему снимок
     * экрана со словами «не работает».
     */
    data class Failed(val kind: String, val detail: String) : TunnelState
}

/**
 * MarviaState — единственное место, где живёт состояние туннеля.
 *
 * Служба и экран работают в одном процессе, поэтому широковещательные
 * сообщения между ними были бы лишним слоем: один пишет, другой читает.
 * Экран может быть закрыт и открыт заново — состояние переживает это, потому
 * что принадлежит процессу, а не экрану.
 */
object MarviaState {
    val traffic = MutableStateFlow(TrafficSnapshot())
    /** Changes only when an exit-IP country lookup succeeds. */
    val countryDetected = MutableStateFlow(0L)
    private val current = MutableStateFlow<TunnelState>(TunnelState.Off)

    val state: StateFlow<TunnelState> = current.asStateFlow()

    /**
     * serviceAlive — существует ли объект службы VPN. «Переподключиться»
     * ждёт, пока прежний разрушится: запуск, пришедший раньше его onDestroy,
     * достался бы уже остановленной службе, и onDestroy оборвал бы новое
     * подключение на полуслове.
     */
    val serviceAlive = MutableStateFlow(false)

    fun set(next: TunnelState) {
        current.value = next
        if (next !is TunnelState.On) traffic.value = traffic.value.copy(seconds = 0, bytesPerSecond = 0.0)
    }

    @Volatile
    private var live: Core? = null

    /**
     * core — поднятое ядро, пока туннель работает.
     *
     * Экрану выбора страны нужно спросить у ядра список нод и попросить
     * переключиться, а ядро держит служба. Служба и экран живут в одном
     * процессе, поэтому передавать его сообщениями было бы лишним слоем.
     *
     * null означает, что туннеля нет: спрашивать не у кого, и экран стран
     * честно говорит, что список появится после подключения.
     */
    val core: Core? get() = live

    /** hold зовёт служба: она одна знает, когда ядро появилось и пропало. */
    fun hold(next: Core?) {
        live = next
        if (next == null) {
            // Замеры принадлежат прошлому туннелю. У следующего может быть
            // другой продавец и другие ноды с теми же номерами.
            measured.clear()
        }
    }

    /**
     * Последний замер каждой ноды.
     *
     * Ядро замеры не хранит: nodes() отдаёт нули, время появляется только в
     * ответе measure(). Без этой памяти список стран показывал бы прочерки
     * сразу после того, как человек нажал «Обновить» и увидел числа, — а
     * главный экран терял бы «42 мс» через пять секунд после подключения.
     */
    private val measured = ConcurrentHashMap<Long, Ping>()

    /** Что мы знаем о ноде по последнему замеру. */
    data class Ping(val ms: Long, val alive: Boolean, val setupMs: Long = 0)

    fun remember(rows: List<NodeRow>) {
        for (row in rows) {
            measured[row.id] = Ping(row.ms, row.alive, row.setupMs)
        }
    }

    fun ping(id: Long): Ping? = measured[id]

    /** Мерили ли вообще хоть что-то с этого подключения. */
    fun anyMeasured(): Boolean = measured.isNotEmpty()
}
