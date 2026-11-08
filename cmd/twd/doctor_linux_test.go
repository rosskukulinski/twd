//go:build linux

package main

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestTailscaledHoldersDetects: a connection from this process to a socket
// bound at …/tailscaled.sock by ANOTHER process (the test binary re-run as
// a server) is reported, via the server end's path and our peer inode
// (ss -x). With no such connection, nothing is reported.
func TestTailscaledHoldersDetects(t *testing.T) {
	if _, err := exec.LookPath("ss"); err != nil {
		t.Skip("no ss (iproute2)")
	}
	dir := t.TempDir()
	sock := filepath.Join(dir, "tailscaled.sock")
	if bad, _ := tailscaledHolders([]int{os.Getpid()}); len(bad) != 0 {
		t.Fatalf("before: %v", bad)
	}
	srv := exec.Command(os.Args[0], "-test.run=^TestHelperUnixServer$")
	srv.Env = append(os.Environ(), "TWD_TEST_UNIX_SERVER="+sock)
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { srv.Process.Kill(); srv.Wait() }()
	var c net.Conn
	var err error
	for i := 0; i < 100; i++ {
		if c, err = net.Dial("unix", sock); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	time.Sleep(100 * time.Millisecond) // accepted
	bad, method := tailscaledHolders([]int{os.Getpid()})
	if len(bad) == 0 || !strings.Contains(strings.Join(bad, " "), sock) || !strings.Contains(method, "ss -x") {
		t.Fatalf("connection to %s not detected: %v (%s)", sock, bad, method)
	}
	// The server process itself is not ours to report.
	c.Close()
	time.Sleep(100 * time.Millisecond)
	if bad, _ := tailscaledHolders([]int{os.Getpid()}); len(bad) != 0 {
		t.Fatalf("after close: %v", bad)
	}
}

func TestHelperUnixServer(t *testing.T) {
	p := os.Getenv("TWD_TEST_UNIX_SERVER")
	if p == "" {
		t.Skip("helper")
	}
	l, err := net.Listen("unix", p)
	if err != nil {
		os.Exit(1)
	}
	for {
		c, err := l.Accept()
		if err != nil {
			os.Exit(0)
		}
		go func() { b := make([]byte, 1); c.Read(b); c.Close() }()
	}
}

func TestLandlockABIAndGlibc(t *testing.T) {
	if abi, err := landlockABI(); err == nil && abi < 1 {
		t.Fatalf("landlock ABI %d with no error", abi)
	}
	st, d, _ := doctorGlibc()
	if st == "" || !strings.Contains(d, "glibc") && !strings.Contains(d, "musl") {
		t.Fatalf("glibc check: %s %s", st, d)
	}
	t.Logf("glibc: %s %s", st, d)
}
