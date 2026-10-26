//go:build windows

package main

import (
	_ "embed"
	"fmt"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Значок в трее.
//
// Без него закрытое окно означало выключенный туннель: свернуть программу
// было некуда, и человек держал окно открытым весь день или терял связь,
// закрыв его по привычке. Теперь окно закрывается в трей, туннель живёт,
// а значок — единственное место, откуда всё видно и выключается.
//
// Реализовано напрямую через user32 и shell32, без сторонней библиотеки: та
// потянула бы свой цикл сообщений, а он здесь уже есть — у окна WebView2.
// Скрытое окно-приёмник живёт в том же потоке, и его сообщения раздаёт тот
// же цикл.

//go:embed ui/tray.png
var trayPNG []byte

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	shell32  = windows.NewLazySystemDLL("shell32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")

	procRegisterClassExW         = user32.NewProc("RegisterClassExW")
	procCreateWindowExW          = user32.NewProc("CreateWindowExW")
	procDefWindowProcW           = user32.NewProc("DefWindowProcW")
	procDestroyWindow            = user32.NewProc("DestroyWindow")
	procCreatePopupMenu          = user32.NewProc("CreatePopupMenu")
	procDestroyMenu              = user32.NewProc("DestroyMenu")
	procAppendMenuW              = user32.NewProc("AppendMenuW")
	procTrackPopupMenu           = user32.NewProc("TrackPopupMenu")
	procGetCursorPos             = user32.NewProc("GetCursorPos")
	procSetForegroundWindow      = user32.NewProc("SetForegroundWindow")
	procPostMessageW             = user32.NewProc("PostMessageW")
	procCreateIconFromResourceEx = user32.NewProc("CreateIconFromResourceEx")
	procDestroyIcon              = user32.NewProc("DestroyIcon")
	procShellNotifyIconW         = shell32.NewProc("Shell_NotifyIconW")
	procGetModuleHandleW         = kernel32.NewProc("GetModuleHandleW")
	procUnregisterClassW         = user32.NewProc("UnregisterClassW")
	procRegisterWindowMessageW   = user32.NewProc("RegisterWindowMessageW")
	procRegisterHotKey           = user32.NewProc("RegisterHotKey")
	procUnregisterHotKey         = user32.NewProc("UnregisterHotKey")
)

const (
	wmDestroy   = 0x0002
	wmClose     = 0x0010
	wmActivate  = 0x0006
	wmCommand   = 0x0111
	wmNull      = 0x0000
	wmLButtonUp = 0x0202
	wmRButtonUp = 0x0205
	wmApp       = 0x8000
	wmTrayEvent = wmApp + 1
	waInactive  = 0

	nimAdd    = 0
	nimModify = 1
	nimDelete = 2

	nifMessage = 0x01
	nifIcon    = 0x02
	nifTip     = 0x04
	nifInfo    = 0x10

	niifInfo            = 0x01
	ninBalloonUserClick = 0x0400 + 5 // WM_USER + 5: нажали на само уведомление

	mfString    = 0x0000
	mfSeparator = 0x0800
	mfGrayed    = 0x0001

	tpmReturnCmd   = 0x0100
	tpmRightButton = 0x0002
	tpmBottomAlign = 0x0020

	hwndMessage = ^uintptr(2) // HWND_MESSAGE: окно только для сообщений, без экрана
)

// Пункты меню по правой кнопке.
const (
	menuOpen = iota + 1
	menuConnect
	menuDisconnect
	menuExit
)

// notifyIconData — NOTIFYICONDATAW. Порядок и типы полей — как в заголовке
// Windows, иначе shell32 прочтёт чужие байты.
type notifyIconData struct {
	Size            uint32
	Wnd             uintptr
	ID              uint32
	Flags           uint32
	CallbackMessage uint32
	Icon            uintptr
	Tip             [128]uint16
	State           uint32
	StateMask       uint32
	Info            [256]uint16
	Version         uint32
	InfoTitle       [64]uint16
	InfoFlags       uint32
	GUID            [16]byte
	BalloonIcon     uintptr
}

