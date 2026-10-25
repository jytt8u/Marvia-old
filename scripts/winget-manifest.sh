#!/usr/bin/env bash
# Манифест winget для опубликованного выпуска.
#
# winget ставит программу сам, без браузера: у файла нет пометки «скачано из
# интернета», и SmartScreen не пугает человека окном «Windows защитила ваш
# компьютер». Для этого манифест надо добавить запросом в microsoft/winget-pkgs
# — это делает владелец со своей учётной записи, см. docs/guide.md, «winget».
#
# Скрипт только пишет файлы: берёт версию и сумму установщика из SHA256SUMS
# опубликованного выпуска, а не считает её сам — манифест должен совпасть с
# тем, что лежит на GitHub, а не с локальной сборкой.
#
#   scripts/winget-manifest.sh v0.13.0-alpha.30 out/
#
# Пишет out/manifests/m/Marvia/Marvia/<версия>/ — ровно тот путь, что в
# репозитории winget-pkgs.
set -euo pipefail

tag="${1:?нужна метка выпуска, например v0.13.0-alpha.30}"
out="${2:?нужен каталог для манифеста}"
repo="jytt8u/marvia"
version="${tag#v}"
base="https://github.com/${repo}/releases/download/${tag}"

sums="$(curl -fsSL "${base}/SHA256SUMS")"
sha="$(printf '%s\n' "$sums" | awk '$2 == "marvia-windows-setup.exe" || $2 == "*marvia-windows-setup.exe" { print toupper($1) }')"
if [ -z "$sha" ]; then
  echo "в SHA256SUMS выпуска ${tag} нет marvia-windows-setup.exe" >&2
  exit 1
fi

id="Marvia.Marvia"
dir="${out}/manifests/m/Marvia/Marvia/${version}"
mkdir -p "$dir"
schema="1.6.0"

cat > "${dir}/${id}.yaml" <<EOF
# yaml-language-server: \$schema=https://aka.ms/winget-manifest.version.${schema}.schema.json
PackageIdentifier: ${id}
PackageVersion: ${version}
DefaultLocale: ru-RU
ManifestType: version
ManifestVersion: ${schema}
EOF

# Установщик ставит только для текущего пользователя и без прав
# администратора (RequestExecutionLevel user в windows-installer.nsi), поэтому
# Scope: user. Код 2 — Marvia запущена: установщик просит выйти из неё через
# трей, а не закрывает её молча посреди туннеля.
cat > "${dir}/${id}.installer.yaml" <<EOF
# yaml-language-server: \$schema=https://aka.ms/winget-manifest.installer.${schema}.schema.json
PackageIdentifier: ${id}
PackageVersion: ${version}
InstallerType: nullsoft
Scope: user
InstallModes:
- interactive
- silent
UpgradeBehavior: install
ExpectedReturnCodes:
- InstallerReturnCode: 2
  ReturnResponse: packageInUse
Protocols:
- marvia
ReleaseDate: $(date -u +%Y-%m-%d)
Installers:
- Architecture: x64
  InstallerUrl: ${base}/marvia-windows-setup.exe
  InstallerSha256: ${sha}
ManifestType: installer
ManifestVersion: ${schema}
EOF

cat > "${dir}/${id}.locale.ru-RU.yaml" <<EOF
# yaml-language-server: \$schema=https://aka.ms/winget-manifest.defaultLocale.${schema}.schema.json
PackageIdentifier: ${id}
PackageVersion: ${version}
PackageLocale: ru-RU
Publisher: Marvia
PublisherUrl: https://github.com/${repo}
PublisherSupportUrl: https://github.com/${repo}/issues
PackageName: Marvia
PackageUrl: https://github.com/${repo}
License: AGPL-3.0
LicenseUrl: https://github.com/${repo}/blob/main/LICENSE
ShortDescription: Клиент VPN с открытым кодом для своей панели и нод — VP1, VLESS и Trojan
Moniker: marvia
Tags:
- vpn
- vless
- trojan
- reality
- proxy
ReleaseNotesUrl: https://github.com/${repo}/releases/tag/${tag}
ManifestType: defaultLocale
ManifestVersion: ${schema}
EOF

cat > "${dir}/${id}.locale.en-US.yaml" <<EOF
# yaml-language-server: \$schema=https://aka.ms/winget-manifest.locale.${schema}.schema.json
PackageIdentifier: ${id}
PackageVersion: ${version}
PackageLocale: en-US
Publisher: Marvia
PackageName: Marvia
License: AGPL-3.0
ShortDescription: Open-source VPN client for your own panel and nodes — VP1, VLESS and Trojan
ReleaseNotesUrl: https://github.com/${repo}/releases/tag/${tag}
ManifestType: locale
ManifestVersion: ${schema}
EOF

echo "$dir"
