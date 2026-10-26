//go:build windows

package main

import (
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jytt8u/marvia/internal/look"
	"github.com/jytt8u/marvia/internal/redact"
	"golang.org/x/sys/windows"
)

//go:embed ui/app.html
var appHTML string

// appPage — страница с вшитой темой. Собирается один раз: тема не меняется
// между отдачами, а метку в разметке ищет тест, не отдача.
var appPage = look.Inline(appHTML)

// version подставляется при сборке релиза через -ldflags "-X main.version=…".
// Окно показывает её в шапке: человек, у которого что-то не работает,
// первым делом спрашивает у продавца «а какая у меня версия».
var version = "dev"

// journal — последние строки о происходящем, для вкладки «Журнал».
//
// Кольцо, а не растущий список: программа может работать сутками, и незачем
// копить в памяти всё, что она когда-либо сказала.
type journal struct {
	mu    sync.Mutex
	lines []string
}

// Сколько даём на замер всех нод и на переключение между ними.
//
// Замер идёт настоящими подключениями ко всем нодам разом, поэтому он не
// мгновенный; переключение — это обычное подключение, только к заданной ноде.
const (
	selectTimeout = 60 * time.Second
)

const journalDepth = 200

func newJournal() *journal { return &journal{} }

func (j *journal) add(format string, args ...any) {
	line := time.Now().Format("15:04:05") + "  " + redact.Secrets(fmt.Sprintf(format, args...))

	j.mu.Lock()
	j.lines = append(j.lines, line)
	if len(j.lines) > journalDepth {
		j.lines = j.lines[len(j.lines)-journalDepth:]
	}
	j.mu.Unlock()
}

func (j *journal) snapshot() []string {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make([]string, len(j.lines))
	copy(out, j.lines)
	return out
}

// ui — маленький сервер, который показывает окно и принимает от него команды.
type ui struct {
	ctl *Controller
	log *journal
	key string

	// onWindow переключает вид окна по просьбе страницы: из виджета в
	// полное окно на вкладку, крестиком виджета — в трей. Указатель, потому
	// что окно появляется позже сервера.
	onWindow *func(windowRequest)
}

