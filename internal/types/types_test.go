package types

import (
	"bytes"
	"reflect"
	"testing"
)

func testSuperblock() *Superblock {
	sb := &Superblock{}
	copy(sb.Header.Magic[:], SuperblockMagic)
	sb.Header.Version = SuperblockVer
	sb.Header.BlockPayload = BlockPayload
	sb.Header.DiskCount = 1
	sb.Header.TotalBlocks = 8
	sb.Disks[0].Label = 'A'
	sb.Disks[0].Capacity = 8
	sb.Disks[0].First = 0
	sb.Disks[0].Serial = 42
	for i := range sb.FAT {
		sb.FAT[i] = BlockFree
	}
	return sb
}

func TestMarshalUnmarshalRoundtrip(t *testing.T) {
	sb := testSuperblock()
	sb.Header.Sequence = 7
	sb.Header.FileCount = 0

	data, err := sb.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	got, err := UnmarshalSuperblock(data)
	if err != nil {
		t.Fatal(err)
	}
	sb.CRC = got.CRC
	if !reflect.DeepEqual(sb, got) {
		t.Fatalf("struct mismatch after roundtrip")
	}

	data2, err := got.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, data2) {
		t.Fatalf("marshal bytes differ on second pass")
	}
}

func TestUnmarshalErrors(t *testing.T) {
	_, err := UnmarshalSuperblock([]byte{1, 2, 3})
	if err == nil {
		t.Fatal("expected error for short buffer")
	}

	badMagic := make([]byte, SuperblockSize)
	copy(badMagic, "BADMAGIC")
	_, err = UnmarshalSuperblock(badMagic)
	if err == nil {
		t.Fatal("expected error for bad magic")
	}

	sb := testSuperblock()
	data, err := sb.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)-1] ^= 0xff
	_, err = UnmarshalSuperblock(data)
	if err == nil {
		t.Fatal("expected CRC error")
	}
}

func TestBlockBitmapAllocFree(t *testing.T) {
	sb := testSuperblock()
	for i := 0; i < 8; i++ {
		if sb.BlockUsed(i) {
			t.Fatalf("block %d should be free", i)
		}
	}
	sb.SetBlockUsed(3, true)
	if !sb.BlockUsed(3) {
		t.Fatal("block 3 should be used")
	}
	sb.SetBlockUsed(3, false)
	if sb.BlockUsed(3) {
		t.Fatal("block 3 should be free")
	}

	b, err := sb.AllocBlock()
	if err != nil || b != 0 {
		t.Fatalf("AllocBlock: b=%d err=%v", b, err)
	}
	if sb.FAT[0] != BlockEOF {
		t.Fatalf("FAT[0]=%#x want BlockEOF", sb.FAT[0])
	}
	sb.FreeBlock(0)
	if sb.BlockUsed(0) || sb.FAT[0] != BlockFree {
		t.Fatal("FreeBlock did not reset")
	}
}

func TestAllocBlockExhausted(t *testing.T) {
	sb := testSuperblock()
	sb.Header.TotalBlocks = 2
	for i := 0; i < 2; i++ {
		_, err := sb.AllocBlock()
		if err != nil {
			t.Fatal(err)
		}
	}
	_, err := sb.AllocBlock()
	if err == nil {
		t.Fatal("expected no free blocks")
	}
}

func TestBlockChain(t *testing.T) {
	sb := testSuperblock()
	if ch := sb.BlockChain(BlockFree); len(ch) != 0 {
		t.Fatalf("BlockChain(BlockFree)=%v", ch)
	}
	if ch := sb.BlockChain(BlockEOF); len(ch) != 0 {
		t.Fatalf("BlockChain(BlockEOF)=%v", ch)
	}

	sb.FAT[0] = 1
	sb.FAT[1] = BlockEOF
	ch := sb.BlockChain(0)
	if !reflect.DeepEqual(ch, []int{0, 1}) {
		t.Fatalf("chain=%v", ch)
	}
}

func TestFindFile(t *testing.T) {
	sb := testSuperblock()
	if sb.FindFile("nope") >= 0 {
		t.Fatal("expected -1")
	}
	sb.Files[2].Flags = FileFlagInUse
	sb.Files[2].SetName("hello")
	if sb.FindFile("hello") != 2 {
		t.Fatal("FindFile failed")
	}
}

func TestDiskForBlock(t *testing.T) {
	sb := testSuperblock()
	sb.Header.DiskCount = 2
	sb.Disks[0].First = 0
	sb.Disks[0].Capacity = 3
	sb.Disks[1].First = 3
	sb.Disks[1].Capacity = 3
	sb.Header.TotalBlocks = 6

	if sb.DiskForBlock(0) != 0 || sb.DiskForBlock(2) != 0 {
		t.Fatal("disk 0 range")
	}
	if sb.DiskForBlock(3) != 1 {
		t.Fatal("disk 1 range")
	}
	if sb.DiskForBlock(99) >= 0 {
		t.Fatal("expected -1 for out of range")
	}
}

func TestFileEntrySetName(t *testing.T) {
	var f FileEntry
	long := bytes.Repeat([]byte("x"), MaxNameLen+10)
	f.SetName(string(long))
	if int(f.NameLen) != MaxNameLen {
		t.Fatalf("NameLen=%d", f.NameLen)
	}
	f.SetName("ok")
	if f.FileName() != "ok" {
		t.Fatalf("got %q", f.FileName())
	}
}

func TestDiskLabel(t *testing.T) {
	d := DiskDesc{Label: 'Z'}
	if DiskLabel(&d) != "Z" {
		t.Fatal(DiskLabel(&d))
	}
}
