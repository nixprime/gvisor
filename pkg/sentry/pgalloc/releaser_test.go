// Copyright 2026 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build !pagesize_64k

package pgalloc

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"runtime"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/memutil"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/usage"
	"gvisor.dev/gvisor/pkg/sync"
)

// mockReleaserClock provides a deterministic monotonic clock for releaserMain.
//
// Lock order: MemoryFile.mu -> mockReleaserClock.mu.
type mockReleaserClock struct {
	mu sync.Mutex
	// +checklocks:mu
	now int64
	// +checklocks:mu
	releaseOverhead int64
	// +checklocks:mu
	sleeping bool
	// +checklocks:mu
	releasing bool
	// +checklocks:mu
	total int
	// +checklocks:mu
	sleeps int
	// +checklocks:mu
	releases int
	// +checklocks:mu
	observedSleeps int
	// +checklocks:mu
	lastWokenSleeps int
}

func installMockReleaserClock(t *testing.T) *mockReleaserClock {
	t.Helper()
	orig := releaserNanotime
	c := &mockReleaserClock{
		now: int64(time.Second),
	}
	releaserNanotime = c.nanotime
	t.Cleanup(func() {
		releaserNanotime = orig
	})
	return c
}

func (c *mockReleaserClock) nanotime(ctx releaserNanotimeContext) int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.total++
	switch ctx {
	case releaserNanotimeBeforeSleep:
		if c.sleeping || c.releasing {
			panic(fmt.Sprintf("releaserNanotimeBeforeSleep while sleeping=%t releasing=%t", c.sleeping, c.releasing))
		}
		c.sleeping = true
		c.sleeps++
	case releaserNanotimeAfterSleep:
		if !c.sleeping || c.releasing {
			panic(fmt.Sprintf("releaserNanotimeAfterSleep while sleeping=%t releasing=%t", c.sleeping, c.releasing))
		}
		c.sleeping = false
		c.lastWokenSleeps = c.sleeps
	case releaserNanotimeBeforeRelease:
		if c.sleeping || c.releasing {
			panic(fmt.Sprintf("releaserNanotimeBeforeRelease while sleeping=%t releasing=%t", c.sleeping, c.releasing))
		}
		c.releasing = true
	case releaserNanotimeAfterRelease:
		if c.sleeping || !c.releasing {
			panic(fmt.Sprintf("releaserNanotimeAfterRelease while sleeping=%t releasing=%t", c.sleeping, c.releasing))
		}
		c.releasing = false
		c.releases++
		c.now += c.releaseOverhead
	default:
		panic(fmt.Sprintf("unknown releaserNanotimeContext: %v", ctx))
	}
	return c.now
}

func (c *mockReleaserClock) setReleaseOverhead(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.releaseOverhead = d.Nanoseconds()
}

func (c *mockReleaserClock) totalCalls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.total
}

func (c *mockReleaserClock) counts() (sleeps, releases int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sleeps, c.releases
}

func (c *mockReleaserClock) isBusy() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sleeping || c.releasing
}

func (c *mockReleaserClock) syncSeenSleeps() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.observedSleeps = c.sleeps
	c.lastWokenSleeps = c.sleeps
}

func destroyAndWait(f *MemoryFile) {
	f.Destroy()
	f.mu.Lock()
	for f.file != nil {
		f.mu.Unlock()
		runtime.Gosched()
		f.mu.Lock()
	}
	f.mu.Unlock()
}

type releaserTestEnv struct {
	t     *testing.T
	clock *mockReleaserClock
	f     *MemoryFile
}

func newReleaserTestEnv(t *testing.T, opts MemoryFileOpts) *releaserTestEnv {
	t.Helper()
	opts.MemoryAccounting = MemoryAccountingDisabled
	return newReleaserTestEnvWithAccounting(t, opts)
}

func newReleaserTestEnvWithAccounting(t *testing.T, opts MemoryFileOpts) *releaserTestEnv {
	t.Helper()
	if opts.MemoryAccounting != MemoryAccountingDisabled {
		if err := usage.Init(); err != nil {
			t.Fatalf("usage.Init: %v", err)
		}
	}
	clock := installMockReleaserClock(t)
	if opts.DelayedEviction == DelayedEvictionDefault {
		opts.DelayedEviction = DelayedEvictionDisabled
	}
	fd, err := memutil.CreateMemFD("pgalloc-releaser-test", 0)
	if err != nil {
		t.Fatalf("CreateMemFD: %v", err)
	}
	file := os.NewFile(uintptr(fd), "pgalloc-releaser-test")
	f, err := NewMemoryFile(file, opts)
	if err != nil {
		_ = file.Close()
		t.Fatalf("NewMemoryFile: %v", err)
	}
	env := &releaserTestEnv{
		t:     t,
		clock: clock,
		f:     f,
	}
	t.Cleanup(env.destroy)
	env.syncIdleReleaser()
	return env
}

func (e *releaserTestEnv) destroy() {
	if e.f == nil {
		return
	}
	f := e.f
	e.f = nil
	destroyAndWait(f)
}

// syncIdleReleaser drains any stale notification in f.releaserC and verifies a
// round-trip through releaserMain's idle loop so releaserMain is parked in
// releaserWait(nil) with an empty releaserC.
//
// Preconditions: f has no waste pages and no pending evictions.
func (e *releaserTestEnv) syncIdleReleaser() {
	e.t.Helper()
	deadline := time.Now().Add(5 * time.Second)

	// Step 1: While holding f.mu, wait until releaserMain consumes a token from
	// f.releaserC (proving it reached releaserWait(nil) and is now blocked on
	// f.mu.Lock() with releaserC empty). If an earlier idle turn was already
	// blocked on f.mu.Lock(), let it run and retry.
	e.f.mu.Lock()
	for {
		if e.f.wastePages != 0 || len(e.f.evictable) != 0 {
			wp := e.f.wastePages
			ev := len(e.f.evictable)
			e.f.mu.Unlock()
			e.t.Fatalf("syncIdleReleaser precondition violated: wastePages=%d, evictable=%d", wp, ev)
		}
		for len(e.f.releaserC) > 0 {
			<-e.f.releaserC
		}
		e.f.releaserC <- struct{}{}
		consumed := false
		for range 64 {
			if len(e.f.releaserC) == 0 {
				consumed = true
				break
			}
			runtime.Gosched()
		}
		if consumed {
			break
		}
		for len(e.f.releaserC) > 0 {
			<-e.f.releaserC
		}
		e.f.mu.Unlock()
		if time.Now().After(deadline) {
			e.t.Fatal("timed out waiting for releaser to block at releaserWait(nil)")
		}
		runtime.Gosched()
		e.f.mu.Lock()
	}

	// Step 2: Set maxWastePages = 1 with releaserC empty, unlock f.mu, and wait
	// for releaserMain to reset maxWastePages = 0 and park in releaserWait(nil).
	e.f.maxWastePages = 1
	e.f.mu.Unlock()
	for {
		e.f.mu.Lock()
		settled := e.f.wastePages == 0 && e.f.maxWastePages == 0 && len(e.f.evictable) == 0 && len(e.f.releaserC) == 0
		if settled {
			e.clock.syncSeenSleeps()
		}
		e.f.mu.Unlock()
		if settled {
			return
		}
		if time.Now().After(deadline) {
			e.t.Fatal("timed out waiting for releaser to park after idle round-trip")
		}
		runtime.Gosched()
	}
}

// waitForSleep blocks until releaserMain has captured sleepStartTime for a new
// throttled sleep.
func (e *releaserTestEnv) waitForSleep() {
	e.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		e.clock.mu.Lock()
		sleeping := e.clock.sleeping
		sleeps := e.clock.sleeps
		seen := max(e.clock.observedSleeps, e.clock.lastWokenSleeps)
		ready := sleeping && sleeps > seen
		if ready {
			e.clock.observedSleeps = sleeps
		}
		e.clock.mu.Unlock()
		if ready {
			return
		}
		if time.Now().After(deadline) {
			wp, maxWP := e.wasteStats()
			e.t.Fatalf("timed out waiting for releaser sleep (sleeping=%t, sleeps=%d, seen=%d, wastePages=%d, maxWastePages=%d)", sleeping, sleeps, seen, wp, maxWP)
		}
		runtime.Gosched()
	}
}

// waitForIdle blocks until releaserMain has released all waste pages, reset
// maxWastePages to 0, completed pending evictions, and parked waiting for new
// waste with an empty releaserC.
func (e *releaserTestEnv) waitForIdle() {
	e.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		e.f.mu.Lock()
		wastePages := e.f.wastePages
		maxWastePages := e.f.maxWastePages
		evictable := len(e.f.evictable)
		busy := e.clock.isBusy()
		e.f.mu.Unlock()
		if wastePages == 0 && maxWastePages == 0 && evictable == 0 && !busy {
			e.syncIdleReleaser()
			return
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("timed out waiting for releaser idle (busy=%t, wastePages=%d, maxWastePages=%d, evictable=%d)", busy, wastePages, maxWastePages, evictable)
		}
		runtime.Gosched()
	}
}

