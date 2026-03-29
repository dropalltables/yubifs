package device

import (
	"bufio"
	"fmt"
	"os"
	"sync"

	"github.com/charmbracelet/log"
	gopiv "github.com/go-piv/piv-go/v2/piv"

	"github.com/dropalltables/yubifs/internal/piv"
	"github.com/dropalltables/yubifs/internal/types"
)

type Manager struct {
	mu          sync.Mutex
	Sb          *types.Superblock
	currentDisk int
	currentYK   *gopiv.YubiKey
	tty         *os.File
}

func NewManager(sb *types.Superblock) *Manager {
	return &Manager{
		Sb:          sb,
		currentDisk: -1,
	}
}

func (dm *Manager) Superblock() *types.Superblock {
	return dm.Sb
}

func (dm *Manager) openTTY() *os.File {
	if dm.tty != nil {
		return dm.tty
	}
	f, err := os.Open("/dev/tty")
	if err != nil {
		return os.Stdin
	}
	dm.tty = f
	return f
}

func (dm *Manager) promptEnter(msg string) {
	fmt.Fprintf(os.Stderr, "%s", msg)
	reader := bufio.NewReader(dm.openTTY())
	reader.ReadString('\n')
}

func (dm *Manager) Close() {
	dm.mu.Lock()
	defer dm.mu.Unlock()
	if dm.currentYK != nil {
		dm.currentYK.Close()
		dm.currentYK = nil
	}
	if dm.tty != nil {
		dm.tty.Close()
		dm.tty = nil
	}
}

func (dm *Manager) diskLabel(i int) string {
	return types.DiskLabel(&dm.Sb.Disks[i])
}

func (dm *Manager) ensureDisk(diskIdx int) error {
	if dm.currentDisk == diskIdx && dm.currentYK != nil {
		serial, err := piv.Serial(dm.currentYK)
		if err == nil && serial == dm.Sb.Disks[diskIdx].Serial {
			return nil
		}
		dm.currentYK.Close()
		dm.currentYK = nil
		dm.currentDisk = -1
	}

	if dm.currentYK != nil {
		dm.currentYK.Close()
		dm.currentYK = nil
		dm.currentDisk = -1
	}

	d := &dm.Sb.Disks[diskIdx]
	label := dm.diskLabel(diskIdx)

	yk, err := piv.OpenBySerial(d.Serial)
	if err == nil {
		dm.currentYK = yk
		dm.currentDisk = diskIdx
		log.Info("disk connected", "disk", label, "serial", d.Serial)
		return nil
	}

	for {
		dm.promptEnter(fmt.Sprintf("\nInsert disk %s (serial %d) and press Enter...", label, d.Serial))

		yk, err = piv.OpenBySerial(d.Serial)
		if err != nil {
			log.Warn("disk not found, try again", "disk", label)
			continue
		}
		dm.currentYK = yk
		dm.currentDisk = diskIdx
		log.Info("disk connected", "disk", label, "serial", d.Serial)
		return nil
	}
}

func (dm *Manager) ReadBlock(block int) ([]byte, error) {
	dm.mu.Lock()
	defer dm.mu.Unlock()

	di := dm.Sb.DiskForBlock(block)
	if di < 0 {
		return nil, fmt.Errorf("block %d not mapped to any disk", block)
	}

	if err := dm.ensureDisk(di); err != nil {
		return nil, err
	}

	slot, err := piv.SlotForBlock(dm.Sb, block)
	if err != nil {
		return nil, err
	}

	log.Debug("block read", "block", block, "disk", dm.diskLabel(di))
	data, err := piv.ReadBlock(dm.currentYK, slot)
	if err != nil {
		log.Warn("block read failed, retrying", "block", block, "err", err)
		dm.currentYK.Close()
		dm.currentYK = nil
		dm.currentDisk = -1
		if err := dm.ensureDisk(di); err != nil {
			return nil, err
		}
		return piv.ReadBlock(dm.currentYK, slot)
	}
	return data, nil
}

func (dm *Manager) WriteBlock(block int, data []byte) error {
	dm.mu.Lock()
	defer dm.mu.Unlock()

	di := dm.Sb.DiskForBlock(block)
	if di < 0 {
		return fmt.Errorf("block %d not mapped to any disk", block)
	}

	if err := dm.ensureDisk(di); err != nil {
		return err
	}

	slot, err := piv.SlotForBlock(dm.Sb, block)
	if err != nil {
		return err
	}

	padded := make([]byte, types.BlockPayload)
	copy(padded, data)
	log.Debug("block write", "block", block, "disk", dm.diskLabel(di))
	if err := piv.WriteBlock(dm.currentYK, slot, padded); err != nil {
		log.Warn("block write failed, retrying", "block", block, "err", err)
		dm.currentYK.Close()
		dm.currentYK = nil
		dm.currentDisk = -1
		if err := dm.ensureDisk(di); err != nil {
			return err
		}
		return piv.WriteBlock(dm.currentYK, slot, padded)
	}
	return nil
}

func (dm *Manager) WriteSuperblockToCurrent() error {
	dm.mu.Lock()
	defer dm.mu.Unlock()

	if dm.currentYK != nil {
		if _, err := piv.Serial(dm.currentYK); err != nil {
			dm.currentYK.Close()
			dm.currentYK = nil
			dm.currentDisk = -1
		}
	}

	if dm.currentYK == nil {
		for i := 0; i < int(dm.Sb.Header.DiskCount); i++ {
			yk, err := piv.OpenBySerial(dm.Sb.Disks[i].Serial)
			if err == nil {
				dm.currentYK = yk
				dm.currentDisk = i
				log.Info("disk connected", "disk", dm.diskLabel(i), "serial", dm.Sb.Disks[i].Serial)
				break
			}
		}
	}

	if dm.currentYK == nil {
		if err := dm.ensureDisk(0); err != nil {
			return err
		}
	}

	dm.Sb.Header.Sequence++
	log.Debug("superblock write", "disk", dm.diskLabel(dm.currentDisk))
	return piv.WriteSuperblock(dm.currentYK, dm.Sb)
}

func (dm *Manager) WriteSuperblockToAll() error {
	dm.mu.Lock()
	defer dm.mu.Unlock()

	dm.Sb.Header.Sequence++

	for i := 0; i < int(dm.Sb.Header.DiskCount); i++ {
		if err := dm.ensureDisk(i); err != nil {
			return fmt.Errorf("disk %s: %w", dm.diskLabel(i), err)
		}
		log.Info("superblock write", "disk", dm.diskLabel(i))
		if err := piv.WriteSuperblock(dm.currentYK, dm.Sb); err != nil {
			return fmt.Errorf("disk %s: superblock write failed: %w", dm.diskLabel(i), err)
		}
	}
	return nil
}
