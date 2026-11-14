// Package imagebuild is twd deploy's container image builder (ADR-032 D3):
// a class whose wrangler "image" (or "images.<name>.dockerfile") is a
// Dockerfile is built on the developer's machine with `docker buildx`,
// once per target architecture, into an OCI image layout, and pushed to
// twd's registry (mgmt's /v2/, over the developer's own tailnet
// connection) in the tenant's namespace. The result is a digest reference
// (twd.local/<tenant>/<repo>@sha256:…, an index when built for several
// arches) that mgmt pins at deploy.
//
// The arm64 alias map is applied as BuildKit named contexts: a Dockerfile
// `FROM docker.io/cloudflare/sandbox:0.12.14` (or `COPY --from=` it, or an
// ARG default naming it) builds for arm64 against twd's rebuild, so the
// app's Dockerfile is used as written. The rebuild comes from the local
// Docker image store when it is there, else from twd's registry (public/)
// as an OCI layout.
//
// Hosts never build: only the developer's machine runs a container engine.
package imagebuild

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/rosskukulinski/twd/internal/container"
	"github.com/rosskukulinski/twd/internal/oci"
	"github.com/rosskukulinski/twd/internal/registry"
)

// Job is one image to build.
type Job struct {
	Class string
	Name  string // images key; "" = the class's default image
	// Dockerfile and Context are absolute paths.
	Dockerfile, Context string
}

// Repo is the registry repository a job pushes to:
// <tenant>/<workload>-<class>[-<name>] in OCI name form.
func (j Job) Repo(tenant, workload string) string {
	n := workload + "-" + j.Class
	if j.Name != "" {
		n += "-" + j.Name
	}
	return tenant + "/" + sanitize(n)
}

var badRepoChars = regexp.MustCompile(`[^a-z0-9]+`)

func sanitize(s string) string {
	s = strings.Trim(badRepoChars.ReplaceAllString(strings.ToLower(s), "-"), "-")
	if s == "" {
		s = "image"
	}
	return s
}

// Plan lists the Dockerfile builds of classes (relative paths resolve
// against appDir, as wrangler resolves them against its config file).
func Plan(appDir string, classes []container.Class) ([]Job, error) {
	var jobs []Job
	add := func(class, name, src string) error {
		p := src
		if !filepath.IsAbs(p) {
			p = filepath.Join(appDir, filepath.FromSlash(src))
		}
		st, err := os.Stat(p)
		if err != nil {
			return fmt.Errorf("containers.%s: %s: %w", class, src, err)
		}
		j := Job{Class: class, Name: name}
		if st.IsDir() {
			j.Context, j.Dockerfile = p, filepath.Join(p, "Dockerfile")
		} else {
			j.Context, j.Dockerfile = filepath.Dir(p), p
		}
		jobs = append(jobs, j)
		return nil
	}
	for _, c := range classes {
		if c.Image == "" && c.Source != "" && container.IsBuildSource(c.Source) {
			if err := add(c.Class, "", c.Source); err != nil {
				return nil, err
			}
		}
		names := make([]string, 0, len(c.ImageSources))
		for n := range c.ImageSources {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			if c.Images[n] == "" {
				if err := add(c.Class, n, c.ImageSources[n]); err != nil {
					return nil, err
				}
			}
		}
	}
	return jobs, nil
}

// Options configure Build.
type Options struct {
	Tenant, Workload string
	// Arches to build for (linux/<arch>).
	Arches  []string
	Aliases []registry.Alias
	// Registry is twd's registry (mgmt /v2/) as the developer: pushes go
	// here, and alias targets absent from the local Docker image store come
	// from here.
	Registry *oci.Client
	// Docker is the docker CLI (default "docker").
	Docker string
	// WorkDir holds the per-arch OCI layouts (default: a temp dir, removed).
	WorkDir string
	// Log receives progress lines and the builder's output.
	Log io.Writer
}

func (o *Options) docker() string {
	if o.Docker != "" {
		return o.Docker
	}
	return "docker"
}

// Result is one built and pushed image.
type Result struct {
	Job       Job
	Ref       string            // twd.local/<repo>@sha256:… (index if several arches)
	Platforms map[string]string // arch → image manifest digest
	Layouts   map[string]string // arch → OCI layout tar (kept only with Options.WorkDir)
}