type wndClassEx struct {
	Size       uint32
	Style      uint32
	WndProc    uintptr
	ClsExtra   int32
	WndExtra   int32
	Instance   uintptr
	Icon       uintptr
	Cursor     uintptr
	Background uintptr
	MenuName   *uint16
	ClassName  *uint16
	IconSm     uintptr
}

type point struct{ X, Y int32 }

// tray — значок и его скрытое окно-приёмник.
type tray struct {
	hwnd            uintptr
	icon            uintptr
	data            notifyIconData
	taskbarCreated  uintptr
	showExisting    uintptr
	classRegistered bool

	// Что делать по нажатиям. Ставит окно: значок про окно ничего не знает.
	onClick   func()
	onShow    func()
	onLink    func(string)
	onMenu    func(id int)
	menuState func() (connected bool, hasAccount bool)

	// onHotkey — нажата горячая клавиша; hotkey — зарегистрирована ли она.
	onHotkey func()
	hotkey   bool
	// hotkeyBusy — клавишу уже держит другая программа: пробовать снова
	// каждые три секунды незачем, хватит одной строки в журнале.
	hotkeyBusy bool
}

// Горячая клавиша — Ctrl+Alt+M: подключить или отключить, не открывая окна.
// M — Marvia; сочетание редкое, и чужие программы его обычно не держат.
const (
	hotkeyID     = 1
	wmHotkey     = 0x0312
	modAlt       = 0x0001
	modControl   = 0x0002
	modNoRepeat  = 0x4000
	hotkeyLetter = 'M'
)

// SetHotkey ставит или снимает клавишу. Зовётся из потока окна: Windows
// принимает регистрацию только от потока, создавшего окно значка.
func (t *tray) SetHotkey(on bool) (busy bool) {
	if on == t.hotkey || (on && t.hotkeyBusy) {
		return false
	}
	if !on {
		_, _, _ = procUnregisterHotKey.Call(t.hwnd, hotkeyID)
		t.hotkey = false
		return false
	}
	r, _, _ := procRegisterHotKey.Call(t.hwnd, hotkeyID, modControl|modAlt|modNoRepeat, hotkeyLetter)
	if r == 0 {
		t.hotkeyBusy = true
		return true
	}
	t.hotkey = true
	return false
}

