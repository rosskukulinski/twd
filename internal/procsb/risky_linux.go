//go:build linux

package procsb

import (
	"os"
	"os/user"
	"path/filepath"
	"slices"

	"golang.org/x/sys/unix"
)

// riskySocketPaths are host unix sockets that grant another identity or
// root-equivalent control to whoever can connect: the system tailscaled's
// LocalAPI (the host's own tailnet identity) and container engines.
var riskySocketPaths = []string{
	"/var/run/tailscale/tailscaled.sock", "/run/tailscale/tailscaled.sock",
	"/var/run/docker.sock", "/run/docker.sock",
	"/run/containerd/containerd.sock", "/run/podman/podman.sock",
	"/var/snap/lxd/common/lxd/unix.socket", "/var/lib/incus/unix.socket",
}

// riskyGroups are groups whose members control a container engine (root
// equivalent).
var riskyGroups = []string{"docker", "podman", "lxd", "incus", "incus-admin"}

// ScanHostSockets reports what this user could reach that would let a
// process act as someone else: the system tailscaled's LocalAPI socket (the
// host's own tailnet identity), and container-engine sockets or group
// memberships (root-equivalent). It checks write permission only; nothing
// is connected to.
func ScanHostSockets() HostSockets {
	var hs HostSockets
	paths := append([]string{}, riskySocketPaths...)
	if rd := os.Getenv("XDG_RUNTIME_DIR"); rd != "" {
		paths = append(paths, filepath.Join(rd, "docker.sock"), filepath.Join(rd, "podman", "podman.sock"))
	}
	seen := map[string]bool{}
	for _, p := range paths {
		r, err := filepath.EvalSymlinks(p)
		if err != nil || seen[r] {
			continue
		}
		seen[r] = true
		st, err := os.Stat(r)
		if err != nil || st.Mode()&os.ModeSocket == 0 || unix.Access(r, unix.W_OK) != nil {
			continue
		}
		if filepath.Base(p) == "tailscaled.sock" {
			if hs.Tailscaled == "" {
				hs.Tailscaled = p
			}
		} else {
			hs.Hard = append(hs.Hard, p)
		}
	}
	if u, err := user.Current(); err == nil {
		gids, _ := u.GroupIds()
		for _, g := range gids {
			if gr, err := user.LookupGroupId(g); err == nil && slices.Contains(riskyGroups, gr.Name) {
				hs.Hard = append(hs.Hard, "group "+gr.Name)
			}
		}
	}
	return hs
}
