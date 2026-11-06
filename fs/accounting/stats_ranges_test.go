package accounting

import (
	"context"
	"fmt"
	"math/rand"
	"slices"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// eRef is a reference copy of the transfer time bookkeeping that
// merges every finished range into one sorted list. It takes the
// current time as an argument so a test can use a logical clock.
type eRef struct {
	started     []*Transfer
	old         timeRanges
	oldDuration time.Duration
}

func (r *eRef) add(tr *Transfer) {
	r.started = append(r.started, tr)
}

func (r *eRef) remove(transfer *Transfer, now time.Time) {
	i := slices.Index(r.started, transfer)
	if i < 0 {
		return
	}
	start, end := transfer.TimeRange()
	if end.IsZero() {
		end = now
	}
	r.old = append(r.old, timeRange{start, end})
	r.old.merge()

	r.started = slices.Delete(r.started, i, i+1)

	oldestStart := now
	for _, tr := range r.started {
		start, _ := tr.TimeRange()
		if start.Before(oldestStart) {
			oldestStart = start
		}
	}
	r.oldDuration += r.old.cull(oldestStart)
}

func (r *eRef) prune(maxKept int, now time.Time) bool {
	if len(r.started) > maxKept {
		for _, tr := range r.started {
			if tr.IsDone() {
				r.remove(tr, now)
				return true
			}
		}
	}
	return false
}

func (r *eRef) total(now time.Time) time.Duration {
	return eRefTotal(now, r.oldDuration, r.old, r.started)
}

func eRefTotal(now time.Time, oldDuration time.Duration, old timeRanges, started []*Transfer) time.Duration {
	trs := make(timeRanges, len(old), len(old)+len(started))
	copy(trs, old)
	for _, tr := range started {
		start, end := tr.TimeRange()
		if end.IsZero() {
			end = now
		}
		trs = append(trs, timeRange{start, end})
	}
	trs.merge()
	return oldDuration + trs.total()
}

// eRefSum is the reference total of the sum of several stats.
func eRefSum(now time.Time, refs ...*eRef) time.Duration {
	var (
		oldDuration time.Duration
		old         timeRanges
		started     []*Transfer
	)
	for _, r := range refs {
		oldDuration += r.oldDuration
		old = append(old, r.old...)
		started = append(started, r.started...)
	}
	return eRefTotal(now, oldDuration, old, started)
}

func eTotalAt(s *StatsInfo, now time.Time) time.Duration {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s._totalDurationAt(now)
}

func eFinish(tr *Transfer, at time.Time) {
	tr.mu.Lock()
	tr.completedAt = at
	tr.mu.Unlock()
}

func TestStatsRangesBoundedUnderLongTransfer(t *testing.T) {
	const n = 10000
	base := time.Now().Add(-time.Hour)
	s := NewStats(context.Background())
	long := &Transfer{startedAt: base}
	s.AddTransfer(long)

	for i := range n {
		// distinct start and end for every transfer, all after long
		start := base.Add(time.Duration(2*i+1) * time.Millisecond)
		tr := &Transfer{startedAt: start}
		s.AddTransfer(tr)
		eFinish(tr, start.Add(time.Millisecond/2))
		s.RemoveTransfer(tr)
		require.LessOrEqual(t, len(s.oldTimeRanges), 2, "after removing transfer %d", i)
	}

	assert.LessOrEqual(t, len(s.oldTimeRanges), 2)
	assert.Equal(t, time.Hour, eTotalAt(s, base.Add(time.Hour)))
}

func TestStatsRangesKeepsTransferStartedBeforeCovering(t *testing.T) {
	base := time.Now().Add(-time.Hour)
	s := NewStats(context.Background())
	early := &Transfer{startedAt: base}
	covering := &Transfer{startedAt: base.Add(3 * time.Second)}
	s.AddTransfer(early)
	s.AddTransfer(covering)
	eFinish(early, base.Add(5*time.Second))
	s.RemoveTransfer(early)

	assert.Equal(t, timeRanges{{base, base.Add(5 * time.Second)}}, s.oldTimeRanges)
	assert.Equal(t, 10*time.Second, eTotalAt(s, base.Add(10*time.Second)))
}

func TestStatsRangesDoneTransferIsNotCovering(t *testing.T) {
	base := time.Now().Add(-time.Hour)
	s := NewStats(context.Background())
	done := &Transfer{startedAt: base}
	short := &Transfer{startedAt: base.Add(2 * time.Second)}
	s.AddTransfer(done)
	s.AddTransfer(short)
	eFinish(done, base.Add(time.Second))
	eFinish(short, base.Add(3*time.Second))
	s.RemoveTransfer(short)

	assert.Equal(t, timeRanges{{base.Add(2 * time.Second), base.Add(3 * time.Second)}}, s.oldTimeRanges)
	assert.Equal(t, 2*time.Second, eTotalAt(s, base.Add(10*time.Second)))
}

func TestStatsRangesResetCounters(t *testing.T) {
	base := time.Now().Add(-time.Hour)
	s := NewStats(context.Background())
	a := &Transfer{startedAt: base}
	b := &Transfer{startedAt: base.Add(time.Second)}
	s.AddTransfer(a)
	s.AddTransfer(b)
	eFinish(b, base.Add(2*time.Second))
	s.RemoveTransfer(b)

	s.ResetCounters()

	assert.Equal(t, time.Duration(0), eTotalAt(s, base.Add(10*time.Second)))
	assert.Empty(t, s.oldTimeRanges)
}

func TestStatsRangesResetCountersClearsKeptRange(t *testing.T) {
	base := time.Now().Add(-time.Hour)
	s := NewStats(context.Background())
	early := &Transfer{startedAt: base}
	covering := &Transfer{startedAt: base.Add(3 * time.Second)}
	s.AddTransfer(early)
	s.AddTransfer(covering)
	eFinish(early, base.Add(5*time.Second))
	s.RemoveTransfer(early)
	require.Len(t, s.oldTimeRanges, 1)

	s.ResetCounters()

	assert.Equal(t, time.Duration(0), eTotalAt(s, base.Add(10*time.Second)))
	assert.Empty(t, s.oldTimeRanges)
}

// With ties, events can share a timestamp, which gives equal, nested
// and touching ranges. The group sum adds each group's culled duration,
// so which ranges a group has culled must match the reference exactly,
// including when a range starts where a running transfer starts.
func TestStatsRangesMatchReference(t *testing.T) {
	for _, ties := range []bool{true, false} {
		t.Run(fmt.Sprintf("ties=%v", ties), func(t *testing.T) {
			eRunMatchReference(t, ties)
		})
	}
}

func eRunMatchReference(t *testing.T, ties bool) {
	const (
		sequences = 300
		steps     = 120
	)
	droppedSomething := false

	for seed := int64(1); seed <= sequences; seed++ {
		rng := rand.New(rand.NewSource(seed))
		ctx, ci := fs.AddConfig(context.Background())
		ci.Transfers = 1

		// all logical times are far in the past so the real clock that
		// RemoveTransfer and PruneTransfers read is never before them
		now := time.Now().Add(-24 * time.Hour)

		sg := newStatsGroups()
		var (
			stats [2]*StatsInfo
			refs  [2]*eRef
		)
		for i := range stats {
			stats[i] = NewStats(ctx)
			stats[i].SetMaxCompletedTransfers(1)
			refs[i] = &eRef{}
			sg.set(ctx, fmt.Sprintf("g%d", i), stats[i])
		}

		for step := range steps {
			g := rng.Intn(len(stats))
			s, ref := stats[g], refs[g]
			var what string
			switch op := rng.Intn(10); {
			case op < 3:
				what = "add"
				tr := &Transfer{startedAt: now}
				s.AddTransfer(tr)
				ref.add(tr)
			case op < 5:
				what = "finish"
				if open := eFilter(ref.started, false); len(open) > 0 {
					eFinish(open[rng.Intn(len(open))], now)
				}
			case op < 7:
				what = "remove"
				if done := eFilter(ref.started, true); len(done) > 0 {
					tr := done[rng.Intn(len(done))]
					s.RemoveTransfer(tr)
					ref.remove(tr, now)
				}
			case op < 8:
				what = "prune"
				got := s.PruneTransfers()
				want := ref.prune(1+ci.Transfers, now)
				require.Equal(t, want, got, "seed %d step %d prune result", seed, step)
			case op < 9:
				what = "removeDone"
				s.RemoveDoneTransfers()
				for ref.prune(1+ci.Transfers, now) {
				}
			default:
				what = "tick"
			}
			advance := rng.Intn(4)
			if !ties {
				advance++
			}
			now = now.Add(time.Duration(advance) * time.Second)

			for i := range stats {
				require.Equal(t, refs[i].total(now), eTotalAt(stats[i], now),
					"seed %d step %d (%s) stats %d", seed, step, what, i)
				if len(stats[i].oldTimeRanges) < len(refs[i].old) {
					droppedSomething = true
				}
			}
			require.Equal(t, eRefSum(now, refs[:]...), eTotalAt(sg.sum(ctx), now),
				"seed %d step %d (%s) group sum", seed, step, what)
		}
	}
	assert.True(t, droppedSomething, "no sequence made the ranges list shorter than the reference")
}

// eFilter returns the transfers whose done state is done.
func eFilter(trs []*Transfer, done bool) (out []*Transfer) {
	for _, tr := range trs {
		if tr.IsDone() == done {
			out = append(out, tr)
		}
	}
	return out
}