// serveUI поднимает интерфейс и возвращает адрес, который надо открыть.
//
// Слушаем только на петле, но и этого мало: по адресу /api/account отдаётся
// ссылка доступа с личным ключом покупателя, а на компьютере может работать
// что угодно, в том числе чужое. Поэтому всё лежит под одноразовым ключом в
// адресе — угадать его чужой программе не проще, чем подобрать пароль.
func serveUI(ctl *Controller, log *journal, onWindow *func(windowRequest)) (string, *http.Server, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", nil, err
	}
	key := base64.RawURLEncoding.EncodeToString(raw)

	u := &ui{ctl: ctl, log: log, key: key, onWindow: onWindow}

	mux := http.NewServeMux()
	prefix := "/" + key
	mux.HandleFunc("GET "+prefix+"/{$}", u.page)
	mux.HandleFunc("GET "+prefix+"/api/state", u.state)
	mux.HandleFunc("GET "+prefix+"/api/log", u.journal)
	mux.HandleFunc("GET "+prefix+"/api/account", u.getAccount)
	mux.HandleFunc("POST "+prefix+"/api/account", u.setAccount)
	mux.HandleFunc("POST "+prefix+"/api/connect", u.connect)
	mux.HandleFunc("POST "+prefix+"/api/disconnect", u.disconnect)
	mux.HandleFunc("GET "+prefix+"/api/nodes", u.nodes)
	mux.HandleFunc("POST "+prefix+"/api/nodes/measure", u.measureNodes)
	mux.HandleFunc("POST "+prefix+"/api/nodes/select", u.selectNode)
	mux.HandleFunc("POST "+prefix+"/api/proxy/off", u.dropProxy)
	mux.HandleFunc("POST "+prefix+"/api/fix", u.fix)
	mux.HandleFunc("POST "+prefix+"/api/check", u.recheck)
	mux.HandleFunc("POST "+prefix+"/api/report/copy", u.copyReport)
	mux.HandleFunc("POST "+prefix+"/api/report/save", u.saveReport)
	mux.HandleFunc("GET "+prefix+"/api/net/where", u.netWhere)
	mux.HandleFunc("POST "+prefix+"/api/net/sites", u.netSites)
	mux.HandleFunc("POST "+prefix+"/api/net/speed", u.netSpeed)
	mux.HandleFunc("POST "+prefix+"/api/diag", u.diag)
	mux.HandleFunc("POST "+prefix+"/api/reconnect", u.reconnect)
	mux.HandleFunc("GET "+prefix+"/api/clipboard/key", u.clipboardKey)
	mux.HandleFunc("POST "+prefix+"/api/update/open", u.openUpdate)
	mux.HandleFunc("POST "+prefix+"/api/seller/open", u.openSeller)
	mux.HandleFunc("POST "+prefix+"/api/seller/seen", u.seenReminder)
	mux.HandleFunc("POST "+prefix+"/api/lang", u.setLang)
	mux.HandleFunc("POST "+prefix+"/api/welcome", u.completeWelcome)
	mux.HandleFunc("POST "+prefix+"/api/window", u.window)
	mux.HandleFunc("GET "+prefix+"/api/autostart", u.getAutostart)
	mux.HandleFunc("POST "+prefix+"/api/autostart", u.setAutostart)
	mux.HandleFunc("GET "+prefix+"/api/close", u.getClose)
	mux.HandleFunc("POST "+prefix+"/api/close", u.setClose)
	mux.HandleFunc("GET "+prefix+"/api/tray", u.getTrayPrefs)
	mux.HandleFunc("POST "+prefix+"/api/tray", u.setTrayPrefs)
	mux.HandleFunc("GET "+prefix+"/api/keys", u.keys)
	mux.HandleFunc("POST "+prefix+"/api/keys/use", u.useKey)
	mux.HandleFunc("POST "+prefix+"/api/keys/remove", u.removeKey)
	mux.HandleFunc("GET "+prefix+"/api/settings", u.getSettings)
	mux.HandleFunc("GET "+prefix+"/api/wallpaper", u.wallpaper)
	mux.HandleFunc("POST "+prefix+"/api/settings", u.setSettings)

	// Шрифты и знак — общие с панелью, из того же пакета. Под тем же
	// одноразовым ключом: адреса под ним не угадать, и чужой программе на
	// этой машине нечего опрашивать.
	mux.HandleFunc("GET "+prefix+"/fonts/{name}", look.ServeFont)
	mux.HandleFunc("GET "+prefix+"/assets/{name}", look.ServeAsset)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, fmt.Errorf("не занять порт для окна: %w", err)
	}

	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = server.Serve(ln) }()

	return fmt.Sprintf("http://%s%s/", ln.Addr().String(), prefix), server, nil
}

func (u *ui) page(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(appPage))
}

func (u *ui) state(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, u.ctl.Status())
}

func (u *ui) journal(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"lines": u.log.snapshot()})
}

func (u *ui) getAccount(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"link": u.ctl.Account(), "lang": readUISetting("language"), "welcome": readUISetting("welcome") == "1",
	})
}

