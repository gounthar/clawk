package vz

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBalloonTarget(t *testing.T) {
	const gib = 1024 * 1024 * 1024
	tests := []struct {
		name  string
		level pressureLevel
		full  uint64
		want  uint64
	}{
		{
			name:  "normal restores full",
			level: pressureNormal,
			full:  4 * gib,
			want:  4 * gib,
		},
		{
			name:  "warn gives back a quarter",
			level: pressureWarn,
			full:  4 * gib,
			want:  3 * gib,
		},
		{
			name:  "critical gives back half",
			level: pressureCritical,
			full:  4 * gib,
			want:  2 * gib,
		},
		{
			name:  "critical clamps to floor",
			level: pressureCritical,
			full:  768 * 1024 * 1024, // half would be 384 MiB, below the floor
			want:  minBalloonFloorBytes,
		},
		{
			name:  "warn clamps to floor",
			level: pressureWarn,
			full:  640 * 1024 * 1024, // 3/4 = 480 MiB, below the floor
			want:  minBalloonFloorBytes,
		},
		{
			name:  "guest at or below floor is left untouched",
			level: pressureCritical,
			full:  minBalloonFloorBytes,
			want:  minBalloonFloorBytes,
		},
		{
			name:  "tiny guest is never inflated below its own size",
			level: pressureCritical,
			full:  256 * 1024 * 1024,
			want:  256 * 1024 * 1024,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := balloonTarget(tt.level, tt.full)
			require.Equal(t, tt.want, got, "balloonTarget(%s, %d)", tt.level, tt.full)
		})
	}
}

func TestBalloonTargetNeverExceedsFull(t *testing.T) {
	const full = 2 * 1024 * 1024 * 1024
	for _, level := range []pressureLevel{pressureNormal, pressureWarn, pressureCritical} {
		got := balloonTarget(level, full)
		require.LessOrEqual(t, got, uint64(full), "balloonTarget(%s, %d) exceeds full size", level, full)
	}
}

