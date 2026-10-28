package io.marvia.android

import org.junit.Assert.assertTrue
import org.junit.Test
import java.io.File

/**
 * Перевод с потерянным или лишним %1$s падает не на сборке, а у человека
 * в руках: getString бросает исключение на том экране, где строку впервые
 * покажут. Поэтому сверяем подстановки каждого перевода с английским.
 *
 * Полноту не требуем: строка, которой нет в переводе, честно покажется
 * по-английски, а новая строка не должна ждать трёх переводчиков.
 */
class TranslationsTest {
    private val entry = Regex("""<(string|item)(?:\s+(?:name|quantity)="([^"]+)")?[^>]*>([\s\S]*?)</\1>""")
    private val format = Regex("""%(\d+\$)?[-#+ 0,(]*\d*(\.\d+)?[sdfxX]""")

    @Test
    fun everyTranslationKeepsTheSamePlaceholders() {
        val res = find("src/main/res")
        val base = strings(File(res, "values"))
        val locales = res.listFiles { f -> f.isDirectory && f.name.matches(Regex("values-[a-z]{2}")) }.orEmpty()
        assertTrue("переводов не нашлось", locales.isNotEmpty())
        val wrong = mutableListOf<String>()
        for (dir in locales) {
            for ((name, text) in strings(dir)) {
                val en = base[name] ?: base[name.substringBefore('#') + "#other"] ?: continue
                if (placeholders(en) != placeholders(text)) wrong += "${dir.name}/$name: «$text»"
            }
        }
        assertTrue("подстановки не совпадают с английскими:\n" + wrong.joinToString("\n"), wrong.isEmpty())
    }

    /** Строки каталога: имя → текст; у plurals — имя#количество. */
    private fun strings(dir: File): Map<String, String> {
        val out = mutableMapOf<String, String>()
        for (file in dir.listFiles { f -> f.extension == "xml" }.orEmpty()) {
            val xml = file.readText().replace(Regex("<!--[\\s\\S]*?-->"), "")
            for (block in Regex("""<(string|plurals)\s+name="([^"]+)"[^>]*>([\s\S]*?)</\1>""").findAll(xml)) {
                val (kind, name, body) = block.destructured
                if (kind == "string") out[name] = body
                else for (item in entry.findAll(body)) out[name + "#" + item.groupValues[2]] = item.groupValues[3]
            }
        }
        return out
    }

    private fun placeholders(text: String) = format.findAll(text).map { it.value }.sorted().toList()

    private fun find(path: String): File {
        var dir: File? = File(System.getProperty("user.dir").orEmpty()).absoluteFile
        while (dir != null) {
            val file = File(dir, path)
            if (file.isDirectory) return file
            dir = dir.parentFile
        }
        throw AssertionError("не нашёл $path от " + System.getProperty("user.dir"))
    }
}