func (u *ui) setAccount(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Link string `json:"link"`
		// Name — необязательное имя ключа в списке; без него — домен продавца.
		Name string `json:"name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": say("badRequest")})
		return
	}
	if err := u.ctl.AddKey(body.Name, strings.TrimSpace(body.Link)); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	u.log.add("%s", say("logKeySaved"))
	writeJSON(w, http.StatusOK, map[string]any{})
}

func (u *ui) connect(w http.ResponseWriter, _ *http.Request) {
	if err := u.ctl.Connect(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{})
}

func (u *ui) disconnect(w http.ResponseWriter, _ *http.Request) {
	u.ctl.StartDisconnect()
	writeJSON(w, http.StatusOK, map[string]any{})
}

// nodes отдаёт список нод без замера: тем, что уже известно.
func (u *ui) nodes(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, u.ctl.Nodes())
}

// measureNodes запускает замер в фоне и сразу отвечает: числа окно
// забирает частями, пока замер идёт (см. Controller.StartMeasure).
func (u *ui) measureNodes(w http.ResponseWriter, _ *http.Request) {
	u.ctl.StartMeasure()
	writeJSON(w, http.StatusOK, u.ctl.Nodes())
}

// selectNode переводит туннель на выбранную ноду. Ноль — обратно к автовыбору.
func (u *ui) selectNode(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID int64 `json:"id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": say("badRequest")})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), selectTimeout)
	defer cancel()

	if err := u.ctl.SelectNode(ctx, body.ID); err != nil {
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, u.ctl.Nodes())
}

// dropProxy снимает системный прокси — по нажатию человека, не сам.
func (u *ui) dropProxy(w http.ResponseWriter, _ *http.Request) {
	if err := u.ctl.DropProxy(); err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, u.ctl.Status())
}

// fix — «Починить»: что сделано, словами, и состояние после.
func (u *ui) fix(w http.ResponseWriter, _ *http.Request) {
	done, err := u.ctl.Fix()
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"done": done, "state": u.ctl.Status()})
}

