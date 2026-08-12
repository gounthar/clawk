# Serial devices

Present a serial port plugged into your Mac — an Arduino, an ESP32, a
USB-TTL adapter — as a device inside a sandbox, so `arduino-cli`, `esptool`,
`avrdude` and a serial monitor can run in there against real hardware.

```sh
clawk serial add <sandbox> /dev/cu.usbmodem1101           # same name in the guest
clawk serial add <sandbox> /dev/cu.usbmodem1101:ttyACM0   # /dev/ttyACM0 in the guest
clawk serial list <sandbox>
clawk serial remove <sandbox> ttyACM0
```

Like reverse forwards, these apply to a running sandbox immediately — no
`down`/`up` cycle — and they are vz (macOS) only, because the guest is the
end that dials and firecracker's vsock is one-way.

## What actually crosses

The USB device is **not** passed through. No hypervisor clawk targets can do
that: Virtualization.framework's USB controller carries virtual mass-storage
devices only, with no API for a physical device, and firecracker has no USB
at all — no PCI bus to hang a controller off.

What crosses is the serial stream and its line settings, over vsock. Inside
the guest that arrives as a PTY symlinked to `/dev/<name>`; on the host,
clawk opens the real port and pumps bytes between the two. This is what every
serial tool actually wants — none of them care that a tty is behind a USB
device rather than a 16550.

Two consequences worth knowing up front:

- **The port is only held while the guest is using it.** clawk opens the
  physical device when a process in the sandbox opens `/dev/<name>` and
  closes it when that process lets go. The Arduino IDE on your Mac can have
  the board the rest of the time.
- **Opening the device resets the board.** That is not a clawk behaviour, it
  is the auto-reset circuit on the board: opening a serial port asserts DTR,
  and DTR is wired to RESET through a capacitor. It's the same reason opening
  the Arduino IDE's serial monitor reboots an Uno. Because the host open is
  tied to the guest open, this happens at exactly the moment the tooling
  expects it.

## What doesn't cross: DTR and RTS

A PTY has no modem-control lines. `TIOCMGET` and `TIOCMSET` return `ENOTTY`
on both ends of one, so a guest tool that toggles DTR or RTS explicitly gets
an error, and no amount of protocol work on clawk's side can change that.

In practice this affects less than it sounds like, because the two things
those lines are used for both have another path:

| Board style | How it enters the bootloader | Works? |
|---|---|---|
| Native USB (Leonardo, Micro, most ESP32-S3, RP2040) | 1200-baud touch — open at 1200 baud, close | **Yes.** A PTY does carry the baud rate, and clawk forwards the close too |
| Classic auto-reset (Uno, Nano, Mega) | DTR pulse | **Yes, via the open.** Opening the port asserts DTR, which is the pulse |
| ESP32 with the classic auto-program circuit | DTR *and* RTS in sequence, to drive GPIO0 and EN separately | **No.** Two lines in a specific order can't be expressed |

For that last row — a plain ESP32 DevKit with `esptool` — hold the BOOT
button while the upload starts, or use a board with native USB. `esptool`'s
`--before no_reset` skips the sequence it can't perform.

You may still see an `ioctl("TIOCMGET")` warning from `avrdude` even on a
board that uploads fine. It is telling the truth about the ioctl and is
harmless: the reset already happened when the port opened.

## Boards that re-enumerate

A board entering its bootloader drops off the USB bus and comes back, often
under a *different* device name — `cu.usbmodem1101` becomes
`cu.usbmodem14201` and back again. A literal path breaks on that; a glob
doesn't:

```sh
clawk serial add <sandbox> '/dev/cu.usbmodem*:ttyACM0'
```

The pattern is resolved each time the port is opened, not when you configure
it. Quote it so your shell doesn't expand it first. A pattern matching two
boards is refused rather than guessed at — flashing the wrong device is the
one failure worth being loud about.

The guest-side name never changes across a re-enumeration, so `arduino-cli
upload -p /dev/ttyACM0` keeps working through the whole cycle.

## Declaring devices in clawk.mod

```
sandbox firmware (
    serial (
        /dev/cu.usbmodem1101                 # /dev/cu.usbmodem1101 in the guest
        /dev/cu.usbserial-A50285BI ttyUSB0   # /dev/ttyUSB0 in the guest
        /dev/cu.usbmodem* ttyACM0            # resolved at open time
    )
)
```

Host device first, optional guest name second — space-separated, matching
`files` and `shares` rather than the CLI's colon form, because a colon inside
a path is ambiguous in a way a port number never is.

Two entries claiming the same guest name, or the same host port, are refused
with an error naming both contributors rather than silently resolved.

## Working with a board from the sandbox

`arduino-cli board list` won't find anything: it enumerates USB VID/PID
through libusb, and there is no USB in there. Name the port and the board
explicitly instead — which is what you'd do in CI anyway:

```sh
arduino-cli compile --fqbn arduino:avr:uno sketch/
arduino-cli upload  --fqbn arduino:avr:uno -p /dev/ttyACM0 sketch/
arduino-cli monitor -p /dev/ttyACM0 -c baudrate=115200
```

`esptool` and `avrdude` take `-p`/`-P` the same way. Anything that opens a
tty and sets a baud rate works; `screen`, `picocom` and `cat` are all fine.

One-shot writes work too, but note what they cost:

```sh
echo 'status?' > /dev/ttyACM0          # opens, writes, closes — and resets
```

The host port is open only while a process in the sandbox holds the device,
so a command like that opens and closes it around a single write — and since
opening asserts DTR, each one resets a board wired for auto-reset. Fine for a
one-off; wrong for a loop. Hold the device open instead, and the port stays
open with it:

```sh
exec 3<>/dev/ttyACM0                   # one open, one reset
echo 'status?' >&3
cat <&3 &
exec 3<&-                              # release it
```

## On macOS: `cu.` not `tty.`

Use the callout device (`/dev/cu.*`), not the dial-in device
(`/dev/tty.*`). The dial-in side blocks on carrier detect, which shows up as
a port that opens and then does nothing. `clawk serial add` warns if you name
a `tty.` device.

## Troubleshooting

**"no device matches"** — the board isn't plugged in, or is mid-reset. clawk
waits about three seconds for it during an attach, which covers a
re-enumeration; past that the guest retries on its own. `clawk serial list`
shows what's present right now.

**"already in use"** — something else has the port. Inside the guest, only
one process can hold a device at a time; on the Mac, check for an open
Arduino IDE serial monitor.

**Uploads hang at "not in sync"** — the board didn't reset. See the DTR table
above; for a classic ESP32 DevKit, hold BOOT.

**Nothing appears at `/dev/<name>`** — the sandbox has to be running vz and
its guest agent has to be current. `clawk down && clawk up` re-injects the
agent; `clawk serial add` says so explicitly when the daemon is too old.

## See also

- [Networking](networking.md) — port forwarding in both directions, which
  serial forwarding is deliberately shaped like
- [Configuration](configuration.md) — the full `clawk.mod` reference
