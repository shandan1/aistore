// Package stats provides methods and functionality to register, track, log,
// and StatsD-notify statistics that, for the most part, include "counter" and "latency" kinds.
/*
 * Copyright (c) 2018, NVIDIA CORPORATION. All rights reserved.
 */
package stats

import (
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/NVIDIA/aistore/3rdparty/glog"
	"github.com/NVIDIA/aistore/cluster"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/stats/statsd"
	jsoniter "github.com/json-iterator/go"
)

//==============================
//
// constants
//
//==============================

const logsMaxSizeCheckTime = time.Hour // how often we check if logs have exceeded the limit

const (
	KindCounter    = "counter"
	KindLatency    = "latency"
	KindThroughput = "throughput"
	KindSpecial    = "special"
)

// CoreStats stats
const (
	// KindCounter
	GetCount         = "get.n"
	PutCount         = "put.n"
	PostCount        = "pst.n"
	DeleteCount      = "del.n"
	RenameCount      = "ren.n"
	ListCount        = "lst.n"
	ErrCount         = "err.n"
	ErrGetCount      = "err.get.n"
	ErrDeleteCount   = "err.delete.n"
	ErrPostCount     = "err.post.n"
	ErrPutCount      = "err.put.n"
	ErrHeadCount     = "err.head.n"
	ErrListCount     = "err.list.n"
	ErrRangeCount    = "err.range.n"
	ErrDownloadCount = "err.dl.n"

	// KindLatency
	GetLatency          = "get.µs"
	ListLatency         = "lst.µs"
	KeepAliveMinLatency = "kalive.µs.min"
	KeepAliveMaxLatency = "kalive.µs.max"
	KeepAliveLatency    = "kalive.µs"
	// KindSpecial
	Uptime = "up.µs.time"
)

//
// public types
//

type (
	Tracker interface {
		Add(name string, val int64)
		AddErrorHTTP(method string, val int64)
		AddMany(namedVal64 ...NamedVal64)
		Register(name string, kind string)
	}
	NamedVal64 struct {
		Name string
		Val  int64
	}
	CoreStats struct {
		Tracker   statsTracker
		statsdC   *statsd.Client
		statsTime time.Duration
	}

	DaemonStatus struct {
		Snode       *cluster.Snode         `json:"snode"`
		Stats       *CoreStats             `json:"daemon_stats"`
		Capacity    map[string]*fscapacity `json:"capacity"`
		SysInfo     cmn.SysInfo            `json:"sys_info"`
		SmapVersion int64                  `json:"smap_version"`
	}
)

//
// private types
//

type (
	metric = statsd.Metric // type alias

	// implemented by the stats runners
	statslogger interface {
		log() (runlru bool)
		housekeep(bool)
		doAdd(nv NamedVal64)
	}
	// implements Tracker, inherited by Prunner and Trunner
	statsRunner struct {
		cmn.Named
		stopCh    chan struct{}
		workCh    chan NamedVal64
		starttime time.Time
		ticker    *time.Ticker
		ctracker  copyTracker // to avoid making it at runtime
		logLimit  int64       // check log size
		logIdx    int64       // time interval counting
	}
	// Stats are tracked via a map of stats names (key) to statsValue (values).
	// There are two main types of stats: counter and latency declared
	// using the the kind field. Only latency stats have numSamples used to compute latency.
	statsValue struct {
		sync.RWMutex
		Value      int64 `json:"v"`
		kind       string
		numSamples int64
		cumulative int64
		isCommon   bool // optional, common to the proxy and target
	}
	copyValue struct {
		Value int64 `json:"v"`
	}
	statsTracker map[string]*statsValue
	copyTracker  map[string]*copyValue
)

//
// globals
//
var jsonCompat = jsoniter.ConfigCompatibleWithStandardLibrary

//
// CoreStats
//
func (s *CoreStats) init(size int) {
	s.Tracker = make(statsTracker, size)
	s.Tracker.registerCommonStats()
}

func (s *CoreStats) MarshalJSON() ([]byte, error) { return jsoniter.Marshal(s.Tracker) }
func (s *CoreStats) UnmarshalJSON(b []byte) error { return jsoniter.Unmarshal(b, &s.Tracker) }

//
// NOTE naming convention: ".n" for the count and ".µs" for microseconds
//
func (s *CoreStats) doAdd(name string, val int64) {
	v, ok := s.Tracker[name]
	cmn.AssertMsg(ok, "Invalid stats name '"+name+"'")
	switch v.kind {
	case KindLatency:
		if strings.HasSuffix(name, ".µs") {
			nroot := strings.TrimSuffix(name, ".µs")
			s.statsdC.Send(nroot, metric{statsd.Timer, "latency", float64(time.Duration(val) / time.Millisecond)})
		}
		v.Lock()
		v.numSamples++
		val = int64(time.Duration(val) / time.Microsecond)
		v.cumulative += val
		v.Value += val
		v.Unlock()
	case KindThroughput:
		v.Lock()
		v.cumulative += val
		v.Value += val
		v.Unlock()
	case KindCounter:
		if strings.HasSuffix(name, ".n") {
			nroot := strings.TrimSuffix(name, ".n")
			s.statsdC.Send(nroot, metric{statsd.Counter, "count", val})
			v.Lock()
			v.Value += val
			v.Unlock()
		}
	}
}

