package fs

import (
	"context"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/charmbracelet/log"
	gofs "github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/dropalltables/yubifs/internal/types"
)

// Device is the block device and superblock backing the FUSE filesystem.
// Production uses *device.Manager; tests use an in-memory implementation.
type Device interface {
	Superblock() *types.Superblock
	ReadBlock(block int) ([]byte, error)
	WriteBlock(block int, data []byte) error
	WriteSuperblockToCurrent() error
}

type YubiFS struct {
	gofs.Inode
	Dev Device
	mu  sync.Mutex
	ino uint32
}

func (r *YubiFS) nextIno() uint32 {
	r.ino++
	return r.ino
}

func (r *YubiFS) sb() *types.Superblock {
	return r.Dev.Superblock()
}

var _ = (gofs.NodeGetattrer)((*YubiFS)(nil))
var _ = (gofs.NodeReaddirer)((*YubiFS)(nil))
var _ = (gofs.NodeLookuper)((*YubiFS)(nil))
var _ = (gofs.NodeCreater)((*YubiFS)(nil))
var _ = (gofs.NodeUnlinker)((*YubiFS)(nil))

func (r *YubiFS) Getattr(ctx context.Context, fh gofs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	out.Mode = 0o755 | syscall.S_IFDIR
	out.Nlink = 2
	out.Uid = uint32(os.Getuid())
	out.Gid = uint32(os.Getgid())
	return 0
}

func (r *YubiFS) Readdir(ctx context.Context) (gofs.DirStream, syscall.Errno) {
	r.mu.Lock()
	defer r.mu.Unlock()

	var entries []fuse.DirEntry
	sb := r.sb()
	for i := 0; i < types.MaxFiles; i++ {
		f := &sb.Files[i]
		if f.Flags&types.FileFlagInUse == 0 {
			continue
		}
		entries = append(entries, fuse.DirEntry{
			Name: f.FileName(),
			Ino:  uint64(f.Inode),
			Mode: syscall.S_IFREG | 0o644,
		})
	}
	return gofs.NewListDirStream(entries), 0
}

func (r *YubiFS) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*gofs.Inode, syscall.Errno) {
	r.mu.Lock()
	defer r.mu.Unlock()

	idx := r.sb().FindFile(name)
	if idx < 0 {
		return nil, syscall.ENOENT
	}

	f := &r.sb().Files[idx]
	node := &YubiFile{root: r, fileIdx: idx}

	out.Attr.Ino = uint64(f.Inode)
	out.Attr.Size = uint64(f.Size)
	out.Attr.Mode = syscall.S_IFREG | 0o644
	out.Attr.Mtime = uint64(f.Mtime)
	out.Attr.Atime = uint64(f.Mtime)
	out.Attr.Ctime = uint64(f.Mtime)
	out.Attr.Nlink = 1
	out.Attr.Blksize = types.BlockPayload
	out.AttrValid = 0
	out.EntryValid = 0

	child := r.NewInode(ctx, node, gofs.StableAttr{
		Mode: syscall.S_IFREG,
		Ino:  uint64(f.Inode),
	})
	return child, 0
}

func (r *YubiFS) Create(ctx context.Context, name string, flags uint32, mode uint32, out *fuse.EntryOut) (*gofs.Inode, gofs.FileHandle, uint32, syscall.Errno) {
	r.mu.Lock()
	defer r.mu.Unlock()

	sb := r.sb()
	if sb.FindFile(name) >= 0 {
		return nil, nil, 0, syscall.EEXIST
	}

	slot := -1
	for i := 0; i < types.MaxFiles; i++ {
		if sb.Files[i].Flags&types.FileFlagInUse == 0 {
			slot = i
			break
		}
	}
	if slot < 0 {
		return nil, nil, 0, syscall.ENOSPC
	}

	now := time.Now().Unix()
	ino := r.nextIno()

	f := &sb.Files[slot]
	f.Flags = types.FileFlagInUse
	f.SetName(name)
	f.FirstBlock = types.BlockFree
	f.Size = 0
	f.Mtime = now
	f.Inode = ino
	sb.Header.FileCount++

	log.Info("file create", "name", name)
	if err := r.Dev.WriteSuperblockToCurrent(); err != nil {
		f.Flags = 0
		sb.Header.FileCount--
		log.Error("file create failed", "name", name, "err", err)
		return nil, nil, 0, syscall.EIO
	}

	node := &YubiFile{root: r, fileIdx: slot}
	out.Attr.Ino = uint64(ino)
	out.Attr.Mode = syscall.S_IFREG | 0o644
	out.Attr.Mtime = uint64(now)
	out.Attr.Nlink = 1
	out.Attr.Blksize = types.BlockPayload
	out.AttrValid = 0
	out.EntryValid = 0

	child := r.NewInode(ctx, node, gofs.StableAttr{
		Mode: syscall.S_IFREG,
		Ino:  uint64(ino),
	})
	return child, nil, fuse.FOPEN_KEEP_CACHE, 0
}

func (r *YubiFS) Unlink(ctx context.Context, name string) syscall.Errno {
	r.mu.Lock()
	defer r.mu.Unlock()

	sb := r.sb()
	idx := sb.FindFile(name)
	if idx < 0 {
		return syscall.ENOENT
	}

	f := &sb.Files[idx]
	chain := sb.BlockChain(f.FirstBlock)
	for _, b := range chain {
		sb.FreeBlock(b)
	}
	f.Flags = 0
	f.FirstBlock = types.BlockFree
	f.Size = 0
	f.NameLen = 0
	sb.Header.FileCount--

	log.Info("file delete", "name", name, "blocks_freed", len(chain))
	if err := r.Dev.WriteSuperblockToCurrent(); err != nil {
		log.Error("file delete failed", "name", name, "err", err)
		return syscall.EIO
	}
	return 0
}

func (r *YubiFS) Statfs(ctx context.Context, out *fuse.StatfsOut) syscall.Errno {
	r.mu.Lock()
	defer r.mu.Unlock()

	sb := r.sb()
	total := uint64(sb.Header.TotalBlocks)
	var used uint64
	for i := 0; i < int(total); i++ {
		if sb.BlockUsed(i) {
			used++
		}
	}

	out.Blocks = total
	out.Bfree = total - used
	out.Bavail = total - used
	out.Bsize = types.BlockPayload
	out.NameLen = types.MaxNameLen
	out.Frsize = types.BlockPayload
	out.Files = types.MaxFiles
	out.Ffree = uint64(types.MaxFiles - sb.Header.FileCount)
	return 0
}
