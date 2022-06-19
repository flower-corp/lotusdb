package index

import (
	"bytes"
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestNewBPTree(t *testing.T) {
	path := t.TempDir()

	type args struct {
		opt BPTreeOptions
	}
	tests := []struct {
		name    string
		args    args
		wantErr bool
	}{
		{
			"normal", args{opt: BPTreeOptions{DirPath: path, IndexType: BptreeBoltDB, ColumnFamilyName: "test-1", BucketName: []byte("test-1"), BatchSize: 1000}}, false,
		},
		{
			"no-dir-path", args{opt: BPTreeOptions{DirPath: "", IndexType: BptreeBoltDB, ColumnFamilyName: "test-1", BucketName: []byte("test-1"), BatchSize: 1000}}, true,
		},
		{
			"no-cf-name", args{opt: BPTreeOptions{DirPath: path, IndexType: BptreeBoltDB, ColumnFamilyName: "", BucketName: []byte("test-1"), BatchSize: 1000}}, true,
		},
		{
			"no-bucket-name", args{opt: BPTreeOptions{DirPath: path, IndexType: BptreeBoltDB, ColumnFamilyName: "test-1", BucketName: nil, BatchSize: 1000}}, true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NewBPTree(tt.args.opt)
			if (err != nil) != tt.wantErr {
				t.Errorf("NewBPTree() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && got == nil {
				t.Errorf("NewBPTree() want a bptree, but got a nil value")
			}
		})
	}
}

func TestBPTree_Put(t *testing.T) {
	path := t.TempDir()
	opts := BPTreeOptions{DirPath: path, IndexType: BptreeBoltDB, ColumnFamilyName: "test", BucketName: []byte("test"), BatchSize: 100000}
	tree, err := NewBPTree(opts)
	assert.Nil(t, err)

	type fields struct {
		bptree *BPTree
	}
	type args struct {
		key   []byte
		value []byte
	}
	tests := []struct {
		name    string
		fields  fields
		args    args
		wantErr bool
	}{
		{
			"nil-key", fields{bptree: tree}, args{key: nil, value: GetValue16B()}, true,
		},
		{
			"nil-value", fields{bptree: tree}, args{key: GetKey(12), value: nil}, false,
		},
		{
			"key-value", fields{bptree: tree}, args{key: GetKey(882), value: GetValue128B()}, false,
		},
		{
			"key-big-value", fields{bptree: tree}, args{key: GetKey(33902), value: GetValue4K()}, false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := tt.fields.bptree
			if err := b.Put(tt.args.key, tt.args.value); (err != nil) != tt.wantErr {
				t.Errorf("Put() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestBPTree_PutBatch(t *testing.T) {
	path := t.TempDir()
	opts := BPTreeOptions{DirPath: path, IndexType: BptreeBoltDB, ColumnFamilyName: "test", BucketName: []byte("test"), BatchSize: 200000, DiscardChn: make(chan [][]byte, 1024)}
	tree, err := NewBPTree(opts)
	assert.Nil(t, err)

	getkv := func(nums int) []*IndexerNode {
		var nodes []*IndexerNode
		for i := nums; i <= nums*2; i++ {
			node := &IndexerNode{Key: GetKey(i)}
			meta := new(IndexerMeta)
			if i%2 == 0 {
				meta.Value = GetValue16B()
			} else {
				meta.Fid = uint32(i)
				meta.Offset = int64(i * 101)
			}
			node.Meta = meta
			nodes = append(nodes, node)
		}
		return nodes
	}

	var testData [][]*IndexerNode
	// 0
	testData = append(testData, nil)
	// 1
	testData = append(testData, getkv(1))
	// 10
	testData = append(testData, getkv(10))
	// 10w
	testData = append(testData, getkv(100000))
	// 20w
	testData = append(testData, getkv(200000))

	type fields struct {
		tree *BPTree
	}
	type args struct {
		nodes []*IndexerNode
	}
	tests := []struct {
		name       string
		fields     fields
		args       args
		wantOffset int
		wantErr    bool
		readKeys   []int
	}{
		{
			"zero", fields{tree: tree}, args{nodes: testData[0]}, len(testData[0]) - 1, false, nil,
		},
		{
			"one", fields{tree: tree}, args{nodes: testData[1]}, len(testData[1]) - 1, false, []int{1},
		},
		{
			"ten", fields{tree: tree}, args{nodes: testData[2]}, len(testData[2]) - 1, false, []int{10, 20},
		},
		{
			"10w", fields{tree: tree}, args{nodes: testData[3]}, len(testData[3]) - 1, false, []int{100000, 200000},
		},
		{
			"20w", fields{tree: tree}, args{nodes: testData[4]}, len(testData[4]) - 1, false, []int{200000, 400000},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := tt.fields.tree
			gotOffset, err := b.PutBatch(tt.args.nodes, WriteOptions{})
			if (err != nil) != tt.wantErr {
				t.Errorf("PutBatch() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if gotOffset != tt.wantOffset {
				t.Errorf("PutBatch() gotOffset = %v, want %v", gotOffset, tt.wantOffset)
			}

			// make sure all writes are valid.
			for _, i := range tt.readKeys {
				meta, err := b.Get(GetKey(i))
				assert.Nil(t, err)
				if len(meta.Value) == 0 && (meta.Fid == 0 && meta.Offset == 0) {
					t.Errorf("PutBatch() got a nil meta, want a valid meta.")
				}
			}
		})
	}
}

const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"

func init() {
	rand.Seed(time.Now().Unix())
}

// GetKey length: 32 Bytes
func GetKey(n int) []byte {
	return []byte("kvstore-bench-key------" + fmt.Sprintf("%09d", n))
}

func GetValue16B() []byte {
	var str bytes.Buffer
	for i := 0; i < 16; i++ {
		str.WriteByte(alphabet[rand.Int()%36])
	}
	return []byte(str.String())
}

func GetValue128B() []byte {
	var str bytes.Buffer
	for i := 0; i < 128; i++ {
		str.WriteByte(alphabet[rand.Int()%36])
	}
	return []byte(str.String())
}

func GetValue4K() []byte {
	var str bytes.Buffer
	for i := 0; i < 4096; i++ {
		str.WriteByte(alphabet[rand.Int()%36])
	}
	return []byte(str.String())
}

func TestBPTree_Get(t *testing.T) {
	path := t.TempDir()
	opts := BPTreeOptions{DirPath: path, IndexType: BptreeBoltDB, ColumnFamilyName: "test", BucketName: []byte("test"), BatchSize: 200000}
	tree, err := NewBPTree(opts)
	assert.Nil(t, err)

	// write some data.
	writeCount := 100
	deleted1 := writeCount - 33
	deleted2 := writeCount - 74
	for i := 0; i <= writeCount; i++ {
		err := tree.Put(GetKey(i), GetValue128B())
		assert.Nil(t, err)
	}

	// delete
	err = tree.Delete(GetKey(deleted1))
	assert.Nil(t, err)
	err = tree.Delete(GetKey(deleted2))
	assert.Nil(t, err)

	type fields struct {
		tree *BPTree
	}
	type args struct {
		key []byte
	}
	tests := []struct {
		name      string
		fields    fields
		args      args
		wantValid bool
		wantErr   bool
	}{
		{
			"nil", fields{tree: tree}, args{key: nil}, false, false,
		},
		{
			"not-exist", fields{tree: tree}, args{key: GetKey(1009932)}, false, false,
		},
		{
			"existed-1", fields{tree: tree}, args{key: GetKey(0)}, true, false,
		},
		{
			"existed-2", fields{tree: tree}, args{key: GetKey(writeCount)}, true, false,
		},
		{
			"deleted-1", fields{tree: tree}, args{key: GetKey(deleted1)}, false, false,
		},
		{
			"deleted-2", fields{tree: tree}, args{key: GetKey(deleted2)}, false, false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := tt.fields.tree
			got, err := b.Get(tt.args.key)
			if (err != nil) != tt.wantErr {
				t.Errorf("Get() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantValid {
				if len(got.Value) == 0 && (got.Fid == 0 && got.Offset == 0) {
					t.Errorf("Get() got a nil meta, want a valid meta")
				}
			}
		})
	}
}

func TestBPTree_Delete(t *testing.T) {
	path := t.TempDir()
	opts := BPTreeOptions{DirPath: path, IndexType: BptreeBoltDB, ColumnFamilyName: "test", BucketName: []byte("test"), BatchSize: 200000, DiscardChn: make(chan [][]byte, 1024)}
	tree, err := NewBPTree(opts)
	assert.Nil(t, err)

	// write some data.
	writeCount := 100
	for i := 0; i <= writeCount; i++ {
		err := tree.Put(GetKey(i), GetValue128B())
		assert.Nil(t, err)
	}

	type fields struct {
		tree *BPTree
	}
	type args struct {
		key []byte
	}
	tests := []struct {
		name    string
		fields  fields
		args    args
		wantErr bool
	}{
		{
			"nil", fields{tree: tree}, args{key: nil}, false,
		},
		{
			"not-exist", fields{tree: tree}, args{key: GetKey(writeCount + 100993)}, false,
		},
		{
			"exist", fields{tree: tree}, args{key: GetKey(writeCount)}, false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := tt.fields.tree
			if err := b.Delete(tt.args.key); (err != nil) != tt.wantErr {
				t.Errorf("Delete() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestBPTree_DeleteBatch(t *testing.T) {
	path := t.TempDir()
	opts := BPTreeOptions{DirPath: path, IndexType: BptreeBoltDB, ColumnFamilyName: "test", BucketName: []byte("test"), BatchSize: 179933, DiscardChn: make(chan [][]byte, 1024)}
	tree, err := NewBPTree(opts)
	assert.Nil(t, err)

	// write some data.
	writeCount := 500000
	var nodes []*IndexerNode
	for i := 0; i <= writeCount; i++ {
		nodes = append(nodes, &IndexerNode{
			Key:  GetKey(i),
			Meta: &IndexerMeta{Value: GetValue16B()},
		})
	}
	_, err = tree.PutBatch(nodes, WriteOptions{})
	assert.Nil(t, err)

	getKeys := func(nums int) [][]byte {
		var keys [][]byte
		for i := nums; i < nums*2; i++ {
			keys = append(keys, GetKey(i))
		}
		return keys
	}

	var deletedKeys [][][]byte
	// 0
	deletedKeys = append(deletedKeys, nil)
	// 1
	deletedKeys = append(deletedKeys, getKeys(1))
	// 10
	deletedKeys = append(deletedKeys, getKeys(10))
	// 10w
	deletedKeys = append(deletedKeys, getKeys(100000))
	// 20w
	deletedKeys = append(deletedKeys, getKeys(200000))

	type fields struct {
		tree *BPTree
	}
	type args struct {
		keys [][]byte
	}
	tests := []struct {
		name    string
		fields  fields
		args    args
		wantErr bool
	}{
		{
			"nil", fields{tree: tree}, args{keys: deletedKeys[0]}, false,
		},
		{
			"one", fields{tree: tree}, args{keys: deletedKeys[1]}, false,
		},
		{
			"ten", fields{tree: tree}, args{keys: deletedKeys[2]}, false,
		},
		{
			"10w", fields{tree: tree}, args{keys: deletedKeys[3]}, false,
		},
		{
			"20w", fields{tree: tree}, args{keys: deletedKeys[4]}, false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := tt.fields.tree
			if err := b.DeleteBatch(tt.args.keys, WriteOptions{}); (err != nil) != tt.wantErr {
				t.Errorf("DeleteBatch() error = %v, wantErr %v", err, tt.wantErr)
			}

			// make sure keys are truly deleted.
			for _, k := range tt.args.keys {
				got, err := b.Get(k)
				assert.Nil(t, err)
				if got != nil && !(len(got.Value) == 0 && (got.Fid == 0 && got.Offset == 0)) {
					t.Log("key = ", string(k))
					t.Errorf("DeleteBatch() want a nil value after deleted, but got = %v", got)
				}
			}
		})
	}
}

func TestBPTree_Sync(t *testing.T) {
	path := t.TempDir()
	opts := BPTreeOptions{DirPath: path, IndexType: BptreeBoltDB, ColumnFamilyName: "test", BucketName: []byte("test"), BatchSize: 200000}
	tree, err := NewBPTree(opts)
	assert.Nil(t, err)

	// write some data.
	writeCount := 5000
	var nodes []*IndexerNode
	for i := 0; i <= writeCount; i++ {
		nodes = append(nodes, &IndexerNode{
			Key:  GetKey(i),
			Meta: &IndexerMeta{Value: GetValue16B()},
		})
	}

	err = tree.Sync()
	assert.Nil(t, err)
}

func TestBPTree_Close(t *testing.T) {
	path := t.TempDir()
	opts := BPTreeOptions{DirPath: path, IndexType: BptreeBoltDB, ColumnFamilyName: "test", BucketName: []byte("test"), BatchSize: 200000}
	tree, err := NewBPTree(opts)
	assert.Nil(t, err)

	// write some data.
	writeCount := 5000
	var nodes []*IndexerNode
	for i := 0; i <= writeCount; i++ {
		nodes = append(nodes, &IndexerNode{
			Key:  GetKey(i),
			Meta: &IndexerMeta{Value: GetValue16B()},
		})
	}

	err = tree.Close()
	assert.Nil(t, err)
}

func TestBPTreeOptions(t *testing.T) {
	opts := &BPTreeOptions{}
	opts.SetDirPath("tmp")
	opts.SetType(BptreeBoltDB)
	opts.SetColumnFamilyName("cf_default")

	opts.GetType()
	opts.GetDirPath()
	opts.GetColumnFamilyName()
}
