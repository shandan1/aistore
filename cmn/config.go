// Package cmn provides common low-level types and utilities for all aistore projects
/*
 * Copyright (c) 2018, NVIDIA CORPORATION. All rights reserved.
 */
package cmn

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/NVIDIA/aistore/3rdparty/atomic"
	"github.com/NVIDIA/aistore/3rdparty/glog"
)

// as in: mountpath/<content-type>/<CloudBs|LocalBs>/<bucket-name>/...
const (
	CloudBs = "cloud"
	LocalBs = "local"
)

var (
	// Translates the various query values for URLParamBckProvider for cluster use
	bckProviderMap = map[string]string{
		// Cloud values
		CloudBs:        CloudBs,
		ProviderAmazon: CloudBs,
		ProviderGoogle: CloudBs,

		// Local values
		LocalBs:     LocalBs,
		ProviderAIS: LocalBs,

		// unset
		"": "",
	}
)

// bucket-is-local to provider helper
func BckProviderFromLocal(isLocal bool) string {
	if isLocal {
		return LocalBs
	}
	return CloudBs
}
func BckProviderFromStr(provider string) (val string, err error) {
	var ok bool
	val, ok = bckProviderMap[strings.ToLower(provider)]
	if !ok {
		err = errors.New("invalid bucket provider '" + provider + "'")
	}
	return
}
func IsValidCloudProvider(bckProvider, cloudProvider string) bool {
	return bckProvider == cloudProvider || bckProvider == CloudBs
}

// $CONFDIR/*
const (
	SmapBackupFile      = "smap.json"
	BucketmdBackupFile  = "bucket-metadata" // base name of the config file; not to confuse with config.Localbuckets mpath
	MountpathBackupFile = "mpaths"          // base name to persist fs.Mountpaths
	GlobalRebMarker     = ".global_rebalancing"
	LocalRebMarker      = ".local_rebalancing"
)

const (
	RevProxyCloud  = "cloud"
	RevProxyTarget = "target"

	KeepaliveHeartbeatType = "heartbeat"
	KeepaliveAverageType   = "average"
)

const (
	ThrottleSleepMin = time.Millisecond * 10
	ThrottleSleepAvg = time.Millisecond * 100
	ThrottleSleepMax = time.Second

	// EC
	MinSliceCount = 1  // minimum number of data or parity slices
	MaxSliceCount = 32 // maximum number of data or parity slices
)

const (
	IgnoreReaction = "ignore"
	WarnReaction   = "warn"
	AbortReaction  = "abort"
)

const (
	// L4
	tcpProto = "tcp"

	// L7
	httpProto  = "http"
	httpsProto = "https"
)

type (
	ValidationArgs struct {
		BckIsLocal bool
		TargetCnt  int // for EC
	}

	Validator interface {
		Validate() error
	}
	PropsValidator interface {
		ValidateAsProps(args *ValidationArgs) error
	}
)

var (
	SupportedReactions = []string{IgnoreReaction, WarnReaction, AbortReaction}
	supportedL4Protos  = []string{tcpProto}
	supportedL7Protos  = []string{httpProto, httpsProto}

	_ Validator = &Config{}
	_ Validator = &CksumConf{}
	_ Validator = &LRUConf{}
	_ Validator = &MirrorConf{}
	_ Validator = &ECConf{}
	_ Validator = &VersionConf{}
	_ Validator = &KeepaliveConf{}
	_ Validator = &PeriodConf{}
	_ Validator = &TimeoutConf{}
	_ Validator = &RebalanceConf{}
	_ Validator = &NetConf{}
	_ Validator = &DownloaderConf{}
	_ Validator = &DSortConf{}

	_ PropsValidator = &CksumConf{}
	_ PropsValidator = &LRUConf{}
	_ PropsValidator = &MirrorConf{}
	_ PropsValidator = &ECConf{}

	// Debugging
	pkgDebug = make(map[string]glog.Level)
)

//
// CONFIG PROVIDER
//
var (
	_ ConfigOwner = &globalConfigOwner{}
)

type (
	// ConfigOwner is interface for interacting with config. For updating we
	// introduce three functions: BeginUpdate, CommitUpdate and DiscardUpdate.
	// These funcs should protect config from being updated simultaneously
	// (update should work as transaction).
	//
	// Subscribe method should be used by services which require to be notified
	// about any config changes.
	ConfigOwner interface {
		Get() *Config
		Clone() *Config
		BeginUpdate() *Config
		CommitUpdate(config *Config)
		DiscardUpdate()

		Subscribe(cl ConfigListener)

		SetConfigFile(path string)
		GetConfigFile() string
	}

	// ConfigListener is interface for listeners which require to be notified
	// about config updates.
	ConfigListener interface {
		ConfigUpdate(oldConf, newConf *Config)
	}
	// selected config overrides via command line
	ConfigCLI struct {
		ConfFile  string        // config filename
		LogLevel  string        // takes precedence over config.Log.Level
		StatsTime time.Duration // overrides config.Periodic.StatsTime
		ProxyURL  string        // primary proxy URL to override config.Proxy.PrimaryURL
	}
)

// globalConfigOwner implements ConfigOwner interface. The implementation is
// protecting config only from concurrent updates but does not use CoW or other
// techniques which involves cloning and updating config. This might change when
// we will have use case for that - then Get and Put would need to be changed
// accordingly.
type globalConfigOwner struct {
	mtx       sync.Mutex // mutex for protecting updates of config
	c         atomic.Pointer
	lmtx      sync.Mutex // mutex for protecting listeners
	listeners []ConfigListener
	confFile  string
}

