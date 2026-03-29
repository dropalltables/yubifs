# yubifs

yubikey-based block filesystem

[https://github.com/dropalltables/yubifs](https://github.com/dropalltables/yubifs)

## Requirements

| Item | Notes |
|------|--------|
| YubiKey 5 | One or more; series 5 NVM limits usable slots |
| macOS FUSE | [macFUSE](https://osxfuse.github.io/) |
| Go | 1.24+ to build from source (see `go.mod`) |
| Smart card | PC/SC access for the YubiKey |

## Build

```bash
git clone https://github.com/dropalltables/yubifs.git
cd yubifs
go build -o yubifs ./cmd/yubifs
```

## Testing

```bash
go test ./...
```

Tests cover on-disk superblock layout (`internal/types`), PIV slot mapping (`internal/piv`), and the FUSE layer with an in-memory device (`internal/fs`). They do not require a YubiKey or FUSE mount.

## Commands

```
yubifs - yubikey-based block filesystem

Usage:
  yubifs format     Format YubiKey(s) for use as yubifs disks
  yubifs mount DIR  Mount yubifs at DIR
  yubifs info       Show filesystem info from inserted key
```

### `format`

Walks through disks one at a time: label each disk, insert when prompted, probes how many data slots can hold a block, then writes the superblock.

> [!WARNING]
> This replaces all PIV certificates on keys you use. FIDO2, OATH, OTP, and OpenPGP are not touched. To restore default PIV later: `ykman piv reset` (or vendor equivalent).

### `mount`

Insert any disk that belongs to the volume to load the superblock. If a read/write needs a different disk, the process prints a prompt on stderr and waits for Enter after you swap.

> [!TIP]
> Unmount with `umount DIR` (or your OS equivalent), or interrupt the process; the tool tries to sync the superblock on exit.

### `info`

Read-only status: disks, blocks, files (requires a key that already contains the filesystem).

## Behaviour

- **Storage model**: Data lives in self-signed ECC P-256 certs with a custom X.509 extension (OID `1.3.6.1.4.1.99999.1`). Slot `9a` holds the superblock; slots `9c`-`95` hold payload blocks (2,048 bytes each when NVM allows)
- **Performance**: No block cache; writes are slow (on the order of ~1s per block).

## Capacity (typical)

Per-key numbers are from format-time probing on the device. Totals scale with how many disks you chose at `format` time (the implementation allows **up to 8** keys; three is just a common example).

| Resource | Approx. |
|----------|---------|
| NVM per YubiKey 5 | ~51,200 bytes |
| Usable data slots per key | ~20 (NVM-limited) |
| Payload per slot | 2,048 bytes |
| Per key | ~40 KB |
| Example: 3 disks | ~120 KB combined |

## Architecture

- **Superblock** (`9a`): header, disk table, allocation bitmap, block chains, fixed file table (16 entries). Replicated to all disks when metadata changes
- **Data blocks** (`9c`-`95`): raw file bytes
- **Allocation**: Fills disks in label order (e.g. A, then B, then C, for as many disks as the volume has)
- **Consistency**: Sequence number and CRC32; on mount the highest valid superblock wins

## Limits

- 1-8 YubiKeys per filesystem
- No directories
- 16 files max
- 60-character file names

## License

This program is licensed under **GNU AGPL-3.0**. See the `LICENSE` file in this repository.
