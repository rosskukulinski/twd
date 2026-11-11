package mgmt

import (
	"context"
	"database/sql/driver"
	"errors"
	"io"
	"net"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
)

// isStoreUnavailable reports errors that mean "the store can't serve this
// right now; retry": lost or refused connections, a restarting server, lock
// and idle-in-transaction timeouts (S5, S6, H18). They map to a retryable
// 503 instead of a 500.
func isStoreUnavailable(err error) bool {
	if err == nil {
		return false
	}
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		switch pg.Code {
		case "55P03", // lock_not_available (lock_timeout)
			"25P03",                   // idle_in_transaction_session_timeout
			"57P01", "57P02", "57P03", // admin/crash shutdown, cannot connect now
			"53300",          // too many connections
			"40001", "40P01": // serialization failure, deadlock: retryable
			return true
		}
		return false
	}
	var ne net.Error
	if errors.As(err, &ne) || errors.Is(err, driver.ErrBadConn) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return true
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return false // the caller's own deadline
	}
	s := err.Error()
	return strings.Contains(s, "connection refused") || strings.Contains(s, "conn closed") || strings.Contains(s, "broken pipe") ||
		strings.Contains(s, "failed to connect")
}