// wakeAfter waits for the releaser to enter an unwoken throttled sleep,
// advances the mock clock by d, and wakes the releaser.
func (e *releaserTestEnv) wakeAfter(d time.Duration) {
	e.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		e.f.mu.Lock()
		pendingWake := len(e.f.releaserC) != 0
		e.clock.mu.Lock()
		sleeping := e.clock.sleeping
		sleeps := e.clock.sleeps
		lastWoken := e.clock.lastWokenSleeps
		ready := !pendingWake && sleeping && sleeps > lastWoken
		if ready {
			e.clock.now += d.Nanoseconds()
			e.clock.lastWokenSleeps = sleeps
			e.clock.observedSleeps = max(e.clock.observedSleeps, sleeps)
		}
		e.clock.mu.Unlock()
		e.f.mu.Unlock()
		if ready {
			e.f.releaserNotify()
			return
		}
		if time.Now().After(deadline) {
			wp, maxWP := e.wasteStats()
			e.t.Fatalf("timed out waiting for releaser sleep to wake (pendingWake=%t, sleeping=%t, sleeps=%d, lastWoken=%d, wastePages=%d, maxWastePages=%d)", pendingWake, sleeping, sleeps, lastWoken, wp, maxWP)
		}
		runtime.Gosched()
	}
}

func (e *releaserTestEnv) hasPendingWake() bool {
	e.f.mu.Lock()
	defer e.f.mu.Unlock()
	return len(e.f.releaserC) != 0
}

func (e *releaserTestEnv) wasteStats() (wastePages, maxWastePages uint64) {
	e.f.mu.Lock()
	defer e.f.mu.Unlock()
	return e.f.wastePages, e.f.maxWastePages
}

func (e *releaserTestEnv) knownCommittedBytes() uint64 {
	e.f.mu.Lock()
	defer e.f.mu.Unlock()
	return e.f.knownCommittedBytes
}

func (e *releaserTestEnv) allocateCommittedNonZero(pages uint64, huge bool) memmap.FileRange {
	e.t.Helper()
	length := pages << hostarch.PageShift
	fr, err := e.f.Allocate(length, AllocOpts{
		Mode: AllocateAndWritePopulate,
		Huge: huge,
	})
	if err != nil {
		e.t.Fatalf("Allocate(%d pages, huge=%t): %v", pages, huge, err)
	}
	e.f.forEachMappingSlice(fr, func(bs []byte) {
		for off := 0; off < len(bs); off += int(hostarch.PageSize) {
			bs[off] = 0x5a
		}
	})
	return fr
}

func (e *releaserTestEnv) isPageCommittedOnHost(fileOff uint64) bool {
	e.t.Helper()
	data, err := unix.Seek(e.f.FD(), int64(fileOff), unix.SEEK_DATA)
	if err == unix.ENXIO {
		return false
	}
	if err != nil {
		e.t.Fatalf("Seek(%#x, SEEK_DATA): %v", fileOff, err)
	}
	return uint64(data) == fileOff
}

func TestNewMemoryFileReleaseWindowOpts(t *testing.T) {
	t.Run("negative ReleaseWindowTicks rejected", func(t *testing.T) {
		fd, err := memutil.CreateMemFD("pgalloc-invalid-opts", 0)
		if err != nil {
			t.Fatalf("CreateMemFD: %v", err)
		}
		file := os.NewFile(uintptr(fd), "pgalloc-invalid-opts")
		defer func() { _ = file.Close() }()
		if f, err := NewMemoryFile(file, MemoryFileOpts{
			ReleaseWindow:      time.Second,
			ReleaseWindowTicks: -1,
		}); err == nil {
			destroyAndWait(f)
			t.Fatal("NewMemoryFile succeeded with negative ReleaseWindowTicks, want error")
		}
	})

	for _, tc := range []struct {
		name          string
		releaseWindow time.Duration
		maxBytes      uint64
	}{
		{name: "zero ReleaseWindow", releaseWindow: 0},
		{name: "zero ReleaseWindow with MaxBytes", releaseWindow: 0, maxBytes: uint64(hostarch.PageSize)},
		{name: "negative ReleaseWindow", releaseWindow: -time.Minute, maxBytes: uint64(hostarch.PageSize)},
		{name: "sub-page non-zero ReleaseWindowMaxBytes", releaseWindow: time.Hour, maxBytes: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newReleaserTestEnv(t, MemoryFileOpts{
				ReleaseWindow:         tc.releaseWindow,
				ReleaseWindowMaxBytes: tc.maxBytes,
			})
			fr := env.allocateCommittedNonZero(8, false)
			env.f.DecRef(fr)
			env.waitForIdle()
			if sleeps, _ := env.clock.counts(); sleeps != 0 {
				t.Errorf("releaser slept %d times when unthrottled, want 0", sleeps)
			}
			if calls := env.clock.totalCalls(); calls != 0 {
				t.Errorf("releaserNanotime called %d times when unthrottled, want 0", calls)
			}
			for p := uint64(0); p < 8; p++ {
				if off := fr.Start + p*uint64(hostarch.PageSize); env.isPageCommittedOnHost(off) {
					t.Errorf("page %d (%#x) still committed after unthrottled release", p, off)
				}
			}
		})
	}

	t.Run("ReleaseWindow capped at 24h", func(t *testing.T) {
		// Request 48h, which must be capped to 24h (maxReleaseWindowNS).
		// With 4 pages and 4 ticks, capped rate is 6h/page (1 page/tick).
		// If uncapped, sleep would require 12h instead of 6h.
		env := newReleaserTestEnv(t, MemoryFileOpts{
			ReleaseWindow:      48 * time.Hour,
			ReleaseWindowTicks: 4,
		})
		fr := env.allocateCommittedNonZero(4, false)
		env.f.DecRef(fr)

		env.waitForSleep()
		if wp, _ := env.wasteStats(); wp != 3 {
			t.Fatalf("wastePages after first tick = %d, want 3", wp)
		}
		// Advancing by 6h (24h / 4) must be enough to release the next page.
		env.wakeAfter(6 * time.Hour)
		env.waitForSleep()
		if wp, _ := env.wasteStats(); wp != 2 {
			t.Fatalf("wastePages after 6h sleep = %d, want 2 (ReleaseWindow was not capped at 24h)", wp)
		}
		env.wakeAfter(6 * time.Hour)
		env.waitForSleep()
		if wp, _ := env.wasteStats(); wp != 1 {
			t.Fatalf("wastePages after second 6h sleep = %d, want 1", wp)
		}
		env.wakeAfter(6 * time.Hour)
		env.waitForIdle()
	})
}

func TestReleaseWindowThrottling(t *testing.T) {
	t.Run("default ticks paces release from highest offsets first", func(t *testing.T) {
		// ReleaseWindow = 4h, default ReleaseWindowTicks = 4.
		// Freeing 16 pages => throttleNSPerPage = 15m, 4 pages per tick (1h per tick).
		env := newReleaserTestEnv(t, MemoryFileOpts{
			ReleaseWindow: 4 * time.Hour,
		})
		fr := env.allocateCommittedNonZero(16, false)
		env.f.DecRef(fr)

		for step, wantWaste := range []uint64{12, 8, 4} {
			env.waitForSleep()
			wp, maxWP := env.wasteStats()
			if wp != wantWaste || maxWP != 16 {
				t.Fatalf("step %d: got (wastePages=%d, maxWastePages=%d), want (%d, 16)", step, wp, maxWP, wantWaste)
			}
			// Pages [0, wantWaste) must still be committed; [wantWaste, 16) must be decommitted.
			for p := uint64(0); p < 16; p++ {
				off := fr.Start + p*uint64(hostarch.PageSize)
				wantCommitted := p < wantWaste
				if got := env.isPageCommittedOnHost(off); got != wantCommitted {
					t.Fatalf("step %d, page %d: committed=%t, want %t", step, p, got, wantCommitted)
				}
			}
			env.wakeAfter(time.Hour)
		}

		env.waitForIdle()
		for p := uint64(0); p < 16; p++ {
			off := fr.Start + p*uint64(hostarch.PageSize)
			if env.isPageCommittedOnHost(off) {
				t.Errorf("page %d still committed after full window", p)
			}
		}
	})

	t.Run("custom ticks and ceiling rounding", func(t *testing.T) {
		// 5 pages over 10h => throttleNSPerPage = 2h.
		// With 2 ticks => maxDelayNS = 5h => ceil(5h / 2h) = 3 pages in first batch (sleep 6h),
		// then remaining 2 pages in second batch.
		env := newReleaserTestEnv(t, MemoryFileOpts{
			ReleaseWindow:      10 * time.Hour,
			ReleaseWindowTicks: 2,
		})
		fr := env.allocateCommittedNonZero(5, false)
		env.f.DecRef(fr)

		env.waitForSleep()
		if wp, _ := env.wasteStats(); wp != 2 {
			t.Fatalf("wastePages after first tick = %d, want 2", wp)
		}
		// 4h (< 6h = 3 * 2h) only sleeps off 2 of the 3 throttled pages.
		env.wakeAfter(4 * time.Hour)
		env.waitForSleep()
		if wp, _ := env.wasteStats(); wp != 2 {
			t.Fatalf("wastePages after 4h partial sleep = %d, want 2", wp)
		}
		// Remaining 2h finishes throttling and releases the final 2 pages.
		env.wakeAfter(2 * time.Hour)
		env.waitForIdle()
	})

	t.Run("ticks greater than page count clamps to 1 page per tick", func(t *testing.T) {
		// 3 pages over 3h => throttleNSPerPage = 1h.
		// With 100 ticks => maxDelayNS < throttleNSPerPage => maxPages = 1.
		env := newReleaserTestEnv(t, MemoryFileOpts{
			ReleaseWindow:      3 * time.Hour,
			ReleaseWindowTicks: 100,
		})
		fr := env.allocateCommittedNonZero(3, false)
		env.f.DecRef(fr)

		env.waitForSleep()
		if wp, _ := env.wasteStats(); wp != 2 {
			t.Fatalf("wastePages = %d, want 2", wp)
		}
		env.wakeAfter(time.Hour)
		env.waitForSleep()
		if wp, _ := env.wasteStats(); wp != 1 {
			t.Fatalf("wastePages = %d, want 1", wp)
		}
		env.wakeAfter(time.Hour)
		env.waitForIdle()
	})
}