var (
	// GCO stands for global config owner which is responsible for updating
	// and notifying listeners about any changes in the config. Config is loaded
	// at startup and then can be accessed/updated by other services.
	GCO = &globalConfigOwner{}
)

func init() {
	config := &Config{}
	GCO.c.Store(unsafe.Pointer(config))
	loadDebugMap()
}

func (gco *globalConfigOwner) Get() *Config {
	return (*Config)(gco.c.Load())
}

func (gco *globalConfigOwner) Clone() *Config {
	config := &Config{}

	// FIXME: CopyStruct is actually shallow copy but because Config
	// has only values (no pointers or slices, except FSPaths) it is
	// deep copy. This may break in the future, so we need solution
	// to make sure that we do *proper* deep copy with good performance.
	CopyStruct(config, gco.Get())
	return config
}

// When updating we need to make sure that the update is transaction and no
// other update can happen when other transaction is in progress. Therefore,
// we introduce locking mechanism which targets this problem.
//
// NOTE: BeginUpdate should be followed by CommitUpdate.
func (gco *globalConfigOwner) BeginUpdate() *Config {
	gco.mtx.Lock()
	return gco.Clone()
}

// CommitUpdate ends transaction of updating config and notifies listeners
// about changes in config.
//
// NOTE: CommitUpdate should be preceded by BeginUpdate.
func (gco *globalConfigOwner) CommitUpdate(config *Config) {
	oldConf := gco.Get()
	GCO.c.Store(unsafe.Pointer(config))

	// TODO: Notify listeners is protected by GCO lock to make sure
	// that config updates are done in correct order. But it has
	// performance impact and it needs to be revisited.
	gco.notifyListeners(oldConf)

	gco.mtx.Unlock()
}

// CommitUpdate ends transaction but contrary to CommitUpdate it does not update
// the config nor it notifies listeners.
//
// NOTE: CommitUpdate should be preceded by BeginUpdate.
func (gco *globalConfigOwner) DiscardUpdate() {
	gco.mtx.Unlock()
}

func (gco *globalConfigOwner) SetConfigFile(path string) {
	gco.mtx.Lock()
	gco.confFile = path
	gco.mtx.Unlock()
}
func (gco *globalConfigOwner) GetConfigFile() string {
	gco.mtx.Lock()
	defer gco.mtx.Unlock()
	return gco.confFile
}

func (gco *globalConfigOwner) notifyListeners(oldConf *Config) {
	gco.lmtx.Lock()
	newConf := gco.Get()
	for _, listener := range gco.listeners {
		listener.ConfigUpdate(oldConf, newConf)
	}
	gco.lmtx.Unlock()
}

// Subscribe allows listeners to sign up for notifications about config updates.
func (gco *globalConfigOwner) Subscribe(cl ConfigListener) {
	gco.lmtx.Lock()
	gco.listeners = append(gco.listeners, cl)
	gco.lmtx.Unlock()
}

//
// CONFIGURATION
//
type Config struct {
	Confdir          string          `json:"confdir"`
	CloudProvider    string          `json:"cloudprovider"`
	Mirror           MirrorConf      `json:"mirror"`
	Readahead        RahConf         `json:"readahead"`
	Log              LogConf         `json:"log"`
	Periodic         PeriodConf      `json:"periodic"`
	Timeout          TimeoutConf     `json:"timeout"`
	Proxy            ProxyConf       `json:"proxy"`
	LRU              LRUConf         `json:"lru"`
	Disk             DiskConf        `json:"disk"`
	Rebalance        RebalanceConf   `json:"rebalance"`
	Replication      ReplicationConf `json:"replication"`
	Cksum            CksumConf       `json:"cksum"`
	Ver              VersionConf     `json:"version"`
	FSpaths          SimpleKVs       `json:"fspaths"`
	TestFSP          TestfspathConf  `json:"test_fspaths"`
	Net              NetConf         `json:"net"`
	FSHC             FSHCConf        `json:"fshc"`
	Auth             AuthConf        `json:"auth"`
	KeepaliveTracker KeepaliveConf   `json:"keepalivetracker"`
	Downloader       DownloaderConf  `json:"downloader"`
	DSort            DSortConf       `json:"distributed_sort"`
}

type MirrorConf struct {
	Copies      int64 `json:"copies"`       // num local copies
	Burst       int64 `json:"burst_buffer"` // channel buffer size
	UtilThresh  int64 `json:"util_thresh"`  // utilizations are considered equivalent when below this threshold
	OptimizePUT bool  `json:"optimize_put"` // optimization objective
	Enabled     bool  `json:"enabled"`      // will only generate local copies when set to true
}

type RahConf struct {
	ObjectMem int64 `json:"object_mem"`
	TotalMem  int64 `json:"total_mem"`
	ByProxy   bool  `json:"by_proxy"`
	Discard   bool  `json:"discard"`
	Enabled   bool  `json:"enabled"`
}

type LogConf struct {
	Dir      string `json:"dir"`       // log directory
	Level    string `json:"level"`     // log level aka verbosity
	MaxSize  uint64 `json:"max_size"`  // size that triggers log rotation
	MaxTotal uint64 `json:"max_total"` // max total size of all the logs in the log directory
}

type PeriodConf struct {
	StatsTimeStr     string `json:"stats_time"`
	RetrySyncTimeStr string `json:"retry_sync_time"`
	// omitempty
	StatsTime     time.Duration `json:"-"`
	RetrySyncTime time.Duration `json:"-"`
}

