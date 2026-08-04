package xapi

import (
	"context"
	"errors"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/clawkwork/clawk/machine"
)

// fakePool is an in-memory Client. It exists so the backend's lifecycle
// logic can be developed and regression-tested with no hypervisor, no
// network and no credentials — which is most of the backend, since the
// interesting parts are ordering and state mapping rather than RPC.
type fakePool struct {
	power    map[VMRef]PowerState
	nextVM   int
	nextVDI  int
	destroys []VMRef
	// calls records the method sequence, so tests can assert ordering
	// (e.g. that Destroy hard-shuts-down before VM.destroy).
	calls []string
	// closed records that the pool session was logged out.
	closed bool
	// powerHook runs at the top of VMPowerState. Tests use it to hold the
	// call open and watch what the rest of the backend can still do while a
	// pool call is outstanding.
	powerHook func()
	// resumeHook runs at the top of VMResumeFromSuspend, for the same reason:
	// Restore's window between its lifecycle check and its commit is only
	// reachable while that call is in flight.
	resumeHook func()
}

func newFakePool() *fakePool {
	return &fakePool{power: map[VMRef]PowerState{}}
}

func (f *fakePool) log(name string) { f.calls = append(f.calls, name) }

func (f *fakePool) VMCreate(_ context.Context, c VMConfig) (VMRef, error) {
	f.log("VMCreate")
	f.nextVM++
	ref := VMRef(c.NameLabel)
	f.power[ref] = PowerHalted
	return ref, nil
}

func (f *fakePool) VMStart(_ context.Context, ref VMRef) error {
	f.log("VMStart")
	if f.power[ref] != PowerHalted && f.power[ref] != PowerSuspended {
		return errors.New("fake: bad power state for start")
	}
	f.power[ref] = PowerRunning
	return nil
}

func (f *fakePool) VMShutdown(_ context.Context, ref VMRef, graceful bool) error {
	if graceful {
		f.log("VMCleanShutdown")
	} else {
		f.log("VMHardShutdown")
	}
	f.power[ref] = PowerHalted
	return nil
}

func (f *fakePool) VMDestroy(_ context.Context, ref VMRef) error {
	f.log("VMDestroy")
	f.destroys = append(f.destroys, ref)
	delete(f.power, ref)
	return nil
}

func (f *fakePool) VMPowerState(_ context.Context, ref VMRef) (PowerState, error) {
	if f.powerHook != nil {
		f.powerHook()
	}
	ps, ok := f.power[ref]
	if !ok {
		return "", errors.New("fake: no such VM")
	}
	return ps, nil
}

func (f *fakePool) VMPause(_ context.Context, ref VMRef) error {
	f.log("VMPause")
	f.power[ref] = PowerPaused
	return nil
}

func (f *fakePool) VMUnpause(_ context.Context, ref VMRef) error {
	f.log("VMUnpause")
	f.power[ref] = PowerRunning
	return nil
}

func (f *fakePool) VMSuspend(_ context.Context, ref VMRef) error {
	f.log("VMSuspend")
	f.power[ref] = PowerSuspended
	return nil
}

func (f *fakePool) VMResumeFromSuspend(_ context.Context, ref VMRef) error {
	if f.resumeHook != nil {
		f.resumeHook()
	}
	f.log("VMResume")
	f.power[ref] = PowerRunning
	return nil
}

func (f *fakePool) VMCheckpoint(_ context.Context, ref VMRef, name string) (SnapshotRef, error) {
	f.log("VMCheckpoint")
	return SnapshotRef(name), nil
}

func (f *fakePool) VMGuestIP(context.Context, VMRef, string) (string, error) {
	return "127.0.0.1", nil
}

func (f *fakePool) VDIImportRaw(_ context.Context, _, name string, r io.Reader, _ int64) (VDIRef, error) {
	f.log("VDIImportRaw")
	if _, err := io.Copy(io.Discard, r); err != nil {
		return "", err
	}
	f.nextVDI++
	return VDIRef(name), nil
}

func (f *fakePool) VDIFindByName(context.Context, string, string) (VDIRef, bool, error) {
	return "", false, nil
}

func (f *fakePool) VDIClone(_ context.Context, ref VDIRef) (VDIRef, error) {
	f.log("VDIClone")
	return ref + "-clone", nil
}

func (f *fakePool) VDIDestroy(context.Context, VDIRef) error { f.log("VDIDestroy"); return nil }

func (f *fakePool) VBDAttach(context.Context, VMRef, VDIRef, string, bool) error {
	f.log("VBDAttach")
	return nil
}

