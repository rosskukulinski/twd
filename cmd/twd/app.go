package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/rosskukulinski/twd/hostd/bundle"
	"github.com/rosskukulinski/twd/mgmt"
)

// ---- init ----

var labelRE = regexp.MustCompile(`[^a-z0-9-]+`)

func workloadLabel(s string) string {
	s = labelRE.ReplaceAllString(strings.ToLower(s), "-")
	s = strings.Trim(regexp.MustCompile(`-+`).ReplaceAllString(s, "-"), "-")
	if len(s) > 40 {
		s = strings.TrimRight(s[:40], "-")
	}
	return s
}

func cmdInit(args []string) error {
	f := newFlags("init")
	tmpl := f.String("template", "hello", "hello | agent | mcp | node-workflow")
	name := f.String("name", "", "workload name (default: the directory name)")
	pos, err := f.parse(args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return fail("bad_request", "twd init needs a directory", "usage: twd "+usageOf("init"))
	}
	dir, _ := filepath.Abs(pos[0])
	ct, err := createTwdDir()
	if err != nil {
		return err
	}
	src := filepath.Join(ct, "templates", *tmpl)
	if st, err := os.Stat(src); err != nil || !st.IsDir() {
		ents, _ := os.ReadDir(filepath.Join(ct, "templates"))
		var av []string
		for _, e := range ents {
			av = append(av, e.Name())
		}
		return fail("bad_request", fmt.Sprintf("unknown template %q", *tmpl), "available: "+strings.Join(av, ", "))
	}
	if ents, err := os.ReadDir(dir); err == nil && len(ents) > 0 {
		return fail("bad_request", dir+" exists and is not empty")
	}
	wl := *name
	if wl == "" {
		wl = filepath.Base(dir)
	}
	wl = workloadLabel(wl)
	if wl == "" {
		return fail("bad_request", "can't derive a workload name from "+dir).withFix("twd init <dir> --name <workload>")
	}
	if err := os.CopyFS(dir, os.DirFS(src)); err != nil {
		return err
	}
	// npm-style: templates ship "gitignore" (npm drops .gitignore files).
	if _, err := os.Stat(filepath.Join(dir, "gitignore")); err == nil {
		os.Rename(filepath.Join(dir, "gitignore"), filepath.Join(dir, ".gitignore"))
	}
	// Name the workload after the directory (twd.jsonc and wrangler.jsonc).
	for _, fn := range []string{"twd.jsonc", "wrangler.jsonc"} {
		p := filepath.Join(dir, fn)
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		re := regexp.MustCompile(`("name"\s*:\s*)"[^"]*"`)
		done := false
		b = re.ReplaceAllFunc(b, func(m []byte) []byte {
			if done {
				return m
			}
			done = true
			return re.ReplaceAll(m, []byte(`${1}"`+wl+`"`))
		})
		os.WriteFile(p, b, 0o644)
	}
	res := map[string]string{"dir": dir, "template": *tmpl, "workload": wl}
	emit(f, res, func() {
		fmt.Printf("✓ created %s from template %s (workload %q)\n", dir, *tmpl, wl)
		fmt.Printf("  cd %s && twd deploy        # or `twd dev` for a local run with no tailnet\n", pos[0])
	})
	return nil
}

// ---- app + build ----

type app struct {
	Dir      string
	Name     string
	Cfg      *mgmt.TwdConfig
	Wrangler map[string]any
	TwdRaw   []byte
	// Runtime is "node" for the Node process tier (no wrangler.jsonc).
	Runtime string
}

