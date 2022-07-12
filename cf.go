package lotusdb

import (
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/flower-corp/lotusdb/flock"
	"github.com/flower-corp/lotusdb/index"
	"github.com/flower-corp/lotusdb/logfile"
	"github.com/flower-corp/lotusdb/util"
)

var (
	// ErrColoumnFamilyNil column family name is nil.
	ErrColoumnFamilyNil = errors.New("column family name is nil")

	// ErrWaitMemSpaceTimeout wait enough memtable space for writing timeout.
	ErrWaitMemSpaceTimeout = errors.New("wait enough memtable space for writing timeout, retry later")

	// ErrInvalidVLogGCRatio invalid value log gc ratio.
	ErrInvalidVLogGCRatio = errors.New("invalid value log gc ratio")

	// ErrValueTooBig value is too big.
	ErrValueTooBig = errors.New("value is too big to fit into memtable")
)

// ColumnFamily is a namespace of keys and values.
// Each key/value pair in LotusDB is associated with exactly one Column Family.
// If no Column Family is specified, key-value pair is associated with Column Family "cf_default".
// Column Families provide a way to logically partition the database.
type ColumnFamily struct {
	// Active memtable for writing.
	activeMem *memtable
	// Immutable memtables, waiting to be flushed to disk.
	immuMems []*memtable
	// Value Log(Put value into value log according to options ValueThreshold).
	vlog *valueLog
	// Store keys and meta info.
	indexer index.Indexer
	// When the active memtable is full, send it to the flushChn, see listenAndFlush.
	flushChn  chan *memtable
	flushLock sync.RWMutex // guarantee flush and compaction exclusive.
	opts      ColumnFamilyOptions
	mu        sync.RWMutex
	// Prevent concurrent db using.
	// At least one FileLockGuard(cf/indexer/vlog dirs are all the same).
	// And at most three FileLockGuards(cf/indexer/vlog dirs are all different).
	dirLocks []*flock.FileLockGuard
	// represents whether the cf is closed, 0: false, 1: true.
	closed    uint32
	closedC   chan struct{}
	closeOnce *sync.Once // guarantee the closedC channel is only be closed once.
}

// Stat statistics info of column family.
type Stat struct {
	MemtableSize int64
}

// OpenColumnFamily open a new or existed column family.
func (db *LotusDB) OpenColumnFamily(opts ColumnFamilyOptions) (*ColumnFamily, error) {
	if opts.CfName == "" {
		return nil, ErrColoumnFamilyNil
	}
	// use db path as default column family path.
	if opts.DirPath == "" {
		opts.DirPath = db.opts.DBPath
	}

	// use column family name as cf path name.
	opts.DirPath, _ = filepath.Abs(filepath.Join(opts.DirPath, opts.CfName))
	if opts.IndexerDir == "" {
		opts.IndexerDir = opts.DirPath
	}
	if opts.ValueLogDir == "" {
		opts.ValueLogDir = opts.DirPath
	}
	if opts.ValueLogGCRatio >= 1.0 || opts.ValueLogGCRatio <= 0.0 {
		return nil, ErrInvalidVLogGCRatio
	}

	// return directly if the column family already exists.
	if columnFamily := db.getColumnFamily(opts.CfName); columnFamily != nil {
		return columnFamily, nil
	}
	// create dir paths.
	paths := []string{opts.DirPath, opts.IndexerDir, opts.ValueLogDir}
	for _, path := range paths {
		if !util.PathExist(path) {
			if err := os.MkdirAll(path, os.ModePerm); err != nil {
				return nil, err
			}
		}
	}

	// acquire file lock to lock cf/indexer/vlog directory.
	flocks, err := acquireDirLocks(opts.DirPath, opts.IndexerDir, opts.ValueLogDir)
	if err != nil {
		return nil, fmt.Errorf("another process is using dir.%v", err.Error())
	}

	cf := &ColumnFamily{
		opts:      opts,
		dirLocks:  flocks,
		closedC:   make(chan struct{}),
		closeOnce: new(sync.Once),
		flushChn:  make(chan *memtable, opts.MemtableNums-1),
	}
	// open active and immutable memtables.
	if err := cf.openMemtables(); err != nil {
		return nil, err
	}

	// open value log.
	var ioType = logfile.FileIO
	if opts.ValueLogMmap {
		ioType = logfile.MMap
	}
	vlogOpt := vlogOptions{
		path:       opts.ValueLogDir,
		blockSize:  opts.ValueLogFileSize,
		ioType:     ioType,
		gcRatio:    opts.ValueLogGCRatio,
		gcInterval: opts.ValueLogGCInterval,
	}
	valueLog := newValueLog(vlogOpt)
	cf.vlog = valueLog
	valueLog.cf = cf

	if err := valueLog.Open(); err != nil {
		return nil, err
	}

	// create bptree indexer.
	bptreeOpt := &index.BPTreeOptions{
		IndexType:        index.BptreeBoltDB,
		ColumnFamilyName: opts.CfName,
		BucketName:       []byte(opts.CfName),
		DirPath:          opts.IndexerDir,
		BatchSize:        opts.FlushBatchSize,
		DiscardChn:       cf.vlog.discard.valChan,
	}
	indexer, err := index.NewIndexer(bptreeOpt)
	if err != nil {
		return nil, err
	}
	cf.indexer = indexer

	db.mu.Lock()
	db.cfs[opts.CfName] = cf
	db.mu.Unlock()
	go cf.listenAndFlush()
	return cf, nil
}

