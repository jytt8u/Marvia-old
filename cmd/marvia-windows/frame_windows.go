//go:build windows

package main

import (
	"unsafe"
)

// Заголовок окна рисует страница, а не Windows.
//
// Системная полоса красилась самым тёмным цветом темы и стояла над шапкой
// страницы: две полосы, верхняя почти чёрная, — на любой теме, кроме
// чёрной, это читалось как чужая деталь. Теперь клиентская область
// начинается от верхнего края окна, шапка страницы и есть заголовок, а
// кнопки окна рисует образ: светофор у macOS, серые выпуклые у Windows 98.
//
// Рамки слева, справа и снизу остаются системными: за них тянут размер,
// по ним Windows рисует тень и скругление. Верхний край и перетаскивание
// страница просит сама — WebView закрывает всё окно и забирает мышь себе,
// поэтому окну неоткуда узнать, что человек взялся за шапку.

var (
	procIsZoomed               = user32.NewProc("IsZoomed")
	procReleaseCapture         = user32.NewProc("ReleaseCapture")
	procGetSystemMetricsForDpi = user32.NewProc("GetSystemMetricsForDpi")
	procGetDpiForWindow        = user32.NewProc("GetDpiForWindow")
)

const (
	wmSize          = 0x0005
	wmNCCalcSize    = 0x0083
	wmNCLButtonDown = 0x00A1
	wmSysCommand    = 0x0112

	htCaption = 2
	htTop     = 12

	scMinimize = 0xF020
	scMaximize = 0xF030
	scRestore  = 0xF120

	sizeMaximized = 2

	smCyFrame        = 33
	smCxPaddedBorder = 92
)

// windowActions — всё, что умеют кнопки и шапка страницы. Тест сверяет
// список с разметкой: кнопка без действия здесь была бы обещанием, которое
// окно не выполняет.
var windowActions = []string{"drag", "top", "min", "max", "close", "state", "through", "solid", "pin", "unpin"}

// ncCalcSizeParams — начало NCCALCSIZE_PARAMS: три прямоугольника. Указатель на
// WINDOWPOS после них нам не нужен, а читаем мы только первый прямоугольник.
type ncCalcSizeParams struct {
	rgrc [3]rect
}

func (s *shell) zoomed() bool {
	r, _, _ := procIsZoomed.Call(s.hwnd)
	return r != 0
}

// calcFrame — ответ на WM_NCCALCSIZE: системный расчёт клиентской области,
// у которой затем отбирается заголовок. Развёрнутое окно Windows ставит
// с выступом за край экрана на толщину рамки; без поправки верх шапки
// уехал бы за монитор вместе с кнопками.
func (s *shell) calcFrame(hwnd, wp, lp uintptr) uintptr {
	// lParam — адрес структуры в памяти Windows; через указатель на сам
	// параметр, чтобы vet не принимал это за потерю указателя сборщиком.
	p := *(**ncCalcSizeParams)(unsafe.Pointer(&lp))
	top := p.rgrc[0].Top
	r, _, _ := procCallWindowProcW.Call(s.oldProc, hwnd, wmNCCalcSize, wp, lp)
	p.rgrc[0].Top = top
	if s.zoomed() {
		dpi, _, _ := procGetDpiForWindow.Call(hwnd)
		if dpi == 0 {
			dpi = 96
		}
		frame, _, _ := procGetSystemMetricsForDpi.Call(smCyFrame, dpi)
		padded, _, _ := procGetSystemMetricsForDpi.Call(smCxPaddedBorder, dpi)
		p.rgrc[0].Top += int32(frame + padded)
	}
	return r
}

// windowAction исполняет просьбу шапки. Возвращает, развёрнуто ли окно
// после неё: по этому страница меняет значок «развернуть» на «восстановить».
func (s *shell) windowAction(action string) bool {
	switch action {
	case "drag", "top":
		// Мышь захвачена движком страницы. Отпускаем её и говорим окну, что
		// нажали на заголовок или на верхний край, — дальше перетаскивание,
		// прилипание к краям экрана и изменение размера ведёт сама Windows.
		if action == "top" && s.zoomed() {
			break
		}
		hit := uintptr(htCaption)
		if action == "top" {
			hit = htTop
		}
		var pt struct{ X, Y int32 }
		_, _, _ = procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
		_, _, _ = procReleaseCapture.Call()
		_, _, _ = procPostMessageW.Call(s.hwnd, wmNCLButtonDown, hit, uintptr(uint32(pt.Y)<<16|uint32(pt.X)&0xffff))
	case "min":
		// Через системную команду, а не ShowWindow: так работает анимация
		// сворачивания и окно уходит на панель задач как обычное.
		_, _, _ = procPostMessageW.Call(s.hwnd, wmSysCommand, scMinimize, 0)
	case "max":
		command := uintptr(scMaximize)
		if s.zoomed() {
			command = scRestore
		}
		_, _, _ = procPostMessageW.Call(s.hwnd, wmSysCommand, command, 0)
	case "close":
		// Тот же путь, что у крестика системы: в трей или выход — по настройке.
		_, _, _ = procPostMessageW.Call(s.hwnd, wmClose, 0, 0)
	case "state":
		// Страница только что загрузилась — ей нужно и место окна на экране:
		// по нему «Прозрачная» сдвигает обои рабочего стола.
		s.place()
	case "pin", "unpin":
		// Виджет поверх всех окон — для стримера, у которого скорость должна
		// быть видна поверх игры. Запоминаем: закреплённый виджет и после
		// перезапуска встаёт закреплённым.
		_ = writeUISetting(pinSetting, map[bool]string{true: "on", false: "off"}[action == "pin"])
		s.applyPin()
	case "through", "solid":
		// «Прозрачная» с видом «всё, что позади» — см. seethrough_windows.go.
		s.seeThrough(action == "through")
	}
	return s.zoomed()
}
