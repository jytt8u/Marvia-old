package io.marvia.android

import androidx.appcompat.app.AppCompatActivity
import androidx.appcompat.app.AppCompatDelegate
import androidx.core.os.LocaleListCompat
import io.marvia.android.databinding.ScreenLanguageBinding

/**
 * LanguageScreen — какой язык у приложения.
 *
 * Первый экран при первом запуске. Раньше язык брался у системы молча, и
 * человек с турецким телефоном, купивший доступ у русскоязычного продавца,
 * получал английское приложение и русские ошибки ядра, не имея ни одной ручки,
 * чтобы это поменять. Теперь ручка есть — и на первом экране, и в настройках.
 *
 * Надписи на экране двуязычные: пока язык не выбран, экран обязан читаться
 * на обоих. Кнопка «Продолжить» — единственное, что переключается вместе с
 * выбором: так человек видит, что нажатие уже подействовало.
 */
class LanguageScreen(
    private val host: AppCompatActivity,
    private val ui: ScreenLanguageBinding,
    private val store: Store,
    private val theme: () -> Theme,
    /** Язык выбран и применён; экран можно закрывать. */
    private val onDone: () -> Unit,
) {

    private var picked = Store.LANG_RU
    private val dp = host.resources.displayMetrics.density

    /** Карточка языка, её отметка и надпись «Продолжить» на этом языке. */
    private class Choice(val card: android.view.View, val mark: android.view.View, val next: Int)

    private val choices = mapOf(
        Store.LANG_RU to Choice(ui.langRu, ui.langRuMark, R.string.language_continue_ru),
        Store.LANG_EN to Choice(ui.langEn, ui.langEnMark, R.string.language_continue_en),
        Store.LANG_KK to Choice(ui.langKk, ui.langKkMark, R.string.language_continue_kk),
        Store.LANG_UZ to Choice(ui.langUz, ui.langUzMark, R.string.language_continue_uz),
    )

    init {
        for ((lang, choice) in choices) choice.card.setOnClickListener { pick(lang) }
        ui.langContinue.setOnClickListener { apply() }
    }

    /**
     * open показывает экран. Отмечен уже выбранный язык, а до первого выбора —
     * тот, что подсказывает система: русский для русскоязычных настроек и
     * соседних языков, где русский читают, иначе английский.
     */
    fun open() {
        picked = store.language.ifEmpty { guess() }
        paint()
    }

    private fun pick(lang: String) {
        picked = lang
        paint()
    }

    /** paint красит карточки по выбору и теме. */
    fun paint() {
        val t = theme()
        Paint.apply(ui.root, t)
        // Точки шагов: первый — длинный акцентом, второй — тихий.
        ui.stepOne.background = Paint.rounded(t.acc, 999, dp)
        ui.stepTwo.background = Paint.rounded(t.line, 999, dp)

        for ((lang, choice) in choices) {
            val on = lang == picked
            choice.card.background = Paint.card(t, dp, stroke = if (on) t.acc else t.line)
            choice.mark.background = Paint.ring(t, dp, chosen = on)
        }
        ui.langContinue.setText((choices[picked] ?: choices.getValue(Store.LANG_RU)).next)
    }

    /**
     * apply запоминает выбор и переключает язык приложения.
     *
     * Через AppCompat, а не подменой конфигурации руками: на Android 13 и новее
     * это системная настройка «язык приложения», и она видна в настройках
     * телефона, а на старых AppCompat хранит её сам. Если язык и правда
     * сменился, экраны пересоздаются системой и onCreate откроет что нужно;
     * если совпал с прежним — пересоздания не будет. Дальше идём в обоих
     * случаях: лишний показ экрана перед пересозданием безобиден, а угадывать,
     * пересоздаст система или нет, — нет.
     */
    private fun apply() {
        store.language = picked
        AppCompatDelegate.setApplicationLocales(LocaleListCompat.forLanguageTags(picked))
        onDone()
    }

    private fun guess(): String {
        val system = host.resources.configuration.locales[0].language
        return when (system) {
            Store.LANG_KK, Store.LANG_UZ -> system
            in RUSSIAN_READERS -> Store.LANG_RU
            else -> Store.LANG_EN
        }
    }

    companion object {
        /** label — имя языка для строки «Язык» в настройках. */
        fun label(lang: String): Int = when (lang) {
            Store.LANG_EN -> R.string.language_en
            Store.LANG_KK -> R.string.language_kk
            Store.LANG_UZ -> R.string.language_uz
            else -> R.string.language_ru
        }

        /** Те же языки, с которыми окно на компьютере подсказывает русский. */
        private val RUSSIAN_READERS = setOf("ru", "be", "uk", "kk", "ky", "uz", "tg", "hy", "az")
    }
}
