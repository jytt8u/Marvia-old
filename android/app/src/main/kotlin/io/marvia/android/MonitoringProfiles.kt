package io.marvia.android

/** Частота измерений не меняет восстановление связи или маршруты. */
object MonitoringProfiles {
    data class Profile(val label: Int, val live: Int, val idle: Int, val ping: Int)

    val all = listOf(
        Profile(R.string.advanced_preset_battery, 10, 60, 60),
        Profile(R.string.advanced_preset_balanced, 2, 30, 30),
        Profile(R.string.advanced_preset_detailed, 2, 10, 10),
    )

    fun selected(store: Store): Int = all.indexOfFirst {
        it.live == store.liveRefreshSeconds && it.idle == store.idleRefreshSeconds && it.ping == store.pingIntervalSeconds
    }

    /**
     * Сколько ждать до следующего опроса и как часто мерить отклик сейчас.
     *
     * Включено системное энергосбережение — человек сам сказал, что батарея
     * важнее. Тогда опрашиваем не чаще профиля «Батарея», что бы ни было
     * выбрано: счётчики и уведомление чуть отстают, туннель работает как
     * был. Ноль у отклика — «не мерить» — так и остаётся нулём.
     */
    fun pace(live: Int, idle: Int, ping: Int, interactive: Boolean, saver: Boolean): Pair<Int, Int> {
        val refresh = if (interactive) live else idle
        if (!saver) return refresh to ping
        val battery = all[0]
        val floor = if (interactive) battery.live else battery.idle
        return maxOf(refresh, floor) to (if (ping == 0) 0 else maxOf(ping, battery.ping))
    }

    fun apply(store: Store, index: Int) {
        val profile = all[index]
        store.liveRefreshSeconds = profile.live
        store.idleRefreshSeconds = profile.idle
        store.pingIntervalSeconds = profile.ping
    }
}
