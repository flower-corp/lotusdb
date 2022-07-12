package lotusdb

import (
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/flower-corp/lotusdb/index"
	"github.com/flower-corp/lotusdb/logfile"
	"github.com/flower-corp/lotusdb/logger"
)

var (
	// ErrActiveLogFileNil active log file not exists.
	ErrActiveLogFileNil = errors.New("active log file not exists")

	// ErrLogFileNil log file not exists.
	ErrLogFileNil = errors.New("log file %d not exists")
)

const vlogDiscardName = "VLOG_DISCARD"

type (
	// valueLog value log is named after the concept in Wisckey paper(https://www.usenix.org/system/files/conference/fast16/fast16-papers-lu.pdf).
	// Values will be stored in value log if its size exceed ValueThreshold in options.
	valueLog struct {
		sync.RWMutex
		opt           vlogOptions
		activeLogFile *logfile.LogFile            // current active log file for writing.
		logFiles      map[uint32]*logfile.LogFile // all log files. Must hold the mutex before modify it.
		cf            *ColumnFamily
		discard       *discard
	}

	// valuePos value position.
	valuePos struct {
		Fid    uint32
		Offset int64
	}

	vlogOptions struct {
		path       string
		blockSize  int64
		ioType     logfile.IOType
		gcRatio    float64
		gcInterval time.Duration
	}
)

func newValueLog(opt vlogOptions) *valueLog {
	return &valueLog{
		opt: opt,
	}
}

// openValueLog create a new value log file.
func (vlog *valueLog) Open() error {
	fileInfos, err := ioutil.ReadDir(vlog.opt.path)
	if err != nil {
		return err
	}

	var fids []uint32
	for _, file := range fileInfos {
		if strings.HasSuffix(file.Name(), logfile.VLogSuffixName) {
			splitNames := strings.Split(file.Name(), ".")
			fid, err := strconv.Atoi(splitNames[0])
			if err != nil {
				return err
			}
			fids = append(fids, uint32(fid))
		}
	}

	// load in order.
	sort.Slice(fids, func(i, j int) bool {
		return fids[i] < fids[j]
	})
	if len(fids) == 0 {
		fids = append(fids, logfile.InitialLogFileId)
	}

	// open discard file.
	discard, err := newDiscard(vlog.opt.path, vlogDiscardName)
	if err != nil {
		return err
	}

	// open active log file only.
	logFile, err := logfile.OpenLogFile(vlog.opt.path, fids[len(fids)-1], vlog.opt.blockSize, logfile.ValueLog, vlog.opt.ioType)
	// set total size in discard file, skip it if exist.
	discard.setTotal(fids[len(fids)-1], uint32(vlog.opt.blockSize))
	if err != nil {
		return err
	}

	vlog.activeLogFile = logFile
	vlog.logFiles = make(map[uint32]*logfile.LogFile)
	vlog.discard = discard

	// load other log files when reading from it.
	for i := 0; i < len(fids)-1; i++ {
		vlog.logFiles[fids[i]] = &logfile.LogFile{Fid: fids[i]}
	}

	if err := vlog.setLogFileState(); err != nil {
		return err
	}
	go vlog.handleCompaction()
	return nil
}

// Read a VLogEntry from a specified vlog file at offset, returns an error, if any.
// If reading from a non-active log file, and the specified file is not open, then we will open it and set it into logFiles.
func (vlog *valueLog) Read(fid uint32, offset int64) (*logfile.LogEntry, error) {
	var logFile *logfile.LogFile
	if fid == vlog.activeLogFile.Fid {
		logFile = vlog.activeLogFile
	} else {
		vlog.RLock()
		logFile = vlog.logFiles[fid]
		if logFile != nil && logFile.IoSelector == nil {
			opt := vlog.opt
			lf, err := logfile.OpenLogFile(opt.path, fid, opt.blockSize, logfile.ValueLog, opt.ioType)
			if err != nil {
				vlog.RUnlock()
				return nil, err
			}
			vlog.logFiles[fid] = lf
			logFile = lf
		}
		vlog.RUnlock()
	}
	if logFile == nil {
		return nil, fmt.Errorf(ErrLogFileNil.Error(), fid)
	}

	entry, _, err := logFile.ReadLogEntry(offset)
	if err == logfile.ErrEndOfEntry {
		return &logfile.LogEntry{}, nil
	}
	return entry, err
}

