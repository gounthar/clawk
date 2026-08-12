package vz

import "fmt"

// pressureLevel is the host memory-pressure level as reported by the XNU
// memorystatus subsystem (the same signal that drives jetsam and
// DISPATCH_SOURCE_TYPE_MEMORYPRESSURE).
type pressureLevel int

const (
	pressureNormal pressureLevel = iota
	pressureWarn
	pressureCritical
)

func (p pressureLevel) String() string {
	switch p {
	case pressureNormal:
		return "normal"
	case pressureWarn:
		return "warn"
	case pressureCritical:
		return "critical"
	default:
		return fmt.Sprintf("pressureLevel(%d)", int(p))
	}
}

// minBalloonFloorBytes is the smallest target we will ever balloon a guest
// down to. Below this a Linux guest running the clawk agent thrashes its own
// OOM killer to no benefit, so we stop reclaiming there and let the host shed
// load elsewhere (or, ultimately, jetsam this VM — which is the acceptable
// outcome we are steering toward instead of a launchd SIGBUS panic).
const minBalloonFloorBytes = 512 * 1024 * 1024

// balloonTarget returns the guest's target memory size in bytes for a given
// host memory-pressure level, where fullBytes is the guest's configured
// (boot) memory. The target is how much memory the guest is allowed to use;
// setting it below fullBytes inflates the virtio balloon, handing the
// difference back to the host. pressureNormal restores the full size
// (balloon fully deflated).
//
// The fractions are deliberately gentle: WARN gives back ~25% and CRITICAL
// ~50%. With several guests live, even the WARN tier reclaims gigabytes —
// enough to relieve the host before the kernel has to fault an unkillable
// process like launchd. Reclaiming is never taken below minBalloonFloorBytes.
func balloonTarget(level pressureLevel, fullBytes uint64) uint64 {
	switch level {
	case pressureWarn:
		return clampFloor(fullBytes/4*3, fullBytes)
	case pressureCritical:
		return clampFloor(fullBytes/2, fullBytes)
	default: // pressureNormal
		return fullBytes
	}
}

// clampFloor keeps target at or above minBalloonFloorBytes, but never raises it
// above full (a guest configured with less than the floor is left untouched).
func clampFloor(target, full uint64) uint64 {
	if full <= minBalloonFloorBytes {
		return full
	}
	if target < minBalloonFloorBytes {
		return minBalloonFloorBytes
	}
	return target
}

// Guest-demand policy.
//
// On top of the host-pressure floor (balloonTarget), a guest configured with
// a baseline below its ceiling is held near the baseline while idle and
// allowed to grow toward the ceiling on demand. Demand is read from the guest
// itself (memReport over vsock) because Apple's balloon reports no guest stats
// to the host. Growth is aggressive (jump straight to the ceiling the moment
// the guest looks tight) so the guest never sits in OOM-thrash waiting for the
// next poll; reclaim is gentle (step down) so we never yank back memory the
// guest just started using.
const (
	// psiDemandCenti is the memory PSI "some avg10" (×100) at or above which
	// we treat the guest as memory-starved and grant the full ceiling. 5%
	// of wall-time stalled on memory in the last 10 s is already a workload
	// that wants more RAM.
	psiDemandCenti = 500

	// lowSlackNum/lowSlackDen express the MemAvailable/MemTotal ratio below
	// which the guest is "tight" and should grow: 1/8 = 12.5% available.
	lowSlackNum, lowSlackDen = 1, 8

	// highSlackNum/highSlackDen express the ratio above which the guest is
	// "roomy" and idle memory can be reclaimed: 2/5 = 40% available. The gap
	// between low and high is the hysteresis band in which we hold steady.
	highSlackNum, highSlackDen = 2, 5

	// swapUsedFloorKiB is how much swap a guest must be holding before we
	// stop believing that its free memory means it has memory to spare.
	//
	// Sandboxes ship with a swap device, and that changes what the two
	// signals above mean. Swapping out cold anonymous pages frees them —
	// MemAvailable rises — and removes the reclaim stalls PSI measures, so a
	// guest that answered a squeeze by paging out reads as roomy on both
	// counts. Reclaiming another step there is how a guest ends up living at
	// its baseline, swapping, instead of growing to the ceiling it was given.
	//
	// 64 MiB is above the incidental few megabytes a long-lived guest pages
	// out and never touches again, and far below anything that indicates real
	// pressure.
	//
	// Occupancy alone is not enough to act on, though — see swapTrend.
	swapUsedFloorKiB = 64 * 1024

	// swapQuietReportsBeforeReclaim is how many consecutive guest reports may
	// show swap at or above the floor WITHOUT growing before the controller
	// stops reading it as pressure.
	//
	// Occupancy latches: a swap slot is released only when its page is faulted
	// back in or its owner exits, so a guest that paged out cold anonymous
	// memory once reports the same number forever. With GuestSwappiness at 80
	// that is an ordinary thing for a long-lived sandbox to do — one big link
	// step early on — and holding on it indefinitely would retire the reclaim
	// path for the rest of that sandbox's life on a signal that never decays.
	//
	// Growth is the part that means "being squeezed right now". 24 reports at
	// memPollInterval (5s) is two minutes of quiet: long enough that a guest
	// still working through a squeeze keeps the hold, short enough that a spike
	// from an hour ago stops speaking for a sandbox that has been idle since.
	//
	// Guests that are actively thrashing are not this signal's job. Paging in
	// and out at the same rate holds occupancy flat, so the trend reads quiet —
	// but that case is exactly what elevated PSI describes, and it grows the
	// guest to the ceiling two branches earlier in guestDesiredTarget.
	swapQuietReportsBeforeReclaim = 24
)

