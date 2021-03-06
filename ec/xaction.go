package ec

import (
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"sync"

	"github.com/NVIDIA/aistore/3rdparty/atomic"
	"github.com/NVIDIA/aistore/3rdparty/glog"
	"github.com/NVIDIA/aistore/cluster"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/transport"
)

const (
	requestBufSizeGlobal = 140
	requestBufSizeFS     = 70
	maxBgJobsPerJogger   = 32
)

type (
	xactECBase struct {
		cmn.XactDemandBase
		cmn.Named
		t cluster.Target

		bmd     cluster.Bowner // bucket manager
		smap    cluster.Sowner // cluster map
		si      *cluster.Snode // target daemonInfo
		stats   stats          // EC statistics
		bckName string         // which bucket xact belongs to

		dOwner *dataOwner // data slice manager

		reqBundle  *transport.StreamBundle // a stream bundle to send lightweight requests
		respBundle *transport.StreamBundle // a stream bungle to transfer data between targets
	}

	xactReqBase struct {
		mpathReqCh chan mpathReq // notify about mountpath changes
		ecCh       chan *Request // to request object encoding

		controlCh chan RequestsControlMsg

		rejectReq atomic.Bool // marker if EC requests should be rejected
	}

	mpathReq struct {
		action string
		mpath  string
	}

	// Manages SGL objects that are waiting for a data from a remote target
	dataOwner struct {
		mtx    sync.Mutex
		slices map[string]*slice
	}
)

const (
	XactGetType = "xactecget"
	XactPutType = "xactecput"
	XactResType = "xactecreq"
)

func newXactReqECBase() xactReqBase {
	return xactReqBase{
		mpathReqCh: make(chan mpathReq, 1),
		ecCh:       make(chan *Request, requestBufSizeGlobal),
		controlCh:  make(chan RequestsControlMsg, 8),
	}
}

func newXactECBase(t cluster.Target, bmd cluster.Bowner, smap cluster.Sowner,
	si *cluster.Snode, bucket string, reqBundle, respBundle *transport.StreamBundle) xactECBase {
	return xactECBase{
		t: t,

		bmd:     bmd,
		smap:    smap,
		si:      si,
		stats:   stats{bckName: bucket},
		bckName: bucket,

		dOwner: &dataOwner{
			mtx:    sync.Mutex{},
			slices: make(map[string]*slice, 10),
		},

		reqBundle:  reqBundle,
		respBundle: respBundle,
	}
}

// ClearRequests disables receiving new EC requests, they will be terminated with error
// Then it starts draining a channel from pending EC requests
// It does not enable receiving new EC requests, it has to be done explicitly, when EC is enabled again
func (r *xactReqBase) ClearRequests() {
	msg := RequestsControlMsg{
		Action: ActClearRequests,
	}

	r.controlCh <- msg
}

func (r *xactReqBase) EnableRequests() {
	msg := RequestsControlMsg{
		Action: ActEnableRequests,
	}

	r.controlCh <- msg
}

func (r *xactReqBase) setEcRequestsDisabled() {
	r.rejectReq.Store(true)
}

func (r *xactReqBase) setEcRequestsEnabled() {
	r.rejectReq.Store(false)
}

func (r *xactReqBase) ecRequestsEnabled() bool {
	return !r.rejectReq.Load()
}

// Create a request header: initializes the `Sender` field with local target's
// daemon ID, and sets `Exists:true` that means "local object exists".
// Later `Exists` can be changed to `false` if local file is unreadable or does
// not exist
func (r *xactECBase) newIntraReq(act intraReqType, meta *Metadata) *IntraReq {
	return &IntraReq{
		Act:    act,
		Sender: r.si.DaemonID,
		Meta:   meta,
		Exists: true,
	}
}

// Sends the replica/meta/slice data: either to copy replicas/slices after
// encoding or to send requested "object" to a client. In the latter case
// if the local object does not exist, it sends an empty body and sets
// exists=false in response header
func (r *xactECBase) dataResponse(act intraReqType, fqn, bucket, objname, id string) error {
	var (
		reader cmn.ReadOpenCloser
		sz     int64
	)
	ireq := r.newIntraReq(act, nil)
	fh, err := cmn.NewFileHandle(fqn)
	// this lom is for local slice/metafile requested by another target
	// to restore the object. So, just create lom and call FromFS
	// because LOM cache does not keep non-objects
	lom, errstr := cluster.LOM{FQN: fqn, T: r.t}.Init()
	if errstr != "" {
		glog.Warningf("Failed to read file stats #1: %s", errstr)
	}
	if errstr := lom.FromFS(); errstr != "" {
		glog.Warningf("Failed to read file stats #2: %s", errstr)
	}
	if err == nil && lom.Size() != 0 {
		sz = lom.Size()
		reader = fh
	} else {
		ireq.Exists = false
	}
	cmn.Assert((sz == 0 && reader == nil) || (sz != 0 && reader != nil))
	objAttrs := transport.ObjectAttrs{
		Size:    sz,
		Version: lom.Version(),
		Atime:   lom.Atime().UnixNano(),
	}

	if lom.Cksum() != nil {
		objAttrs.CksumType, objAttrs.CksumValue = lom.Cksum().Get()
	}

	rHdr := transport.Header{
		Bucket:   bucket,
		Objname:  objname,
		ObjAttrs: objAttrs,
	}
	if rHdr.Opaque, err = ireq.Marshal(); err != nil {
		if fh != nil {
			fh.Close()
		}
		return err
	}

	cb := func(hdr transport.Header, c io.ReadCloser, err error) {
		if err != nil {
			glog.Errorf("Failed to send %s/%s: %v", hdr.Bucket, hdr.Objname, err)
		}
	}
	return r.sendByDaemonID([]string{id}, rHdr, reader, cb, false)
}

