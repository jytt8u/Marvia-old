package panel

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Страна ноды по её адресу.
//
// Раньше страну писал только продавец: базы адресов у хостеров ошибаются,
// и сервер в Финляндии числится то в Нидерландах, то в США. Но пустая
// страна хуже: покупатель видит в списке «vm-4823917» без флага и не
// понимает, куда подключается. Поэтому панель подставляет страну по адресу
// сама — только туда, где продавец ничего не написал. Написанное руками
// главнее и не перезаписывается никогда.
//
// Спрашиваем внешние базы: ipinfo.io, а если он молчит — ipwho.is. Уходит
// туда только адрес сервера продавца, тот самый, что и так стоит в
// подписке у каждого покупателя. Адреса покупателей не уходят никуда.

// geoTimeout — сколько ждём одну базу.
const geoTimeout = 5 * time.Second

// geoLookup — откуда панель узнаёт код страны. Переменная ради тестов: в
// них в интернет не ходят.
var geoLookup = lookupCountry

// geoSources — базы по очереди. Обе отвечают без ключа на малом потоке, а
// панель спрашивает раз на ноду.
var geoSources = []struct {
	url   string
	parse func([]byte) string
}{
	{"https://ipinfo.io/%s/country", func(b []byte) string { return strings.TrimSpace(string(b)) }},
	{"https://ipwho.is/%s?fields=country_code", func(b []byte) string {
		var v struct {
			Code string `json:"country_code"`
		}
		_ = json.Unmarshal(b, &v)
		return v.Code
	}},
}

var geoClient = &http.Client{Timeout: geoTimeout}

// lookupCountry отдаёт двухбуквенный код страны адреса маленькими буквами.
func lookupCountry(ctx context.Context, host string) (string, error) {
	ip := net.ParseIP(host)
	if ip == nil {
		// Имя, а не адрес: у ноды за CDN это домен, и страна CDN покупателю
		// ни о чём не скажет.
		return "", errors.New("не адрес")
	}
	if ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
		return "", errors.New("частный адрес")
	}
	var last error
	for _, src := range geoSources {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf(src.url, ip.String()), nil)
		if err != nil {
			return "", err
		}
		resp, err := geoClient.Do(req)
		if err != nil {
			last = err
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		code := strings.ToLower(src.parse(body))
		if resp.StatusCode == http.StatusOK && len(code) == 2 && code[0] >= 'a' && code[0] <= 'z' && code[1] >= 'a' && code[1] <= 'z' {
			return code, nil
		}
		last = fmt.Errorf("ответ %s", resp.Status)
	}
	return "", fmt.Errorf("страна адреса не узнана: %w", last)
}

// countryOfAddress — страна для записи ноды: русское название, если оно у
// нас есть, иначе код большими буквами — флаг по нему всё равно рисуется.
func countryOfAddress(ctx context.Context, address string) (string, error) {
	code, err := geoLookup(ctx, hostOfAddr(address))
	if err != nil {
		return "", err
	}
	if name, ok := countryNames[code]; ok {
		return name, nil
	}
	return strings.ToUpper(code), nil
}

// countryUnknown — страны нет или записано то, в чём страну не узнать
// («Пиюсн», опечатка): флага по такому слову не нарисует ни панель, ни
// приложения, и покупатель видит «··» вместо флага. Такое слово панель
// заменяет страной по адресу; узнаваемую страну, вписанную руками, не трогает
// никогда.
func countryUnknown(country string) bool {
	return countryCodeOf(country) == ""
}

// fillCountry подставляет ноде страну по адресу, если её страну не узнать.
// Фоном: установщик и сверка ноды не должны ждать чужую базу. was — что
// стоит сейчас: пишем, только если за время запроса продавец не вписал своё.
func (a *API) fillCountry(id int64, was, address string) {
	if !a.geo.claim(id) {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*geoTimeout+time.Second)
		defer cancel()
		country, err := countryOfAddress(ctx, address)
		if err != nil {
			return
		}
		if err := a.store.SetCountryIfStill(ctx, id, was, country); err != nil {
			log.Printf("страна ноды %d не записалась: %v", id, err)
		}
	}()
}

