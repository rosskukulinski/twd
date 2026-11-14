package oci

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Client speaks the OCI distribution API to one registry.
type Client struct {
	// Base is the registry's origin ("https://registry-1.docker.io",
	// "http://mgmt" for twd's unix socket).
	Base string
	HTTP *http.Client
	// Edit, if set, adds headers to every request (twd: the host token or
	// host ID on the local-dev socket; tailnet callers need nothing, mgmt
	// authenticates the connection by WhoIs).
	Edit func(*http.Request)
	// Anonymous bearer tokens (Docker Hub, GHCR: the WWW-Authenticate
	// challenge flow, no credentials) are fetched and cached per scope.
	mu     sync.Mutex
	tokens map[string]string
}

// RegistryBase maps a registry host to its API origin (docker.io's API is
// registry-1.docker.io; everything else is https://<host>). insecure hosts
// (tests: a local upstream) use plain http.
func RegistryBase(host string, insecure bool) string {
	if host == "docker.io" {
		host = "registry-1.docker.io"
	}
	if insecure {
		return "http://" + host
	}
	return "https://" + host
}

// Error is a registry error response (the distribution spec's error codes).
type Error struct {
	Status  int
	Code    string // BLOB_UNKNOWN, MANIFEST_UNKNOWN, DENIED, …
	Message string
}

func (e *Error) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("registry: HTTP %d %s", e.Status, e.Message)
	}
	return fmt.Sprintf("registry: %d %s: %s", e.Status, e.Code, e.Message)
}

// IsNotFound reports whether err is a registry 404.
func IsNotFound(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Status == 404
}

func (c *Client) hc() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

func (c *Client) do(ctx context.Context, method, path, repo string, hdr http.Header, body func() io.Reader, size int64) (*http.Response, error) {
	u := path
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		u = c.Base + path
	}
	// body is called once per attempt (a 401 challenge retries once), so
	// every body twd sends can be produced again.
	for attempt := 0; ; attempt++ {
		var rb io.Reader
		if body != nil {
			rb = body()
		}
		req, err := http.NewRequestWithContext(ctx, method, u, rb)
		if err != nil {
			return nil, err
		}
		if size >= 0 && rb != nil {
			req.ContentLength = size
		}
		for k, v := range hdr {
			req.Header[k] = v
		}
		if c.Edit != nil {
			c.Edit(req)
		}
		c.mu.Lock()
		tok := c.tokens[repo]
		c.mu.Unlock()
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err := c.hc().Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 {
			ch := resp.Header.Get("WWW-Authenticate")
			resp.Body.Close()
			if err := c.authenticate(ctx, repo, ch); err != nil {
				return nil, err
			}
			continue
		}
		return resp, nil
	}
}

// authenticate answers a Bearer challenge with an anonymous token.
func (c *Client) authenticate(ctx context.Context, repo, challenge string) error {
	scheme, params := parseChallenge(challenge)
	if !strings.EqualFold(scheme, "bearer") || params["realm"] == "" {
		return &Error{Status: 401, Code: "UNAUTHORIZED", Message: "registry wants " + challenge}
	}
	q := url.Values{}
	if s := params["service"]; s != "" {
		q.Set("service", s)
	}
	scope := params["scope"]
	if scope == "" && repo != "" {
		scope = "repository:" + repo + ":pull"
	}
	if scope != "" {
		q.Set("scope", scope)
	}
	u := params["realm"]
	if strings.Contains(u, "?") {
		u += "&" + q.Encode()
	} else {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return err
	}
	resp, err := c.hc().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return &Error{Status: resp.StatusCode, Code: "UNAUTHORIZED", Message: "token endpoint refused anonymous access"}
	}
	var t struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&t); err != nil {
		return err
	}
	if t.Token == "" {
		t.Token = t.AccessToken
	}
	if t.Token == "" {
		return errors.New("registry token endpoint returned no token")
	}
	c.mu.Lock()
	if c.tokens == nil {
		c.tokens = map[string]string{}
	}
	c.tokens[repo] = t.Token
	c.mu.Unlock()
	return nil
}