// newTray вешает значок. Зовётся из потока цикла сообщений.
func newTray() (*tray, error) {
	t := &tray{}
	installed := false
	defer func() {
		if !installed {
			t.Remove()
		}
	}()

	className, _ := syscall.UTF16PtrFromString("MarviaTray")
	inst, _, _ := procGetModuleHandleW.Call(0)
	wc := wndClassEx{
		Size:      uint32(unsafe.Sizeof(wndClassEx{})),
		WndProc:   syscall.NewCallback(t.wndProc),
		Instance:  inst,
		ClassName: className,
	}
	if r, _, err := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); r == 0 {
		return nil, fmt.Errorf("класс окна трея: %w", err)
	}
	t.classRegistered = true
	restartName, _ := syscall.UTF16PtrFromString("TaskbarCreated")
	t.taskbarCreated, _, _ = procRegisterWindowMessageW.Call(uintptr(unsafe.Pointer(restartName)))
	t.showExisting = instanceMessage()
	instance, err := instanceName()
	if err != nil {
		return nil, err
	}
	windowName, _ := syscall.UTF16PtrFromString(instance)
	// Окно только для сообщений не получает TaskbarCreated. Скрытое верхнее
	// окно позволяет вернуть значок после перезапуска Проводника без VPN-рестарта.
	hwnd, _, err := procCreateWindowExW.Call(0, uintptr(unsafe.Pointer(className)), uintptr(unsafe.Pointer(windowName)), 0, 0, 0, 0, 0,
		0, 0, inst, 0)
	if hwnd == 0 {
		return nil, fmt.Errorf("окно трея: %w", err)
	}
	t.hwnd = hwnd
	// Клиент повышает права для VPN, а Проводник остаётся обычным процессом.
	// Разрешаем только сигнал о его перезапуске через границу UIPI.
	if t.taskbarCreated != 0 {
		_, _, _ = user32.NewProc("ChangeWindowMessageFilterEx").Call(hwnd, t.taskbarCreated, 1, 0)
	}
	// Обычный запуск передаёт только показ окна и ссылку с подтверждением.
	// Прочие сообщения через границу прав по-прежнему запрещены.
	if t.showExisting != 0 {
		_, _, _ = user32.NewProc("ChangeWindowMessageFilterEx").Call(hwnd, t.showExisting, 1, 0)
	}
	_, _, _ = user32.NewProc("ChangeWindowMessageFilterEx").Call(hwnd, wmCopyData, 1, 0)

	icon, err := applicationIcon(32)
	if err != nil {
		return nil, fmt.Errorf("иконка трея: %w", err)
	}
	t.icon = icon

	t.data = notifyIconData{
		Size:            uint32(unsafe.Sizeof(notifyIconData{})),
		Wnd:             hwnd,
		ID:              1,
		Flags:           nifMessage | nifIcon | nifTip,
		CallbackMessage: wmTrayEvent,
		Icon:            icon,
	}
	t.setTip("Marvia")
	if r, _, err := procShellNotifyIconW.Call(nimAdd, uintptr(unsafe.Pointer(&t.data))); r == 0 {
		return nil, fmt.Errorf("значок в трее: %w", err)
	}
	installed = true
	return t, nil
}

func (t *tray) setTip(text string) {
	u, _ := syscall.UTF16FromString(text)
	for i := range t.data.Tip {
		t.data.Tip[i] = 0
	}
	copy(t.data.Tip[:len(t.data.Tip)-1], u)
}

// Tip меняет подсказку под курсором: «Подключено · Финляндия» и тому подобное.
func (t *tray) Tip(text string) {
	t.setTip(text)
	_, _, _ = procShellNotifyIconW.Call(nimModify, uintptr(unsafe.Pointer(&t.data)))
}

// Balloon показывает уведомление Windows от значка — для того, что важно и
// когда окно закрыто: «доступ кончится через три дня».
//
// Флаг NIF_INFO снимаем сразу после показа: значок обновляется часто (каждая
// смена подсказки — тот же NIM_MODIFY), и с оставленным флагом уведомление
// всплывало бы заново на каждое обновление.
func (t *tray) Balloon(title, text string) {
	clear(t.data.Info[:])
	clear(t.data.InfoTitle[:])
	u, _ := syscall.UTF16FromString(text)
	copy(t.data.Info[:len(t.data.Info)-1], u)
	u, _ = syscall.UTF16FromString(title)
	copy(t.data.InfoTitle[:len(t.data.InfoTitle)-1], u)
	t.data.InfoFlags = niifInfo
	t.data.Flags |= nifInfo
	_, _, _ = procShellNotifyIconW.Call(nimModify, uintptr(unsafe.Pointer(&t.data)))
	t.data.Flags &^= nifInfo
	clear(t.data.Info[:])
	clear(t.data.InfoTitle[:])
}

// Remove снимает значок. Обязательно перед выходом: иначе он висит до
// первого наведения, как у половины программ на свете.
func (t *tray) Remove() {
	if t.data.Wnd != 0 {
		_, _, _ = procShellNotifyIconW.Call(nimDelete, uintptr(unsafe.Pointer(&t.data)))
		t.data.Wnd = 0
	}
	if t.icon != 0 {
		_, _, _ = procDestroyIcon.Call(t.icon)
		t.icon = 0
	}
	if t.hwnd != 0 {
		_, _, _ = procDestroyWindow.Call(t.hwnd)
		t.hwnd = 0
	}
	if t.classRegistered {
		className, _ := syscall.UTF16PtrFromString("MarviaTray")
		inst, _, _ := procGetModuleHandleW.Call(0)
		_, _, _ = procUnregisterClassW.Call(uintptr(unsafe.Pointer(className)), inst)
		t.classRegistered = false
	}
}

