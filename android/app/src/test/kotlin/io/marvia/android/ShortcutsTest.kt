package io.marvia.android

import org.junit.Assert.assertTrue
import org.junit.Test
import java.io.File

/**
 * Ярлык на значке — обещание «нажми, и подключится». Строка действия живёт
 * в двух местах, в разметке ярлыков и в окне; разойдутся — ярлык откроет
 * приложение и ничего не сделает.
 */
class ShortcutsTest {
    @Test
    fun everyShortcutActionIsHandledByTheWindow() {
        val shortcuts = read("src/main/res/xml/shortcuts.xml")
        val window = read("src/main/kotlin/io/marvia/android/MainActivity.kt")
        val actions = Regex("""android:action="([^"]+)"""").findAll(shortcuts).map { it.groupValues[1] }.toList()
        assertTrue("в ярлыках нет ни одного действия", actions.isNotEmpty())
        for (action in actions) {
            assertTrue("ярлык $action окно не разбирает", window.contains("\"$action\""))
        }
        assertTrue("разметка ярлыков не подключена к значку", read("src/main/AndroidManifest.xml").contains("@xml/shortcuts"))
    }

    private fun read(path: String): String {
        var dir: File? = File(System.getProperty("user.dir").orEmpty()).absoluteFile
        while (dir != null) {
            val file = File(dir, path)
            if (file.isFile) return file.readText()
            dir = dir.parentFile
        }
        throw AssertionError("не нашёл $path от " + System.getProperty("user.dir"))
    }
}
