// Package release tests the release packaging helpers in scripts/.
package release

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The release vendors every workerd template's deps, but only @twd/* for
// Node-tier templates (their third-party deps are not in the root
// workspace; vendoring them made `make release` fail after M9).
func TestTemplateRoots(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("needs node")
	}
	root, _ := filepath.Abs("../..")
	cmd := exec.Command(node, "scripts/template-roots.mjs", "js/packages/create-twd/templates")
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("template-roots: %v\n%s", err, out)
	}
	roots := strings.Fields(string(out))
	for _, want := range []string{"@twd/tailnet", "@twd/mcp", "@twd/node", "agents", "ai", "zod", "@modelcontextprotocol/server"} {
		if !slices.Contains(roots, want) {
			t.Errorf("roots lack %s: %v", want, roots)
		}
	}
	for _, not := range []string{"workflow", "@workflow/world-local", "@ai-sdk/workflow"} {
		if slices.Contains(roots, not) {
			t.Errorf("roots include Node-tier-only dep %s: %v", not, roots)
		}
	}
	// Every root must resolve from the repo's node_modules (what
	// vendor-node-modules.mjs copies from), or the release build fails.
	if _, err := os.Stat(filepath.Join(root, "node_modules")); err != nil {
		t.Skip("no node_modules (make deps)")
	}
	for _, r := range roots {
		if _, err := os.Stat(filepath.Join(root, "node_modules", r, "package.json")); err != nil {
			t.Errorf("root %s doesn't resolve from node_modules: %v", r, err)
		}
	}
}