// copyReport — «Скопировать отчёт»: отчёт собирается и кладётся в буфер
// программой, см. clipboard_windows.go.
func (u *ui) copyReport(w http.ResponseWriter, _ *http.Request) {
	if err := copyText(u.ctl.Report()); err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// saveReport кладёт отчёт файлом в «Загрузки» и показывает его в
// проводнике. Буфер обмена годится для чата, а поддержке по почте или в
// тикете нужен файл; к тому же длинный отчёт мессенджеры режут.
func (u *ui) saveReport(w http.ResponseWriter, _ *http.Request) {
	path, err := saveReportFile(downloadsDir(), u.ctl.Report(), time.Now())
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	_ = exec.Command("explorer.exe", "/select,"+path).Start()
	writeJSON(w, http.StatusOK, map[string]string{"path": path})
}

// downloadsDir — папка «Загрузки», а без неё — временная.
func downloadsDir() string {
	dir, err := windows.KnownFolderPath(windows.FOLDERID_Downloads, 0)
	if err != nil || dir == "" {
		return os.TempDir()
	}
	return dir
}

// saveReportFile пишет отчёт в dir. Имя с датой и временем: два отчёта
// подряд не затирают друг друга.
func saveReportFile(dir, report string, now time.Time) (string, error) {
	path := filepath.Join(dir, "marvia-report-"+now.Format("2006-01-02-150405")+".txt")
	// BOM — чтобы «Блокнот» постарше не принял русский текст за кракозябры.
	body := strings.ReplaceAll(report, "\n", "\r\n")
	if err := os.WriteFile(path, append([]byte{0xef, 0xbb, 0xbf}, body...), 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// recheck — «Проверить ещё раз».
func (u *ui) recheck(w http.ResponseWriter, _ *http.Request) {
	if err := u.ctl.Recheck(); err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, u.ctl.Status())
}

func (u *ui) openUpdate(w http.ResponseWriter, _ *http.Request) {
	if err := u.ctl.OpenUpdate(); err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// openSeller открывает ссылку продавца по имени кнопки: "support" или
// "renew". Адрес страница не присылает — см. Controller.OpenSeller.
func (u *ui) openSeller(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Link string `json:"link"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": say("badRequest")})
		return
	}
	if err := u.ctl.OpenSeller(body.Link); err != nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// seenReminder — человек закрыл напоминание о сроке или трафике.
func (u *ui) seenReminder(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Key string `json:"key"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": say("badRequest")})
		return
	}
	if err := u.ctl.SeenReminder(body.Key); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func writeJSON(w http.ResponseWriter, code int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(payload)
}

// setLang запоминает язык, выбранный в окне.
//
// Выбирает его окно, а не программа: язык там берётся у браузера, то есть у
// системы, и человек может его переключить. Программе он нужен затем, что
// часть сообщений — ошибки и строки журнала — собирается здесь.
func (u *ui) setLang(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Lang string `json:"lang"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<12)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": say("badRequest")})
		return
	}
	setUILang(body.Lang)
	if body.Lang == "ru" || body.Lang == "en" {
		if err := writeUISetting("language", body.Lang); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{})
}

func (u *ui) completeWelcome(w http.ResponseWriter, _ *http.Request) {
	if err := writeUISetting("welcome", "1"); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{})
}

// Настройки окна живут рядом с ключом доступа: случайный порт WebView2
// меняется при каждом запуске, поэтому localStorage не может быть основным
// хранилищем языка и уже пройденного первого экрана.
func readUISetting(name string) string {
	dir, err := settingsDir()
	if err != nil {
		return ""
	}
	raw, err := os.ReadFile(filepath.Join(dir, "ui-"+name))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

func writeUISetting(name, value string) error {
	dir, err := settingsDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "ui-"+name), []byte(value), 0o600)
}

type windowRequest struct {
	Mode       string `json:"mode"`
	Tab        string `json:"tab"`
	Background string `json:"background"`
	Foreground string `json:"foreground"`
}

// window принимает переключение вида или цвета системного заголовка от страницы.
func (u *ui) window(w http.ResponseWriter, r *http.Request) {
	var body windowRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<12)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": say("badRequest")})
		return
	}
	if body.Mode == "theme" {
		if _, err := captionColors(body.Background, body.Foreground); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": say("badRequest")})
			return
		}
	}
	// Вкладку проверяем по списку: она уходит в адрес страницы, и мусор в
	// ней — это мусор в адресной строке движка.
	switch body.Tab {
	case "", "home", "nodes", "diag", "theme", "settings":
	default:
		body.Tab = ""
	}
	if u.onWindow != nil && *u.onWindow != nil {
		(*u.onWindow)(body)
	}
	writeJSON(w, http.StatusOK, map[string]any{})
}

func (u *ui) getAutostart(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"on": autostartOn()})
}

func (u *ui) setAutostart(w http.ResponseWriter, r *http.Request) {
	var body struct {
		On bool `json:"on"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<12)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": say("badRequest")})
		return
	}
	if err := setAutostart(body.On); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if body.On {
		u.log.add("%s", say("logAutostartOn"))
	} else {
		u.log.add("%s", say("logAutostartOff"))
	}
	writeJSON(w, http.StatusOK, map[string]any{"on": autostartOn()})
}

func (u *ui) getSettings(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, u.ctl.Settings())
}

// setSettings меняет настройки подключения. Строгий разбор: опечатка в
// имени поля должна стать ошибкой, а не молча ничего не поменять.
func (u *ui) setSettings(w http.ResponseWriter, r *http.Request) {
	var body SettingsPatch
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<12))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": say("badRequest")})
		return
	}
	view, err := u.ctl.UpdateSettings(body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (u *ui) keys(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"keys": u.ctl.Keys()})
}

// keyRequest — какой ключ из списка, по отпечатку: сама ссылка с личным
// ключом через окно второй раз не ходит.
func keyRequest(w http.ResponseWriter, r *http.Request) (string, bool) {
	var body struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<12)).Decode(&body); err != nil || body.ID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": say("badRequest")})
		return "", false
	}
	return body.ID, true
}