// BuildAndPush builds every job for every arch and pushes them; the map is
// what DeployRequest.containerImages wants.
func BuildAndPush(ctx context.Context, jobs []Job, o Options) (map[string]registry.Built, []Result, error) {
	work := o.WorkDir
	if work == "" {
		d, err := os.MkdirTemp("", "twd-imagebuild-")
		if err != nil {
			return nil, nil, err
		}
		defer os.RemoveAll(d)
		work = d
	}
	out := map[string]registry.Built{}
	var results []Result
	for _, j := range jobs {
		layouts := map[string]string{}
		for _, arch := range o.Arches {
			p, err := Build(ctx, j, arch, work, o)
			if err != nil {
				return nil, nil, err
			}
			layouts[arch] = p
		}
		r, err := Push(ctx, o.Registry, j.Repo(o.Tenant, o.Workload), layouts, o.Log)
		if err != nil {
			return nil, nil, fmt.Errorf("push %s: %w", j.Repo(o.Tenant, o.Workload), err)
		}
		r.Job, r.Layouts = j, layouts
		results = append(results, *r)
		b := out[j.Class]
		if j.Name == "" {
			b.Image = r.Ref
		} else {
			if b.Images == nil {
				b.Images = map[string]string{}
			}
			b.Images[j.Name] = r.Ref
		}
		out[j.Class] = b
	}
	return out, results, nil
}

// Build runs `docker buildx build` for one job and arch into an OCI layout
// tarball under work, and returns its path.
func Build(ctx context.Context, j Job, arch, work string, o Options) (string, error) {
	df, err := os.ReadFile(j.Dockerfile)
	if err != nil {
		return "", err
	}
	out := filepath.Join(work, sanitize(j.Class+"-"+j.Name)+"-"+arch+".oci.tar")
	args := []string{"buildx", "build", "--platform", "linux/" + arch, "--provenance=false", "--sbom=false",
		"--output", "type=oci,dest=" + out, "-f", j.Dockerfile}
	ctxs, cleanup, err := aliasContexts(ctx, string(df), arch, work, o)
	if err != nil {
		return "", err
	}
	defer cleanup()
	for _, c := range ctxs {
		args = append(args, "--build-context", c)
	}
	args = append(args, j.Context)
	if o.Log != nil {
		fmt.Fprintf(o.Log, "› build      %s (linux/%s): %s %s\n", jobName(j), arch, o.docker(), strings.Join(args, " "))
	}
	cmd := exec.CommandContext(ctx, o.docker(), args...)
	var tail bytes.Buffer
	if o.Log != nil {
		cmd.Stdout, cmd.Stderr = io.MultiWriter(o.Log, &tail), io.MultiWriter(o.Log, &tail)
	} else {
		cmd.Stdout, cmd.Stderr = &tail, &tail
	}
	if err := cmd.Run(); err != nil {
		t := tail.String()
		if len(t) > 2000 {
			t = "…" + t[len(t)-2000:]
		}
		return "", fmt.Errorf("docker buildx build %s for linux/%s: %v\n%s", jobName(j), arch, err, t)
	}
	return out, nil
}

func jobName(j Job) string {
	if j.Name != "" {
		return j.Class + "." + j.Name
	}
	return j.Class
}

// aliasContexts are the --build-context values for the alias map entries
// the Dockerfile names (as written: "docker.io/cloudflare/sandbox:1.0.0"
// or "cloudflare/sandbox:1.0.0"), for arch.
func aliasContexts(ctx context.Context, dockerfile, arch, work string, o Options) ([]string, func(), error) {
	var out []string
	var dirs []string
	cleanup := func() {
		for _, d := range dirs {
			os.RemoveAll(d)
		}
	}
	for _, a := range o.Aliases {
		if a.Arch != "" && a.Arch != arch {
			continue
		}
		r, err := oci.ParseRef(a.From)
		if err != nil {
			continue
		}
		var written []string
		for _, form := range refForms(r) {
			re := regexp.MustCompile(`(^|[\s="'=])` + regexp.QuoteMeta(form) + `($|[\s"'])`)
			if re.MatchString(dockerfile) {
				written = append(written, form)
			}
		}
		if len(written) == 0 {
			continue
		}
		src, dir, err := aliasSource(ctx, a.To, arch, work, o)
		if err != nil {
			cleanup()
			return nil, func() {}, fmt.Errorf("alias %s → %s (linux/%s): %w", a.From, a.To, arch, err)
		}
		if dir != "" {
			dirs = append(dirs, dir)
		}
		for _, w := range written {
			out = append(out, w+"="+src)
		}
		if o.Log != nil {
			fmt.Fprintf(o.Log, "› alias      %s → %s (linux/%s, %s)\n", strings.Join(written, ", "), a.To, arch, strings.SplitN(src, "://", 2)[0])
		}
	}
	return out, cleanup, nil
}

// refForms are the ways a Dockerfile may write r.
func refForms(r oci.Ref) []string {
	tag := ":" + r.Tag
	if r.Digest != "" {
		tag = "@" + r.Digest
	}
	forms := []string{r.Registry + "/" + r.Repository + tag}
	if r.Registry == "docker.io" {
		forms = append(forms, r.Repository+tag)
		if short, ok := strings.CutPrefix(r.Repository, "library/"); ok {
			forms = append(forms, short+tag, "docker.io/"+short+tag)
		}
	}
	return forms
}

