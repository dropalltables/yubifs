package main

import (
	"bufio"
	"crypto/rand"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/charmbracelet/log"
	gofs "github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"yubifs/internal/device"
	"yubifs/internal/fs"
	"yubifs/internal/piv"
	"yubifs/internal/types"
)

func usage() {
	fmt.Fprintf(os.Stderr, `yubifs - yubikey-based block filesystem

Usage:
  yubifs format     Format YubiKey(s) for use as yubifs disks
  yubifs mount DIR  Mount yubifs at DIR
  yubifs info       Show filesystem info from inserted key
`)
	os.Exit(1)
}

func prompt(msg string) {
	fmt.Fprintf(os.Stderr, "%s", msg)
	bufio.NewReader(os.Stdin).ReadString('\n')
}

func promptLine(msg string) string {
	fmt.Fprintf(os.Stderr, "%s", msg)
	s, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	return strings.TrimSpace(s)
}

func cmdFormat() {
	log.Warn("format will erase all PIV certificate data on your YubiKey(s)")
	log.Info("FIDO2, OATH, OTP, and OpenPGP are NOT affected")

	countStr := promptLine("\nHow many disks? ")
	var diskCount int
	fmt.Sscanf(countStr, "%d", &diskCount)
	if diskCount < 1 || diskCount > types.MaxDisks {
		log.Fatal("invalid disk count", "min", 1, "max", types.MaxDisks)
	}

	var fsUUID [16]byte
	rand.Read(fsUUID[:])

	type probed struct {
		label    byte
		serial   uint32
		capacity int
		uuid     [16]byte
	}
	disks := make([]probed, 0, diskCount)

	for i := 0; i < diskCount; i++ {
		label := promptLine(fmt.Sprintf("Label for disk %d (e.g. A, B, C): ", i+1))
		if len(label) == 0 {
			label = string(rune('A' + i))
		}

		prompt(fmt.Sprintf("Insert disk %s and press Enter...", label))

		yk, err := piv.OpenFirst()
		if err != nil {
			log.Fatal("yubikey open failed", "err", err)
		}

		serial, err := piv.Serial(yk)
		if err != nil {
			yk.Close()
			log.Fatal("serial read failed", "err", err)
		}
		log.Info("yubikey found", "serial", serial)

		log.Info("piv reset", "phase", "pre-probe")
		if err := piv.ResetPIV(yk); err != nil {
			yk.Close()
			log.Fatal("piv reset failed", "err", err,
				"hint", fmt.Sprintf("ykman --device %d piv reset", serial))
		}
		yk.Close()

		yk, err = piv.OpenFirst()
		if err != nil {
			log.Fatal("yubikey reopen failed", "err", err)
		}

		log.Info("probing capacity")
		capacity := 0
		testPayload := make([]byte, types.BlockPayload)
		for j := 0; j < len(piv.DataSlotKeys); j++ {
			slot := piv.SlotForKey(piv.DataSlotKeys[j])
			err := piv.WriteBlock(yk, slot, testPayload)
			if err != nil {
				log.Debug("slot probe", "slot", fmt.Sprintf("%02x", piv.DataSlotKeys[j]), "result", "no space")
				break
			}
			capacity++
			log.Debug("slot probe", "slot", fmt.Sprintf("%02x", piv.DataSlotKeys[j]), "result", "ok")
		}

		log.Info("piv reset", "phase", "post-probe")
		if err := piv.ResetPIV(yk); err != nil {
			yk.Close()
			log.Fatal("piv reset failed", "err", err)
		}
		yk.Close()

		if capacity == 0 {
			log.Fatal("no usable slots on this key")
		}
		// Reserve 1 block as NVM safety margin (superblock shares NVM space)
		capacity--

		var diskUUID [16]byte
		rand.Read(diskUUID[:])

		disks = append(disks, probed{
			label:    label[0],
			serial:   serial,
			capacity: capacity,
			uuid:     diskUUID,
		})

		log.Info("disk probed", "disk", label, "serial", serial,
			"blocks", capacity, "bytes", capacity*types.BlockPayload)
	}

	var totalBlocks int
	for _, d := range disks {
		totalBlocks += d.capacity
	}

	sb := &types.Superblock{}
	copy(sb.Header.Magic[:], types.SuperblockMagic)
	sb.Header.Version = types.SuperblockVer
	sb.Header.FSUUID = fsUUID
	sb.Header.Sequence = 1
	sb.Header.BlockPayload = types.BlockPayload
	sb.Header.DiskCount = uint8(diskCount)
	sb.Header.TotalBlocks = uint8(totalBlocks)
	sb.Header.FileCount = 0

	blockStart := 0
	for i, d := range disks {
		sb.Disks[i] = types.DiskDesc{
			Label:    d.label,
			Capacity: uint8(d.capacity),
			First:    uint8(blockStart),
			Serial:   d.serial,
			UUID:     d.uuid,
		}
		blockStart += d.capacity
	}

	for i := range sb.FAT {
		sb.FAT[i] = types.BlockFree
	}

	log.Info("writing superblock to all disks")
	for i, d := range disks {
		label := string([]byte{d.label})
		prompt(fmt.Sprintf("Insert disk %s (serial %d) and press Enter...", label, d.serial))

		yk, err := piv.OpenBySerial(d.serial)
		if err != nil {
			log.Fatal("disk open failed", "disk", label, "err", err)
		}
		if err := piv.WriteSuperblock(yk, sb); err != nil {
			yk.Close()
			log.Fatal("superblock write failed", "disk", label, "err", err)
		}
		yk.Close()
		log.Info("superblock written", "disk", label,
			"progress", fmt.Sprintf("%d/%d", i+1, diskCount))
	}

	log.Info("format complete", "disks", diskCount, "blocks", totalBlocks,
		"bytes", totalBlocks*types.BlockPayload)
}

