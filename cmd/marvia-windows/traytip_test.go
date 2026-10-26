//go:build windows

package main

import (
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// Навёл на значок — и видно, подключён ли, куда и с каким пингом; длинное
// имя ноды не ломает подсказку, а обрезается.
func TestTrayTipSaysStateNodeAndPing(t *testing.T) {
	got := trayTip(Status{State: StateConnected, Node: "Финляндия · Хельсинки", PingMS: 42})
	for _, want := range []string{"Marvia", say("tipConnected"), "Финляндия · Хельсинки", "42"} {
		if !strings.Contains(got, want) {
			t.Errorf("в подсказке %q нет %q", got, want)
		}
	}
	if got := trayTip(Status{State: StateIdle}); !strings.Contains(got, say("tipOff")) {
		t.Errorf("отключено: %q", got)
	}
	long := trayTip(Status{State: StateConnected, Node: strings.Repeat("Очень длинное имя ноды ", 20)})
	if utf8.RuneCountInString(long) > trayTipMax {
		t.Errorf("подсказка длиннее %d знаков: %d", trayTipMax, utf8.RuneCountInString(long))
	}
}

// Отчёт файлом открывается в «Блокноте» с русскими буквами и переводами
// строк Windows, а два отчёта подряд не затирают друг друга.
func TestReportFileOpensInNotepad(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	path, err := saveReportFile(dir, "Отчёт\nстрока", now)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	if !strings.HasPrefix(string(raw), "\xef\xbb\xbf") || !strings.Contains(string(raw), "Отчёт\r\nстрока") {
		t.Fatalf("файл: %q", raw)
	}
	other, _ := saveReportFile(dir, "x", now.Add(time.Second))
	if other == path {
		t.Fatal("второй отчёт затёр первый")
	}
}
