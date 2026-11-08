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

// RiskyHostSockets lists what this user could reach that would let a
// workload escaping its sandbox act as someone else: connectable risky
// sockets (the user has write permission; nothing is connected to) and
// membership of container-engine groups. Empty means none found.
func RiskyHostSockets() []string {
	var out []string
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
		if err != nil || st.Mode()&os.ModeSocket == 0 {
			continue
		}
		if unix.Access(r, unix.W_OK) == nil {
			out = append(out, p)
		}
	}
	if u, err := user.Current(); err == nil {
		gids, _ := u.GroupIds()
		for _, g := range gids {
			if gr, err := user.LookupGroupId(g); err == nil && slices.Contains(riskyGroups, gr.Name) {
				out = append(out, "group "+gr.Name)
			}
		}
	}
	return out
}
