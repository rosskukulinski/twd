// Package vm is twd-hostd's microVM tier: it boots one VM per workload
// instance from the guest image built by guest/build-image.sh, and gives it
// a host-side network stack in the style of tailscale/tailvisor, so that
// tailnet identity (every tsnet.Server and key) stays in host netd. Two
// backends share the stack (vnet.go), the guest agent channel (agent.go)
// and the config drive: Firecracker on Linux + KVM (vm_linux.go, jailed via
// twd-vmhelper; netns_linux.go) and Virtualization.framework on macOS
// (vz_darwin.go: one twd-vz process per VM, see vzapi).
package vm