// swapTrend tracks swap occupancy across guest reports, so the controller can
// tell a guest paging out right now from one that has old cold pages parked in
// swap. One per VM, owned by runBalloonController's goroutine — no lock.
type swapTrend struct {
	prevUsedKiB uint64
	quiet       int
}

// observe folds one guest report in and reports whether swap use should still
// count as evidence of memory pressure.
//
// Call it once per REPORT, not once per balloon re-evaluation. Growth is only
// observable between two distinct reports, so re-running it on the same report
// (as the reeval ticker would) counts as quiet and would decay the hold in
// seconds instead of minutes. The controller keeps the verdict between reports,
// exactly as it already keeps the report itself.
func (s *swapTrend) observe(r memReport) bool {
	used := r.SwapUsedKiB()
	// Below the floor is the same "no signal" as a guest with no swap device,
	// or one whose agent is too old to report any — reset, so a later rise
	// starts a fresh hold rather than inheriting a stale quiet count.
	if used < swapUsedFloorKiB {
		s.prevUsedKiB, s.quiet = used, 0
		return false
	}
	if used > s.prevUsedKiB {
		s.quiet = 0 // still paging out
	} else {
		s.quiet++
	}
	s.prevUsedKiB = used
	return s.quiet < swapQuietReportsBeforeReclaim
}

// reclaimStepBytes is how much we inflate per reclaim step when the guest is
// idle. Gentle by design — a step at a time avoids reclaiming a large chunk
// the instant a guest goes briefly quiet, only to deflate it again moments
// later. 128 MiB converges an idle 4 GiB guest to its baseline in a handful
// of poll intervals.
const reclaimStepBytes = 128 * 1024 * 1024

// guestDesiredTarget returns the balloon target the guest's own memory state
// argues for, in the closed range [baseline, ceiling], given the current
// target cur (used for gentle stepping and hysteresis). It ignores host
// pressure; mergedBalloonTarget layers that on top.
//
// A zero report (TotalKiB == 0) means "no fresh guest data" — the caller is
// responsible for not constraining the guest in that case; here we simply hold
// cur clamped into range.
// swapPressure is swapTrend.observe's latest verdict for this guest: swap is
// in use AND recently growing. It is a parameter rather than something derived
// from r because the judgement needs history across reports, which this
// deliberately-pure function has none of.
func guestDesiredTarget(cur, baseline, ceiling uint64, r memReport, swapPressure bool) uint64 {
	if ceiling <= baseline || r.TotalKiB == 0 {
		return clampRange(cur, baseline, ceiling)
	}
	// Grow to the ceiling when the guest is stalling on memory or has little
	// reclaimable headroom left.
	if r.PSIMemSomeCenti >= psiDemandCenti ||
		r.AvailableKiB*uint64(lowSlackDen) < r.TotalKiB*uint64(lowSlackNum) {
		return ceiling
	}
	// A guest that is paging out is not idle, whatever its slack says: it is
	// being squeezed hard enough to evict, and its headroom is the product of
	// that, not evidence it was never needed. Hold instead of reclaiming —
	// growth stays available through the branch above, which fires as soon as
	// it stalls or runs genuinely tight.
	if swapPressure {
		return clampRange(cur, baseline, ceiling)
	}
	// Reclaim a step toward the baseline when the guest is roomy: high slack
	// means the guest genuinely isn't using the memory, so handing it back to
	// the host is exactly right.
	if r.AvailableKiB*uint64(highSlackDen) > r.TotalKiB*uint64(highSlackNum) {
		if cur <= baseline+reclaimStepBytes {
			return baseline
		}
		return cur - reclaimStepBytes
	}
	// Within the hysteresis band: hold.
	return clampRange(cur, baseline, ceiling)
}

// mergedBalloonTarget combines the guest's demand-driven target with the
// host-pressure floor. The guest may ask to grow up to the ceiling, but host
// pressure caps how much memory the guest is allowed to hold: under WARN/
// CRITICAL the cap drops to balloonTarget's fractions, reclaiming RAM for the
// host even against guest demand (the guest's DEFLATE_ON_OOM is its safety
// net). The result is always min(guest-desired, host-allowed).
// Note that swapPressure only ever suppresses the guest's own voluntary
// reclaim; it is not a veto over the host. A guest holding swap still gets
// clipped to 3/4 (WARN) or 1/2 (CRITICAL) of its ceiling, because the host
// needing RAM outranks any read of what the guest would prefer.
func mergedBalloonTarget(level pressureLevel, cur, baseline, ceiling uint64, r memReport, swapPressure bool) uint64 {
	desired := guestDesiredTarget(cur, baseline, ceiling, r, swapPressure)
	allowed := balloonTarget(level, ceiling)
	if desired > allowed {
		return allowed
	}
	return desired
}

// clampRange constrains v to [lo, hi]. When lo > hi (a guest with no burst
// headroom configured, baseline == ceiling) it collapses to lo.
func clampRange(v, lo, hi uint64) uint64 {
	if hi < lo {
		return lo
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