// Put put to current column family.
func (cf *ColumnFamily) Put(key, value []byte) error {
	return cf.PutWithOptions(key, value, nil)
}

// PutWithOptions put to current column family with options.
func (cf *ColumnFamily) PutWithOptions(key, value []byte, opt *WriteOptions) error {
	// waiting for enough memtable sapce to write.
	size := uint32(len(key) + len(value))
	if err := cf.waitWritesMemSpace(size); err != nil {
		return err
	}
	if opt == nil {
		opt = new(WriteOptions)
	}
	cf.mu.Lock()
	defer cf.mu.Unlock()
	if err := cf.activeMem.put(key, value, false, *opt); err != nil {
		return err
	}
	return nil
}

// Get get value by the specified key from current column family.
func (cf *ColumnFamily) Get(key []byte) ([]byte, error) {
	tables := cf.getMemtables()
	// get from active and immutable memtables.
	for _, mem := range tables {
		if invalid, value := mem.get(key); len(value) != 0 || invalid {
			return value, nil
		}
	}

	cf.mu.RLock()
	defer cf.mu.RUnlock()
	// get index from bptree.
	indexMeta, err := cf.indexer.Get(key)
	if err != nil {
		return nil, err
	}
	if indexMeta == nil {
		return nil, nil
	}

	// get value from value log.
	if len(indexMeta.Value) == 0 {
		ent, err := cf.vlog.Read(indexMeta.Fid, indexMeta.Offset)
		if err != nil {
			return nil, err
		}
		if ent.ExpiredAt != 0 && ent.ExpiredAt <= time.Now().Unix() {
			return nil, nil
		}
		if len(ent.Value) != 0 {
			return ent.Value, nil
		}
	}
	return indexMeta.Value, nil
}

// Delete delete from current column family.
func (cf *ColumnFamily) Delete(key []byte) error {
	return cf.DeleteWithOptions(key, nil)
}

// DeleteWithOptions delete from current column family with options.
func (cf *ColumnFamily) DeleteWithOptions(key []byte, opt *WriteOptions) error {
	size := uint32(len(key))
	if err := cf.waitWritesMemSpace(size); err != nil {
		return err
	}
	if opt == nil {
		opt = new(WriteOptions)
	}
	cf.mu.Lock()
	defer cf.mu.Unlock()
	if err := cf.activeMem.delete(key, *opt); err != nil {
		return err
	}
	return nil
}

// Stat returns some statistics info of current column family.
func (cf *ColumnFamily) Stat() (*Stat, error) {
	st := &Stat{}
	tables := cf.getMemtables()
	for _, table := range tables {
		st.MemtableSize += int64(table.skl.Size())
	}
	return st, nil
}

