package fs

import (
	"context"
	"os"
	"syscall"
	"time"

	"github.com/charmbracelet/log"
	gofs "github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/dropalltables/yubifs/internal/types"
)

type YubiFile struct {
	gofs.Inode
	root    *YubiFS
	fileIdx int
}

var _ = (gofs.NodeGetattrer)((*YubiFile)(nil))
var _ = (gofs.NodeSetattrer)((*YubiFile)(nil))
var _ = (gofs.NodeOpener)((*YubiFile)(nil))
var _ = (gofs.NodeReader)((*YubiFile)(nil))
var _ = (gofs.NodeWriter)((*YubiFile)(nil))
var _ = (gofs.NodeSetxattrer)((*YubiFile)(nil))
var _ = (gofs.NodeGetxattrer)((*YubiFile)(nil))

func (yf *YubiFile) Setxattr(ctx context.Context, attr string, data []byte, flags uint32) syscall.Errno {
	return 0
}

func (yf *YubiFile) Getxattr(ctx context.Context, attr string, dest []byte) (uint32, syscall.Errno) {
	return 0, syscall.ENODATA
}

func (yf *YubiFile) entry() *types.FileEntry {
	return &yf.root.sb().Files[yf.fileIdx]
}

func (yf *YubiFile) Getattr(ctx context.Context, fh gofs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	yf.root.mu.Lock()
	defer yf.root.mu.Unlock()

	f := yf.entry()
	out.Attr.Ino = uint64(f.Inode)
	out.Attr.Size = uint64(f.Size)
	out.Attr.Mode = syscall.S_IFREG | 0o644
	out.Attr.Mtime = uint64(f.Mtime)
	out.Attr.Atime = uint64(f.Mtime)
	out.Attr.Ctime = uint64(f.Mtime)
	out.Attr.Nlink = 1
	out.Attr.Blksize = types.BlockPayload
	out.Attr.Uid = uint32(os.Getuid())
	out.Attr.Gid = uint32(os.Getgid())
	out.AttrValid = 0
	return 0
}

