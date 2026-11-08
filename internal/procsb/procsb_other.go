//go:build !linux

package procsb

import (
	"errors"
	"net"
)

// ABI is 0 off Linux: there is no Landlock.
func ABI() (int, error) { return 0, errors.New("landlock is Linux-only") }

// Supported is false off Linux.
func Supported() bool { return false }

// Sandboxed is false off Linux (Seatbelt confines socket connects itself).
func Sandboxed(int) bool { return false }

// Descends is unused off Linux.
func Descends(pid, root int) bool { return pid == root }

// Guard returns ln unchanged off Linux.
func Guard(ln net.Listener, _ func(int) bool) net.Listener { return ln }
