package panel

import (
	"context"
	"errors"
	"time"
)

// SetReleaseURL подменяет адрес последнего релиза на время проверки.
func SetReleaseURL(u string) (restore func()) {
	was := releaseURL
	releaseURL = u
	return func() { releaseURL = was }
}

// BackdateReports сдвигает все отчёты о доступности в прошлое: тестам на срок
// хранения нужно изобразить отчёт, которому уже не шесть часов.
func (s *Store) BackdateReports(ctx context.Context, by time.Duration) error {
	_, err := s.db.ExecContext(ctx, `UPDATE node_reports SET reported_at = ?`, format(time.Now().UTC().Add(-by)))
	return err
}

// Rows — сколько строк в таблице. Обещания о хранении проверяются по самой
// базе, а не по коду, который в неё пишет.
func (s *Store) Rows(ctx context.Context, table string) int {
	var n int
	_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&n)
	return n
}

// Columns перечисляет столбцы таблицы.
func (s *Store) Columns(ctx context.Context, table string) []string {
	rows, err := s.db.QueryContext(ctx, `SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if rows.Scan(&name) == nil {
			out = append(out, name)
		}
	}
	return out
}

// Tables перечисляет все таблицы базы.
func (s *Store) Tables(ctx context.Context) []string {
	rows, err := s.db.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if rows.Scan(&name) == nil {
			out = append(out, name)
		}
	}
	return out
}

// Exec выполняет запрос к базе напрямую: тестам обновления нужно положить
// данные в том виде, в каком их оставила прежняя версия.
func (s *Store) Exec(ctx context.Context, query string, args ...any) error {
	_, err := s.db.ExecContext(ctx, query, args...)
	return err
}

// SetFreshEvery меняет паузу между настоящими проверками по кнопке.
func SetFreshEvery(d time.Duration) (restore func()) {
	was := freshEvery
	freshEvery = d
	return func() { freshEvery = was }
}

// AutoUpdateAt — одна проверка автообновления в момент now.
func (a *API) AutoUpdateAt(ctx context.Context, now time.Time) error { return a.autoUpdateAt(ctx, now) }

// Тесты пакета шлют сотни запросов с одного адреса, проверяя совсем другое,
// и боевые пределы делали бы их зависимыми от скорости машины. Поэтому по
// умолчанию пределов нет, а тесты самих пределов включают их явно. Страну
// по адресу тоже не спрашивают: тест, который ходит во внешнюю базу, падает
// без сети.
func init() {
	floodDefaults = floodLimits{}
	geoLookup = func(context.Context, string) (string, error) {
		return "", errors.New("в тестах без сети")
	}
}

// SetGeoLookup подменяет базу стран.
func SetGeoLookup(f func(ctx context.Context, host string) (string, error)) (restore func()) {
	was := geoLookup
	geoLookup = f
	return func() { geoLookup = was }
}

// GuardAgainstFlood включает боевые пределы на часах now. Вызывается до
// Handler: маршрутизатор берёт пределы при сборке.
func (a *API) GuardAgainstFlood(now func() time.Time) {
	a.flood = productionFlood
	a.floodNow = now
}

// FloodFailures — сколько неудач за окно ведут к блокировке.
func FloodFailures() int { return productionFlood.failures }

// NextReset — день ежемесячного сброса, см. reset.go.
var NextReset = nextReset
