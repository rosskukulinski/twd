package mgmt

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestStoreUnavailableMapping(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{&pgconn.PgError{Code: "55P03"}, true},
		{&pgconn.PgError{Code: "25P03"}, true},
		{&pgconn.PgError{Code: "57P01"}, true},
		{&pgconn.PgError{Code: "23505"}, false}, // unique violation is not "unavailable"
		{io.ErrUnexpectedEOF, true},
		{errors.New("dial tcp 127.0.0.1:5432: connect: connection refused"), true},
		{ErrNotFound, false},
	} {
		if got := isStoreUnavailable(tc.err); got != tc.want {
			t.Errorf("%v: got %v, want %v", tc.err, got, tc.want)
		}
	}
	e := asAPIError(&pgconn.PgError{Code: "55P03", Message: "canceling statement due to lock timeout"})
	if e.Status != 503 || e.Code != "store_unavailable" || e.RetryAfter != 1 {
		t.Fatalf("lock timeout → %+v, want 503 store_unavailable with Retry-After", e)
	}
}

func TestWithSessionLimits(t *testing.T) {
	d := withSessionLimits("postgres://u:p@h:5432/db?sslmode=disable")
	if !strings.Contains(d, "&lock_timeout=3000") || !strings.Contains(d, "idle_in_transaction_session_timeout=3000") {
		t.Fatalf("limits not added: %s", d)
	}
	if d2 := withSessionLimits("postgres://h/db?lock_timeout=100"); strings.Count(d2, "lock_timeout=") != 1 || !strings.Contains(d2, "?lock_timeout=100") {
		t.Fatalf("an explicit lock_timeout must be kept: %s", d2)
	}
}