func TestReleaseWindowPartialSleepAndSpuriousWakeup(t *testing.T) {
	// 8 pages over 8h, 2 ticks => throttleNSPerPage = 1h, 4 pages/tick (4h sleep).
	env := newReleaserTestEnv(t, MemoryFileOpts{
		ReleaseWindow:      8 * time.Hour,
		ReleaseWindowTicks: 2,
	})
	fr := env.allocateCommittedNonZero(8, false)
	env.f.DecRef(fr)

	env.waitForSleep()
	if wp, _ := env.wasteStats(); wp != 4 {
		t.Fatalf("wastePages = %d, want 4", wp)
	}

	// Spurious wakeups with 0 elapsed time must still retire at least 1 page
	// each so frequent wakeups cannot stall throttling indefinitely.
	for i := range 3 {
		env.wakeAfter(0)
		env.waitForSleep()
		if wp, _ := env.wasteStats(); wp != 4 {
			t.Fatalf("after %d zero-duration wakeups: wastePages = %d, want 4", i+1, wp)
		}
	}
	// The 4th zero-duration wakeup retires the 4th throttled page and releases the remaining 4 pages.
	env.wakeAfter(0)
	env.waitForIdle()
}

func TestReleaseWindowRecycleWaste(t *testing.T) {
	t.Run("bottom-up and top-down partial recycle during release window", func(t *testing.T) {
		// 16 pages over 4h, 4 ticks => throttleNSPerPage = 15m, 4 pages/tick.
		// MemoryAccountingCommitted is required so AllocateUncommitted does not
		// recycle waste pages.
		env := newReleaserTestEnvWithAccounting(t, MemoryFileOpts{
			MemoryAccounting:   MemoryAccountingCommitted,
			ReleaseWindow:      4 * time.Hour,
			ReleaseWindowTicks: 4,
		})
		fr := env.allocateCommittedNonZero(16, false)
		env.f.DecRef(fr)

		// First 4 pages [12, 16) released; [0, 12) remain waste while throttled.
		env.waitForSleep()
		if wp, _ := env.wasteStats(); wp != 12 {
			t.Fatalf("wastePages = %d, want 12", wp)
		}

		// Non-committing allocation must NOT recycle waste pages [0, 12);
		// it uses the newly freed [12, 16) range instead.
		uncommittedFR, err := env.f.Allocate(4*uint64(hostarch.PageSize), AllocOpts{Mode: AllocateUncommitted})
		if err != nil {
			t.Fatalf("Allocate(AllocateUncommitted): %v", err)
		}
		defer env.f.DecRef(uncommittedFR)
		if uncommittedFR.Start < 12*uint64(hostarch.PageSize) {
			t.Errorf("AllocateUncommitted recycled waste range %v, want offset >= %#x", uncommittedFR, 12*uint64(hostarch.PageSize))
		}
		if wp, _ := env.wasteStats(); wp != 12 {
			t.Fatalf("wastePages after AllocateUncommitted = %d, want 12", wp)
		}

		// Bottom-up committing allocation recycles 4 waste pages [0, 4) and zeroes them.
		botFR, err := env.f.Allocate(4*uint64(hostarch.PageSize), AllocOpts{
			Mode: AllocateAndWritePopulate,
			Dir:  BottomUp,
		})
		if err != nil {
			t.Fatalf("Allocate(BottomUp): %v", err)
		}
		defer env.f.DecRef(botFR)
		wantBot := memmap.FileRange{Start: 0, End: 4 * uint64(hostarch.PageSize)}
		if botFR != wantBot {
			t.Fatalf("bottom-up recycled range = %v, want %v", botFR, wantBot)
		}

		// Top-down committing allocation recycles 4 waste pages from the high end [8, 12).
		topFR, err := env.f.Allocate(4*uint64(hostarch.PageSize), AllocOpts{
			Mode: AllocateAndWritePopulate,
			Dir:  TopDown,
		})
		if err != nil {
			t.Fatalf("Allocate(TopDown): %v", err)
		}
		defer env.f.DecRef(topFR)
		wantTop := memmap.FileRange{Start: 8 * uint64(hostarch.PageSize), End: 12 * uint64(hostarch.PageSize)}
		if topFR != wantTop {
			t.Fatalf("top-down recycled range = %v, want %v", topFR, wantTop)
		}

		for _, r := range []memmap.FileRange{botFR, topFR} {
			env.f.forEachMappingSlice(r, func(bs []byte) {
				for i, b := range bs {
					if b != 0 {
						t.Fatalf("recycled range %v not zeroed at byte %d: got %#x", r, i, b)
					}
				}
			})
		}

		// wastePages dropped from 12 to 4 ([4, 8)), while maxWastePages stays 16.
		if wp, maxWP := env.wasteStats(); wp != 4 || maxWP != 16 {
			t.Fatalf("after recycle: got (wastePages=%d, maxWastePages=%d), want (4, 16)", wp, maxWP)
		}

		// Completing the 1h sleep releases the remaining 4 waste pages [4, 8).
		env.wakeAfter(time.Hour)
		env.waitForIdle()
	})

	t.Run("full recycle while throttled and subsequent DecRef before wake", func(t *testing.T) {
		env := newReleaserTestEnv(t, MemoryFileOpts{
			ReleaseWindow:      4 * time.Hour,
			ReleaseWindowTicks: 4,
		})
		fr := env.allocateCommittedNonZero(8, false)
		extraFR := env.allocateCommittedNonZero(2, false)
		env.f.DecRef(fr)

		// 2 pages released, 6 waste pages left.
		env.waitForSleep()
		if wp, _ := env.wasteStats(); wp != 6 {
			t.Fatalf("wastePages = %d, want 6", wp)
		}

		// Recycle all 6 remaining waste pages while releaserMain is sleeping.
		recycledFR, err := env.f.Allocate(6*uint64(hostarch.PageSize), AllocOpts{Mode: AllocateAndCommit})
		if err != nil {
			t.Fatalf("Allocate(AllocateAndCommit): %v", err)
		}
		if wp, maxWP := env.wasteStats(); wp != 0 || maxWP != 8 {
			t.Fatalf("after full recycle before wake: got (wastePages=%d, maxWastePages=%d), want (0, 8)", wp, maxWP)
		}

		// Freeing 2 pages while releaserMain is still in throttled sleep keeps
		// maxWastePages = 8 (2 <= 8) and queues 2 waste pages for when sleep finishes.
		env.f.DecRef(extraFR)
		if wp, maxWP := env.wasteStats(); wp != 2 || maxWP != 8 {
			t.Fatalf("after DecRef during zero-waste sleep: got (wastePages=%d, maxWastePages=%d), want (2, 8)", wp, maxWP)
		}

		// Waking after 1h releases those 2 pages (bringing wastePages to 0 and resetting maxWastePages).
		env.wakeAfter(time.Hour)
		env.waitForIdle()

		// Also verify full recycle to 0 with no new DecRef cleanly parks on wake.
		env.f.DecRef(recycledFR)
		env.waitForSleep()
		recycledFR2, err := env.f.Allocate(4*uint64(hostarch.PageSize), AllocOpts{Mode: AllocateAndCommit})
		if err != nil {
			t.Fatalf("Allocate(AllocateAndCommit 2): %v", err)
		}
		defer env.f.DecRef(recycledFR2)
		env.wakeAfter(time.Hour)
		env.waitForIdle()
	})

	t.Run("huge waste page recycle during release window", func(t *testing.T) {
		hugePages := pagesPerHugePage()
		env := newReleaserTestEnv(t, MemoryFileOpts{
			ExpectHugepages:    true,
			ReleaseWindow:      8 * time.Hour,
			ReleaseWindowTicks: 4,
		})
		fr := env.allocateCommittedNonZero(4*hugePages, true)
		env.f.DecRef(fr)

		// 1 hugepage [3*HP, 4*HP) released; [0, 3*HP) remain waste.
		env.waitForSleep()
		if wp, _ := env.wasteStats(); wp != 3*hugePages {
			t.Fatalf("wastePages = %d, want %d", wp, 3*hugePages)
		}

		recycledHuge, err := env.f.Allocate(2*uint64(hostarch.HugePageSize), AllocOpts{
			Mode: AllocateAndWritePopulate,
			Huge: true,
		})
		if err != nil {
			t.Fatalf("Allocate(huge recycle): %v", err)
		}
		defer env.f.DecRef(recycledHuge)
		wantHuge := memmap.FileRange{Start: 0, End: 2 * uint64(hostarch.HugePageSize)}
		if recycledHuge != wantHuge {
			t.Fatalf("recycled huge range = %v, want %v", recycledHuge, wantHuge)
		}
		env.f.forEachMappingSlice(recycledHuge, func(bs []byte) {
			for off := 0; off < len(bs); off += int(hostarch.PageSize) {
				if bs[off] != 0 {
					t.Fatalf("recycled huge page at offset %#x not zeroed: got %#x", off, bs[off])
				}
			}
		})

		env.wakeAfter(2 * time.Hour)
		env.waitForIdle()
	})
}

