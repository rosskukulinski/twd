package hostd

import (
	"bufio"
	"bytes"
	"os"
	"strconv"
	"strings"
)

// osMemInfo returns MemTotal and the huge page pool (HugePages_Total ×
// Hugepagesize) in MiB from /proc/meminfo (0, 0 where there is none).
func osMemInfo() (totalMiB, hugeMiB int) {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	var pages, pageKB int
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 2 {
			continue
		}
		n, _ := strconv.Atoi(f[1])
		switch f[0] {
		case "MemTotal:":
			totalMiB = n / 1024
		case "HugePages_Total:":
			pages = n
		case "Hugepagesize:":
			pageKB = n
		}
	}
	return totalMiB, pages * pageKB / 1024
}
