// smoke-xapi boots an OCI image as a VM on an XCP-ng pool via the machine
// library, in the same spirit as smoke-alpine and smoke-firecracker: a
// hand-crank end-to-end check of one backend with nothing else attached.
//
// Usage:
//
//	go run ./cmd/smoke-xapi \
//	    -url https://xcp-ng-01.lab.example -user root -password ... \
//	    -sr <sr-uuid> -mgmt-net <network-uuid> [-sandbox-net <network-uuid>] \
//	    -kernel /path/to/vmlinux [-ref docker.io/library/alpine:3.20]
//
// What it proves, in order: the OCI builder produces a disk the pool will
// accept; VDI import and clone work; the VM boots far enough to bring up
// its management VIF; and the control channel is reachable over TCP where
// vsock would have been. That last step is the one that decides whether
// this backend is viable, so it is the loudest thing this tool prints.
//
// It does NOT exercise the egress filter: on this backend that lives in the
// gateway VM, not in the daemon, so it is a separate test.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/clawkwork/clawk/machine"
	"github.com/clawkwork/clawk/machine/oci"
	"github.com/clawkwork/clawk/machine/xapi"
)

const (
	defaultRef  = "docker.io/library/alpine:3.20"
	controlPort = 1024 // whatever the guest agent listens on
)

func main() {
	var (
		url        = flag.String("url", "", "pool master URL (required)")
		user       = flag.String("user", "root", "XAPI username")
		password   = flag.String("password", os.Getenv("XAPI_PASSWORD"), "XAPI password (or $XAPI_PASSWORD)")
		srUUID     = flag.String("sr", "", "SR uuid for rootfs VDIs (required, prefer a file-based SR)")
		mgmtNet    = flag.String("mgmt-net", "", "network uuid for the control VIF (required)")
		sandboxNet = flag.String("sandbox-net", "", "network uuid for the guest VIF; empty creates a private one")
		ref        = flag.String("ref", defaultRef, "OCI image reference")
		kernel     = flag.String("kernel", "", "vmlinux for the synthesised boot VDI (required)")
		vcpu       = flag.Uint("cpu", 2, "vCPUs")
		memMiB     = flag.Uint64("memory", 1024, "guest memory, MiB")
		keep       = flag.Bool("keep", false, "leave the VM behind on exit")
		insecure   = flag.Bool("insecure", false, "skip TLS verification (stock XCP-ng certs are self-signed)")
	)
	flag.Parse()

	for name, v := range map[string]string{"url": *url, "sr": *srUUID, "mgmt-net": *mgmtNet, "kernel": *kernel} {
		if v == "" {
			log.Fatalf("-%s is required", name)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	work, err := os.MkdirTemp("", "smoke-xapi-")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(work)

	// 1. Build the rootfs. This step is backend-independent — the same
	//    code path vz and firecracker use — which is most of why adding a
	//    backend is tractable at all.
	log.Printf("building rootfs from %s", *ref)
	built, err := oci.Build(ctx, oci.Options{
		Ref:      *ref,
		CacheDir: filepath.Join(work, "oci"),
	})
	if err != nil {
		log.Fatalf("oci build: %v", err)
	}
	log.Printf("rootfs %s (digest %s)", built.DiskPath, built.Digest)

	// 2. Point the backend at the pool.
	xapi.Configure(xapi.Config{
		URL:                *url,
		Username:           *user,
		Password:           *password,
		SRUUID:             *srUUID,
		MgmtNetworkUUID:    *mgmtNet,
		SandboxNetworkUUID: *sandboxNet,
		InsecureTLS:        *insecure,
	})

	b, err := machine.Get(xapi.Name)
	if err != nil {
		log.Fatal(err)
	}

	m, err := b.New(ctx, machine.Spec{
		ID:        "smoke",
		VCPU:      *vcpu,
		MemoryMiB: *memMiB,
		Boot: machine.DirectKernel{
			Vmlinux: *kernel,
			// root=/dev/xvdb: the boot VDI is xvda, the rootfs is xvdb.
			Cmdline: "console=hvc0 root=/dev/xvdb rw init=/bin/sh",
		},
		RootFS: machine.OCIImage{
			Ref:      *ref,
			CacheDir: filepath.Join(work, "oci"),
		},
		Serial: machine.Serial{LogPath: filepath.Join(work, "console.log")},
	}, filepath.Join(work, "state"))
	if err != nil {
		log.Fatalf("new machine: %v", err)
	}

	// 3. Create, start, and wait for the guest to be addressable.
	if err := m.Create(ctx); err != nil {
		log.Fatalf("create: %v", err)
	}
	if !*keep {
		defer func() {
			if err := m.Destroy(context.WithoutCancel(ctx)); err != nil {
				log.Printf("destroy: %v", err)
			}
		}()
	}

	start := time.Now()
	if err := m.Start(ctx); err != nil {
		log.Fatalf("start: %v", err)
	}
	log.Printf("VM.start returned after %s", time.Since(start).Round(time.Millisecond))

	if err := waitState(ctx, m, machine.StateRunning, 60*time.Second); err != nil {
		log.Fatalf("waiting for running: %v", err)
	}
	log.Printf("running after %s", time.Since(start).Round(time.Millisecond))

	// 4. The interesting part: reach the guest agent over TCP where every
	//    other backend would use vsock.
	dialer, ok := m.(xapi.ControlDialer)
	if !ok {
		log.Fatal("backend does not implement ControlDialer")
	}
	dialCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	conn, err := dialer.Control(dialCtx, controlPort)
	if err != nil {
		log.Fatalf("control channel: %v (this is the load-bearing failure — "+
			"if the guest is up and this does not connect, the mgmt VIF or the "+
			"guest agent is the problem, not the backend)", err)
	}
	defer conn.Close()
	fmt.Printf("control channel up: %s -> %s after %s\n",
		conn.LocalAddr(), conn.RemoteAddr(), time.Since(start).Round(time.Millisecond))

	// 5. Exercise the optional interfaces XAPI is unusually good at.
	if p, ok := m.(machine.Pauseable); ok {
		if err := p.Pause(ctx); err != nil {
			log.Printf("pause: %v", err)
		} else if err := p.Resume(ctx); err != nil {
			log.Printf("resume: %v", err)
		} else {
			fmt.Println("pause/resume ok")
		}
	}
	if s, ok := m.(machine.Suspendable); ok {
		dir := filepath.Join(work, "suspend")
		if err := s.Suspend(ctx, dir); err != nil {
			log.Printf("suspend: %v", err)
		} else {
			fmt.Println("suspended to SR")
			if r, ok := m.(machine.Snapshottable); ok {
				if err := r.Restore(ctx, dir); err != nil {
					log.Printf("restore: %v", err)
				} else {
					fmt.Println("restored from suspend")
				}
			}
		}
	}
}

func waitState(ctx context.Context, m machine.Machine, want machine.State, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		got, err := m.State(ctx)
		if err != nil {
			return err
		}
		if got == want {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out in state %q waiting for %q", got, want)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}