func TestReleaseWindowDynamicRate(t *testing.T) {
	// ReleaseWindow = 16h, 4 ticks.
	env := newReleaserTestEnv(t, MemoryFileOpts{
		ReleaseWindow:      16 * time.Hour,
		ReleaseWindowTicks: 4,
	})
	frA := env.allocateCommittedNonZero(4, false)
	frB := env.allocateCommittedNonZero(13, false)
	frC := env.allocateCommittedNonZero(2, false)
	frD := env.allocateCommittedNonZero(4, false)

	// Free 4 pages => maxWastePages = 4, throttleNSPerPage = 4h, 1 page/tick.
	// First tick releases 1 page => wastePages = 3, throttlePages = 1 (needs 4h sleep).
	env.f.DecRef(frA)
	env.waitForSleep()
	if wp, maxWP := env.wasteStats(); wp != 3 || maxWP != 4 {
		t.Fatalf("after DecRef(frA): got (wastePages=%d, maxWastePages=%d), want (3, 4)", wp, maxWP)
	}

	// Free 1 page of frB => wastePages = 3 + 1 = 4 <= maxWastePages (4):
	// no rate increase and no wakeup.
	callsBefore := env.clock.totalCalls()
	env.f.DecRef(memmap.FileRange{Start: frB.Start, End: frB.Start + uint64(hostarch.PageSize)})
	runtime.Gosched()
	if wp, maxWP := env.wasteStats(); wp != 4 || maxWP != 4 {
		t.Fatalf("after non-increasing DecRef: got (wastePages=%d, maxWastePages=%d), want (4, 4)", wp, maxWP)
	}
	if calls := env.clock.totalCalls(); calls != callsBefore || env.hasPendingWake() {
		t.Fatalf("non-increasing DecRef woke releaser: calls=%d (want %d), pendingWake=%t", calls, callsBefore, env.hasPendingWake())
	}

	// Free remaining 12 pages of frB => wastePages = 4 + 12 = 16 > maxWastePages (4),
	// waking the sleeping releaser.
	env.f.DecRef(memmap.FileRange{
		Start: frB.Start + uint64(hostarch.PageSize),
		End:   frB.End,
	})

	// Waking with 0 elapsed time retires sleptPages = max(1, 0) = 1, clearing throttlePages (1 -> 0).
	// Releaser immediately recomputes throttleNSPerPage = 16h / 16 = 1h (4 pages/tick),
	// releases 4 pages from the last waste gap (wastePages: 16 -> 12), and sleeps.
	env.waitForSleep()
	if wp, maxWP := env.wasteStats(); wp != 12 || maxWP != 16 {
		t.Fatalf("after rate increase: got (wastePages=%d, maxWastePages=%d), want (12, 16)", wp, maxWP)
	}

	// Advance 4h => releases 4 pages (wastePages: 12 -> 8).
	env.wakeAfter(4 * time.Hour)
	env.waitForSleep()
	if wp, maxWP := env.wasteStats(); wp != 8 || maxWP != 16 {
		t.Fatalf("got (wastePages=%d, maxWastePages=%d), want (8, 16)", wp, maxWP)
	}

	// Free frC (2 pages at a higher offset) => wastePages = 10 <= maxWastePages (16).
	// maxWastePages MUST stay 16 (1h/page, 4 pages/tick max); if it dropped to 10
	// (1.6h/page), 2h would not finish sleeping off frC's 2 pages and the next
	// batch would only release ceil(4h/1.6h) = 3 pages instead of 4.
	env.f.DecRef(frC)
	if wp, maxWP := env.wasteStats(); wp != 10 || maxWP != 16 {
		t.Fatalf("after smaller DecRef: got (wastePages=%d, maxWastePages=%d), want (10, 16)", wp, maxWP)
	}

	// Next tick releases the 2-page frC gap (10 -> 8), sleeping for 2 * 1h = 2h.
	env.wakeAfter(4 * time.Hour)
	env.waitForSleep()
	if wp, _ := env.wasteStats(); wp != 8 {
		t.Fatalf("wastePages = %d, want 8", wp)
	}
	// After 2h (2 pages * 1h/page), next tick releases 4 of the 5 remaining
	// pages in frB (8 -> 4), leaving 1 page in frB and 3 pages in frA.
	env.wakeAfter(2 * time.Hour)
	env.waitForSleep()
	if wp, _ := env.wasteStats(); wp != 4 {
		t.Fatalf("wastePages = %d, want 4", wp)
	}
	// Next tick releases the remaining 1 page in frB (4 -> 3, 1h sleep).
	env.wakeAfter(4 * time.Hour)
	env.waitForSleep()
	if wp, _ := env.wasteStats(); wp != 3 {
		t.Fatalf("wastePages = %d, want 3", wp)
	}
	// Final tick after 1h releases the 3 pages in frA (3 -> 0).
	env.wakeAfter(time.Hour)
	env.waitForIdle()

	// Now that wastePages reached 0, maxWastePages was reset to 0.
	// Freeing frD (4 pages) uses the slower 4-page rate (4h/page, 1 page/tick) again.
	env.f.DecRef(frD)
	env.waitForSleep()
	if wp, maxWP := env.wasteStats(); wp != 3 || maxWP != 4 {
		t.Fatalf("after reset DecRef(frD): got (wastePages=%d, maxWastePages=%d), want (3, 4)", wp, maxWP)
	}
	for range 2 {
		env.wakeAfter(4 * time.Hour)
		env.waitForSleep()
	}
	env.wakeAfter(4 * time.Hour)
	env.waitForIdle()
}

func TestReleaseWindowMaxBytes(t *testing.T) {
	// ReleaseWindow = 20h, ReleaseWindowTicks = 4.
	// Set non-page-aligned ReleaseWindowMaxBytes = 8.5 pages => truncates to 8 pages.
	env := newReleaserTestEnv(t, MemoryFileOpts{
		ReleaseWindow:         20 * time.Hour,
		ReleaseWindowMaxBytes: 8*uint64(hostarch.PageSize) + uint64(hostarch.PageSize)/2,
		ReleaseWindowTicks:    4,
	})
	fr1 := env.allocateCommittedNonZero(20, false)
	frExact := env.allocateCommittedNonZero(5, false)
	frMid := env.allocateCommittedNonZero(5, false)
	fr2 := env.allocateCommittedNonZero(20, false)

	// Free 20 pages (> 8 max pages):
	// 1. Releaser immediately releases 20 - 8 = 12 pages unthrottled
	//    (wastePages = 8) without adding them to throttlePages.
	// 2. Releaser immediately executes a throttled tick of 5 pages
	//    (wastePages = 3) and enters throttled sleep for 5h
	//    (1 sleep, 1 throttled release).
	env.f.DecRef(fr1)
	env.waitForSleep()
	if sleeps, releases := env.clock.counts(); sleeps != 1 || releases != 1 {
		t.Fatalf("clock counts at first sleep = (sleeps=%d, releases=%d), want (1, 1)", sleeps, releases)
	}
	if wp, maxWP := env.wasteStats(); wp != 3 || maxWP != 20 {
		t.Fatalf("after initial burst above ReleaseWindowMaxBytes: got (wastePages=%d, maxWastePages=%d), want (3, 20)", wp, maxWP)
	}
	for p := uint64(0); p < 20; p++ {
		off := fr1.Start + p*uint64(hostarch.PageSize)
		wantCommitted := p < 3
		if got := env.isPageCommittedOnHost(off); got != wantCommitted {
			t.Fatalf("fr1 page %d: committed=%t, want %t", p, got, wantCommitted)
		}
	}

	// Freeing 5 pages brings wastePages to 3 + 5 == 8 == releaseWindowMaxPages
	// (still <= maxWastePages = 20), which must NOT unthrottle or wake the releaser.
	callsBefore := env.clock.totalCalls()
	env.f.DecRef(frExact)
	runtime.Gosched()
	if wp, maxWP := env.wasteStats(); wp != 8 || maxWP != 20 {
		t.Fatalf("at exact ReleaseWindowMaxBytes boundary: got (wastePages=%d, maxWastePages=%d), want (8, 20)", wp, maxWP)
	}
	if calls := env.clock.totalCalls(); calls != callsBefore || env.hasPendingWake() {
		t.Fatalf("DecRef to exact releaseWindowMaxPages woke releaser: calls=%d (want %d), pendingWake=%t", calls, callsBefore, env.hasPendingWake())
	}

	// Freeing 5 more pages (frMid, contiguous with frExact) brings wastePages to
	// 13, exceeding releaseWindowMaxPages (8) while remaining <= maxWastePages (20).
	// DecRef wakes the releaser:
	// 1. Waking with 0 elapsed time retires 1 of 5 throttlePages.
	// 2. Releaser sees wastePages (13) > 8, clears throttlePages = 0
	//    (throttleNSPerPage == 0), and immediately releases 13 - 8 = 5 pages
	//    from the [frExact, frMid) gap unthrottled, leaving 5 pages in frExact
	//    and 3 pages in fr1 (total wastePages = 8).
	// 3. Releaser immediately executes a throttled tick of 5 pages (all of
	//    frExact), leaving 3 waste pages in fr1, and sleeps for 5h.
	env.f.DecRef(frMid)
	env.waitForSleep()
	if sleeps, releases := env.clock.counts(); sleeps != 2 || releases != 2 {
		t.Fatalf("clock counts after frMid burst = (sleeps=%d, releases=%d), want (2, 2)", sleeps, releases)
	}
	if wp, maxWP := env.wasteStats(); wp != 3 || maxWP != 20 {
		t.Fatalf("after burst in (releaseWindowMaxPages, maxWastePages]: got (wastePages=%d, maxWastePages=%d), want (3, 20)", wp, maxWP)
	}

	// While throttled with 3 waste pages in fr1, free fr2 (20 pages) =>
	// wastePages = 23 > maxWastePages (20), waking the releaser:
	// 1. Waking with 0 elapsed time retires 1 of 5 throttlePages.
	// 2. Releaser sees wastePages (23) > 8, clears throttlePages = 0
	//    (throttleNSPerPage == 0), and immediately releases 23 - 8 = 15 pages
	//    from fr2 unthrottled, leaving 5 pages in fr2 and 3 pages in fr1
	//    (total wastePages = 8).
	// 3. Releaser immediately executes a throttled tick (max 6 pages at 20h/23
	//    per page), releasing all 5 remaining pages in the last gap (fr2),
	//    leaving 3 waste pages in fr1, and sleeps for 5 * 20h/23 (< 5h).
	env.f.DecRef(fr2)
	env.waitForSleep()
	if sleeps, releases := env.clock.counts(); sleeps != 3 || releases != 3 {
		t.Fatalf("clock counts after fr2 burst = (sleeps=%d, releases=%d), want (3, 3)", sleeps, releases)
	}
	if wp, maxWP := env.wasteStats(); wp != 3 || maxWP != 23 {
		t.Fatalf("after second burst above ReleaseWindowMaxBytes: got (wastePages=%d, maxWastePages=%d), want (3, 23)", wp, maxWP)
	}

	// Sleeping 5h (> 5 * 20h/23) releases the remaining 3 waste pages in fr1.
	env.wakeAfter(5 * time.Hour)
	env.waitForIdle()
}

