package panel

import (
	"encoding/csv"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Метки клиентов и выгрузка списка.
//
// Метка — короткое слово продавца: «семья», «опт», «тест». Тарифом её не
// заменить: тариф решает срок и лимиты, а метка — только то, как продавец
// делит людей у себя в голове, и по ней фильтруют список.

const (
	maxTags   = 10
	maxTagLen = 24
)

// ErrBadTags — метки не прошли проверку.
var ErrBadTags = errors.New("метки: не больше 10, каждая до 24 знаков, без запятых")

// cleanTags убирает пробелы и повторы без учёта регистра. Запятая внутри
// метки запрещена: метки хранятся строкой через запятую, и «опт,семья»
// стала бы двумя.
func cleanTags(in []string) ([]string, error) {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, t := range in {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		if strings.Contains(t, ",") || len([]rune(t)) > maxTagLen {
			return nil, ErrBadTags
		}
		key := strings.ToLower(t)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, t)
	}
	if len(out) > maxTags {
		return nil, ErrBadTags
	}
	return out, nil
}

func joinTags(tags []string) string { return strings.Join(tags, ",") }

func splitTags(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

// exportUsers отдаёт список клиентов таблицей для своей бухгалтерии.
//
// Точка с запятой и метка BOM в начале — ради Excel с русскими настройками:
// с запятой он сваливает всю строку в одну ячейку, а без BOM читает
// кириллицу как кракозябры. LibreOffice и Google Таблицы понимают и так.
func (a *API) exportUsers(w http.ResponseWriter, r *http.Request) {
	list, err := a.store.ListUsers(r.Context())
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	plans, err := a.store.ListPlans(r.Context())
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	planName := map[int64]string{}
	for _, p := range plans {
		planName[p.ID] = p.Name
	}

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="marvia-clients-%s.csv"`, time.Now().Format("2006-01-02")))
	_, _ = w.Write([]byte{0xef, 0xbb, 0xbf})
	out := csv.NewWriter(w)
	out.Comma = ';'
	_ = out.Write([]string{"номер", "имя", "метки", "заметка", "состояние", "срок", "тариф",
		"лимит, ГБ", "израсходовано, ГБ", "устройств", "скорость, Мбит/с", "заведён", "ключ продавца"})
	gb := func(b int64) string { return strconv.FormatFloat(float64(b)/(1<<30), 'f', 2, 64) }
	date := func(t *time.Time) string {
		if t == nil {
			return "бессрочно"
		}
		return t.In(moscow).Format("2006-01-02 15:04")
	}
	for _, u := range list {
		state := "включён"
		switch {
		case u.FrozenAt != nil:
			state = "заморожен"
		case !u.Enabled:
			state = "отключён"
		}
		plan := ""
		if u.PlanID != nil {
			plan = planName[*u.PlanID]
		}
		created := u.CreatedAt
		_ = out.Write([]string{
			strconv.FormatInt(u.ID, 10), csvSafe(u.Label), csvSafe(strings.Join(u.Tags, ", ")), csvSafe(u.Note), state,
			date(u.ExpiresAt), csvSafe(plan), gb(u.TrafficLimit), gb(u.Used), strconv.Itoa(u.MaxIPs),
			strconv.FormatInt(u.SpeedLimit/125000, 10), date(&created), csvSafe(u.ExternalID),
		})
	}
	out.Flush()
	a.record(r, EventUsersExport, Event{Detail: fmt.Sprintf("клиентов: %d", len(list))})
}

// csvSafe не даёт имени клиента стать формулой. «=HYPERLINK(...)» в имени,
// которое вписал бот со слов покупателя, Excel выполнил бы при открытии.
func csvSafe(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}