// Send a data or request to one or few targets by their DaemonIDs. Most of the time
// only DaemonID is known - that is why the function gets DaemonID and internally
// transforms it into cluster.Snode.
// * daemonIDs - a list of targets
// * hdr - transport header
// * reader - a data to send
// * cb - optional callback to be called when the transfer completes
// * isRequest - defines the type of request:
//		- true - send lightweight request to all targets (usually reader is nil
//			in this case)
//	    - false - send a slice/replica/metadata to targets
func (r *xactECBase) sendByDaemonID(daemonIDs []string, hdr transport.Header,
	reader cmn.ReadOpenCloser, cb transport.SendCallback, isRequest bool) error {
	nodes := make([]*cluster.Snode, 0, len(daemonIDs))
	smap := r.smap.Get()
	for _, id := range daemonIDs {
		si, ok := smap.Tmap[id]
		if !ok {
			glog.Errorf("Target with ID %s not found", id)
			continue
		}
		nodes = append(nodes, si)
	}

	if len(nodes) == 0 {
		return errors.New("destination list is empty")
	}

	var err error
	if isRequest {
		err = r.reqBundle.Send(hdr, reader, cb, nodes)
	} else {
		err = r.respBundle.Send(hdr, reader, cb, nodes)
	}
	return err
}

// send request to a target, wait for its response, read the data into writer.
// * daemonID - target to send a request
// * bucket/objname - what to request
// * uname - unique name for the operation: the name is built from daemonID,
//		bucket and object names. HTTP data receiving handler generates a name
//		when receiving data and if it finds a writer registered with the same
//		name, it puts the data to its writer and notifies when download is done
// * request - request to send
// * writer - an opened writer that will receive the replica/slice/meta
func (r *xactECBase) readRemote(lom *cluster.LOM, daemonID, uname string, request []byte, writer io.Writer) error {
	hdr := transport.Header{
		Bucket:  lom.Bucket,
		Objname: lom.Objname,
		Opaque:  request,
	}
	var reader cmn.ReadOpenCloser
	reader, hdr.ObjAttrs.Size = nil, 0

	sw := &slice{
		writer: writer,
		wg:     cmn.NewTimeoutGroup(),
		lom:    lom,
	}

	sw.wg.Add(1)
	r.regWriter(uname, sw)

	if glog.V(4) {
		glog.Infof("Requesting object %s/%s from %s", lom.Bucket, lom.Objname, daemonID)
	}
	if err := r.sendByDaemonID([]string{daemonID}, hdr, reader, nil, true); err != nil {
		r.unregWriter(uname)
		return err
	}
	c := cmn.GCO.Get()
	if sw.wg.WaitTimeout(c.Timeout.SendFile) {
		r.unregWriter(uname)
		return fmt.Errorf("timed out waiting for %s is read", uname)
	}
	r.unregWriter(uname)
	_, _ = lom.Load(true) // FIXME: handle errors
	if glog.V(4) {
		glog.Infof("Received object %s/%s from %s", lom.Bucket, lom.Objname, daemonID)
	}
	return nil
}

// Registers a new slice that will wait for the data to come from
// a remote target
func (r *xactECBase) regWriter(uname string, writer *slice) bool {
	r.dOwner.mtx.Lock()
	_, ok := r.dOwner.slices[uname]
	if ok {
		glog.Errorf("Writer for %s is already registered", uname)
	} else {
		r.dOwner.slices[uname] = writer
	}
	r.dOwner.mtx.Unlock()

	return !ok
}

// Unregisters a slice that has been waiting for the data to come from
// a remote target
func (r *xactECBase) unregWriter(uname string) (writer *slice, ok bool) {
	r.dOwner.mtx.Lock()
	wr, ok := r.dOwner.slices[uname]
	delete(r.dOwner.slices, uname)
	r.dOwner.mtx.Unlock()

	return wr, ok
}

