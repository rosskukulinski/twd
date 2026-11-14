// Package registry is twd's OCI distribution registry (ADR-032 D3): the
// push/pull subset of the distribution spec v1.1, served by twd-mgmt on its
// tsnet listeners (and the local-dev socket), behind mgmt's WhoIs
// authentication.
//
// Layout:
//   - one repository namespace per tenant ("<tenantID>/<name>…") plus the
//     operator's "public" namespace (shared base images, alias targets);
//   - blobs are content-addressed files under Dir/blobs/sha256 (shared by
//     every namespace, so identical layers are stored once), but a blob or
//     manifest is only visible through a repository it was pushed (or
//     mirrored) into: Meta records those links. A HEAD or GET in tenant B's
//     repository for a digest only tenant A pushed is 404, exactly as if
//     the bytes did not exist, so the shared store reveals nothing;
//   - in-progress uploads are files under Dir/uploads (any replica that
//     shares Dir can finish one).
//
// Authorization is the caller's (Authorize): mgmt admits admins to push and
// pull every namespace and hosts to pull only the namespaces of tenants
// they serve, plus "public".
package registry

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/rosskukulinski/twd/internal/oci"
)

// PublicNamespace holds operator-marked public base images (twd image
// public add) and the alias targets (twd's arm64 rebuilds): readable by
// every host and admin, writable by admins. No tenant may be called this.
const PublicNamespace = "public"

// ManifestRec is one manifest linked into a repository.
type ManifestRec struct {
	Digest    string
	MediaType string
	Size      int64
}

// Meta is the registry's metadata: which repository may see which blob or
// manifest, and tags. mgmt implements it on its store (SQLite or Postgres);
// MemMeta is the in-memory one.
type Meta interface {
	LinkBlob(ctx context.Context, repo, digest string, size int64) error
	// BlobSize reports a blob linked into repo (ok false: not linked).
	BlobSize(ctx context.Context, repo, digest string) (size int64, ok bool, err error)
	LinkManifest(ctx context.Context, repo string, m ManifestRec) error
	GetManifest(ctx context.Context, repo, digest string) (ManifestRec, bool, error)
	SetTag(ctx context.Context, repo, tag, digest string) error
	ResolveTag(ctx context.Context, repo, tag string) (string, bool, error)
	Tags(ctx context.Context, repo string) ([]string, error)
}

// Authorizer decides whether r may pull (push false) or push namespace ns.
// It returns a *Denied (or any error: 403) to refuse.
type Authorizer func(r *http.Request, ns string, push bool) error

// Denied refuses a request.
type Denied struct{ Reason string }

func (d *Denied) Error() string { return d.Reason }

// Registry serves /v2/.
type Registry struct {
	Dir       string
	Meta      Meta
	Authorize Authorizer
	// Pushed, if set, is called after a manifest is stored (audit).
	Pushed func(r *http.Request, repo, reference, digest, mediaType string)
	// MaxBlob bounds one blob (default 16 GiB).
	MaxBlob int64
}

// Namespace is a repository's first path component.
func Namespace(repo string) string {
	ns, _, _ := strings.Cut(repo, "/")
	return ns
}

func (g *Registry) maxBlob() int64 {
	if g.MaxBlob > 0 {
		return g.MaxBlob
	}
	return 16 << 30
}

func (g *Registry) blobPath(d string) string {
	return filepath.Join(g.Dir, "blobs", "sha256", oci.Hex(d))
}

// regErr is a distribution-spec error. The body also carries twd's error
// fields (code, message, status) so the twd CLI reports it like any API
// error.
type regErr struct {
	status int
	code   string // OCI code
	msg    string
}

func (e *regErr) Error() string { return e.code + ": " + e.msg }

func errOf(status int, code, format string, a ...any) *regErr {
	return &regErr{status: status, code: code, msg: fmt.Sprintf(format, a...)}
}

var twdCodes = map[int]string{400: "bad_request", 401: "unauthorized", 403: "forbidden", 404: "not_found", 405: "unsupported", 413: "too_large", 416: "bad_range", 500: "internal"}