func loadApp(dir, name string) (*app, error) {
	dir, _ = filepath.Abs(dir)
	a := &app{Dir: dir}
	raw, err := os.ReadFile(filepath.Join(dir, "twd.jsonc"))
	if err != nil {
		return nil, fail("bad_request", "no twd.jsonc in "+dir).withFix("twd init <dir> --template hello|agent|mcp")
	}
	a.TwdRaw = raw
	if a.Cfg, err = mgmt.ParseTwdConfig(raw); err != nil {
		return nil, fail("bad_request", err.Error())
	}
	var top struct {
		Name    string `json:"name"`
		Runtime string `json:"runtime"`
	}
	bundle.UnmarshalJSONC(raw, &top)
	a.Runtime = top.Runtime
	a.Wrangler = map[string]any{}
	if a.Runtime != bundle.RuntimeNode {
		wb, err := os.ReadFile(filepath.Join(dir, "wrangler.jsonc"))
		if err != nil {
			return nil, fail("bad_request", "no wrangler.jsonc in "+dir)
		}
		if err := bundle.UnmarshalJSONC(wb, &a.Wrangler); err != nil {
			return nil, fail("bad_request", "wrangler.jsonc: "+err.Error())
		}
	}
	a.Name = name
	if a.Name == "" {
		a.Name = top.Name
	}
	if a.Name == "" {
		a.Name, _ = a.Wrangler["name"].(string)
	}
	if a.Name == "" {
		a.Name = filepath.Base(dir)
	}
	if wl := workloadLabel(a.Name); wl != a.Name {
		return nil, fail("bad_request", fmt.Sprintf("workload name %q is not a DNS label", a.Name)).withFix("twd deploy --name " + wl)
	}
	if len(a.Cfg.Tailnet.Tags) == 0 {
		return nil, fail("bad_request", "twd.jsonc has no tailnet.tags (the workload node's tags)").
			withFix(`add "tailnet": { "tags": ["tag:twd-wl-` + a.Name + `"] }`)
	}
	return a, nil
}

// srcTags are every tag this app's nodes will carry.
func (a *app) srcTags() []string {
	var out []string
	for _, c := range a.Cfg.Classes() {
		for _, t := range c.Tags {
			if !slices.Contains(out, t) {
				out = append(out, t)
			}
		}
	}
	return out
}

// build bundles the app with create-twd's esbuild bundler (as hostd/gen
// expects: dist/index.js + twd.jsonc + wrangler.jsonc + twd-bundle.json)
// and packs it.
func (a *app) build(vars map[string]string) ([]byte, int, error) {
	ct, err := createTwdDir()
	if err != nil {
		return nil, 0, err
	}
	node, err := exec.LookPath("node")
	if err != nil {
		return nil, 0, fail("not_installed", "node is needed to bundle apps (esbuild)").withFix("install Node.js ≥ 22")
	}
	out, err := os.MkdirTemp("", "twd-build-")
	if err != nil {
		return nil, 0, err
	}
	defer os.RemoveAll(out)
	if a.Runtime == bundle.RuntimeNode {
		return a.buildNode(node, ct, out, vars)
	}
	outfile := filepath.Join(out, "index.js")
	cmd := exec.Command(node, filepath.Join(ct, "lib", "build.mjs"), a.Dir, outfile)
	cmd.Dir = a.Dir
	cmd.Env = append(os.Environ(), "TWD_NODE_PATH="+strings.Join(nodePaths(ct), ":"))
	if b, err := cmd.CombinedOutput(); err != nil {
		return nil, 0, fail("build_failed", "esbuild failed for "+a.Dir, strings.Split(strings.TrimSpace(string(b)), "\n")...)
	}
	js, err := os.ReadFile(outfile)
	if err != nil {
		return nil, 0, err
	}
	files := map[string][]byte{"dist/index.js": js, "twd-bundle.json": []byte(`{"main":"dist/index.js"}`), "twd.jsonc": a.TwdRaw}
	w := map[string]any{}
	for k, v := range a.Wrangler {
		w[k] = v
	}
	if len(vars) > 0 {
		v, _ := w["vars"].(map[string]any)
		nv := map[string]any{}
		for k, x := range v {
			nv[k] = x
		}
		for k, x := range vars {
			nv[k] = x
		}
		w["vars"] = nv
	}
	wb, _ := json.MarshalIndent(w, "", "  ")
	files["wrangler.jsonc"] = wb
	tb, err := bundle.Pack(files)
	return tb, len(js), err
}

// buildNode bundles a runtime "node" app with create-twd's build-node.mjs
// (dist/server.mjs, plus the Vercel Workflow bundles when workflows/ exists).
func (a *app) buildNode(node, ct, out string, vars map[string]string) ([]byte, int, error) {
	args := []string{filepath.Join(ct, "lib", "build-node.mjs"), a.Dir, filepath.Join(out, "app")}
	for k, v := range vars {
		args = append(args, "--var", k+"="+v)
	}
	cmd := exec.Command(node, args...)
	cmd.Dir = a.Dir
	cmd.Env = append(os.Environ(), "TWD_NODE_PATH="+strings.Join(nodePaths(ct), ":"))
	if b, err := cmd.CombinedOutput(); err != nil {
		return nil, 0, fail("build_failed", "build-node failed for "+a.Dir, strings.Split(strings.TrimSpace(string(b)), "\n")...).
			withFix("cd " + a.Dir + " && npm install")
	}
	files, err := bundle.ReadDir(filepath.Join(out, "app"), ".")
	if err != nil {
		return nil, 0, err
	}
	tb, err := bundle.Pack(files)
	return tb, len(files["dist/server.mjs"]), err
}