// Close close current column family.
func (cf *ColumnFamily) Close() error {
	cf.mu.Lock()
	defer cf.mu.Unlock()
	atomic.StoreUint32(&cf.closed, 1)
	cf.closeOnce.Do(func() { close(cf.closedC) })

	var err error
	// commits the current contents of the file to stable storage
	if syncErr := cf.Sync(); syncErr != nil {
		err = syncErr
	}

	// close all write ahead log files
	if walErr := cf.activeMem.closeWAL(); walErr != nil {
		err = walErr
	}
	for _, mem := range cf.immuMems {
		if walErr := mem.closeWAL(); walErr != nil {
			err = walErr
		}
	}

	// close index data file
	if idxErr := cf.indexer.Close(); idxErr != nil {
		err = idxErr
	}

	// close value log files
	if vlogErr := cf.vlog.Close(); vlogErr != nil {
		err = vlogErr
	}

	// release file locks
	for _, dirLock := range cf.dirLocks {
		if lockErr := dirLock.Release(); lockErr != nil {
			err = lockErr
		}
	}
	return err
}

// IsClosed return whether the column family is closed.
func (cf *ColumnFamily) IsClosed() bool {
	return atomic.LoadUint32(&cf.closed) == 1
}

// Sync syncs the content of current column family to disk.
func (cf *ColumnFamily) Sync() error {
	if err := cf.activeMem.syncWAL(); err != nil {
		return err
	}
	if err := cf.indexer.Sync(); err != nil {
		return err
	}
	return cf.vlog.Sync()
}

// Options returns a copy of current column family options.
func (cf *ColumnFamily) Options() ColumnFamilyOptions {
	return cf.opts
}

func (cf *ColumnFamily) openMemtables() error {
	// read wal dirs.
	fileInfos, err := ioutil.ReadDir(cf.opts.DirPath)
	if err != nil {
		return err
	}

	// find all wal files`id.
	var fids []uint32
	for _, file := range fileInfos {
		if !strings.HasSuffix(file.Name(), logfile.WalSuffixName) {
			continue
		}
		splitNames := strings.Split(file.Name(), ".")
		fid, err := strconv.Atoi(splitNames[0])
		if err != nil {
			return err
		}
		fids = append(fids, uint32(fid))
	}

	// load memtables in order.
	sort.Slice(fids, func(i, j int) bool {
		return fids[i] < fids[j]
	})
	if len(fids) == 0 {
		fids = append(fids, logfile.InitialLogFileId)
	}

	var ioType = logfile.FileIO
	if cf.opts.WalMMap {
		ioType = logfile.MMap
	}
	memOpts := memOptions{
		path:       cf.opts.DirPath,
		fsize:      int64(cf.opts.MemtableSize),
		ioType:     ioType,
		memSize:    cf.opts.MemtableSize,
		bytesFlush: cf.opts.WalBytesFlush,
	}
	for i, fid := range fids {
		memOpts.fid = fid
		table, err := openMemtable(memOpts)
		if err != nil {
			return err
		}
		if i == 0 {
			cf.activeMem = table
		} else {
			cf.immuMems = append(cf.immuMems, table)
		}
	}
	return nil
}

func (cf *ColumnFamily) getMemtables() []*memtable {
	cf.mu.RLock()
	defer cf.mu.RUnlock()

	immuLen := len(cf.immuMems)
	var tables = make([]*memtable, immuLen+1)
	tables[0] = cf.activeMem
	for idx := 0; idx < immuLen; idx++ {
		tables[idx+1] = cf.immuMems[immuLen-idx-1]
	}
	return tables
}

func acquireDirLocks(cfDir, indexerDir, vlogDir string) ([]*flock.FileLockGuard, error) {
	var dirs = []string{cfDir}
	if indexerDir != cfDir {
		dirs = append(dirs, indexerDir)
	}
	if vlogDir != cfDir && vlogDir != indexerDir {
		dirs = append(dirs, vlogDir)
	}

	var flocks []*flock.FileLockGuard
	for _, dir := range dirs {
		lock, err := flock.AcquireFileLock(dir+separator+lockFileName, false)
		if err != nil {
			return nil, err
		}
		flocks = append(flocks, lock)
	}
	return flocks, nil
}
