package nodesync

import (
	"bufio"
	"bytes"
	"os"
	"runtime"
	"strconv"
	"strings"
)

// SysStat — нагрузка машины ноды: продавцу видно, что сервер упёрся в
// процессор или память, раньше, чем покупатели скажут «тормозит». Только
// числа о самой машине, ничего о покупателях.
type SysStat struct {
	// Load — средняя нагрузка за минуту (/proc/loadavg); CPUs — ядер.
	Load float64 `json:"load"`
	CPUs int     `json:"cpus"`
	// MemTotal и MemAvail — память машины в байтах (/proc/meminfo).
	MemTotal int64 `json:"mem_total"`
	MemAvail int64 `json:"mem_avail"`
}

// readSysStat читает /proc. Не Linux или /proc закрыт — нули: показать
// нечего, и это не повод ронять отчёт о расходе.
func readSysStat() SysStat {
	s := SysStat{CPUs: runtime.NumCPU()}
	if raw, err := os.ReadFile("/proc/loadavg"); err == nil {
		if fields := strings.Fields(string(raw)); len(fields) > 0 {
			s.Load, _ = strconv.ParseFloat(fields[0], 64)
		}
	}
	if raw, err := os.ReadFile("/proc/meminfo"); err == nil {
		s.MemTotal, s.MemAvail = parseMeminfo(raw)
	}
	return s
}

// parseMeminfo достаёт MemTotal и MemAvailable; в /proc/meminfo они в
// килобайтах.
func parseMeminfo(raw []byte) (total, avail int64) {
	sc := bufio.NewScanner(bytes.NewReader(raw))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		kb, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			continue
		}
		switch fields[0] {
		case "MemTotal:":
			total = kb << 10
		case "MemAvailable:":
			avail = kb << 10
		}
	}
	return total, avail
}
