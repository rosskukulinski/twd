//go:build darwin && arm64

package main

import (
	"fmt"

	"github.com/Code-Hex/vz/v3"
	"github.com/rosskukulinski/twd/hostd/vm/vzapi"
)

func rosettaState() string {
	switch vz.LinuxRosettaDirectoryShareAvailability() {
	case vz.LinuxRosettaAvailabilityInstalled:
		return "installed"
	case vz.LinuxRosettaAvailabilityNotInstalled:
		return "not-installed"
	}
	return "not-supported"
}

// addRosetta shares Rosetta for Linux into the guest (virtiofs tag
// vzapi.RosettaTag). It must already be installed: installing it needs the
// user's consent (softwareupdate --install-rosetta), never done implicitly.
func addRosetta(vmc *vz.VirtualMachineConfiguration) error {
	if s := rosettaState(); s != "installed" {
		return fmt.Errorf("rosetta for linux is %s on this host", s)
	}
	share, err := vz.NewLinuxRosettaDirectoryShare()
	if err != nil {
		return fmt.Errorf("rosetta share: %w", err)
	}
	fs, err := vz.NewVirtioFileSystemDeviceConfiguration(vzapi.RosettaTag)
	if err != nil {
		return err
	}
	fs.SetDirectoryShare(share)
	vmc.SetDirectorySharingDevicesVirtualMachineConfiguration([]vz.DirectorySharingDeviceConfiguration{fs})
	return nil
}