func TestReleaseWindowOverheadDeduction(t *testing.T) {
	t.Run("partial release overhead shortens subsequent throttle sleep", func(t *testing.T) {
		// 16 pages over 16h, 4 ticks => throttleNSPerPage = 1h, 4 pages/tick.
		// Configure 2h of release overhead (2 pages) per releaseLocked call,
		// leaving throttlePages = 4 - 2 = 2 (2h sleep instead of 4h).
		env := newReleaserTestEnv(t, MemoryFileOpts{
			ReleaseWindow:      16 * time.Hour,
			ReleaseWindowTicks: 4,
		})
		env.clock.setReleaseOverhead(2 * time.Hour)
		fr := env.allocateCommittedNonZero(16, false)
		env.f.DecRef(fr)

		env.waitForSleep()
		if wp, _ := env.wasteStats(); wp != 12 {
			t.Fatalf("wastePages = %d, want 12", wp)
		}
		// 1h sleep retires 1 of the 2 remaining throttlePages (does NOT release yet).
		env.wakeAfter(time.Hour)
		env.waitForSleep()
		if wp, _ := env.wasteStats(); wp != 12 {
			t.Fatalf("wastePages after 1h = %d, want 12", wp)
		}
		// Second 1h (total 2h sleep + 2h overhead = 4h) releases the next 4 pages.
		env.wakeAfter(time.Hour)
		env.waitForSleep()
		if wp, _ := env.wasteStats(); wp != 8 {
			t.Fatalf("wastePages after 2h = %d, want 8", wp)
		}
		env.wakeAfter(2 * time.Hour)
		env.waitForSleep()
		if wp, _ := env.wasteStats(); wp != 4 {
			t.Fatalf("wastePages = %d, want 4", wp)
		}
		env.wakeAfter(2 * time.Hour)
		env.waitForIdle()
	})

	t.Run("overhead exceeding batch size skips sleep completely", func(t *testing.T) {
		// 16 pages over 16h, 4 ticks => 4 pages/tick (4h).
		// With 5h overhead per release, overheadPages (5) >= frPages (4),
		// so throttlePages stays 0 and all 4 batches release without sleeping.
		env := newReleaserTestEnv(t, MemoryFileOpts{
			ReleaseWindow:      16 * time.Hour,
			ReleaseWindowTicks: 4,
		})
		env.clock.setReleaseOverhead(5 * time.Hour)
		fr := env.allocateCommittedNonZero(16, false)
		env.f.DecRef(fr)
		env.waitForIdle()
		if sleeps, releases := env.clock.counts(); sleeps != 0 || releases != 4 {
			t.Errorf("clock counts = (sleeps=%d, releases=%d), want (0, 4)", sleeps, releases)
		}
	})
}

func TestReleaseWindowSubNanosecondPerPage(t *testing.T) {
	// ReleaseWindow = 10ns with 20 waste pages => throttleNSPerPage = 10 / 20 = 0.
	// All pages must release immediately without sleeping or dividing by zero.
	env := newReleaserTestEnv(t, MemoryFileOpts{
		ReleaseWindow:      10 * time.Nanosecond,
		ReleaseWindowTicks: 4,
	})
	fr := env.allocateCommittedNonZero(20, false)
	env.f.DecRef(fr)
	env.waitForIdle()
	if calls := env.clock.totalCalls(); calls != 0 {
		t.Errorf("releaserNanotime calls = %d, want 0 when throttleNSPerPage == 0", calls)
	}
}

