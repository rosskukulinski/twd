package netd

import "time"

// SetPublicRejectWindow shortens the reject-summary window for tests.
func SetPublicRejectWindow(d time.Duration) (restore func()) {
	old := publicRejectWindow
	publicRejectWindow = d
	return func() { publicRejectWindow = old }
}
