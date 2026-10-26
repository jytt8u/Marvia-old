//go:build windows

package main

import (
	"encoding/binary"
	"path/filepath"
	"sync"
	"time"
	"unsafe"

	"github.com/jytt8u/marvia/internal/tunbridge"
	"golang.org/x/sys/windows"
)

// Чья программа открыла соединение — по таблице соединений Windows.
//
// Мост знает порт, с которого программа открыла соединение через туннель;
// GetExtendedTcpTable и GetExtendedUdpTable называют по порту процесс, а
// процесс — имя своего файла. Ни адресов, ни истории здесь не копится: имя
// нужно на одно решение и забывается.

var (
	iphlpapi                = windows.NewLazySystemDLL("iphlpapi.dll")
	procGetExtendedTCPTable = iphlpapi.NewProc("GetExtendedTcpTable")
	procGetExtendedUDPTable = iphlpapi.NewProc("GetExtendedUdpTable")
	appNameCache            sync.Map // pid → cachedName
)

const (
	tcpTableOwnerPIDAll     = 5
	udpTableOwnerPID        = 1
	afInet, afInet6         = 2, 23
	processQueryLimitedInfo = 0x1000
	appNameTTL              = 30 * time.Second
	errInsufficientBuffer   = 122
	tableTries              = 3

	// Строки MIB_TCPROW_OWNER_PID, MIB_TCP6ROW_OWNER_PID,
	// MIB_UDPROW_OWNER_PID и MIB_UDP6ROW_OWNER_PID: длина, где порт, где PID.
	tcpRow4, tcpPortAt4, tcpPIDAt4 = 24, 8, 20
	tcpRow6, tcpPortAt6, tcpPIDAt6 = 56, 20, 52
	udpRow4, udpPortAt4, udpPIDAt4 = 12, 4, 8
	udpRow6, udpPortAt6, udpPIDAt6 = 28, 20, 24
)

type cachedName struct {
	name string
	at   time.Time
}

// appRoute — ответ мосту: куда вести соединение с порта port.
func (c *Controller) appRoute(port uint16, udp bool) tunbridge.Route {
	if !c.apps.active() {
		return tunbridge.RouteAny
	}
	pid := pidByPort(port, udp)
	if pid == 0 {
		return tunbridge.RouteAny
	}
	return c.apps.route(processName(pid))
}

// pidByPort ищет процесс, чей сокет открыт с этого порта, — сначала среди
// IPv4, потом IPv6.
func pidByPort(port uint16, udp bool) uint32 {
	for _, af := range []uintptr{afInet, afInet6} {
		table := ownerTable(af, udp)
		if pid := findPID(table, af, udp, port); pid != 0 {
			return pid
		}
	}
	return 0
}

func ownerTable(af uintptr, udp bool) []byte {
	size := uint32(16 << 10)
	for i := 0; i < tableTries; i++ {
		buf := make([]byte, size)
		var r uintptr
		if udp {
			r, _, _ = procGetExtendedUDPTable.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)), 0, af, udpTableOwnerPID, 0)
		} else {
			r, _, _ = procGetExtendedTCPTable.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)), 0, af, tcpTableOwnerPIDAll, 0)
		}
		if r == 0 {
			return buf[:size]
		}
		if r != errInsufficientBuffer {
			return nil
		}
	}
	return nil
}

// findPID проходит по строкам таблицы. Порт в строке лежит в сетевом
// порядке байт в младших двух байтах слова.
func findPID(table []byte, af uintptr, udp bool, port uint16) uint32 {
	if len(table) < 4 {
		return 0
	}
	n := int(binary.LittleEndian.Uint32(table[:4]))
	row, portAt, pidAt := tcpRow4, tcpPortAt4, tcpPIDAt4
	switch {
	case udp && af == afInet:
		row, portAt, pidAt = udpRow4, udpPortAt4, udpPIDAt4
	case udp:
		row, portAt, pidAt = udpRow6, udpPortAt6, udpPIDAt6
	case af == afInet6:
		row, portAt, pidAt = tcpRow6, tcpPortAt6, tcpPIDAt6
	}
	for i := 0; i < n; i++ {
		off := 4 + i*row
		if off+row > len(table) {
			break
		}
		r := table[off : off+row]
		if binary.BigEndian.Uint16(r[portAt:portAt+2]) == port {
			return binary.LittleEndian.Uint32(r[pidAt : pidAt+4])
		}
	}
	return 0
}

// processName — имя файла процесса, с коротким кэшем: одна программа
// открывает десятки соединений подряд.
func processName(pid uint32) string {
	if v, ok := appNameCache.Load(pid); ok {
		if c := v.(cachedName); time.Since(c.at) < appNameTTL {
			return c.name
		}
	}
	h, err := windows.OpenProcess(processQueryLimitedInfo, false, pid)
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(h)
	buf := make([]uint16, windows.MAX_PATH)
	size := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &size); err != nil {
		return ""
	}
	name := filepath.Base(windows.UTF16ToString(buf[:size]))
	appNameCache.Store(pid, cachedName{name, time.Now()})
	return name
}
