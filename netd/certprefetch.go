package netd

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"tailscale.com/client/local"
)

// PrefetchCert requests name's HTTPS certificate in the background so the
// first TLS handshake doesn't have to wait for issuance. A new node name's
// first certificate takes 30–100 s or more, longer than a Funnel client's
// handshake timeout, so without this every early handshake fails until one
// happens to land after issuance (seen on the WIF issuer's own node). ACME
// orders also fail transiently (CreateOrder 404), so it retries a few
// times; a later handshake would retry on its own anyway, this only warms.
func PrefetchCert(lc *local.Client, name string, log *slog.Logger) {
	name = strings.TrimSuffix(name, ".")
	if lc == nil || name == "" {
		return
	}
	go func() {
		st := time.Now()
		for attempt := 1; ; attempt++ {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			_, _, err := lc.CertPair(ctx, name)
			cancel()
			if err == nil {
				log.Info("HTTPS certificate ready", "name", name, "ms", time.Since(st).Milliseconds(), "attempts", attempt)
				return
			}
			log.Warn("HTTPS certificate prefetch failed", "name", name, "attempt", attempt, "err", RedactError(err))
			if attempt == 4 {
				return
			}
			time.Sleep(time.Duration(attempt) * 20 * time.Second)
		}
	}()
}
