package attacks

import "os"

// mkShort returns a short temp dir (unix socket paths are limited to 104
// bytes on macOS; go test's TempDir can be longer).
func mkShort() (string, error) { return os.MkdirTemp("/tmp", "twd-rt-") }

func removeAll(dir string) error { return os.RemoveAll(dir) }
