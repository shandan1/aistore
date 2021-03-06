// Package ais_test contains AIS integration tests.
/*
 * Copyright (c) 2018, NVIDIA CORPORATION. All rights reserved.
 */
package ais_test

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NVIDIA/aistore/api"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/dsort"
	"github.com/NVIDIA/aistore/dsort/extract"
	"github.com/NVIDIA/aistore/tutils"
	"github.com/NVIDIA/aistore/tutils/tassert"
	sigar "github.com/cloudfoundry/gosigar"
)

const (
	dsortDescAllPrefix = "dsort-test-integration"
	dsortDescAllRegex  = "^" + dsortDescAllPrefix
)

var (
	dsortDescCurPrefix = fmt.Sprintf("%s-%d-", dsortDescAllPrefix, os.Getpid())
)

type dsortFramework struct {
	m *metadata

	outputBucket string
	inputPrefix  string
	outputPrefix string

	inputTempl            string
	outputTempl           string
	tarballCnt            int
	tarballCntToSkip      int
	fileInTarballCnt      int
	fileInTarballSize     int
	tarballSize           int
	outputShardCnt        int
	recordDuplicationsCnt int

	extension       string
	algorithm       *dsort.SortAlgorithm
	outputShardSize int64
	maxMemUsage     string
}

func generateDSortDesc() string {
	return dsortDescCurPrefix + time.Now().Format(time.RFC3339Nano)
}

func (df *dsortFramework) init() {
	if df.inputTempl == "" {
		df.inputTempl = fmt.Sprintf("input-{0..%d}", df.tarballCnt-1)
	}
	if df.outputTempl == "" {
		df.outputTempl = "output-{00000..10000}"
	}

	// Assumption is that all prefixes end with dash: "-"
	df.inputPrefix = df.inputTempl[:strings.Index(df.inputTempl, "-")+1]
	df.outputPrefix = df.outputTempl[:strings.Index(df.outputTempl, "-")+1]

	if df.fileInTarballSize == 0 {
		df.fileInTarballSize = cmn.KiB
	}

	df.tarballSize = df.fileInTarballCnt * df.fileInTarballSize
	df.outputShardSize = int64(10 * df.fileInTarballCnt * df.fileInTarballSize)
	df.outputShardCnt = (df.tarballCnt * df.tarballSize) / int(df.outputShardSize)

	if df.algorithm == nil {
		df.algorithm = &dsort.SortAlgorithm{}
	}
}

func (df *dsortFramework) gen() dsort.RequestSpec {
	return dsort.RequestSpec{
		ProcDescription:  generateDSortDesc(),
		Bucket:           df.m.bucket,
		OutputBucket:     df.outputBucket,
		Extension:        df.extension,
		IntputFormat:     df.inputTempl,
		OutputFormat:     df.outputTempl,
		OutputShardSize:  df.outputShardSize,
		Algorithm:        *df.algorithm,
		BckProvider:      cmn.LocalBs,
		ExtractConcLimit: 10,
		CreateConcLimit:  10,
		MaxMemUsage:      df.maxMemUsage,
	}
}