// ---- deploy ----

type varFlags map[string]string

func (v varFlags) String() string { return fmt.Sprint(map[string]string(v)) }
func (v varFlags) Set(s string) error {
	k, val, ok := strings.Cut(s, "=")
	if !ok || k == "" {
		return fmt.Errorf("--var wants KEY=VALUE")
	}
	v[k] = val
	return nil
}

type deployResult struct {
	Workload     string               `json:"workload"`
	DeploymentID string               `json:"deploymentID"`
	Host         string               `json:"host"`
	Status       string               `json:"status"`
	Endpoint     any                  `json:"endpoint"`
	Preflight    mgmt.PreflightResult `json:"preflight"`
	BundleBytes  int                  `json:"bundleBytes"`
	Timings      map[string]float64   `json:"timingsMs"`
}

func cmdDeploy(args []string) error {
	f := newFlags("deploy")
	name := f.String("name", "", "workload name (default: twd.jsonc name)")
	allowMissing := f.Bool("allow-missing-grants", false, "deploy even if tailnet.connects grants are missing")
	noWait := f.Bool("no-wait", false, "return once mgmt accepted the deploy")
	wait := f.Duration("wait", 3*time.Minute, "how long to wait for ready")
	vars := varFlags{}
	f.Var(vars, "var", "override a wrangler var (KEY=VALUE, repeatable)")
	pos, err := f.parse(args)
	if err != nil {
		return err
	}
	dir := "."
	if len(pos) > 0 {
		dir = pos[0]
	}
	s, err := openState(*f.stateDir)
	if err != nil {
		return err
	}
	if err := s.requireUp(); err != nil {
		return err
	}
	a, err := loadApp(dir, *name)
	if err != nil {
		return err
	}
	t0 := time.Now()
	res := deployResult{Workload: a.Name, Timings: map[string]float64{}}
	ctx, cancel := context.WithTimeout(context.Background(), *wait+time.Minute)
	defer cancel()
	mc := s.mgmt()

	// Preflight tailnet.connects and tagOwners against the tenant policy.
	var pf mgmt.PreflightResult
	if err := mc.Do(ctx, "POST", "/v0/tenants/"+s.Cfg.TenantID+"/preflight",
		mgmt.PreflightRequest{SrcTags: a.srcTags(), Connects: a.Cfg.Tailnet.Connects}, &pf); err != nil {
		return err
	}
	res.Preflight = pf
	res.Timings["preflight"] = ms(time.Since(t0))
	if !*f.json {
		printPreflight(a, pf)
	}
	if !pf.OK && !*allowMissing {
		e := fail("acl_denied", "preflight failed: the tenant policy is missing grants or tagOwners this app needs")
		if len(pf.MissingTagOwners) > 0 {
			e.Fix = append(e.Fix, "add to your tailnet policy:", "  "+pf.TagOwnersSnippet)
		}
		if len(pf.Missing) > 0 {
			e.Fix = append(e.Fix, `add to "grants" in your tailnet policy:`)
			for _, m := range pf.Missing {
				e.Fix = append(e.Fix, "  "+m.Snippet)
			}
		}
		e.Fix = append(e.Fix, "then re-run, or deploy anyway with --allow-missing-grants")
		return e
	}

	bs := time.Now()
	tb, jsBytes, err := a.build(vars)
	if err != nil {
		return err
	}
	res.BundleBytes = jsBytes
	res.Timings["build"] = ms(time.Since(bs))
	if !*f.json {
		main := "dist/index.js"
		if a.Runtime == bundle.RuntimeNode {
			main = "dist/server.mjs (runtime node)"
		}
		fmt.Printf("✓ build      %s %.1f KB (esbuild) · bundle %.1f KB\n", main, float64(jsBytes)/1024, float64(len(tb))/1024)
	}
	ds := time.Now()
	var dr mgmt.DeployResponse
	if err := mc.Do(ctx, "POST", "/v0/deployments", map[string]any{"tenantID": s.Cfg.TenantID, "workload": a.Name, "bundle": tb}, &dr); err != nil {
		return err
	}
	res.DeploymentID, res.Host = dr.DeploymentID, dr.Host
	res.Timings["deploy_api"] = ms(time.Since(ds))
	if *noWait {
		res.Status = "assigned"
		emit(f, res, func() {
			fmt.Printf("✓ deploying  %s@%s (%s) · `twd status` to follow\n", a.Name, dr.Host, dr.DeploymentID)
		})
		return nil
	}
	var v mgmt.DeploymentView
	activeAt := time.Duration(0)
	for {
		if err := mc.Do(ctx, "GET", "/v0/deployments/"+dr.DeploymentID, nil, &v); err != nil {
			return err
		}
		if v.Status == "failed" {
			return fail("deploy_failed", "the host could not start "+a.Name, strings.Split(v.Error, "\n")...).withFix("twd logs " + a.Name)
		}
		if v.Status == "active" && activeAt == 0 {
			activeAt = time.Since(ds)
			res.Timings["active"] = ms(activeAt)
		}
		if v.Status == "active" && v.Endpoint.Status == "ready" && v.Endpoint.MagicDNS != "" {
			break
		}
		if v.Endpoint.Status == "failed" || v.Endpoint.Status == "killed" {
			return fail("bootstrap_failed", "the workload node for "+a.Name+" is "+v.Endpoint.Status).withFix("twd logs --component hostd")
		}
		if time.Since(ds) > *wait {
			return fail("identity_pending", fmt.Sprintf("%s not ready after %v (status %s, node %s)", a.Name, *wait, v.Status, v.Endpoint.Status)).withFix("twd status", "twd logs "+a.Name)
		}
		time.Sleep(100 * time.Millisecond)
	}
	res.Status, res.Endpoint = v.Status, v.Endpoint
	res.Timings["ready"] = ms(time.Since(ds))
	res.Timings["total"] = ms(time.Since(t0))
	emit(f, res, func() {
		fmt.Printf("✓ deployed   %s@%s · node %s · http://%s/  (deploy → ready %.1fs)\n", a.Name, v.Host, v.Endpoint.Node, v.Endpoint.MagicDNS, res.Timings["ready"]/1000)
	})
	return nil
}