func parseChallenge(h string) (string, map[string]string) {
	scheme, rest, _ := strings.Cut(strings.TrimSpace(h), " ")
	params := map[string]string{}
	for rest != "" {
		rest = strings.TrimLeft(rest, " ,")
		k, v, ok := strings.Cut(rest, "=")
		if !ok {
			break
		}
		k = strings.TrimSpace(k)
		if strings.HasPrefix(v, `"`) {
			end := strings.Index(v[1:], `"`)
			if end < 0 {
				params[k] = v[1:]
				break
			}
			params[k], rest = v[1:end+1], v[end+2:]
		} else {
			val, r, _ := strings.Cut(v, ",")
			params[k], rest = val, r
		}
	}
	return scheme, params
}

func respError(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	e := &Error{Status: resp.StatusCode}
	var doc struct {
		Errors []struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if json.Unmarshal(b, &doc) == nil && len(doc.Errors) > 0 {
		e.Code, e.Message = doc.Errors[0].Code, doc.Errors[0].Message
	} else {
		e.Message = strings.TrimSpace(string(b))
	}
	return e
}

func esc(repo string) string { return repo } // names are [a-z0-9._/-]: nothing to escape

// GetManifest fetches repo's manifest by tag or digest. A digest reference
// is verified; a tag's digest is the hash of the bytes returned.
func (c *Client) GetManifest(ctx context.Context, repo, reference string) (body []byte, mediaType, digest string, err error) {
	resp, err := c.do(ctx, "GET", "/v2/"+esc(repo)+"/manifests/"+reference, repo, http.Header{"Accept": {ManifestAccept}}, nil, -1)
	if err != nil {
		return nil, "", "", err
	}
	if resp.StatusCode != 200 {
		return nil, "", "", respError(resp)
	}
	defer resp.Body.Close()
	body, err = io.ReadAll(io.LimitReader(resp.Body, MaxManifest+1))
	if err != nil {
		return nil, "", "", err
	}
	if len(body) > MaxManifest {
		return nil, "", "", fmt.Errorf("manifest %s@%s is larger than %d bytes", repo, reference, MaxManifest)
	}
	digest = Digest(body)
	if ValidDigest(reference) && digest != reference {
		return nil, "", "", fmt.Errorf("%w: manifest %s@%s hashes to %s", ErrDigestMismatch, repo, reference, digest)
	}
	mediaType = strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0])
	if !IsManifestType(mediaType) {
		var probe struct {
			MediaType string `json:"mediaType"`
		}
		json.Unmarshal(body, &probe)
		mediaType = probe.MediaType
	}
	return body, mediaType, digest, nil
}

// HeadManifest resolves a tag or digest to (digest, mediaType, size)
// without the body.
func (c *Client) HeadManifest(ctx context.Context, repo, reference string) (string, string, int64, error) {
	resp, err := c.do(ctx, "HEAD", "/v2/"+esc(repo)+"/manifests/"+reference, repo, http.Header{"Accept": {ManifestAccept}}, nil, -1)
	if err != nil {
		return "", "", 0, err
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", "", 0, &Error{Status: resp.StatusCode, Code: map[int]string{404: "MANIFEST_UNKNOWN", 403: "DENIED"}[resp.StatusCode]}
	}
	return resp.Header.Get("Docker-Content-Digest"), resp.Header.Get("Content-Type"), resp.ContentLength, nil
}

// GetBlob opens a blob. The reader verifies the digest (and size, if ≥ 0):
// its final Read fails unless the content matches.
func (c *Client) GetBlob(ctx context.Context, repo, digest string, size int64) (io.ReadCloser, error) {
	resp, err := c.do(ctx, "GET", "/v2/"+esc(repo)+"/blobs/"+digest, repo, nil, nil, -1)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, respError(resp)
	}
	return struct {
		io.Reader
		io.Closer
	}{NewVerifier(resp.Body, digest, size), resp.Body}, nil
}

// BlobExists is HEAD /v2/<repo>/blobs/<digest>.
func (c *Client) BlobExists(ctx context.Context, repo, digest string) (bool, error) {
	resp, err := c.do(ctx, "HEAD", "/v2/"+esc(repo)+"/blobs/"+digest, repo, nil, nil, -1)
	if err != nil {
		return false, err
	}
	resp.Body.Close()
	switch resp.StatusCode {
	case 200:
		return true, nil
	case 404:
		return false, nil
	}
	return false, &Error{Status: resp.StatusCode, Code: map[int]string{403: "DENIED", 401: "UNAUTHORIZED"}[resp.StatusCode]}
}

