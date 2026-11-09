package main

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/rosskukulinski/twd/hostd/mgmtapi"
)

// Helpers for the /v1 API (api/openapi.yaml): paged lists and the host
// join flow.

// page is the /v1 list envelope.
type page[T any] struct {
	Items      []T     `json:"items"`
	NextCursor *string `json:"nextCursor"`
}

// listAll fetches a /v1 list, following nextCursor. max > 0 stops after
// that many items (twd ls --limit); 0 fetches everything (--all).
func listAll[T any](ctx context.Context, c *mgmtapi.Client, path string, max int) ([]T, error) {
	var out []T
	cursor := ""
	for {
		u := path
		sep := "?"
		if strings.Contains(u, "?") {
			sep = "&"
		}
		limit := 500
		if max > 0 && max-len(out) < limit {
			limit = max - len(out)
		}
		u += sep + "limit=" + strconv.Itoa(limit)
		if cursor != "" {
			u += "&cursor=" + url.QueryEscape(cursor)
		}
		var pg page[T]
		if err := c.Do(ctx, "GET", u, nil, &pg); err != nil {
			return out, err
		}
		out = append(out, pg.Items...)
		if pg.NextCursor == nil || *pg.NextCursor == "" || (max > 0 && len(out) >= max) {
			return out, nil
		}
		cursor = *pg.NextCursor
	}
}

// hostJoin is POST /v1/hosts (the key is never printed by the CLI).
type hostJoin struct {
	Host struct {
		HostID string `json:"hostID"`
		Status string `json:"status"`
	} `json:"host"`
	AuthKey    string   `json:"authKey"`
	ExpiresAt  int64    `json:"expiresAt"`
	Tags       []string `json:"tags"`
	Hostname   string   `json:"hostname"`
	ControlURL string   `json:"controlURL"`
	Tailnet    string   `json:"tailnet"`
}

func (hostJoin) String() string { return "hostJoin{[REDACTED]}" }

// mintHostJoin asks mgmt for this host's single-use join key and writes it
// to the join file twd-hostd waits for (0600 in hostd's 0700 state dir;
// hostd deletes it after the join).
func (s *state) mintHostJoin(ctx context.Context, hostID, tenantID string) error {
	var hj hostJoin
	if err := s.mgmt().DoHeaders(ctx, "POST", "/v1/hosts", map[string]any{"hostID": hostID, "tenantID": tenantID}, &hj,
		map[string]string{"Twd-Actor": actor()}); err != nil {
		if me, ok := mgmtapi.AsError(err); ok && me.Code == "exists" {
			return fail("state_conflict", "mgmt already has a host "+hostID+" bound to another node", me.Message).
				withFix("twd host rm "+hostID+" --drain   (then twd up again)", "or twd up --host-id <another id>")
		}
		return err
	}
	if hj.AuthKey == "" {
		return fail("bootstrap_failed", "mgmt returned no join key for host "+hostID)
	}
	if err := os.MkdirAll(filepath.Dir(s.hostJoinFile()), 0o700); err != nil {
		return err
	}
	tmp := s.hostJoinFile() + ".tmp"
	if err := os.WriteFile(tmp, []byte("TS_AUTHKEY="+hj.AuthKey+"\n"), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.hostJoinFile())
}

// waitHostLive waits until mgmt reports the host live.
func (s *state) waitHostLive(ctx context.Context, hostID string, timeout time.Duration) error {
	dl := time.Now().Add(timeout)
	for {
		var h struct {
			Status string `json:"status"`
		}
		if s.mgmt().Do(ctx, "GET", "/v1/hosts/"+hostID, nil, &h) == nil && h.Status == "live" {
			return nil
		}
		if time.Now().After(dl) {
			return fmt.Errorf("host %s did not become live within %v", hostID, timeout)
		}
		time.Sleep(200 * time.Millisecond)
	}
}