func (yf *YubiFile) Setattr(ctx context.Context, fh gofs.FileHandle, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno {
	yf.root.mu.Lock()
	defer yf.root.mu.Unlock()

	f := yf.entry()
	sb := yf.root.sb()

	if sz, ok := in.GetSize(); ok {
		if err := yf.truncateLocked(sb, f, sz); err != nil {
			log.Error("truncate failed", "file", f.FileName(), "err", err)
			return syscall.EIO
		}
	}

	out.Attr.Size = uint64(f.Size)
	out.Attr.Mode = syscall.S_IFREG | 0o644
	out.Attr.Mtime = uint64(f.Mtime)
	out.Attr.Nlink = 1
	out.Attr.Blksize = types.BlockPayload
	out.AttrValid = 0
	return 0
}

func (yf *YubiFile) truncateLocked(sb *types.Superblock, f *types.FileEntry, newSize uint64) error {
	if newSize == 0 {
		chain := sb.BlockChain(f.FirstBlock)
		for _, b := range chain {
			sb.FreeBlock(b)
		}
		f.FirstBlock = types.BlockFree
		f.Size = 0
		f.Mtime = time.Now().Unix()
		log.Debug("truncate", "file", f.FileName(), "size", 0)
		return yf.root.Dev.WriteSuperblockToCurrent()
	}

	blocksNeeded := int((newSize + uint64(types.BlockPayload) - 1) / uint64(types.BlockPayload))
	chain := sb.BlockChain(f.FirstBlock)

	if blocksNeeded < len(chain) {
		for i := blocksNeeded; i < len(chain); i++ {
			sb.FreeBlock(chain[i])
		}
		if blocksNeeded > 0 {
			sb.FAT[chain[blocksNeeded-1]] = types.BlockEOF
		} else {
			f.FirstBlock = types.BlockFree
		}
	} else if blocksNeeded > len(chain) {
		for i := len(chain); i < blocksNeeded; i++ {
			blk, err := sb.AllocBlock()
			if err != nil {
				return err
			}
			if len(chain) == 0 {
				f.FirstBlock = uint8(blk)
			} else {
				sb.FAT[chain[len(chain)-1]] = uint8(blk)
			}
			chain = append(chain, blk)
		}
	}

	f.Size = uint32(newSize)
	f.Mtime = time.Now().Unix()
	log.Debug("truncate", "file", f.FileName(), "size", newSize)
	return yf.root.Dev.WriteSuperblockToCurrent()
}

func (yf *YubiFile) Open(ctx context.Context, flags uint32) (gofs.FileHandle, uint32, syscall.Errno) {
	return nil, fuse.FOPEN_KEEP_CACHE, 0
}

func (yf *YubiFile) Read(ctx context.Context, fh gofs.FileHandle, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	yf.root.mu.Lock()
	defer yf.root.mu.Unlock()

	f := yf.entry()
	sb := yf.root.sb()

	fileSize := int64(f.Size)
	if off >= fileSize {
		return fuse.ReadResultData(nil), 0
	}

	end := off + int64(len(dest))
	if end > fileSize {
		end = fileSize
	}

	chain := sb.BlockChain(f.FirstBlock)
	buf := make([]byte, end-off)
	pos := off
	written := 0

	log.Debug("file read", "file", f.FileName(), "offset", off, "len", end-off)

	for pos < end {
		blockIdx := int(pos) / types.BlockPayload
		blockOff := int(pos) % types.BlockPayload

		if blockIdx >= len(chain) {
			break
		}

		data, err := yf.root.Dev.ReadBlock(chain[blockIdx])
		if err != nil {
			log.Error("block read failed", "file", f.FileName(),
				"block", chain[blockIdx], "err", err)
			return nil, syscall.EIO
		}

		n := types.BlockPayload - blockOff
		if int64(n) > end-pos {
			n = int(end - pos)
		}
		if blockOff+n > len(data) {
			avail := len(data) - blockOff
			if avail <= 0 {
				break
			}
			n = avail
		}
		copy(buf[written:], data[blockOff:blockOff+n])
		written += n
		pos += int64(n)
	}

	return fuse.ReadResultData(buf[:written]), 0
}

func (yf *YubiFile) Write(ctx context.Context, fh gofs.FileHandle, data []byte, off int64) (uint32, syscall.Errno) {
	yf.root.mu.Lock()
	defer yf.root.mu.Unlock()

	f := yf.entry()
	sb := yf.root.sb()

	endPos := off + int64(len(data))

	blocksNeeded := int((endPos + int64(types.BlockPayload) - 1) / int64(types.BlockPayload))
	chain := sb.BlockChain(f.FirstBlock)

	for len(chain) < blocksNeeded {
		blk, err := sb.AllocBlock()
		if err != nil {
			log.Error("block alloc failed", "file", f.FileName(), "err", err)
			return 0, syscall.ENOSPC
		}
		if len(chain) == 0 {
			f.FirstBlock = uint8(blk)
		} else {
			sb.FAT[chain[len(chain)-1]] = uint8(blk)
		}
		chain = append(chain, blk)
	}

	log.Debug("file write", "file", f.FileName(), "offset", off, "len", len(data))

	pos := off
	srcOff := 0

	for pos < endPos {
		blockIdx := int(pos) / types.BlockPayload
		blockOff := int(pos) % types.BlockPayload

		var blockData []byte
		if blockOff > 0 || int(endPos-pos) < types.BlockPayload {
			existing, err := yf.root.Dev.ReadBlock(chain[blockIdx])
			if err != nil {
				blockData = make([]byte, types.BlockPayload)
			} else {
				blockData = make([]byte, types.BlockPayload)
				copy(blockData, existing)
			}
		} else {
			blockData = make([]byte, types.BlockPayload)
		}

		n := types.BlockPayload - blockOff
		if n > len(data)-srcOff {
			n = len(data) - srcOff
		}
		copy(blockData[blockOff:], data[srcOff:srcOff+n])

		if err := yf.root.Dev.WriteBlock(chain[blockIdx], blockData); err != nil {
			log.Error("block write failed", "file", f.FileName(),
				"block", chain[blockIdx], "err", err)
			return 0, syscall.EIO
		}

		srcOff += n
		pos += int64(n)
	}

	if uint32(endPos) > f.Size {
		f.Size = uint32(endPos)
	}
	f.Mtime = time.Now().Unix()

	if err := yf.root.Dev.WriteSuperblockToCurrent(); err != nil {
		log.Error("superblock sync failed", "file", f.FileName(), "err", err)
		return 0, syscall.EIO
	}

	return uint32(len(data)), 0
}