// PutBlob uploads a blob (POST, then one PUT with the content) unless the
// repo already has it. open is called for each attempt.
func (c *Client) PutBlob(ctx context.Context, repo, digest string, size int64, open func() (io.ReadCloser, error)) error {
	if ok, err := c.BlobExists(ctx, repo, digest); err != nil {
		var e *Error
		if !errors.As(err, &e) || e.Status != 401 && e.Status != 403 {
			return err
		}
		// A HEAD refusal has no body: the POST below says why.
	} else if ok {
		return nil
	}
	resp, err := c.do(ctx, "POST", "/v2/"+esc(repo)+"/blobs/uploads/", repo, nil, func() io.Reader { return http.NoBody }, 0)
	if err != nil {
		return err
	}
	if resp.StatusCode != 202 {
		return respError(resp)
	}
	resp.Body.Close()
	loc, err := c.location(resp)
	if err != nil {
		return err
	}
	rc, err := open()
	if err != nil {
		return err
	}
	defer rc.Close()
	u := loc + sep(loc) + "digest=" + url.QueryEscape(digest)
	used := false
	resp, err = c.do(ctx, "PUT", u, repo, http.Header{"Content-Type": {"application/octet-stream"}}, func() io.Reader {
		if used { // a 401 retry: reopen
			rc.Close()
			if r2, err := open(); err == nil {
				rc = r2
			}
		}
		used = true
		return rc
	}, size)
	if err != nil {
		return err
	}
	if resp.StatusCode != 201 {
		return respError(resp)
	}
	resp.Body.Close()
	return nil
}

// PutBlobBytes uploads b.
func (c *Client) PutBlobBytes(ctx context.Context, repo string, b []byte) (string, error) {
	d := Digest(b)
	return d, c.PutBlob(ctx, repo, d, int64(len(b)), func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(b)), nil })
}

// PutManifest stores a manifest under reference (a tag or its digest) and
// returns its digest.
func (c *Client) PutManifest(ctx context.Context, repo, reference, mediaType string, b []byte) (string, error) {
	resp, err := c.do(ctx, "PUT", "/v2/"+esc(repo)+"/manifests/"+reference, repo, http.Header{"Content-Type": {mediaType}},
		func() io.Reader { return bytes.NewReader(b) }, int64(len(b)))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != 201 {
		return "", respError(resp)
	}
	resp.Body.Close()
	d := Digest(b)
	if got := resp.Header.Get("Docker-Content-Digest"); got != "" && got != d {
		return "", fmt.Errorf("%w: registry stored the manifest as %s, it hashes to %s", ErrDigestMismatch, got, d)
	}
	return d, nil
}

// Tags lists a repository's tags.
func (c *Client) Tags(ctx context.Context, repo string) ([]string, error) {
	resp, err := c.do(ctx, "GET", "/v2/"+esc(repo)+"/tags/list", repo, nil, nil, -1)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, respError(resp)
	}
	defer resp.Body.Close()
	var out struct {
		Tags []string `json:"tags"`
	}
	return out.Tags, json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&out)
}

func (c *Client) location(resp *http.Response) (string, error) {
	loc := resp.Header.Get("Location")
	if loc == "" {
		return "", errors.New("registry: upload without a Location")
	}
	u, err := url.Parse(loc)
	if err != nil {
		return "", err
	}
	if u.IsAbs() {
		return loc, nil
	}
	base, _ := url.Parse(c.Base)
	return base.ResolveReference(u).String(), nil
}

func sep(u string) string {
	if strings.Contains(u, "?") {
		return "&"
	}
	return "?"
}

// Ping is GET /v2/ (200 = speaks the distribution API and admits us).
func (c *Client) Ping(ctx context.Context) error {
	resp, err := c.do(ctx, "GET", "/v2/", "", nil, nil, -1)
	if err != nil {
		return err
	}
	if resp.StatusCode != 200 {
		return respError(resp)
	}
	resp.Body.Close()
	return nil
}

// DefaultHTTP is the client used for upstream registries (mirroring).
var DefaultHTTP = &http.Client{Timeout: 10 * time.Minute}
