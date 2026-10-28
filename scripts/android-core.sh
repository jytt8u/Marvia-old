#!/bin/sh
#
# Ядро для Android — android/app/libs/marvia.aar из ./mobile.
#
#   ANDROID_NDK_HOME=/путь/к/ndk sh scripts/android-core.sh
#
# Один рецепт на всех: так собирает выпуск на GitHub (release.yml) и так же
# соберёт F-Droid (docs/fdroid.md). Две копии команды разошлись бы на первой
# же правке флагов, и APK из F-Droid вёл бы себя не как APK из выпусков.
# На Windows для эмулятора — scripts/android.ps1 -Emulator: там ещё x86_64.

set -eu

cd "$(dirname "$0")/.."

[ -n "${ANDROID_NDK_HOME:-}" ] || { echo 'нужен ANDROID_NDK_HOME — путь к NDK' >&2; exit 1; }

# Версия gomobile — из go.mod, а не @latest: иначе одна и та же метка
# собиралась бы разными генераторами в разные дни.
go install golang.org/x/mobile/cmd/gomobile
go install golang.org/x/mobile/cmd/gobind

# -checklinkname=0 — Xray тянет github.com/wlynxg/anet, а тот на Android
# ссылается на внутренности net через go:linkname, что компоновщик с Go
# 1.23 без флага запрещает. Так советует сам anet; Xray для v2rayNG
# собирают так же. Функцию, которой он нужен, Marvia не вызывает.
mkdir -p android/app/libs
"$(go env GOPATH)/bin/gomobile" bind \
	-target=android/arm64,android/arm -androidapi 24 -trimpath -ldflags="-s -w -checklinkname=0" \
	-javapkg=io.marvia -o android/app/libs/marvia.aar ./mobile
