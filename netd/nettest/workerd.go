package nettest

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

// Workerd is a running stock workerd (test supervisor; hostd owns the real one in M3).
type Workerd struct {
	Cmd         *exec.Cmd
	IngressSock string
	ColdStart   time.Duration
	LogPath     string
}

// StartWorkerd runs `workerd serve config --experimental` with the ingress
// socket at <rundir>/ingress.sock and the netd egress external pointing at
// egressSock. rundir must exist (0700).
func StartWorkerd(bin, config, rundir, egressSock string) (*Workerd, error) {
	ingress := filepath.Join(rundir, "ingress.sock")
	do := filepath.Join(rundir, "do")
	if err := os.MkdirAll(do, 0o700); err != nil {
		return nil, err
	}
	logPath := filepath.Join(rundir, "workerd.log")
	lf, err := os.Create(logPath)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(bin, "serve", config, "--experimental",
		"-s", "ingress=unix:"+ingress, "-e", "netd-egress=unix:"+egressSock,
		"--directory-path", "do-storage="+do)
	cmd.Stdout, cmd.Stderr = lf, lf
	st := time.Now()
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	w := &Workerd{Cmd: cmd, IngressSock: ingress, LogPath: logPath}
	c := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", ingress)
	}}, Timeout: time.Second}
	for {
		if resp, err := c.Get("http://workerd/"); err == nil {
			resp.Body.Close()
			w.ColdStart = time.Since(st)
			return w, nil
		}
		if time.Since(st) > 20*time.Second {
			w.Stop()
			b, _ := os.ReadFile(logPath)
			return nil, fmt.Errorf("workerd did not come up: %s", b)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Stop terminates workerd.
func (w *Workerd) Stop() {
	if w.Cmd.Process != nil {
		w.Cmd.Process.Signal(syscall.SIGTERM)
		done := make(chan struct{})
		go func() { w.Cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			w.Cmd.Process.Kill()
			<-done
		}
	}
}
