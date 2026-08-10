package xapi

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"strings"
)

// This file builds the raw bytes of the boot VDI: a GPT-partitioned disk
// holding one FAT32 EFI System Partition. It knows nothing about XAPI, so it
// is testable without a pool — feed it files, get an image, mount it or read
// it back byte by byte.
//
// The scope is deliberately narrow, because a general FAT32 implementation is
// where this would stop being reviewable. The ESP is written once, in one
// pass, from a known set of files, and never modified afterwards. That removes
// deletion, fragmentation, free-space search and in-place update — the parts
// of FAT that carry the interesting bugs. What is left is layout arithmetic.
//
// Two consequences of writing once, both load-bearing:
//
//   - Every file is allocated a contiguous run of clusters, so a chain is a
//     range and never a linked walk over scattered entries.
//   - Long filenames are not supported. Callers must use 8.3 names. This is
//     not a limitation in practice: the paths the boot VDI needs are
//     \EFI\BOOT\BOOTX64.EFI and \INITRD.IMG, which are already 8.3. VFAT long
//     name entries are the single largest chunk of a full FAT writer and they
//     would earn nothing here.
//
// Timestamps are fixed rather than taken from the clock. The boot VDI is
// cached in the SR per (kernel, cmdline), so identical inputs must produce
// identical bytes — otherwise every build looks like a new image to the cache
// and to any digest keyed on content.

const (
	sectorSize = 512

	// espFirstLBA aligns the partition to 1 MiB, which is what every
	// partitioning tool does and what firmware and SRs expect.
	espFirstLBA = 2048

	// gptEntryCount and gptEntrySize are fixed by the UEFI spec's minimum:
	// 128 entries of 128 bytes, so the table occupies 32 sectors.
	gptEntryCount = 128
	gptEntrySize  = 128
	gptTableLBAs  = gptEntryCount * gptEntrySize / sectorSize

	// fatReservedSectors covers the boot sector, the FSInfo sector and the
	// backup boot sector at sector 6, with room to spare. 32 is the
	// conventional value for FAT32 and keeps the FATs 4 KiB-aligned.
	fatReservedSectors = 32
	fatCount           = 2

	// fat32MinClusters is the threshold that makes a volume FAT32 rather
	// than FAT16. A volume with fewer clusters than this is FAT16 by
	// definition regardless of what the BPB claims, and firmware that
	// checks will refuse it. With one sector per cluster this puts the
	// floor on the partition at roughly 33 MiB.
	fat32MinClusters = 65525

	// sectorsPerCluster is 1 so the minimum FAT32 volume stays near 33 MiB.
	// A larger cluster would raise that floor — 8 sectors would force a
	// 256 MiB partition to hold an 8 MiB kernel — and the boot VDI is
	// stored in the SR, so the floor is what matters, not the slack.
	sectorsPerCluster = 1
	clusterSize       = sectorsPerCluster * sectorSize

	// fatEOC marks the last cluster of a chain. The top four bits of a
	// FAT32 entry are reserved and must be preserved on read; we only ever
	// write, so they stay zero.
	fatEOC  = 0x0FFFFFFF
	fatFree = 0x00000000
)

// espFile is one file to place in the image. Name is an 8.3 name; Dir is the
// directory that holds it, "" for the root.
type espFile struct {
	Dir  string // "" or "EFI/BOOT"
	Name string // "BOOTX64.EFI"
	Data []byte
}

// buildESPImage returns a complete raw disk image: protective MBR, primary
// GPT, one FAT32 ESP holding files, and the backup GPT.
//
// The image is assembled in memory. The largest thing it holds is the initrd,
// and the caller is about to stream the result to the pool's import endpoint
// anyway, so a streaming builder would buy nothing until the initrd grows
// past what is comfortable to hold twice.
func buildESPImage(files []espFile) ([]byte, error) {
	if len(files) == 0 {
		return nil, errors.New("xapi: boot image needs at least one file")
	}
	for _, f := range files {
		if err := validate83(f.Name); err != nil {
			return nil, fmt.Errorf("xapi: file %q: %w", f.Name, err)
		}
		for _, part := range splitDir(f.Dir) {
			if err := validate83(part); err != nil {
				return nil, fmt.Errorf("xapi: directory %q: %w", f.Dir, err)
			}
		}
	}

	fat, err := layoutFAT(files)
	if err != nil {
		return nil, err
	}

	partSectors := fat.totalSectors
	// Primary GPT is LBA 0..33 (MBR + header + table); the backup mirrors
	// it at the end of the disk.
	totalSectors := espFirstLBA + partSectors + gptTableLBAs + 1
	img := make([]byte, totalSectors*sectorSize)

	fat.writeInto(img[espFirstLBA*sectorSize:])
	writeGPT(img, totalSectors, espFirstLBA, espFirstLBA+partSectors-1)
	return img, nil
}