func TestReleaseWindowHugePages(t *testing.T) {
	hugePages := pagesPerHugePage()

	t.Run("rounds throttled and max-bytes releases up to huge page granularity", func(t *testing.T) {
		// ReleaseWindowMaxBytes = 2 hugepages + 1 small page.
		// Freeing 4 committed hugepages:
		// 1. Unthrottled excess = 4*HP - (2*HP + 1) = 2*HP - 1 small pages,
		//    which MustHugePageRoundUp rounds up to 2 full hugepages.
		//    Remaining waste = 2*HP (<= 2*HP + 1), with throttlePages = 0.
		// 2. Releaser immediately executes a throttled tick: ReleaseWindow = 8h,
		//    Ticks = 8, maxWastePages = 4*HP => maxDelayNS = 1h => raw maxPages
		//    = 0.5*HP, which MustHugePageRoundUp rounds up to 1 full hugepage.
		//    Releases 1*HP, leaving 1*HP waste, with subreleased map empty (no
		//    hugepage splitting), and enters throttled sleep for 2h (1*HP out
		//    of 4*HP over 8h).
		env := newReleaserTestEnv(t, MemoryFileOpts{
			ExpectHugepages:       true,
			ReleaseWindow:         8 * time.Hour,
			ReleaseWindowMaxBytes: 2*uint64(hostarch.HugePageSize) + uint64(hostarch.PageSize),
			ReleaseWindowTicks:    8,
		})
		fr := env.allocateCommittedNonZero(4*hugePages, true)
		env.f.DecRef(fr)

		env.waitForSleep()
		if wp, maxWP := env.wasteStats(); wp != hugePages || maxWP != 4*hugePages {
			t.Fatalf("after unthrottled excess and initial throttled tick: got (wastePages=%d, maxWastePages=%d), want (%d, %d)", wp, maxWP, hugePages, 4*hugePages)
		}
		env.f.mu.Lock()
		numSubreleased := len(env.f.subreleased)
		env.f.mu.Unlock()
		if numSubreleased != 0 {
			t.Errorf("subreleased map has %d entries, want 0 (whole hugepages should be released)", numSubreleased)
		}
		if !env.isPageCommittedOnHost(fr.Start) {
			t.Errorf("first hugepage at %#x unexpectedly decommitted before final tick", fr.Start)
		}
		if env.isPageCommittedOnHost(fr.Start + uint64(hostarch.HugePageSize)) {
			t.Errorf("second hugepage at %#x still committed after initial throttled tick", fr.Start+uint64(hostarch.HugePageSize))
		}
		if env.isPageCommittedOnHost(fr.Start + 2*uint64(hostarch.HugePageSize)) {
			t.Errorf("third hugepage at %#x still committed after unthrottled excess release", fr.Start+2*uint64(hostarch.HugePageSize))
		}
		if env.isPageCommittedOnHost(fr.Start + 3*uint64(hostarch.HugePageSize)) {
			t.Errorf("fourth hugepage at %#x still committed after unthrottled excess release", fr.Start+3*uint64(hostarch.HugePageSize))
		}

		// 1 hugepage out of 4*HP maxWastePages over 8h sleeps for 2h.
		env.wakeAfter(2 * time.Hour)
		env.waitForIdle()
		if env.isPageCommittedOnHost(fr.Start) {
			t.Errorf("first hugepage at %#x still committed after final release", fr.Start)
		}

		// Re-allocating 4 uncommitted hugepages must reuse the freed range at fr.Start.
		reusedFR, err := env.f.Allocate(4*uint64(hostarch.HugePageSize), AllocOpts{
			Mode: AllocateUncommitted,
			Huge: true,
		})
		if err != nil {
			t.Fatalf("Allocate(reused hugepages): %v", err)
		}
		defer env.f.DecRef(reusedFR)
		if reusedFR != fr {
			t.Errorf("re-allocated huge range = %v, want %v (freed in unfreeHuge)", reusedFR, fr)
		}
	})

	t.Run("partial hugepage subrelease frees containing hugepage once complete", func(t *testing.T) {
		env := newReleaserTestEnv(t, MemoryFileOpts{
			ExpectHugepages:    true,
			ReleaseWindow:      8 * time.Hour,
			ReleaseWindowTicks: 2,
		})
		fr := env.allocateCommittedNonZero(hugePages, true)
		mid := fr.Start + uint64(hostarch.HugePageSize)/2

		// Free first half of the hugepage; with 1 batch, it subreleases [0, HP/2) and goes idle.
		env.f.DecRef(memmap.FileRange{Start: fr.Start, End: mid})
		env.waitForIdle()
		env.f.mu.Lock()
		subrel := env.f.subreleased[fr.Start]
		env.f.mu.Unlock()
		if subrel != hugePages/2 {
			t.Fatalf("subreleased[%#x] = %d, want %d", fr.Start, subrel, hugePages/2)
		}

		// Free second half of the hugepage; once released, the whole hugepage returns to free.
		env.f.DecRef(memmap.FileRange{Start: mid, End: fr.End})
		env.waitForIdle()
		env.f.mu.Lock()
		numSubrel := len(env.f.subreleased)
		env.f.mu.Unlock()
		if numSubrel != 0 {
			t.Fatalf("subreleased has %d entries after full hugepage release, want 0", numSubrel)
		}
		reusedFR, err := env.f.Allocate(uint64(hostarch.HugePageSize), AllocOpts{
			Mode: AllocateUncommitted,
			Huge: true,
		})
		if err != nil {
			t.Fatalf("Allocate(reused single hugepage): %v", err)
		}
		defer env.f.DecRef(reusedFR)
		if reusedFR != fr {
			t.Errorf("re-allocated huge range = %v, want %v", reusedFR, fr)
		}
	})

	t.Run("unaligned huge waste gap aligns slice start to avoid splitting intact hugepages", func(t *testing.T) {
		// Allocate 3 hugepages [0, 3*HP), keep [2*HP + PageSize, 3*HP) referenced,
		// and free [0, 2*HP + PageSize) (2*HP + 1 pages).
		// With ReleaseWindow = 24h and Ticks = 3, maxBytes = 1*HP:
		// 1. First tick aligns fr.Start = MustHugePageRoundUp((2*HP + PageSize) - HP) = 2*HP,
		//    releasing only the 1-page tail [2*HP, 2*HP + PageSize) without splitting
		//    hugepages 0 or 1, and sleeping for 1 page (~84s real timer).
		// 2. Second tick releases [HP, 2*HP) intact (subreleased still only has 2*HP: 1).
		// 3. Third tick releases [0, HP) intact.
		env := newReleaserTestEnv(t, MemoryFileOpts{
			ExpectHugepages:    true,
			ReleaseWindow:      24 * time.Hour,
			ReleaseWindowTicks: 3,
		})
		fr := env.allocateCommittedNonZero(3*hugePages, true)
		hp2Start := fr.Start + 2*uint64(hostarch.HugePageSize)
		unalignedEnd := hp2Start + uint64(hostarch.PageSize)

		env.f.DecRef(memmap.FileRange{Start: fr.Start, End: unalignedEnd})
		env.waitForSleep()
		if wp, maxWP := env.wasteStats(); wp != 2*hugePages || maxWP != 2*hugePages+1 {
			t.Fatalf("after tail release: got (wastePages=%d, maxWastePages=%d), want (%d, %d)", wp, maxWP, 2*hugePages, 2*hugePages+1)
		}
		env.f.mu.Lock()
		numSubrel := len(env.f.subreleased)
		hp2Subrel := env.f.subreleased[hp2Start]
		env.f.mu.Unlock()
		if numSubrel != 1 || hp2Subrel != 1 {
			t.Fatalf("subreleased after tail release: len=%d, subreleased[%#x]=%d, want len=1, 1", numSubrel, hp2Start, hp2Subrel)
		}

		// Sleep for 1 page (24h / (2*HP + 1) < 1h) releases [HP, 2*HP) intact.
		env.wakeAfter(time.Hour)
		env.waitForSleep()
		if wp, _ := env.wasteStats(); wp != hugePages {
			t.Fatalf("after second tick: wastePages = %d, want %d", wp, hugePages)
		}
		env.f.mu.Lock()
		numSubrel = len(env.f.subreleased)
		hp2Subrel = env.f.subreleased[hp2Start]
		env.f.mu.Unlock()
		if numSubrel != 1 || hp2Subrel != 1 {
			t.Fatalf("subreleased after intact hugepage release: len=%d, subreleased[%#x]=%d, want len=1, 1", numSubrel, hp2Start, hp2Subrel)
		}

		// Sleep for 1*HP releases [0, HP) intact.
		env.wakeAfter(12 * time.Hour)
		env.waitForIdle()

		// Freeing the remaining [2*HP + PageSize, 3*HP) completes hugepage 2.
		env.f.DecRef(memmap.FileRange{Start: unalignedEnd, End: fr.End})
		env.waitForIdle()
		env.f.mu.Lock()
		numSubrel = len(env.f.subreleased)
		env.f.mu.Unlock()
		if numSubrel != 0 {
			t.Fatalf("subreleased has %d entries after full release, want 0", numSubrel)
		}
		reusedFR, err := env.f.Allocate(3*uint64(hostarch.HugePageSize), AllocOpts{
			Mode: AllocateUncommitted,
			Huge: true,
		})
		if err != nil {
			t.Fatalf("Allocate(reused 3 hugepages): %v", err)
		}
		defer env.f.DecRef(reusedFR)
		if reusedFR != fr {
			t.Errorf("re-allocated huge range = %v, want %v", reusedFR, fr)
		}
	})

	t.Run("multi-hugepage unaligned subrelease tracks and completes boundary hugepages", func(t *testing.T) {
		// Allocate 3 hugepages [0, 3*HP) with Ticks = 1 so multi-hugepage ranges
		// are released in a single releaseLocked call.
		env := newReleaserTestEnv(t, MemoryFileOpts{
			ExpectHugepages:    true,
			ReleaseWindow:      8 * time.Hour,
			ReleaseWindowTicks: 1,
		})
		fr := env.allocateCommittedNonZero(3*hugePages, true)
		hp0Start := fr.Start
		hp2Start := fr.Start + 2*uint64(hostarch.HugePageSize)
		pageSz := uint64(hostarch.PageSize)

		// Pre-subrelease 1 page at the start of HP0 and 1 page at the end of HP2,
		// keeping 1 page in HP0 ([PageSize, 2*PageSize)) and 1 page in HP2
		// ([3*HP - 2*PageSize, 3*HP - PageSize)) referenced.
		env.f.DecRef(memmap.FileRange{Start: fr.Start, End: fr.Start + pageSz})
		env.waitForIdle()
		env.f.DecRef(memmap.FileRange{Start: fr.End - pageSz, End: fr.End})
		env.waitForIdle()

		// Release [2*PageSize, 3*HP - 2*PageSize): spans 3 hugepages, freeing
		// interior HP1 completely while accumulating onto non-zero oldSubrel (1)
		// in both HP0 and HP2 (reaching hugePages - 1 in each).
		env.f.DecRef(memmap.FileRange{Start: fr.Start + 2*pageSz, End: fr.End - 2*pageSz})
		env.waitForIdle()
		env.f.mu.Lock()
		sub0 := env.f.subreleased[hp0Start]
		sub2 := env.f.subreleased[hp2Start]
		numSubrel := len(env.f.subreleased)
		env.f.mu.Unlock()
		if numSubrel != 2 || sub0 != hugePages-1 || sub2 != hugePages-1 {
			t.Fatalf("subreleased = (len=%d, hp0=%d, hp2=%d), want (2, %d, %d)", numSubrel, sub0, sub2, hugePages-1, hugePages-1)
		}
		// Interior HP1 must already be free in unfreeHuge.
		hp1Reused, err := env.f.Allocate(uint64(hostarch.HugePageSize), AllocOpts{
			Mode: AllocateUncommitted,
			Huge: true,
		})
		if err != nil {
			t.Fatalf("Allocate(interior HP1): %v", err)
		}
		wantHP1 := memmap.FileRange{
			Start: fr.Start + uint64(hostarch.HugePageSize),
			End:   fr.Start + 2*uint64(hostarch.HugePageSize),
		}
		if hp1Reused != wantHP1 {
			t.Fatalf("re-allocated interior hugepage = %v, want %v", hp1Reused, wantHP1)
		}

		// Free the remaining 1 page in HP0, re-allocated HP1, and remaining 1 page in HP2.
		env.f.DecRef(memmap.FileRange{Start: fr.Start + pageSz, End: fr.Start + 2*pageSz})
		env.waitForIdle()
		env.f.DecRef(hp1Reused)
		env.waitForIdle()
		env.f.DecRef(memmap.FileRange{Start: fr.End - 2*pageSz, End: fr.End - pageSz})
		env.waitForIdle()

		env.f.mu.Lock()
		numSubrel = len(env.f.subreleased)
		env.f.mu.Unlock()
		if numSubrel != 0 {
			t.Fatalf("subreleased has %d entries after completing boundary hugepages, want 0", numSubrel)
		}

		// Also verify completing pre-subreleased boundary hugepages inside a single
		// multi-hugepage releaseLocked call (oldSubrel > 0 && newSubrel == pagesPerHugePage()).
		fr2 := env.allocateCommittedNonZero(2*hugePages, true)
		if want := (memmap.FileRange{Start: fr.Start, End: fr.Start + 2*uint64(hostarch.HugePageSize)}); fr2 != want {
			t.Fatalf("re-allocated 2 hugepages = %v, want %v", fr2, want)
		}
		env.f.DecRef(memmap.FileRange{Start: fr2.Start, End: fr2.Start + pageSz})
		env.waitForIdle()
		env.f.DecRef(memmap.FileRange{Start: fr2.End - pageSz, End: fr2.End})
		env.waitForIdle()
		env.f.DecRef(memmap.FileRange{Start: fr2.Start + pageSz, End: fr2.End - pageSz})
		env.waitForIdle()

		env.f.mu.Lock()
		numSubrel = len(env.f.subreleased)
		env.f.mu.Unlock()
		if numSubrel != 0 {
			t.Fatalf("subreleased has %d entries after multi-hugepage completion, want 0", numSubrel)
		}
		reusedFR, err := env.f.Allocate(3*uint64(hostarch.HugePageSize), AllocOpts{
			Mode: AllocateUncommitted,
			Huge: true,
		})
		if err != nil {
			t.Fatalf("Allocate(reused 3 hugepages): %v", err)
		}
		defer env.f.DecRef(reusedFR)
		if reusedFR != fr {
			t.Errorf("re-allocated huge range = %v, want %v (no leak in unfreeHuge)", reusedFR, fr)
		}
	})

	t.Run("releases small waste before huge waste", func(t *testing.T) {
		// Use ReleaseWindow = 24h so throttleNSPerPage = 24h / 1024 (~84s),
		// keeping real throttleTimer well clear of test execution time.
		env := newReleaserTestEnv(t, MemoryFileOpts{
			ExpectHugepages:    true,
			ReleaseWindow:      24 * time.Hour,
			ReleaseWindowTicks: 100000,
		})
		hugeFR, err := env.f.Allocate(2*uint64(hostarch.HugePageSize), AllocOpts{
			Mode: AllocateUncommitted,
			Huge: true,
		})
		if err != nil {
			t.Fatalf("Allocate(huge): %v", err)
		}
		smallFR := env.allocateCommittedNonZero(2, false)

		env.f.DecRef(hugeFR)
		env.waitForSleep()
		// 1 hugepage remains in unwasteHuge (maxWastePages = 2*HP).
		// Freeing 2 small pages keeps wastePages (HP + 2) <= maxWastePages (2*HP),
		// so the releaser stays asleep until we advance the clock.
		env.f.DecRef(smallFR)
		env.wakeAfter(12 * time.Hour)

		// The next two releases must pick unwasteSmall (1 small page each) before unwasteHuge.
		env.waitForSleep()
		if wp, _ := env.wasteStats(); wp != hugePages+1 {
			t.Fatalf("wastePages = %d, want %d (1 hugepage + 1 small page)", wp, hugePages+1)
		}
		env.wakeAfter(24 * time.Hour)
		env.waitForSleep()
		if wp, _ := env.wasteStats(); wp != hugePages {
			t.Fatalf("wastePages = %d, want %d (1 hugepage)", wp, hugePages)
		}
		env.wakeAfter(24 * time.Hour)
		env.waitForIdle()
	})
}

