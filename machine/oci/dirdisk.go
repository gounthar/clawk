// Userspace directory → ext4 disk image. The firecracker provider carries a
// sandbox's worktree on its own virtio-blk disk built through here, because
// the alternative — loop-mounting the rootfs and copying files in — needs
// CAP_SYS_ADMIN on the host for every boot, which is the privilege clawk
// exists to avoid handing out.
//
// The same ext4 writer already builds every OCI rootfs (writeDisk in oci.go);
// this is that path with a filesystem walk in front of it instead of a
// flattened image.

package oci

import (
	"archive/tar"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/clawkwork/clawk/machine/internal/ext4"
)

// WriteDirDisk builds a writable ext4 image at dst holding the tree rooted
// at srcDir, padded to sizeBytes so the guest has room to allocate (the
// padding is a sparse hole; it costs no physical disk until written). The
// tree lands at the image root: srcDir/foo is /foo in the image.
//
// Ownership, mode bits and symlinks are preserved as-is — including srcDir's
// own, so the mounted root belongs to whoever owns the source rather than to
// root — which keeps the semantics of the `cp -a` this replaced. Sockets and
// devices are skipped: a worktree has none, and neither survives a copy
// meaningfully. Hardlinks are written as independent regular files: git's
// object store has none (objects are distinct files), and duplicating is safer
// than emitting a link whose target the walk hasn't reached yet.
//
// The image is built under a temporary name and renamed into place, so an
// interrupted build never leaves a plausible-looking partial disk behind.
func WriteDirDisk(srcDir, dst string, sizeBytes int64) error {
	// Resolved before anything looks at it, because the walk below lstats its
	// root: a srcDir whose last component is a symlink to a directory passes
	// the IsDir check (os.Stat follows) and then yields exactly one walk entry
	// — the root — producing a well-formed EMPTY image with no error anywhere.
	// Errors keep naming the path the caller passed, not the resolved one.
	given := srcDir
	srcDir, err := filepath.EvalSymlinks(given)
	if err != nil {
		return fmt.Errorf("oci: dir disk source %q: %w", given, err)
	}
	info, err := os.Stat(srcDir)
	if err != nil {
		return fmt.Errorf("oci: dir disk source %q: %w", given, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("oci: dir disk source %q is not a directory", given)
	}

	pr, pw := io.Pipe()
	go func() {
		tw := tar.NewWriter(pw)
		err := tarDir(tw, srcDir)
		if err == nil {
			err = tw.Close()
		}
		pw.CloseWithError(err)
	}()
	// Closing the read end unblocks the walk goroutine if the conversion
	// bails before draining the stream.
	defer pr.Close()

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("oci: preparing dir disk dir: %w", err)
	}
	tmp := dst + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("oci: creating dir disk: %w", err)
	}
	defer func() {
		f.Close()
		os.Remove(tmp)
	}()

	if err := ext4.Convert(pr, f, ext4.Writable(), ext4.TotalSize(sizeBytes)); err != nil {
		return fmt.Errorf("oci: converting %s to ext4: %w", srcDir, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("oci: closing dir disk: %w", err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		return fmt.Errorf("oci: promoting dir disk: %w", err)
	}
	return nil
}

// tarDir walks root and writes every entry to tw with paths relative to
// root. Walk order is lexical (fs.WalkDir), so parents always precede
// children — though the converter tolerates any order.
//
// The root's own entry ("./") is emitted like any other: the converter reads it
// for the image root's owner and mode, which it would otherwise leave at
// 0755 root:root — a workspace whose root the guest user cannot write in.
func tarDir(tw *tar.Writer, root string) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			// A file that vanished mid-walk (a build artifact, an editor's
			// temp file) must not fail the whole disk.
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		switch {
		case info.Mode().IsRegular(), d.IsDir(), info.Mode()&os.ModeSymlink != 0:
		default:
			return nil // sockets, devices, fifos
		}

		var link string
		if info.Mode()&os.ModeSymlink != 0 {
			if link, err = os.Readlink(path); err != nil {
				return err
			}
		}
		hdr, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		// Slash-separated, no leading slash — same shape appendExtra uses.
		// (Uid/Gid need no help: tar.FileInfoHeader fills them from the
		// syscall.Stat_t on every unix.)
		hdr.Name = filepath.ToSlash(rel)
		if d.IsDir() && !strings.HasSuffix(hdr.Name, "/") {
			hdr.Name += "/"
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return fmt.Errorf("oci: writing %s: %w", hdr.Name, err)
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		src, err := os.Open(path)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		defer src.Close()
		// CopyN, not Copy: the header committed to info.Size() bytes, and a
		// file being appended to while we walk would otherwise desync the
		// stream. Short files are padded by the zero writer below.
		n, err := io.CopyN(tw, src, info.Size())
		if err == io.EOF && n < info.Size() {
			_, err = tw.Write(make([]byte, info.Size()-n))
		}
		if err != nil && err != io.EOF {
			return fmt.Errorf("oci: copying %s: %w", hdr.Name, err)
		}
		return nil
	})
}
