package types

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
)

const (
	SuperblockMagic = "YKFLOPY1"
	SuperblockVer   = 1
	MaxDisks        = 8
	MaxBlocks       = 96
	MaxFiles        = 16
	MaxNameLen      = 60
	BlockPayload    = 2048
	BlockFree       = 0xFF
	BlockEOF        = 0xFE
	SuperblockSize = 2048
)

const FileFlagInUse uint8 = 0x01

type SuperblockHeader struct {
	Magic        [8]byte
	Version      uint16
	Flags        uint16
	FSUUID       [16]byte
	Sequence     uint64
	BlockPayload uint16
	DiskCount    uint8
	TotalBlocks  uint8
	FileCount    uint8
}

type DiskDesc struct {
	Label    byte
	Flags    byte
	Capacity uint8
	First    uint8
	Serial   uint32
	UUID     [16]byte
}

type FileEntry struct {
	Flags      uint8
	NameLen    uint8
	FirstBlock uint8
	Size       uint32
	Mtime      int64
	Inode      uint32
	Name       [MaxNameLen]byte
}

type Superblock struct {
	Header SuperblockHeader
	Disks  [MaxDisks]DiskDesc
	Bitmap [MaxBlocks / 8]byte
	FAT    [MaxBlocks]byte
	Files  [MaxFiles]FileEntry
	CRC    uint32
}

func (sb *Superblock) Marshal() ([]byte, error) {
	buf := make([]byte, SuperblockSize)
	copy(buf[0:8], sb.Header.Magic[:])
	binary.LittleEndian.PutUint16(buf[8:], sb.Header.Version)
	binary.LittleEndian.PutUint16(buf[10:], sb.Header.Flags)
	copy(buf[12:28], sb.Header.FSUUID[:])
	binary.LittleEndian.PutUint64(buf[28:], sb.Header.Sequence)
	binary.LittleEndian.PutUint16(buf[36:], sb.Header.BlockPayload)
	buf[38] = sb.Header.DiskCount
	buf[39] = sb.Header.TotalBlocks
	buf[40] = sb.Header.FileCount

	off := 64
	for i := 0; i < MaxDisks; i++ {
		d := &sb.Disks[i]
		buf[off] = d.Label
		buf[off+1] = d.Flags
		buf[off+2] = d.Capacity
		buf[off+3] = d.First
		binary.LittleEndian.PutUint32(buf[off+4:], d.Serial)
		copy(buf[off+8:off+24], d.UUID[:])
		off += 28
	}

	copy(buf[off:off+MaxBlocks/8], sb.Bitmap[:])
	off += MaxBlocks / 8
	copy(buf[off:off+MaxBlocks], sb.FAT[:])
	off += MaxBlocks

	for i := 0; i < MaxFiles; i++ {
		f := &sb.Files[i]
		buf[off] = f.Flags
		buf[off+1] = f.NameLen
		buf[off+2] = f.FirstBlock
		buf[off+3] = 0
		binary.LittleEndian.PutUint32(buf[off+4:], f.Size)
		binary.LittleEndian.PutUint64(buf[off+8:], uint64(f.Mtime))
		binary.LittleEndian.PutUint32(buf[off+16:], f.Inode)
		copy(buf[off+24:off+24+MaxNameLen], f.Name[:])
		off += 24 + MaxNameLen
	}

	c := crc32.ChecksumIEEE(buf[:off])
	binary.LittleEndian.PutUint32(buf[off:], c)

	return buf[:off+4], nil
}

