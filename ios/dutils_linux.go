// Package ios is a collection of interfaces to the local storage subsystem;
// the package includes OS-dependent implementations for those interfaces.
/*
 * Copyright (c) 2018, NVIDIA CORPORATION. All rights reserved.
 */
package ios

import (
	"os/exec"
	"strings"

	"github.com/NVIDIA/aistore/3rdparty/glog"
	"github.com/NVIDIA/aistore/cmn"
	jsoniter "github.com/json-iterator/go"
)

type LsBlk struct {
	BlockDevices []BlockDevice `json:"blockdevices"`
}

type BlockDevice struct {
	Name         string        `json:"name"`
	BlockDevices []BlockDevice `json:"children"`
}

// Fs2disks is used when a mountpath is added to
// retrieve the disk(s) associated with a filesystem.
// This returns multiple disks only if the filesystem is RAID.
func Fs2disks(fs string) (disks cmn.StringSet) {
	getDiskCommand := exec.Command("lsblk", "-no", "name", "-J")
	outputBytes, err := getDiskCommand.Output()
	if err != nil {
		glog.Errorf("Failed to lsblk, err %v", err)
		return
	}
	if len(outputBytes) == 0 {
		glog.Errorf("Failed to lsblk - no disks?")
		return
	}
	disks = LsblkOutput2disks(outputBytes, fs)
	return
}

func LsblkOutput2disks(lsblkOutputBytes []byte, fs string) (disks cmn.StringSet) {
	disks = make(cmn.StringSet)
	device := strings.TrimPrefix(fs, "/dev/")
	var lsBlkOutput LsBlk
	err := jsoniter.Unmarshal(lsblkOutputBytes, &lsBlkOutput)
	if err != nil {
		glog.Errorf("Unable to unmarshal lsblk output [%s]. Error: [%v]", string(lsblkOutputBytes), err)
		return
	}

	findDevDisks(lsBlkOutput.BlockDevices, device, disks)
	if glog.V(4) {
		glog.Infof("Device: %s, disk list: %v\n", device, disks)
	}

	return disks
}

//
// private
//

func childMatches(devList []BlockDevice, device string) bool {
	for _, dev := range devList {
		if dev.Name == device {
			return true
		}

		if len(dev.BlockDevices) != 0 && childMatches(dev.BlockDevices, device) {
			return true
		}
	}

	return false
}

func findDevDisks(devList []BlockDevice, device string, disks cmn.StringSet) {
	for _, bd := range devList {
		if bd.Name == device {
			disks[bd.Name] = struct{}{}
			continue
		}
		if len(bd.BlockDevices) != 0 {
			if childMatches(bd.BlockDevices, device) {
				disks[bd.Name] = struct{}{}
			}
		}
	}
}
