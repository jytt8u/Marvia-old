package panel

import (
	"net/http"
	"strings"
)

// Вход в панель: кто вошёл и с какими правами.
//
// Помощнику продавца не нужен админский токен, чтобы смотреть: ему выпускают
// ключ с одним правом read, и панель по ответу /me прячет всё, что меняет.
// Право решает сервер, а не страница: спрятанная кнопка — удобство, а отказ
// всё равно даёт API.
//
// Каждый вход пишется в журнал: кем, из какого браузера и с какого адреса.
// Это адреса продавца и его помощников, не покупателей — и ровно то, что
// нужно, чтобы заметить чужой вход по утёкшему токену.

// Me — кто вошёл.
type Me struct {
	Admin  bool     `json:"admin"`
	Name   string   `json:"name"`
	Scopes []string `json:"scopes"`
}

// whoami опознаёт токен: админский или ключ. Пустой ответ — токена нет.
func (a *API) whoami(r *http.Request) (Me, bool) {
	token := bearer(r)
	if token == "" {
		return Me{}, false
	}
	if TokensEqual(token, a.adminToken) {
		return Me{Admin: true, Name: ActorAdmin, Scopes: []string{ScopeUsers, ScopeNodes, ScopeRead}}, true
	}
	key, err := a.store.AuthenticateAPIKey(r.Context(), token)
	if err != nil {
		return Me{}, false
	}
	return Me{Name: key.Name, Scopes: key.Scopes}, true
}

func (a *API) me(w http.ResponseWriter, r *http.Request) {
	me, known := a.whoami(r)
	if !known {
		fail(w, http.StatusUnauthorized, "неизвестный токен")
		return
	}
	ok(w, map[string]any{"me": me})
}

// login — то же, что me, но с записью в журнал. Страница зовёт его один раз,
// когда человек ввёл токен; перезагрузка страницы входом не считается.
func (a *API) login(w http.ResponseWriter, r *http.Request) {
	me, known := a.whoami(r)
	if !known {
		fail(w, http.StatusUnauthorized, "неизвестный токен")
		return
	}
	detail := browserOf(r.UserAgent()) + " · " + callerIP(r)
	a.record(withActor(r, me.Name), EventPanelLogin, Event{Detail: detail})
	ok(w, map[string]any{"me": me})
}

// browserOf — браузер и система одним словом, а не вся строка User-Agent:
// узнать «это не мой ноутбук» хватает, а копить отпечатки незачем.
func browserOf(ua string) string {
	browser := "браузер"
	for _, b := range []struct{ mark, name string }{
		{"Edg/", "Edge"}, {"OPR/", "Opera"}, {"YaBrowser/", "Яндекс"}, {"Firefox/", "Firefox"},
		{"Chrome/", "Chrome"}, {"Safari/", "Safari"},
	} {
		if strings.Contains(ua, b.mark) {
			browser = b.name
			break
		}
	}
	system := ""
	for _, s := range []struct{ mark, name string }{
		{"Windows", "Windows"}, {"Android", "Android"}, {"iPhone", "iPhone"}, {"iPad", "iPad"},
		{"Mac OS", "macOS"}, {"Linux", "Linux"},
	} {
		if strings.Contains(ua, s.mark) {
			system = s.name
			break
		}
	}
	if system == "" {
		return browser
	}
	return browser + ", " + system
}
