package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"slices"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/rosskukulinski/twd/hostd/bundle"
	"github.com/rosskukulinski/twd/hostd/mgmtapi"
	"github.com/rosskukulinski/twd/internal/container"
	"github.com/rosskukulinski/twd/internal/imagebuild"
	"github.com/rosskukulinski/twd/internal/oci"
	"github.com/rosskukulinski/twd/internal/registry"
	"github.com/rosskukulinski/twd/mgmt"
)

// Container images (ADR-032 D3): twd deploy builds Dockerfile classes on
// this machine and pushes them to twd's registry; `twd image` manages
// public bases and pushes operator images (alias targets) to public/.

// buildContainerImages builds and pushes the bundle's Dockerfile images
// (imagebuild) and returns DeployRequest.containerImages (nil: nothing to
// build). platforms overrides the target arches ("" = the arches of the
// tenant pool's live container hosts).
func buildContainerImages(ctx context.Context, mc *mgmtapi.Client, a *app, bundleTar []byte, tenantID, platforms string, log io.Writer) (map[string]registry.Built, error) {
	files, err := bundle.Unpack(bundleTar)
	if err != nil {
		return nil, err
	}
	ba, err := bundle.Parse(files)
	if err != nil {
		return nil, err
	}
	var classes []container.Class
	for _, c := range ba.Containers {
		classes = append(classes, c)
	}
	sort.Slice(classes, func(i, j int) bool { return classes[i].Class < classes[j].Class })
	jobs, err := imagebuild.Plan(a.Dir, classes)
	if err != nil || len(jobs) == 0 {
		return nil, err
	}
	arches, err := buildArches(ctx, mc, tenantID, platforms, classes)
	if err != nil {
		return nil, err
	}
	var al struct {
		Items []registry.Alias `json:"items"`
	}
	if err := mc.Do(ctx, "GET", "/v1/images/aliases", nil, &al); err != nil {
		return nil, err
	}
	st := time.Now()
	built, results, err := imagebuild.BuildAndPush(ctx, jobs, imagebuild.Options{Tenant: tenantID, Workload: a.Name, Arches: arches,
		Aliases: al.Items, Registry: mc.OCI(), Log: log})
	if err != nil {
		return nil, fail("image_build_failed", err.Error()).withFix("docker buildx version   (twd deploy builds Dockerfile images locally with docker buildx)")
	}
	for _, r := range results {
		fmt.Fprintf(log, "✓ image      %s → %s (%s; %.1fs)\n", jobLabel(r.Job), r.Ref, strings.Join(arches, ", "), time.Since(st).Seconds())
	}
	return built, nil
}

func jobLabel(j imagebuild.Job) string {
	if j.Name != "" {
		return j.Class + ".images." + j.Name
	}
	return j.Class
}

// buildArches: --platform, else the class's stated arch, else the arches
// of the live hosts in the tenant's pool that offer containers.
func buildArches(ctx context.Context, mc *mgmtapi.Client, tenantID, platforms string, classes []container.Class) ([]string, error) {
	var out []string
	if platforms != "" {
		for _, p := range strings.Split(platforms, ",") {
			p = strings.TrimPrefix(strings.TrimSpace(p), "linux/")
			if p != container.ArchAMD64 && p != container.ArchARM64 {
				return nil, fail("bad_request", fmt.Sprintf("--platform %q: want linux/amd64 and/or linux/arm64", p))
			}
			out = append(out, p)
		}
		return out, nil
	}
	if arch, err := container.Arch(classes); err == nil && arch != "" {
		return []string{arch}, nil
	}
	var tn struct {
		Pool string `json:"pool"`
	}
	if err := mc.Do(ctx, "GET", "/v1/tenants/"+tenantID, nil, &tn); err != nil {
		return nil, err
	}
	hosts, err := listAll[struct {
		Status string   `json:"status"`
		Tiers  []string `json:"tiers"`
		Arch   string   `json:"arch"`
		Pool   string   `json:"pool"`
	}](ctx, mc, "/v1/hosts", 0)
	if err != nil {
		return nil, err
	}
	for _, h := range hosts {
		if h.Status == "live" && slices.Contains(h.Tiers, mgmt.CapContainers) && h.Arch != "" && (tn.Pool == "" || h.Pool == tn.Pool) && !slices.Contains(out, h.Arch) {
			out = append(out, h.Arch)
		}
	}
	sort.Strings(out)
	if len(out) == 0 {
		return nil, fail("no_host", "no live host in this tenant's pool offers containers, so there is no arch to build the Dockerfile images for").
			withFix("set up a Linux host with KVM: sudo twd host setup --microvm", "or build anyway: twd deploy --platform linux/arm64")
	}
	return out, nil
}

// ---- twd image ----