// fatLayout is the computed geometry of the FAT32 volume plus the placement
// decision for every file and directory. Everything is decided before a byte
// is written, because the boot sector has to state the FAT size and the FAT
// has to state chains that depend on where the data lands.
type fatLayout struct {
	totalSectors uint32
	fatSectors   uint32
	dataStart    uint32 // sector offset of cluster 2, relative to partition
	clusterCount uint32

	dirs  []fatDir
	files []fatFileAt
}

type fatDir struct {
	path    string // "EFI/BOOT", "" for root
	cluster uint32
	parent  uint32 // cluster of parent; 0 means root, per FAT's "." / ".." rule
	entries []dirEntry
}

type fatFileAt struct {
	espFile
	cluster uint32
	// clusters is the count occupied; the run is contiguous from cluster.
	clusters uint32
}

// layoutFAT decides the size of the volume and where everything sits in it.
//
// The ordering constraint that shapes this function: the number of FAT
// sectors depends on the cluster count, the cluster count depends on how many
// sectors are left after the FATs, and both depend on the total size. Rather
// than solve that simultaneously, it computes the data the volume must hold,
// grows the volume to the FAT32 floor if that is larger, then sizes the FAT
// for the resulting cluster count and re-checks. One iteration converges
// because growing the FAT only ever shrinks the data region by a few sectors.
func layoutFAT(files []espFile) (*fatLayout, error) {
	// Collect directories, parents first, so a child's parent cluster is
	// always already assigned when the child is placed.
	dirPaths := collectDirs(files)

	// Cluster 2 is the root directory. Directories then files follow.
	next := uint32(2)
	dirs := make([]fatDir, 0, len(dirPaths)+1)
	byPath := map[string]uint32{"": 2}
	dirs = append(dirs, fatDir{path: "", cluster: 2, parent: 0})
	next++

	for _, p := range dirPaths {
		// A directory here is one cluster. At 512 bytes that is 16
		// entries, and the ESP holds a handful of names in total; a
		// directory that outgrows one cluster is rejected below rather
		// than silently truncated.
		dirs = append(dirs, fatDir{path: p, cluster: next, parent: byPath[parentOf(p)]})
		byPath[p] = next
		next++
	}

	placed := make([]fatFileAt, 0, len(files))
	for _, f := range files {
		n := uint32((len(f.Data) + clusterSize - 1) / clusterSize)
		if n == 0 {
			// A zero-length file owns no clusters; FAT records first
			// cluster 0 for it.
			placed = append(placed, fatFileAt{espFile: f, cluster: 0, clusters: 0})
			continue
		}
		placed = append(placed, fatFileAt{espFile: f, cluster: next, clusters: n})
		next += n
	}

	// next is now one past the last used cluster; clusters are numbered
	// from 2, so this is the count of clusters the data actually needs.
	usedClusters := next - 2
	clusterCount := usedClusters
	if clusterCount < fat32MinClusters {
		clusterCount = fat32MinClusters
	}

	// Size the FAT for that many clusters, remembering the two reserved
	// entries at the head of the table.
	fatBytes := (clusterCount + 2) * 4
	fatSectors := (fatBytes + sectorSize - 1) / sectorSize

	dataStart := uint32(fatReservedSectors) + fatSectors*fatCount
	totalSectors := dataStart + clusterCount*sectorsPerCluster

	// Re-check: the volume must still hold at least fat32MinClusters after
	// the FATs took their share. Growing the volume by the FAT's own size
	// settles it without a second pass, because the FAT grows by four
	// bytes per cluster and we are adding whole sectors.
	if clusterCount < fat32MinClusters {
		return nil, errors.New("xapi: FAT32 cluster count underflow")
	}

	l := &fatLayout{
		totalSectors: totalSectors,
		fatSectors:   fatSectors,
		dataStart:    dataStart,
		clusterCount: clusterCount,
		dirs:         dirs,
		files:        placed,
	}

	// Build directory entries now that every cluster is assigned.
	for i := range l.dirs {
		d := &l.dirs[i]
		if d.path != "" {
			d.entries = append(d.entries,
				dotEntry(".", d.cluster),
				dotEntry("..", d.parent),
			)
		}
		for _, sub := range l.dirs {
			if sub.path != "" && parentOf(sub.path) == d.path {
				d.entries = append(d.entries, dirEntry{
					name:    base(sub.path),
					attr:    attrDirectory,
					cluster: sub.cluster,
				})
			}
		}
		for _, f := range l.files {
			if f.Dir == d.path {
				d.entries = append(d.entries, dirEntry{
					name:    f.Name,
					attr:    attrArchive,
					cluster: f.cluster,
					size:    uint32(len(f.Data)),
				})
			}
		}
		if len(d.entries)*32 > clusterSize {
			return nil, fmt.Errorf("xapi: directory %q has too many entries for one cluster", d.path)
		}
	}
	return l, nil
}

