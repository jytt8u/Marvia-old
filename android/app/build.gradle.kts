import java.util.Properties

plugins {
    id("com.android.application")
}

// Ключ подписи живёт вне репозитория и не попадает в него никогда.
//
// Подпись — это личность приложения на всю жизнь: телефон принимает обновление
// только за той же подписью, что и установленную версию. Потеряешь ключ — и
// обновить приложение у покупателей будет нечем, придётся просить их удалить
// старое и поставить новое как чужое. Утечёт — кто угодно выпустит «обновление»
// от твоего имени, и телефоны его примут.
//
// Путь задаётся переменной MARVIA_RELEASE_KEYS — той же, что читают
// publish-apk.ps1 и руководство. По умолчанию — каталог veil-keys рядом с
// репозиторием: сам ключ при переименовании не менялся и не мог, он один на
// всю жизнь приложения. Файла нет — сборка release выйдет неподписанной и
// честно об этом скажет, вместо того чтобы молча подписаться отладочным
// ключом.
val releaseKeysFile = file(
    System.getenv("MARVIA_RELEASE_KEYS")
        ?: rootProject.file("../../veil-keys/veil-release.properties").path,
)

val releaseKeys = Properties().apply {
    val ciStore = System.getenv("MARVIA_KEYSTORE")
    if (!ciStore.isNullOrEmpty()) {
        // Сборка выпуска на GitHub: хранилище ключа расшифровано из секрета во
        // временный файл, пароли приходят переменными. Файл свойств с
        // паролями там не пишем вовсе — меньше мест, где они лежат на диске.
        setProperty("storeFile", ciStore)
        setProperty("storePassword", System.getenv("MARVIA_KEYSTORE_PASSWORD").orEmpty())
        setProperty("keyAlias", System.getenv("MARVIA_KEY_ALIAS").orEmpty())
        setProperty("keyPassword", System.getenv("MARVIA_KEY_PASSWORD").orEmpty())
    } else if (releaseKeysFile.exists()) {
        releaseKeysFile.inputStream().use { load(it) }
    } else {
        logger.warn("ключ подписи не найден: $releaseKeysFile — сборка release будет неподписанной")
    }
}

// Версия — из метки релиза, а не из этого файла.
//
// publish-apk.ps1 кладёт метку в MARVIA_VERSION (v0.10.0 → 0.10.0), и та же
// строка уезжает в панель файлом .version: по ней приложение у покупателя
// узнаёт, что устарело. Две версии в двух местах разошлись бы в первый же
// релиз. versionCode Play требует строго растущим на каждую загрузку —
// Для возврата к номеру alpha после 1.x или повторных кандидатов задаём
// MARVIA_VERSION_CODE отдельно: меньший versionName не должен мешать обновлению.
// Без переменной — dev и код 1: сборка разработчика, обновлений не ждёт.
//
// То же можно передать свойствами Gradle -PmarviaVersion и
// -PmarviaVersionCode: сборщик F-Droid задаёт их так (gradleprops), а
// переменных окружения в его рецепте нет (docs/fdroid.md).
val appVersion: String = (System.getenv("MARVIA_VERSION") ?: providers.gradleProperty("marviaVersion").orNull)?.trim()?.removePrefix("v")?.takeIf { it.isNotEmpty() } ?: "dev"
val explicitVersionCode = (System.getenv("MARVIA_VERSION_CODE") ?: providers.gradleProperty("marviaVersionCode").orNull)?.let { raw ->
    requireNotNull(raw.toIntOrNull()?.takeIf { it in 1..2100000000 }) {
        "MARVIA_VERSION_CODE должен быть целым числом от 1 до 2100000000"
    }
}
val appVersionCode: Int = explicitVersionCode ?: appVersion.split("-")[0].split(".").let { parts ->
    val nums = parts.map { it.toIntOrNull() }
    if (nums.size in 1..3 && nums.all { it != null && it in 0..99 }) {
        val n = nums.map { it!! } + List(3 - nums.size) { 0 }
        n[0] * 10000 + n[1] * 100 + n[2]
    } else {
        1
    }
}

