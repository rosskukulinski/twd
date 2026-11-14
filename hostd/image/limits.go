package image

import (
	"errors"
	"fmt"
	"io"
	"strings"
)

// Limits caps what converting one image may consume on the host (security
// review 2026-11-14): a pinned image is tenant input, and its layers are
// decompressed and applied into the host's tmp and cache disk. Every cap
// is checked while reading (a decompression bomb stops at the cap, not
// after it has been written out). Zero fields take the defaults.
type Limits struct {
	// MaxBytes caps the uncompressed bytes of all layers together (the
	// decompressed streams, and the sum of the entries' sizes, which is
	// what applying them writes; a sparse entry counts at its full size)
	// and the bytes extracted from an image tarball. Default
	// DefaultMaxImageBytes.
	MaxBytes int64
	// MaxLayers caps the layer count (default 128, Docker's own limit).
	MaxLayers int
	// MaxEntries caps the tar entries of all layers together (default
	// 1 Mi), and of an image tarball.
	MaxEntries int
	// MaxDepth caps an entry's path depth in components (default 256);
	// MaxName its length in bytes (default 4096, PATH_MAX).
	MaxDepth, MaxName int
}

// DefaultMaxImageBytes is twice Cloudflare's largest container disk (20 GB).
const DefaultMaxImageBytes = 40 << 30

func (l Limits) withDefaults() Limits {
	if l.MaxBytes <= 0 {
		l.MaxBytes = DefaultMaxImageBytes
	}
	if l.MaxLayers <= 0 {
		l.MaxLayers = 128
	}
	if l.MaxEntries <= 0 {
		l.MaxEntries = 1 << 20
	}
	if l.MaxDepth <= 0 {
		l.MaxDepth = 256
	}
	if l.MaxName <= 0 {
		l.MaxName = 4096
	}
	return l
}

// ErrLimit: the image exceeds a conversion limit (errors.Is; the error is
// a *LimitError).
var ErrLimit = errors.New("image exceeds a conversion limit")

// LimitError says which limit an image exceeded.
type LimitError struct {
	What  string // "uncompressed bytes", "layers", "entries", "path depth", "path length"
	Limit int64
}

func (e *LimitError) Error() string {
	return fmt.Sprintf("image exceeds the conversion limit on %s (%d)", e.What, e.Limit)
}

func (e *LimitError) Unwrap() error { return ErrLimit }

// budget counts one conversion pass against Limits.
type budget struct {
	lim           Limits
	stream, sizes int64
	entries       int
}

// reader counts r's bytes into the budget's stream total and fails the
// read that passes MaxBytes.
func (b *budget) reader(r io.Reader) io.Reader { return &countReader{r: r, b: b} }

type countReader struct {
	r io.Reader
	b *budget
}

func (c *countReader) Read(p []byte) (int, error) {
	if c.b.stream > c.b.lim.MaxBytes {
		return 0, &LimitError{"uncompressed bytes", c.b.lim.MaxBytes}
	}
	// Never read more than one byte past the cap.
	if rest := c.b.lim.MaxBytes - c.b.stream + 1; int64(len(p)) > rest {
		p = p[:rest]
	}
	n, err := c.r.Read(p)
	c.b.stream += int64(n)
	if c.b.stream > c.b.lim.MaxBytes {
		return n, &LimitError{"uncompressed bytes", c.b.lim.MaxBytes}
	}
	return n, err
}

// entry counts one tar entry (its cleaned name and size).
func (b *budget) entry(name string, size int64) error {
	b.entries++
	if b.entries > b.lim.MaxEntries {
		return &LimitError{"entries", int64(b.lim.MaxEntries)}
	}
	if len(name) > b.lim.MaxName {
		return &LimitError{"path length", int64(b.lim.MaxName)}
	}
	if d := strings.Count(name, "/") + 1; d > b.lim.MaxDepth {
		return &LimitError{"path depth", int64(b.lim.MaxDepth)}
	}
	if size > 0 {
		b.sizes += size
		if b.sizes > b.lim.MaxBytes || b.sizes < 0 {
			return &LimitError{"uncompressed bytes", b.lim.MaxBytes}
		}
	}
	return nil
}
