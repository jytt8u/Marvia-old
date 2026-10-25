package main

import (
	"os"
	"strings"
	"testing"
)

func readRepoFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile("../../" + path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.ReplaceAll(string(raw), "\r\n", "\n")
}

// winget и CI ставят установщик с /S. Окно с вопросом без ответа по
// умолчанию в тихом режиме всё равно всплывает и ждёт нажатия — установка
// висела бы, пока кто-нибудь не подойдёт к компьютеру.
func TestSilentInstallNeverStopsOnAQuestion(t *testing.T) {
	nsi := readRepoFile(t, "scripts/windows-installer.nsi")
	found := 0
	for _, line := range strings.Split(nsi, "\n") {
		if !strings.Contains(line, "MessageBox ") {
			continue
		}
		found++
		if !strings.Contains(line, "/SD ") {
			t.Errorf("окно без ответа для тихой установки: %s", strings.TrimSpace(line))
		}
	}
	if found == 0 {
		t.Fatal("в установщике не нашлось ни одного MessageBox — проверка смотрит не туда")
	}
}

// Манифест winget обещает то, что делает установщик: ставит для текущего
// пользователя без прав администратора, пишет в «Программы и компоненты»
// того же издателя и ту же версию, а код 2 означает «Marvia запущена».
// Разойдись они — winget не узнал бы установленную программу и предлагал
// бы ставить её снова.
func TestWingetManifestPromisesWhatTheInstallerDoes(t *testing.T) {
	nsi := readRepoFile(t, "scripts/windows-installer.nsi")
	manifest := readRepoFile(t, "scripts/winget-manifest.sh")
	pairs := []struct{ nsi, manifest string }{
		{"RequestExecutionLevel user", "Scope: user"},
		{`"Publisher" "Marvia"`, "Publisher: Marvia"},
		{`"DisplayVersion" "${VERSION}"`, "PackageVersion: ${version}"},
		{"SetErrorLevel 2", "InstallerReturnCode: 2"},
		{`"Software\Classes\marvia`, "- marvia"},
		{"!include \"MUI2.nsh\"", "InstallerType: nullsoft"},
	}
	for _, p := range pairs {
		if !strings.Contains(nsi, p.nsi) {
			t.Errorf("в установщике нет %q", p.nsi)
		}
		if !strings.Contains(manifest, p.manifest) {
			t.Errorf("в манифесте winget нет %q", p.manifest)
		}
	}
}