// aliasSource is where BuildKit gets an alias target, always as a local
// OCI layout (oci-layout://dir@digest): from twd's registry (public/, the
// very image mgmt pins for this arch), else from the local Docker image
// store (docker save). A docker-image:// context naming a twd.local image
// would make BuildKit try to resolve it over the network first.
func aliasSource(ctx context.Context, to, arch, work string, o Options) (string, string, error) {
	r, err := oci.ParseRef(to)
	if err != nil {
		return "", "", err
	}
	if r.Registry != registry.LocalRegistry {
		return "docker-image://" + to, "", nil // an upstream ref: BuildKit pulls it itself
	}
	dir, err := os.MkdirTemp(work, "alias-")
	if err != nil {
		return "", "", err
	}
	fail := func(err error) (string, string, error) {
		os.RemoveAll(dir)
		return "", "", err
	}
	var src oci.Source
	repo, ref := registry.LocalRepo(o.Tenant, r.Repository), r.Reference()
	var regErr error
	if o.Registry != nil {
		if d, err := oci.Resolve(ctx, o.Registry, repo, ref, arch); err == nil {
			src, ref = o.Registry, d
		} else {
			regErr = err
		}
	}
	if src == nil {
		// Not in twd's registry: the local Docker image, if it has one.
		if localImageArch(ctx, o.docker(), to) != arch {
			return fail(fmt.Errorf("%s is neither in twd's registry (%s: %v) nor a local linux/%s Docker image; the operator pushes it with `twd image push`", to, repo, regErr, arch))
		}
		tar := filepath.Join(dir, "save.tar")
		if b, err := exec.CommandContext(ctx, o.docker(), "image", "save", "-o", tar, to).CombinedOutput(); err != nil {
			return fail(fmt.Errorf("docker image save %s: %v: %s", to, err, b))
		}
		l, err := oci.OpenLayout(tar)
		if err != nil {
			return fail(err)
		}
		defer l.Close()
		d, err := oci.Resolve(ctx, l, "", l.Top.Digest, arch)
		if err != nil {
			return fail(err)
		}
		src, repo, ref = l, "", d
	}
	out := filepath.Join(dir, "layout")
	w := &oci.WriteLayout{Dir: out}
	cp, err := oci.Copy(ctx, src, repo, ref, w, "", []string{arch}, "")
	if err != nil {
		return fail(err)
	}
	fi, err := os.Stat(filepath.Join(out, "blobs", "sha256", oci.Hex(ref)))
	if err != nil {
		return fail(err)
	}
	if err := w.Finish(oci.Descriptor{MediaType: cp.MediaType, Digest: ref, Size: fi.Size()}); err != nil {
		return fail(err)
	}
	return "oci-layout://" + out + "@" + ref, dir, nil
}

// localImageArch is the architecture of a local Docker image ("" = absent).
func localImageArch(ctx context.Context, docker, ref string) string {
	b, err := exec.CommandContext(ctx, docker, "image", "inspect", "--format", "{{json .Architecture}}", ref).Output()
	if err != nil {
		return ""
	}
	var a string
	json.Unmarshal(bytes.TrimSpace(b), &a)
	return a
}

// Push pushes per-arch OCI layouts to repo and returns the digest ref: the
// single image manifest, or an index over every arch.
func Push(ctx context.Context, reg *oci.Client, repo string, layouts map[string]string, log io.Writer) (*Result, error) {
	arches := make([]string, 0, len(layouts))
	for a := range layouts {
		arches = append(arches, a)
	}
	sort.Strings(arches)
	res := &Result{Platforms: map[string]string{}}
	var idx oci.Manifest
	idx.SchemaVersion, idx.MediaType = 2, oci.MediaTypeIndex
	for _, a := range arches {
		l, err := oci.OpenLayout(layouts[a])
		if err != nil {
			return nil, err
		}
		cp, err := oci.Copy(ctx, l, "", l.Top.Digest, reg, repo, []string{a}, "")
		l.Close()
		if err != nil {
			return nil, err
		}
		d := cp.Platforms[a]
		if d == "" {
			return nil, fmt.Errorf("%s: no linux/%s image in the build output", layouts[a], a)
		}
		res.Platforms[a] = d
		if log != nil {
			fmt.Fprintf(log, "› push       %s linux/%s %s (%d MB sent)\n", repo, a, d, cp.Bytes>>20)
		}
		b, mt, _, err := reg.GetManifest(ctx, repo, d)
		if err != nil {
			return nil, err
		}
		idx.Manifests = append(idx.Manifests, oci.Descriptor{MediaType: mt, Digest: d, Size: int64(len(b)), Platform: &oci.Platform{OS: "linux", Architecture: a}})
	}
	if len(arches) == 1 {
		res.Ref = registry.LocalRegistry + "/" + repo + "@" + res.Platforms[arches[0]]
		return res, nil
	}
	ib, _ := json.Marshal(idx)
	d, err := reg.PutManifest(ctx, repo, oci.Digest(ib), oci.MediaTypeIndex, ib)
	if err != nil {
		return nil, err
	}
	res.Ref = registry.LocalRegistry + "/" + repo + "@" + d
	return res, nil
}
