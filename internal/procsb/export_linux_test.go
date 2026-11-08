//go:build linux

package procsb

// FilterForTest exposes the seccomp program's instructions to tests.
func FilterForTest() []any {
	var out []any
	for _, i := range buildFilter(filterOpts{self: 1234, noUnix: true}) {
		out = append(out, i)
	}
	return out
}