func (df *dsortFramework) createInputShards() {
	const tmpDir = "/tmp"
	var (
		err error
	)
	tutils.Logf("creating %d tarballs...\n", df.tarballCnt)
	wg := &sync.WaitGroup{}
	errCh := make(chan error, df.tarballCnt)
	for i := df.tarballCntToSkip; i < df.tarballCnt; i++ {
		wg.Add(1)
		go func(i int) {
			duplication := i < df.recordDuplicationsCnt

			path := fmt.Sprintf("%s/%s%d", tmpDir, df.inputPrefix, i)
			if df.algorithm.Kind == dsort.SortKindContent {
				err = tutils.CreateTarWithCustomFiles(path, df.fileInTarballCnt, df.fileInTarballSize, df.algorithm.FormatType, df.algorithm.Extension)
			} else if df.extension == ".tar" {
				err = tutils.CreateTarWithRandomFiles(path, false, df.fileInTarballCnt, df.fileInTarballSize, duplication)
			} else if df.extension == ".tar.gz" {
				err = tutils.CreateTarWithRandomFiles(path, true, df.fileInTarballCnt, df.fileInTarballSize, duplication)
			} else if df.extension == ".zip" {
				err = tutils.CreateZipWithRandomFiles(path, df.fileInTarballCnt, df.fileInTarballSize)
			} else {
				df.m.t.Fail()
			}
			tassert.CheckFatal(df.m.t, err)

			fqn := path + df.extension
			defer os.Remove(fqn)

			reader, err := tutils.NewFileReaderFromFile(fqn, false)
			tassert.CheckFatal(df.m.t, err)

			tutils.PutAsync(wg, df.m.proxyURL, df.m.bucket, filepath.Base(fqn), reader, errCh)
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		tassert.CheckFatal(df.m.t, err)
	}
	tutils.Logln("done creating tarballs")
}

func (df *dsortFramework) checkOutputShards(zeros int) {
	tutils.Logln("checking if files are sorted...")

	lastName := ""
	var lastValue interface{}

	gzipped := false
	if df.extension != ".tar" {
		gzipped = true
	}

	inversions := 0
	baseParams := tutils.BaseAPIParams(df.m.proxyURL)
	for i := 0; i < df.outputShardCnt; i++ {
		shardName := fmt.Sprintf("%s%0*d%s", df.outputPrefix, zeros, i, df.extension)
		var buffer bytes.Buffer
		getOptions := api.GetObjectInput{
			Writer: &buffer,
		}

		bucket := df.m.bucket
		if df.outputBucket != "" {
			bucket = df.outputBucket
		}

		_, err := api.GetObject(baseParams, bucket, shardName, getOptions)
		if err != nil && df.extension == ".zip" && i > df.outputShardCnt/2 {
			// We estimated too much output shards to be produced - zip compression
			// was so good that we could fit more files inside the shard.
			//
			// Sanity check to make sure that error did not occur before half
			// of the shards estimated (zip compression should be THAT good).
			break
		}
		tassert.CheckFatal(df.m.t, err)

		if df.algorithm.Kind == dsort.SortKindContent {
			files, err := tutils.GetFilesFromTarBuffer(buffer, df.algorithm.Extension)
			tassert.CheckFatal(df.m.t, err)
			for _, file := range files {
				if file.Ext == df.algorithm.Extension {
					if strings.TrimSuffix(file.Name, filepath.Ext(file.Name)) != strings.TrimSuffix(lastName, filepath.Ext(lastName)) {
						// custom files should be AFTER the regular files
						df.m.t.Fatalf("names are not in correct order (shard: %s, lastName: %s, curName: %s)", shardName, lastName, file.Name)
					}

					switch df.algorithm.FormatType {
					case extract.FormatTypeInt:
						intValue, err := strconv.ParseInt(string(file.Content), 10, 64)
						tassert.CheckFatal(df.m.t, err)
						if lastValue != nil && intValue < lastValue.(int64) {
							df.m.t.Fatalf("int values are not in correct order (shard: %s, lastIntValue: %d, curIntValue: %d)", shardName, lastValue.(int64), intValue)
						}
						lastValue = intValue
					case extract.FormatTypeFloat:
						floatValue, err := strconv.ParseFloat(string(file.Content), 64)
						tassert.CheckFatal(df.m.t, err)
						if lastValue != nil && floatValue < lastValue.(float64) {
							df.m.t.Fatalf("string values are not in correct order (shard: %s, lastStringValue: %f, curStringValue: %f)", shardName, lastValue.(float64), floatValue)
						}
						lastValue = floatValue
					case extract.FormatTypeString:
						stringValue := string(file.Content)
						if lastValue != nil && stringValue < lastValue.(string) {
							df.m.t.Fatalf("string values are not in correct order (shard: %s, lastStringValue: %s, curStringValue: %s)", shardName, lastValue.(string), stringValue)
						}
						lastValue = stringValue
					default:
						df.m.t.Fail()
					}
				} else {
					lastName = file.Name
				}
			}
		} else {
			var (
				files []os.FileInfo
			)

			if df.extension == ".tar" || df.extension == ".tar.gz" {
				files, err = tutils.GetFileInfosFromTarBuffer(buffer, gzipped)
			} else if df.extension == ".zip" {
				files, err = tutils.GetFileInfosFromZipBuffer(buffer)
			}

			tassert.CheckFatal(df.m.t, err)
			if len(files) == 0 {
				df.m.t.Fatal("number of files inside shard is 0")
			}

			for _, file := range files {
				if df.algorithm.Kind == "" {
					if lastName > file.Name() {
						df.m.t.Fatalf("names are not in correct order (shard: %s, lastName: %s, curName: %s)", shardName, lastName, file.Name())
					}
				} else if df.algorithm.Kind == dsort.SortKindShuffle {
					if lastName > file.Name() {
						inversions++
					}
				}
				if file.Size() != int64(df.fileInTarballSize) {
					df.m.t.Fatalf("file sizes has changed (expected: %d, got: %d)", df.fileInTarballSize, file.Size())
				}
				lastName = file.Name()
			}
		}
	}

	if df.algorithm.Kind == dsort.SortKindShuffle {
		if inversions == 0 {
			df.m.t.Fatal("shuffle sorting did not create any inversions")
		}
	}
}

func (df *dsortFramework) checkReactionResult(reaction string, allMetrics map[string]*dsort.Metrics, expectedProblemsCnt int) {
	switch reaction {
	case cmn.IgnoreReaction:
		for target, metrics := range allMetrics {
			if len(metrics.Warnings) != 0 {
				df.m.t.Errorf("target %q has dSort warnings: %s", target, metrics.Warnings)
			}
			if len(metrics.Errors) != 0 {
				df.m.t.Errorf("target %q has dSort errors: %s", target, metrics.Errors)
			}
		}
	case cmn.WarnReaction:
		totalWarnings := 0
		for target, metrics := range allMetrics {
			totalWarnings += len(metrics.Warnings)

			if len(metrics.Errors) != 0 {
				df.m.t.Errorf("target %q has dSort errors: %s", target, metrics.Errors)
			}
		}

		if totalWarnings != expectedProblemsCnt {
			df.m.t.Errorf("number of total warnings %d is different than number of deleted shards: %d", totalWarnings, expectedProblemsCnt)
		}
	case cmn.AbortReaction:
		totalErrors := 0
		for target, metrics := range allMetrics {
			if !metrics.Aborted {
				df.m.t.Errorf("dsort was not aborted by target: %s", target)
			}
			totalErrors += len(metrics.Errors)
		}

		if totalErrors == 0 {
			df.m.t.Error("expected errors on abort, got nothing")
		}
	}
}

func (df *dsortFramework) clearDSortList() {
	for {
		seenRunningDSort := false
		listDSort, err := tutils.ListDSort(df.m.proxyURL, dsortDescAllRegex)
		tassert.CheckFatal(df.m.t, err)
		for k, v := range listDSort {
			if v.Archived {
				err = tutils.RemoveDSort(df.m.proxyURL, k)
				tassert.CheckFatal(df.m.t, err)
			} else {
				// We get here when dsort is aborted because a target goes down
				// but when it rejoins it thinks the dsort is not aborted and doesn't do cleanup
				// this happens in TestDistributedSortKillTargetDuringPhases
				tutils.Logf("Stopping: %v...\n", k)
				err = tutils.AbortDSort(df.m.proxyURL, k)
				tassert.CheckFatal(df.m.t, err)
				seenRunningDSort = true
			}
		}

		if !seenRunningDSort {
			break
		}

		time.Sleep(time.Second)
	}
}

func (df *dsortFramework) checkDSortList(expNumEntries ...int) {
	defer df.clearDSortList()

	expNumEntriesVal := 1
	if len(expNumEntries) > 0 {
		expNumEntriesVal = expNumEntries[0]
	}

	listDSort, err := tutils.ListDSort(df.m.proxyURL, dsortDescCurPrefix)
	tassert.CheckFatal(df.m.t, err)
	actEntries := len(listDSort)

	if expNumEntriesVal != actEntries {
		df.m.t.Fatalf("Incorrect # of dsort proc entries: expected %d, actual %d", expNumEntriesVal, actEntries)
	}
}

// helper for dispatching i-th dSort job
func dispatchDSortJob(m *metadata, i int) {
	dsortFW := &dsortFramework{
		m:                m,
		inputTempl:       fmt.Sprintf("input%d-{0..999}", i),
		outputTempl:      fmt.Sprintf("output%d-{00000..01000}", i),
		tarballCnt:       1000,
		fileInTarballCnt: 50,
		extension:        ".tar",
		maxMemUsage:      "99%",
	}
	dsortFW.init()

	dsortFW.createInputShards()

	tutils.Logln("starting distributed sort...")
	rs := dsortFW.gen()
	managerUUID, err := tutils.StartDSort(m.proxyURL, rs)
	tassert.CheckFatal(m.t, err)

	_, err = tutils.WaitForDSortToFinish(m.proxyURL, managerUUID)
	tassert.CheckFatal(m.t, err)
	tutils.Logln("finished distributed sort")

	metrics, err := tutils.MetricsDSort(m.proxyURL, managerUUID)
	tassert.CheckFatal(m.t, err)
	if len(metrics) != m.originalTargetCount {
		m.t.Errorf("number of metrics %d is not same as number of targets %d", len(metrics), m.originalTargetCount)
	}

	dsortFW.checkOutputShards(5)
}

func waitForDSortPhase(t *testing.T, proxyURL, managerUUID, phaseName string, callback func()) {
	tutils.Logf("waiting for %s phase...\n", phaseName)
	for {
		allMetrics, err := tutils.MetricsDSort(proxyURL, managerUUID)
		if err != nil { // in case of error call callback anyway
			t.Error(err)
			callback()
			break
		}

		phase := true
		for _, metrics := range allMetrics {
			switch phaseName {
			case "extraction":
				phase = phase && (metrics.Extraction.Running || metrics.Extraction.Finished)
			case "sorting":
				phase = phase && (metrics.Sorting.Running || metrics.Sorting.Finished)
			case "creation":
				phase = phase && (metrics.Creation.Running || metrics.Creation.Finished)
			default:
				t.Fatal(phaseName)
			}
		}

		if phase {
			callback()
			break
		}

		time.Sleep(time.Millisecond * 10)
	}
}

func TestDistributedSort(t *testing.T) {
	var (
		err error
		m   = &metadata{
			t:      t,
			bucket: TestLocalBucketName,
		}
		dsortFW = &dsortFramework{
			m:                m,
			tarballCnt:       1000,
			fileInTarballCnt: 100,
			extension:        ".tar",
			maxMemUsage:      "99%",
		}
	)
	if testing.Short() {
		t.Skip(skipping)
	}

	dsortFW.init()

	// Initialize metadata
	m.saveClusterState()
	if m.originalTargetCount < 3 {
		t.Fatalf("Must have 3 or more targets in the cluster, have only %d", m.originalTargetCount)
	}

	dsortFW.clearDSortList()

	// Create local bucket
	tutils.CreateFreshLocalBucket(t, m.proxyURL, m.bucket)
	defer tutils.DestroyLocalBucket(t, m.proxyURL, m.bucket)

	dsortFW.createInputShards()

	tutils.Logln("starting distributed sort...")
	rs := dsortFW.gen()
	managerUUID, err := tutils.StartDSort(m.proxyURL, rs)
	tassert.CheckFatal(t, err)

	_, err = tutils.WaitForDSortToFinish(m.proxyURL, managerUUID)
	tassert.CheckFatal(t, err)
	tutils.Logln("finished distributed sort")

	metrics, err := tutils.MetricsDSort(m.proxyURL, managerUUID)
	tassert.CheckFatal(t, err)
	if len(metrics) != m.originalTargetCount {
		t.Errorf("number of metrics %d is not same as number of targets %d", len(metrics), m.originalTargetCount)
	}

	dsortFW.checkOutputShards(5)
	dsortFW.checkDSortList()
}

func TestDistributedSortWithNonExistingBuckets(t *testing.T) {
	var (
		m = &metadata{
			t:      t,
			bucket: t.Name() + "input",
		}
		dsortFW = &dsortFramework{
			m:                m,
			outputBucket:     t.Name() + "output",
			tarballCnt:       1000,
			fileInTarballCnt: 100,
			extension:        ".tar",
			maxMemUsage:      "99%",
		}
	)
	if testing.Short() {
		t.Skip(skipping)
	}

	dsortFW.init()

	// Initialize metadata
	m.saveClusterState()
	if m.originalTargetCount < 3 {
		t.Fatalf("Must have 3 or more targets in the cluster, have only %d", m.originalTargetCount)
	}

	dsortFW.clearDSortList()

	// Create local output bucket
	tutils.CreateFreshLocalBucket(t, m.proxyURL, dsortFW.outputBucket)

	tutils.Logln("starting distributed sort...")
	rs := dsortFW.gen()
	if _, err := tutils.StartDSort(m.proxyURL, rs); err == nil {
		t.Error("expected dSort job to fail when input bucket does not exist")
	}

	// Now destroy output bucket and create input bucket
	tutils.DestroyLocalBucket(t, m.proxyURL, dsortFW.outputBucket)
	tutils.CreateFreshLocalBucket(t, m.proxyURL, m.bucket)
	defer tutils.DestroyLocalBucket(t, m.proxyURL, m.bucket)

	tutils.Logln("starting second distributed sort...")
	if _, err := tutils.StartDSort(m.proxyURL, rs); err == nil {
		t.Error("expected dSort job to fail when output bucket does not exist")
	}

	dsortFW.checkDSortList(0)
}

func TestDistributedSortWithOutputBucket(t *testing.T) {
	var (
		err error
		m   = &metadata{
			t:      t,
			bucket: t.Name() + "input",
		}
		dsortFW = &dsortFramework{
			m:                m,
			outputBucket:     t.Name() + "output",
			tarballCnt:       1000,
			fileInTarballCnt: 100,
			extension:        ".tar",
			maxMemUsage:      "99%",
		}
	)
	if testing.Short() {
		t.Skip(skipping)
	}

	dsortFW.init()

	// Initialize metadata
	m.saveClusterState()
	if m.originalTargetCount < 3 {
		t.Fatalf("Must have 3 or more targets in the cluster, have only %d", m.originalTargetCount)
	}

	dsortFW.clearDSortList()

	// Create local buckets
	tutils.CreateFreshLocalBucket(t, m.proxyURL, m.bucket)
	defer tutils.DestroyLocalBucket(t, m.proxyURL, m.bucket)

	// Create local output bucket
	tutils.CreateFreshLocalBucket(t, m.proxyURL, dsortFW.outputBucket)
	defer tutils.DestroyLocalBucket(t, m.proxyURL, dsortFW.outputBucket)

	dsortFW.createInputShards()

	tutils.Logln("starting distributed sort...")
	rs := dsortFW.gen()
	managerUUID, err := tutils.StartDSort(m.proxyURL, rs)
	tassert.CheckFatal(t, err)

	_, err = tutils.WaitForDSortToFinish(m.proxyURL, managerUUID)
	tassert.CheckFatal(t, err)
	tutils.Logln("finished distributed sort")

	metrics, err := tutils.MetricsDSort(m.proxyURL, managerUUID)
	tassert.CheckFatal(t, err)
	if len(metrics) != m.originalTargetCount {
		t.Errorf("number of metrics %d is not same as number of targets %d", len(metrics), m.originalTargetCount)
	}

	dsortFW.checkOutputShards(5)
	dsortFW.checkDSortList()
}

// TestDistributedSortParallel runs multiple dSorts in parallel
func TestDistributedSortParallel(t *testing.T) {
	var (
		m = &metadata{
			t:      t,
			bucket: TestLocalBucketName,
		}
		dSortsCount = 5
	)
	if testing.Short() {
		t.Skip(skipping)
	}

	dsortFW := &dsortFramework{m: m}

	// Initialize metadata
	m.saveClusterState()
	if m.originalTargetCount < 3 {
		t.Fatalf("Must have 3 or more targets in the cluster, have only %d", m.originalTargetCount)
	}

	dsortFW.clearDSortList()

	// Create local bucket
	tutils.CreateFreshLocalBucket(t, m.proxyURL, m.bucket)
	defer tutils.DestroyLocalBucket(t, m.proxyURL, m.bucket)

	wg := &sync.WaitGroup{}
	for i := 0; i < dSortsCount; i++ {
		wg.Add(1)
		go func(i int) {
			dispatchDSortJob(m, i)
			wg.Done()
		}(i)
	}
	wg.Wait()

	dsortFW.checkDSortList(dSortsCount)
}

// TestDistributedSortChain runs multiple dSorts one after another
func TestDistributedSortChain(t *testing.T) {
	var (
		m = &metadata{
			t:      t,
			bucket: TestLocalBucketName,
		}
		dSortsCount = 5
	)
	if testing.Short() {
		t.Skip(skipping)
	}

	dsortFW := &dsortFramework{m: m}

	// Initialize metadata
	m.saveClusterState()
	if m.originalTargetCount < 3 {
		t.Fatalf("Must have 3 or more targets in the cluster, have only %d", m.originalTargetCount)
	}

	dsortFW.clearDSortList()

	// Create local bucket
	tutils.CreateFreshLocalBucket(t, m.proxyURL, m.bucket)
	defer tutils.DestroyLocalBucket(t, m.proxyURL, m.bucket)

	for i := 0; i < dSortsCount; i++ {
		dispatchDSortJob(m, i)
	}

	dsortFW.checkDSortList(dSortsCount)
}

func TestDistributedSortShuffle(t *testing.T) {
	var (
		err error
		m   = &metadata{
			t:      t,
			bucket: TestLocalBucketName,
		}
		dsortFW = &dsortFramework{
			m:                m,
			algorithm:        &dsort.SortAlgorithm{Kind: dsort.SortKindShuffle},
			tarballCnt:       1000,
			fileInTarballCnt: 10,
			extension:        ".tar",
			maxMemUsage:      "99%",
		}
	)

	if testing.Short() {
		t.Skip(skipping)
	}

	dsortFW.init()

	// Initialize metadata
	m.saveClusterState()
	if m.originalTargetCount < 3 {
		t.Fatalf("Must have 3 or more targets in the cluster, have only %d", m.originalTargetCount)
	}

	dsortFW.clearDSortList()

	// Create local bucket
	tutils.CreateFreshLocalBucket(t, m.proxyURL, m.bucket)
	defer tutils.DestroyLocalBucket(t, m.proxyURL, m.bucket)

	dsortFW.createInputShards()

	tutils.Logln("starting distributed sort...")
	rs := dsortFW.gen()
	managerUUID, err := tutils.StartDSort(m.proxyURL, rs)
	tassert.CheckFatal(t, err)

	_, err = tutils.WaitForDSortToFinish(m.proxyURL, managerUUID)
	tassert.CheckFatal(t, err)
	tutils.Logln("finished distributed sort")

	metrics, err := tutils.MetricsDSort(m.proxyURL, managerUUID)
	tassert.CheckFatal(t, err)
	if len(metrics) != m.originalTargetCount {
		t.Errorf("number of metrics %d is not same as number of targets %d", len(metrics), m.originalTargetCount)
	}

	dsortFW.checkOutputShards(5)
	dsortFW.checkDSortList()
}

func TestDistributedSortWithDisk(t *testing.T) {
	var (
		err error
		m   = &metadata{
			t:      t,
			bucket: TestLocalBucketName,
		}
		dsortFW = &dsortFramework{
			m:                m,
			outputTempl:      "output-{0..1000}",
			tarballCnt:       100,
			fileInTarballCnt: 10,
			extension:        ".tar",
			maxMemUsage:      "1KB",
		}
	)

	if testing.Short() {
		t.Skip(skipping)
	}

	dsortFW.init()

	// Initialize metadata
	m.saveClusterState()
	if m.originalTargetCount < 3 {
		t.Fatalf("Must have 3 or more targets in the cluster, have only %d", m.originalTargetCount)
	}

	dsortFW.clearDSortList()

	// Create local bucket
	tutils.CreateFreshLocalBucket(t, m.proxyURL, m.bucket)
	defer tutils.DestroyLocalBucket(t, m.proxyURL, m.bucket)

	dsortFW.createInputShards()

	tutils.Logln("starting distributed sort with spilling to disk...")
	rs := dsortFW.gen()
	managerUUID, err := tutils.StartDSort(m.proxyURL, rs)
	tassert.CheckFatal(t, err)

	_, err = tutils.WaitForDSortToFinish(m.proxyURL, managerUUID)
	tassert.CheckFatal(t, err)
	tutils.Logln("finished distributed sort")

	allMetrics, err := tutils.MetricsDSort(m.proxyURL, managerUUID)
	tassert.CheckFatal(t, err)
	if len(allMetrics) != m.originalTargetCount {
		t.Errorf("number of metrics %d is not same as number of targets %d", len(allMetrics), m.originalTargetCount)
	}

	for target, metrics := range allMetrics {
		if metrics.Extraction.ExtractedToDiskCnt == 0 && metrics.Extraction.ExtractedCnt > 0 {
			t.Errorf("target %s did not extract any files do disk", target)
		}
	}

	dsortFW.checkOutputShards(0)
	dsortFW.checkDSortList()
}

func TestDistributedSortZip(t *testing.T) {
	var (
		err error
		m   = &metadata{
			t:      t,
			bucket: TestLocalBucketName,
		}
		dsortFW = &dsortFramework{
			m:                m,
			tarballCnt:       1000,
			fileInTarballCnt: 100,
			extension:        ".zip",
			maxMemUsage:      "60%",
		}
	)

	if testing.Short() {
		t.Skip(skipping)
	}

	dsortFW.init()

	// Initialize metadata
	m.saveClusterState()
	if m.originalTargetCount < 3 {
		t.Fatalf("Must have 3 or more targets in the cluster, have only %d", m.originalTargetCount)
	}

	dsortFW.clearDSortList()

	// Create local bucket
	tutils.CreateFreshLocalBucket(t, m.proxyURL, m.bucket)
	defer tutils.DestroyLocalBucket(t, m.proxyURL, m.bucket)

	dsortFW.createInputShards()

	tutils.Logln("starting distributed sort with compression (.zip)...")
	rs := dsortFW.gen()
	managerUUID, err := tutils.StartDSort(m.proxyURL, rs)
	tassert.CheckFatal(t, err)

	_, err = tutils.WaitForDSortToFinish(m.proxyURL, managerUUID)
	tassert.CheckFatal(t, err)
	tutils.Logln("finished distributed sort")

	allMetrics, err := tutils.MetricsDSort(m.proxyURL, managerUUID)
	tassert.CheckFatal(t, err)
	if len(allMetrics) != m.originalTargetCount {
		t.Errorf("number of metrics %d is not same as number of targets %d", len(allMetrics), m.originalTargetCount)
	}

	dsortFW.checkOutputShards(5)
	dsortFW.checkDSortList()
}

func TestDistributedSortWithCompression(t *testing.T) {
	var (
		err error
		m   = &metadata{
			t:      t,
			bucket: TestLocalBucketName,
		}
		dsortFW = &dsortFramework{
			m:                m,
			tarballCnt:       1000,
			fileInTarballCnt: 50,
			extension:        ".tar.gz",
			maxMemUsage:      "60%",
		}
	)

	if testing.Short() {
		t.Skip(skipping)
	}

	dsortFW.init()

	// Initialize metadata
	m.saveClusterState()
	if m.originalTargetCount < 3 {
		t.Fatalf("Must have 3 or more targets in the cluster, have only %d", m.originalTargetCount)
	}

	dsortFW.clearDSortList()

	// Create local bucket
	tutils.CreateFreshLocalBucket(t, m.proxyURL, m.bucket)
	defer tutils.DestroyLocalBucket(t, m.proxyURL, m.bucket)

	dsortFW.createInputShards()

	tutils.Logln("starting distributed sort with compression (.tar.gz)...")
	rs := dsortFW.gen()
	managerUUID, err := tutils.StartDSort(m.proxyURL, rs)
	tassert.CheckFatal(t, err)

	_, err = tutils.WaitForDSortToFinish(m.proxyURL, managerUUID)
	tassert.CheckFatal(t, err)
	tutils.Logln("finished distributed sort")

	allMetrics, err := tutils.MetricsDSort(m.proxyURL, managerUUID)
	tassert.CheckFatal(t, err)
	if len(allMetrics) != m.originalTargetCount {
		t.Errorf("number of metrics %d is not same as number of targets %d", len(allMetrics), m.originalTargetCount)
	}

	dsortFW.checkOutputShards(5)
	dsortFW.checkDSortList()
}

func TestDistributedSortWithContentInt(t *testing.T) {
	var (
		err error
		m   = &metadata{
			t:      t,
			bucket: TestLocalBucketName,
		}
		dsortFW = &dsortFramework{
			m: m,
			algorithm: &dsort.SortAlgorithm{
				Kind:       dsort.SortKindContent,
				Extension:  ".loss",
				FormatType: extract.FormatTypeInt,
			},
			tarballCnt:       1000,
			fileInTarballCnt: 100,
			extension:        ".tar",
			maxMemUsage:      "90%",
		}
	)
	if testing.Short() {
		t.Skip(skipping)
	}

	dsortFW.init()

	// Initialize metadata
	m.saveClusterState()
	if m.originalTargetCount < 3 {
		t.Fatalf("Must have 3 or more targets in the cluster, have only %d", m.originalTargetCount)
	}

	dsortFW.clearDSortList()

	// Create local bucket
	tutils.CreateFreshLocalBucket(t, m.proxyURL, m.bucket)
	defer tutils.DestroyLocalBucket(t, m.proxyURL, m.bucket)

	dsortFW.createInputShards()

	tutils.Logln("starting distributed sort...")
	rs := dsortFW.gen()
	managerUUID, err := tutils.StartDSort(m.proxyURL, rs)
	tassert.CheckFatal(t, err)

	_, err = tutils.WaitForDSortToFinish(m.proxyURL, managerUUID)
	tassert.CheckFatal(t, err)
	tutils.Logln("finished distributed sort")

	metrics, err := tutils.MetricsDSort(m.proxyURL, managerUUID)
	tassert.CheckFatal(t, err)
	if len(metrics) != m.originalTargetCount {
		t.Errorf("number of metrics %d is not same as number of targets %d", len(metrics), m.originalTargetCount)
	}

	dsortFW.checkOutputShards(5)
	dsortFW.checkDSortList()
}

func TestDistributedSortWithContentFloat(t *testing.T) {
	var (
		err error
		m   = &metadata{
			t:      t,
			bucket: TestLocalBucketName,
		}
		dsortFW = &dsortFramework{
			m: m,
			algorithm: &dsort.SortAlgorithm{
				Kind:       dsort.SortKindContent,
				Extension:  ".cls",
				FormatType: extract.FormatTypeFloat,
			},
			tarballCnt:       1000,
			fileInTarballCnt: 50,
			extension:        ".tar",
			maxMemUsage:      "90%",
		}
	)
	if testing.Short() {
		t.Skip(skipping)
	}

	dsortFW.init()

	// Initialize metadata
	m.saveClusterState()
	if m.originalTargetCount < 3 {
		t.Fatalf("Must have 3 or more targets in the cluster, have only %d", m.originalTargetCount)
	}

	dsortFW.clearDSortList()

	// Create local bucket
	tutils.CreateFreshLocalBucket(t, m.proxyURL, m.bucket)
	defer tutils.DestroyLocalBucket(t, m.proxyURL, m.bucket)

	dsortFW.createInputShards()

	tutils.Logln("starting distributed sort...")
	rs := dsortFW.gen()
	managerUUID, err := tutils.StartDSort(m.proxyURL, rs)
	tassert.CheckFatal(t, err)

	_, err = tutils.WaitForDSortToFinish(m.proxyURL, managerUUID)
	tassert.CheckFatal(t, err)
	tutils.Logln("finished distributed sort")

	metrics, err := tutils.MetricsDSort(m.proxyURL, managerUUID)
	tassert.CheckFatal(t, err)
	if len(metrics) != m.originalTargetCount {
		t.Errorf("number of metrics %d is not same as number of targets %d", len(metrics), m.originalTargetCount)
	}

	dsortFW.checkOutputShards(5)
	dsortFW.checkDSortList()
}

func TestDistributedSortWithContentString(t *testing.T) {
	var (
		err error
		m   = &metadata{
			t:      t,
			bucket: TestLocalBucketName,
		}
		dsortFW = &dsortFramework{
			m: m,
			algorithm: &dsort.SortAlgorithm{
				Kind:       dsort.SortKindContent,
				Extension:  ".smth",
				FormatType: extract.FormatTypeString,
			},
			tarballCnt:       1000,
			fileInTarballCnt: 100,
			extension:        ".tar",
			maxMemUsage:      "90%",
		}
	)
	if testing.Short() {
		t.Skip(skipping)
	}

	dsortFW.init()

	// Initialize metadata
	m.saveClusterState()
	if m.originalTargetCount < 3 {
		t.Fatalf("Must have 3 or more targets in the cluster, have only %d", m.originalTargetCount)
	}

	dsortFW.clearDSortList()

	// Create local bucket
	tutils.CreateFreshLocalBucket(t, m.proxyURL, m.bucket)
	defer tutils.DestroyLocalBucket(t, m.proxyURL, m.bucket)

	dsortFW.createInputShards()

	tutils.Logln("starting distributed sort...")
	rs := dsortFW.gen()
	managerUUID, err := tutils.StartDSort(m.proxyURL, rs)
	tassert.CheckFatal(t, err)

	_, err = tutils.WaitForDSortToFinish(m.proxyURL, managerUUID)
	tassert.CheckFatal(t, err)
	tutils.Logln("finished distributed sort")

	metrics, err := tutils.MetricsDSort(m.proxyURL, managerUUID)
	tassert.CheckFatal(t, err)
	if len(metrics) != m.originalTargetCount {
		t.Errorf("number of metrics %d is not same as number of targets %d", len(metrics), m.originalTargetCount)
	}

	dsortFW.checkOutputShards(5)
	dsortFW.checkDSortList()
}

func TestDistributedSortAbort(t *testing.T) {
	var (
		err error
		m   = &metadata{
			t:      t,
			bucket: TestLocalBucketName,
		}
		dsortFW = &dsortFramework{
			m:                m,
			tarballCnt:       1000,
			fileInTarballCnt: 10,
			extension:        ".tar",
			maxMemUsage:      "60%",
		}
	)

	if testing.Short() {
		t.Skip(skipping)
	}

	dsortFW.init()

	// Initialize metadata
	m.saveClusterState()
	if m.originalTargetCount < 3 {
		t.Fatalf("Must have 3 or more targets in the cluster, have only %d", m.originalTargetCount)
	}

	dsortFW.clearDSortList()

	// Create local bucket
	tutils.CreateFreshLocalBucket(t, m.proxyURL, m.bucket)
	defer tutils.DestroyLocalBucket(t, m.proxyURL, m.bucket)

	dsortFW.createInputShards()

	tutils.Logln("starting distributed sort...")
	rs := dsortFW.gen()
	managerUUID, err := tutils.StartDSort(m.proxyURL, rs)
	tassert.CheckFatal(t, err)

	tutils.Logln("aborting distributed sort...")
	err = tutils.AbortDSort(m.proxyURL, managerUUID)
	tassert.CheckFatal(t, err)

	tutils.Logln("waiting for distributed sort to finish up...")
	aborted, err := tutils.WaitForDSortToFinish(m.proxyURL, managerUUID)
	tassert.CheckFatal(t, err)
	if !aborted {
		t.Error("dsort was not aborted")
	}

	tutils.Logln("checking metrics...")
	allMetrics, err := tutils.MetricsDSort(m.proxyURL, managerUUID)
	tassert.CheckFatal(t, err)
	if len(allMetrics) != m.originalTargetCount {
		t.Errorf("number of metrics %d is not same as number of targets %d", len(allMetrics), m.originalTargetCount)
	}

	for target, metrics := range allMetrics {
		if !metrics.Aborted {
			t.Errorf("dsort was not aborted by target: %s", target)
		}
	}

	dsortFW.checkDSortList()
}

func TestDistributedSortAbortDuringPhases(t *testing.T) {
	var (
		m = &metadata{
			t:      t,
			bucket: TestLocalBucketName,
		}
		dsortFW = &dsortFramework{
			m:                m,
			tarballCnt:       1000,
			fileInTarballCnt: 200,
			extension:        ".tar",
			maxMemUsage:      "40%",
		}
	)

	if testing.Short() {
		t.Skip(skipping)
	}

	dsortFW.init()

	// Initialize metadata
	m.saveClusterState()
	if m.originalTargetCount < 3 {
		t.Fatalf("Must have 3 or more targets in the cluster, have only %d", m.originalTargetCount)
	}

	dsortFW.clearDSortList()

	// Create local bucket
	tutils.CreateFreshLocalBucket(t, m.proxyURL, m.bucket)
	defer tutils.DestroyLocalBucket(t, m.proxyURL, m.bucket)

	for _, phase := range []string{"extraction", "sorting", "creation"} {
		dsortFW.createInputShards()

		tutils.Logf("starting distributed sort (abort on: %s)...\n", phase)
		rs := dsortFW.gen()
		managerUUID, err := tutils.StartDSort(m.proxyURL, rs)
		tassert.CheckFatal(t, err)

		waitForDSortPhase(t, m.proxyURL, managerUUID, phase, func() {
			tutils.Logln("aborting distributed sort...")
			err = tutils.AbortDSort(m.proxyURL, managerUUID)
			tassert.CheckFatal(t, err)
		})

		tutils.Logln("waiting for distributed sort to finish up...")
		aborted, err := tutils.WaitForDSortToFinish(m.proxyURL, managerUUID)
		tassert.CheckFatal(t, err)
		if !aborted {
			t.Error("dsort was not aborted")
		}

		tutils.Logln("checking metrics...")
		allMetrics, err := tutils.MetricsDSort(m.proxyURL, managerUUID)
		tassert.CheckFatal(t, err)
		if len(allMetrics) != m.originalTargetCount {
			t.Errorf("number of metrics %d is not same as number of targets %d", len(allMetrics), m.originalTargetCount)
		}

		for target, metrics := range allMetrics {
			if !metrics.Aborted {
				t.Errorf("dsort was not aborted by target: %s", target)
			}
		}
	}

	dsortFW.checkDSortList(3)
}

func TestDistributedSortKillTargetDuringPhases(t *testing.T) {
	var (
		m = &metadata{
			t:      t,
			bucket: TestLocalBucketName,
		}
		dsortFW = &dsortFramework{
			m:                m,
			outputTempl:      "output-{0..100000}",
			tarballCnt:       1000,
			fileInTarballCnt: 200,
			extension:        ".tar",
			maxMemUsage:      "40%",
		}
	)

	if testing.Short() {
		t.Skip(skipping)
	}

	dsortFW.init()

	// Initialize metadata
	m.saveClusterState()
	if m.originalTargetCount < 3 {
		t.Fatalf("Must have 3 or more targets in the cluster, have only %d", m.originalTargetCount)
	}
	targets := tutils.ExtractTargetNodes(m.smap)

	dsortFW.clearDSortList()

	// Create local bucket
	tutils.CreateFreshLocalBucket(t, m.proxyURL, m.bucket)
	defer tutils.DestroyLocalBucket(t, m.proxyURL, m.bucket)

	for idx, phase := range []string{"extraction", "sorting", "creation"} {
		dsortFW.createInputShards()

		tutils.Logf("starting distributed sort (abort on: %s)...\n", phase)
		rs := dsortFW.gen()
		managerUUID, err := tutils.StartDSort(m.proxyURL, rs)
		tassert.CheckFatal(t, err)

		waitForDSortPhase(t, m.proxyURL, managerUUID, phase, func() {
			tutils.Logln("killing target...")
			err = tutils.UnregisterTarget(m.proxyURL, targets[idx].DaemonID)
			tassert.CheckFatal(t, err)
		})

		tutils.Logln("waiting for distributed sort to finish up...")
		aborted, err := tutils.WaitForDSortToFinish(m.proxyURL, managerUUID)
		tassert.CheckError(t, err)
		if !aborted {
			t.Error("dsort was not aborted")
		}

		tutils.Logln("checking metrics...")
		allMetrics, err := tutils.MetricsDSort(m.proxyURL, managerUUID)
		tassert.CheckError(t, err)
		if len(allMetrics) == m.originalTargetCount {
			t.Errorf("number of metrics %d is same as number of original targets %d", len(allMetrics), m.originalTargetCount)
		}

		for target, metrics := range allMetrics {
			if !metrics.Aborted {
				t.Errorf("dsort was not aborted by target: %s", target)
			}
		}

		m.reregisterTarget(targets[idx])
		time.Sleep(time.Second)
	}

	dsortFW.checkDSortList(3)
}

func TestDistributedSortAddTarget(t *testing.T) {
	var (
		m = &metadata{
			t:      t,
			bucket: TestLocalBucketName,
		}
		dsortFW = &dsortFramework{
			m:                m,
			outputTempl:      "output-{0..100000}",
			tarballCnt:       1000,
			fileInTarballCnt: 100,
			extension:        ".tar",
			maxMemUsage:      "40%",
		}
	)

	if testing.Short() {
		t.Skip(skipping)
	}

	dsortFW.init()

	// Initialize metadata
	m.saveClusterState()
	if m.originalTargetCount < 3 {
		t.Fatalf("Must have 3 or more targets in the cluster, have only %d", m.originalTargetCount)
	}
	targets := tutils.ExtractTargetNodes(m.smap)

	dsortFW.clearDSortList()

	tutils.Logln("killing target...")
	err := tutils.UnregisterTarget(m.proxyURL, targets[0].DaemonID)
	tassert.CheckFatal(t, err)

	// Create local bucket
	tutils.CreateFreshLocalBucket(t, m.proxyURL, m.bucket)
	defer tutils.DestroyLocalBucket(t, m.proxyURL, m.bucket)

	dsortFW.createInputShards()

	tutils.Logln("starting distributed sort...")
	rs := dsortFW.gen()
	managerUUID, err := tutils.StartDSort(m.proxyURL, rs)
	tassert.CheckError(t, err)

	waitForDSortPhase(t, m.proxyURL, managerUUID, "sorting", func() {
		// Reregister target 0
		m.reregisterTarget(targets[0])
		tutils.Logln("reregistering complete")
	})

	tutils.Logln("waiting for distributed sort to finish up...")
	aborted, err := tutils.WaitForDSortToFinish(m.proxyURL, managerUUID)
	tassert.CheckFatal(t, err)
	if aborted {
		t.Error("dsort was aborted")
	}

	tutils.Logln("checking metrics...")
	allMetrics, err := tutils.MetricsDSort(m.proxyURL, managerUUID)
	tassert.CheckFatal(t, err)
	if len(allMetrics) != m.originalTargetCount-1 {
		t.Errorf("number of metrics %d is different than number of targets when dSort started %d", len(allMetrics), m.originalTargetCount-1)
	}

	dsortFW.checkDSortList()
}

func TestDistributedSortMetricsAfterFinish(t *testing.T) {
	var (
		err error
		m   = &metadata{
			t:      t,
			bucket: TestLocalBucketName,
		}
		dsortFW = &dsortFramework{
			m:                m,
			outputTempl:      "output-{0..1000}",
			tarballCnt:       100,
			fileInTarballCnt: 10,
			extension:        ".tar",
			maxMemUsage:      "40%",
		}
	)

	if testing.Short() {
		t.Skip(skipping)
	}

	dsortFW.init()

	// Initialize metadata
	m.saveClusterState()
	if m.originalTargetCount < 3 {
		t.Fatalf("Must have 3 or more targets in the cluster, have only %d", m.originalTargetCount)
	}

	dsortFW.clearDSortList()

	// Create local bucket
	tutils.CreateFreshLocalBucket(t, m.proxyURL, m.bucket)
	defer tutils.DestroyLocalBucket(t, m.proxyURL, m.bucket)

	dsortFW.createInputShards()

	tutils.Logln("starting distributed sort...")
	rs := dsortFW.gen()
	managerUUID, err := tutils.StartDSort(m.proxyURL, rs)
	tassert.CheckFatal(t, err)

	_, err = tutils.WaitForDSortToFinish(m.proxyURL, managerUUID)
	tassert.CheckFatal(t, err)
	tutils.Logln("finished distributed sort")

	allMetrics, err := tutils.MetricsDSort(m.proxyURL, managerUUID)
	tassert.CheckFatal(t, err)
	if len(allMetrics) != m.originalTargetCount {
		t.Errorf("number of metrics %d is not same as number of targets %d", len(allMetrics), m.originalTargetCount)
	}

	dsortFW.checkOutputShards(0)

	tutils.Logln("checking if metrics are still accessible after some time..")
	time.Sleep(time.Second * 2)

	// Check if metrics can be fetched after some time
	allMetrics, err = tutils.MetricsDSort(m.proxyURL, managerUUID)
	tassert.CheckFatal(t, err)
	if len(allMetrics) != m.originalTargetCount {
		t.Errorf("number of metrics %d is not same as number of targets %d", len(allMetrics), m.originalTargetCount)
	}

	dsortFW.checkDSortList()
}

func TestDistributedSortSelfAbort(t *testing.T) {
	var (
		err error
		m   = &metadata{
			t:      t,
			bucket: TestLocalBucketName,
		}
		dsortFW = &dsortFramework{
			m:                m,
			tarballCnt:       1000,
			fileInTarballCnt: 100,
			extension:        ".tar",
			maxMemUsage:      "99%",
		}
	)

	dsortFW.init()

	// Initialize metadata
	m.saveClusterState()
	if m.originalTargetCount < 3 {
		t.Fatalf("Must have 3 or more targets in the cluster, have only %d", m.originalTargetCount)
	}

	dsortFW.clearDSortList()

	// Create local bucket
	tutils.CreateFreshLocalBucket(t, m.proxyURL, m.bucket)
	defer tutils.DestroyLocalBucket(t, m.proxyURL, m.bucket)

	tutils.Logln("starting distributed sort without any files generated...")
	rs := dsortFW.gen()
	managerUUID, err := tutils.StartDSort(m.proxyURL, rs)
	tassert.CheckFatal(t, err)

	_, err = tutils.WaitForDSortToFinish(m.proxyURL, managerUUID)
	tassert.CheckFatal(t, err)
	tutils.Logln("finished distributed sort")

	// Wait a while for all targets to abort
	time.Sleep(2 * time.Second)

	allMetrics, err := tutils.MetricsDSort(m.proxyURL, managerUUID)
	tassert.CheckFatal(t, err)
	if len(allMetrics) != m.originalTargetCount {
		t.Errorf("number of metrics %d is not same as number of targets %d", len(allMetrics), m.originalTargetCount)
	}

	// Check that all nodes have aborted their jobs
	for target, metrics := range allMetrics {
		if !metrics.Aborted {
			t.Errorf("dsort was not aborted by target: %s", target)
		}
	}

	dsortFW.checkDSortList()
}

func TestDistributedSortOnOOM(t *testing.T) {
	t.Skip("test can take more than couple minutes, run it only when necessary")

	var (
		mem = sigar.Mem{}
		m   = &metadata{
			t:      t,
			bucket: TestLocalBucketName,
		}
		dsortFW = &dsortFramework{
			m:                 m,
			fileInTarballCnt:  200,
			fileInTarballSize: 10 * cmn.MiB,
			extension:         ".tar",
			maxMemUsage:       "80%",
		}
	)

	err := mem.Get()
	tassert.CheckFatal(t, err)

	// Calculate number of shards to cause OOM and overestimate it to make sure
	// that if dSort doesn't prevent it, it will happen. Notice that maxMemUsage
	// is 80% so dSort should never go above this number in memory usage.
	dsortFW.tarballCnt = int(float64(mem.ActualFree/uint64(dsortFW.fileInTarballSize)/uint64(dsortFW.fileInTarballCnt)) * 1.4)

	dsortFW.init()

	// Initialize metadata
	m.saveClusterState()
	if m.originalTargetCount < 3 {
		t.Fatalf("Must have 3 or more targets in the cluster, have only %d", m.originalTargetCount)
	}

	dsortFW.clearDSortList()

	// Create local bucket
	tutils.CreateFreshLocalBucket(t, m.proxyURL, m.bucket)
	defer tutils.DestroyLocalBucket(t, m.proxyURL, m.bucket)

	dsortFW.createInputShards()

	tutils.Logln("starting distributed sort...")
	rs := dsortFW.gen()
	managerUUID, err := tutils.StartDSort(m.proxyURL, rs)
	tassert.CheckFatal(t, err)

	_, err = tutils.WaitForDSortToFinish(m.proxyURL, managerUUID)
	tassert.CheckFatal(t, err)
	tutils.Logln("finished distributed sort")

	allMetrics, err := tutils.MetricsDSort(m.proxyURL, managerUUID)
	tassert.CheckFatal(t, err)
	if len(allMetrics) != m.originalTargetCount {
		t.Errorf("number of metrics %d is not same as number of targets %d", len(allMetrics), m.originalTargetCount)
	}

	dsortFW.checkOutputShards(5)
	dsortFW.checkDSortList()
}

func TestDistributedSortMissingShards(t *testing.T) {
	if testing.Short() {
		t.Skip(skipping)
	}

	var (
		m = &metadata{
			t:      t,
			bucket: TestLocalBucketName,
		}
		dsortFW = &dsortFramework{
			m:                m,
			outputTempl:      "output-{0..100000}",
			tarballCnt:       1000,
			tarballCntToSkip: 50,
			fileInTarballCnt: 200,
			extension:        ".tar",
			maxMemUsage:      "40%",
		}
	)

	dsortFW.init()

	// Initialize metadata
	m.saveClusterState()
	if m.originalTargetCount < 3 {
		t.Fatalf("Must have 3 or more targets in the cluster, have only %d", m.originalTargetCount)
	}

	// Create local bucket
	tutils.CreateFreshLocalBucket(t, m.proxyURL, m.bucket)
	defer tutils.DestroyLocalBucket(t, m.proxyURL, m.bucket)
	defer setClusterConfig(t, proxyURL, cmn.SimpleKVs{"distributed_sort.missing_shards": cmn.AbortReaction})

	for _, reaction := range cmn.SupportedReactions {
		dsortFW.clearDSortList()

		setClusterConfig(t, proxyURL, cmn.SimpleKVs{"distributed_sort.missing_shards": reaction})

		dsortFW.createInputShards()

		tutils.Logln("starting distributed sort...")
		rs := dsortFW.gen()
		managerUUID, err := tutils.StartDSort(m.proxyURL, rs)
		tassert.CheckFatal(t, err)

		_, err = tutils.WaitForDSortToFinish(m.proxyURL, managerUUID)
		tassert.CheckFatal(t, err)
		tutils.Logln("finished distributed sort")

		allMetrics, err := tutils.MetricsDSort(m.proxyURL, managerUUID)
		tassert.CheckFatal(t, err)
		if len(allMetrics) != m.originalTargetCount {
			t.Errorf("number of metrics %d is not same as number of targets %d", len(allMetrics), m.originalTargetCount)
		}

		dsortFW.checkReactionResult(reaction, allMetrics, dsortFW.tarballCntToSkip)
	}

	dsortFW.checkDSortList()
}

func TestDistributedSortDuplications(t *testing.T) {
	if testing.Short() {
		t.Skip(skipping)
	}

	var (
		m = &metadata{
			t:      t,
			bucket: TestLocalBucketName,
		}
		dsortFW = &dsortFramework{
			m:                     m,
			outputTempl:           "output-{0..100000}",
			tarballCnt:            1000,
			fileInTarballCnt:      200,
			recordDuplicationsCnt: 50,
			extension:             ".tar",
			maxMemUsage:           "40%",
		}
	)

	dsortFW.init()

	// Initialize metadata
	m.saveClusterState()
	if m.originalTargetCount < 3 {
		t.Fatalf("Must have 3 or more targets in the cluster, have only %d", m.originalTargetCount)
	}

	// Create local bucket
	tutils.CreateFreshLocalBucket(t, m.proxyURL, m.bucket)
	defer tutils.DestroyLocalBucket(t, m.proxyURL, m.bucket)
	defer setClusterConfig(t, proxyURL, cmn.SimpleKVs{"distributed_sort.duplicated_records": cmn.AbortReaction})

	for _, reaction := range cmn.SupportedReactions {
		dsortFW.clearDSortList()

		setClusterConfig(t, proxyURL, cmn.SimpleKVs{"distributed_sort.duplicated_records": reaction})

		dsortFW.createInputShards()

		tutils.Logln("starting distributed sort...")
		rs := dsortFW.gen()
		managerUUID, err := tutils.StartDSort(m.proxyURL, rs)
		tassert.CheckFatal(t, err)

		_, err = tutils.WaitForDSortToFinish(m.proxyURL, managerUUID)
		tassert.CheckFatal(t, err)
		tutils.Logln("finished distributed sort")

		allMetrics, err := tutils.MetricsDSort(m.proxyURL, managerUUID)
		tassert.CheckFatal(t, err)
		if len(allMetrics) != m.originalTargetCount {
			t.Errorf("number of metrics %d is not same as number of targets %d", len(allMetrics), m.originalTargetCount)
		}

		dsortFW.checkReactionResult(reaction, allMetrics, dsortFW.recordDuplicationsCnt)
	}

	dsortFW.checkDSortList()
}
