package core

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	lru "github.com/hashicorp/golang-lru"
	"github.com/vmihailenco/msgpack/v5"
)

// ErrKeyNotFound occurs when a key is not found in the data store
var ErrKeyNotFound = errors.New("key not found")

// logEntryIndexByKey map that holds log entries by the associated key
type logEntryIndexByKey map[string]*LogEntryIndex

// Engine thread safe storage engine that uses the hash index strategy for keeping track
// where data is located on disk
type Engine struct {
	logEntryIndexesBySegmentID map[string]logEntryIndexByKey // map that holds log entry indexes by segment id
	segments                   []string                      // historical list of segments id's
	lruSegments                *lru.Cache                    // cache that holds the most recently used data segments
	segment                    *dataSegment                  // current data segment
	segmentMaxSize             int                           // max size of entries stored in a data segment
	compactorInterval          time.Duration                 // intervals that compaction process occurs
	mu                         *sync.RWMutex                 // mutex that synchronizes access to properties
	compactorWorkerCount       int                           // number of workers compaction process uses
	snapshotTTLDuration        time.Duration                 // snapshot files time to live duration
	isCompactingSegments       bool                          // flag that ensures only one segments compaction process is running at a time
	isCompactingSnapshots      bool                          // flag that ensures only one snapshots compaction process is running at a time

	ctx context.Context
}

// EngineConfig configuration properties utilized when initializing an engine
type EngineConfig struct {
	SegmentMaxSize             int           // max size of entries stored in a data segment
	SnapshotInterval           time.Duration // intervals that snapshots are captured
	TolerableSnapshotFailCount int           // max number of acceptable failures during the snapshotting
	CacheSize                  int           // max number of data segments to hold in memory
	CompactorInterval          time.Duration // intervals that compaction process occurs
	CompactorWorkerCount       int           // number of workers compaction process uses
	SnapshotTTLDuration        time.Duration // snapshot files time to live duration
}

// captureSnapshots captures snapshots at an interval
func (engine *Engine) captureSnapshots(ctx context.Context, interval time.Duration, tolerableFailCount int) {
	failCounts := 0
	for {
		if failCounts >= tolerableFailCount {
			panic("snapshotting failure")
		}

		select {
		case <-time.Tick(interval):
			engine.mu.RLock()
			err := engine.snapshot()
			if err != nil {
				fmt.Printf("error %s", err)
				failCounts++
			}
			engine.mu.RUnlock()
		case <-ctx.Done():
			return
		}
	}
}

// checkDataSegment verifies data segments and performs handoff between old and
// new data segments
// this method is expected to be used within a writable thread safe method
func (engine *Engine) checkDataSegment() error {
	if engine.segment.entriesCount >= engine.segmentMaxSize {
		// switch to new segment
		if err := engine.segment.close(); err != nil {
			return err
		}

		if err := engine.snapshot(); err != nil {
			return err
		}

		newDataSegment, err := newDataSegment()

		if err != nil {
			return err
		}

		engine.segment = newDataSegment
		engine.segments = append(engine.segments, newDataSegment.id)

		//fmt.Println("switched to new segment")
	}
	return nil
}

// addLogEntryIndex stores log entry index in memory
func (engine *Engine) addLogEntryIndex(key string, logEntryIndex *LogEntryIndex) {
	_, ok := engine.logEntryIndexesBySegmentID[engine.segment.id]

	if !ok {
		engine.logEntryIndexesBySegmentID[engine.segment.id] = make(logEntryIndexByKey)
	}

	engine.logEntryIndexesBySegmentID[engine.segment.id][key] = logEntryIndex
}

// Set stores a key and it's associated value
func (engine *Engine) Set(key, value string) error {
	engine.mu.Lock()
	defer engine.mu.Unlock()

	err := engine.checkDataSegment()

	if err != nil {
		return err
	}

	logEntry := NewLogEntry(key, value)
	logEntryIndex, err := engine.segment.addLogEntry(logEntry)

	if err != nil {
		return nil
	}

	engine.addLogEntryIndex(key, logEntryIndex)

	return nil
}

