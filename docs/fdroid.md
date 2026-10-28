# F-Droid

F-Droid собирает приложения сам, из исходников по метке, и подписывает своим
ключом. Для покупателя это магазин без Google, которому доверяют больше,
чем APK со страницы выпусков; для проекта — ещё одна независимая сборка того
же кода.

## Что уже готово в репозитории

- **Описание и снимки** — `fastlane/metadata/android/{ru-RU,en-US}`: название,
  короткое и полное описание, значок, три снимка, список изменений на
  каждый код версии (`changelogs/<versionCode>.txt`). F-Droid берёт их
  прямо отсюда.
- **Сборка ядра одним скриптом** — `scripts/android-core.sh`. Тот же
  скрипт собирает ядро для выпусков на GitHub (`release.yml`), так что APK
  из F-Droid и APK из выпусков собраны одной командой.
- **Версия свойствами Gradle** — `-PmarviaVersion` и `-PmarviaVersionCode`
  (`gradleprops` в рецепте); переменных окружения рецепт F-Droid не задаёт.
- **Зависимости свободные**: AndroidX, Material, ZXing, CameraX, WorkManager,
  ядро на Go. Сервисов Google и счётчиков нет, QR распознаётся без ML Kit.
- **Лицензия** — AGPL-3.0 (`LICENSE`); «или более поздняя» в проекте не заявлена.

## Чего F-Droid не примет как есть

- **Альфы.** F-Droid не держит предварительные выпуски как основные, а
  публикация 1.x отложена до критериев готовности
  ([development-plan.md](development-plan.md)). Подавать заявку разумно
  к первой стабильной метке; рецепт ниже уже следит только за такими.
- **Подпись.** Сборка F-Droid подписана ключом F-Droid, не нашим
  (`53:EB:5B:D4…:7E:82`). Android не даст поставить одну поверх другой:
  перейти между GitHub и F-Droid можно только переустановкой, ключи при
  этом надо сохранить заранее. Обойти это можно воспроизводимой сборкой
  (F-Droid сверяет свой APK с нашим и публикует наш) — для Go-ядра это
  отдельная работа: одна версия Go, `-trimpath`, одинаковый NDK. Пока не
  сделано, и в описании для F-Droid это надо сказать прямо.
- **Предложение обновиться от продавца** открывает его ссылку в браузере и
  ничего не ставит само — для F-Droid это допустимо, но в описании стоит
  упомянуть, что ссылку даёт продавец, а не F-Droid.

## Черновик рецепта для fdroiddata

Файл `metadata/io.marvia.android.yml` в репозитории
[fdroiddata](https://gitlab.com/fdroid/fdroiddata). Это черновик: версии
NDK и Go сверить с `release.yml` и `go.mod` на дату заявки, сборку
проверить локально через `fdroid build -v -l io.marvia.android`.

```yaml
Categories:
  - VPN & Proxy
License: AGPL-3.0-only
AuthorName: jytt8u
SourceCode: https://github.com/jytt8u/marvia
IssueTracker: https://github.com/jytt8u/marvia/issues
Changelog: https://github.com/jytt8u/marvia/blob/HEAD/CHANGELOG.md

AutoName: Marvia

RepoType: git
Repo: https://github.com/jytt8u/marvia.git

Builds:
  - versionName: 1.0.0
    versionCode: 10100
    commit: v1.0.0
    subdir: android/app
    sudo:
      - apt-get update
      - apt-get install -y golang-go
    gradle:
      - yes
    gradleprops:
      - marviaVersion=1.0.0
      - marviaVersionCode=10100
    # Debian-овский Go старше нужного; go.mod требует свою версию, и Go
    # с GOTOOLCHAIN=auto сам скачает её с proxy.golang.org, сверив сумму
    # по sum.golang.org.
    prebuild: cd ../.. && GOTOOLCHAIN=auto ANDROID_NDK_HOME=$$NDK$$ sh scripts/android-core.sh
    ndk: 28.2.13676358

AutoUpdateMode: Version
UpdateCheckMode: Tags ^v[0-9]+\.[0-9]+\.[0-9]+$
UpdateCheckData: android/version-code|(\d+)|VERSION|(.+)
CurrentVersion: 1.0.0
CurrentVersionCode: 10100
```

Номера `1.0.0` и `10100` — для примера: настоящие берутся из `VERSION` и
`android/version-code` той метки, с которой подают заявку.

## Что делает владелец

1. К первой стабильной метке открыть заявку «Request for Packaging» или сразу
   merge request в fdroiddata с рецептом выше.
2. Ответить на вопросы ревьюеров F-Droid: их обычно задают в самом MR.
3. После публикации добавить ссылку на F-Droid в README и на сайт — только
   когда приложение там действительно появится.
