package io.marvia.android

import android.Manifest
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.content.ActivityNotFoundException
import android.content.Context
import android.content.Intent
import android.content.pm.PackageManager
import android.net.Uri
import android.os.Build
import android.widget.Toast
import androidx.core.app.NotificationCompat

/**
 * SellerChannel — связь покупателя с продавцом: «Продлить», «Поддержка» и
 * напоминание о конце доступа.
 *
 * Кончившийся без предупреждения доступ выглядит поломкой, и человек идёт
 * не платить, а жаловаться. Ссылки и тексты приходят из подписки — своей или
 * чужой панели; ядро их уже проверило (только https и tg, текст без
 * управляющих символов), здесь — последняя проверка перед тем, как отдать
 * ссылку системе.
 */
object SellerChannel {
    private const val CHANNEL = "seller"

    /** Сколько последних напоминаний помнить на подписку: их ключи короткие и меняются редко. */
    private const val KEEP = 8

    /**
     * openable — можно ли отдать ссылку системе. Только https и tg: intent://,
     * file:// или market:// открыли бы не страницу продавца, а что-то в
     * самом телефоне.
     */
    fun openable(url: String): Boolean {
        val lower = url.lowercase()
        return (lower.startsWith("https://") && lower.length > "https://".length) ||
            (lower.startsWith("tg://") && lower.length > "tg://".length)
    }

    /** open открывает ссылку продавца; Telegram не установлен — говорим об этом, а не молчим. */
    fun open(context: Context, url: String) {
        if (!openable(url)) return
        try {
            context.startActivity(Intent(Intent.ACTION_VIEW, Uri.parse(url)).addFlags(Intent.FLAG_ACTIVITY_NEW_TASK))
        } catch (_: ActivityNotFoundException) {
            Toast.makeText(context, R.string.seller_no_app, Toast.LENGTH_LONG).show()
        }
    }

    /** firstTime запоминает ключ и говорит, видим ли мы его впервые. Пустой ключ — напоминать не о чем. */
    fun firstTime(seen: MutableSet<String>, key: String): Boolean = key.isNotEmpty() && seen.add(key)

    /**
     * remind показывает напоминание, если его ещё не показывали.
     *
     * Ключ ядро строит так, что «осталось 3 дня» и «осталось 2» — одно
     * напоминание, а после продления — новое: человека не дёргают каждый
     * день, но и следующий срок не пропускают.
     */
    // Экран и фоновая работа могут получить один ответ одновременно:
    // проверка ключа и его сохранение должны оставаться одним действием.
    @Synchronized
    fun remind(context: Context, store: Store, subName: String, link: String, kind: String, value: Long, key: String, renewUrl: String) {
        val seen = LinkedHashSet(store.reminded(link))
        if (!firstTime(seen, key)) return
        val text = when (kind) {
            "expiry" -> context.getString(
                R.string.seller_remind_expiry,
                context.resources.getQuantityString(R.plurals.days_left, value.toInt(), value.toInt()),
            )
            "traffic" -> context.getString(R.string.seller_remind_traffic, value.toInt())
            // Прогноз ядра: при нынешнем расходе трафик кончится раньше срока.
            "forecast" -> context.getString(
                R.string.seller_remind_forecast,
                context.resources.getQuantityString(R.plurals.days_left, value.toInt(), value.toInt()),
            )
            else -> return
        }
        show(context, store, subName, link, seen, key, text, renewUrl)
    }

    /**
     * announce сообщает объявление продавца — «работы ночью», «новая нода» —
     * уведомлением, а не только строкой на экране серверов: туда заходят,
     * когда уже что-то не работает. Ключ — сам текст: то же объявление
     * второй раз не приходит, новое — приходит.
     */
    @Synchronized
    fun announce(context: Context, store: Store, subName: String, link: String, text: String) {
        if (text.isBlank()) return
        val seen = LinkedHashSet(store.reminded(link))
        val key = "announce:" + Integer.toHexString(text.hashCode())
        if (!firstTime(seen, key)) return
        // Нажатие ведёт в приложение: у объявления нет «продлить».
        show(context, store, subName, link, seen, key, text, "")
    }

    private fun show(context: Context, store: Store, subName: String, link: String, seen: Set<String>, key: String, text: String, renewUrl: String) {
        // Без разрешения на уведомления не показываем и ключ не тратим:
        // разрешат позже — напоминание придёт при следующем обновлении.
        if (Build.VERSION.SDK_INT >= 33 &&
            context.checkSelfPermission(Manifest.permission.POST_NOTIFICATIONS) != PackageManager.PERMISSION_GRANTED
        ) return
        store.setReminded(link, seen.toList().takeLast(KEEP))

        val manager = context.getSystemService(NotificationManager::class.java)
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
            manager.createNotificationChannel(
                NotificationChannel(CHANNEL, context.getString(R.string.seller_channel), NotificationManager.IMPORTANCE_DEFAULT).apply {
                    description = context.getString(R.string.seller_channel_desc)
                },
            )
        }
        // Нажатие ведёт туда, где продлевают; нет такой ссылки — в приложение.
        val target = if (openable(renewUrl)) Intent(Intent.ACTION_VIEW, Uri.parse(renewUrl))
        else Intent(context, MainActivity::class.java)
        val tap = PendingIntent.getActivity(
            context, key.hashCode(), target,
            PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT,
        )
        val n = NotificationCompat.Builder(context, CHANNEL)
            .setSmallIcon(R.drawable.ic_stat_marvia)
            .setContentTitle(subName)
            .setContentText(text)
            .setStyle(NotificationCompat.BigTextStyle().bigText(text))
            .setContentIntent(tap)
            .setAutoCancel(true)
            .build()
        // Объявление — отдельным уведомлением: иначе оно затирало бы
        // напоминание о сроке той же подписки.
        val slot = if (key.startsWith("announce:")) 1 else 0
        manager.notify((CHANNEL.hashCode() xor link.hashCode()) + slot, n)
    }
}