// timeoutconfig contains timeouts used for intra-cluster communication
type TimeoutConf struct {
	DefaultStr         string        `json:"default_timeout"`
	Default            time.Duration `json:"-"` // omitempty
	DefaultLongStr     string        `json:"default_long_timeout"`
	DefaultLong        time.Duration `json:"-"` //
	MaxKeepaliveStr    string        `json:"max_keepalive"`
	MaxKeepalive       time.Duration `json:"-"` //
	ProxyPingStr       string        `json:"proxy_ping"`
	ProxyPing          time.Duration `json:"-"` //
	CplaneOperationStr string        `json:"cplane_operation"`
	CplaneOperation    time.Duration `json:"-"` //
	SendFileStr        string        `json:"send_file_time"`
	SendFile           time.Duration `json:"-"` //
	StartupStr         string        `json:"startup_time"`
	Startup            time.Duration `json:"-"` //
	ListStr            string        `json:"list_timeout"`
	List               time.Duration `json:"-"` //
}

type ProxyConf struct {
	NonElectable bool   `json:"non_electable"`
	PrimaryURL   string `json:"primary_url"`
	OriginalURL  string `json:"original_url"`
	DiscoveryURL string `json:"discovery_url"`
}

type LRUConf struct {
	// LowWM: used capacity low-watermark (% of total local storage capacity)
	LowWM int64 `json:"lowwm"`

	// HighWM: used capacity high-watermark (% of total local storage capacity)
	// NOTE:
	// - LRU starts evicting objects when the currently used capacity (used-cap) gets above HighWM
	// - and keeps evicting objects until the used-cap gets below LowWM
	// - while self-throttling itself in accordance with target utilization
	HighWM int64 `json:"highwm"`

	// Out-of-Space: if exceeded, the target starts failing new PUTs and keeps
	// failing them until its local used-cap gets back below HighWM (see above)
	OOS int64 `json:"out_of_space"`

	// DontEvictTimeStr denotes the period of time during which eviction of an object
	// is forbidden [atime, atime + DontEvictTime]
	DontEvictTimeStr string `json:"dont_evict_time"`

	// DontEvictTime is the parsed value of DontEvictTimeStr
	DontEvictTime time.Duration `json:"-"`

	// CapacityUpdTimeStr denotes the frequency at which AIStore updates local capacity utilization
	CapacityUpdTimeStr string `json:"capacity_upd_time"`

	// CapacityUpdTime is the parsed value of CapacityUpdTimeStr
	CapacityUpdTime time.Duration `json:"-"`

	// LocalBuckets: Enables or disables LRU for local buckets
	LocalBuckets bool `json:"local_buckets"`

	// Enabled: LRU will only run when set to true
	Enabled bool `json:"enabled"`
}

type DiskConf struct {
	DiskUtilLowWM   int64         `json:"disk_util_low_wm"`  // Low watermark below which no throttling is required
	DiskUtilHighWM  int64         `json:"disk_util_high_wm"` // High watermark above which throttling is required for longer duration
	IostatTimeLong  time.Duration `json:"-"`
	IostatTimeShort time.Duration `json:"-"`

	IostatTimeLongStr  string `json:"iostat_time_long"`
	IostatTimeShortStr string `json:"iostat_time_short"`
}

type RebalanceConf struct {
	DestRetryTimeStr string        `json:"dest_retry_time"`
	DestRetryTime    time.Duration `json:"-"` //
	Enabled          bool          `json:"enabled"`
}

type ReplicationConf struct {
	OnColdGet     bool `json:"on_cold_get"`     // object replication on cold GET request
	OnPut         bool `json:"on_put"`          // object replication on PUT request
	OnLRUEviction bool `json:"on_lru_eviction"` // object replication on LRU eviction
}

type CksumConf struct {
	// Type of hashing algorithm used to check for object corruption
	// Values: none, xxhash, md5, inherit
	// Value of 'none' disables hash checking
	Type string `json:"type"`

	// ValidateColdGet determines whether or not the checksum of received object
	// is checked after downloading it from the cloud or next tier
	ValidateColdGet bool `json:"validate_cold_get"`

	// ValidateWarmGet: if enabled, the object's version (if in Cloud-based bucket)
	// and checksum are checked. If either value fail to match, the object
	// is removed from local storage
	ValidateWarmGet bool `json:"validate_warm_get"`

	// ValidateClusterMigration determines if the migrated objects across single cluster
	// should have their checksum validated.
	ValidateClusterMigration bool `json:"validate_cluster_migration"`

	// EnableReadRange: Return read range checksum otherwise return entire object checksum
	EnableReadRange bool `json:"enable_read_range"`
}

type VersionConf struct {
	Type            string `json:"type"`              // inherited/owned
	Enabled         bool   `json:"enabled"`           // defined by the Versioning; can be redefined on a bucket level
	ValidateWarmGet bool   `json:"validate_warm_get"` // validate object version upon warm GET
}

type TestfspathConf struct {
	Root     string `json:"root"`
	Count    int    `json:"count"`
	Instance int    `json:"instance"`
}

type NetConf struct {
	IPv4             string   `json:"ipv4"`
	IPv4IntraControl string   `json:"ipv4_intra_control"`
	IPv4IntraData    string   `json:"ipv4_intra_data"`
	UseIntraControl  bool     `json:"-"`
	UseIntraData     bool     `json:"-"`
	L4               L4Conf   `json:"l4"`
	HTTP             HTTPConf `json:"http"`
}

type L4Conf struct {
	Proto               string `json:"proto"` // tcp, udp
	PortStr             string `json:"port"`  // listening port
	Port                int    `json:"-"`
	PortIntraControlStr string `json:"port_intra_control"` // listening port for intra control network
	PortIntraControl    int    `json:"-"`
	PortIntraDataStr    string `json:"port_intra_data"` // listening port for intra data network
	PortIntraData       int    `json:"-"`
}

