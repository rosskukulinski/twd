// Package hostdtest builds twd app bundles for tests (templates and
// fixtures) with the repo's esbuild-based bundler (node required).
package hostdtest

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/rosskukulinski/twd/hostd/bundle"
	"github.com/rosskukulinski/twd/netd/nettest"
)

// Template returns the path of a create-twd template.
func Template(name string) string {
	root, _ := nettest.FindRepoRoot()
	return filepath.Join(root, "js", "packages", "create-twd", "templates", name)
}

// Fixture returns the path of a hostd test fixture app.
func Fixture(name string) string {
	root, _ := nettest.FindRepoRoot()
	return filepath.Join(root, "hostd", "testdata", "apps", name)
}

var buildMu sync.Mutex

// Build bundles the app in dir and returns its files (dist/index.js,
// twd.jsonc, wrangler.jsonc, twd-bundle.json). vars override wrangler vars
// (written into twd-bundle-vars via a rewritten wrangler.json).
func Build(dir string, vars map[string]string) (map[string][]byte, error) {
	buildMu.Lock()
	defer buildMu.Unlock()
	root, err := nettest.FindRepoRoot()
	if err != nil {
		return nil, err
	}
	node, err := exec.LookPath("node")
	if err != nil {
		return nil, fmt.Errorf("node not found: %w", err)
	}
	out, err := os.MkdirTemp("", "twd-build-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(out)
	outfile := filepath.Join(out, "index.js")
	cmd := exec.Command(node, filepath.Join(root, "js", "packages", "create-twd", "lib", "build.mjs"), dir, outfile)
	cmd.Dir = root
	if b, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("build %s: %v: %s", dir, err, b)
	}
	js, err := os.ReadFile(outfile)
	if err != nil {
		return nil, err
	}
	files := map[string][]byte{"dist/index.js": js, "twd-bundle.json": []byte(`{"main":"dist/index.js"}`)}
	for _, f := range []string{"twd.jsonc", "wrangler.jsonc"} {
		if b, err := os.ReadFile(filepath.Join(dir, f)); err == nil {
			files[f] = b
		}
	}
	if len(vars) > 0 {
		var w map[string]any
		if err := bundle.UnmarshalJSONC(files["wrangler.jsonc"], &w); err != nil {
			return nil, err
		}
		v, _ := w["vars"].(map[string]any)
		if v == nil {
			v = map[string]any{}
		}
		for k, val := range vars {
			v[k] = val
		}
		w["vars"] = v
		b, _ := json.MarshalIndent(w, "", "  ")
		files["wrangler.jsonc"] = b
	}
	return files, nil
}

// MustTar packs files.
func MustTar(files map[string][]byte) []byte {
	b, err := bundle.Pack(files)
	if err != nil {
		panic(err)
	}
	return b
}

// ShortDir makes a short 0700 temp dir under /tmp (unix socket path limits).
func ShortDir(prefix string) (string, error) {
	d, err := os.MkdirTemp("/tmp", prefix)
	if err != nil {
		return "", err
	}
	return d, os.Chmod(d, 0o700)
}

// HasNode reports whether node is on PATH.
func HasNode() bool {
	_, err := exec.LookPath("node")
	return err == nil
}

// Trim shortens s for test logs.
func Trim(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
