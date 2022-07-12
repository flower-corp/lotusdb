package lotusdb

import (
	"bytes"
	"io/ioutil"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/flower-corp/lotusdb/logfile"
	"github.com/stretchr/testify/assert"
)

func TestOpenValueLog(t *testing.T) {
	t.Run("fileio", func(t *testing.T) {
		testOpenValueLog(t, logfile.FileIO)
	})

	t.Run("mmap", func(t *testing.T) {
		testOpenValueLog(t, logfile.MMap)
	})

	t.Run("set file state", func(t *testing.T) {
		path, err := filepath.Abs(filepath.Join("/tmp", "vlog-test"))
		assert.Nil(t, err)
		err = os.MkdirAll(path, os.ModePerm)
		assert.Nil(t, err)

		defer func() {
			_ = os.RemoveAll(path)
		}()
		vlog, err := openValueLogForTest(path, 180, logfile.FileIO, 0.5)
		assert.Nil(t, err)

		_, _, err = vlog.Write(&logfile.LogEntry{Key: GetKey(923), Value: GetValue128B()})
		assert.Nil(t, err)

		// open again, the old active log file is close to full, so we weill create a new active log file.
		vlog1, err := openValueLogForTest(path, 180, logfile.FileIO, 0.5)
		assert.Nil(t, err)
		assert.NotNil(t, vlog1)
	})
}

func testOpenValueLog(t *testing.T, ioType logfile.IOType) {
	path, err := filepath.Abs(filepath.Join("/tmp", "vlog-test"))
	assert.Nil(t, err)
	err = os.MkdirAll(path, os.ModePerm)
	assert.Nil(t, err)

	defer func() {
		_ = os.RemoveAll(path)
	}()
	type args struct {
		path      string
		blockSize int64
		ioType    logfile.IOType
	}
	tests := []struct {
		name     string
		args     args
		hasFiles bool
		wantErr  bool
	}{
		{
			"size-zero", args{path: path, blockSize: 0, ioType: ioType}, false, true,
		},
		{
			"no-files", args{path: path, blockSize: 100, ioType: ioType}, false, false,
		},
		{
			"with-files", args{path: path, blockSize: 100, ioType: ioType}, true, false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.hasFiles {
				for i := 1; i <= 5; i++ {
					_, err := logfile.OpenLogFile(path, uint32(i), 100, logfile.ValueLog, ioType)
					assert.Nil(t, err)
				}
			}
			got, err := openValueLogForTest(tt.args.path, tt.args.blockSize, tt.args.ioType, 0.5)
			if (err != nil) != tt.wantErr {
				t.Errorf("openValueLog() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr {
				assert.NotNil(t, got)
			}
		})
	}
}

func TestValueLog_Write(t *testing.T) {
	t.Run("fileio", func(t *testing.T) {
		testValueLogWrite(t, logfile.FileIO)
	})

	t.Run("mmap", func(t *testing.T) {
		testValueLogWrite(t, logfile.MMap)
	})
}

func testValueLogWrite(t *testing.T, ioType logfile.IOType) {
	path, err := filepath.Abs(filepath.Join("/tmp", "vlog-test"))
	assert.Nil(t, err)
	err = os.MkdirAll(path, os.ModePerm)
	assert.Nil(t, err)

	defer func() {
		_ = os.RemoveAll(path)
	}()
	vlog, err := openValueLogForTest(path, 1024<<20, ioType, 0.5)
	assert.Nil(t, err)

	type fields struct {
		vlog *valueLog
	}
	type args struct {
		e *logfile.LogEntry
	}
	tests := []struct {
		name    string
		fields  fields
		args    args
		want    *valuePos
		wantErr bool
	}{
		// don`t run the sub test alone, because the offset is incremental, run them all at once!!!
		{
			"nil-entry", fields{vlog: vlog}, args{e: nil}, &valuePos{}, false,
		},
		{
			"no-key", fields{vlog: vlog}, args{e: &logfile.LogEntry{Value: []byte("lotusdb")}}, &valuePos{Fid: 0, Offset: 0}, false,
		},
		{
			"no-value", fields{vlog: vlog}, args{e: &logfile.LogEntry{Key: []byte("key1")}}, &valuePos{Fid: 0, Offset: 15}, false,
		},
		{
			"with-key-value", fields{vlog: vlog}, args{e: &logfile.LogEntry{Key: []byte("key2"), Value: []byte("lotusdb-2")}}, &valuePos{Fid: 0, Offset: 27}, false,
		},
		{
			"key-big-value", fields{vlog: vlog}, args{e: &logfile.LogEntry{Key: []byte("key3"), Value: GetValue4K()}}, &valuePos{Fid: 0, Offset: 48}, false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vlog := tt.fields.vlog
			got, _, err := vlog.Write(tt.args.e)
			if (err != nil) != tt.wantErr {
				t.Errorf("Write() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Write() got = %v, want %v", got, tt.want)
			}
			// get from vlog.
			if tt.args.e != nil {
				e, err := vlog.Read(got.Fid, got.Offset)
				assert.Nil(t, err)
				if bytes.Compare(e.Key, tt.args.e.Key) != 0 || bytes.Compare(e.Value, tt.args.e.Value) != 0 {
					t.Errorf("Write() write = %v, but got = %v", tt.args.e, got)
				}
			}
		})
	}
}