type HTTPConf struct {
	Proto         string `json:"proto"`              // http or https
	RevProxy      string `json:"rproxy"`             // RevProxy* enum
	RevProxyCache bool   `json:"rproxy_cache"`       // RevProxy caches or work as transparent proxy
	Certificate   string `json:"server_certificate"` // HTTPS: openssl certificate
	Key           string `json:"server_key"`         // HTTPS: openssl key
	MaxNumTargets int    `json:"max_num_targets"`    // estimated max num targets (to count idle conns)
	UseHTTPS      bool   `json:"use_https"`          // use HTTPS instead of HTTP
}

type FSHCConf struct {
	Enabled       bool `json:"enabled"`
	TestFileCount int  `json:"test_files"`  // the number of files to read and write during a test
	ErrorLimit    int  `json:"error_limit"` // max number of errors (exceeding any results in disabling mpath)
}

type AuthConf struct {
	Secret  string `json:"secret"`
	Enabled bool   `json:"enabled"`
	CredDir string `json:"creddir"`
}

// config for one keepalive tracker
// all type of trackers share the same struct, not all fields are used by all trackers
type KeepaliveTrackerConf struct {
	IntervalStr string        `json:"interval"` // keepalives are sent(target)/checked(promary proxy) every interval
	Interval    time.Duration `json:"-"`
	Name        string        `json:"name"`   // "heartbeat", "average"
	Factor      uint8         `json:"factor"` // only average
}

type KeepaliveConf struct {
	Proxy         KeepaliveTrackerConf `json:"proxy"`  // how proxy tracks target keepalives
	Target        KeepaliveTrackerConf `json:"target"` // how target tracks primary proxies keepalives
	RetryFactor   uint8                `json:"retry_factor"`
	TimeoutFactor uint8                `json:"timeout_factor"`
}

type DownloaderConf struct {
	TimeoutStr string        `json:"timeout"`
	Timeout    time.Duration `json:"-"`
}

type DSortConf struct {
	DuplicatedRecords string `json:"duplicated_records"`
	MissingShards     string `json:"missing_shards"`
}

func SetLogLevel(config *Config, loglevel string) (err error) {
	v := flag.Lookup("v").Value
	if v == nil {
		return fmt.Errorf("nil -v Value")
	}
	err = v.Set(loglevel)
	if err == nil {
		config.Log.Level = loglevel
	}
	return
}

//==============================
//
// config functions
//
//==============================
func LoadConfig(clivars *ConfigCLI) (config *Config, changed bool) {
	GCO.SetConfigFile(clivars.ConfFile)

	config = GCO.BeginUpdate()
	defer GCO.CommitUpdate(config)

	err := LocalLoad(clivars.ConfFile, &config)

	// NOTE: glog.Errorf + os.Exit is used instead of glog.Fatalf to not crash
	// with dozens of backtraces on screen - this is user error not some
	// internal error.

	if err != nil {
		ExitLogf("Failed to load config %q, err: %s", clivars.ConfFile, err)
	}
	if err = flag.Lookup("log_dir").Value.Set(config.Log.Dir); err != nil {
		ExitLogf("Failed to flag-set glog dir %q, err: %s", config.Log.Dir, err)
	}
	if err = CreateDir(config.Log.Dir); err != nil {
		ExitLogf("Failed to create log dir %q, err: %s", config.Log.Dir, err)
	}
	if err := config.Validate(); err != nil {
		ExitLogf("%s", err)
	}

	// glog rotate
	glog.MaxSize = config.Log.MaxSize
	if glog.MaxSize > GiB {
		glog.Errorf("Log.MaxSize %d exceeded 1GB, setting the default 1MB", glog.MaxSize)
		glog.MaxSize = MiB
	}

	config.Net.HTTP.Proto = "http" // not validating: read-only, and can take only two values
	if config.Net.HTTP.UseHTTPS {
		config.Net.HTTP.Proto = "https"
	}

	differentIPs := config.Net.IPv4 != config.Net.IPv4IntraControl
	differentPorts := config.Net.L4.Port != config.Net.L4.PortIntraControl
	config.Net.UseIntraControl = false
	if config.Net.IPv4IntraControl != "" && config.Net.L4.PortIntraControl != 0 && (differentIPs || differentPorts) {
		config.Net.UseIntraControl = true
	}

	differentIPs = config.Net.IPv4 != config.Net.IPv4IntraData
	differentPorts = config.Net.L4.Port != config.Net.L4.PortIntraData
	config.Net.UseIntraData = false
	if config.Net.IPv4IntraData != "" && config.Net.L4.PortIntraData != 0 && (differentIPs || differentPorts) {
		config.Net.UseIntraData = true
	}

	// CLI override
	if clivars.StatsTime != 0 {
		config.Periodic.StatsTime = clivars.StatsTime
		changed = true
	}
	if clivars.ProxyURL != "" {
		config.Proxy.PrimaryURL = clivars.ProxyURL
		changed = true
	}
	if clivars.LogLevel != "" {
		if err = SetLogLevel(config, clivars.LogLevel); err != nil {
			ExitLogf("Failed to set log level = %s, err: %s", clivars.LogLevel, err)
		}
		config.Log.Level = clivars.LogLevel
		changed = true
	} else if err = SetLogLevel(config, config.Log.Level); err != nil {
		ExitLogf("Failed to set log level = %s, err: %s", config.Log.Level, err)
	}
	glog.Infof("Logdir: %q Proto: %s Port: %d Verbosity: %s",
		config.Log.Dir, config.Net.L4.Proto, config.Net.L4.Port, config.Log.Level)
	glog.Infof("Config: %q StatsTime: %v", clivars.ConfFile, config.Periodic.StatsTime)
	return
}