func TestReleaseWindowSharedRef(t *testing.T) {
	env := newReleaserTestEnv(t, MemoryFileOpts{
		ReleaseWindow:      4 * time.Hour,
		ReleaseWindowTicks: 4,
	})
	sharedFR := env.allocateCommittedNonZero(16, false)
	env.f.IncRef(sharedFR, 0)
	otherFR := env.allocateCommittedNonZero(4, false)

	// Throttle releaser on otherFR (releases 1 page, sleeps with 3 waste pages).
	env.f.DecRef(otherFR)
	env.waitForSleep()
	callsBefore := env.clock.totalCalls()

	// Dropping 1 of 2 refs on sharedFR must neither create waste nor wake the releaser.
	env.f.DecRef(sharedFR)
	runtime.Gosched()
	if wp, maxWP := env.wasteStats(); wp != 3 || maxWP != 4 {
		t.Fatalf("after shared DecRef: got (wastePages=%d, maxWastePages=%d), want (3, 4)", wp, maxWP)
	}
	if calls := env.clock.totalCalls(); calls != callsBefore || env.hasPendingWake() {
		t.Fatalf("shared DecRef woke releaser: calls=%d (want %d), pendingWake=%t", calls, callsBefore, env.hasPendingWake())
	}

	// Dropping the final ref on sharedFR creates 16 waste pages, merging with
	// otherFR's 3 waste pages into a single 19-page gap (19 > 4), waking the
	// releaser immediately. With 4 ticks, maxPages = 5, so batches drain 19 -> 14 -> 9 -> 4 -> 0.
	env.f.DecRef(sharedFR)
	env.waitForSleep()
	if wp, maxWP := env.wasteStats(); wp != 14 || maxWP != 19 {
		t.Fatalf("got (wastePages=%d, maxWastePages=%d), want (14, 19)", wp, maxWP)
	}
	for range 2 {
		env.wakeAfter(2 * time.Hour)
		env.waitForSleep()
	}
	env.wakeAfter(2 * time.Hour)
	env.waitForIdle()
}

func TestReleaseWindowUnthrottleOnSaveAndPressure(t *testing.T) {
	t.Run("SaveTo unthrottles waste release, round-trips LoadFrom, and restores throttling", func(t *testing.T) {
		env := newReleaserTestEnv(t, MemoryFileOpts{
			ReleaseWindow:      4 * time.Hour,
			ReleaseWindowTicks: 4,
		})
		// Empty SaveTo with ReleaseWindow configured succeeds immediately.
		if err := env.f.SaveTo(context.Background(), io.Discard, &SaveOpts{}); err != nil {
			t.Fatalf("empty SaveTo: %v", err)
		}

		liveFR := env.allocateCommittedNonZero(2, false)
		liveHeld := true
		t.Cleanup(func() {
			if liveHeld && env.f != nil {
				env.f.mu.Lock()
				env.f.releaserUnthrottleLocked()
				env.f.mu.Unlock()
				env.f.DecRef(liveFR)
				env.f.releaserNotify()
			}
		})
		wasteFR := env.allocateCommittedNonZero(16, false)
		env.f.DecRef(wasteFR)

		// Wait until releaser is throttled with 12 waste pages remaining.
		env.waitForSleep()
		if wp, _ := env.wasteStats(); wp != 12 {
			t.Fatalf("wastePages before SaveTo = %d, want 12", wp)
		}

		// SaveTo must unthrottle the releaser and complete without advancing mock time.
		var checkpoint bytes.Buffer
		if err := env.f.SaveTo(context.Background(), &checkpoint, &SaveOpts{}); err != nil {
			t.Fatalf("SaveTo: %v", err)
		}
		env.waitForIdle()
		if got, want := env.knownCommittedBytes(), liveFR.Length(); got != want {
			t.Errorf("knownCommittedBytes after SaveTo = %d, want %d (only liveFR)", got, want)
		}

		// Subsequent deallocations on the saved MemoryFile must be throttled again.
		afterFR := env.allocateCommittedNonZero(8, false)
		env.f.DecRef(afterFR)
		env.waitForSleep()
		if wp, maxWP := env.wasteStats(); wp != 6 || maxWP != 8 {
			t.Fatalf("after SaveTo DecRef: got (wastePages=%d, maxWastePages=%d), want (6, 8)", wp, maxWP)
		}
		for range 2 {
			env.wakeAfter(time.Hour)
			env.waitForSleep()
		}
		env.wakeAfter(time.Hour)
		env.waitForIdle()

		// Release liveFR and tear down env before starting restoredEnv so two
		// MemoryFiles never share releaserNanotime concurrently.
		liveHeld = false
		env.f.DecRef(liveFR)
		env.waitForSleep()
		env.wakeAfter(2 * time.Hour)
		env.waitForIdle()
		env.destroy()

		// Restore into a new ReleaseWindow-configured MemoryFile and verify both
		// preserved live contents and post-restore throttling.
		restoredEnv := newReleaserTestEnv(t, MemoryFileOpts{
			ReleaseWindow:      4 * time.Hour,
			ReleaseWindowTicks: 4,
		})
		if err := restoredEnv.f.LoadFrom(context.Background(), &checkpoint, &LoadOpts{}); err != nil {
			t.Fatalf("LoadFrom: %v", err)
		}
		restoredHeld := true
		t.Cleanup(func() {
			if restoredHeld && restoredEnv.f != nil {
				restoredEnv.f.mu.Lock()
				restoredEnv.f.releaserUnthrottleLocked()
				restoredEnv.f.mu.Unlock()
				restoredEnv.f.DecRef(liveFR)
				restoredEnv.f.releaserNotify()
			}
		})
		restoredEnv.f.forEachMappingSlice(liveFR, func(bs []byte) {
			for off := 0; off < len(bs); off += int(hostarch.PageSize) {
				if bs[off] != 0x5a {
					t.Errorf("restored live page at offset %#x = %#x, want 0x5a", off, bs[off])
				}
			}
		})
		restoredHeld = false
		restoredEnv.f.DecRef(liveFR)
		restoredEnv.waitForSleep()
		if wp, maxWP := restoredEnv.wasteStats(); wp != 1 || maxWP != 2 {
			t.Fatalf("post-LoadFrom DecRef: got (wastePages=%d, maxWastePages=%d), want (1, 2)", wp, maxWP)
		}
		restoredEnv.wakeAfter(2 * time.Hour)
		restoredEnv.waitForIdle()
	})

	t.Run("releaserUnthrottleLocked drains remaining waste and resets throttling", func(t *testing.T) {
		env := newReleaserTestEnv(t, MemoryFileOpts{
			ReleaseWindow:      4 * time.Hour,
			ReleaseWindowTicks: 4,
		})
		fr := env.allocateCommittedNonZero(16, false)
		env.f.DecRef(fr)

		env.waitForSleep()
		if wp, _ := env.wasteStats(); wp != 12 {
			t.Fatalf("wastePages = %d, want 12", wp)
		}

		// Unthrottle + notify without advancing mock time drains all 12 waste pages.
		env.f.mu.Lock()
		env.f.releaserUnthrottleLocked()
		env.f.mu.Unlock()
		env.f.releaserNotify()
		env.waitForIdle()

		// Subsequent DecRef after waste reached 0 must be throttled again.
		fr2 := env.allocateCommittedNonZero(4, false)
		env.f.DecRef(fr2)
		env.waitForSleep()
		if wp, maxWP := env.wasteStats(); wp != 3 || maxWP != 4 {
			t.Fatalf("after unthrottle reset: got (wastePages=%d, maxWastePages=%d), want (3, 4)", wp, maxWP)
		}
		for range 2 {
			env.wakeAfter(time.Hour)
			env.waitForSleep()
		}
		env.wakeAfter(time.Hour)
		env.waitForIdle()
	})
}

