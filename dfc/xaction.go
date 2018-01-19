package dfc

import (
	"fmt"
	"sync"
	"time"

	"github.com/golang/glog"
)

// xaction: Extended (in time) Action -- Transaction

type xactInterface interface {
	getid() int64
	getkind() string
	tostring() string
}

type xactInProgress struct {
	xactinp []xactInterface
	lock    *sync.Mutex
}

type xactBase struct {
	id    int64
	stime time.Time
	etime time.Time
	kind  string
	abrt  chan struct{}
}

type xactRebalance struct {
	xactBase
	curversion   int64
	targetrunner *targetrunner
}

type xactLRU struct {
	xactBase
	// TODO
}

//====================
//
// xactBase
//
//===================
func newxactBase(id int64, kind string) *xactBase {
	return &xactBase{id: id, stime: time.Now(), kind: kind, abrt: make(chan struct{}, 1)}
}

func (xact *xactBase) getid() int64 {
	return xact.id
}

func (xact *xactBase) getkind() string {
	return xact.kind
}

func (xact *xactBase) tostring() string {
	start := xact.stime.Format(time.RFC3339)
	if xact.etime.IsZero() {
		return fmt.Sprintf("xaction %s:%d %v", xact.kind, xact.id, start)
	}
	fin := xact.etime.Format(time.RFC3339)
	return fmt.Sprintf("xaction %s:%d started %v finished %v", xact.kind, xact.id, start, fin)
}

//===================
//
// xactInProgress
//
//===================

func newxactinp() *xactInProgress {
	q := make([]xactInterface, 4)
	qq := &xactInProgress{xactinp: q[0:0]}
	qq.lock = &sync.Mutex{}
	return qq
}

func (q *xactInProgress) uniqueid() int64 {
	id := time.Now().UTC().UnixNano() & 0xffff
	for i := 0; i < 10; i++ {
		if _, x := q.find(id); x == nil {
			return id
		}
		id = (time.Now().UTC().UnixNano() + id) & 0xffff
	}
	assert(false)
	return 0
}

func (q *xactInProgress) add(xact xactInterface) {
	l := len(q.xactinp)
	q.xactinp = append(q.xactinp, nil)
	q.xactinp[l] = xact
}

func (q *xactInProgress) find(by interface{}) (idx int, xact xactInterface) {
	var id int64
	var kind string
	switch by.(type) {
	case int64:
		id = by.(int64)
	case string:
		kind = by.(string)
	default:
		assert(false, fmt.Sprintf("unexpected find() arg: %#v", by))
	}
	for i, xact := range q.xactinp {
		if id != 0 && xact.getid() == id {
			return i, xact
		}
		if kind != "" && xact.getkind() == kind {
			return i, xact
		}
	}
	return -1, nil
}

func (q *xactInProgress) del(by interface{}) {
	q.lock.Lock()
	defer q.lock.Unlock()
	k, xact := q.find(by)
	if xact == nil {
		glog.Errorf("Failed to find xact by %#v", by)
		return
	}
	l := len(q.xactinp)
	if k < l-1 {
		copy(q.xactinp[k:], q.xactinp[k+1:])
	}
	q.xactinp[l-1] = nil
	q.xactinp = q.xactinp[:l-1]
}

func (q *xactInProgress) renewRebalance(curversion int64, t *targetrunner) *xactRebalance {
	q.lock.Lock()
	defer q.lock.Unlock()
	_, xx := q.find(ActionRebalance)
	if xx != nil {
		xreb := xx.(*xactRebalance)
		if xreb.etime.IsZero() {
			assert(!(xreb.curversion > curversion))
			if xreb.curversion == curversion {
				glog.Infof("%s already running, nothing to do", xreb.tostring())
				return nil
			}
			close(xreb.abrt) // abort
			xreb.etime = time.Now()
			glog.Infof("%s must be aborted", xreb.tostring())
		}
	}
	id := q.uniqueid()
	xreb := &xactRebalance{xactBase: *newxactBase(id, ActionRebalance), curversion: curversion}
	xreb.targetrunner = t
	q.add(xreb)
	return xreb
}

func (q *xactInProgress) renewLRU() *xactLRU {
	q.lock.Lock()
	defer q.lock.Unlock()
	_, xx := q.find(ActionLRU)
	if xx != nil {
		xlru := xx.(*xactLRU)
		glog.Infof("%s already running, nothing to do", xlru.tostring())
		return nil
	}
	id := q.uniqueid()
	xlru := &xactLRU{xactBase: *newxactBase(id, ActionLRU)}
	q.add(xlru)
	return xlru
}

//===================
//
// xactRebalance
//
//===================
func (xact *xactRebalance) tostring() string {
	start := xact.stime.Format(time.RFC3339)
	if xact.etime.IsZero() {
		return fmt.Sprintf("xaction %s:%d v%d started %v", xact.kind, xact.id, xact.curversion, start)
	}
	fin := xact.etime.Format(time.RFC3339)
	return fmt.Sprintf("xaction %s:%d v%d started %v finished %v",
		xact.kind, xact.id, xact.curversion, start, fin)
}