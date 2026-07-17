package sandbox

import "github.com/clawkwork/clawk/internal/config"

// DefaultDiskSizeGiB is the virtual ceiling a VM root disk is grown to when
// the sandbox doesn't request its own size (clawk.mod `vm ( disk <size> )`).
// The image is sparse ext4: the guest's unwritten tail is a hole, so the
// ceiling bounds how far a box may grow rather than charging its full size.
//
// The ceiling is deliberately generous: dependency caches (Go modules,
// Cargo registry) live on the per-VM rootfs, not on a shared mount, so a
// tight ceiling would fill mid-build. Physical usage is still bounded by
// the host's free disk; this only stops the guest hitting an artificial
// wall well before that.
//
// The ceiling is not free, though: the inode table is materialized at build
// time at one 256-byte inode per 16 KiB of disk, so a padded rootfs costs
// about 1/64 of its ceiling in real host bytes before the guest writes
// anything — ~512 MiB at 32 GiB, ~1 GiB at 64 GiB. That is the price of not
// running out of inodes on a file-heavy build (see compactext4's
// inodesForBlocks); it's why the default is 32 and not 256. Per-VM clones
// reflink off the cached image, so the charge lands once per distinct
// (image, size) cache entry rather than once per sandbox.
//
// OCI rootfs disks are padded sparse ext4 images sized to this ceiling (or
// the per-sandbox override) at build time.
const DefaultDiskSizeGiB = 32

// RootDiskSizeMiB is the ext4 root-disk floor for sb, in MiB: the
// per-sandbox `vm ( disk <size> )` override when set, otherwise
// DefaultDiskSizeGiB. It is a sparse floor (see machine.OCIImage.SizeMiB) —
// larger image content wins and the unused remainder stays a hole. Raising
// it is cheap but not free: see DefaultDiskSizeGiB on the inode-table cost.
func RootDiskSizeMiB(sb *config.Sandbox) int {
	if sb != nil && sb.DiskMiB > 0 {
		return int(sb.DiskMiB)
	}
	return DefaultDiskSizeGiB << 10
}