func TestGuestDesiredTarget(t *testing.T) {
	const (
		mib      = 1024 * 1024
		gib      = 1024 * mib
		baseline = 1 * gib
		ceiling  = 4 * gib
		totalKiB = ceiling / 1024 // guest boots at the ceiling
	)
	tests := []struct {
		name string
		cur  uint64
		r    memReport
		// swapPressure is swapTrend.observe's verdict — swap in use and
		// recently growing. Its own logic is covered by TestSwapTrend; here it
		// is an input, so these cases pin what the target does with it.
		swapPressure bool
		want         uint64
	}{
		{
			name: "no report holds current",
			cur:  2 * gib,
			r:    memReport{}, // TotalKiB == 0
			want: 2 * gib,
		},
		{
			name: "high PSI grows straight to ceiling",
			cur:  baseline,
			r:    memReport{TotalKiB: totalKiB, AvailableKiB: totalKiB / 2, PSIMemSomeCenti: psiDemandCenti},
			want: ceiling,
		},
		{
			name: "low slack grows to ceiling",
			cur:  baseline,
			r:    memReport{TotalKiB: totalKiB, AvailableKiB: totalKiB / 16}, // ~6% available < 12.5%
			want: ceiling,
		},
		{
			name: "high slack reclaims one step",
			cur:  ceiling,
			r:    memReport{TotalKiB: totalKiB, AvailableKiB: totalKiB / 2}, // 50% available > 40%
			want: ceiling - reclaimStepBytes,
		},
		{
			name: "high slack near baseline snaps to baseline",
			cur:  baseline + reclaimStepBytes/2,
			r:    memReport{TotalKiB: totalKiB, AvailableKiB: totalKiB / 2},
			want: baseline,
		},
		{
			name: "hysteresis band holds current",
			cur:  2 * gib,
			r:    memReport{TotalKiB: totalKiB, AvailableKiB: totalKiB / 4}, // 25% available, between 12.5% and 40%
			want: 2 * gib,
		},
		{
			name: "no burst headroom collapses to baseline",
			cur:  baseline,
			r:    memReport{TotalKiB: baseline / 1024, AvailableKiB: baseline / 1024 / 2},
			want: baseline, // ceiling == baseline here
		},
		// The slack a paging guest shows is manufactured: it evicted to produce
		// it. Reclaiming on the strength of it is the feedback loop that parks
		// a working guest at its baseline, swapping, forever.
		{
			name: "high slack with swap pressure holds instead of reclaiming",
			cur:  ceiling,
			r: memReport{
				TotalKiB: totalKiB, AvailableKiB: totalKiB / 2, // 50% available > 40%
				SwapTotalKiB: 4 << 20, SwapFreeKiB: 4<<20 - swapUsedFloorKiB,
			},
			swapPressure: true,
			want:         ceiling,
		},
		// Occupancy without pressure is history, not demand: once the trend has
		// gone quiet the headroom is real and the guest gives memory back. This
		// is what stops one early spike retiring reclaim for the sandbox's life.
		{
			name: "swap held but no longer growing reclaims again",
			cur:  ceiling,
			r: memReport{
				TotalKiB: totalKiB, AvailableKiB: totalKiB / 2,
				SwapTotalKiB: 4 << 20, SwapFreeKiB: 4<<20 - 2*swapUsedFloorKiB,
			},
			swapPressure: false,
			want:         ceiling - reclaimStepBytes,
		},
		// Swap pressure suppresses reclaim, never growth: a guest that is both
		// paging and stalling still needs the ceiling.
		{
			name: "swap pressure does not block growth on demand",
			cur:  baseline,
			r: memReport{
				TotalKiB: totalKiB, AvailableKiB: totalKiB / 2, PSIMemSomeCenti: psiDemandCenti,
				SwapTotalKiB: 4 << 20, SwapFreeKiB: 0,
			},
			swapPressure: true,
			want:         ceiling,
		},
		// An untouched swap device says nothing at all — a guest that never
		// swapped is as reclaimable as one with no swap device.
		{
			name: "swap present but unused reclaims normally",
			cur:  ceiling,
			r: memReport{
				TotalKiB: totalKiB, AvailableKiB: totalKiB / 2,
				SwapTotalKiB: 4 << 20, SwapFreeKiB: 4 << 20,
			},
			want: ceiling - reclaimStepBytes,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := uint64(ceiling)
			if tt.name == "no burst headroom collapses to baseline" {
				c = baseline
			}
			got := guestDesiredTarget(tt.cur, baseline, c, tt.r, tt.swapPressure)
			require.Equal(t, tt.want, got)
		})
	}
}

