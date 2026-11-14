//go:build e2eimages

// The developer-machine half of E2E-CONTAINER-REGISTRY (ADR-032 D3): twd
// deploy's image builder (internal/imagebuild, docker buildx) runs on the
// acceptance examples' own Dockerfiles, unmodified, with the arm64 alias
// map as BuildKit named contexts, and writes one OCI layout per image.
// The Lima half (registry_test.go) pushes them through twd's registry,
// deploys, and boots the containers on Firecracker: hosts never run a
// container engine, so the build has to happen here.
//
//	make e2e-container-build   (on the Mac; then make e2e-container-registry in Lima)
package container

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rosskukulinski/twd/hostd/bundle"
	"github.com/rosskukulinski/twd/internal/container"
	"github.com/rosskukulinski/twd/internal/imagebuild"
	"github.com/rosskukulinski/twd/internal/registry"
)

// BuiltImage is one entry of built.json.
type BuiltImage struct {
	App    string `json:"app"`
	Class  string `json:"class"`
	Name   string `json:"name,omitempty"`
	Arch   string `json:"arch"`
	Layout string `json:"layout"` // relative to built.json
	Ms     int64  `json:"buildMs"`
}

func TestBuildExampleImages(t *testing.T) {
	apps, out := os.Getenv("TWD_CTR_APPS"), os.Getenv("TWD_CTR_BUILT")
	if apps == "" || out == "" {
		t.Skip("TWD_CTR_APPS (scripts/e2e-container-prep.sh output) and TWD_CTR_BUILT must be set (make e2e-container-build)")
	}
	arch := os.Getenv("TWD_CTR_ARCH")
	if arch == "" {
		arch = "arm64"
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	var built []BuiltImage
	for _, app := range []string{"v0min", "v1min"} {
		dir := filepath.Join(apps, app)
		raw, err := os.ReadFile(filepath.Join(dir, "wrangler.jsonc"))
		if err != nil {
			t.Fatal(err)
		}
		var w any
		if err := bundle.UnmarshalJSONC(raw, &w); err != nil {
			t.Fatal(err)
		}
		wj, _ := json.Marshal(w)
		classes, err := container.Parse(wj, nil) // no twd.jsonc pins: the Dockerfile flow
		if err != nil {
			t.Fatal(err)
		}
		jobs, err := imagebuild.Plan(dir, classes)
		if err != nil || len(jobs) != 1 {
			t.Fatalf("%s: plan %+v %v", app, jobs, err)
		}
		st := time.Now()
		p, err := imagebuild.Build(ctx, jobs[0], arch, out, imagebuild.Options{Aliases: registry.DefaultAliases, Log: testWriter{t}})
		if err != nil {
			t.Fatal(err)
		}
		final := filepath.Join(out, app+"-"+filepath.Base(p))
		if err := os.Rename(p, final); err != nil {
			t.Fatal(err)
		}
		built = append(built, BuiltImage{App: app, Class: jobs[0].Class, Name: jobs[0].Name, Arch: arch, Layout: filepath.Base(final), Ms: time.Since(st).Milliseconds()})
	}
	b, _ := json.MarshalIndent(built, "", "  ")
	if err := os.WriteFile(filepath.Join(out, "built.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("built.json:\n%s", b)
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(string(p))
	return len(p), nil
}
