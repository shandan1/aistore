// Package ais provides core functionality for the AIStore object storage.
/*
 * Copyright (c) 2018, NVIDIA CORPORATION. All rights reserved.
 */
package ais

import (
	"flag"
	"fmt"
	"os"
	"strconv"

	"github.com/NVIDIA/aistore/3rdparty/glog"
	"github.com/NVIDIA/aistore/cluster"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/fs"
	"github.com/NVIDIA/aistore/health"
	"github.com/NVIDIA/aistore/memsys"
	"github.com/NVIDIA/aistore/stats"
	"github.com/NVIDIA/aistore/transport"
	jsoniter "github.com/json-iterator/go"
)

// runners
const (
	xmem             = "gmem2"
	xstreamc         = "stream-collector"
	xsignal          = "signal"
	xproxystats      = "proxystats"
	xstorstats       = "storstats"
	xproxykeepalive  = "proxykeepalive"
	xtargetkeepalive = "targetkeepalive"
	xlomcache        = "lomcache"
	xmetasyncer      = "metasyncer"
	xfshc            = "fshc"
	xreadahead       = "readahead"
)

type (
	cliVars struct {
		role     string        // proxy | target
		config   cmn.ConfigCLI // selected config overrides
		confjson string        // JSON formatted "{name: value, ...}" string to override selected knob(s)
		ntargets int           // expected number of targets in a starting-up cluster (proxy only)
		persist  bool          // true: make cmn.ConfigCLI settings permanent, false: leave them transient
	}
	// daemon instance: proxy or storage target
	daemon struct {
		rg *rungroup
	}

	rungroup struct {
		runarr []cmn.Runner
		runmap map[string]cmn.Runner // redundant, named
		errCh  chan error
		stopCh chan error
	}
)

// - selective disabling of a disk and/or network IO.
// - dry-run is initialized at startup and cannot be changed.
// - the values can be set via clivars or environment (environment will override clivars).
// - for details see README, section "Performance testing"
type dryRunConfig struct {
	sizeStr string // random content size used when disk IO is disabled (-dryobjsize/AIS_DRYOBJSIZE)
	size    int64  // as above converted to bytes from a string like '8m'
	disk    bool   // dry-run disk (-nodiskio/AIS_NODISKIO)
}

//====================
//
// globals
//
//====================
var (
	gmem2      *memsys.Mem2 // gen-purpose system-wide memory manager and slab/SGL allocator (instance, runner)
	clivars    = &cliVars{}
	ctx        = &daemon{}
	jsonCompat = jsoniter.ConfigCompatibleWithStandardLibrary
	dryRun     = &dryRunConfig{}
)

//====================
//
// rungroup
//
//====================
func (g *rungroup) add(r cmn.Runner, name string) {
	r.Setname(name)
	g.runarr = append(g.runarr, r)
	g.runmap[name] = r
}

func (g *rungroup) run() error {
	if len(g.runarr) == 0 {
		return nil
	}
	g.errCh = make(chan error, len(g.runarr))
	g.stopCh = make(chan error, 1)
	for _, r := range g.runarr {
		go func(r cmn.Runner) {
			err := r.Run()
			glog.Warningf("Runner [%s] exited with err [%v].", r.Getname(), err)
			g.errCh <- err
		}(r)
	}

	// wait here for (any/first) runner termination
	err := <-g.errCh
	for _, r := range g.runarr {
		r.Stop(err)
	}
	for i := 0; i < cap(g.errCh)-1; i++ {
		<-g.errCh
	}
	glog.Flush()
	g.stopCh <- nil
	return err
}

func init() {
	flag.StringVar(&clivars.role, "role", "", "role of this AIS daemon: proxy | target")

	// config itself and its command line overrides
	flag.StringVar(&clivars.config.ConfFile, "config", "", "config filename: local file that stores this daemon's configuration")
	flag.StringVar(&clivars.config.LogLevel, "loglevel", "", "log verbosity level (2 - minimal, 3 - default, 4 - super-verbose)")
	flag.DurationVar(&clivars.config.StatsTime, "statstime", 0, "stats reporting (logging) interval")
	flag.StringVar(&clivars.config.ProxyURL, "proxyurl", "", "primary proxy/gateway URL to override local configuration")
	flag.StringVar(&clivars.confjson, "confjson", "", "JSON formatted \"{name: value, ...}\" string to override selected knob(s)")
	flag.BoolVar(&clivars.persist, "persist", false, "true: apply command-line args to the configuration and save the latter to disk\nfalse: keep it transient (for this run only)")

	flag.IntVar(&clivars.ntargets, "ntargets", 0, "number of storage targets to expect at startup (hint, proxy-only)")

	flag.BoolVar(&dryRun.disk, "nodiskio", false, "dry-run: if true, no disk operations for GET and PUT")
	flag.StringVar(&dryRun.sizeStr, "dryobjsize", "8m", "dry-run: in-memory random content")
}