// writeInto serialises the volume into part, which must be the partition's
// slice of the disk image and at least totalSectors*sectorSize long.
func (l *fatLayout) writeInto(part []byte) {
	l.writeBootSector(part[0:])
	l.writeFSInfo(part[sectorSize:])
	// The backup boot sector at sector 6 is what firmware falls back to
	// when sector 0 fails to parse; an ESP without it boots on forgiving
	// firmware and fails on strict firmware.
	l.writeBootSector(part[6*sectorSize:])

	fat := l.buildFAT()
	for i := range uint32(fatCount) {
		off := (fatReservedSectors + i*l.fatSectors) * sectorSize
		copy(part[off:], fat)
	}

	for _, d := range l.dirs {
		off := l.clusterOffset(d.cluster)
		buf := part[off : off+clusterSize]
		for i, e := range d.entries {
			e.writeInto(buf[i*32:])
		}
	}
	for _, f := range l.files {
		if f.clusters == 0 {
			continue
		}
		off := l.clusterOffset(f.cluster)
		copy(part[off:], f.Data)
	}
}

// clusterOffset converts a cluster number to a byte offset inside the
// partition. Cluster numbering starts at 2, which is why the data region's
// first cluster is not at dataStart + 2*clusterSize.
func (l *fatLayout) clusterOffset(cluster uint32) uint32 {
	return (l.dataStart + (cluster-2)*sectorsPerCluster) * sectorSize
}

// buildFAT returns one copy of the file allocation table.
func (l *fatLayout) buildFAT() []byte {
	fat := make([]byte, l.fatSectors*sectorSize)
	put := func(cluster, value uint32) {
		binary.LittleEndian.PutUint32(fat[cluster*4:], value)
	}
	// Entry 0 carries the media descriptor in its low byte; entry 1 is the
	// end-of-chain marker. Both are conventions rather than data.
	put(0, 0x0FFFFFF8)
	put(1, fatEOC)

	// Every directory is exactly one cluster, so its chain is one entry.
	for _, d := range l.dirs {
		put(d.cluster, fatEOC)
	}
	// Files occupy contiguous runs, so each entry points at its successor
	// and the last one terminates.
	for _, f := range l.files {
		for i := uint32(0); i < f.clusters; i++ {
			c := f.cluster + i
			if i == f.clusters-1 {
				put(c, fatEOC)
			} else {
				put(c, c+1)
			}
		}
	}
	return fat
}

func (l *fatLayout) writeBootSector(b []byte) {
	// Jump instruction and OEM name. Firmware does not execute this — the
	// ESP is read by the firmware's own FAT driver — but the fields are
	// checked for plausibility, and a zero jump fails that check.
	b[0], b[1], b[2] = 0xEB, 0x58, 0x90
	copy(b[3:11], "MSWIN4.1") // the string every FAT implementation trusts

	binary.LittleEndian.PutUint16(b[11:], sectorSize)
	b[13] = sectorsPerCluster
	binary.LittleEndian.PutUint16(b[14:], fatReservedSectors)
	b[16] = fatCount
	binary.LittleEndian.PutUint16(b[17:], 0) // root entries: 0 on FAT32
	binary.LittleEndian.PutUint16(b[19:], 0) // total sectors 16: 0, see offset 32
	b[21] = 0xF8                             // fixed disk
	binary.LittleEndian.PutUint16(b[22:], 0) // FAT size 16: 0 on FAT32
	binary.LittleEndian.PutUint16(b[24:], 63)
	binary.LittleEndian.PutUint16(b[26:], 255)
	binary.LittleEndian.PutUint32(b[28:], espFirstLBA) // hidden sectors
	binary.LittleEndian.PutUint32(b[32:], l.totalSectors)

	// FAT32 extended BPB.
	binary.LittleEndian.PutUint32(b[36:], l.fatSectors)
	binary.LittleEndian.PutUint16(b[40:], 0) // no mirroring flags: both FATs live
	binary.LittleEndian.PutUint16(b[42:], 0) // version 0.0
	binary.LittleEndian.PutUint32(b[44:], 2) // root directory cluster
	binary.LittleEndian.PutUint16(b[48:], 1) // FSInfo sector
	binary.LittleEndian.PutUint16(b[50:], 6) // backup boot sector
	b[64] = 0x80                             // drive number
	b[66] = 0x29                             // extended boot signature
	// Volume ID is fixed, not derived from the clock: see the note on
	// reproducibility at the top of this file.
	binary.LittleEndian.PutUint32(b[67:], 0x21544F4F)
	copy(b[71:82], "EFI SYSTEM ")
	copy(b[82:90], "FAT32   ")
	b[510], b[511] = 0x55, 0xAA
}

