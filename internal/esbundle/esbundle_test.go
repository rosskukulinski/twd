package esbundle

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/rosskukulinski/twd/hostd/bundle"
)

func repoRoot(t *testing.T) string {
	d, _ := filepath.Abs("../..")
	if _, err := os.Stat(filepath.Join(d, "node_modules", "agents")); err != nil {
		t.Skip("needs node_modules (make deps)")
	}
	return d
}

// TestMatchesBuildMJS: the Go bundler produces the same module as
// create-twd's build.mjs for every template (and works with no node).
func TestMatchesBuildMJS(t *testing.T) {
	root := repoRoot(t)
	node, nodeErr := exec.LookPath("node")
	ents, _ := os.ReadDir(filepath.Join(root, "js", "packages", "create-twd", "templates"))
	if len(ents) < 3 {
		t.Fatalf("templates: %d", len(ents))
	}
	for _, en := range ents {
		tmpl := en.Name()
		t.Run(tmpl, func(t *testing.T) {
			dir := filepath.Join(root, "js", "packages", "create-twd", "templates", tmpl)
			if tb, err := os.ReadFile(filepath.Join(dir, "twd.jsonc")); err == nil {
				var c struct {
					Runtime string `json:"runtime"`
				}
				if bundle.UnmarshalJSONC(tb, &c) == nil && c.Runtime == "node" {
					t.Skip("runtime \"node\" template: bundled by build-node.mjs, not esbundle")
				}
			}
			wb, err := os.ReadFile(filepath.Join(dir, "wrangler.jsonc"))
			if err != nil {
				t.Fatal(err)
			}
			var w struct {
				Main string `json:"main"`
			}
			if err := bundle.UnmarshalJSONC(wb, &w); err != nil {
				t.Fatal(err)
			}
			out := t.TempDir()
			goOut := filepath.Join(out, "go.js")
			n, err := Build(Options{Dir: dir, Main: w.Main, Outfile: goOut})
			if err != nil {
				t.Fatal(err)
			}
			if n < 100 {
				t.Fatalf("tiny bundle: %d bytes", n)
			}
			gb, _ := os.ReadFile(goOut)
			if bytes.Contains(gb, []byte(`from "fs"`)) {
				t.Fatalf("unmapped builtin or require() in the bundle")
			}
			if nodeErr != nil {
				t.Logf("no node: skipping the build.mjs comparison")
				return
			}
			jsOut := filepath.Join(out, "js.js")
			cmd := exec.Command(node, filepath.Join(root, "js", "packages", "create-twd", "lib", "build.mjs"), dir, jsOut)
			if b, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("build.mjs: %v %s", err, b)
			}
			jb, _ := os.ReadFile(jsOut)
			if !bytes.Equal(gb, jb) {
				t.Fatalf("Go bundle (%d bytes) differs from build.mjs (%d bytes)", len(gb), len(jb))
			}
		})
	}
}

func TestBuildError(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "x.ts"), []byte(`import "does-not-exist"; export default {}`), 0o644)
	if _, err := Build(Options{Dir: dir, Main: "x.ts", Outfile: filepath.Join(dir, "o.js")}); err == nil {
		t.Fatal("want an error for an unresolvable import")
	}
	if _, err := Build(Options{Dir: dir, Outfile: filepath.Join(dir, "o.js")}); err == nil {
		t.Fatal("want an error with no main")
	}
}
