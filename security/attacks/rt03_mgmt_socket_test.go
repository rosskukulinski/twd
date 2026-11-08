package attacks

// RT-03 regression test (Critical, Linux process/Node tier): the mgmt admin
// unix socket (mgmt/server.go: ListenUnix + uidListener) authorizes by
// same-uid peercred only. Since every tenant workload runs as the same uid as
// mgmt, any tenant process that can reach the socket (the Node tier on Linux,
// or a workerd isolate escape) is accepted as the "local" admin principal.
//
// Desired invariant (post-fix), encoded below: a bare same-uid connection must
// NOT be accepted as admin without an additional credential (token / distinct
// uid / microVM boundary). The test dials the real listener from this same
// process (same uid) and asserts the connection is refused. It currently
// SUCCEEDS in connecting, so the invariant fails. Skip-marked with the ID.

import (
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/rosskukulinski/twd/mgmt"
)

func TestRT03_MgmtSocketRejectsBareSameUid(t *testing.T) {
	t.Skip("RT-03: mgmt admin socket accepts any same-uid peer as admin. Remove skip to reproduce on Linux/macOS.")

	sock := filepath.Join(t.TempDir(), "mgmt.sock")
	ln, err := mgmt.ListenUnix(sock)
	if err != nil {
		t.Fatalf("ListenUnix: %v", err)
	}
	defer ln.Close()

	accepted := make(chan bool, 1)
	go func() {
		c, err := ln.Accept() // uidListener only returns same-uid conns
		if err != nil {
			accepted <- false
			return
		}
		c.Close()
		accepted <- true // the weakness: a bare same-uid peer got through
	}()

	c, err := net.Dial("unix", sock)
	if err == nil {
		c.Close()
	}

	select {
	case got := <-accepted:
		// DESIRED post-fix: a bare same-uid dial must NOT be admitted as admin.
		if got {
			t.Fatalf("RT-03: mgmt admin socket admitted a bare same-uid peer with no credential; a tenant Node app takes over every tenant")
		}
	case <-time.After(2 * time.Second):
		// No connection admitted within the window = desired behaviour.
	}
}