func (f *fakePool) NetworkCreatePrivate(context.Context, string) (string, error) {
	f.log("NetworkCreatePrivate")
	return "net-fake", nil
}

func (f *fakePool) NetworkDestroy(context.Context, string) error {
	f.log("NetworkDestroy")
	return nil
}

func (f *fakePool) VIFAttach(context.Context, VMRef, string, string, string) error {
	f.log("VIFAttach")
	return nil
}

func (f *fakePool) Close() error {
	f.log("Close")
	f.closed = true
	return nil
}

var _ Client = (*fakePool)(nil)

// withFakePool points the backend at an in-memory pool for the duration of
// a test.
func withFakePool(t *testing.T) *fakePool {
	t.Helper()
	f := newFakePool()
	prevCfg := currentConfig()
	prevDial := dialClient
	Configure(Config{URL: "https://fake", SRUUID: "sr-fake", MgmtNetworkUUID: "net-mgmt"})
	dialClient = func(context.Context, Config) (Client, error) { return f, nil }
	t.Cleanup(func() { Configure(prevCfg); dialClient = prevDial })
	return f
}

func validSpec() machine.Spec {
	return machine.Spec{
		ID:        "test",
		VCPU:      2,
		MemoryMiB: 512,
		Boot:      machine.DirectKernel{Vmlinux: "/nonexistent/vmlinux", Cmdline: "root=/dev/xvdb"},
		RootFS:    machine.RawDisk{Path: "/nonexistent/root.ext4"},
	}
}

func TestRegistered(t *testing.T) {
	if _, err := machine.Get(Name); err != nil {
		t.Fatalf("backend not registered: %v", err)
	}
}

// The Spec fields Xen cannot express must be refused by New, before any
// side effects — not silently ignored at boot.
func TestNewRejectsUnsupportedSpec(t *testing.T) {
	withFakePool(t)
	b, err := machine.Get(Name)
	if err != nil {
		t.Fatal(err)
	}

	cases := map[string]func(*machine.Spec){
		"shares":    func(s *machine.Spec) { s.Shares = []machine.Share{{Tag: "repo", HostPath: "/tmp"}} },
		"vsock":     func(s *machine.Spec) { s.VSockCID = 3 },
		"usermode":  func(s *machine.Spec) { s.Net = []machine.Net{machine.UserMode{}} },
		"tap":       func(s *machine.Spec) { s.Net = []machine.Net{machine.TAP{Device: "tap0"}} },
		"balloon":   func(s *machine.Spec) { s.MemoryMaxMiB = 4096 },
		"nestedoff": func(s *machine.Spec) { s.NestedVirt = true },
	}

	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			spec := validSpec()
			mutate(&spec)
			if _, err := b.New(context.Background(), spec, t.TempDir()); !errors.Is(err, machine.ErrUnsupportedSpec) {
				t.Fatalf("want ErrUnsupportedSpec, got %v", err)
			}
		})
	}
}

func TestNewRequiresConfigure(t *testing.T) {
	prev := currentConfig()
	Configure(Config{})
	t.Cleanup(func() { Configure(prev) })

	b, _ := machine.Get(Name)
	if _, err := b.New(context.Background(), validSpec(), t.TempDir()); err == nil {
		t.Fatal("want error when unconfigured")
	}
}

