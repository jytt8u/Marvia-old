//go:build windows

package main

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Система называет программу по порту её соединения — и по TCP, и по UDP.
// Проверяем на себе: тест открывает соединение и ищет по его порту себя же.
func TestWindowsNamesTheProgramByItsPort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	port := uint16(conn.LocalAddr().(*net.TCPAddr).Port)
	if pid := pidByPort(port, false); pid != uint32(os.Getpid()) {
		t.Fatalf("TCP-порт %d принадлежит %d, а не нам (%d)", port, pid, os.Getpid())
	}
	exe, _ := os.Executable()
	if got := processName(uint32(os.Getpid())); !strings.EqualFold(got, filepath.Base(exe)) {
		t.Fatalf("имя процесса %q, ждали %q", got, filepath.Base(exe))
	}

	udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	if pid := pidByPort(uint16(udp.LocalAddr().(*net.UDPAddr).Port), true); pid != uint32(os.Getpid()) {
		t.Fatalf("UDP-порт принадлежит %d, а не нам", pid)
	}
}
