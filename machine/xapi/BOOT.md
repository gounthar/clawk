# How the boot VDI actually boots

Reference for the `DirectKernel` boot path on XCP-ng. Three things here were
established by investigation rather than by reading a spec, and each of them
invalidates something that looks obvious. They are written down because the
cost of rediscovering them is a VM that silently never reaches a kernel.

Everything below was checked on 2026-08-10 against XCP-ng 8.3.0 / XAPI 26.1,
reversing the `EFI-variables` blob of a standing UEFI template. All pool access
was read-only: session login, getters, logout.

## 1. The kernel must be a bzImage, not the vmlinux the module hands you

`machine/kernel` returns a raw ELF **vmlinux**. On x86_64 that is
`opt/kata/share/kata-containers/vmlinux-6.18.15-186`, because
`DefaultKernelURLs` publishes an arm64 asset only and every other arch falls
back to the Kata static archive. Firecracker and vz load ELF directly, so this
is correct for them and must not be "fixed" globally.

It is wrong here. Nothing in the UEFI path boots a bare ELF vmlinux: GRUB's
`linux` command implements the Linux/x86 boot protocol and expects a bzImage,
and a stock vmlinux is not multiboot2-compliant either. XCP-ng is x86_64 in
practice, so the fallback path is the path.

The same Kata archive ships both, under the same version string:

| Member | Size | Format |
|--------|------|--------|
| `vmlinux-6.18.15-186` | 339,965,608 | ELF |
| `vmlinuz-6.18.15-186` | 8,347,648 | bzImage |

The bzImage is a UEFI-loadable PE: `MZ` at offset 0, PE pointer at `0x3c`, and
it contains the strings `EFI stub:` and `ERROR: efi_stub_entry() failed!`, so
`CONFIG_EFI_STUB` is on. `file(1)` reports boot protocol 2.15, relocatable,
max cmdline 2047.

So this backend wants a per-backend `BinaryPath` selecting the `vmlinuz`
member. It does not want a change to `kernel.DefaultBinaryPath`.

The archive ships **no** `grubx64.efi` and no shim — only OVMF firmware. Any
design that wants GRUB has to source that binary from somewhere else and put it
in the trust path.

## 2. The ESP

`espimage.go` builds the disk: protective MBR, GPT, one FAT32 EFI System
Partition. The firmware loads the kernel from the removable-media fallback
path, so the bzImage goes to `\EFI\BOOT\BOOTX64.EFI` with the initrd beside it.

The FAT writer is deliberately partial — write-once, contiguous cluster runs,
8.3 names only. See the comment at the top of `espimage.go` for why that
removes the parts of FAT that carry bugs rather than merely cutting corners.

## 3. The kernel command line rides in UEFI NVRAM

There is no bootloader, so nothing is available to hand the kernel a command
line. The EFI stub reads it from the EFI `LoadOptions` it was launched with,
and the removable-media fallback path supplies none. The command line is
therefore delivered as a boot option in NVRAM.

An `EFI_LOAD_OPTION` is:

    attributes            u32
    file_path_list_length u16
    description           UTF-16, NUL terminated
    device path
    optional data         to the end of the variable

That trailing optional data is what the stub reads as its command line. So
`root=/dev/xvdb` means writing a `Boot0000` whose optional data is the cmdline,
and a `BootOrder` pointing at it.

### VM.NVRAM wire format

`VM.NVRAM` is a map with exactly one key, `EFI-variables`, holding base64. The
decoded blob is **varstored's own serialisation**, not the EDK2 flash varstore
layout — which is the trap, because the EDK2 format is what you find if you go
looking for "UEFI variable store format".

Header, 32 bytes:

| Offset | Size | Meaning |
|--------|------|---------|
| 0  | 4 | magic `VARS` |
| 4  | 4 | version, `2` |
| 8  | 8 | variable count |
| 16 | 8 | length of the variable section |
| 24 | 8 | zero |

The variable section does **not** begin at offset 32. In the sample blob it
began at 292, and `292 + section_length == len(blob)` exactly. Compute the
start as `len(blob) - section_length`; do not hardcode 292. The 260 bytes
between the header and the first record were not identified.

That is not a theoretical caution. The sample reversed here was the *template*
`jenkins-golden-debian` (37564 bytes); the VM of the same name on the same host
carries 37444. Two records that look interchangeable do not have the same
length, so an offset that happens to work against one will silently mis-parse
the other. Note also that `VM.get_all_records` returns templates alongside VMs,
so a probe that matches on `name_label` alone can easily read the one it did
not mean to — which is how the size difference came to light.

Then `count` records, each:

    name_len    u64 LE   (bytes, not characters)
    name        UTF-16LE, name_len bytes, no NUL terminator
    data_len    u64 LE
    data        data_len bytes
    guid        16 bytes, EFI mixed-endian
    attributes  u32 LE
    trailer     48 bytes

The 48-byte trailer is all zero for ordinary variables. It is non-zero only for
the four time-based authenticated variables — PK, KEK, db, dbx — whose
attributes are `0x27`, that is `0x07 | TIME_BASED_AUTHENTICATED_WRITE_ACCESS`.
An ordinary variable is written with `attr = 0x07` (NV|BS|RT) and 48 zero
bytes.

This was verified by walking, not by assumption: parsing 31 records from offset
292 lands exactly on EOF and matches the declared count.

### Secure Boot

The UEFI VMs on the reference pool have PK, KEK, db and dbx all provisioned. An
unsigned, self-built kernel will not load while Secure Boot is enforcing.

Because this backend *constructs* the NVRAM rather than inheriting it, the fix
is to omit those four variables, which leaves the firmware in setup mode with
Secure Boot off. The trap is copying a stock VM's NVRAM wholesale as a starting
point — it looks like the obvious shortcut and it quietly breaks the boot.

## Re-deriving this

If a future XCP-ng release changes the format, the way back is to read
`VM.NVRAM` from any existing UEFI VM, base64-decode, and locate known variable
names as UTF-16LE inside the blob; the bytes around them reveal the framing.
Solve the trailer size by walking records and requiring the walk to land
exactly on EOF with the declared count. That is how the table above was
produced, and it does not depend on varstored's source being to hand.
