package panel

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Копия встаёт на место базы при следующем запуске, а прежняя база не
// пропадает — её откладывают рядом.
func TestRestoredBackupReplacesTheBaseAndKeepsTheOldOne(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	old, err := Open(filepath.Join(dir, "old.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := old.CreateUser(ctx, CreateUserParams{Label: "из копии"}); err != nil {
		t.Fatal(err)
	}
	copyPath := filepath.Join(dir, "copy.db")
	if err := old.Backup(ctx, copyPath); err != nil {
		t.Fatal(err)
	}
	old.Close()
	raw, _ := os.ReadFile(copyPath)

	live := filepath.Join(dir, "panel.db")
	cur, err := Open(live)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := cur.CreateUser(ctx, CreateUserParams{Label: "живой"}); err != nil {
		t.Fatal(err)
	}
	report, err := StageRestore(ctx, live, raw, "")
	if err != nil {
		t.Fatal(err)
	}
	if report.Clients != 1 {
		t.Fatalf("в копии клиентов %d", report.Clients)
	}
	cur.Close()

	done, err := ApplyStagedRestore(live, time.Now())
	if err != nil || !done {
		t.Fatalf("подмена: %v, %v", done, err)
	}
	restored, err := Open(live)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	list, _ := restored.ListUsers(ctx)
	if len(list) != 1 || list[0].Label != "из копии" {
		t.Fatalf("после восстановления: %+v", list)
	}
	matches, _ := filepath.Glob(live + ".before-restore-*")
	kept := 0
	for _, m := range matches {
		if !strings.HasSuffix(m, "-wal") && !strings.HasSuffix(m, "-shm") {
			kept++
		}
	}
	if kept != 1 {
		t.Fatalf("прежняя база не отложена: %v", matches)
	}
	if again, _ := ApplyStagedRestore(live, time.Now()); again {
		t.Fatal("подмена сработала второй раз без новой копии")
	}
}

// Чужой файл и чужую SQLite-базу восстановление не принимает: иначе
// продавец, ошибившись файлом, получил бы пустую панель вместо своей.
func TestRestoreRefusesWhatIsNotAMarviaBase(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	live := filepath.Join(dir, "panel.db")
	if _, err := StageRestore(ctx, live, []byte("просто текст"), ""); err == nil {
		t.Fatal("текстовый файл принят как копия")
	}
	foreign := filepath.Join(dir, "foreign.db")
	db, err := sql.Open("sqlite", foreign)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE notes (id INTEGER PRIMARY KEY, text TEXT)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	raw, _ := os.ReadFile(foreign)
	if _, err := StageRestore(ctx, live, raw, ""); err == nil || !strings.Contains(err.Error(), "не база панели") {
		t.Fatalf("чужая база: %v", err)
	}
	if _, err := os.Stat(live + restoreSuffix); err == nil {
		t.Fatal("после отказа осталась подготовленная копия")
	}
}

// Парольная копия открывается паролем, а с неверным паролем — нет.
func TestPasswordBackupRestoresOnlyWithItsPassword(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	s, err := Open(filepath.Join(dir, "src.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CreateUser(ctx, CreateUserParams{Label: "a"}); err != nil {
		t.Fatal(err)
	}
	plain := filepath.Join(dir, "plain.db")
	if err := s.Backup(ctx, plain); err != nil {
		t.Fatal(err)
	}
	s.Close()
	public, salt, err := PasswordBackupRecipient("верный пароль")
	if err != nil {
		t.Fatal(err)
	}
	sealedPath, err := sealPasswordBackup(plain, public, salt)
	if err != nil {
		t.Fatal(err)
	}
	sealed, _ := os.ReadFile(sealedPath)
	live := filepath.Join(dir, "panel.db")
	if _, err := StageRestore(ctx, live, sealed, "не тот"); err == nil {
		t.Fatal("копия открылась чужим паролем")
	}
	if r, err := StageRestore(ctx, live, sealed, "верный пароль"); err != nil || r.Clients != 1 {
		t.Fatalf("верный пароль: %+v, %v", r, err)
	}
}