func TestReleaseWindowMemoryAccounting(t *testing.T) {
	if err := usage.Init(); err != nil {
		t.Fatalf("usage.Init: %v", err)
	}
	baseStats, _ := usage.MemoryAccounting.Copy()

	env := newReleaserTestEnvWithAccounting(t, MemoryFileOpts{
		ReleaseWindow:      4 * time.Hour,
		ReleaseWindowTicks: 4,
	})
	var held []memmap.FileRange
	t.Cleanup(func() {
		if env.f == nil {
			return
		}
		env.f.mu.Lock()
		env.f.releaserUnthrottleLocked()
		env.f.mu.Unlock()
		for _, r := range held {
			env.f.DecRef(r)
		}
		env.f.releaserNotify()
		env.waitForIdle()
	})
	fr, err := env.f.Allocate(8*uint64(hostarch.PageSize), AllocOpts{
		Kind: usage.Anonymous,
		Mode: AllocateAndWritePopulate,
	})
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	held = append(held, fr)
	env.f.forEachMappingSlice(fr, func(bs []byte) {
		for off := 0; off < len(bs); off += int(hostarch.PageSize) {
			bs[off] = 0x5a
		}
	})
	if err := env.f.UpdateUsage(nil); err != nil {
		t.Fatalf("UpdateUsage: %v", err)
	}
	if got, want := env.knownCommittedBytes(), fr.Length(); got != want {
		t.Fatalf("knownCommittedBytes = %d, want %d", got, want)
	}
	stats, _ := usage.MemoryAccounting.Copy()
	if got, want := stats.Anonymous-baseStats.Anonymous, fr.Length(); got != want {
		t.Fatalf("Anonymous accounting delta = %d, want %d", got, want)
	}

	// Freeing moves accounting from Anonymous to System; first tick releases
	// 2 pages, leaving 6 waste pages accounted to System.
	held = nil
	env.f.DecRef(fr)
	env.waitForSleep()
	stats, _ = usage.MemoryAccounting.Copy()
	if got := stats.Anonymous - baseStats.Anonymous; got != 0 {
		t.Errorf("Anonymous accounting delta after DecRef = %d, want 0", got)
	}
	if got, want := stats.System-baseStats.System, 6*uint64(hostarch.PageSize); got != want {
		t.Errorf("System accounting delta while 6 waste pages throttled = %d, want %d", got, want)
	}

	// Recycling 2 waste pages decrements System by 2 pages, and releasing the
	// remaining 4 waste pages decrements System back to baseline.
	recycledFR, err := env.f.Allocate(2*uint64(hostarch.PageSize), AllocOpts{
		Kind: usage.Anonymous,
		Mode: AllocateAndWritePopulate,
	})
	if err != nil {
		t.Fatalf("Allocate(recycle): %v", err)
	}
	held = append(held, recycledFR)
	stats, _ = usage.MemoryAccounting.Copy()
	if got, want := stats.System-baseStats.System, 4*uint64(hostarch.PageSize); got != want {
		t.Errorf("System accounting delta after 2-page recycle = %d, want %d", got, want)
	}
	env.wakeAfter(time.Hour)
	env.waitForSleep()
	env.wakeAfter(time.Hour)
	env.waitForIdle()

	held = nil
	env.f.DecRef(recycledFR)
	env.waitForSleep()
	env.wakeAfter(2 * time.Hour)
	env.waitForIdle()

	if committed := env.knownCommittedBytes(); committed != 0 {
		t.Errorf("knownCommittedBytes after releasing all pages = %d, want 0", committed)
	}
	stats, _ = usage.MemoryAccounting.Copy()
	if got := stats.System - baseStats.System; got != 0 {
		t.Errorf("System accounting delta after releasing all pages = %d, want 0", got)
	}
}

type testEvictableUser struct {
	mu sync.Mutex
	// +checklocks:mu
	evicted []EvictableRange
	// +checklocks:mu
	onEvict func()
}

func (u *testEvictableUser) Evict(_ context.Context, er EvictableRange) {
	u.mu.Lock()
	u.evicted = append(u.evicted, er)
	fn := u.onEvict
	u.mu.Unlock()
	if fn != nil {
		fn()
	}
}

func (u *testEvictableUser) count() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.evicted)
}

func TestReleaseWindowDelayedEvictionAndDestroy(t *testing.T) {
	t.Run("DelayedEvictionEnabled waits until throttled waste reaches zero", func(t *testing.T) {
		env := newReleaserTestEnv(t, MemoryFileOpts{
			DelayedEviction:    DelayedEvictionEnabled,
			ReleaseWindow:      4 * time.Hour,
			ReleaseWindowTicks: 2,
		})
		evictFR := env.allocateCommittedNonZero(4, false)
		wasteFR := env.allocateCommittedNonZero(8, false)

		// Free wasteFR => releases 4 pages, sleeps with 4 waste pages left.
		env.f.DecRef(wasteFR)
		env.waitForSleep()

		evictedCh := make(chan struct{})
		user := &testEvictableUser{
			onEvict: func() {
				env.f.DecRef(evictFR)
				close(evictedCh)
			},
		}
		// MarkEvictable calls releaserNotify(), waking the releaser with 0 elapsed time
		// (retiring 1 of 4 throttled pages, leaving 3 throttled pages and 4 waste pages).
		env.f.MarkEvictable(user, EvictableRange{Start: 0, End: uint64(hostarch.PageSize)})
		env.waitForSleep()

		if got := user.count(); got != 0 {
			t.Fatalf("evicted %d ranges while wastePages > 0, want 0", got)
		}

		// Finish sleeping off wasteFR => wastePages hits 0 => releaser starts eviction.
		// Evict callback DecRefs evictFR (4 pages), which then enters throttled sleep (2 pages left).
		env.wakeAfter(4 * time.Hour)
		select {
		case <-evictedCh:
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for Evict after wastePages reached 0")
		}
		env.f.WaitForEvictions()
		env.waitForSleep()
		if wp, maxWP := env.wasteStats(); wp != 2 || maxWP != 4 {
			t.Fatalf("after eviction DecRef: got (wastePages=%d, maxWastePages=%d), want (2, 4)", wp, maxWP)
		}
		env.wakeAfter(2 * time.Hour)
		env.waitForIdle()
	})

	t.Run("Destroy with DecommitOnDestroy wakes throttled releaser immediately", func(t *testing.T) {
		env := newReleaserTestEnv(t, MemoryFileOpts{
			DecommitOnDestroy:  true,
			ReleaseWindow:      24 * time.Hour,
			ReleaseWindowTicks: 4,
		})
		fr := env.allocateCommittedNonZero(16, false)
		env.f.DecRef(fr)
		env.waitForSleep()
		if wp, _ := env.wasteStats(); wp != 12 {
			t.Fatalf("wastePages = %d, want 12", wp)
		}
		// Destroy must promptly wake releaserMain out of its 6-hour timer sleep.
		env.destroy()
	})
}

func TestReleaseWindowRealTimer(t *testing.T) {
	// Verify that defaultReleaserNanotime and the throttleTimer.C channel
	// wake releaserMain across ticks without any external releaserNotify calls.
	fd, err := memutil.CreateMemFD("pgalloc-real-timer", 0)
	if err != nil {
		t.Fatalf("CreateMemFD: %v", err)
	}
	file := os.NewFile(uintptr(fd), "pgalloc-real-timer")
	f, err := NewMemoryFile(file, MemoryFileOpts{
		DelayedEviction:         DelayedEvictionDisabled,
		MemoryAccounting: MemoryAccountingDisabled,
		ReleaseWindow:           4 * time.Millisecond,
		ReleaseWindowTicks:      4,
	})
	if err != nil {
		_ = file.Close()
		t.Fatalf("NewMemoryFile: %v", err)
	}
	defer destroyAndWait(f)

	fr, err := f.Allocate(4*uint64(hostarch.PageSize), AllocOpts{Mode: AllocateAndWritePopulate})
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	f.forEachMappingSlice(fr, func(bs []byte) {
		for off := 0; off < len(bs); off += int(hostarch.PageSize) {
			bs[off] = 0x5a
		}
	})
	f.DecRef(fr)

	deadline := time.Now().Add(5 * time.Second)
	for {
		f.mu.Lock()
		done := f.wastePages == 0 && f.maxWastePages == 0
		f.mu.Unlock()
		if done {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for real throttleTimer to drain wastePages")
		}
		time.Sleep(time.Millisecond)
	}
	if off, err := unix.Seek(f.FD(), int64(fr.Start), unix.SEEK_DATA); err != unix.ENXIO && (err != nil || uint64(off) < fr.End) {
		t.Errorf("Seek(%#x, SEEK_DATA) after release = (%#x, %v), want ENXIO or offset >= %#x", fr.Start, off, err, fr.End)
	}
}
