//go:build darwin && !arm64

package main

import (
	"errors"

	"github.com/Code-Hex/vz/v3"
)

func rosettaState() string { return "not-supported" }

func addRosetta(*vz.VirtualMachineConfiguration) error {
	return errors.New("rosetta for linux needs Apple silicon")
}