func (c *Config) Validate() error {
	validators := []Validator{
		&c.Disk, &c.LRU, &c.Mirror, &c.Cksum,
		&c.Timeout, &c.Periodic, &c.Rebalance, &c.KeepaliveTracker, &c.Net, &c.Ver,
		&c.Downloader,
	}
	for _, validator := range validators {
		if err := validator.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// ipv4ListsOverlap checks if two comma-separated ipv4 address lists
// contain at least one common ipv4 address
func ipv4ListsOverlap(alist, blist string) (overlap bool, addr string) {
	if alist == "" || blist == "" {
		return
	}
	alistAddrs := strings.Split(alist, ",")
	for _, a := range alistAddrs {
		a = strings.TrimSpace(a)
		if a == "" {
			continue
		}
		if strings.Contains(blist, a) {
			return true, a
		}
	}
	return
}

// validKeepaliveType returns true if the keepalive type is supported.
func validKeepaliveType(t string) bool {
	return t == KeepaliveHeartbeatType || t == KeepaliveAverageType
}

func (c *DiskConf) Validate() (err error) {
	lwm, hwm := c.DiskUtilLowWM, c.DiskUtilHighWM
	if lwm <= 0 || hwm <= lwm || hwm > 100 {
		return fmt.Errorf("invalid disk (disk_util_lwm, disk_util_hwm) configuration %+v", c)
	}

	if c.IostatTimeLong, err = time.ParseDuration(c.IostatTimeLongStr); err != nil {
		return fmt.Errorf("bad disk.iostat_time_long format %s, err %v", c.IostatTimeLongStr, err)
	}
	if c.IostatTimeShort, err = time.ParseDuration(c.IostatTimeShortStr); err != nil {
		return fmt.Errorf("bad disk.iostat_time_short format %s, err %v", c.IostatTimeShortStr, err)
	}
	if c.IostatTimeLong <= 0 {
		return fmt.Errorf("disk.iostat_time_long is zero")
	}
	if c.IostatTimeShort <= 0 {
		return fmt.Errorf("disk.iostat_time_short is zero")
	}
	if c.IostatTimeLong < c.IostatTimeShort {
		return fmt.Errorf("disk.iostat_time_long %v shorter than disk.iostat_time_short %v", c.IostatTimeLong, c.IostatTimeShort)
	}

	return nil
}

func (c *LRUConf) Validate() (err error) {
	lwm, hwm, oos := c.LowWM, c.HighWM, c.OOS
	if lwm <= 0 || hwm < lwm || oos < hwm || oos > 100 {
		return fmt.Errorf("invalid lru (lwm, hwm, oos) configuration %+v", c)
	}
	if c.DontEvictTime, err = time.ParseDuration(c.DontEvictTimeStr); err != nil {
		return fmt.Errorf("invalid lru.dont_evict_time format: %v", err)
	}
	if c.CapacityUpdTime, err = time.ParseDuration(c.CapacityUpdTimeStr); err != nil {
		return fmt.Errorf("invalid lru.capacity_upd_time format: %v", err)
	}
	return nil
}

func (c *LRUConf) ValidateAsProps(args *ValidationArgs) (err error) {
	if !c.Enabled {
		return nil
	}
	return c.Validate()
}

func (c *CksumConf) Validate() error {
	if c.Type != ChecksumXXHash && c.Type != ChecksumNone {
		return fmt.Errorf("invalid checksum.type: %s (expected one of [%s, %s])", c.Type, ChecksumXXHash, ChecksumNone)
	}
	return nil
}

func (c *CksumConf) ValidateAsProps(args *ValidationArgs) error {
	if c.Type != PropInherit && c.Type != ChecksumNone && c.Type != ChecksumXXHash {
		return fmt.Errorf("invalid checksum.type: %s (expected one of: [%s, %s, %s])",
			c.Type, ChecksumXXHash, ChecksumNone, PropInherit)
	}
	return nil
}

func (c *VersionConf) Validate() error {
	if c.ValidateWarmGet && !c.Enabled {
		return errors.New("validate-warm-get requires versioning to be enabled")
	}
	return nil
}
func (c *VersionConf) ValidateAsProps() error { return c.Validate() }

func (c *MirrorConf) Validate() error {
	if c.UtilThresh < 0 || c.UtilThresh > 100 {
		return fmt.Errorf("bad mirror.util_thresh: %v (expected value in range [0, 100])", c.UtilThresh)
	}
	if c.Burst < 0 {
		return fmt.Errorf("bad mirror.burst: %v (expected >0)", c.UtilThresh)
	}
	if c.Enabled && c.Copies != 2 {
		return fmt.Errorf("bad mirror.copies: %d (expected 2)", c.Copies)
	}
	return nil
}

func (c *MirrorConf) ValidateAsProps(args *ValidationArgs) error {
	if !c.Enabled {
		return nil
	}
	return c.Validate()
}

func (c *ECConf) Validate() error {
	if c.ObjSizeLimit < 0 {
		return fmt.Errorf("bad ec.obj_size_limit: %d (expected >=0)", c.ObjSizeLimit)
	}
	if c.DataSlices < MinSliceCount || c.DataSlices > MaxSliceCount {
		return fmt.Errorf("bad ec.data_slices: %d (expected value in range [%d, %d])", c.DataSlices, MinSliceCount, MaxSliceCount)
	}
	// TODO: warn about performance if number is OK but large?
	if c.ParitySlices < MinSliceCount || c.ParitySlices > MaxSliceCount {
		return fmt.Errorf("bad ec.parity_slices: %d (expected value in range [%d, %d])", c.ParitySlices, MinSliceCount, MaxSliceCount)
	}
	return nil
}

func (c *ECConf) ValidateAsProps(args *ValidationArgs) error {
	if !c.Enabled {
		return nil
	}
	if !args.BckIsLocal {
		return fmt.Errorf("erasure coding does not support cloud buckets")
	}
	if err := c.Validate(); err != nil {
		return err
	}
	if required := c.RequiredEncodeTargets(); args.TargetCnt < required {
		return fmt.Errorf(
			"erasure coding requires %d targets to use %d data and %d parity slices "+
				"(the cluster has only %d targets)",
			required, c.DataSlices, c.ParitySlices, args.TargetCnt)

	}
	return nil
}

func (c *TimeoutConf) Validate() (err error) {
	if c.Default, err = time.ParseDuration(c.DefaultStr); err != nil {
		return fmt.Errorf("bad timeout.default format %s, err %v", c.DefaultStr, err)
	}
	if c.DefaultLong, err = time.ParseDuration(c.DefaultLongStr); err != nil {
		return fmt.Errorf("bad timeout.default_long format %s, err %v", c.DefaultLongStr, err)
	}
	if c.List, err = time.ParseDuration(c.ListStr); err != nil {
		return fmt.Errorf("bad timeout.list_timeout format %s, err %v", c.ListStr, err)
	}
	if c.MaxKeepalive, err = time.ParseDuration(c.MaxKeepaliveStr); err != nil {
		return fmt.Errorf("bad timeout.max_keepalive format %s, err %v", c.MaxKeepaliveStr, err)
	}
	if c.ProxyPing, err = time.ParseDuration(c.ProxyPingStr); err != nil {
		return fmt.Errorf("bad timeout.proxy_ping format %s, err %v", c.ProxyPingStr, err)
	}
	if c.CplaneOperation, err = time.ParseDuration(c.CplaneOperationStr); err != nil {
		return fmt.Errorf("bad timeout.vote_request format %s, err %v", c.CplaneOperationStr, err)
	}
	if c.SendFile, err = time.ParseDuration(c.SendFileStr); err != nil {
		return fmt.Errorf("bad timeout.send_file_time format %s, err %v", c.SendFileStr, err)
	}
	if c.Startup, err = time.ParseDuration(c.StartupStr); err != nil {
		return fmt.Errorf("bad timeout.startup_time format %s, err %v", c.StartupStr, err)
	}
	return nil
}

func (c *RebalanceConf) Validate() (err error) {
	if c.DestRetryTime, err = time.ParseDuration(c.DestRetryTimeStr); err != nil {
		return fmt.Errorf("bad rebalance.dest_retry_time format %s, err %v", c.DestRetryTimeStr, err)
	}
	return nil
}

func (c *PeriodConf) Validate() (err error) {
	if c.StatsTime, err = time.ParseDuration(c.StatsTimeStr); err != nil {
		return fmt.Errorf("bad periodic.stats_time format %s, err %v", c.StatsTimeStr, err)
	}
	if c.RetrySyncTime, err = time.ParseDuration(c.RetrySyncTimeStr); err != nil {
		return fmt.Errorf("bad periodic.retry_sync_time format %s, err %v", c.RetrySyncTimeStr, err)
	}
	return nil
}

func (c *KeepaliveConf) Validate() (err error) {
	if c.Proxy.Interval, err = time.ParseDuration(c.Proxy.IntervalStr); err != nil {
		return fmt.Errorf("bad keepalivetracker.proxy.interval %s", c.Proxy.IntervalStr)
	}
	if c.Target.Interval, err = time.ParseDuration(c.Target.IntervalStr); err != nil {
		return fmt.Errorf("bad keepalivetracker.target.interval %s", c.Target.IntervalStr)
	}
	if !validKeepaliveType(c.Proxy.Name) {
		return fmt.Errorf("bad keepalivetracker.proxy.name %s", c.Proxy.Name)
	}
	if !validKeepaliveType(c.Target.Name) {
		return fmt.Errorf("bad keepalivetracker.target.name %s", c.Target.Name)
	}
	return nil
}

func (c *NetConf) Validate() (err error) {
	if !StringInSlice(c.L4.Proto, supportedL4Protos) {
		return fmt.Errorf("l4 proto is not recognized %s, expected one of: %s", c.L4.Proto, supportedL4Protos)
	}

	if !StringInSlice(c.HTTP.Proto, supportedL7Protos) {
		return fmt.Errorf("http proto is not recognized %s, expected one of: %s", c.HTTP.Proto, supportedL7Protos)
	}

	// Parse ports
	if c.L4.Port, err = ParsePort(c.L4.PortStr); err != nil {
		return fmt.Errorf("bad public port specified: %v", err)
	}
	if c.L4.PortIntraControl != 0 {
		if c.L4.PortIntraControl, err = ParsePort(c.L4.PortIntraControlStr); err != nil {
			return fmt.Errorf("bad intra control port specified: %v", err)
		}
	}
	if c.L4.PortIntraData != 0 {
		if c.L4.PortIntraData, err = ParsePort(c.L4.PortIntraDataStr); err != nil {
			return fmt.Errorf("bad intra data port specified: %v", err)
		}
	}

	c.IPv4 = strings.Replace(c.IPv4, " ", "", -1)
	c.IPv4IntraControl = strings.Replace(c.IPv4IntraControl, " ", "", -1)
	c.IPv4IntraData = strings.Replace(c.IPv4IntraData, " ", "", -1)

	if overlap, addr := ipv4ListsOverlap(c.IPv4, c.IPv4IntraControl); overlap {
		return fmt.Errorf(
			"public and internal addresses overlap: %s (public: %s; internal: %s)",
			addr, c.IPv4, c.IPv4IntraControl,
		)
	}
	if overlap, addr := ipv4ListsOverlap(c.IPv4, c.IPv4IntraData); overlap {
		return fmt.Errorf(
			"public and replication addresses overlap: %s (public: %s; replication: %s)",
			addr, c.IPv4, c.IPv4IntraData,
		)
	}
	if overlap, addr := ipv4ListsOverlap(c.IPv4IntraControl, c.IPv4IntraData); overlap {
		return fmt.Errorf(
			"internal and replication addresses overlap: %s (internal: %s; replication: %s)",
			addr, c.IPv4IntraControl, c.IPv4IntraData,
		)
	}
	if c.HTTP.RevProxy != "" {
		if c.HTTP.RevProxy != RevProxyCloud && c.HTTP.RevProxy != RevProxyTarget {
			return fmt.Errorf("invalid http rproxy configuration: %s (expecting: ''|%s|%s)",
				c.HTTP.RevProxy, RevProxyCloud, RevProxyTarget)
		}
	}
	return nil
}

func (c *DownloaderConf) Validate() (err error) {
	if c.Timeout, err = time.ParseDuration(c.TimeoutStr); err != nil {
		return fmt.Errorf("bad downloader.timeout %s", c.TimeoutStr)
	}
	return nil
}

func (c *DSortConf) Validate() (err error) {
	if !StringInSlice(c.DuplicatedRecords, SupportedReactions) {
		return fmt.Errorf("bad c.duplicated_records: %s (expecting one of: %s)", c.DuplicatedRecords, SupportedReactions)
	}
	if !StringInSlice(c.MissingShards, SupportedReactions) {
		return fmt.Errorf("bad c.missing_records: %s (expecting one of: %s)", c.MissingShards, SupportedReactions)
	}
	return nil
}

// setGLogVModule sets glog's vmodule flag
// sets 'v' as is, no verificaton is done here
// syntax for v: target=5,proxy=1, p*=3, etc
func SetGLogVModule(v string) error {
	f := flag.Lookup("vmodule")
	if f == nil {
		return nil
	}

	err := f.Value.Set(v)
	if err == nil {
		glog.Info("log level vmodule changed to ", v)
	}

	return err
}

func (conf *Config) update(key, value string) (Validator, error) {
	// updateValue sets `to` value (required to be pointer) and runs number of
	// provided validators which would check if the value which was just set is
	// correct.
	updateValue := func(to interface{}) error {
		// `to` parameter needs to pointer so we can set it
		Assert(reflect.ValueOf(to).Kind() == reflect.Ptr)

		tmpValue := value
		switch to.(type) {
		case *string:
			// Strings must be quoted so that Unmarshal treat it well
			tmpValue = strconv.Quote(tmpValue)
		default:
			break
		}

		// Unmarshal not only tries to parse `tmpValue` but since `to` is pointer,
		// it will set value of it.
		if err := json.Unmarshal([]byte(tmpValue), to); err != nil {
			return fmt.Errorf("failed to parse %q, %s err: %v", key, value, err)
		}

		return nil
	}

	switch key {
	// TOP LEVEL CONFIG
	case "vmodule":
		if err := SetGLogVModule(value); err != nil {
			return nil, fmt.Errorf("failed to set vmodule = %s, err: %v", value, err)
		}
	case "log_level", "log.level":
		if err := SetLogLevel(conf, value); err != nil {
			return nil, fmt.Errorf("failed to set log level = %s, err: %v", value, err)
		}

	// PERIODIC
	case "stats_time", "periodic.stats_time":
		return &conf.Periodic, updateValue(&conf.Periodic.StatsTimeStr)

	// LRU
	case "lru_enabled", "lru.enabled":
		return &conf.LRU, updateValue(&conf.LRU.Enabled)
	case "lowwm", "lru.lowwm":
		return &conf.LRU, updateValue(&conf.LRU.LowWM)
	case "highwm", "lru.highwm":
		return &conf.LRU, updateValue(&conf.LRU.HighWM)
	case "dont_evict_time", "lru.dont_evict_time":
		return &conf.LRU, updateValue(&conf.LRU.DontEvictTimeStr)
	case "capacity_upd_time", "lru.capacity_upd_time":
		return &conf.LRU, updateValue(&conf.LRU.CapacityUpdTimeStr)
	case "lru_local_buckets", "lru.local_buckets":
		return &conf.LRU, updateValue(&conf.LRU.LocalBuckets)

	// DISK
	case "disk_util_low_wm", "disk.disk_util_low_wm":
		return &conf.Disk, updateValue(&conf.Disk.DiskUtilLowWM)
	case "disk_util_high_wm", "disk.disk_util_high_wm":
		return &conf.Disk, updateValue(&conf.Disk.DiskUtilHighWM)
	case "iostat_time_long", "disk.iostat_time_long":
		return &conf.Disk, updateValue(&conf.Disk.IostatTimeLongStr)
	case "iostat_time_short", "disk.iostat_time_short":
		return &conf.Disk, updateValue(&conf.Disk.IostatTimeShortStr)

	// REBALANCE
	case "dest_retry_time", "rebalance.dest_retry_time":
		return &conf.Rebalance, updateValue(&conf.Rebalance.DestRetryTimeStr)
	case "rebalance_enabled", "rebalance.enabled":
		return nil, updateValue(&conf.Rebalance.Enabled)

	// TIMEOUT
	case "send_file_time", "timeout.send_file_time":
		return &conf.Timeout, updateValue(&conf.Timeout.SendFileStr)
	case "default_timeout", "timeout.default_timeout":
		return &conf.Timeout, updateValue(&conf.Timeout.DefaultStr)
	case "default_long_timeout", "timeout.default_long_timeout":
		return &conf.Timeout, updateValue(&conf.Timeout.DefaultLongStr)
	case "proxy_ping", "timeout.proxy_ping":
		return &conf.Timeout, updateValue(&conf.Timeout.ProxyPingStr)
	case "cplane_operation", "timeout.cplane_operation":
		return &conf.Timeout, updateValue(&conf.Timeout.CplaneOperationStr)
	case "max_keepalive", "timeout.max_keepalive":
		return &conf.Timeout, updateValue(&conf.Timeout.MaxKeepaliveStr)

	// CHECKSUM
	case "checksum", "cksum.type":
		return &conf.Cksum, updateValue(&conf.Cksum.Type)
	case "validate_checksum_cold_get", "cksum.validate_cold_get":
		return &conf.Cksum, updateValue(&conf.Cksum.ValidateColdGet)
	case "validate_checksum_warm_get", "cksum.validate_warm_get":
		return &conf.Cksum, updateValue(&conf.Cksum.ValidateWarmGet)
	case "enable_read_range_checksum", "cksum.enable_read_range":
		return &conf.Cksum, updateValue(&conf.Cksum.EnableReadRange)

	// VERSION
	case "versioning_enabled", "versioning.enabled":
		return nil, updateValue(&conf.Ver.Enabled)
	case "validate_version_warm_get", "version.validate_warm_get":
		return nil, updateValue(&conf.Ver.ValidateWarmGet)

	// FSHC
	case "fshc_enabled", "fshc.enabled":
		return nil, updateValue(&conf.FSHC.Enabled)

	// MIRROR
	case "mirror_enabled", "mirror.enabled":
		return &conf.Mirror, updateValue(&conf.Mirror.Enabled)
	case "mirror_burst_buffer", "mirror.burst_buffer":
		return &conf.Mirror, updateValue(&conf.Mirror.Burst)
	case "mirror_util_thresh", "mirror.util_thresh":
		return &conf.Mirror, updateValue(&conf.Mirror.UtilThresh)

	// KEEPALIVE
	case "keepalivetracker.proxy.interval":
		return &conf.KeepaliveTracker, updateValue(&conf.KeepaliveTracker.Proxy.IntervalStr)
	case "keepalivetracker.proxy.factor":
		return &conf.KeepaliveTracker, updateValue(&conf.KeepaliveTracker.Proxy.Factor)
	case "keepalivetracker.target.interval":
		return &conf.KeepaliveTracker, updateValue(&conf.KeepaliveTracker.Target.IntervalStr)
	case "keepalivetracker.target.factor":
		return &conf.KeepaliveTracker, updateValue(&conf.KeepaliveTracker.Target.Factor)

	// DISTRIBUTED SORT
	case "distributed_sort.duplicated_records":
		return &conf.DSort, updateValue(&conf.DSort.DuplicatedRecords)
	case "distributed_sort.missing_shards":
		return &conf.DSort, updateValue(&conf.DSort.MissingShards)

	default:
		return nil, fmt.Errorf("cannot set config key: %q - is readonly or unsupported", key)
	}
	return nil, nil
}

func SetConfigMany(nvmap SimpleKVs) (err error) {
	if len(nvmap) == 0 {
		return errors.New("setConfig: empty nvmap")
	}

	conf := GCO.BeginUpdate()

	var (
		persist bool
	)

	validators := make(map[Validator]struct{})

	for name, value := range nvmap {
		if name == ActPersist {
			if persist, err = strconv.ParseBool(value); err != nil {
				err = fmt.Errorf("invalid value set for %s, err: %v", name, err)
				GCO.DiscardUpdate()
				return
			}
		} else {
			validator, err := conf.update(name, value)
			if err != nil {
				GCO.DiscardUpdate()
				return err
			}
			if validator != nil {
				validators[validator] = struct{}{}
			}

		}

		glog.Infof("%s: %s=%s", ActSetConfig, name, value)
	}

	// validate after everything is set
	for val := range validators {
		err := val.Validate()
		if err != nil {
			GCO.DiscardUpdate()
			return err
		}
	}

	GCO.CommitUpdate(conf)

	if persist {
		conf := GCO.Get()
		if err := LocalSave(GCO.GetConfigFile(), conf); err != nil {
			glog.Errorf("%s: failed to write, err: %v", ActSetConfig, err)
		} else {
			glog.Infof("%s: stored", ActSetConfig)
		}
	}
	return
}

// ========== Cluster Wide Config =========

// TestingEnv returns true if AIStore is running in a development environment
// where a single local filesystem is partitioned between all (locally running)
// targets and is used for both local and Cloud buckets
func TestingEnv() bool {
	return GCO.Get().TestFSP.Count > 0
}

func CheckDebug(pkgName string) (logLvl glog.Level, ok bool) {
	logLvl, ok = pkgDebug[pkgName]
	return
}

// loadDebugMap sets debug verbosity for different packages based on
// environment variables. It is to help enable asserts that were originally
// used for testing/initial development and to set the verbosity of glog
func loadDebugMap() {
	var opts []string
	// Input will be in the format of AISDEBUG=transport=4,memsys=3 (same as GODEBUG)
	if val := os.Getenv("AIS_DEBUG"); val != "" {
		opts = strings.Split(val, ",")
	}

	for _, ele := range opts {
		pair := strings.Split(ele, "=")
		if len(pair) != 2 {
			ExitLogf("Failed to get name=val element: %q", ele)
		}
		key := pair[0]
		logLvl, err := strconv.Atoi(pair[1])
		if err != nil {
			ExitLogf("Failed to convert verbosity level = %s, err: %s", pair[1], err)
		}
		pkgDebug[key] = glog.Level(logLvl)
	}
}
