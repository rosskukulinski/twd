//go:build linux

package vm

import (
	"fmt"
	"os"
	"strings"
)

// procRSSKB is pid's resident memory in kB: VmRSS plus hugetlb pages
// (guest memory backed by huge pages is not counted in VmRSS).
func procRSSKB(pid int) int64 {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0
	}
	var total int64
	for _, ln := range strings.Split(string(b), "\n") {
		for _, k := range []string{"VmRSS:", "HugetlbPages:"} {
			if strings.HasPrefix(ln, k) {
				var n int64
				fmt.Sscan(strings.TrimSpace(strings.TrimPrefix(ln, k)), &n)
				total += n
			}
		}
	}
	return total
}

// FreeHugePages2M returns the host's free 2 MiB hugetlbfs pages.
func FreeHugePages2M() int {
	b, err := os.ReadFile("/sys/kernel/mm/hugepages/hugepages-2048kB/free_hugepages")
	if err != nil {
		return 0
	}
	var n int
	fmt.Sscan(strings.TrimSpace(string(b)), &n)
	return n
}
