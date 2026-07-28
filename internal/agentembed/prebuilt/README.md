# Prebuilt guest binaries (generated — not committed)

`make guestbin` writes `clawk-init`, `clawk-pty-agent`, `clawk-time-sync` and
`manifest.json` here, and `go:embed` bakes them into the clawk binary so a
release needs no Go toolchain at runtime.

Everything except this file and `.gitignore` is generated and ignored by git:
the binaries are ~7 MiB per architecture, and committing them would both bloat
the history and let them drift behind the `.go.in` sources they are built from.

A plain `go build` / `make install` leaves this directory empty, and
`internal/guestbuild` then compiles the guest binaries from the embedded
sources on first boot — the long-standing behaviour, and the one contributors
want while editing those sources.
