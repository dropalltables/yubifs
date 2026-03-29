package piv

import (
	"testing"

	gopiv "github.com/go-piv/piv-go/v2/piv"

	"github.com/dropalltables/yubifs/internal/types"
)

func TestSlotForKey(t *testing.T) {
	cases := []struct {
		key  byte
		want uint32
	}{
		{0x9a, gopiv.SlotAuthentication.Key},
		{0x9c, gopiv.SlotSignature.Key},
		{0x9d, gopiv.SlotKeyManagement.Key},
		{0x9e, gopiv.SlotCardAuthentication.Key},
	}
	for _, tc := range cases {
		s := SlotForKey(tc.key)
		if s.Key != tc.want {
			t.Fatalf("key %#x: got Key=%d want %d", tc.key, s.Key, tc.want)
		}
	}
}

func TestSlotForBlock(t *testing.T) {
	sb := &types.Superblock{}
	copy(sb.Header.Magic[:], types.SuperblockMagic)
	sb.Header.DiskCount = 1
	sb.Header.TotalBlocks = 8
	sb.Disks[0].First = 0
	sb.Disks[0].Capacity = 8

	slot, err := SlotForBlock(sb, 0)
	if err != nil {
		t.Fatal(err)
	}
	if slot.Key != gopiv.SlotSignature.Key {
		t.Fatalf("block 0 slot Key=%d", slot.Key)
	}

	_, err = SlotForBlock(sb, 99)
	if err == nil {
		t.Fatal("expected error for unmapped block")
	}
}
