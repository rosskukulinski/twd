package tsapi

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
)

// Tracker records the device (node) IDs a test run created so cleanup deletes
// exactly those devices and nothing else. Several engineers run e2e suites on
// the same tailnet concurrently, so cleanup must never sweep by hostname
// prefix or tag.
type Tracker struct {
	API *Client
	mu  sync.Mutex
	ids map[string]string // stable node ID -> label (for reports)
}

// NewTracker returns a Tracker for api.
func NewTracker(api *Client) *Tracker { return &Tracker{API: api, ids: map[string]string{}} }

// Track records a device this run created. Empty IDs are ignored.
func (t *Tracker) Track(nodeID, label string) {
	if nodeID == "" {
		return
	}
	t.mu.Lock()
	t.ids[nodeID] = label
	t.mu.Unlock()
}

// IDs returns the tracked node IDs, sorted.
func (t *Tracker) IDs() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]string, 0, len(t.ids))
	for id := range t.ids {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// CleanupReport says what Cleanup did.
type CleanupReport struct {
	Tracked int      `json:"tracked"`
	Deleted int      `json:"deleted"` // includes already-gone (404) devices
	Left    int      `json:"left"`    // tracked devices still listed after deletion
	Errors  []string `json:"errors,omitempty"`
	// Untracked lists devices whose hostname starts with runPrefix but whose
	// ID was never tracked. They are reported, never deleted (ephemeral nodes
	// expire on their own); a non-empty list means a tracking bug.
	Untracked []string `json:"untracked,omitempty"`
}

// Cleanup deletes every tracked device by ID, then verifies none is left.
// runPrefix (optional, must be unique to this run) is used only to report
// untracked devices.
func (t *Tracker) Cleanup(ctx context.Context, runPrefix string) CleanupReport {
	ids := t.IDs()
	rep := CleanupReport{Tracked: len(ids)}
	for _, id := range ids {
		if err := t.API.DeleteDevice(ctx, id); err != nil {
			rep.Errors = append(rep.Errors, t.label(id)+": "+err.Error())
		} else {
			rep.Deleted++
		}
	}
	devs, err := t.API.ListDevices(ctx)
	if err != nil {
		rep.Errors = append(rep.Errors, "list: "+err.Error())
		rep.Left = -1
		return rep
	}
	tracked := map[string]bool{}
	for _, id := range ids {
		tracked[id] = true
	}
	for _, d := range devs {
		if tracked[d.NodeID] || tracked[d.ID] {
			rep.Left++
		} else if runPrefix != "" && (strings.HasPrefix(d.Hostname, runPrefix) || strings.HasPrefix(d.Name, runPrefix)) {
			rep.Untracked = append(rep.Untracked, d.Hostname)
		}
	}
	return rep
}

// Err returns a non-nil error if cleanup left devices behind or failed.
func (r CleanupReport) Err() error {
	var errs []error
	if r.Left != 0 {
		errs = append(errs, errors.New("tracked devices left on the tailnet"))
	}
	for _, e := range r.Errors {
		errs = append(errs, errors.New(e))
	}
	return errors.Join(errs...)
}

func (t *Tracker) label(id string) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.ids[id] + "(" + id + ")"
}
