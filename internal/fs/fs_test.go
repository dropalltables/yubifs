package fs

import (
	"context"
	"sync"
	"syscall"
	"testing"

	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/dropalltables/yubifs/internal/types"
)

// memDevice is an in-memory Device for tests (no YubiKey).
type memDevice struct {
	sb     *types.Superblock
	blocks map[int][]byte
	mu     sync.Mutex
}

func newMemDevice(sb *types.Superblock) *memDevice {
	return &memDevice{
		sb:     sb,
		blocks: make(map[int][]byte),
	}
}

func (m *memDevice) Superblock() *types.Superblock { return m.sb }

func (m *memDevice) ReadBlock(block int) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if b, ok := m.blocks[block]; ok {
		out := make([]byte, types.BlockPayload)
		copy(out, b)
		return out, nil
	}
	return make([]byte, types.BlockPayload), nil
}

func (m *memDevice) WriteBlock(block int, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	buf := make([]byte, types.BlockPayload)
	copy(buf, data)
	m.blocks[block] = buf
	return nil
}

func (m *memDevice) WriteSuperblockToCurrent() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sb.Header.Sequence++
	return nil
}

func testFSuperblock() *types.Superblock {
	sb := &types.Superblock{}
	copy(sb.Header.Magic[:], types.SuperblockMagic)
	sb.Header.Version = types.SuperblockVer
	sb.Header.BlockPayload = types.BlockPayload
	sb.Header.DiskCount = 1
	sb.Header.TotalBlocks = 8
	sb.Header.FileCount = 0
	sb.Disks[0].Label = 'A'
	sb.Disks[0].Capacity = 8
	sb.Disks[0].First = 0
	sb.Disks[0].Serial = 1
	for i := range sb.FAT {
		sb.FAT[i] = types.BlockFree
	}
	return sb
}

func TestLookupENOENT(t *testing.T) {
	root := &YubiFS{Dev: newMemDevice(testFSuperblock())}
	var out fuse.EntryOut
	_, errno := root.Lookup(context.Background(), "missing", &out)
	if errno != syscall.ENOENT {
		t.Fatalf("errno=%v", errno)
	}
}

func TestUnlinkENOENT(t *testing.T) {
	root := &YubiFS{Dev: newMemDevice(testFSuperblock())}
	errno := root.Unlink(context.Background(), "nope")
	if errno != syscall.ENOENT {
		t.Fatalf("errno=%v", errno)
	}
}

func TestStatfs(t *testing.T) {
	sb := testFSuperblock()
	root := &YubiFS{Dev: newMemDevice(sb)}
	var out fuse.StatfsOut
	errno := root.Statfs(context.Background(), &out)
	if errno != 0 {
		t.Fatal(errno)
	}
	if out.Blocks != 8 || out.Bsize != types.BlockPayload {
		t.Fatalf("Statfs: blocks=%d bsize=%d", out.Blocks, out.Bsize)
	}
}

func TestReaddir(t *testing.T) {
	sb := testFSuperblock()
	sb.Files[0].Flags = types.FileFlagInUse
	sb.Files[0].SetName("a.txt")
	sb.Files[0].Inode = 10
	root := &YubiFS{Dev: newMemDevice(sb)}
	ds, errno := root.Readdir(context.Background())
	if errno != 0 {
		t.Fatal(errno)
	}
	var names []string
	for ds.HasNext() {
		e, errno := ds.Next()
		if errno != 0 {
			t.Fatal(errno)
		}
		names = append(names, e.Name)
	}
	if len(names) != 1 || names[0] != "a.txt" {
		t.Fatalf("names=%v", names)
	}
}

func TestUnlinkClearsFile(t *testing.T) {
	sb := testFSuperblock()
	sb.Header.FileCount = 1
	sb.Files[0].Flags = types.FileFlagInUse
	sb.Files[0].SetName("gone.txt")
	sb.Files[0].FirstBlock = types.BlockFree
	sb.Files[0].Size = 0

	root := &YubiFS{Dev: newMemDevice(sb)}
	errno := root.Unlink(context.Background(), "gone.txt")
	if errno != 0 {
		t.Fatal(errno)
	}
	if sb.Header.FileCount != 0 || sb.Files[0].Flags != 0 {
		t.Fatal("file not cleared")
	}
}