func writeErr(w http.ResponseWriter, r *http.Request, err error) {
	var e *regErr
	var d *Denied
	switch {
	case errors.As(err, &e):
	case errors.As(err, &d):
		e = errOf(403, "DENIED", "%s", d.Reason)
	default:
		e = errOf(500, "UNKNOWN", "%v", err)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Twd-Error", twdCodes[e.status])
	w.WriteHeader(e.status)
	if r.Method == http.MethodHead {
		return
	}
	json.NewEncoder(w).Encode(map[string]any{
		"errors": []map[string]any{{"code": e.code, "message": e.msg}},
		"code":   twdCodes[e.status], "message": e.msg, "status": e.status,
		"docURL": "https://twd.dev/errors/registry",
	})
}

// route is a parsed /v2 path.
type route struct {
	kind  string // base, tags, manifest, blob, upload
	repo  string
	ref   string // manifest reference, blob digest, upload id
	isNew bool   // POST /blobs/uploads/
}

func parse(p string) (route, bool) {
	rest, ok := strings.CutPrefix(p, "/v2/")
	if !ok {
		if p == "/v2" {
			return route{kind: "base"}, true
		}
		return route{}, false
	}
	if rest == "" {
		return route{kind: "base"}, true
	}
	if repo, ok := strings.CutSuffix(rest, "/tags/list"); ok {
		return route{kind: "tags", repo: repo}, true
	}
	if i := strings.LastIndex(rest, "/blobs/uploads/"); i >= 0 {
		id := rest[i+len("/blobs/uploads/"):]
		return route{kind: "upload", repo: rest[:i], ref: id, isNew: id == ""}, !strings.Contains(id, "/")
	}
	if strings.HasSuffix(rest, "/blobs/uploads") {
		return route{kind: "upload", repo: strings.TrimSuffix(rest, "/blobs/uploads"), isNew: true}, true
	}
	if i := strings.LastIndex(rest, "/manifests/"); i >= 0 {
		return route{kind: "manifest", repo: rest[:i], ref: rest[i+len("/manifests/"):]}, true
	}
	if i := strings.LastIndex(rest, "/blobs/"); i >= 0 {
		return route{kind: "blob", repo: rest[:i], ref: rest[i+len("/blobs/"):]}, true
	}
	return route{}, false
}

// pushOp reports whether the request writes.
func pushOp(rt route, method string) bool {
	switch rt.kind {
	case "upload":
		return true // even GET status: only the uploader needs it
	case "manifest":
		return method == http.MethodPut || method == http.MethodDelete
	case "blob":
		return method == http.MethodDelete
	}
	return false
}

// ServeHTTP implements the distribution API.
func (g *Registry) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
	rt, ok := parse(r.URL.Path)
	if !ok {
		writeErr(w, r, errOf(404, "NAME_UNKNOWN", "%s is not a registry route", r.URL.Path))
		return
	}
	push := pushOp(rt, r.Method)
	// Authorize before looking at the name: a malformed request from a
	// caller that may not make it at all is a 403, not a 400.
	ns := Namespace(rt.repo)
	if err := g.Authorize(r, ns, push); err != nil {
		writeErr(w, r, err)
		return
	}
	if rt.kind == "base" {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			writeErr(w, r, errOf(405, "UNSUPPORTED", "%s /v2/", r.Method))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("{}\n"))
		return
	}
	if !oci.ValidName(rt.repo) || !strings.Contains(rt.repo, "/") {
		writeErr(w, r, errOf(400, "NAME_INVALID", "repository %q: want <namespace>/<name> (lowercase [a-z0-9._-] components)", rt.repo))
		return
	}
	ctx := r.Context()
	var err error
	switch rt.kind {
	case "tags":
		err = g.tags(w, r, rt)
	case "manifest":
		switch r.Method {
		case http.MethodGet, http.MethodHead:
			err = g.getManifest(ctx, w, r, rt)
		case http.MethodPut:
			err = g.putManifest(ctx, w, r, rt)
		default:
			err = errOf(405, "UNSUPPORTED", "%s manifests is not supported (twd's registry never deletes by request; see docs/containers.md)", r.Method)
		}
	case "blob":
		switch r.Method {
		case http.MethodGet, http.MethodHead:
			err = g.getBlob(ctx, w, r, rt)
		default:
			err = errOf(405, "UNSUPPORTED", "%s blobs is not supported", r.Method)
		}
	case "upload":
		err = g.upload(ctx, w, r, rt)
	}
	if err != nil {
		writeErr(w, r, err)
	}
}