func cmdImage(args []string) error {
	// Global flags (--state-dir X, --json) may precede the subcommands:
	// pull the positional words out, keep the rest for the flag parser.
	var words, rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case (a == "--state-dir" || a == "-state-dir") && i+1 < len(args):
			rest = append(rest, a, args[i+1])
			i++
		case strings.HasPrefix(a, "-"):
			rest = append(rest, a)
		case len(words) < 2 && (len(words) == 0 || words[0] == "public"):
			words = append(words, a)
		default:
			rest = append(rest, a)
		}
	}
	if len(words) == 0 {
		return fail("bad_request", "twd image needs a subcommand: public add|ls|rm, push", "usage: twd "+usageOf("image"))
	}
	switch words[0] {
	case "public":
		if len(words) < 2 {
			return fail("bad_request", "twd image public add|ls|rm", "usage: twd "+usageOf("image"))
		}
		return cmdImagePublic(words[1], rest)
	case "push":
		return cmdImagePush(rest)
	}
	args = words
	return fail("bad_request", "unknown twd image subcommand "+args[0], "usage: twd "+usageOf("image"))
}

func cmdImagePublic(sub string, args []string) error {
	f := newFlags("image public " + sub)
	arch := f.String("arch", "", "comma-separated arches to mark (default: every linux amd64/arm64 image the ref has)")
	pos, err := f.parse(args)
	if err != nil {
		return err
	}
	s, err := openState(*f.stateDir)
	if err != nil {
		return err
	}
	mc := s.mgmt()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	type pub = mgmt.PublicImage
	switch sub {
	case "add":
		if len(pos) != 1 {
			return fail("bad_request", "twd image public add <ref> [--arch amd64,arm64]", "usage: twd "+usageOf("image"))
		}
		body := map[string]any{"ref": pos[0]}
		if *arch != "" {
			body["arches"] = strings.Split(*arch, ",")
		}
		var out struct {
			Items []pub `json:"items"`
		}
		if err := mc.DoHeaders(ctx, "POST", "/v1/images/public", body, &out, map[string]string{"Twd-Actor": actor()}); err != nil {
			return err
		}
		emit(f, out, func() {
			for _, p := range out.Items {
				fmt.Printf("✓ public     %s linux/%s → %s@%s (%s)\n", p.Ref, p.Arch, p.Repository, p.Digest, p.ID)
			}
		})
		return nil
	case "ls":
		ps, err := listAll[pub](ctx, mc, "/v1/images/public", 0)
		if err != nil {
			return err
		}
		emit(f, ps, func() {
			tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tREF\tARCH\tREPOSITORY\tDIGEST\tBY")
			for _, p := range ps {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", p.ID, p.Ref, p.Arch, p.Repository, p.Digest[:19]+"…", p.CreatedBy)
			}
			tw.Flush()
		})
		return nil
	case "rm":
		if len(pos) != 1 {
			return fail("bad_request", "twd image public rm <id>", "usage: twd "+usageOf("image"))
		}
		if err := mc.DoHeaders(ctx, "DELETE", "/v1/images/public/"+pos[0], nil, nil, map[string]string{"Twd-Actor": actor()}); err != nil {
			return err
		}
		emit(f, map[string]string{"removed": pos[0]}, func() { fmt.Printf("✓ removed    %s\n", pos[0]) })
		return nil
	}
	return fail("bad_request", "twd image public add|ls|rm", "usage: twd "+usageOf("image"))
}

// cmdImagePush pushes a local image (an OCI layout or `docker save`
// tarball) into twd's registry: twd.local/<name> is the operator's public/
// namespace (e.g. the arm64 alias targets), twd.local/<tenantID>/<name> a
// tenant's.
func cmdImagePush(args []string) error {
	f := newFlags("image push")
	pos, err := f.parse(args)
	if err != nil {
		return err
	}
	if len(pos) != 2 {
		return fail("bad_request", "twd image push <oci-layout|docker-save.tar> twd.local/<name>:<tag>", "usage: twd "+usageOf("image"))
	}
	r, err := oci.ParseRef(pos[1])
	if err != nil || r.Registry != registry.LocalRegistry || r.Tag == "" {
		return fail("bad_request", fmt.Sprintf("%q: want twd.local/<name>:<tag> (public/) or twd.local/<tenantID>/<name>:<tag>", pos[1]))
	}
	s, err := openState(*f.stateDir)
	if err != nil {
		return err
	}
	l, err := oci.OpenLayout(pos[0])
	if err != nil {
		return err
	}
	defer l.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	// twd.local/<tenantID>/… is that tenant's namespace; anything else is
	// the operator's public/ (as mgmt reads twd.local refs at deploy).
	repo := r.Repository
	if ns := registry.Namespace(repo); ns != registry.PublicNamespace {
		if !strings.Contains(repo, "/") || s.mgmt().Do(ctx, "GET", "/v1/tenants/"+ns, nil, nil) != nil {
			repo = registry.PublicNamespace + "/" + repo
		}
	}
	st := time.Now()
	cp, err := oci.Copy(ctx, l, "", l.Top.Digest, s.mgmt().OCI(), repo, nil, r.Tag)
	if err != nil {
		return err
	}
	emit(f, cp, func() {
		archs := make([]string, 0, len(cp.Platforms))
		for a, d := range cp.Platforms {
			archs = append(archs, a+" "+d)
		}
		sort.Strings(archs)
		fmt.Printf("✓ pushed     %s:%s %s (%s; %d MB in %.1fs)\n", repo, r.Tag, cp.Digest, strings.Join(archs, ", "), cp.Bytes>>20, time.Since(st).Seconds())
	})
	return nil
}