// loadSegment loads a data segment attempting to hit the cache first
func (engine *Engine) loadSegment(segmentID string) (*dataSegment, error) {
	var segment *dataSegment
	cacheHit, ok := engine.lruSegments.Get(segmentID)

	if !ok {
		loadedSegment, err := loadDataSegment(segmentID)
		if err != nil {
			return nil, err
		}

		segment = loadedSegment
		engine.lruSegments.Add(segmentID, segment)
	} else {
		segment = cacheHit.(*dataSegment)
	}

	return segment, nil
}

// findLogEntryByKey locates the log entry of provided key by locating
// the latest data segment containing that kejjy
func (engine *Engine) findLogEntryByKey(key string) (*LogEntry, error) {
	var segment *dataSegment
	cursor := len(engine.segments) - 1

	for cursor >= 0 {
		segmentID := engine.segments[cursor]
		logEntryIndexesByKey, _ := engine.logEntryIndexesBySegmentID[segmentID]
		logEntryIndex, ok := logEntryIndexesByKey[key]
		cursor--

		if !ok {
			continue
		}

		if segmentID != engine.segment.id {
			var err error
			segment, err = engine.loadSegment(segmentID)

			if err != nil {
				return nil, err
			}
		} else {
			segment = engine.segment
		}

		return segment.getLogEntry(logEntryIndex)
	}

	return nil, ErrKeyNotFound
}

// Get retrieves stored value for associated key
func (engine *Engine) Get(key string) (string, error) {
	engine.mu.RLock()
	defer engine.mu.RUnlock()

	logEntry, err := engine.findLogEntryByKey(key)
	if err != nil {
		return "", err
	}

	if logEntry.IsDeleted {
		return "", ErrKeyNotFound
	}

	return logEntry.Value, nil
}

// Delete deletes a key by appending a tombstone log entry to the latest data
// segment
func (engine *Engine) Delete(key string) error {
	//fmt.Println(engine)
	engine.mu.Lock()
	defer engine.mu.Unlock()

	engine.checkDataSegment()
	logEntry, err := engine.findLogEntryByKey(key)

	if err != nil {
		return err
	}

	logEntry.IsDeleted = true
	logEntry.Value = ""
	logEntryIndex, err := engine.segment.addLogEntry(logEntry)

	if err != nil {
		return err
	}

	engine.addLogEntryIndex(key, logEntryIndex)

	return nil
}

// Close closes the storage engine
func (engine *Engine) Close() error {

	if err := engine.segment.close(); err != nil {
		return err
	}

	if err := engine.snapshot(); err != nil {
		return err
	}

	engine.ctx.Done()

	return nil
}

// snapshot writes a snapshot of log entry indexes by segment id to disk
func (engine *Engine) snapshot() error {
	snapshotEntry, err := newSnapshotEntry(engine.logEntryIndexesBySegmentID)
	if err != nil {
		return err
	}

	file, err := os.Create(snapshotEntry.ComputeFilename())
	if err != nil {
		return err
	}

	snapshotEntryBytes, err := snapshotEntry.Encode()
	if err != nil {
		return err
	}

	if _, err := file.Write(snapshotEntryBytes); err != nil {
		return err
	}

	if err = file.Close(); err != nil {
		return err
	}

	return nil
}

// loadSnapshot loads snapshot from disk to memory
func (engine *Engine) loadSnapshot(fileName string) error {
	snapshotBytes, err := ioutil.ReadFile(fileName)
	if err != nil {
		return err
	}

	snapshot := new(SnapshotEntry)
	err = snapshot.Decode(snapshotBytes)

	if err != nil {
		return err
	}

	err = msgpack.Unmarshal(snapshot.Snapshot, &engine.logEntryIndexesBySegmentID)
	if err != nil {
		return err
	}

	for segmentID := range engine.logEntryIndexesBySegmentID {
		engine.segments = append(engine.segments, segmentID)
	}

	return nil
}

// isRecoverable checkes if storage engine contains existing data of which's logn
// entry indexes can be recovered
func (engine *Engine) isRecoverable() (bool, error) {
	files, err := ioutil.ReadDir(getSnapshotsPath())
	if err != nil {
		return false, err
	}

	if _, err := ioutil.ReadDir(getSegmentsPath()); err != nil {
		return false, err
	}

	for _, file := range files {
		if strings.Contains(file.Name(), ".snapshot") {
			return true, nil
		}
	}

	return false, nil
}