func (g *Registry) tags(w http.ResponseWriter, r *http.Request, rt route) error {
	if r.Method != http.MethodGet {
		return errOf(405, "UNSUPPORTED", "%s tags/list", r.Method)
	}
	tags, err := g.Meta.Tags(r.Context(), rt.repo)
	if err != nil {
		return err
	}
	sort.Strings(tags)
	if last := r.URL.Query().Get("last"); last != "" {
		i := sort.SearchStrings(tags, last)
		for i < len(tags) && tags[i] <= last {
			i++
		}
		tags = tags[i:]
	}
	if n, err := strconv.Atoi(r.URL.Query().Get("n")); err == nil && n >= 0 && n < len(tags) {
		tags = tags[:n]
		if n > 0 {
			w.Header().Set("Link", fmt.Sprintf(`</v2/%s/tags/list?n=%d&last=%s>; rel="next"`, rt.repo, n, tags[n-1]))
		}
	}
	if tags == nil {
		tags = []string{}
	}
	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(map[string]any{"name": rt.repo, "tags": tags})
}

// resolve turns a manifest reference (tag or digest) into the linked record.
func (g *Registry) resolve(ctx context.Context, repo, ref string) (ManifestRec, error) {
	d := ref
	if !oci.ValidDigest(ref) {
		if !oci.ValidTag(ref) {
			return ManifestRec{}, errOf(400, "TAG_INVALID", "bad tag or digest %q", ref)
		}
		dg, ok, err := g.Meta.ResolveTag(ctx, repo, ref)
		if err != nil {
			return ManifestRec{}, err
		}
		if !ok {
			return ManifestRec{}, errOf(404, "MANIFEST_UNKNOWN", "%s:%s is not known", repo, ref)
		}
		d = dg
	}
	m, ok, err := g.Meta.GetManifest(ctx, repo, d)
	if err != nil {
		return m, err
	}
	if !ok {
		return m, errOf(404, "MANIFEST_UNKNOWN", "%s@%s is not known", repo, d)
	}
	return m, nil
}

func (g *Registry) getManifest(ctx context.Context, w http.ResponseWriter, r *http.Request, rt route) error {
	m, err := g.resolve(ctx, rt.repo, rt.ref)
	if err != nil {
		return err
	}
	f, err := os.Open(g.blobPath(m.Digest))
	if err != nil {
		return errOf(404, "MANIFEST_UNKNOWN", "%s@%s: content missing", rt.repo, m.Digest)
	}
	defer f.Close()
	w.Header().Set("Content-Type", m.MediaType)
	w.Header().Set("Docker-Content-Digest", m.Digest)
	w.Header().Set("Content-Length", strconv.FormatInt(m.Size, 10))
	w.Header().Set("ETag", `"`+m.Digest+`"`)
	if r.Method == http.MethodHead {
		return nil
	}
	io.Copy(w, f)
	return nil
}

func (g *Registry) putManifest(ctx context.Context, w http.ResponseWriter, r *http.Request, rt route) error {
	if !oci.ValidDigest(rt.ref) && !oci.ValidTag(rt.ref) {
		return errOf(400, "TAG_INVALID", "bad tag or digest %q", rt.ref)
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, oci.MaxManifest+1))
	if err != nil {
		return err
	}
	if len(b) > oci.MaxManifest {
		return errOf(413, "SIZE_INVALID", "manifest larger than %d bytes", oci.MaxManifest)
	}
	m, err := oci.ParseManifest(b, r.Header.Get("Content-Type"))
	if err != nil {
		return errOf(400, "MANIFEST_INVALID", "%v", err)
	}
	d := oci.Digest(b)
	if oci.ValidDigest(rt.ref) && rt.ref != d {
		return errOf(400, "DIGEST_INVALID", "manifest hashes to %s, not %s", d, rt.ref)
	}
	// Everything it references must already be in this repository: a
	// manifest can't make another namespace's content visible here.
	for _, ref := range m.Refs() {
		if oci.IsManifestType(ref.MediaType) || m.IsIndex() {
			if _, ok, err := g.Meta.GetManifest(ctx, rt.repo, ref.Digest); err != nil {
				return err
			} else if !ok {
				return errOf(400, "MANIFEST_BLOB_UNKNOWN", "manifest %s is not in %s: push it first", ref.Digest, rt.repo)
			}
			continue
		}
		if _, ok, err := g.Meta.BlobSize(ctx, rt.repo, ref.Digest); err != nil {
			return err
		} else if !ok {
			return errOf(400, "MANIFEST_BLOB_UNKNOWN", "blob %s is not in %s: push it first", ref.Digest, rt.repo)
		}
	}
	if err := g.storeBytes(d, b); err != nil {
		return err
	}
	if err := g.Meta.LinkManifest(ctx, rt.repo, ManifestRec{Digest: d, MediaType: m.MediaType, Size: int64(len(b))}); err != nil {
		return err
	}
	if !oci.ValidDigest(rt.ref) {
		if err := g.Meta.SetTag(ctx, rt.repo, rt.ref, d); err != nil {
			return err
		}
	}
	if g.Pushed != nil {
		g.Pushed(r, rt.repo, rt.ref, d, m.MediaType)
	}
	w.Header().Set("Location", "/v2/"+rt.repo+"/manifests/"+d)
	w.Header().Set("Docker-Content-Digest", d)
	w.WriteHeader(201)
	return nil
}

