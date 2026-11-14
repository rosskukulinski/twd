//go:build darwin

package main

/*
#include <libproc.h>
#include <stdlib.h>
#include <string.h>
#include <sys/proc_info.h>

// holdsFile: does pid have path open?
static int holdsFile(pid_t pid, const char *path) {
	int sz = proc_pidinfo(pid, PROC_PIDLISTFDS, 0, NULL, 0);
	if (sz <= 0) return 0;
	struct proc_fdinfo *fds = malloc(sz + 64 * sizeof(struct proc_fdinfo));
	if (!fds) return 0;
	sz = proc_pidinfo(pid, PROC_PIDLISTFDS, 0, fds, sz + 64 * sizeof(struct proc_fdinfo));
	int n = sz / (int)sizeof(struct proc_fdinfo), found = 0;
	for (int i = 0; i < n && !found; i++) {
		if (fds[i].proc_fdtype != PROX_FDTYPE_VNODE) continue;
		struct vnode_fdinfowithpath vi;
		if (proc_pidfdinfo(pid, fds[i].proc_fd, PROC_PIDFDVNODEPATHINFO, &vi, sizeof(vi)) != sizeof(vi)) continue;
		if (strcmp(vi.pvip.vip_path, path) == 0) found = 1;
	}
	free(fds);
	return found;
}

// vmServicePID finds the com.apple.Virtualization.VirtualMachine XPC
// service running our guest: the one that holds our (per-VM) disk open.
static int vmServicePID(const char *disk) {
	int n = proc_listallpids(NULL, 0);
	if (n <= 0) return 0;
	pid_t *pids = calloc(n + 64, sizeof(pid_t));
	if (!pids) return 0;
	n = proc_listallpids(pids, (n + 64) * sizeof(pid_t));
	char path[PROC_PIDPATHINFO_MAXSIZE];
	int found = 0;
	for (int i = 0; i < n && !found; i++) {
		if (pids[i] <= 1) continue;
		if (proc_pidpath(pids[i], path, sizeof(path)) <= 0) continue;
		if (!strstr(path, "com.apple.Virtualization.VirtualMachine")) continue;
		if (holdsFile(pids[i], disk)) found = pids[i];
	}
	free(pids);
	return found;
}
*/
import "C"

import (
	"path/filepath"
	"unsafe"
)

// vmServicePID is the pid of the XPC process that holds this VM's guest
// memory and vCPUs (0 if not found): Virtualization.framework runs every VM
// in its own sandboxed XPC service, not in the calling process. disk is a
// disk image only this VM uses (its config drive).
func vmServicePID(disk string) int {
	if p, err := filepath.EvalSymlinks(disk); err == nil {
		disk = p
	}
	cs := C.CString(disk)
	defer C.free(unsafe.Pointer(cs))
	return int(C.vmServicePID(cs))
}