func (s *CoreStats) copyZeroReset(ctracker copyTracker) {
	for name, v := range s.Tracker {
		if v.kind == KindLatency {
			v.Lock()
			if v.numSamples > 0 {
				ctracker[name] = &copyValue{Value: v.Value / v.numSamples} // note: int divide
			}
			v.Value = 0
			v.numSamples = 0
			v.Unlock()
		} else if v.kind == KindThroughput {
			cmn.AssertMsg(s.statsTime.Seconds() > 0, "CoreStats: statsTime not set")
			v.Lock()
			throughput := v.Value / int64(s.statsTime.Seconds()) // note: int divide
			ctracker[name] = &copyValue{Value: throughput}
			v.Value = 0
			v.Unlock()
			if strings.HasSuffix(name, ".bps") {
				nroot := strings.TrimSuffix(name, ".bps")
				s.statsdC.Send(nroot,
					metric{Type: statsd.Gauge, Name: "throughput", Value: throughput},
				)
			}
		} else if v.kind == KindCounter {
			v.RLock()
			if v.Value != 0 {
				ctracker[name] = &copyValue{Value: v.Value}
			}
			v.RUnlock()
		} else {
			ctracker[name] = &copyValue{Value: v.Value} // KindSpecial as is and wo/ lock
		}
	}
}

func (s *CoreStats) copyCumulative(ctracker copyTracker) {
	// serves to satisfy REST API what=stats query

	for name, v := range s.Tracker {
		v.RLock()
		if v.kind == KindLatency || v.kind == KindThroughput {
			ctracker[name] = &copyValue{Value: v.cumulative}
		} else if v.kind == KindCounter {
			if v.Value != 0 {
				ctracker[name] = &copyValue{Value: v.Value}
			}
		} else {
			ctracker[name] = &copyValue{Value: v.Value} // KindSpecial as is and wo/ lock
		}
		v.RUnlock()
	}
}

//
// StatsD client using 8125 (default) StatsD port - https://github.com/etsy/statsd
//
func (s *CoreStats) initStatsD(daemonStr, daemonID string) (err error) {
	suffix := strings.Replace(daemonID, ":", "_", -1)
	statsD, err := statsd.New("localhost", 8125, daemonStr+"."+suffix)
	s.statsdC = &statsD
	if err != nil {
		glog.Infof("Failed to connect to StatsD daemon: %v", err)
	}
	return
}

//
// statsValue
//

func (v *statsValue) MarshalJSON() (b []byte, err error) {
	v.RLock()
	b, err = jsoniter.Marshal(v.Value)
	v.RUnlock()
	return
}
func (v *statsValue) UnmarshalJSON(b []byte) error { return jsoniter.Unmarshal(b, &v.Value) }

//
// copyValue
//

func (v *copyValue) MarshalJSON() (b []byte, err error) { return jsoniter.Marshal(v.Value) }
func (v *copyValue) UnmarshalJSON(b []byte) error       { return jsoniter.Unmarshal(b, &v.Value) }

//
// statsTracker
//

func (tracker statsTracker) register(key string, kind string, isCommon ...bool) {
	cmn.AssertMsg(kind == KindCounter || kind == KindLatency || kind == KindThroughput || kind == KindSpecial, "Invalid stats kind '"+kind+"'")
	tracker[key] = &statsValue{kind: kind}
	if len(isCommon) > 0 {
		tracker[key].isCommon = isCommon[0]
	}
}

// stats that are common to proxy and target
func (tracker statsTracker) registerCommonStats() {
	tracker.register(GetCount, KindCounter, true)
	tracker.register(PutCount, KindCounter, true)
	tracker.register(PostCount, KindCounter, true)
	tracker.register(DeleteCount, KindCounter, true)
	tracker.register(RenameCount, KindCounter, true)
	tracker.register(ListCount, KindCounter, true)
	tracker.register(GetLatency, KindLatency, true)
	tracker.register(ListLatency, KindLatency, true)
	tracker.register(KeepAliveMinLatency, KindLatency, true)
	tracker.register(KeepAliveMaxLatency, KindLatency, true)
	tracker.register(KeepAliveLatency, KindLatency, true)
	tracker.register(ErrCount, KindCounter, true)
	tracker.register(ErrGetCount, KindCounter, true)
	tracker.register(ErrDeleteCount, KindCounter, true)
	tracker.register(ErrPostCount, KindCounter, true)
	tracker.register(ErrPutCount, KindCounter, true)
	tracker.register(ErrHeadCount, KindCounter, true)
	tracker.register(ErrListCount, KindCounter, true)
	tracker.register(ErrRangeCount, KindCounter, true)
	tracker.register(ErrDownloadCount, KindCounter, true)
	//
	tracker.register(Uptime, KindSpecial, true)
}

//
// statsunner
//

