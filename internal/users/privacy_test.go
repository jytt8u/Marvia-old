package users

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Обещание docs/privacy.md: нода держит адреса покупателей только ради лимита
// устройств, только в памяти и только на время окна, а наружу — в файл
// расхода, в отчёт панели — уходит одно число.

const buyerIP = "203.0.113.77"

// addresses — какие адреса нода сейчас помнит за аккаунтом.
func addresses(r *Registry) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, acc := range r.byAccount {
		n += len(acc.ips)
	}
	return n
}

// TestNodeKeepsNoAddressesWithoutDeviceLimit — тому, кому лимит устройств не
// задан, нода адреса не запоминает вовсе.
func TestNodeKeepsNoAddressesWithoutDeviceLimit(t *testing.T) {
	raw, encoded := newKey(t)
	r, err := NewRegistry([]User{{Secret: encoded, Enabled: true}})
	if err != nil {
		t.Fatal(err)
	}
	s, err := r.Admit(KindVP1, raw, addr(buyerIP))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if n := addresses(r); n != 0 {
		t.Fatalf("без лимита устройств нода запомнила адресов: %d", n)
	}
}

// TestNodeForgetsAddressesAfterTheWindow — адрес живёт в памяти ноды не
// дольше окна, даже если человек больше не приходил.
//
// Забывание не ждёт следующего входа этого же человека: срез для панели
// снимается каждые пятнадцать секунд и заодно выметает устаревшие адреса.
func TestNodeForgetsAddressesAfterTheWindow(t *testing.T) {
	raw, encoded := newKey(t)
	r, err := NewRegistry([]User{{Secret: encoded, Enabled: true, MaxIPs: 2}})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	r.now = func() time.Time { return now }

	s, err := r.Admit(KindVP1, raw, addr(buyerIP))
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if n := addresses(r); n != 1 {
		t.Fatalf("в окне нода помнит адресов %d, ждали один — проверять нечего", n)
	}

	now = now.Add(ipWindow + time.Second)
	_ = r.Stats()
	if n := addresses(r); n != 0 {
		t.Fatalf("после окна нода всё ещё помнит адресов: %d", n)
	}
}

// TestDroppedDeviceLimitDropsAddressesAtOnce — снятый лимит устройств сразу
// снимает и адреса.
//
// Раньше они переезжали в новый список вместе с расходом и доживали до конца
// окна, хотя считать их было уже незачем.
func TestDroppedDeviceLimitDropsAddressesAtOnce(t *testing.T) {
	raw, encoded := newKey(t)
	r, err := NewRegistry([]User{{Secret: encoded, Enabled: true, MaxIPs: 2, Account: "7"}})
	if err != nil {
		t.Fatal(err)
	}
	s, err := r.Admit(KindVP1, raw, addr(buyerIP))
	if err != nil {
		t.Fatal(err)
	}
	s.Close()

	if err := r.Replace([]User{{Secret: encoded, Enabled: true, Account: "7"}}); err != nil {
		t.Fatal(err)
	}
	if n := addresses(r); n != 0 {
		t.Fatalf("лимит сняли, а нода помнит адресов: %d", n)
	}
}

// TestAddressesNeverLeaveTheNodeMemory — ни файл расхода, ни срез для панели
// не несут самих адресов: только их число.
func TestAddressesNeverLeaveTheNodeMemory(t *testing.T) {
	raw, encoded := newKey(t)
	r, err := NewRegistry([]User{{Secret: encoded, Enabled: true, MaxIPs: 3, Label: "Артём", Account: "7"}})
	if err != nil {
		t.Fatal(err)
	}
	s, err := r.Admit(KindVP1, raw, addr(buyerIP))
	if err != nil {
		t.Fatal(err)
	}
	s.Add(100, 100)
	defer s.Close()

	path := filepath.Join(t.TempDir(), "usage.json")
	if err := r.SaveUsage(path); err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(saved), buyerIP) {
		t.Fatalf("адрес покупателя лёг в файл расхода: %s", saved)
	}

	for _, stat := range r.Stats() {
		p, err := json.Marshal(Presence{Conns: stat.Conns, IPs: stat.IPs})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(p), buyerIP) {
			t.Fatalf("адрес покупателя в срезе для панели: %s", p)
		}
		if stat.IPs != 1 {
			t.Fatalf("в срезе адресов %d, ждали число 1", stat.IPs)
		}
	}
}

// TestDeviceResetFreesTheLimitAtOnce — продавец нажал «Сбросить устройства»:
// человек, сменивший телефон, подключается с нового адреса сразу, а не через
// час, когда старый адрес забылся бы сам.
func TestDeviceResetFreesTheLimitAtOnce(t *testing.T) {
	raw, encoded := newKey(t)
	r, err := NewRegistry([]User{{Secret: encoded, Enabled: true, MaxIPs: 1, Account: "7"}})
	if err != nil {
		t.Fatal(err)
	}
	s, err := r.Admit(KindVP1, raw, addr(buyerIP))
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, err := r.Admit(KindVP1, raw, addr("198.51.100.99:5000")); err == nil {
		t.Fatal("второй адрес пущен сверх лимита в одно устройство")
	}

	if err := r.Replace([]User{{Secret: encoded, Enabled: true, MaxIPs: 1, Account: "7", DevicesFrom: time.Now().Add(time.Second)}}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Admit(KindVP1, raw, addr("198.51.100.99:5000")); err != nil {
		t.Fatalf("после сброса устройств новый адрес не пущен: %v", err)
	}
}