type compactedSegmentEntriesContext struct {
	compactedEntries map[string]*LogEntry // key to log entry
	timestamp        int64
}

type jobContext struct {
	timestamp       int64
	segmentID       string
	logEntriesBytes [][]byte
}

type segmentContext struct {
	fileName string
	id       string
}

// compactSegments compacts data segments by joining closed segments together
// and getting rid of duplicaate log engtries by keys
func (engine *Engine) compactSegments() error {
	files, err := ioutil.ReadDir(getSegmentsPath())
	if err != nil {
		return err
	}

	compactedSegmentEntries := make([]compactedSegmentEntriesContext, 0)
	wg := new(sync.WaitGroup)
	mu := new(sync.Mutex)
	jobChan := make(chan *jobContext)
	jobCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	segmentsToDelete := make([]segmentContext, 0)

	// start compaction workers
	for i := 1; i <= engine.compactorWorkerCount; i++ {
		go func(ctx context.Context) {
			for {
				select {
				case <-ctx.Done():
					return

				case jobData := <-jobChan:
					//fmt.Println(fmt.Sprintf("received job for %s", jobData.segmentID))
					latestLogEntries := make(map[string]*LogEntry)

					for _, logEntryBytes := range jobData.logEntriesBytes {
						if len(logEntryBytes) <= 0 {
							continue
						}

						logEntry := &LogEntry{}
						err := logEntry.Decode(logEntryBytes)

						if err != nil {
							continue
						}

						latestLogEntries[logEntry.Key] = logEntry
					}

					mu.Lock()
					compactedSegmentEntries = append(compactedSegmentEntries, compactedSegmentEntriesContext{
						compactedEntries: latestLogEntries,
						timestamp:        jobData.timestamp,
					})
					mu.Unlock()
					wg.Done()
				}
			}
		}(jobCtx)
	}

	// send compaction jobs to workers through shared channel
	for _, file := range files {
		if !strings.Contains(file.Name(), ".segment") {
			continue
		}

		segmentID := strings.Split(file.Name(), ".")[0]
		segmentContentBytes, err := ioutil.ReadFile(path.Join(getSegmentsPath(), file.Name()))

		if err != nil {
			return err
		}

		wg.Add(1)

		logEntriesBytes := bytes.Split(segmentContentBytes, LogEntrySeperator)
		jobChan <- &jobContext{
			timestamp:       file.ModTime().Unix(),
			logEntriesBytes: logEntriesBytes,
			segmentID:       segmentID,
		}
		segmentsToDelete = append(segmentsToDelete, segmentContext{
			fileName: file.Name(),
			id:       segmentID,
		})
	}

	wg.Wait()

	compactedLogEntries := make(map[string]*LogEntry)

	// sorts compacted segment entries by timestamp in order to have the latest
	// keys updated last
	sort.Slice(compactedSegmentEntries, func(a, b int) bool {
		return compactedSegmentEntries[a].timestamp < compactedSegmentEntries[b].timestamp
	})

	for _, compactedSegmentEntry := range compactedSegmentEntries {
		for key, logEntry := range compactedSegmentEntry.compactedEntries {
			compactedLogEntries[key] = logEntry
		}
	}

	compactedSegment, err := newDataSegment()
	if err != nil {
		return err
	}

	// writes log entries to to compacted segment and index it in memory
	for _, logEntry := range compactedLogEntries {
		logEntryIndex, err := compactedSegment.addLogEntry(logEntry)
		if err != nil {
			return err
		}

		engine.addLogEntryIndex(logEntry.Key, logEntryIndex)
	}

	engine.segments = append(engine.segments, compactedSegment.id)
	if err := compactedSegment.close(); err != nil {
		return err
	}

	// clean up segment references from memory
	for _, segmentCtx := range segmentsToDelete {
		delete(engine.logEntryIndexesBySegmentID, segmentCtx.id)
		os.Remove(path.Join(getSegmentsPath(), segmentCtx.fileName))
	}

	return nil
}