// Write new VLogEntry to value log file.
// If the active log file is full, it will be closed and a new active file will be created to replace it.
func (vlog *valueLog) Write(ent *logfile.LogEntry) (*valuePos, int, error) {
	vlog.Lock()
	defer vlog.Unlock()
	buf, eSize := logfile.EncodeEntry(ent)
	// if active is reach to thereshold, close it and open a new one.
	if vlog.activeLogFile.WriteAt+int64(eSize) >= vlog.opt.blockSize {
		if err := vlog.Sync(); err != nil {
			return nil, 0, err
		}
		vlog.logFiles[vlog.activeLogFile.Fid] = vlog.activeLogFile

		logFile, err := vlog.createLogFile()
		if err != nil {
			return nil, 0, err
		}
		vlog.activeLogFile = logFile
	}

	err := vlog.activeLogFile.Write(buf)
	if err != nil {
		return nil, 0, err
	}

	writeAt := atomic.LoadInt64(&vlog.activeLogFile.WriteAt)
	return &valuePos{
		Fid:    vlog.activeLogFile.Fid,
		Offset: writeAt - int64(eSize),
	}, eSize, nil
}

// Sync only for the active log file.
func (vlog *valueLog) Sync() error {
	if vlog.activeLogFile == nil {
		return ErrActiveLogFileNil
	}

	vlog.activeLogFile.Lock()
	defer vlog.activeLogFile.Unlock()
	return vlog.activeLogFile.Sync()
}

// Close only for the active log file.
func (vlog *valueLog) Close() error {
	// close discard channel
	vlog.discard.closeChan()

	var vlogErr error
	// close archived log files.
	for _, lf := range vlog.logFiles {
		if err := lf.Close(); err != nil {
			vlogErr = err
		}
	}

	// close active log file.
	if err := vlog.activeLogFile.Close(); err != nil {
		vlogErr = err
	}
	return vlogErr
}

func (vlog *valueLog) createLogFile() (*logfile.LogFile, error) {
	opt := vlog.opt
	fid := vlog.activeLogFile.Fid
	logFile, err := logfile.OpenLogFile(opt.path, fid+1, opt.blockSize, logfile.ValueLog, opt.ioType)
	if err != nil {
		return nil, err
	}
	vlog.discard.setTotal(fid+1, uint32(opt.blockSize))
	return logFile, nil
}

func (vlog *valueLog) setLogFileState() error {
	if vlog.activeLogFile == nil {
		return ErrActiveLogFileNil
	}
	var offset int64 = 0
	for {
		if _, size, err := vlog.activeLogFile.ReadLogEntry(offset); err == nil {
			offset += size
			// No need to use atomic updates.
			// This function is only be executed in one goroutine at startup.
			vlog.activeLogFile.WriteAt += size
		} else {
			if err == io.EOF || err == logfile.ErrEndOfEntry {
				break
			}
			return err
		}
	}
	// if active file`s capacity is nearly close to block size, open a new active file.
	if vlog.activeLogFile.WriteAt+logfile.MaxHeaderSize >= vlog.opt.blockSize {
		vlog.logFiles[vlog.activeLogFile.Fid] = vlog.activeLogFile
		logFile, err := vlog.createLogFile()
		if err != nil {
			return err
		}
		vlog.activeLogFile = logFile
	}
	return nil
}