func UnmarshalSuperblock(data []byte) (*Superblock, error) {
	if len(data) < 64 {
		return nil, fmt.Errorf("superblock too small")
	}
	sb := &Superblock{}
	copy(sb.Header.Magic[:], data[0:8])
	if string(sb.Header.Magic[:]) != SuperblockMagic {
		return nil, fmt.Errorf("bad magic: %q", sb.Header.Magic)
	}
	sb.Header.Version = binary.LittleEndian.Uint16(data[8:])
	sb.Header.Flags = binary.LittleEndian.Uint16(data[10:])
	copy(sb.Header.FSUUID[:], data[12:28])
	sb.Header.Sequence = binary.LittleEndian.Uint64(data[28:])
	sb.Header.BlockPayload = binary.LittleEndian.Uint16(data[36:])
	sb.Header.DiskCount = data[38]
	sb.Header.TotalBlocks = data[39]
	sb.Header.FileCount = data[40]

	off := 64
	for i := 0; i < MaxDisks; i++ {
		d := &sb.Disks[i]
		d.Label = data[off]
		d.Flags = data[off+1]
		d.Capacity = data[off+2]
		d.First = data[off+3]
		d.Serial = binary.LittleEndian.Uint32(data[off+4:])
		copy(d.UUID[:], data[off+8:off+24])
		off += 28
	}

	copy(sb.Bitmap[:], data[off:off+MaxBlocks/8])
	off += MaxBlocks / 8
	copy(sb.FAT[:], data[off:off+MaxBlocks])
	off += MaxBlocks

	for i := 0; i < MaxFiles; i++ {
		f := &sb.Files[i]
		f.Flags = data[off]
		f.NameLen = data[off+1]
		f.FirstBlock = data[off+2]
		f.Size = binary.LittleEndian.Uint32(data[off+4:])
		f.Mtime = int64(binary.LittleEndian.Uint64(data[off+8:]))
		f.Inode = binary.LittleEndian.Uint32(data[off+16:])
		copy(f.Name[:], data[off+24:off+24+MaxNameLen])
		off += 24 + MaxNameLen
	}

	stored := binary.LittleEndian.Uint32(data[off:])
	computed := crc32.ChecksumIEEE(data[:off])
	if stored != computed {
		return nil, fmt.Errorf("superblock CRC mismatch: stored=%08x computed=%08x", stored, computed)
	}
	sb.CRC = stored

	return sb, nil
}

func (sb *Superblock) BlockUsed(block int) bool {
	return sb.Bitmap[block/8]&(1<<uint(block%8)) != 0
}

func (sb *Superblock) SetBlockUsed(block int, used bool) {
	if used {
		sb.Bitmap[block/8] |= 1 << uint(block%8)
	} else {
		sb.Bitmap[block/8] &^= 1 << uint(block%8)
	}
}

func (sb *Superblock) AllocBlock() (int, error) {
	total := int(sb.Header.TotalBlocks)
	for i := 0; i < total; i++ {
		if !sb.BlockUsed(i) {
			sb.SetBlockUsed(i, true)
			sb.FAT[i] = BlockEOF
			return i, nil
		}
	}
	return -1, fmt.Errorf("no free blocks")
}

func (sb *Superblock) FreeBlock(block int) {
	sb.SetBlockUsed(block, false)
	sb.FAT[block] = BlockFree
}

func (sb *Superblock) BlockChain(first uint8) []int {
	if first == BlockFree || first == BlockEOF {
		return nil
	}
	var chain []int
	b := int(first)
	for b < MaxBlocks && len(chain) < MaxBlocks {
		chain = append(chain, b)
		next := sb.FAT[b]
		if next == BlockEOF || next == BlockFree {
			break
		}
		b = int(next)
	}
	return chain
}

func (sb *Superblock) FindFile(name string) int {
	for i := 0; i < MaxFiles; i++ {
		f := &sb.Files[i]
		if f.Flags&FileFlagInUse == 0 {
			continue
		}
		if f.FileName() == name {
			return i
		}
	}
	return -1
}

func (sb *Superblock) DiskForBlock(block int) int {
	for i := 0; i < int(sb.Header.DiskCount); i++ {
		d := &sb.Disks[i]
		start := int(d.First)
		end := start + int(d.Capacity)
		if block >= start && block < end {
			return i
		}
	}
	return -1
}

func (f *FileEntry) FileName() string {
	return string(f.Name[:f.NameLen])
}

func (f *FileEntry) SetName(name string) {
	n := len(name)
	if n > MaxNameLen {
		n = MaxNameLen
	}
	f.NameLen = uint8(n)
	copy(f.Name[:], name[:n])
}

func DiskLabel(d *DiskDesc) string {
	return string([]byte{d.Label})
}