android {
    namespace = "io.marvia.android"
    compileSdk = 36

    defaultConfig {
        applicationId = "io.marvia.android"
        minSdk = 24
        targetSdk = 36
        versionName = appVersion
        versionCode = appVersionCode

        // Только ARM. На x86 работают эмуляторы и несколько редких планшетов,
        // ради которых пакет вырос бы в полтора раза: ядро на Go весит около
        // двадцати мегабайт на каждую архитектуру.
        ndk {
            abiFilters += listOf("arm64-v8a", "armeabi-v7a")
        }
    }

    signingConfigs {
        if (!releaseKeys.isEmpty) {
            create("release") {
                storeFile = file(releaseKeys.getProperty("storeFile"))
                storePassword = releaseKeys.getProperty("storePassword")
                keyAlias = releaseKeys.getProperty("keyAlias")
                keyPassword = releaseKeys.getProperty("keyPassword")

                // v3 включается явно. Она единственная позволяет однажды
                // сменить ключ подписи, не теряя установленные приложения, —
                // а ключ живёт тридцать лет, и за это время всякое бывает.
                // v1 не нужна: это подпись для Android 6 и старше, а мы
                // начинаем с седьмого.
                enableV1Signing = false
                enableV2Signing = true
                enableV3Signing = true
            }
        }
    }

    buildTypes {
        debug {
            // Отладочная сборка умеет ещё и x86_64 — ради эмулятора. Ядро на
            // Go под ARM в трансляторе эмулятора падает с SIGILL, так что без
            // своей библиотеки на x86_64 приложение на нём не проверить
            // вовсе. В релиз это не попадает: там ARM и только ARM. Чтобы
            // библиотека появилась, ядро собирают с -Emulator (scripts/android.ps1);
            // без неё фильтр просто ничего не находит и пакет остаётся ARM.
            ndk {
                abiFilters += "x86_64"
            }
        }

        release {
            signingConfig = signingConfigs.findByName("release")

            // Сжатие кода выключено намеренно, и это не забывчивость.
            //
            // Выигрыш от него здесь почти нулевой: из шестнадцати мегабайт
            // пакета пятнадцать — это ядро на Go, скомпилированное в машинный
            // код, до которого R8 не дотягивается вовсе. Сжать он может от силы
            // сотню-другую килобайт байткода.
            //
            // А риск настоящий: классы моста вызываются через JNI, статический
            // анализ обращений к ним не видит и выбрасывает их как ненужные.
            // Правила в proguard-rules.pro это чинят, но ошибка в них
            // проявляется не на сборке, а падением уже у покупателя.
            isMinifyEnabled = false
            proguardFiles(
                getDefaultProguardFile("proguard-android-optimize.txt"),
                "proguard-rules.pro",
            )
        }
    }

    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }

    buildFeatures {
        viewBinding = true
    }

    testOptions {
        unitTests.isIncludeAndroidResources = true
    }

    packaging {
        jniLibs {
            // Ядро кладём в пакет сжатым. Несжатое запускалось бы чуть быстрее
            // и не занимало место дважды, но пакет весил бы сорок мегабайт
            // вместо семнадцати. Его качают через телеграм-бота, часто с
            // мобильного интернета и часто в стране, где он дорогой.
            useLegacyPackaging = true
        }
    }
}


dependencies {
    // Ядро: тот же код на Go, что работает на сервере и на настольной машине.
    implementation(files("libs/marvia.aar"))

    implementation("androidx.appcompat:appcompat:1.7.0")
    implementation("androidx.core:core-ktx:1.15.0")
    implementation("androidx.activity:activity-ktx:1.9.3")
    implementation("androidx.constraintlayout:constraintlayout:2.2.1")
    implementation("androidx.lifecycle:lifecycle-runtime-ktx:2.8.7")
    implementation("com.google.android.material:material:1.12.0")
    implementation("androidx.recyclerview:recyclerview:1.3.2")
    implementation("org.jetbrains.kotlinx:kotlinx-coroutines-android:1.10.2")
    // Один декодер для галереи и камеры: сервисы Google на телефоне не нужны.
    implementation("com.google.zxing:core:3.5.3")
    implementation("androidx.camera:camera-camera2:1.6.2")
    implementation("androidx.camera:camera-lifecycle:1.6.2")
    implementation("androidx.camera:camera-view:1.6.2")
    implementation("androidx.work:work-runtime-ktx:2.11.2")

    // Тесты на JVM, без телефона: арифметика темы сверяется с look.js.
    testImplementation("junit:junit:4.13.2")
    testImplementation("org.robolectric:robolectric:4.16.1")
    testImplementation("androidx.work:work-testing:2.11.2")
}
