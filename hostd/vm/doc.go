// Package vm is twd-hostd's microVM tier (Linux + KVM): it boots one
// Firecracker VM per workload instance from the guest image built by
// guest/build-image.sh, and gives it a host-side network stack in the style
// of tailscale/tailvisor, so that tailnet identity (every tsnet.Server and
// key) stays in host netd. See vnet_linux.go and netns_linux.go.
package vm