func (g *Registry) getBlob(ctx context.Context, w http.ResponseWriter, r *http.Request, rt route) error {
	if !oci.ValidDigest(rt.ref) {
		return errOf(400, "DIGEST_INVALID", "bad digest %q", rt.ref)
	}
	size, ok, err := g.Meta.BlobSize(ctx, rt.repo, rt.ref)
	if err != nil {
		return err
	}
	if !ok {
		if m, mok, err := g.Meta.GetManifest(ctx, rt.repo, rt.ref); err == nil && mok {
			size, ok = m.Size, true
		}
	}
	if !ok {
		return errOf(404, "BLOB_UNKNOWN", "%s@%s is not known", rt.repo, rt.ref)
	}
	f, err := os.Open(g.blobPath(rt.ref))
	if err != nil {
		return errOf(404, "BLOB_UNKNOWN", "%s@%s: content missing", rt.repo, rt.ref)
	}
	defer f.Close()
	w.Header().Set("Docker-Content-Digest", rt.ref)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("ETag", `"`+rt.ref+`"`)
	w.Header().Set("Cache-Control", "max-age=31536000")
	if r.Method == http.MethodHead {
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		return nil
	}
	http.ServeContent(w, r, "", time.Time{}, f) // Range requests
	return nil
}

// ---- uploads ----

func (g *Registry) uploadPath(id string) string { return filepath.Join(g.Dir, "uploads", id) }