// dry-run environment overrides dry-run clivars
func dryinit() {
	str := os.Getenv("AIS_NODISKIO")
	if b, err := strconv.ParseBool(str); err == nil {
		dryRun.disk = b
	}
	str = os.Getenv("AIS_DRYOBJSIZE")
	if str != "" {
		if size, err := cmn.S2B(str); size > 0 && err == nil {
			dryRun.size = size
		}
	}
	if dryRun.disk {
		warning := "Dry-run: disk IO will be disabled"
		fmt.Fprintf(os.Stderr, "%s\n", warning)
		glog.Infof("%s - in memory file size: %d (%s) bytes", warning, dryRun.size, dryRun.sizeStr)
	}
}

//==================
//
// daemon init & run
//
//==================
func aisinit(version, build string) {
	var (
		err         error
		config      *cmn.Config
		confChanged bool
	)
	flag.Parse()
	cmn.AssertMsg(clivars.role == cmn.Proxy || clivars.role == cmn.Target, "Invalid flag: role="+clivars.role)

	dryRun.size, err = cmn.S2B(dryRun.sizeStr)
	if dryRun.size < 1 || err != nil {
		fmt.Fprintf(os.Stderr, "Invalid object size: %d [%s]\n", dryRun.size, dryRun.sizeStr)
	}
	if clivars.config.ConfFile == "" {
		str := "Missing configuration file (must be provided via command line)\n"
		str += "Usage: ... -role=<proxy|target> -config=<conf.json> ..."
		cmn.ExitLogf(str)
	}
	config, confChanged = cmn.LoadConfig(&clivars.config)
	if confChanged {
		if err := cmn.LocalSave(cmn.GCO.GetConfigFile(), config); err != nil {
			cmn.ExitLogf("CLI %s: failed to write, err: %s", cmn.ActSetConfig, err)
		}
		glog.Infof("CLI %s: stored", cmn.ActSetConfig)
	}

	glog.Infof("git: %s | build-time: %s\n", version, build)

	// init daemon
	fs.Mountpaths = fs.NewMountedFS()
	// NOTE: proxy and, respectively, target terminations are executed in the same
	//       exact order as the initializations below
	ctx.rg = &rungroup{
		runarr: make([]cmn.Runner, 0, 8),
		runmap: make(map[string]cmn.Runner, 8),
	}
	if clivars.role == cmn.Proxy {
		p := &proxyrunner{}
		p.initSI(cmn.Proxy)
		ctx.rg.add(p, cmn.Proxy)

		ps := &stats.Prunner{}
		ps.Init("aisproxy", p.si.DaemonID)
		ctx.rg.add(ps, xproxystats)

		ctx.rg.add(newProxyKeepaliveRunner(p), xproxykeepalive)
		ctx.rg.add(newmetasyncer(p), xmetasyncer)
	} else {
		t := &targetrunner{}
		t.initSI(cmn.Target)
		ctx.rg.add(t, cmn.Target)

		ts := &stats.Trunner{T: t} // iostat below
		ts.Init("aistarget", t.si.DaemonID)
		ctx.rg.add(ts, xstorstats)

		ctx.rg.add(newTargetKeepaliveRunner(t), xtargetkeepalive)

		t.fsprg.init(t) // subgroup of the ctx.rg rungroup

		// system-wide gen-purpose memory manager and slab/SGL allocator
		mem := &memsys.Mem2{MinPctTotal: 4, MinFree: cmn.GiB * 2} // free mem: try to maintain at least the min of these two
		_ = mem.Init(false)                                       // don't ignore init-time errors
		ctx.rg.add(mem, xmem)                                     // to periodically house-keep
		gmem2 = getmem2()                                         // making it global; getmem2() can still be used

		// Stream Collector - a singleton object with responsibilities that include:
		sc := transport.Init()
		ctx.rg.add(sc, xstreamc)

		// fs.Mountpaths must be inited prior to all runners that utilize them
		// for mountpath definition, see fs/mountfs.go
		if cmn.TestingEnv() {
			glog.Infof("Warning: configuring %d fspaths for testing", config.TestFSP.Count)
			fs.Mountpaths.DisableFsIDCheck()
			t.testCachepathMounts()
		} else {
			fsPaths := make([]string, 0, len(config.FSpaths))
			for path := range config.FSpaths {
				fsPaths = append(fsPaths, path)
			}

			if err := fs.Mountpaths.Init(fsPaths); err != nil {
				glog.Fatal(err)
			}
		}

		fshc := health.NewFSHC(fs.Mountpaths, gmem2, fs.CSM)
		ctx.rg.add(fshc, xfshc)
		t.fsprg.Reg(fshc)

		if config.Readahead.Enabled {
			readaheader := newReadaheader()
			ctx.rg.add(readaheader, xreadahead)
			t.fsprg.Reg(readaheader)
			t.readahead = readaheader
		} else {
			t.readahead = &dummyreadahead{}
		}

		lct := cluster.NewLomCacheRunner(gmem2, t)
		ctx.rg.add(lct, xlomcache)

		_ = ts.UpdateCapacityOOS(nil) // goes after fs.Mountpaths.Init
	}
	ctx.rg.add(&sigrunner{}, xsignal)

	// even more config changes, e.g:
	// -config=/etc/ais.json -role=target -persist=true -confjson="{\"default_timeout\": \"13s\" }"
	if clivars.confjson != "" {
		var nvmap cmn.SimpleKVs
		if err = jsoniter.Unmarshal([]byte(clivars.confjson), &nvmap); err != nil {
			cmn.ExitLogf("Failed to unmarshal JSON [%s], err: %s", clivars.confjson, err)
		}
		if err := cmn.SetConfigMany(nvmap); err != nil {
			cmn.ExitLogf("Failed to set config: %s", err)
		}
	}
}

