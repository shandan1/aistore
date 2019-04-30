// Package mirror provides local mirroring and replica management
/*
 * Copyright (c) 2019, NVIDIA CORPORATION. All rights reserved.
 */
package mirror

import (
	"os"
	"unsafe"

	"github.com/NVIDIA/aistore/3rdparty/glog"
	"github.com/NVIDIA/aistore/cluster"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/fs"
)

type mpather interface {
	mountpathInfo() *fs.MountpathInfo
	stop()
	post(lom *cluster.LOM)
}

func findLeastUtilized(lom *cluster.LOM, mpathers map[string]mpather) (out mpather) {
	var util int64 = 101
loop:
	for _, j := range mpathers {
		mpathInfo := j.mountpathInfo()
		if mpathInfo.Path == lom.ParsedFQN.MpathInfo.Path {
			continue
		}
		if lom.HasCopies() {
			for _, cpyfqn := range lom.CopyFQN() {
				parsedFQN, err := fs.Mountpaths.FQN2Info(cpyfqn) // can be optimized via lom.init
				if err != nil {
					glog.Errorf("%s: failed to parse copyFQN %s, err: %v", lom, cpyfqn, err)
					continue loop
				}
				if mpathInfo.Path == parsedFQN.MpathInfo.Path {
					continue loop
				}
			}
		}
		if u := fs.Mountpaths.Iostats.GetDiskUtil(mpathInfo.Path); u < util {
			out = j
			util = u
		}
	}
	return
}

func copyTo(lom *cluster.LOM, mpathInfo *fs.MountpathInfo, buf []byte) (err error) {
	mp := lom.ParsedFQN.MpathInfo
	lom.ParsedFQN.MpathInfo = mpathInfo // to generate work fname
	tie := uint16(uintptr(unsafe.Pointer(lom)) & 0xffff)
	workFQN := fs.CSM.GenContentParsedFQN(lom.ParsedFQN, fs.WorkfileType, fs.WorkfilePut, tie)
	lom.ParsedFQN.MpathInfo = mp

	_, err = lom.CopyObject(workFQN, buf)
	if err != nil {
		return
	}

	cpyFQN := fs.CSM.FQN(mpathInfo, lom.ParsedFQN.ContentType, lom.BckIsLocal, lom.Bucket, lom.Objname)

	if err = cmn.MvFile(workFQN, cpyFQN); err != nil {
		if errRemove := os.Remove(workFQN); errRemove != nil {
			glog.Errorf("Failed to remove %s, err: %v", workFQN, errRemove)
		}
		return
	}

	// Append copyFQN to FQNs of existing copies
	lom.AddXcopy(cpyFQN)

	if err = lom.Persist(); err == nil {
		copyLOM := lom.Clone(cpyFQN)
		copyLOM.SetCopyFQN([]string{lom.FQN})
		if err = copyLOM.Persist(); err == nil {
			lom.ReCache()
			return
		}
	}

	// on error
	// FIXME: add rollback which restores lom's metadata in case of failure
	if err := os.Remove(cpyFQN); err != nil && !os.IsNotExist(err) {
		lom.T.FSHC(err, lom.FQN)
	}

	lom.ReCache()
	return
}