func cmdInfo() {
	log.Info("reading superblock")

	yk, err := piv.OpenFirst()
	if err != nil {
		log.Fatal("yubikey open failed", "err", err)
	}
	defer yk.Close()

	sb, err := piv.ReadSuperblock(yk)
	if err != nil {
		log.Fatal("superblock read failed (is this key formatted?)", "err", err)
	}

	totalBlocks := int(sb.Header.TotalBlocks)
	usedBlocks := 0
	for i := 0; i < totalBlocks; i++ {
		if sb.BlockUsed(i) {
			usedBlocks++
		}
	}

	log.Info("filesystem", "version", sb.Header.Version,
		"sequence", sb.Header.Sequence, "disks", sb.Header.DiskCount)
	log.Info("blocks", "used", usedBlocks, "total", totalBlocks,
		"free", totalBlocks-usedBlocks)
	log.Info("capacity", "total_bytes", totalBlocks*types.BlockPayload,
		"used_bytes", usedBlocks*types.BlockPayload)
	log.Info("files", "count", sb.Header.FileCount, "max", types.MaxFiles)

	for i := 0; i < int(sb.Header.DiskCount); i++ {
		d := &sb.Disks[i]
		label := types.DiskLabel(d)
		diskUsed := 0
		for j := int(d.First); j < int(d.First)+int(d.Capacity); j++ {
			if sb.BlockUsed(j) {
				diskUsed++
			}
		}
		log.Info("disk", "disk", label, "serial", d.Serial,
			"used", diskUsed, "capacity", d.Capacity)
	}

	for i := 0; i < types.MaxFiles; i++ {
		f := &sb.Files[i]
		if f.Flags&types.FileFlagInUse == 0 {
			continue
		}
		blocks := len(sb.BlockChain(f.FirstBlock))
		t := time.Unix(f.Mtime, 0)
		log.Info("file", "name", f.FileName(), "size", f.Size,
			"blocks", blocks, "modified", t.Format("2006-01-02 15:04"))
	}
}

func cmdMount(mountpoint string) {
	log.Info("waiting for disk")

	var sb *types.Superblock
	var bootDisk string
	for {
		yk, err := piv.OpenFirst()
		if err != nil {
			prompt("Insert any yubifs disk and press Enter...")
			continue
		}
		serial, _ := piv.Serial(yk)
		sb, err = piv.ReadSuperblock(yk)
		yk.Close()
		if err != nil {
			log.Warn("superblock read failed", "serial", serial, "err", err)
			prompt("Insert a formatted yubifs disk and press Enter...")
			continue
		}
		for i := 0; i < int(sb.Header.DiskCount); i++ {
			if sb.Disks[i].Serial == serial {
				bootDisk = types.DiskLabel(&sb.Disks[i])
				break
			}
		}
		log.Info("superblock loaded", "disk", bootDisk, "serial", serial)
		break
	}

	log.Info("filesystem", "disks", sb.Header.DiskCount,
		"blocks", sb.Header.TotalBlocks, "files", sb.Header.FileCount)

	dev := device.NewManager(sb)
	root := &fs.YubiFS{Dev: dev}

	if err := os.MkdirAll(mountpoint, 0o755); err != nil {
		log.Fatal("mountpoint create failed", "err", err)
	}

	server, err := gofs.Mount(mountpoint, root, &gofs.Options{
		MountOptions: fuse.MountOptions{
			FsName:         "yubifs",
			Name:           "yubifs",
			DisableXAttrs:  true,
			Debug:          false,
			AllowOther:     false,
			SingleThreaded: true,
			Options:        []string{"nobrowse"},
		},
		AttrTimeout:  nil,
		EntryTimeout: nil,
	})
	if err != nil {
		dev.Close()
		log.Fatal("mount failed", "err", err,
			"hint", "is macFUSE or FUSE-T installed?")
	}

	log.Info("mounted", "path", mountpoint)
	log.Info("to unmount", "cmd", fmt.Sprintf("umount %s", mountpoint))

	sigch := make(chan os.Signal, 1)
	signal.Notify(sigch, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigch
		log.Info("signal received, unmounting")
		server.Unmount()
	}()

	server.Wait()

	log.Info("syncing superblock to all disks")
	if err := dev.WriteSuperblockToAll(); err != nil {
		log.Error("superblock sync failed", "err", err)
	}

	dev.Close()
	log.Info("unmounted")
}

func main() {
	log.SetLevel(log.DebugLevel)

	if len(os.Args) < 2 {
		usage()
	}

	switch os.Args[1] {
	case "format":
		cmdFormat()
	case "info":
		cmdInfo()
	case "mount":
		if len(os.Args) < 3 {
			log.Fatal("missing mountpoint", "usage", "yubifs mount <mountpoint>")
		}
		cmdMount(os.Args[2])
	default:
		usage()
	}
}
