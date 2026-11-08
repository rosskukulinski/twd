//go:build !linux

// twd-guestd is the init of a twd microVM guest; it only builds for Linux.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "twd-guestd: Linux guests only")
	os.Exit(2)
}