func TestValueLog_WriteAfterReopen(t *testing.T) {
	path, err := filepath.Abs(filepath.Join("/tmp", "vlog-test"))
	assert.Nil(t, err)
	err = os.MkdirAll(path, os.ModePerm)
	assert.Nil(t, err)

	defer func() {
		_ = os.RemoveAll(path)
	}()
	vlog, err := openValueLogForTest(path, 100, logfile.FileIO, 0.5)
	assert.Nil(t, err)

	tests := []*logfile.LogEntry{
		{
			Key: []byte("key-1"), Value: []byte("val-1")},
		{
			Key: []byte("key-2"), Value: []byte("val-2"),
		},
	}
	var pos []*valuePos

	pos1, _, err := vlog.Write(tests[0])
	assert.Nil(t, err)
	pos = append(pos, pos1)

	err = vlog.Close()
	assert.Nil(t, err)

	// reopen it.
	vlog, err = openValueLogForTest(path, 100, logfile.MMap, 0.5)
	assert.Nil(t, err)
	pos2, _, err := vlog.Write(tests[1])
	assert.Nil(t, err)
	pos = append(pos, pos2)

	for i := 0; i < len(pos); i++ {
		res, err := vlog.Read(pos[i].Fid, pos[i].Offset)
		assert.Nil(t, err)
		if bytes.Compare(res.Key, tests[i].Key) != 0 || bytes.Compare(res.Value, tests[i].Value) != 0 {
			t.Errorf("WriteAfterReopen() write = %v, but got = %v", tests[i], res)
		}
	}
}

func TestValueLog_WriteUntilNewActiveFileOpen(t *testing.T) {
	t.Run("fileio", func(t *testing.T) {
		testValueLogWriteUntilNewActiveFileOpen(t, logfile.FileIO)
	})

	t.Run("mmap", func(t *testing.T) {
		testValueLogWriteUntilNewActiveFileOpen(t, logfile.MMap)
	})
}

func testValueLogWriteUntilNewActiveFileOpen(t *testing.T, ioType logfile.IOType) {
	path, err := filepath.Abs(filepath.Join("/tmp", "vlog-test"))
	assert.Nil(t, err)
	err = os.MkdirAll(path, os.ModePerm)
	assert.Nil(t, err)

	defer func() {
		_ = os.RemoveAll(path)
	}()
	vlog, err := openValueLogForTest(path, 10<<20, ioType, 0.5)
	assert.Nil(t, err)

	writeCount := 100000
	var poses []*valuePos
	random := rand.Intn(writeCount - 1)
	if random == 0 {
		random++
	}
	for i := 0; i <= writeCount; i++ {
		pos, _, err := vlog.Write(&logfile.LogEntry{Key: GetKey(i), Value: GetValue128B()})
		assert.Nil(t, err)
		if i == 0 || i == writeCount || i == random {
			poses = append(poses, pos)
		}
	}
	// make sure all writes are valid.
	for i := 0; i < len(poses); i++ {
		e, err := vlog.Read(poses[i].Fid, poses[i].Offset)
		assert.Nil(t, err)
		if len(e.Key) == 0 && len(e.Value) == 0 {
			t.Errorf("WriteUntilNewActiveFileOpen() write a valid entry, but got = %v", e)
		}
	}
}