// compactSnapshots compacts snapshots
func (engine *Engine) compactSnapshots() error {
	//fmt.Println("started snapshot compaction")

	deletedCount := 0
	now := time.Now()

	files, err := ioutil.ReadDir(getSnapshotsPath())
	if err != nil {
		return err
	}

	for _, file := range files {
		if now.Sub(file.ModTime()) > engine.snapshotTTLDuration {
			deletedCount++
			os.Remove(path.Join(getSnapshotsPath(), file.Name()))
		}
	}

	//fmt.Printf("deleted %d snapshots \n", deletedCount)

	return nil
}

// startCompactor start segment and snspshot compaaction go routines
func (engine *Engine) startCompactors(ctx context.Context) error {
	ticker := time.NewTicker(engine.compactorInterval)
	errChan := make(chan error, 1)

	// segment compactor
	go func() {
		for {
			select {
			case <-ctx.Done():
				fmt.Println("stopping compactor process")
				return

			case <-ticker.C:
				engine.mu.Lock()
				if engine.isCompactingSegments == false {
					engine.isCompactingSegments = true

					if err := engine.compactSegments(); err != nil {
						errChan <- err
					}

					engine.isCompactingSegments = false
				} else {
					fmt.Println("double segments compaction mitigated")
				}
				engine.mu.Unlock()
			}
		}
	}()

	// snapshot compactor
	go func() {
		for {
			select {
			case <-ctx.Done():
				fmt.Println("stopping compactor process")
				return

			case <-ticker.C:
				if engine.isCompactingSnapshots == false {
					engine.isCompactingSnapshots = true

					if err := engine.compactSnapshots(); err != nil {
						errChan <- err
					}

					engine.isCompactingSnapshots = false
				}
			}
		}
	}()

	select {
	case <-ctx.Done():
		return nil

	case err := <-errChan:
		panic(fmt.Sprintf("compactor error %s", err))
	}
}

// recover recovers database indexes from snapshots
func (engine *Engine) recover() error {
	files, err := ioutil.ReadDir(getSnapshotsPath())
	if err != nil {
		return err
	}

	latestSnapshotFilename := ""

	for _, file := range files {
		if latestSnapshotFilename == "" {
			latestSnapshotFilename = file.Name()
			continue
		}

		fileTimeStamp, err := strconv.Atoi(strings.Split(file.Name(), "-")[0])
		if err != nil {
			return err
		}

		latestSnapshotTimestamp, err := strconv.Atoi(strings.Split(latestSnapshotFilename, "-")[0])
		if err != nil {
			return err
		}

		if fileTimeStamp >= latestSnapshotTimestamp {
			latestSnapshotFilename = file.Name()
		}
	}

	err = engine.loadSnapshot(path.Join(getSnapshotsPath(), latestSnapshotFilename))
	if err != nil {
		return err
	}

	return nil
}

//NewEngine creates a new engine
func NewEngine(config *EngineConfig) (*Engine, error) {
	for _, dataPath := range []string{getDataPath(), getSegmentsPath(), getSnapshotsPath()} {
		if _, err := os.Stat(dataPath); os.IsNotExist(err) {
			err = os.MkdirAll(dataPath, 0777)
			if err != nil {
				return nil, err
			}
		}
	}

	segment, err := newDataSegment()
	if err != nil {
		return nil, err
	}

	lruCache, err := lru.New(config.CacheSize)
	if err != nil {
		return nil, err
	}

	engine := &Engine{
		logEntryIndexesBySegmentID: make(map[string]logEntryIndexByKey),
		segmentMaxSize:             config.SegmentMaxSize,
		mu:                         new(sync.RWMutex),
		segment:                    segment,
		lruSegments:                lruCache,
		segments:                   []string{segment.id},
		ctx:                        context.Background(),
		compactorInterval:          config.CompactorInterval,
		compactorWorkerCount:       config.CompactorWorkerCount,
		snapshotTTLDuration:        config.SnapshotTTLDuration,
		isCompactingSegments:       false,
		isCompactingSnapshots:      false,
	}

	recoverable, err := engine.isRecoverable()
	if err != nil {
		return nil, err
	}

	if recoverable {
		//fmt.Println("recovering database")
		if err = engine.recover(); err != nil {
			return nil, err
		}
		//fmt.Println("recovered database")
	}

	go engine.captureSnapshots(engine.ctx, config.SnapshotInterval, config.TolerableSnapshotFailCount)
	go engine.startCompactors(engine.ctx)

	return engine, nil
}