// Used to copy replicas/slices after the object is encoded after PUT/restored
// after GET, or to respond to meta/slice/replica request.
// * daemonIDs - receivers of the data
// * bucket/objname - object path
// * reader - object/slice/meta data
// * src - extra information about the data to send
// * cb - a caller may set its own callback to execute when the transfer is done.
//		A special case:
//		if a caller does not define its own callback, and it sets the `obj` in
//		`src` it means that the caller wants to automatically free the memory
//		allocated for the `obj` SGL after the object is transferred. The caller
//		may set optional counter in `obj` - the default callback decreases the
//		counter each time the callback is called and when the value drops below 1,
//		`writeRemote` callback frees the SGL
//      The counter is used for sending slices of one big SGL to a few nodes. In
//		this case every slice must be sent to only one target, and transport bundle
//		cannot help to track automatically when SGL should be freed.
func (r *xactECBase) writeRemote(daemonIDs []string, lom *cluster.LOM, src *dataSource, cb transport.SendCallback) error {
	req := r.newIntraReq(src.reqType, src.metadata)
	req.IsSlice = src.isSlice

	putData, err := req.Marshal()
	if err != nil {
		return err
	}
	objAttrs := transport.ObjectAttrs{
		Size:    src.size,
		Version: lom.Version(),
		Atime:   lom.Atime().UnixNano(),
	}
	if lom.Cksum() != nil {
		objAttrs.CksumType, objAttrs.CksumValue = lom.Cksum().Get()
	}
	hdr := transport.Header{
		Objname:  lom.Objname,
		Bucket:   lom.Bucket,
		Opaque:   putData,
		ObjAttrs: objAttrs,
	}
	if cb == nil && src.obj != nil {
		obj := src.obj
		cb = func(hdr transport.Header, reader io.ReadCloser, err error) {
			if obj != nil {
				obj.release()
			}
			if err != nil {
				glog.Errorf("Failed to send %s/%s to %v: %v", lom.Bucket, lom.Objname, daemonIDs, err)
			}
		}
	}
	return r.sendByDaemonID(daemonIDs, hdr, src.reader, cb, false)
}

// save data from a target response to SGL. When exists is false it
// just drains the response body and returns - because it does not contain
// any data. On completion the function must call writer.wg.Done to notify
// the caller that the data read is completed.
// * writer - where to save the slice/meta/replica data
// * exists - if the remote target had the requested object
// * reader - response body
func (r *xactECBase) writerReceive(writer *slice, exists bool, objAttrs transport.ObjectAttrs, reader io.Reader) (err error) {
	buf, slab := mem2.AllocFromSlab2(cmn.MiB)

	if !exists {
		writer.wg.Done()
		// drain the body, to avoid panic:
		// http: panic serving: assertion failed: "expecting an error or EOF as the reason for failing to read
		_, _ = io.CopyBuffer(ioutil.Discard, reader, buf)
		slab.Free(buf)
		return ErrorNotFound
	}

	writer.n, err = io.CopyBuffer(writer.writer, reader, buf)
	if file, ok := writer.writer.(*os.File); ok {
		file.Close()
	}

	writer.cksum = cmn.NewCksum(objAttrs.CksumType, objAttrs.CksumValue)
	if writer.version != "" && objAttrs.Version != "" {
		writer.version = objAttrs.Version
	}

	writer.wg.Done()
	slab.Free(buf)
	return err
}

func (r *xactECBase) Stats() *ECStats {
	return r.stats.stats()
}

//
// fsprunner methods
//
func (r *xactReqBase) ReqAddMountpath(mpath string) {
	r.mpathReqCh <- mpathReq{action: cmn.ActMountpathAdd, mpath: mpath}
}

func (r *xactReqBase) ReqRemoveMountpath(mpath string) {
	r.mpathReqCh <- mpathReq{action: cmn.ActMountpathRemove, mpath: mpath}
}

func (r *xactECBase) ReqEnableMountpath(mpath string)  { /* do nothing */ }
func (r *xactECBase) ReqDisableMountpath(mpath string) { /* do nothing */ }

type (
	BckXacts struct {
		get *XactGet
		put *XactPut
		req *XactRespond
	}
)

func (xacts *BckXacts) Get() *XactGet {
	return xacts.get
}

func (xacts *BckXacts) Put() *XactPut {
	return xacts.put
}

func (xacts *BckXacts) Req() *XactRespond {
	return xacts.req
}

func (xacts *BckXacts) SetGet(xact *XactGet) {
	xacts.get = xact
}

func (xacts *BckXacts) SetPut(xact *XactPut) {
	xacts.put = xact
}

func (xacts *BckXacts) SetReq(xact *XactRespond) {
	xacts.req = xact
}

func (xacts *BckXacts) StopGet() {
	if xacts.get != nil && !xacts.get.Finished() {
		xacts.get.stop()
	}
}

func (xacts *BckXacts) StopPut() {
	if xacts.put != nil && !xacts.put.Finished() {
		xacts.put.stop()
	}
}