func (l *fatLayout) writeFSInfo(b []byte) {
	binary.LittleEndian.PutUint32(b[0:], 0x41615252)
	binary.LittleEndian.PutUint32(b[484:], 0x61417272)
	// Free count and next-free are advisory. 0xFFFFFFFF means "unknown",
	// which is honest and costs a rescan the firmware does anyway.
	binary.LittleEndian.PutUint32(b[488:], 0xFFFFFFFF)
	binary.LittleEndian.PutUint32(b[492:], 0xFFFFFFFF)
	binary.LittleEndian.PutUint32(b[508:], 0xAA550000)
}

const (
	attrArchive   = 0x20
	attrDirectory = 0x10
)

// dirEntry is one 32-byte FAT directory entry, short-name only.
type dirEntry struct {
	name    string
	attr    byte
	cluster uint32
	size    uint32
}

func dotEntry(name string, cluster uint32) dirEntry {
	return dirEntry{name: name, attr: attrDirectory, cluster: cluster}
}

func (e dirEntry) writeInto(b []byte) {
	copy(b[0:11], pad83(e.name))
	b[11] = e.attr
	// Creation and write timestamps are fixed: 1980-01-01, the FAT epoch,
	// encoded as date 0x0021. See the reproducibility note above.
	binary.LittleEndian.PutUint16(b[16:], 0x0021) // creation date
	binary.LittleEndian.PutUint16(b[18:], 0x0021) // last access date
	binary.LittleEndian.PutUint16(b[20:], uint16(e.cluster>>16))
	binary.LittleEndian.PutUint16(b[22:], 0)      // write time
	binary.LittleEndian.PutUint16(b[24:], 0x0021) // write date
	binary.LittleEndian.PutUint16(b[26:], uint16(e.cluster))
	binary.LittleEndian.PutUint32(b[28:], e.size)
}

// pad83 renders "BOOTX64.EFI" as the 11-byte on-disk form "BOOTX64 EFI".
func pad83(name string) []byte {
	out := []byte("           ")
	if name == "." || name == ".." {
		copy(out, name)
		return out
	}
	stem, ext, _ := strings.Cut(name, ".")
	copy(out[0:8], stem)
	copy(out[8:11], ext)
	return out
}

func validate83(name string) error {
	if name == "" {
		return errors.New("empty name")
	}
	stem, ext, hasDot := strings.Cut(name, ".")
	if len(stem) == 0 || len(stem) > 8 {
		return errors.New("stem must be 1-8 characters (long names are not supported)")
	}
	if hasDot && len(ext) > 3 {
		return errors.New("extension must be 0-3 characters")
	}
	if strings.Contains(ext, ".") {
		return errors.New("only one dot allowed")
	}
	if name != strings.ToUpper(name) {
		return errors.New("must be upper case")
	}
	return nil
}

func splitDir(dir string) []string {
	if dir == "" {
		return nil
	}
	return strings.Split(dir, "/")
}

func parentOf(p string) string {
	i := strings.LastIndex(p, "/")
	if i < 0 {
		return ""
	}
	return p[:i]
}

func base(p string) string {
	i := strings.LastIndex(p, "/")
	if i < 0 {
		return p
	}
	return p[i+1:]
}