func printPreflight(a *app, pf mgmt.PreflightResult) {
	if len(a.Cfg.Tailnet.Connects) == 0 && len(pf.MissingTagOwners) == 0 {
		fmt.Printf("✓ preflight  no tailnet.connects declared · tagOwners ok for %s\n", strings.Join(a.srcTags(), ", "))
		return
	}
	for _, t := range pf.MissingTagOwners {
		fmt.Printf("› preflight: tagOwners %s … ✗ not in tagOwners (minting its nodes would fail)\n", t)
	}
	missing := map[string]bool{}
	for _, m := range pf.Missing {
		missing[m.Src+"→"+m.Dst] = true
		fmt.Printf("› preflight: %s → %s tcp:80 … ✗ no grant\n", m.Src, m.Dst)
	}
	for _, dst := range a.Cfg.Tailnet.Connects {
		if slices.Contains(pf.Unchecked, dst) {
			fmt.Printf("› preflight: → %s … ? a name, not a tag (not checked; name it by tag in tailnet.connects)\n", dst)
			continue
		}
		var ok []string
		for _, src := range a.srcTags() {
			if !missing[src+"→"+dst] {
				ok = append(ok, src)
			}
		}
		if len(ok) > 0 {
			fmt.Printf("✓ preflight  %s → %s granted\n", strings.Join(ok, ", "), dst)
		}
	}
	if len(pf.Missing) > 0 || len(pf.MissingTagOwners) > 0 {
		if len(pf.MissingTagOwners) > 0 {
			fmt.Printf("  add to your tailnet policy:\n    %s\n", pf.TagOwnersSnippet)
		}
		if len(pf.Missing) > 0 {
			fmt.Printf("  add to \"grants\" in your tailnet policy:\n")
			for _, m := range pf.Missing {
				fmt.Printf("    %s\n", m.Snippet)
			}
		}
	}
}