var uploadIDOK = func(id string) bool {
	if len(id) != 32 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

func (g *Registry) upload(ctx context.Context, w http.ResponseWriter, r *http.Request, rt route) error {
	q := r.URL.Query()
	if rt.isNew {
		if r.Method != http.MethodPost {
			return errOf(405, "UNSUPPORTED", "%s blobs/uploads/", r.Method)
		}
		if from, dg := q.Get("from"), q.Get("mount"); dg != "" {
			// Cross-repository mount, only within one namespace (another
			// tenant's link is never borrowed); otherwise a fresh upload.
			if oci.ValidDigest(dg) && Namespace(from) == Namespace(rt.repo) && oci.ValidName(from) {
				if size, ok, err := g.Meta.BlobSize(ctx, from, dg); err == nil && ok {
					if err := g.Meta.LinkBlob(ctx, rt.repo, dg, size); err != nil {
						return err
					}
					w.Header().Set("Location", "/v2/"+rt.repo+"/blobs/"+dg)
					w.Header().Set("Docker-Content-Digest", dg)
					w.WriteHeader(201)
					return nil
				}
			}
		}
		id := newID()
		if err := os.MkdirAll(filepath.Join(g.Dir, "uploads"), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(g.uploadPath(id)+".repo", []byte(rt.repo), 0o600); err != nil {
			return err
		}
		f, err := os.OpenFile(g.uploadPath(id), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		f.Close()
		if dg := q.Get("digest"); dg != "" { // monolithic POST
			if _, err := g.appendTo(id, r.Body, -1); err != nil {
				g.dropUpload(id)
				return err
			}
			return g.finish(ctx, w, r, rt.repo, id, dg)
		}
		g.uploadHeaders(w, rt.repo, id, 0)
		w.WriteHeader(202)
		return nil
	}
	if !uploadIDOK(rt.ref) {
		return errOf(404, "BLOB_UPLOAD_UNKNOWN", "upload %q is not known", rt.ref)
	}
	if owner, err := os.ReadFile(g.uploadPath(rt.ref) + ".repo"); err != nil || string(owner) != rt.repo {
		return errOf(404, "BLOB_UPLOAD_UNKNOWN", "upload %s is not known in %s", rt.ref, rt.repo)
	}
	st, err := os.Stat(g.uploadPath(rt.ref))
	if err != nil {
		return errOf(404, "BLOB_UPLOAD_UNKNOWN", "upload %s is not known", rt.ref)
	}
	switch r.Method {
	case http.MethodGet:
		g.uploadHeaders(w, rt.repo, rt.ref, st.Size())
		w.WriteHeader(204)
		return nil
	case http.MethodDelete:
		g.dropUpload(rt.ref)
		w.WriteHeader(204)
		return nil
	case http.MethodPatch:
		start := int64(-1)
		if cr := r.Header.Get("Content-Range"); cr != "" {
			a, _, ok := strings.Cut(strings.TrimPrefix(cr, "bytes "), "-")
			n, err := strconv.ParseInt(a, 10, 64)
			if !ok || err != nil {
				return errOf(416, "BLOB_UPLOAD_INVALID", "bad Content-Range %q", cr)
			}
			start = n
		}
		size, err := g.appendTo(rt.ref, r.Body, start)
		if err != nil {
			return err
		}
		g.uploadHeaders(w, rt.repo, rt.ref, size)
		w.WriteHeader(202)
		return nil
	case http.MethodPut:
		if _, err := g.appendTo(rt.ref, r.Body, -1); err != nil {
			return err
		}
		return g.finish(ctx, w, r, rt.repo, rt.ref, q.Get("digest"))
	}
	return errOf(405, "UNSUPPORTED", "%s on an upload", r.Method)
}

func (g *Registry) uploadHeaders(w http.ResponseWriter, repo, id string, size int64) {
	w.Header().Set("Location", "/v2/"+repo+"/blobs/uploads/"+id)
	w.Header().Set("Docker-Upload-UUID", id)
	end := size - 1
	if end < 0 {
		end = 0
	}
	w.Header().Set("Range", fmt.Sprintf("0-%d", end))
	w.Header().Set("Content-Length", "0")
}

// appendTo appends body to an upload; start ≥ 0 must equal the current size.
func (g *Registry) appendTo(id string, body io.Reader, start int64) (int64, error) {
	f, err := os.OpenFile(g.uploadPath(id), os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return 0, errOf(404, "BLOB_UPLOAD_UNKNOWN", "upload %s is not known", id)
	}
	defer f.Close()
	st, _ := f.Stat()
	if start >= 0 && start != st.Size() {
		return st.Size(), errOf(416, "BLOB_UPLOAD_INVALID", "chunk starts at %d; the upload has %d bytes", start, st.Size())
	}
	max := g.maxBlob() - st.Size()
	n, err := io.Copy(f, io.LimitReader(body, max+1))
	if err != nil {
		return 0, err
	}
	if n > max {
		return 0, errOf(413, "SIZE_INVALID", "blob larger than %d bytes", g.maxBlob())
	}
	return st.Size() + n, nil
}

func (g *Registry) dropUpload(id string) {
	os.Remove(g.uploadPath(id))
	os.Remove(g.uploadPath(id) + ".repo")
}

// finish verifies an upload against digest and links it into repo.
func (g *Registry) finish(ctx context.Context, w http.ResponseWriter, r *http.Request, repo, id, digest string) error {
	if !oci.ValidDigest(digest) {
		return errOf(400, "DIGEST_INVALID", "finishing an upload needs ?digest=sha256:<hex> (got %q)", digest)
	}
	p := g.uploadPath(id)
	f, err := os.Open(p)
	if err != nil {
		return errOf(404, "BLOB_UPLOAD_UNKNOWN", "upload %s is not known", id)
	}
	h := sha256.New()
	n, err := io.Copy(h, f)
	f.Close()
	if err != nil {
		return err
	}
	if got := "sha256:" + hex.EncodeToString(h.Sum(nil)); got != digest {
		g.dropUpload(id)
		return errOf(400, "DIGEST_INVALID", "the upload hashes to %s, not %s", got, digest)
	}
	if err := os.MkdirAll(filepath.Dir(g.blobPath(digest)), 0o700); err != nil {
		return err
	}
	if _, err := os.Stat(g.blobPath(digest)); err == nil {
		os.Remove(p) // already stored (another namespace, or earlier)
	} else if err := os.Rename(p, g.blobPath(digest)); err != nil {
		return err
	}
	os.Remove(p + ".repo")
	if err := g.Meta.LinkBlob(ctx, repo, digest, n); err != nil {
		return err
	}
	w.Header().Set("Location", "/v2/"+repo+"/blobs/"+digest)
	w.Header().Set("Docker-Content-Digest", digest)
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(201)
	return nil
}

// storeBytes writes content-addressed bytes (manifests) if absent.
func (g *Registry) storeBytes(d string, b []byte) error {
	p := g.blobPath(d)
	if _, err := os.Stat(p); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	tmp := p + ".tmp" + newID()[:8]
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

func newID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// ---- in-process access (mgmt's mirroring; trusted, no Authorize) ----

// HasBlob implements oci.Target.
func (g *Registry) HasBlob(ctx context.Context, repo, digest string) (bool, error) {
	_, ok, err := g.Meta.BlobSize(ctx, repo, digest)
	return ok, err
}

// WriteBlob implements oci.Target. Content another namespace already
// stored is verified anew (r is read in full and must hash to digest).
func (g *Registry) WriteBlob(ctx context.Context, repo, digest string, size int64, r io.Reader) error {
	if !oci.ValidDigest(digest) {
		return fmt.Errorf("bad digest %q", digest)
	}
	if err := os.MkdirAll(filepath.Join(g.Dir, "uploads"), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Join(g.Dir, "uploads"), "mirror-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	n, err := io.Copy(tmp, oci.NewVerifier(io.LimitReader(r, g.maxBlob()+1), digest, size))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(g.blobPath(digest)), 0o700); err != nil {
		return err
	}
	if _, err := os.Stat(g.blobPath(digest)); err != nil {
		if err := os.Rename(tmp.Name(), g.blobPath(digest)); err != nil {
			return err
		}
	}
	return g.Meta.LinkBlob(ctx, repo, digest, n)
}

// WriteManifest implements oci.Target (same checks as a pushed manifest).
func (g *Registry) WriteManifest(ctx context.Context, repo, reference, mediaType string, b []byte) error {
	m, err := oci.ParseManifest(b, mediaType)
	if err != nil {
		return err
	}
	d := oci.Digest(b)
	if oci.ValidDigest(reference) && reference != d {
		return oci.ErrDigestMismatch
	}
	for _, ref := range m.Refs() {
		var ok bool
		if m.IsIndex() {
			_, ok, err = g.Meta.GetManifest(ctx, repo, ref.Digest)
		} else {
			_, ok, err = g.Meta.BlobSize(ctx, repo, ref.Digest)
		}
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("manifest %s references %s, which is not in %s", d, ref.Digest, repo)
		}
	}
	if err := g.storeBytes(d, b); err != nil {
		return err
	}
	if err := g.Meta.LinkManifest(ctx, repo, ManifestRec{Digest: d, MediaType: m.MediaType, Size: int64(len(b))}); err != nil {
		return err
	}
	if !oci.ValidDigest(reference) {
		return g.Meta.SetTag(ctx, repo, reference, d)
	}
	return nil
}

// Manifest implements oci.Source.
func (g *Registry) Manifest(ctx context.Context, repo, reference string) ([]byte, string, string, error) {
	m, err := g.resolve(ctx, repo, reference)
	if err != nil {
		return nil, "", "", &oci.Error{Status: 404, Code: "MANIFEST_UNKNOWN", Message: err.Error()}
	}
	b, err := os.ReadFile(g.blobPath(m.Digest))
	if err != nil {
		return nil, "", "", err
	}
	if oci.Digest(b) != m.Digest {
		return nil, "", "", oci.ErrDigestMismatch
	}
	return b, m.MediaType, m.Digest, nil
}

// Blob implements oci.Source.
func (g *Registry) Blob(ctx context.Context, repo, digest string, size int64) (io.ReadCloser, error) {
	if _, ok, err := g.Meta.BlobSize(ctx, repo, digest); err != nil {
		return nil, err
	} else if !ok {
		return nil, &oci.Error{Status: 404, Code: "BLOB_UNKNOWN", Message: repo + "@" + digest}
	}
	f, err := os.Open(g.blobPath(digest))
	if err != nil {
		return nil, err
	}
	return struct {
		io.Reader
		io.Closer
	}{oci.NewVerifier(f, digest, size), f}, nil
}