// VSock must fail with the sentinel the machine package documents, so
// callers can feature-detect rather than string-match.
func TestVSockUnsupported(t *testing.T) {
	withFakePool(t)
	b, _ := machine.Get(Name)
	m, err := b.New(context.Background(), validSpec(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.VSock(context.Background(), 1024); !errors.Is(err, machine.ErrVSockUnsupported) {
		t.Fatalf("want ErrVSockUnsupported, got %v", err)
	}
}

func TestOptionalInterfaces(t *testing.T) {
	withFakePool(t)
	b, _ := machine.Get(Name)
	m, err := b.New(context.Background(), validSpec(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m.(machine.Pauseable); !ok {
		t.Error("want Pauseable")
	}
	if _, ok := m.(machine.Suspendable); !ok {
		t.Error("want Suspendable")
	}
	if _, ok := m.(machine.Snapshottable); !ok {
		t.Error("want Snapshottable")
	}
	if _, ok := m.(ControlDialer); !ok {
		t.Error("want ControlDialer")
	}
	if b.Capabilities().VSock {
		t.Error("Caps.VSock must be false on Xen")
	}
	if !b.Capabilities().Snapshot {
		t.Error("Caps.Snapshot must be true")
	}
}

// Suspend writes a pointer file rather than a memory image, since XAPI
// keeps the image in the SR. Round-tripping it is the contract this
// backend actually has to honour.
func TestSuspendPointerRoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := pointer{Kind: "suspend", VM: "vm-1"}
	if err := writePointer(dir, want); err != nil {
		t.Fatal(err)
	}
	got, err := readPointer(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

// writePointer replaces the file atomically, so a second Suspend overwrites
// a first cleanly and leaves no temp file behind for readPointer to trip on.
func TestWritePointerReplacesAtomically(t *testing.T) {
	dir := t.TempDir()
	if err := writePointer(dir, pointer{Kind: "suspend", VM: "vm-1"}); err != nil {
		t.Fatal(err)
	}
	want := pointer{Kind: "checkpoint", VM: "vm-1", Snapshot: "snap-1"}
	if err := writePointer(dir, want); err != nil {
		t.Fatal(err)
	}

	got, err := readPointer(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}

	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 1 || ents[0].Name() != pointerName {
		var names []string
		for _, e := range ents {
			names = append(names, e.Name())
		}
		t.Fatalf("want only %s in the directory, got %v", pointerName, names)
	}
}

// TODO(create): once Create is implemented, assert the call ordering
// against fakePool.calls — import before clone, clone before VBDAttach,
// VIFAttach before VMStart — and that Destroy hard-shuts-down first and
// never destroys the shared golden VDI.

// --- regressions for the review findings on PR #1 ----------------------

// markCreated fakes what a successful Create would have recorded. Create is
// still a stub, and the lifecycle rules below are all about a machine that
// *was* created, so there is no other way to reach that state yet. Same
// package, so this is white-box on purpose rather than for convenience.
func markCreated(t *testing.T, m machine.Machine, f *fakePool, ref VMRef) *vm {
	t.Helper()
	v, ok := m.(*vm)
	if !ok {
		t.Fatalf("want *vm, got %T", m)
	}
	v.mu.Lock()
	v.ref, v.created = ref, true
	v.mu.Unlock()
	f.power[ref] = PowerRunning
	return v
}

// newMachine returns a fresh machine on the given fake pool.
func newMachine(t *testing.T) (machine.Machine, *fakePool) {
	t.Helper()
	f := withFakePool(t)
	b, err := machine.Get(Name)
	if err != nil {
		t.Fatal(err)
	}
	m, err := b.New(context.Background(), validSpec(), t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return m, f
}

// A Destroy that failed must not mark the machine destroyed. Doing so makes
// the next Destroy return nil having torn nothing down, and makes State
// report StateDestroyed for a VM still running on the pool.
func TestFailedDestroyDoesNotMarkDestroyed(t *testing.T) {
	m, f := newMachine(t)
	markCreated(t, m, f, "vm-1")

	// teardown is still a stub, so Destroy on a created machine must fail...
	if err := m.Destroy(context.Background()); err == nil {
		t.Fatal("Destroy returned nil; it is unimplemented and must report that")
	}
	// ...and failing must not have been recorded as success.
	if err := m.Destroy(context.Background()); err == nil {
		t.Fatal("second Destroy returned nil: the first one marked the machine " +
			"destroyed despite failing")
	}
	st, err := m.State(context.Background())
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if st == machine.StateDestroyed {
		t.Fatal("State reports StateDestroyed after a failed Destroy")
	}
	if f.closed {
		t.Fatal("pool session was logged out even though Destroy failed; " +
			"a retry would find a dead session")
	}
}

// Every call that reaches the pool with v.ref must first check that the
// machine was created and not destroyed. Otherwise the empty ref goes to
// XAPI, which answers HANDLE_INVALID instead of ErrInvalidState.
func TestOperationsBeforeCreateAreInvalidState(t *testing.T) {
	withFakePool(t)
	b, err := machine.Get(Name)
	if err != nil {
		t.Fatal(err)
	}
	m, err := b.New(context.Background(), validSpec(), t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	dir := t.TempDir()

	ops := map[string]func() error{
		"Start":    func() error { return m.Start(ctx) },
		"Stop":     func() error { return m.Stop(ctx, false) },
		"Pause":    func() error { return m.(machine.Pauseable).Pause(ctx) },
		"Resume":   func() error { return m.(machine.Pauseable).Resume(ctx) },
		"Suspend":  func() error { return m.(machine.Suspendable).Suspend(ctx, dir) },
		"Snapshot": func() error { return m.(machine.Snapshottable).Snapshot(ctx, dir) },
		"Control": func() error {
			_, err := m.(ControlDialer).Control(ctx, 1024)
			return err
		},
	}
	for name, op := range ops {
		t.Run(name, func(t *testing.T) {
			err := op()
			if err == nil {
				t.Fatalf("%s before Create returned nil", name)
			}
			if !errors.Is(err, machine.ErrInvalidState) {
				t.Fatalf("%s before Create: got %v, want ErrInvalidState", name, err)
			}
		})
	}
}

// --- issue #9: State must not hold v.mu across the pool call -----------

// A hung VM.get_power_state must not block every other operation on the
// machine. The call you reach for when the pool has stopped answering is
// exactly the one that would be queued behind it.
func TestStateDoesNotHoldLockAcrossPoolCall(t *testing.T) {
	m, f := newMachine(t)
	markCreated(t, m, f, "vm-1")

	entered, release := make(chan struct{}), make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseOnce)
	f.powerHook = func() {
		close(entered)
		<-release
	}

	stateDone := make(chan struct{})
	go func() {
		defer close(stateDone)
		_, _ = m.State(context.Background())
	}()
	<-entered // VMPowerState is now in flight and will not return yet.

	// Snapshot goes through liveRef, which takes v.mu. It touches nothing
	// else the State call touches, so if it blocks it blocks on the mutex.
	snapDone := make(chan error, 1)
	go func() { snapDone <- m.(machine.Snapshottable).Snapshot(context.Background(), t.TempDir()) }()

	select {
	case err := <-snapDone:
		if err != nil {
			t.Errorf("Snapshot: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("Snapshot blocked while State was waiting on the pool: " +
			"State holds v.mu across VMPowerState")
	}

	releaseOnce()
	<-stateDone
}

// --- issue #10: Destroy must not run teardown when Create never ran ----

// New() then Destroy() — an abandoned setup, a failed Create, a test — must
// not reach teardown with an empty ref. It is also the one route into the
// session leak that can be closed: with no pool-side resources to remove,
// Destroy has nothing to fail at and can log out.
func TestDestroyWithoutCreateSkipsTeardown(t *testing.T) {
	m, f := newMachine(t)

	if err := m.Destroy(context.Background()); err != nil {
		t.Fatalf("Destroy on a machine that was never created: %v", err)
	}
	for _, c := range f.calls {
		if c != "Close" {
			t.Errorf("teardown reached the pool for a machine that was never "+
				"created: calls=%v", f.calls)
			break
		}
	}
	if !f.closed {
		t.Error("pool session was not released; nothing was created, so there " +
			"is nothing for a retry to come back to")
	}
	st, err := m.State(context.Background())
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if st != machine.StateDestroyed {
		t.Errorf("State = %v after a successful Destroy, want %v", st, machine.StateDestroyed)
	}
}

// --- issue #3: Restore must leave the machine addressable --------------

// A machine restored from a pointer file must be usable afterwards. Before
// the fix, Restore resumed the recorded ref but left v.ref empty, so every
// later call drove a different VM from the one that was actually resumed.
func TestRestoreAdoptsThePointerRef(t *testing.T) {
	// The machine that suspends.
	m, f := newMachine(t)
	markCreated(t, m, f, "vm-1")
	dir := t.TempDir()
	if err := m.(machine.Suspendable).Suspend(context.Background(), dir); err != nil {
		t.Fatalf("Suspend: %v", err)
	}

	// A second machine, standing in for a later process, restores it.
	b, err := machine.Get(Name)
	if err != nil {
		t.Fatal(err)
	}
	m2, err := b.New(context.Background(), validSpec(), t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := m2.(machine.Snapshottable).Restore(context.Background(), dir); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	st, err := m2.State(context.Background())
	if err != nil {
		t.Fatalf("State after Restore: %v", err)
	}
	if st != machine.StateRunning {
		t.Fatalf("State after Restore = %v, want %v: the restored machine is not "+
			"addressable, so v.ref was left stale", st, machine.StateRunning)
	}
	if err := m2.Stop(context.Background(), false); err != nil {
		t.Fatalf("Stop after Restore: %v", err)
	}
	if got := f.power["vm-1"]; got != PowerHalted {
		t.Fatalf("vm-1 power = %v after Stop, want %v: the restored machine drove "+
			"a different ref from the one it resumed", got, PowerHalted)
	}
}

// Restore repoints the machine at whatever the pointer file names. Doing
// that to a machine that already has a ref would orphan the VM it held, and
// doing it after Destroy would resurrect a machine the caller has finished
// with. Both are refused, the same way Create refuses to run after Destroy.
func TestRestoreRefusesNonEmptyLifecycle(t *testing.T) {
	dir := t.TempDir()
	if err := writePointer(dir, pointer{Kind: "suspend", VM: "vm-1"}); err != nil {
		t.Fatal(err)
	}

	t.Run("after Create", func(t *testing.T) {
		m, f := newMachine(t)
		markCreated(t, m, f, "vm-2")
		err := m.(machine.Snapshottable).Restore(context.Background(), dir)
		if !errors.Is(err, machine.ErrInvalidState) {
			t.Fatalf("got %v, want ErrInvalidState", err)
		}
	})

	t.Run("after Destroy", func(t *testing.T) {
		m, _ := newMachine(t)
		if err := m.Destroy(context.Background()); err != nil {
			t.Fatalf("Destroy: %v", err)
		}
		err := m.(machine.Snapshottable).Restore(context.Background(), dir)
		if !errors.Is(err, machine.ErrInvalidState) {
			t.Fatalf("got %v, want ErrInvalidState", err)
		}
	})
}

// Restore checks the lifecycle, drops v.mu for the pool call, then commits.
// A Destroy landing in that window saw created == false, skipped teardown,
// marked the machine destroyed and logged the session out — and then Restore
// committed ref and created on top of it. That leaves a VM resumed on the
// pool that no later Destroy will ever tear down, while State reports
// StateDestroyed. Found by review on PR #12.
//
// The deadlock-free fix cannot be "hold the lock across the resume": that is
// the defect #9 removed from State. Restore reserves the transition instead,
// and Destroy refuses while it is held.
func TestDestroyDuringRestoreCannotInterleave(t *testing.T) {
	dir := t.TempDir()
	if err := writePointer(dir, pointer{Kind: "suspend", VM: "vm-1"}); err != nil {
		t.Fatal(err)
	}
	m, f := newMachine(t)

	entered, release := make(chan struct{}), make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseOnce)
	f.resumeHook = func() {
		close(entered)
		<-release
	}

	restoreDone := make(chan error, 1)
	go func() { restoreDone <- m.(machine.Snapshottable).Restore(context.Background(), dir) }()
	<-entered // the resume is in flight; Restore has released v.mu.

	destroyErr := m.Destroy(context.Background())
	releaseOnce()
	restoreErr := <-restoreDone

	// Exactly one of the two may win. What must never happen is both
	// "succeeding" into a machine that is destroyed and created at once.
	if destroyErr == nil && restoreErr == nil {
		t.Fatal("Destroy and Restore both reported success: the machine is " +
			"marked destroyed and created at the same time, and the resumed VM " +
			"is unreachable for teardown")
	}
	if destroyErr != nil && !errors.Is(destroyErr, machine.ErrInvalidState) {
		t.Fatalf("Destroy during Restore: got %v, want ErrInvalidState", destroyErr)
	}

	st, err := m.State(context.Background())
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if restoreErr == nil && st == machine.StateDestroyed {
		t.Fatal("Restore succeeded but State reports StateDestroyed")
	}
}

// Two concurrent Restores must not both pass the lifecycle check and resume
// different pointer targets, with the later commit silently overwriting
// v.ref and orphaning the first VM.
func TestConcurrentRestoreRejectsTheSecond(t *testing.T) {
	dirA, dirB := t.TempDir(), t.TempDir()
	if err := writePointer(dirA, pointer{Kind: "suspend", VM: "vm-a"}); err != nil {
		t.Fatal(err)
	}
	if err := writePointer(dirB, pointer{Kind: "suspend", VM: "vm-b"}); err != nil {
		t.Fatal(err)
	}
	m, f := newMachine(t)

	entered, release := make(chan struct{}), make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseOnce)
	// Only the first resume blocks. sync.Once is wrong here: Do makes later
	// callers wait for the first to return, so the second Restore would block
	// inside the hook instead of reaching the lifecycle check being tested.
	var resumes atomic.Int32
	f.resumeHook = func() {
		if resumes.Add(1) == 1 {
			close(entered)
			<-release
		}
	}

	first := make(chan error, 1)
	go func() { first <- m.(machine.Snapshottable).Restore(context.Background(), dirA) }()
	<-entered

	second := m.(machine.Snapshottable).Restore(context.Background(), dirB)
	releaseOnce()
	firstErr := <-first

	if firstErr == nil && second == nil {
		t.Fatal("both Restores succeeded; the second overwrote v.ref and " +
			"orphaned the VM the first one resumed")
	}
	if second != nil && !errors.Is(second, machine.ErrInvalidState) {
		t.Fatalf("second Restore: got %v, want ErrInvalidState", second)
	}
}
