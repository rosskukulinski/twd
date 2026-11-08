package netd

import (
	"encoding/json"
	"errors"
	"net/http"
)

// Error codes. They match the TailnetErrorCode set in RFC §6.1 where one exists.
const (
	CodePeerNotVisible       = "peer_not_visible"
	CodeForbiddenDestination = "forbidden_destination"
	CodeKilled               = "killed"
	CodeIdentityPending      = "identity_pending"
	CodeBootstrapFailed      = "bootstrap_failed"
	CodeNoIdentity           = "no_identity"
	CodeUnknownObject        = "unknown_object"
	CodeBadRequest           = "bad_request"
	CodeDialFailed           = "dial_failed"
	CodeUpstreamUnavailable  = "upstream_unavailable"
)

// DocBase is the prefix for doc_url in error bodies.
const DocBase = "https://github.com/rosskukulinski/twd/blob/main/docs/errors.md#"

// Error is a typed netd refusal. It is rendered as JSON with the code also in
// the twd-error response header.
type Error struct {
	Code        string `json:"code"`
	Detail      string `json:"detail"`
	Object      string `json:"object,omitempty"`
	Destination string `json:"destination,omitempty"`
	DocURL      string `json:"doc_url"`
	status      int
}

func (e *Error) Error() string {
	s := "netd: " + e.Code + ": " + e.Detail
	if e.Destination != "" {
		s += " (destination " + e.Destination + ")"
	}
	return s
}

// Status is the HTTP status for this error.
func (e *Error) Status() int {
	if e.status != 0 {
		return e.status
	}
	switch e.Code {
	case CodeForbiddenDestination, CodePeerNotVisible, CodeUnknownObject, CodeNoIdentity:
		return http.StatusForbidden
	case CodeBadRequest:
		return http.StatusBadRequest
	case CodeKilled:
		return http.StatusGone
	case CodeIdentityPending, CodeUpstreamUnavailable:
		return http.StatusServiceUnavailable
	default:
		return http.StatusBadGateway
	}
}

func newErr(code, detail string) *Error {
	return &Error{Code: code, Detail: detail, DocURL: DocBase + code}
}

// AsError extracts a *Error from err's chain.
func AsError(err error) (*Error, bool) {
	var e *Error
	ok := errors.As(err, &e)
	return e, ok
}

// WriteError renders e as a typed JSON response.
func WriteError(w http.ResponseWriter, e *Error) {
	if e.DocURL == "" {
		e.DocURL = DocBase + e.Code
	}
	h := w.Header()
	h.Set("content-type", "application/json")
	h.Set(HeaderError, e.Code)
	h.Set("cache-control", "no-store")
	w.WriteHeader(e.Status())
	e.Detail = Redact(e.Detail)
	_ = json.NewEncoder(w).Encode(e)
}