func TestValueLog_Read(t *testing.T) {
	t.Run("fileio", func(t *testing.T) {
		testValueLogRead(t, logfile.FileIO)
	})

	t.Run("mmap", func(t *testing.T) {
		testValueLogRead(t, logfile.MMap)
	})
}

func testValueLogRead(t *testing.T, ioType logfile.IOType) {
	path, err := filepath.Abs(filepath.Join("/tmp", "vlog-test"))
	assert.Nil(t, err)
	err = os.MkdirAll(path, os.ModePerm)
	assert.Nil(t, err)

	defer func() {
		_ = os.RemoveAll(path)
	}()
	vlog, err := openValueLogForTest(path, 10<<20, ioType, 0.5)
	assert.Nil(t, err)

	type data struct {
		e   *logfile.LogEntry
		pos *valuePos
	}
	var datas []*data

	// write some data.
	writeCount := 100000
	random := rand.Intn(writeCount - 1)
	if random == 0 {
		random++
	}
	for i := 0; i <= writeCount; i++ {
		v := GetValue128B()
		pos, _, err := vlog.Write(&logfile.LogEntry{Key: GetKey(i), Value: v})
		assert.Nil(t, err)
		if i == 0 || i == writeCount || i == random {
			datas = append(datas, &data{e: &logfile.LogEntry{Key: GetKey(i), Value: v}, pos: pos})
		}
	}

	type fields struct {
		vlog *valueLog
	}
	type args struct {
		fid    uint32
		offset int64
	}
	tests := []struct {
		name    string
		fields  fields
		args    args
		want    *logfile.LogEntry
		wantErr bool
	}{
		{
			"invalid-fid", fields{vlog: vlog}, args{fid: 100, offset: 0}, nil, true,
		},
		{
			"invalid-offset", fields{vlog: vlog}, args{fid: 0, offset: -23}, nil, true,
		},
		{
			"offset-not-entry", fields{vlog: vlog}, args{fid: 0, offset: 1}, nil, true,
		},
		{
			"valid-0", fields{vlog: vlog}, args{fid: datas[0].pos.Fid, offset: datas[0].pos.Offset}, datas[0].e, false,
		},
		{
			"valid-1", fields{vlog: vlog}, args{fid: datas[1].pos.Fid, offset: datas[1].pos.Offset}, datas[1].e, false,
		},
		{
			"valid-2", fields{vlog: vlog}, args{fid: datas[2].pos.Fid, offset: datas[2].pos.Offset}, datas[2].e, false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vlog := tt.fields.vlog
			got, err := vlog.Read(tt.args.fid, tt.args.offset)
			if (err != nil) != tt.wantErr {
				t.Errorf("Read() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Read() got = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestValueLog_ReadFromArchivedFile(t *testing.T) {
	path, err := filepath.Abs(filepath.Join("/tmp", "vlog-test"))
	assert.Nil(t, err)
	err = os.MkdirAll(path, os.ModePerm)
	assert.Nil(t, err)

	defer func() {
		_ = os.RemoveAll(path)
	}()
	vlog, err := openValueLogForTest(path, 10<<20, logfile.FileIO, 0.5)
	assert.Nil(t, err)

	writeCount := 100000
	for i := 0; i <= writeCount; i++ {
		_, _, err := vlog.Write(&logfile.LogEntry{Key: GetKey(i), Value: GetValue128B()})
		assert.Nil(t, err)
	}
	// close and reopen it.
	err = vlog.Close()
	assert.Nil(t, err)

	vlog1, err := openValueLogForTest(path, 10<<20, logfile.FileIO, 0.5)
	assert.Nil(t, err)
	e, err := vlog1.Read(0, 0)
	assert.Nil(t, err)
	assert.True(t, len(e.Key) > 0)
	assert.True(t, len(e.Value) > 0)
}

func TestValueLog_Sync(t *testing.T) {
	path, err := filepath.Abs(filepath.Join("/tmp", "vlog-test"))
	assert.Nil(t, err)
	err = os.MkdirAll(path, os.ModePerm)
	assert.Nil(t, err)

	defer func() {
		_ = os.RemoveAll(path)
	}()
	vlog, err := openValueLogForTest(path, 10<<20, logfile.FileIO, 0.5)
	assert.Nil(t, err)

	err = vlog.Sync()
	assert.Nil(t, err)
}

func TestValueLog_Close(t *testing.T) {
	path, err := filepath.Abs(filepath.Join("/tmp", "vlog-test"))
	assert.Nil(t, err)
	err = os.MkdirAll(path, os.ModePerm)
	assert.Nil(t, err)

	defer func() {
		_ = os.RemoveAll(path)
	}()
	vlog, err := openValueLogForTest(path, 10<<20, logfile.MMap, 0.5)
	assert.Nil(t, err)

	err = vlog.Close()
	assert.Nil(t, err)
}

func openValueLogForTest(path string, blockSize int64, ioType logfile.IOType, gcRatio float64) (*valueLog, error) {
	opts := vlogOptions{
		path:      path,
		blockSize: blockSize,
		ioType:    ioType,
		gcRatio:   gcRatio,
	}
	vlog := newValueLog(opts)
	err := vlog.Open()

	return vlog, err
}

func TestValueLog_Compaction_Normal(t *testing.T) {
	opts := DefaultOptions("/tmp" + separator + "lotusdb")
	opts.CfOpts.ValueLogFileSize = 16 * 1024 * 1024
	opts.CfOpts.MemtableSize = 32 << 20
	opts.CfOpts.ValueLogGCRatio = 0.5
	opts.CfOpts.ValueLogGCInterval = time.Second * 7

	db, err := Open(opts)
	assert.Nil(t, err)
	defer destroyDB(db)

	// write enough data that can trigger flush operation.
	var writeCount = 600000
	for i := 0; i <= writeCount; i++ {
		err := db.Put(GetKey(i), GetValue128B())
		assert.Nil(t, err)
	}
	for i := 100; i < writeCount/2; i++ {
		err := db.Delete(GetKey(i))
		assert.Nil(t, err)
	}
	// flush again
	for i := writeCount * 2; i <= writeCount*4; i++ {
		err := db.Put(GetKey(i), GetValue128B())
		assert.Nil(t, err)
	}
	time.Sleep(time.Second * 2)
}

//func TestValueLog_Compaction_While_Writing(t *testing.T) {
//	testCompacction(t, false, true)
//}
//
//func TestValueLog_Compaction_While_Reading(t *testing.T) {
//	testCompacction(t, true, false)
//}
//
//func TestValueLog_Compaction_Rewrite(t *testing.T) {
//	testCompacction(t, false, false)
//}

func testCompacction(t *testing.T, reading, writing bool) {
	path, _ := ioutil.TempDir("", "lotusdb")
	opts := DefaultOptions(path)
	opts.CfOpts.ValueLogFileSize = 16 * 1024 * 1024
	opts.CfOpts.MemtableSize = 32 << 20
	opts.CfOpts.ValueLogGCRatio = 0.5
	opts.CfOpts.ValueLogGCInterval = time.Second * 7

	db, err := Open(opts)
	assert.Nil(t, err)
	defer destroyDB(db)

	// write enough data that can trigger flush operation.
	var writeCount = 600000
	for i := 0; i <= writeCount; i++ {
		err := db.Put(GetKey(i), GetValue128B())
		assert.Nil(t, err)
	}
	type kv struct {
		key   []byte
		value []byte
	}
	var kvs []*kv
	for i := 100; i < writeCount/2; i++ {
		k, v := GetKey(i), GetValue128B()
		err := db.Put(k, v)
		assert.Nil(t, err)
		kvs = append(kvs, &kv{key: k, value: v})
	}

	if reading {
		go func() {
			time.Sleep(time.Second)
			rand.Seed(time.Now().UnixNano())
			for {
				k := GetKey(rand.Intn(writeCount / 2))
				_, err := db.Get(k)
				if err != nil {
					t.Log("read data err.", err)
				}
			}
		}()
	}

	if writing {
		go func() {
			time.Sleep(time.Second)
			count := writeCount * 5
			for {
				err := db.Put(GetKey(count), GetValue128B())
				count++
				if err != nil {
					t.Log("write data err.", err)
				}
			}
		}()
	}

	// flush again
	for i := writeCount * 2; i <= writeCount*4; i++ {
		err := db.Put(GetKey(i), GetValue128B())
		assert.Nil(t, err)
	}
	time.Sleep(time.Second * 2)

	for _, kv := range kvs {
		v, err := db.Get(kv.key)
		assert.Nil(t, err)
		assert.Equal(t, v, kv.value)
	}
}
