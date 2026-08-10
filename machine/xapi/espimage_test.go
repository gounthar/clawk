package xapi

import (
	"bytes"
	"encoding/binary"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// sampleImage builds the image the boot VDI actually needs: an EFI-stub
// kernel at the removable-media fallback path, and an initrd beside it.
func sampleImage(t *testing.T) []byte {
	t.Helper()
	kernel := bytes.Repeat([]byte{0x4D, 0x5A}, 4096) // 8 KiB of "MZ"
	initrd := bytes.Repeat([]byte{0xAB}, 70000)      // spans many clusters
	img, err := buildESPImage([]espFile{
		{Dir: "EFI/BOOT", Name: "BOOTX64.EFI", Data: kernel},
		{Dir: "", Name: "INITRD.IMG", Data: initrd},
	})
	require.NoError(t, err)
	return img
}

// TestDumpImage writes an image to disk for inspection by tools that are not
// this package — sfdisk, file, a FAT parser. Skipped unless CLAWK_ESP_DUMP
// names a path. It asserts nothing: its whole purpose is to hand the bytes to
// an independent implementation, because a test that checks this file's
// layout arithmetic against this file's layout arithmetic cannot fail.
func TestDumpImage(t *testing.T) {
	path := os.Getenv("CLAWK_ESP_DUMP")
	if path == "" {
		t.Skip("set CLAWK_ESP_DUMP=<path> to write a sample image")
	}
	require.NoError(t, os.WriteFile(path, sampleImage(t), 0o644))
	t.Logf("wrote %s", path)
}

func TestImageGeometry(t *testing.T) {
	img := sampleImage(t)

	// Protective MBR: one 0xEE partition, and the boot signature.
	require.Equal(t, byte(0xEE), img[446+4], "protective MBR partition type")
	require.Equal(t, []byte{0x55, 0xAA}, img[510:512], "MBR signature")

	// Primary GPT header at LBA 1.
	require.Equal(t, "EFI PART", string(img[sectorSize:sectorSize+8]))

	// The backup header must sit in the last sector and point back at the
	// primary. Getting these two crossed is the classic GPT bug and it
	// only shows up when something tries to repair the disk.
	total := uint64(len(img) / sectorSize)
	backup := img[(total-1)*sectorSize:]
	require.Equal(t, "EFI PART", string(backup[0:8]))
	require.Equal(t, total-1, binary.LittleEndian.Uint64(backup[24:]), "backup myLBA")
	require.Equal(t, uint64(1), binary.LittleEndian.Uint64(backup[32:]), "backup altLBA")
	require.Equal(t, total-1, binary.LittleEndian.Uint64(img[sectorSize+32:]), "primary altLBA")
}

func TestFATIsFAT32(t *testing.T) {
	img := sampleImage(t)
	bs := img[espFirstLBA*sectorSize:]

	require.Equal(t, "FAT32   ", string(bs[82:90]))
	require.Equal(t, uint16(sectorSize), binary.LittleEndian.Uint16(bs[11:]))
	require.Equal(t, byte(sectorsPerCluster), bs[13])
	require.Equal(t, uint16(0), binary.LittleEndian.Uint16(bs[17:]), "FAT32 root entry count must be 0")
	require.Equal(t, uint32(2), binary.LittleEndian.Uint32(bs[44:]), "root cluster")
	require.Equal(t, []byte{0x55, 0xAA}, bs[510:512])

	// The cluster count is what actually decides FAT16 vs FAT32, whatever
	// the label says. Recompute it the way a driver would, from the BPB
	// alone, rather than trusting the layout that produced it.
	totalSec := binary.LittleEndian.Uint32(bs[32:])
	fatSz := binary.LittleEndian.Uint32(bs[36:])
	rsvd := uint32(binary.LittleEndian.Uint16(bs[14:]))
	numFATs := uint32(bs[16])
	dataSec := totalSec - (rsvd + fatSz*numFATs)
	clusters := dataSec / uint32(bs[13])
	require.GreaterOrEqual(t, clusters, uint32(fat32MinClusters),
		"volume must have enough clusters to be FAT32, not FAT16 wearing a FAT32 label")

	// The backup boot sector at sector 6 must match sector 0.
	require.Equal(t, bs[0:512], bs[6*sectorSize:6*sectorSize+512], "backup boot sector")
}

func TestRejectsLongNames(t *testing.T) {
	_, err := buildESPImage([]espFile{{Name: "TOOLONGNAME.EFI", Data: []byte("x")}})
	require.Error(t, err)

	_, err = buildESPImage([]espFile{{Name: "grubx64.efi", Data: []byte("x")}})
	require.Error(t, err, "lower case must be rejected rather than silently upcased")
}

// readBack parses the volume the way a FAT driver would: from the BPB, then
// through the FAT, with no reference to fatLayout. That independence is the
// point. It was checked against a separate Python parser and against `file`
// and `sfdisk` when written; keeping it here is what stops a later change to
// the writer from silently agreeing with itself.
func readBack(t *testing.T, img []byte) map[string][]byte {
	t.Helper()
	bs := img[espFirstLBA*sectorSize:]
	bps := uint32(binary.LittleEndian.Uint16(bs[11:]))
	spc := uint32(bs[13])
	rsvd := uint32(binary.LittleEndian.Uint16(bs[14:]))
	nfat := uint32(bs[16])
	fatSz := binary.LittleEndian.Uint32(bs[36:])
	root := binary.LittleEndian.Uint32(bs[44:])

	dataStart := rsvd + nfat*fatSz
	fatOff := rsvd * bps
	entry := func(n uint32) uint32 {
		return binary.LittleEndian.Uint32(bs[fatOff+n*4:]) & 0x0FFFFFFF
	}
	clusOff := func(c uint32) uint32 { return (dataStart + (c-2)*spc) * bps }
	chain := func(start uint32) []uint32 {
		var out []uint32
		for c := start; c >= 2 && c < 0x0FFFFFF8; c = entry(c) {
			out = append(out, c)
			require.Less(t, len(out), 200000, "FAT chain loop")
		}
		return out
	}

	// Both FAT copies must agree; a driver may read either.
	f1 := bs[fatOff : fatOff+fatSz*bps]
	f2 := bs[fatOff+fatSz*bps : fatOff+2*fatSz*bps]
	require.Equal(t, f1, f2, "the two FAT copies must be identical")

	out := map[string][]byte{}
	var walk func(cluster uint32, prefix string)
	walk = func(cluster uint32, prefix string) {
		for _, c := range chain(cluster) {
			base := clusOff(c)
			for i := uint32(0); i < spc*bps; i += 32 {
				e := bs[base+i : base+i+32]
				if e[0] == 0x00 {
					return
				}
				if e[0] == 0xE5 {
					continue
				}
				require.NotEqual(t, byte(0x0F), e[11]&0x0F, "unexpected long-name entry")
				name := strings.TrimRight(string(e[0:8]), " ")
				ext := strings.TrimRight(string(e[8:11]), " ")
				full := name
				if ext != "" {
					full = name + "." + ext
				}
				first := uint32(binary.LittleEndian.Uint16(e[20:]))<<16 |
					uint32(binary.LittleEndian.Uint16(e[26:]))
				size := binary.LittleEndian.Uint32(e[28:])
				if e[11]&attrDirectory != 0 {
					if full == "." || full == ".." {
						continue
					}
					walk(first, prefix+"/"+full)
					continue
				}
				var data []byte
				for _, fc := range chain(first) {
					data = append(data, bs[clusOff(fc):clusOff(fc)+spc*bps]...)
				}
				out[prefix+"/"+full] = data[:size]
			}
		}
	}
	walk(root, "")
	return out
}

func TestFilesRoundTrip(t *testing.T) {
	kernel := bytes.Repeat([]byte{0x4D, 0x5A}, 4096)
	initrd := bytes.Repeat([]byte{0xAB}, 70000)

	got := readBack(t, sampleImage(t))

	// The initrd spans many clusters, so this also proves the chain is
	// walked rather than the first cluster being read and padded.
	require.Equal(t, kernel, got["/EFI/BOOT/BOOTX64.EFI"])
	require.Equal(t, initrd, got["/INITRD.IMG"])
	require.Len(t, got, 2)
}

func TestReproducible(t *testing.T) {
	// The boot VDI is cached per (kernel, cmdline). If two builds of the
	// same inputs differ, every build looks like a new image to the cache.
	require.Equal(t, sampleImage(t), sampleImage(t))
}
