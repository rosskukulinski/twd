//go:build e2emultimgmt

package multimgmt

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestMM_H11_FileCustodyPerReplica: with -custody file (the default) each
// replica kept minted auth keys in its own state dir, so a credential minted
// by one replica could not be released by another (H11: 6/12 failed on each
// replica). Fixed by F6: a second replica sharing the store with file
// custody refuses to start, with a message that says why and what to do,
// while a single replica restarting right after a crash still starts.
func TestMM_H11_FileCustodyPerReplica(t *testing.T) {
	r := newRun(t, "H11")
	c := newCluster(t, r, 2)
	c.extraArgs = []string{"-custody", "file"} // the default; overrides the harness's setec (last flag wins)
	c.start(0)
	if _, err := c.waitReady(0, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	// A second file-custody replica on the same store: refused.
	c.start(1)
	select {
	case <-c.replicas[1].exited:
	case <-time.After(10 * time.Second):
		t.Fatal("I3/I8: a second file-custody replica started and is serving; it must refuse (keys would be unreleasable across replicas)")
	}
	log, _ := os.ReadFile(filepath.Join(r.dir, "r1.log"))
	for _, want := range []string{"refusing to start", "file custody", "-custody setec"} {
		if !strings.Contains(string(log), want) {
			t.Errorf("r1's refusal should mention %q; log:\n%s", want, log)
		}
	}
	c.replicas[1].cmd = nil // already exited
	// r0 is unaffected.
	if st, b, err := c.call(0, "GET", "/v1/status", nil); err != nil || st != 200 || !strings.Contains(string(b), `"ok":true`) {
		t.Errorf("r0 status after the refused join: %d %v %s", st, err, b)
	}
	// A crash and immediate restart of the single replica (its registry row
	// is still live) must start: it has the same stable replica ID.
	c.stop(0, syscall.SIGKILL)
	c.start(0)
	if d, err := c.waitReady(0, 10*time.Second); err != nil {
		t.Fatalf("restart after SIGKILL: %v", err)
	} else {
		t.Logf("r0 restarted after SIGKILL in %v", d.Round(time.Millisecond))
	}
	st, b, _ := c.call(0, "GET", "/v1/status", nil)
	t.Logf("r0 status: %d %s", st, fmt.Sprintf("%.400s", b))
}