func (u *ui) useKey(w http.ResponseWriter, r *http.Request) {
	id, ok := keyRequest(w, r)
	if !ok {
		return
	}
	if err := u.ctl.UseKey(id); err != nil {
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": u.ctl.Keys()})
}

func (u *ui) removeKey(w http.ResponseWriter, r *http.Request) {
	id, ok := keyRequest(w, r)
	if !ok {
		return
	}
	if err := u.ctl.RemoveKey(id); err != nil {
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": u.ctl.Keys()})
}

// closeSetting — что делает крестик окна. По умолчанию прячет в трей:
// туннель продолжает работать, а окно VPN, которое закрылось вместе с
// защитой, — неприятный сюрприз. Кто хочет, чтобы крестик закрывал
// программу, выбирает это сам в «Настройках».
const closeSetting = "close"

func closeQuits() bool { return readUISetting(closeSetting) == "quit" }

// Настройки трея: горячая клавиша включена, пока её не выключили, тихий
// режим — наоборот (traytip.go).
const (
	hotkeySetting = "hotkey"
	quietSetting  = "quiet"
)

func hotkeyOn() bool { return readUISetting(hotkeySetting) != "off" }

// quietOn — тихий режим: ни одного всплывающего уведомления, только значок.
// Для тех, у кого программа стоит на рабочем ПК и уведомления мешают.
func quietOn() bool { return readUISetting(quietSetting) == "on" }

func (u *ui) getTrayPrefs(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"hotkey": hotkeyOn(), "quiet": quietOn()})
}

func (u *ui) setTrayPrefs(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Hotkey *bool `json:"hotkey"`
		Quiet  *bool `json:"quiet"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<12)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": say("badRequest")})
		return
	}
	flag := func(name string, on bool, yes, no string) error {
		value := no
		if on {
			value = yes
		}
		return writeUISetting(name, value)
	}
	if body.Hotkey != nil {
		if err := flag(hotkeySetting, *body.Hotkey, "on", "off"); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
	}
	if body.Quiet != nil {
		if err := flag(quietSetting, *body.Quiet, "on", "off"); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
	}
	u.getTrayPrefs(w, r)
}

func (u *ui) getClose(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"quit": closeQuits()})
}

func (u *ui) setClose(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Quit bool `json:"quit"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<12)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": say("badRequest")})
		return
	}
	value := "tray"
	if body.Quit {
		value = "quit"
	}
	if err := writeUISetting(closeSetting, value); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"quit": closeQuits()})
}

// netWhere — адрес и страна через туннель; см. netcheck.go.
func (u *ui) netWhere(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, u.ctl.Where(r.Context()))
}

// netSites проверяет главные сайты через туннель.
func (u *ui) netSites(w http.ResponseWriter, r *http.Request) {
	sites, err := u.ctl.CheckSites(r.Context())
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sites": sites})
}

// diagTimeout — потолок одного шага «Проверить всё»: самый долгий —
// страница двумя путями, по probeTimeout на каждый.
const diagTimeout = 2*probeTimeout + 4*time.Second

// diag проходит один шаг «Проверить всё»; какой — по имени из diagSteps.
func (u *ui) diag(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Step string `json:"step"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": say("badRequest")})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), diagTimeout)
	defer cancel()
	step, err := u.ctl.Diagnose(ctx, body.Step)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, step)
}

// reconnect — «Переподключиться»: туннель заново с выбранными настройками.
func (u *ui) reconnect(w http.ResponseWriter, _ *http.Request) {
	if err := u.ctl.Reconnect(); err != nil {
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, u.ctl.Status())
}

// clipboardKey — есть ли в буфере обмена ключ доступа; см. KeyOffer.
func (u *ui) clipboardKey(w http.ResponseWriter, _ *http.Request) {
	offer, err := u.ctl.ClipboardOffer()
	if err != nil {
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, offer)
}

// netSpeed меряет скорость приёма через туннель — только по кнопке.
func (u *ui) netSpeed(w http.ResponseWriter, r *http.Request) {
	v, err := u.ctl.SpeedTest(r.Context())
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, v)
}
