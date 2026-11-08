//go:build linux

package procsb

// FilterForTest exposes the seccomp program's instructions to tests.
func FilterForTest() []any {
	var out []any
	for _, i := range buildFilter() {
		out = append(out, i)
	}
	return out
}
