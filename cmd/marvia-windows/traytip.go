//go:build windows

package main

import "strings"

// Подсказка у значка в трее и настройки поведения трея.
//
// Навести мышь на значок — самый быстрый способ узнать «я подключён и
// куда», не открывая окна. Раньше там всегда было одно «Marvia».

// trayTipMax — сколько знаков Windows показывает в подсказке значка
// (NOTIFYICONDATAW.szTip — 128 с завершающим нулём).
const trayTipMax = 127

// trayTip — подсказка по состоянию: «Marvia · Подключено · Финляндия · 42 мс».
func trayTip(st Status) string {
	parts := []string{"Marvia"}
	switch st.State {
	case StateConnected:
		parts = append(parts, say("tipConnected"))
		if st.Node != "" {
			parts = append(parts, st.Node)
		}
		if st.PingMS > 0 {
			parts = append(parts, sayf("tipPing", st.PingMS))
		}
	case StateConnecting:
		parts = append(parts, say("tipConnecting"))
	case StateStalled:
		parts = append(parts, say("tipStalled"))
		if st.Node != "" {
			parts = append(parts, st.Node)
		}
	case StateFailed:
		parts = append(parts, say("tipFailed"))
	default:
		parts = append(parts, say("tipOff"))
	}
	tip := strings.Join(parts, " · ")
	if r := []rune(tip); len(r) > trayTipMax {
		tip = string(r[:trayTipMax-1]) + "…"
	}
	return tip
}