var (
	_ Tracker            = &statsRunner{}
	_ cmn.ConfigListener = &statsRunner{}
)

func (r *statsRunner) runcommon(logger statslogger) error {
	r.stopCh = make(chan struct{}, 4)
	r.workCh = make(chan NamedVal64, 256)
	r.starttime = time.Now()

	glog.Infof("Starting %s", r.Getname())
	r.ticker = time.NewTicker(cmn.GCO.Get().Periodic.StatsTime)
	for {
		select {
		case nv, ok := <-r.workCh:
			if ok {
				logger.doAdd(nv)
			}
		case <-r.ticker.C:
			runlru := logger.log()
			logger.housekeep(runlru)
		case <-r.stopCh:
			r.ticker.Stop()
			return nil
		}
	}
}

func (r *statsRunner) ConfigUpdate(oldConf, newConf *cmn.Config) {
	if oldConf.Periodic.StatsTime != newConf.Periodic.StatsTime {
		r.ticker.Stop()
		r.ticker = time.NewTicker(newConf.Periodic.StatsTime)
		r.logLimit = cmn.DivCeil(int64(logsMaxSizeCheckTime), int64(newConf.Periodic.StatsTime))
	}
}

func (r *statsRunner) Stop(err error) {
	glog.Infof("Stopping %s, err: %v", r.Getname(), err)
	r.stopCh <- struct{}{}
	close(r.stopCh)
}

// statslogger interface impl
func (r *statsRunner) Register(name string, kind string) { cmn.Assert(false) } // NOTE: currently, proxy's stats == common and hardcoded
func (r *statsRunner) Add(name string, val int64)        { r.workCh <- NamedVal64{name, val} }
func (r *statsRunner) AddMany(nvs ...NamedVal64) {
	for _, nv := range nvs {
		r.workCh <- nv
	}
}
func (r *statsRunner) housekeep(bool) {
	// keep total log size below the configured max
	r.logIdx++
	if r.logIdx >= r.logLimit {
		go r.removeLogs(cmn.GCO.Get())
		r.logIdx = 0
	}
}

func (r *statsRunner) removeLogs(config *cmn.Config) {
	var maxtotal = int64(config.Log.MaxTotal)
	logfinfos, err := ioutil.ReadDir(config.Log.Dir)
	if err != nil {
		glog.Errorf("GC logs: cannot read log dir %s, err: %v", config.Log.Dir, err)
		_ = cmn.CreateDir(config.Log.Dir) // FIXME: (local non-containerized + kill/restart under test)
		return
	}
	// sample name ais.ip-10-0-2-19.root.log.INFO.20180404-031540.2249
	var logtypes = []string{".INFO.", ".WARNING.", ".ERROR."}
	for _, logtype := range logtypes {
		var (
			tot   = int64(0)
			infos = make([]os.FileInfo, 0, len(logfinfos))
		)
		for _, logfi := range logfinfos {
			if logfi.IsDir() {
				continue
			}
			if !strings.Contains(logfi.Name(), ".log.") {
				continue
			}
			if strings.Contains(logfi.Name(), logtype) {
				tot += logfi.Size()
				infos = append(infos, logfi)
			}
		}
		if tot > maxtotal {
			r.removeOlderLogs(tot, maxtotal, config.Log.Dir, logtype, infos)
		}
	}
}

func (r *statsRunner) removeOlderLogs(tot, maxtotal int64, logdir, logtype string, filteredInfos []os.FileInfo) {
	l := len(filteredInfos)
	if l <= 1 {
		glog.Warningf("GC logs: cannot cleanup %s, dir %s, tot %d, max %d", logtype, logdir, tot, maxtotal)
		return
	}
	fiLess := func(i, j int) bool {
		return filteredInfos[i].ModTime().Before(filteredInfos[j].ModTime())
	}
	if glog.V(3) {
		glog.Infof("GC logs: started")
	}
	sort.Slice(filteredInfos, fiLess)
	filteredInfos = filteredInfos[:l-1] // except the last = current
	for _, logfi := range filteredInfos {
		logfqn := filepath.Join(logdir, logfi.Name())
		if err := os.Remove(logfqn); err == nil {
			tot -= logfi.Size()
			glog.Infof("GC logs: removed %s", logfqn)
			if tot < maxtotal {
				break
			}
		} else {
			glog.Errorf("GC logs: failed to remove %s", logfqn)
		}
	}
	if glog.V(3) {
		glog.Infof("GC logs: done")
	}
}

func (r *statsRunner) AddErrorHTTP(method string, val int64) {
	switch method {
	case http.MethodGet:
		r.workCh <- NamedVal64{ErrGetCount, val}
	case http.MethodDelete:
		r.workCh <- NamedVal64{ErrDeleteCount, val}
	case http.MethodPost:
		r.workCh <- NamedVal64{ErrPostCount, val}
	case http.MethodPut:
		r.workCh <- NamedVal64{ErrPutCount, val}
	case http.MethodHead:
		r.workCh <- NamedVal64{ErrHeadCount, val}
	default:
		r.workCh <- NamedVal64{ErrCount, val}
	}
}
