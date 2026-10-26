package panel

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Восстановление из копии кнопкой в панели.
//
// Раньше это была ночь с консолью: расшифровать копию -verify-backup,
// остановить службу, подложить файл, запустить. Теперь продавец выбирает
// файл в панели, а панель делает то же самое сама — и только то же самое:
// проверяет копию, доводит её схему до своей версии на копии же, кладёт
// рядом с базой и перезапускается. Подмена — при запуске, до открытия базы:
// менять файл под работающей панелью значило бы ловить половину записей
// в старую базу, половину в новую.

// restoreSuffix — имя подготовленной копии рядом с базой.
const restoreSuffix = ".restore"

var sqliteMagic = []byte("SQLite format 3\x00")

// StageRestore проверяет копию и готовит её к подмене при следующем
// запуске. raw — обычная копия («Копия базы») или парольная; ключевые копии
// (marvia-keygen -backup) панель открыть не может — приватного ключа у неё
// нет и быть не должно.
func StageRestore(ctx context.Context, dbPath string, raw []byte, password string) (BackupReport, error) {
	var report BackupReport
	plain := raw
	if !bytes.HasPrefix(raw, sqliteMagic) {
		if !bytes.HasPrefix(raw, passwordBackupMagic) {
			return report, errors.New("это не копия Marvia: ни база, ни парольная копия; копию с ключом расшифруй marvia-keygen и пришли базу")
		}
		opened, err := OpenPasswordBackup(raw, password)
		if err != nil {
			return report, err
		}
		plain = opened
		defer clear(plain)
	}

	check := dbPath + ".restore-check"
	_ = os.Remove(check)
	if err := os.WriteFile(check, plain, 0o600); err != nil {
		return report, fmt.Errorf("запись копии: %w", err)
	}
	defer os.Remove(check)

	// Сначала — как есть, без миграций: что это вообще наша база и что она
	// цела. Open создал бы недостающие таблицы, и чужой файл выглядел бы
	// пустой панелью.
	uriPath := filepath.ToSlash(check)
	if !strings.HasPrefix(uriPath, "/") {
		uriPath = "/" + uriPath
	}
	raw2, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: uriPath, RawQuery: "mode=ro"}).String())
	if err != nil {
		return report, err
	}
	var integrity string
	if err := raw2.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity); err != nil || integrity != "ok" {
		raw2.Close()
		return report, errors.New("целостность базы в копии нарушена")
	}
	for _, table := range []string{"users", "nodes", "credentials"} {
		var n int
		if err := raw2.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_schema WHERE type = 'table' AND name = ?`, table).Scan(&n); err != nil || n == 0 {
			raw2.Close()
			return report, errors.New("это не база панели Marvia: нет таблицы " + table)
		}
	}
	raw2.Close()

	// Потом — миграции на самой копии: копия от прошлой версии становится
	// базой этой, и при запуске подменять останется только файл.
	s, err := Open(check)
	if err != nil {
		return report, fmt.Errorf("копия не открылась этой версией панели: %w", err)
	}
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM users").Scan(&report.Clients); err == nil {
		_ = s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM nodes").Scan(&report.Nodes)
	}
	staged := dbPath + restoreSuffix
	_ = os.Remove(staged)
	err = s.Backup(ctx, staged)
	_ = s.Close()
	if err != nil {
		return report, fmt.Errorf("подготовка копии: %w", err)
	}
	return report, nil
}

// ApplyStagedRestore подменяет базу подготовленной копией, если она есть.
// Зовётся при запуске, до Open. Прежняя база не удаляется, а
// переименовывается: ошибиться копией — не повод потерять настоящую.
func ApplyStagedRestore(dbPath string, now time.Time) (bool, error) {
	staged := dbPath + restoreSuffix
	if _, err := os.Stat(staged); err != nil {
		return false, nil
	}
	if _, err := os.Stat(dbPath); err == nil {
		keep := dbPath + ".before-restore-" + now.UTC().Format("20060102-150405")
		if err := os.Rename(dbPath, keep); err != nil {
			return false, fmt.Errorf("прежняя база не отложена: %w", err)
		}
		// WAL и SHM — от прежней базы; рядом с новой они испортили бы её.
		for _, side := range []string{"-wal", "-shm"} {
			if _, err := os.Stat(dbPath + side); err == nil {
				_ = os.Rename(dbPath+side, keep+side)
			}
		}
		log.Printf("прежняя база отложена в %s", keep)
	}
	if err := os.Rename(staged, dbPath); err != nil {
		return false, fmt.Errorf("подмена базы: %w", err)
	}
	return true, nil
}

// WithRestore даёт панели путь к базе и способ перезапуститься после
// подготовки копии.
func (a *API) WithRestore(dbPath string, restart func()) *API {
	a.restorePath, a.restart = dbPath, restart
	return a
}

func (a *API) restoreBackup(w http.ResponseWriter, r *http.Request) {
	if a.restorePath == "" || a.restart == nil {
		fail(w, http.StatusNotImplemented, "восстановление этой панелью не настроено")
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxSealedBackup+1024))
	if err != nil {
		fail(w, http.StatusBadRequest, "копия не дочиталась")
		return
	}
	if len(raw) > maxSealedBackup {
		fail(w, http.StatusRequestEntityTooLarge, "копия больше 512 МБ")
		return
	}
	report, err := StageRestore(r.Context(), a.restorePath, raw, r.Header.Get("X-Backup-Password"))
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	a.record(r, EventPanelRestore, Event{Detail: fmt.Sprintf("клиентов %d, нод %d", report.Clients, report.Nodes)})
	ok(w, map[string]any{"clients": report.Clients, "nodes": report.Nodes})
	if f, okF := w.(http.Flusher); okF {
		f.Flush()
	}
	// Ответ уходит раньше перезапуска: иначе страница увидела бы обрыв и
	// сочла восстановление неудачным.
	go func() {
		time.Sleep(time.Second)
		a.restart()
	}()
}
