package io.marvia.android

import android.app.Application
import android.content.Context
import android.content.res.Configuration
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test
import org.junit.runner.RunWith
import org.robolectric.RobolectricTestRunner
import org.robolectric.RuntimeEnvironment
import org.robolectric.annotation.Config
import java.util.Locale

/**
 * «Диагностика»: коды шагов и кнопок приходят из ядра, слова — отсюда. Тест
 * ядра (mobile/diag_test.go) сверяет, что строки есть в обоих словарях; этот
 * — что экран действительно находит их на обоих языках и что отчёт продавцу
 * не несёт адреса.
 */
@RunWith(RobolectricTestRunner::class)
@Config(application = Application::class, sdk = [35])
class DiagnosticsTest {

    /** Те же коды, что в mobile/diag.go; расхождение ловит тест ядра. */
    private val steps = listOf("net", "dns", "node", "tunnel", "where", "leak", "sites")
    private val acts = listOf("connect", "reconnect", "nodes", "fragment", "dns")

    @Test fun everyStepAndButtonSpeaksBothLanguages() {
        for (lang in listOf("ru", "en")) {
            val ctx = localized(lang)
            for (id in steps) {
                val title = Diagnostics.title(id)
                assertNotNull("у шага $id нет названия", title)
                assertTrue("$lang: пустое название шага $id", ctx.getString(title!!).isNotBlank())
                for (st in listOf("warn", "bad")) {
                    Diagnostics.say(Diagnostics.Step(id, st))?.let {
                        assertTrue("$lang: пустые слова для $id/$st", ctx.getString(it).isNotBlank())
                    }
                }
            }
            for (code in acts) {
                val label = Diagnostics.act(code)
                assertNotNull("у кнопки $code нет подписи", label)
                assertTrue("$lang: пустая подпись кнопки $code", ctx.getString(label!!).isNotBlank())
            }
        }
        assertFalse(
            "русский и английский совпали — перевода нет",
            localized("ru").getString(R.string.diag_step_net) == localized("en").getString(R.string.diag_step_net),
        )
    }

    @Test fun aCodeTheAppDoesNotKnowGetsNeitherWordsNorAButton() {
        assertNull(Diagnostics.title("system"))
        assertNull(Diagnostics.act("fix"))
        val step = Diagnostics.step("""{"id":"node","st":"warn","n":2,"of":3,"ms":40,"act":["nodes","fix"]}""")!!
        assertEquals(listOf("nodes", "fix"), step.act)
        assertEquals(listOf("nodes"), step.act.filter { Diagnostics.act(it) != null })
    }

    @Test fun stepsThatNeedTheTunnelSayWhyTheyWereSkipped() {
        val ctx = localized("ru")
        for (id in Diagnostics.NEED_TUNNEL) {
            assertEquals(ctx.getString(R.string.diag_need_up), Diagnostics.value(ctx, Diagnostics.Step(id, "off")))
        }
    }

    @Test fun theReportForTheSellerCarriesTheCountryButNotTheExitAddress() {
        val ctx = localized("ru")
        val snapshot = Diagnostics.Snapshot(
            connected = true,
            server = "Финляндия · Хельсинки · fi-1",
            where = Diagnostics.Where("198.51.100.1", "fi"),
            quality = Diagnostics.Quality(42, 3, 0, 12),
            route = Diagnostics.Route("vp1", "reality", "", "www.example.com", 2, "chrome"),
            steps = listOf(
                Diagnostics.Step("where", "ok", ip = "198.51.100.1", country = "fi"),
                Diagnostics.Step("sites", "warn", n = 7, of = 8, act = listOf("nodes")),
            ),
            speed = Diagnostics.SpeedHistory.Record(1_700_000_000_000, 48.2, "fi-1"),
        )
        val text = Diagnostics.report(ctx, snapshot, "журнал", "dev") { it }
        assertFalse("в отчёте адрес выхода: $text", text.contains("198.51.100.1"))
        assertTrue("в отчёте нет страны: $text", text.contains(Diagnostics.countryName(ctx, "fi")))
        assertTrue("в отчёте нет беды шага: $text", text.contains(ctx.getString(R.string.diag_sites_warn)))
        assertTrue("журнал не дописан в отчёт", text.endsWith("журнал"))
    }

    @Test fun speedHistoryKeepsTheLastFiveNewestFirstAcrossRestarts() {
        var history = emptyList<Diagnostics.SpeedHistory.Record>()
        for (i in 1..7) {
            history = Diagnostics.SpeedHistory.add(history, Diagnostics.SpeedHistory.Record(i.toLong(), i * 10.0, "нода\t$i"))
        }
        val restored = Diagnostics.SpeedHistory.decode(Diagnostics.SpeedHistory.encode(history))
        assertEquals(Diagnostics.SpeedHistory.KEEP, restored.size)
        assertEquals(listOf(7L, 6L, 5L, 4L, 3L), restored.map { it.at })
        assertEquals("нода 7", restored.first().node)
        assertTrue("мусор в настройках принят за замер", Diagnostics.SpeedHistory.decode("x\ty\n1\t-5\tн").isEmpty())
    }

    private fun localized(lang: String): Context {
        val base = RuntimeEnvironment.getApplication()
        val config = Configuration(base.resources.configuration).apply { setLocale(Locale.forLanguageTag(lang)) }
        return base.createConfigurationContext(config)
    }
}