func (vlog *valueLog) getActiveFid() uint32 {
	var fid uint32
	vlog.Lock()
	if vlog.activeLogFile != nil {
		fid = vlog.activeLogFile.Fid
	}
	vlog.Unlock()
	return fid
}

func (vlog *valueLog) getLogFile(fid uint32) *logfile.LogFile {
	vlog.Lock()
	defer vlog.Unlock()
	return vlog.logFiles[fid]
}

func (vlog *valueLog) handleCompaction() {
	if vlog.opt.gcInterval <= 0 {
		return
	}

	quitSig := make(chan os.Signal, 1)
	signal.Notify(quitSig, os.Interrupt, os.Kill, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
	ticker := time.NewTicker(vlog.opt.gcInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if err := vlog.compact(); err != nil {
				logger.Errorf("value log compaction err: %+v", err)
			}
		case <-quitSig:
		case <-vlog.cf.closedC:
			return
		}
	}
}

func (vlog *valueLog) compact() error {
	rewrite := func(file *logfile.LogFile) error {
		vlog.cf.flushLock.Lock()
		defer vlog.cf.flushLock.Unlock()

		var offset int64
		var validEntries []*logfile.LogEntry
		ts := time.Now().Unix()
		for {
			entry, sz, err := file.ReadLogEntry(offset)
			if err != nil {
				if err == io.EOF || err == logfile.ErrEndOfEntry {
					break
				}
				return err
			}
			var eoff = offset
			offset += sz
			indexMeta, err := vlog.cf.indexer.Get(entry.Key)
			if err != nil {
				return err
			}
			if indexMeta == nil {
				continue
			}
			// if value is already stored in indexer, the value in vlog must be old.
			if len(indexMeta.Value) != 0 {
				continue
			}
			if indexMeta.Fid == file.Fid && indexMeta.Offset == eoff {
				validEntries = append(validEntries, entry)
			}
		}

		var nodes []*index.IndexerNode
		var deletedKeys [][]byte
		// rewrite valid log entries.
		for _, e := range validEntries {
			if e.ExpiredAt != 0 && e.ExpiredAt <= ts {
				deletedKeys = append(deletedKeys, e.Key)
				continue
			}

			valuePos, esize, err := vlog.Write(e)
			if err != nil {
				return err
			}
			nodes = append(nodes, &index.IndexerNode{
				Key: e.Key,
				Meta: &index.IndexerMeta{
					Fid:       valuePos.Fid,
					Offset:    valuePos.Offset,
					EntrySize: esize,
				},
			})
		}
		writeOpts := index.WriteOptions{SendDiscard: false}
		if _, err := vlog.cf.indexer.PutBatch(nodes, writeOpts); err != nil {
			return err
		}
		if len(deletedKeys) > 0 {
			if err := vlog.cf.indexer.DeleteBatch(deletedKeys, writeOpts); err != nil {
				return err
			}
		}
		if err := vlog.cf.indexer.Sync(); err != nil {
			return err
		}
		return nil
	}

	opt := vlog.opt
	activeFid := vlog.getActiveFid()
	ccl, err := vlog.discard.getCCL(activeFid, opt.gcRatio)
	if err != nil {
		return err
	}

	for _, fid := range ccl {
		lf := vlog.getLogFile(fid)
		if lf == nil || lf.IoSelector == nil {
			file, err := logfile.OpenLogFile(opt.path, fid, opt.blockSize, logfile.ValueLog, opt.ioType)
			if err != nil {
				return err
			}
			lf = file
		}

		if err = rewrite(lf); err != nil {
			logger.Warnf("compact rewrite err: %+v", err)
			return err
		}
		// clear discard state.
		vlog.discard.clear(fid)

		// delete older vlog file.
		vlog.Lock()
		if _, ok := vlog.logFiles[fid]; ok {
			delete(vlog.logFiles, fid)
		}
		vlog.Unlock()
		if err = lf.Delete(); err != nil {
			return err
		}
	}
	return nil
}
