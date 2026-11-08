package gen_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/rosskukulinski/twd/hostd/gen"
	"github.com/rosskukulinski/twd/netd/nettest"
)

// TestRuntimeEmbedFresh: hostd/gen/js/twd-rt.js must be the current build of
// js/packages/tailnet/src/runtime.ts (`make gen-js`).
func TestRuntimeEmbedFresh(t *testing.T) {
	root, _ := nettest.FindRepoRoot()
	esb := filepath.Join(root, "node_modules", ".bin", "esbuild")
	if _, err := os.Stat(esb); err != nil {
		t.Skip("esbuild not installed")
	}
	out := filepath.Join(t.TempDir(), "rt.js")
	cmd := exec.Command(esb, "js/packages/tailnet/src/runtime.ts", "--bundle", "--format=esm", "--platform=neutral",
		"--target=es2022", "--legal-comments=none", "--log-level=warning", "--outfile="+out)
	cmd.Dir = root
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, b)
	}
	b, _ := os.ReadFile(out)
	if string(b) != gen.RuntimeJS() {
		t.Fatal("hostd/gen/js/twd-rt.js is stale; run `make gen-js`")
	}
}