func TestYubiFileWriteRead(t *testing.T) {
	sb := testFSuperblock()
	sb.Files[0].Flags = types.FileFlagInUse
	sb.Files[0].SetName("w.txt")
	sb.Files[0].Inode = 1
	sb.Files[0].FirstBlock = types.BlockFree
	sb.Files[0].Size = 0

	dev := newMemDevice(sb)
	root := &YubiFS{Dev: dev}
	yf := &YubiFile{root: root, fileIdx: 0}
	ctx := context.Background()

	payload := []byte("hello, yubifs")
	n, errno := yf.Write(ctx, nil, payload, 0)
	if errno != 0 {
		t.Fatal(errno)
	}
	if n != uint32(len(payload)) {
		t.Fatalf("write n=%d", n)
	}
	if sb.Files[0].Size != uint32(len(payload)) {
		t.Fatalf("size=%d", sb.Files[0].Size)
	}

	buf := make([]byte, 64)
	res, errno := yf.Read(ctx, nil, buf, 0)
	if errno != 0 {
		t.Fatal(errno)
	}
	data, st := res.Bytes(nil)
	if st != fuse.OK {
		t.Fatal(st)
	}
	if string(data) != string(payload) {
		t.Fatalf("read %q want %q", data, payload)
	}
}

func TestYubiFileReadSparse(t *testing.T) {
	sb := testFSuperblock()
	sb.Files[0].Flags = types.FileFlagInUse
	sb.Files[0].SetName("sparse.txt")
	sb.Files[0].Inode = 1
	sb.Files[0].FirstBlock = types.BlockFree
	sb.Files[0].Size = 0

	dev := newMemDevice(sb)
	root := &YubiFS{Dev: dev}
	yf := &YubiFile{root: root, fileIdx: 0}
	ctx := context.Background()

	// Write one byte at end of first block so a second block is allocated and mostly zero.
	off := int64(types.BlockPayload)
	n, errno := yf.Write(ctx, nil, []byte{'x'}, off)
	if errno != 0 || n != 1 {
		t.Fatalf("write errno=%v n=%d", errno, n)
	}

	buf := make([]byte, types.BlockPayload+2)
	res, errno := yf.Read(ctx, nil, buf, 0)
	if errno != 0 {
		t.Fatal(errno)
	}
	data, st := res.Bytes(nil)
	if st != fuse.OK {
		t.Fatal(st)
	}
	if len(data) != types.BlockPayload+1 {
		t.Fatalf("len=%d", len(data))
	}
	for i := 0; i < types.BlockPayload; i++ {
		if data[i] != 0 {
			t.Fatalf("byte %d should be zero", i)
		}
	}
	if data[types.BlockPayload] != 'x' {
		t.Fatal("last byte")
	}
}

func TestYubiFileTruncate(t *testing.T) {
	sb := testFSuperblock()
	sb.Files[0].Flags = types.FileFlagInUse
	sb.Files[0].SetName("t.txt")
	sb.Files[0].Inode = 1
	sb.Files[0].FirstBlock = types.BlockFree
	sb.Files[0].Size = 0

	dev := newMemDevice(sb)
	root := &YubiFS{Dev: dev}
	yf := &YubiFile{root: root, fileIdx: 0}
	ctx := context.Background()

	if _, errno := yf.Write(ctx, nil, []byte("abc"), 0); errno != 0 {
		t.Fatal(errno)
	}

	in := &fuse.SetAttrIn{}
	in.Valid = fuse.FATTR_SIZE
	in.Size = 0
	var out fuse.AttrOut
	if errno := yf.Setattr(ctx, nil, in, &out); errno != 0 {
		t.Fatal(errno)
	}
	if sb.Files[0].Size != 0 || sb.Files[0].FirstBlock != types.BlockFree {
		t.Fatalf("truncate: size=%d first=%d", sb.Files[0].Size, sb.Files[0].FirstBlock)
	}
}
