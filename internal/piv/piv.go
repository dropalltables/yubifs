package piv

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"fmt"
	"math/big"
	"strings"
	"time"

	gopiv "github.com/go-piv/piv-go/v2/piv"

	"github.com/dropalltables/yubifs/internal/types"
)

var (
	dataOID        = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 99999, 1}
	defaultMgmtKey = []byte{
		0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
		0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
		0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
	}
)

const MetadataSlot = 0x9a

var DataSlotKeys = []byte{
	0x9c, 0x9d, 0x9e,
	0x82, 0x83, 0x84, 0x85, 0x86, 0x87, 0x88, 0x89,
	0x8a, 0x8b, 0x8c, 0x8d, 0x8e, 0x8f,
	0x90, 0x91, 0x92, 0x93, 0x94, 0x95,
}

func SlotForKey(key byte) gopiv.Slot {
	switch key {
	case 0x9a:
		return gopiv.SlotAuthentication
	case 0x9c:
		return gopiv.SlotSignature
	case 0x9d:
		return gopiv.SlotKeyManagement
	case 0x9e:
		return gopiv.SlotCardAuthentication
	default:
		slot, ok := gopiv.RetiredKeyManagementSlot(uint32(key))
		if !ok {
			return gopiv.Slot{Key: uint32(key)}
		}
		return slot
	}
}

func SlotForBlock(sb *types.Superblock, block int) (gopiv.Slot, error) {
	di := sb.DiskForBlock(block)
	if di < 0 {
		return gopiv.Slot{}, fmt.Errorf("block %d not on any disk", block)
	}
	local := block - int(sb.Disks[di].First)
	if local < 0 || local >= len(DataSlotKeys) {
		return gopiv.Slot{}, fmt.Errorf("block %d local index %d out of range", block, local)
	}
	return SlotForKey(DataSlotKeys[local]), nil
}

func ListYubiKeys() ([]string, error) {
	cards, err := gopiv.Cards()
	if err != nil {
		return nil, err
	}
	var result []string
	for _, c := range cards {
		if strings.Contains(strings.ToLower(c), "yubikey") {
			result = append(result, c)
		}
	}
	return result, nil
}

func OpenFirst() (*gopiv.YubiKey, error) {
	cards, err := ListYubiKeys()
	if err != nil {
		return nil, err
	}
	if len(cards) == 0 {
		return nil, fmt.Errorf("no YubiKey found")
	}
	return gopiv.Open(cards[0])
}

func OpenBySerial(serial uint32) (*gopiv.YubiKey, error) {
	cards, err := ListYubiKeys()
	if err != nil {
		return nil, err
	}
	for _, c := range cards {
		yk, err := gopiv.Open(c)
		if err != nil {
			continue
		}
		s, err := yk.Serial()
		if err != nil {
			yk.Close()
			continue
		}
		if s == serial {
			return yk, nil
		}
		yk.Close()
	}
	return nil, fmt.Errorf("YubiKey with serial %d not found", serial)
}

func Serial(yk *gopiv.YubiKey) (uint32, error) {
	return yk.Serial()
}

func makeCert(payload []byte) (*x509.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}

	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "yubifs-block"},
		NotBefore:    now,
		NotAfter:     now.Add(100 * 365 * 24 * time.Hour),
		ExtraExtensions: []pkix.Extension{
			{
				Id:    dataOID,
				Value: payload,
			},
		},
	}

	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	return x509.ParseCertificate(der)
}

func WriteBlock(yk *gopiv.YubiKey, slot gopiv.Slot, data []byte) error {
	cert, err := makeCert(data)
	if err != nil {
		return fmt.Errorf("make cert: %w", err)
	}
	return yk.SetCertificate(defaultMgmtKey, slot, cert)
}

func ReadBlock(yk *gopiv.YubiKey, slot gopiv.Slot) ([]byte, error) {
	cert, err := yk.Certificate(slot)
	if err != nil {
		return nil, err
	}
	for _, ext := range cert.Extensions {
		if ext.Id.Equal(dataOID) {
			return ext.Value, nil
		}
	}
	return nil, fmt.Errorf("no yubifs extension in slot")
}

func ResetPIV(yk *gopiv.YubiKey) error {
	for i := 0; i < 5; i++ {
		yk.VerifyPIN("000000")
	}
	for i := 0; i < 5; i++ {
		yk.Unblock("00000000", "000000")
	}
	return yk.Reset()
}

func WriteSuperblock(yk *gopiv.YubiKey, sb *types.Superblock) error {
	data, err := sb.Marshal()
	if err != nil {
		return err
	}
	return WriteBlock(yk, SlotForKey(MetadataSlot), data)
}

func ReadSuperblock(yk *gopiv.YubiKey) (*types.Superblock, error) {
	data, err := ReadBlock(yk, SlotForKey(MetadataSlot))
	if err != nil {
		return nil, err
	}
	return types.UnmarshalSuperblock(data)
}
