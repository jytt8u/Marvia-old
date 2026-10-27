package io.marvia.android

import android.content.Context
import android.os.PowerManager
import androidx.work.Constraints
import androidx.work.CoroutineWorker
import androidx.work.ExistingPeriodicWorkPolicy
import androidx.work.NetworkType
import androidx.work.PeriodicWorkRequest
import androidx.work.WorkManager
import androidx.work.WorkerParameters
import io.marvia.mobile.Mobile
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.ensureActive
import kotlinx.coroutines.withContext
import org.json.JSONObject
import java.util.concurrent.TimeUnit

/**
 * Расписание принадлежит приложению, а не экрану: WorkManager сохраняет его
 * после закрытия процесса и перезагрузки. UPDATE не отодвигает следующий
 * запуск при каждом открытии приложения. В самой работе ключей нет — их
 * читаем из Store на момент запуска, чтобы удалённый ключ не жил в очереди.
 */
object SubscriptionRefresh {
    const val WORK_NAME = "subscription_refresh"

    fun request(): PeriodicWorkRequest = PeriodicWorkRequest.Builder(
        SubscriptionRefreshWorker::class.java, 12, TimeUnit.HOURS, 6, TimeUnit.HOURS,
    ).setInitialDelay(6, TimeUnit.HOURS)
        .setConstraints(Constraints.Builder()
            .setRequiredNetworkType(NetworkType.CONNECTED)
            .setRequiresBatteryNotLow(true)
            .build())
        .build()

    fun sync(context: Context, hasSubscriptions: Boolean) {
        val manager = WorkManager.getInstance(context)
        if (hasSubscriptions) {
            manager.enqueueUniquePeriodicWork(WORK_NAME, ExistingPeriodicWorkPolicy.UPDATE, request())
        } else {
            manager.cancelUniqueWork(WORK_NAME)
        }
    }
}

class SubscriptionRefreshWorker(context: Context, params: WorkerParameters) : CoroutineWorker(context, params) {
    override suspend fun doWork(): Result = withContext(Dispatchers.IO) {
        // Не просим исключения из экономии ради фонового запроса. Кроме
        // ограничений WorkManager учитываем явно включённую экономию питания.
        if (applicationContext.getSystemService(PowerManager::class.java).isPowerSaveMode) return@withContext Result.retry()
        val store = Store(applicationContext)
        var retry = false
        for (sub in store.subscriptions) {
            ensureActive()
            try {
                val core = MarviaState.core
                val raw = if (core == null) Mobile.subscription(sub.link, store.cacheDir(), true)
                    else core.subscription(sub.link, store.cacheDir(), true)
                // За время запроса человек мог удалить ключ: напоминать о нём
                // уже нельзя, а ядро могло успеть записать кэш обратно.
                if (store.subscriptions.none { it.link == sub.link }) {
                    Mobile.forgetSubscription(sub.link, store.cacheDir())
                    continue
                }
                ensureActive()
                val view = JSONObject(raw)
                SellerChannel.remind(
                    applicationContext, store, sub.name, sub.link,
                    view.optString("remind"), view.optLong("remind_value"),
                    view.optString("remind_key"), view.optString("renew_url"),
                )
                SellerChannel.announce(applicationContext, store, sub.name, sub.link, view.optString("announce"))
                // При недоступной панели ядро возвращает старый кэш, включая
                // рассчитанное на сегодня напоминание. Это полезный результат,
                // но список всё равно нужно обновить после восстановления сети.
                retry = retry || view.optBoolean("stale")
            } catch (cancelled: CancellationException) {
                throw cancelled
            } catch (_: Exception) {
                retry = true
            }
        }
        if (retry) Result.retry() else Result.success()
    }
}
