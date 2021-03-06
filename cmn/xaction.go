// Package cmn provides common API constants and types, and low-level utilities for all aistore projects
/*
 * Copyright (c) 2018, NVIDIA CORPORATION. All rights reserved.
 */
package cmn

import (
	"fmt"
	"time"

	"github.com/NVIDIA/aistore/3rdparty/atomic"
	"github.com/NVIDIA/aistore/3rdparty/glog"
)

const timeStampFormat = "15:04:05.000000"

const xactIdleTimeout = time.Minute * 3

type (
	Xact interface {
		ID() int64
		Kind() string
		Bucket() string
		StartTime(s ...time.Time) time.Time
		EndTime(e ...time.Time) time.Time
		String() string
		Abort()
		ChanAbort() <-chan struct{}
		Finished() bool
	}
	XactBase struct {
		id         int64
		sutime     atomic.Int64
		eutime     atomic.Int64
		kind       string
		bucket     string
		abrt       chan struct{}
		aborted    atomic.Bool
		bckIsLocal bool
	}
	//
	// xaction that self-terminates after staying idle for a while
	// with an added capability to renew itself and ref-count its pending work
	//
	XactDemand interface {
		Xact
		ChanCheckTimeout() <-chan time.Time
		Renew()
		Timeout() bool
		IncPending()
		DecPending()
	}
	XactDemandBase struct {
		XactBase
		ticker  *time.Ticker
		renew   atomic.Int64
		pending atomic.Int64
	}
	ErrXpired struct { // return it if called (right) after self-termination
		errstr string
	}
)

func (e *ErrXpired) Error() string     { return e.errstr }
func NewErrXpired(s string) *ErrXpired { return &ErrXpired{errstr: s} }

//
// XactBase - implements Xact interface
//

var _ Xact = &XactBase{}

func NewXactBase(id int64, kind string) *XactBase {
	stime := time.Now()
	xact := &XactBase{id: id, kind: kind, abrt: make(chan struct{})}
	xact.sutime.Store(stime.UnixNano())
	return xact
}
func NewXactBaseWithBucket(id int64, kind string, bucket string, bckIsLocal bool) *XactBase {
	xact := NewXactBase(id, kind)
	xact.bucket, xact.bckIsLocal = bucket, bckIsLocal
	return xact
}

func (xact *XactBase) ID() int64                  { return xact.id }
func (xact *XactBase) Kind() string               { return xact.kind }
func (xact *XactBase) Bucket() string             { Assert(xact.bucket != ""); return xact.bucket }
func (xact *XactBase) BckIsLocal() bool           { return xact.bckIsLocal }
func (xact *XactBase) Finished() bool             { return xact.eutime.Load() != 0 }
func (xact *XactBase) ChanAbort() <-chan struct{} { return xact.abrt }
func (xact *XactBase) Aborted() bool              { return xact.aborted.Load() }

func (xact *XactBase) String() string {
	stime := xact.StartTime()
	stimestr := stime.Format(timeStampFormat)
	if !xact.Finished() {
		return fmt.Sprintf("%s:%d started %s", xact.Kind(), xact.ID(), stimestr)
	}
	etime := xact.EndTime()
	d := etime.Sub(stime)
	return fmt.Sprintf("%s:%d started %s ended %s (%v)", xact.Kind(), xact.ID(), stimestr, etime.Format(timeStampFormat), d)
}

func (xact *XactBase) StartTime(s ...time.Time) time.Time {
	if len(s) == 0 {
		u := xact.sutime.Load()
		if u == 0 {
			return time.Time{}
		}
		return time.Unix(0, u)
	}
	stime := s[0]
	xact.sutime.Store(stime.UnixNano())
	return stime
}

func (xact *XactBase) EndTime(e ...time.Time) time.Time {
	if len(e) == 0 {
		u := xact.eutime.Load()
		if u == 0 {
			return time.Time{}
		}
		return time.Unix(0, u)
	}
	etime := e[0]
	xact.eutime.Store(etime.UnixNano())
	glog.Infoln(xact.String())
	return etime
}

func (xact *XactBase) Abort() {
	if !xact.aborted.CAS(false, true) {
		glog.Infof("already aborted: " + xact.String())
		return
	}
	xact.eutime.Store(time.Now().UnixNano())
	close(xact.abrt)
	glog.Infof("ABORT: " + xact.String())
}

//
// XactDemandBase - implements XactDemand interface
//

var _ XactDemand = &XactDemandBase{}

func NewXactDemandBase(id int64, kind string, bucket string, bckIsLocal bool, idleTime ...time.Duration) *XactDemandBase {
	tickTime := xactIdleTimeout
	if len(idleTime) != 0 {
		tickTime = idleTime[0]
	}
	ticker := time.NewTicker(tickTime)
	return &XactDemandBase{
		XactBase: *NewXactBaseWithBucket(id, kind, bucket, bckIsLocal),
		ticker:   ticker,
	}
}

func (r *XactDemandBase) ChanCheckTimeout() <-chan time.Time { return r.ticker.C }
func (r *XactDemandBase) Renew()                             { r.renew.Store(1) } // see Timeout()
func (r *XactDemandBase) IncPending()                        { r.pending.Inc() }
func (r *XactDemandBase) DecPending()                        { r.pending.Dec() }
func (r *XactDemandBase) Pending() int64                     { return r.pending.Load() }

func (r *XactDemandBase) Timeout() bool {
	if r.pending.Load() > 0 {
		return false
	}
	return r.renew.Dec() < 0
}

func (r *XactDemandBase) Stop() { r.ticker.Stop() }

func ValidXact(xact string) (bool, bool) {
	meta, ok := XactKind[xact]
	return meta.IsGlobal, ok
}