// collectDirs returns every directory the files imply, parents before
// children, deduplicated.
func collectDirs(files []espFile) []string {
	seen := map[string]bool{}
	var out []string
	for _, f := range files {
		parts := splitDir(f.Dir)
		for i := range parts {
			p := strings.Join(parts[:i+1], "/")
			if !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	return out
}

// espTypeGUID is the EFI System Partition type, C12A7328-F81F-11D2-BA4B-
// 00A0C93EC93B, in GPT's mixed-endian on-disk form.
var espTypeGUID = [16]byte{
	0x28, 0x73, 0x2A, 0xC1, 0x1F, 0xF8, 0xD2, 0x11,
	0xBA, 0x4B, 0x00, 0xA0, 0xC9, 0x3E, 0xC9, 0x3B,
}

// espUniqueGUID identifies this particular partition. It is fixed for the
// same reason the timestamps are: the boot VDI is content-addressed, and a
// random GUID would make every build unique.
var espUniqueGUID = [16]byte{
	0x0B, 0xAD, 0xC0, 0xDE, 0x00, 0x01, 0x40, 0x00,
	0x9C, 0x1A, 0x00, 0x00, 0x63, 0x6C, 0x61, 0x77,
}

var diskGUID = [16]byte{
	0x0B, 0xAD, 0xC0, 0xDE, 0x00, 0x02, 0x40, 0x00,
	0x9C, 0x1A, 0x00, 0x00, 0x63, 0x6C, 0x61, 0x77,
}

// writeGPT lays the protective MBR, the primary GPT at LBA 1, and the backup
// GPT at the end of the disk.
func writeGPT(img []byte, totalSectors, firstLBA, lastLBA uint32) {
	writeProtectiveMBR(img, totalSectors)

	entries := make([]byte, gptEntryCount*gptEntrySize)
	copy(entries[0:16], espTypeGUID[:])
	copy(entries[16:32], espUniqueGUID[:])
	binary.LittleEndian.PutUint64(entries[32:], uint64(firstLBA))
	binary.LittleEndian.PutUint64(entries[40:], uint64(lastLBA))
	binary.LittleEndian.PutUint64(entries[48:], 0) // no attributes
	// Partition name, UTF-16LE.
	for i, r := range "EFI System Partition" {
		binary.LittleEndian.PutUint16(entries[56+i*2:], uint16(r))
	}
	entriesCRC := crc32.ChecksumIEEE(entries)

	backupHeaderLBA := uint64(totalSectors - 1)
	backupTableLBA := backupHeaderLBA - gptTableLBAs

	// Primary: header at LBA 1, table at LBA 2.
	copy(img[2*sectorSize:], entries)
	hdr := gptHeader(1, backupHeaderLBA, 2, uint64(firstLBA), uint64(lastLBA), entriesCRC)
	copy(img[1*sectorSize:], hdr)

	// Backup: table just below the last sector, header in it. The two
	// headers differ only in which LBA each calls "mine" and "other", and
	// in where each points for the table.
	copy(img[backupTableLBA*sectorSize:], entries)
	bhdr := gptHeader(backupHeaderLBA, 1, backupTableLBA, uint64(firstLBA), uint64(lastLBA), entriesCRC)
	copy(img[backupHeaderLBA*sectorSize:], bhdr)
}

func gptHeader(myLBA, altLBA, tableLBA, firstUsable, lastUsable uint64, entriesCRC uint32) []byte {
	h := make([]byte, sectorSize)
	copy(h[0:8], "EFI PART")
	binary.LittleEndian.PutUint32(h[8:], 0x00010000) // revision 1.0
	binary.LittleEndian.PutUint32(h[12:], 92)        // header size
	// h[16:20] is the header CRC, computed last over these 92 bytes with
	// the field itself zeroed.
	binary.LittleEndian.PutUint64(h[24:], myLBA)
	binary.LittleEndian.PutUint64(h[32:], altLBA)
	binary.LittleEndian.PutUint64(h[40:], firstUsable)
	binary.LittleEndian.PutUint64(h[48:], lastUsable)
	copy(h[56:72], diskGUID[:])
	binary.LittleEndian.PutUint64(h[72:], tableLBA)
	binary.LittleEndian.PutUint32(h[80:], gptEntryCount)
	binary.LittleEndian.PutUint32(h[84:], gptEntrySize)
	binary.LittleEndian.PutUint32(h[88:], entriesCRC)
	binary.LittleEndian.PutUint32(h[16:], crc32.ChecksumIEEE(h[0:92]))
	return h
}

// writeProtectiveMBR fills LBA 0 with a single type-0xEE partition spanning
// the disk, so tooling that only understands MBR sees the disk as fully
// occupied rather than as empty and safe to repartition.
func writeProtectiveMBR(img []byte, totalSectors uint32) {
	p := img[446:]
	p[0] = 0x00 // not bootable
	p[1], p[2], p[3] = 0x00, 0x02, 0x00
	p[4] = 0xEE
	p[5], p[6], p[7] = 0xFF, 0xFF, 0xFF
	binary.LittleEndian.PutUint32(p[8:], 1)
	size := totalSectors - 1
	if totalSectors-1 > 0xFFFFFFFF {
		size = 0xFFFFFFFF
	}
	binary.LittleEndian.PutUint32(p[12:], size)
	img[510], img[511] = 0x55, 0xAA
}