func (t *tray) wndProc(hwnd, msg, wp, lp uintptr) uintptr {
	if t.showExisting != 0 && msg == t.showExisting {
		if t.onShow != nil {
			t.onShow()
		}
		return 0
	}
	if msg == wmCopyData && lp != 0 {
		payload := instancePayload(lp)
		if link := instanceLink(&payload); link != "" && t.onLink != nil {
			t.onLink(link)
			return 1
		}
		return 0
	}
	if t.taskbarCreated != 0 && msg == t.taskbarCreated {
		_, _, _ = procShellNotifyIconW.Call(nimAdd, uintptr(unsafe.Pointer(&t.data)))
		return 0
	}
	if msg == wmHotkey && wp == hotkeyID {
		if t.onHotkey != nil {
			t.onHotkey()
		}
		return 0
	}
	if msg == wmTrayEvent {
		switch lp & 0xffff {
		case wmLButtonUp:
			if t.onClick != nil {
				t.onClick()
			}
		case wmRButtonUp:
			t.showMenu()
		case ninBalloonUserClick:
			// Нажали на уведомление — открываем окно, где «Продлить».
			if t.onShow != nil {
				t.onShow()
			}
		}
		return 0
	}
	r, _, _ := procDefWindowProcW.Call(hwnd, msg, wp, lp)
	return r
}

// showMenu показывает меню по правой кнопке и сразу исполняет выбор.
func (t *tray) showMenu() {
	menu, _, _ := procCreatePopupMenu.Call()
	if menu == 0 {
		return
	}
	defer procDestroyMenu.Call(menu)

	connected, hasAccount := false, false
	if t.menuState != nil {
		connected, hasAccount = t.menuState()
	}
	add := func(id int, text string, grayed bool) {
		flags := uintptr(mfString)
		if grayed {
			flags |= mfGrayed
		}
		s, _ := syscall.UTF16PtrFromString(text)
		_, _, _ = procAppendMenuW.Call(menu, flags, uintptr(id), uintptr(unsafe.Pointer(s)))
	}
	d := trayWords()
	add(menuOpen, d.open, false)
	if connected {
		add(menuDisconnect, d.disconnect, false)
	} else {
		add(menuConnect, d.connect, !hasAccount)
	}
	_, _, _ = procAppendMenuW.Call(menu, mfSeparator, 0, 0)
	add(menuExit, d.exit, false)

	// Без SetForegroundWindow меню не закрывается по клику мимо, а без
	// пустого сообщения после — открывается не с первого раза. Обе странности
	// описаны у Microsoft и лечатся ровно так.
	var pt point
	_, _, _ = procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	_, _, _ = procSetForegroundWindow.Call(t.hwnd)
	id, _, _ := procTrackPopupMenu.Call(menu, tpmReturnCmd|tpmRightButton|tpmBottomAlign,
		uintptr(pt.X), uintptr(pt.Y), 0, t.hwnd, 0)
	_, _, _ = procPostMessageW.Call(t.hwnd, wmNull, 0, 0)

	if id != 0 && t.onMenu != nil {
		t.onMenu(int(id))
	}
}

// trayWords — надписи меню на языке окна. Словарь окна живёт в странице, а
// меню рисует Windows, поэтому эти четыре слова — здесь.
type trayDict struct{ open, connect, disconnect, exit string }

func trayWords() trayDict {
	if lang, _ := uiLang.Load().(string); lang == "en" {
		return trayDict{"Open Marvia", "Connect", "Disconnect", "Quit"}
	}
	return trayDict{"Открыть Marvia", "Подключиться", "Отключиться", "Выйти"}
}
