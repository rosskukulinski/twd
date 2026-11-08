package netd

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
)

// keyRE matches anything that looks like a Tailscale secret (auth keys, OAuth
// client secrets, API keys): the tskey- prefix and the token that follows.
var keyRE = regexp.MustCompile(`tskey-[A-Za-z0-9_-]+`)

// Redact replaces every tskey-* token in s.
func Redact(s string) string {
	if len(s) < 6 {
		return s
	}
	return keyRE.ReplaceAllString(s, "tskey-[REDACTED]")
}

// RedactError returns err with any secret removed from its message. It
// returns nil for nil.
func RedactError(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if r := Redact(msg); r != msg {
		return redactedError{msg: r, err: err}
	}
	return err
}

type redactedError struct {
	msg string
	err error
}

func (e redactedError) Error() string { return e.msg }

// Unwrap keeps errors.Is/As working on the original chain. The original's
// message is never rendered by this type.
func (e redactedError) Unwrap() error { return e.err }

// RedactingHandler wraps a slog.Handler so that no tskey-* token reaches it,
// whether it appears in the message, an attribute value, a group, or an error.
type RedactingHandler struct{ h slog.Handler }

// NewRedactingHandler returns a handler that redacts secrets before passing
// records to h.
func NewRedactingHandler(h slog.Handler) *RedactingHandler { return &RedactingHandler{h: h} }

func (r *RedactingHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return r.h.Enabled(ctx, l)
}

func (r *RedactingHandler) Handle(ctx context.Context, rec slog.Record) error {
	out := slog.NewRecord(rec.Time, rec.Level, Redact(rec.Message), rec.PC)
	rec.Attrs(func(a slog.Attr) bool {
		out.AddAttrs(redactAttr(a))
		return true
	})
	return r.h.Handle(ctx, out)
}

func (r *RedactingHandler) WithAttrs(as []slog.Attr) slog.Handler {
	red := make([]slog.Attr, len(as))
	for i, a := range as {
		red[i] = redactAttr(a)
	}
	return &RedactingHandler{h: r.h.WithAttrs(red)}
}

func (r *RedactingHandler) WithGroup(name string) slog.Handler {
	return &RedactingHandler{h: r.h.WithGroup(name)}
}

func redactAttr(a slog.Attr) slog.Attr {
	v := a.Value.Resolve()
	switch v.Kind() {
	case slog.KindString:
		return slog.String(a.Key, Redact(v.String()))
	case slog.KindGroup:
		g := v.Group()
		red := make([]any, len(g))
		for i, ga := range g {
			red[i] = redactAttr(ga)
		}
		return slog.Group(a.Key, red...)
	case slog.KindAny:
		switch x := v.Any().(type) {
		case error:
			return slog.String(a.Key, Redact(x.Error()))
		case fmt.Stringer:
			return slog.String(a.Key, Redact(x.String()))
		default:
			return slog.String(a.Key, Redact(fmt.Sprint(x)))
		}
	}
	return slog.Attr{Key: a.Key, Value: v}
}

// tsnetLogf adapts tsnet's printf-style logger to a redacting slog logger at
// debug level.
func tsnetLogf(l *slog.Logger, objectID string) func(string, ...any) {
	return func(format string, args ...any) {
		if !l.Enabled(context.Background(), slog.LevelDebug) {
			return
		}
		l.Debug(Redact(fmt.Sprintf(format, args...)), "object", objectID, "src", "tsnet")
	}
}