// geoTries — когда по ноде спрашивали в последний раз. Сверка идёт каждые
// четверть минуты, а база без ключа терпит малый поток: по одной ноде не
// чаще раза в час, и не важно, получилось или нет.
type geoTries struct {
	mu   sync.Mutex
	last map[int64]time.Time
}

func (g *geoTries) claim(id int64) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.last == nil {
		g.last = map[int64]time.Time{}
	}
	if t, ok := g.last[id]; ok && time.Since(t) < time.Hour {
		return false
	}
	g.last[id] = time.Now()
	return true
}

// SetCountryIfStill пишет страну, только если в записи всё ещё was: к
// моменту ответа базы продавец мог вписать свою.
func (s *Store) SetCountryIfStill(ctx context.Context, id int64, was, country string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE nodes SET country = ? WHERE id = ? AND country = ?`, country, id, was)
	return err
}

// nodeGeo — кнопка «По IP» в окне ноды: страна адреса, без записи. Окно
// подставляет её в поле, а сохраняет продавец сам.
func (a *API) nodeGeo(w http.ResponseWriter, r *http.Request) {
	id, okID := pathID(w, r)
	if !okID {
		return
	}
	node, err := a.store.GetNode(r.Context(), id)
	if err != nil {
		respondStoreErr(w, err)
		return
	}
	country, err := countryOfAddress(r.Context(), node.Address)
	if err != nil {
		fail(w, http.StatusBadGateway, err.Error())
		return
	}
	ok(w, map[string]any{"country": country})
}

// countryNames — как страну назвать покупателю. Те страны, где стоят ноды
// продавцов; остальные останутся кодом, и флаг по нему всё равно будет.
var countryNames = map[string]string{
	"ae": "ОАЭ", "am": "Армения", "at": "Австрия", "au": "Австралия", "az": "Азербайджан",
	"be": "Бельгия", "bg": "Болгария", "br": "Бразилия", "by": "Беларусь", "ca": "Канада",
	"ch": "Швейцария", "cn": "Китай", "cy": "Кипр", "cz": "Чехия", "de": "Германия",
	"dk": "Дания", "ee": "Эстония", "es": "Испания", "fi": "Финляндия", "fr": "Франция",
	"gb": "Великобритания", "ge": "Грузия", "gr": "Греция", "hk": "Гонконг", "hr": "Хорватия",
	"hu": "Венгрия", "id": "Индонезия", "ie": "Ирландия", "il": "Израиль", "in": "Индия",
	"is": "Исландия", "it": "Италия", "jp": "Япония", "kg": "Киргизия", "kr": "Южная Корея",
	"kz": "Казахстан", "lt": "Литва", "lu": "Люксембург", "lv": "Латвия", "md": "Молдова",
	"mx": "Мексика", "my": "Малайзия", "nl": "Нидерланды", "no": "Норвегия", "pl": "Польша",
	"pt": "Португалия", "ro": "Румыния", "rs": "Сербия", "ru": "Россия", "se": "Швеция",
	"sg": "Сингапур", "si": "Словения", "sk": "Словакия", "th": "Таиланд", "tr": "Турция",
	"tw": "Тайвань", "ua": "Украина", "us": "США", "uz": "Узбекистан", "vn": "Вьетнам",
	"za": "ЮАР",
}

// countryTitle — страна так, как её увидит покупатель. Продавцы пишут и
// кодом («FI», «ae»), а в списке серверов это читалось как «FI · fi-1».
// Знакомый код становится названием; остальное — как написано.
func countryTitle(s string) string {
	s = strings.TrimSpace(s)
	if len(s) == 2 {
		if name, ok := countryNames[strings.ToLower(s)]; ok {
			return name
		}
	}
	return s
}

// namedCountries один раз переводит коды стран у уже заведённых нод в
// названия — те, что записаны до countryTitle.
func namedCountries(db *sql.DB) error {
	for code, name := range countryNames {
		if _, err := db.Exec(`UPDATE nodes SET country = ? WHERE lower(country) = ?`, name, code); err != nil {
			return fmt.Errorf("название страны %s: %w", code, err)
		}
	}
	return nil
}