// Run is the 'main' where everything gets started
func Run(version, build string) {
	aisinit(version, build)
	var ok bool

	err := ctx.rg.run()
	if err == nil {
		goto m
	}
	_, ok = err.(*signalError)
	if ok {
		goto m
	}
	cmn.ExitLogf("Terminated with err: %s", err)
m:
	glog.Infoln("Terminated OK")
	glog.Flush()
}

//==================
//
// global helpers
//
//==================
func getproxystatsrunner() *stats.Prunner {
	r := ctx.rg.runmap[xproxystats]
	rr, ok := r.(*stats.Prunner)
	cmn.Assert(ok)
	return rr
}

func getproxykeepalive() *proxyKeepaliveRunner {
	r := ctx.rg.runmap[xproxykeepalive]
	rr, ok := r.(*proxyKeepaliveRunner)
	cmn.Assert(ok)
	return rr
}

func getmem2() *memsys.Mem2 {
	r := ctx.rg.runmap[xmem]
	rr, ok := r.(*memsys.Mem2)
	cmn.Assert(ok)
	return rr
}

func gettargetkeepalive() *targetKeepaliveRunner {
	r := ctx.rg.runmap[xtargetkeepalive]
	rr, ok := r.(*targetKeepaliveRunner)
	cmn.Assert(ok)
	return rr
}

func getstorstatsrunner() *stats.Trunner {
	r := ctx.rg.runmap[xstorstats]
	rr, ok := r.(*stats.Trunner)
	cmn.Assert(ok)
	return rr
}

func getcloudif() cloudif {
	r := ctx.rg.runmap[cmn.Target]
	rr, ok := r.(*targetrunner)
	cmn.Assert(ok)
	return rr.cloudif
}

func getmetasyncer() *metasyncer {
	r := ctx.rg.runmap[xmetasyncer]
	rr, ok := r.(*metasyncer)
	cmn.Assert(ok)
	return rr
}

func getfshealthchecker() *health.FSHC {
	r := ctx.rg.runmap[xfshc]
	rr, ok := r.(*health.FSHC)
	cmn.Assert(ok)
	return rr
}