// TestSwapTrend covers the signal that decides swapPressure. The property that
// matters is that it DECAYS: swap occupancy latches (a slot is freed only when
// its page is faulted back in or its owner exits), so reading the raw level as
// pressure pinned a guest that spiked once and then idled, and retired the
// reclaim path for the rest of its life.
func TestSwapTrend(t *testing.T) {
	const total = 4 << 20 // 4 GiB of swap, in KiB

	used := func(kib uint64) memReport {
		return memReport{TotalKiB: 4 << 20, AvailableKiB: 2 << 20,
			SwapTotalKiB: total, SwapFreeKiB: total - kib}
	}

	t.Run("below the floor is no signal", func(t *testing.T) {
		var s swapTrend
		for i := 0; i < 3; i++ {
			require.False(t, s.observe(used(swapUsedFloorKiB-1)))
		}
	})

	t.Run("no swap device at all is no signal", func(t *testing.T) {
		var s swapTrend
		require.False(t, s.observe(memReport{TotalKiB: 4 << 20, AvailableKiB: 2 << 20}))
	})

	t.Run("crossing the floor starts a hold", func(t *testing.T) {
		var s swapTrend
		require.True(t, s.observe(used(swapUsedFloorKiB)))
	})

	t.Run("hold decays once growth stops", func(t *testing.T) {
		var s swapTrend
		require.True(t, s.observe(used(2*swapUsedFloorKiB)), "first sighting holds")
		// Same occupancy, report after report: the guest evicted and moved on.
		for i := 1; i < swapQuietReportsBeforeReclaim; i++ {
			require.True(t, s.observe(used(2*swapUsedFloorKiB)),
				"quiet report %d should still hold", i)
		}
		require.False(t, s.observe(used(2*swapUsedFloorKiB)),
			"past the quiet window the occupancy is history, not pressure")
		require.False(t, s.observe(used(2*swapUsedFloorKiB)), "and stays decayed")
	})

	t.Run("renewed growth restarts the hold", func(t *testing.T) {
		var s swapTrend
		s.observe(used(2 * swapUsedFloorKiB))
		for i := 0; i < swapQuietReportsBeforeReclaim+5; i++ {
			s.observe(used(2 * swapUsedFloorKiB))
		}
		require.False(t, s.observe(used(2*swapUsedFloorKiB)), "decayed first")
		require.True(t, s.observe(used(3*swapUsedFloorKiB)),
			"growing again means the guest is being squeezed now")
	})

	t.Run("dropping below the floor resets the quiet count", func(t *testing.T) {
		var s swapTrend
		s.observe(used(2 * swapUsedFloorKiB))
		for i := 0; i < swapQuietReportsBeforeReclaim+5; i++ {
			s.observe(used(2 * swapUsedFloorKiB))
		}
		require.False(t, s.observe(used(2*swapUsedFloorKiB)), "decayed")
		// Pages came back in and the guest fell under the floor; a later rise
		// must not inherit the stale quiet count.
		require.False(t, s.observe(used(0)))
		require.True(t, s.observe(used(2*swapUsedFloorKiB)))
	})

	t.Run("shrinking swap counts as quiet", func(t *testing.T) {
		var s swapTrend
		require.True(t, s.observe(used(10*swapUsedFloorKiB)))
		// Faulting pages back in is not eviction pressure.
		for i := 1; i < swapQuietReportsBeforeReclaim; i++ {
			require.True(t, s.observe(used(uint64(10-i/4)*swapUsedFloorKiB)))
		}
		require.False(t, s.observe(used(5*swapUsedFloorKiB)))
	})
}

func TestGuestDesiredTargetStaysInRange(t *testing.T) {
	const (
		gib      = 1024 * 1024 * 1024
		baseline = 1 * gib
		ceiling  = 4 * gib
	)
	reports := []memReport{
		{},
		{TotalKiB: ceiling / 1024, AvailableKiB: 0, PSIMemSomeCenti: 9999},
		{TotalKiB: ceiling / 1024, AvailableKiB: ceiling / 1024},
		{TotalKiB: ceiling / 1024, AvailableKiB: ceiling / 1024 / 4},
	}
	for _, cur := range []uint64{baseline, 2 * gib, ceiling} {
		for _, r := range reports {
			got := guestDesiredTarget(cur, baseline, ceiling, r, false)
			require.GreaterOrEqual(t, got, uint64(baseline))
			require.LessOrEqual(t, got, uint64(ceiling))
		}
	}
}

func TestMergedBalloonTargetHostPressureWins(t *testing.T) {
	const (
		gib      = 1024 * 1024 * 1024
		baseline = 1 * gib
		ceiling  = 4 * gib
	)
	// Guest is starved and wants the full ceiling.
	starved := memReport{TotalKiB: ceiling / 1024, AvailableKiB: 0, PSIMemSomeCenti: 9999}

	// Under normal pressure the guest gets what it asks for.
	require.Equal(t, uint64(ceiling),
		mergedBalloonTarget(pressureNormal, baseline, baseline, ceiling, starved, false))

	// Under WARN the host caps growth at balloonTarget(warn, ceiling) even
	// though the guest is starving.
	wantWarn := balloonTarget(pressureWarn, ceiling)
	require.Equal(t, wantWarn,
		mergedBalloonTarget(pressureWarn, baseline, baseline, ceiling, starved, false))
	require.Less(t, wantWarn, uint64(ceiling))

	// Under CRITICAL it caps even lower.
	wantCrit := balloonTarget(pressureCritical, ceiling)
	require.Equal(t, wantCrit,
		mergedBalloonTarget(pressureCritical, baseline, baseline, ceiling, starved, false))
	require.Less(t, wantCrit, wantWarn)
}
