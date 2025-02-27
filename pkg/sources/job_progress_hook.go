package sources

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	lru "github.com/hashicorp/golang-lru/v2"
	"github.com/trufflesecurity/trufflehog/v3/pkg/context"
)

// UnitHook implements JobProgressHook for tracking the progress of each
// individual unit.
type UnitHook struct {
	metrics *lru.Cache[string, *UnitMetrics]
	mu      sync.Mutex
	NoopHook
}

type UnitHookOpt func(*UnitHook)

func WithUnitHookCache(cache *lru.Cache[string, *UnitMetrics]) UnitHookOpt {
	return func(hook *UnitHook) { hook.metrics = cache }
}

func NewUnitHook(ctx context.Context, opts ...UnitHookOpt) *UnitHook {
	// lru.NewWithEvict can only fail if the size is < 0.
	cache, _ := lru.NewWithEvict(1024, func(key string, value *UnitMetrics) {
		if value.handled {
			return
		}
		ctx.Logger().Error(fmt.Errorf("eviction"), "dropping unit metric",
			"id", key,
			"metric", value,
		)
	})
	hook := UnitHook{metrics: cache}
	for _, opt := range opts {
		opt(&hook)
	}
	return &hook
}

// id is a helper method to generate an ID for the given job and unit.
func (u *UnitHook) id(ref JobProgressRef, unit SourceUnit) string {
	unitID := ""
	if unit != nil {
		unitID = unit.SourceUnitID()
	}
	return fmt.Sprintf("%d/%d/%s", ref.SourceID, ref.JobID, unitID)
}

func (u *UnitHook) StartUnitChunking(ref JobProgressRef, unit SourceUnit, start time.Time) {
	id := u.id(ref, unit)
	u.mu.Lock()
	defer u.mu.Unlock()

	u.metrics.Add(id, &UnitMetrics{
		Unit:      unit,
		Parent:    ref,
		StartTime: &start,
	})
}

func (u *UnitHook) EndUnitChunking(ref JobProgressRef, unit SourceUnit, end time.Time) {
	id := u.id(ref, unit)
	u.mu.Lock()
	defer u.mu.Unlock()

	metrics, ok := u.metrics.Get(id)
	if !ok {
		return
	}
	metrics.EndTime = &end
}

func (u *UnitHook) ReportChunk(ref JobProgressRef, unit SourceUnit, chunk *Chunk) {
	id := u.id(ref, unit)
	u.mu.Lock()
	defer u.mu.Unlock()

	metrics, ok := u.metrics.Get(id)
	if !ok && unit != nil {
		// The unit has been evicted.
		return
	} else if !ok && unit == nil {
		// This is a chunk from a non-unit source.
		metrics = &UnitMetrics{
			Unit:      nil,
			Parent:    ref,
			StartTime: ref.Snapshot().StartTime,
		}
		u.metrics.Add(id, metrics)
	}
	metrics.TotalChunks++
	metrics.TotalBytes += uint64(len(chunk.Data))
}

func (u *UnitHook) ReportError(ref JobProgressRef, err error) {
	u.mu.Lock()
	defer u.mu.Unlock()

	// Always add the error to the nil unit if it exists.
	if metrics, ok := u.metrics.Get(u.id(ref, nil)); ok {
		metrics.Errors = append(metrics.Errors, err)
	}

	// Check if it's a ChunkError for a specific unit.
	var chunkErr ChunkError
	if !errors.As(err, &chunkErr) {
		return
	}
	id := u.id(ref, chunkErr.Unit)

	metrics, ok := u.metrics.Get(id)
	if !ok {
		return
	}
	metrics.Errors = append(metrics.Errors, err)
}

func (u *UnitHook) Finish(ref JobProgressRef) {
	u.mu.Lock()
	defer u.mu.Unlock()
	// Clear out any metrics on this job. This covers the case for the
	// source running without unit support.
	prefix := u.id(ref, nil)
	for _, id := range u.metrics.Keys() {
		if !strings.HasPrefix(id, prefix) {
			continue
		}
		metric, ok := u.metrics.Get(id)
		if !ok {
			continue
		}
		// If the unit is nil, the source does not support units.
		// Use the overall job metrics instead.
		if metric.Unit == nil {
			snap := ref.Snapshot()
			metric.StartTime = snap.StartTime
			metric.EndTime = snap.EndTime
			metric.Errors = snap.Errors
		}
	}
}

// UnitMetrics gets all the currently active or newly finished metrics for this
// job. If a unit returned from this method has finished, it will be removed
// from the cache and no longer returned in successive calls to UnitMetrics().
func (u *UnitHook) UnitMetrics() []UnitMetrics {
	u.mu.Lock()
	defer u.mu.Unlock()
	output := make([]UnitMetrics, 0, u.metrics.Len())
	for _, id := range u.metrics.Keys() {
		metric, ok := u.metrics.Get(id)
		if !ok {
			continue
		}
		output = append(output, *metric)
		if metric.IsFinished() {
			metric.handled = true
			u.metrics.Remove(id)
		}
	}
	return output
}

type UnitMetrics struct {
	Unit   SourceUnit     `json:"unit,omitempty"`
	Parent JobProgressRef `json:"parent,omitempty"`
	// Start and end time for chunking this unit.
	StartTime *time.Time `json:"start_time,omitempty"`
	EndTime   *time.Time `json:"end_time,omitempty"`
	// Total number of chunks produced from this unit.
	TotalChunks uint64 `json:"total_chunks,omitempty"`
	// Total number of bytes produced from this unit.
	TotalBytes uint64 `json:"total_bytes,omitempty"`
	// All errors encountered by this unit.
	Errors []error `json:"errors,omitempty"`
	// Flag to mark that these metrics were intentionally evicted from
	// the cache.
	handled bool
}

func (u UnitMetrics) IsFinished() bool {
	return u.EndTime != nil
}

// ElapsedTime is a convenience method that provides the elapsed time the job
// has been running. If it hasn't started yet, 0 is returned. If it has
// finished, the total time is returned.
func (u UnitMetrics) ElapsedTime() time.Duration {
	if u.StartTime == nil {
		return 0
	}
	if u.EndTime == nil {
		return time.Since(*u.StartTime)
	}
	return u.EndTime.Sub(*u.StartTime)
}

// NoopHook implements JobProgressHook by doing nothing. This is useful for
// embedding in other structs to overwrite only the methods of the interface
// that you care about.
type NoopHook struct{}

func (NoopHook) Start(JobProgressRef, time.Time)                         {}
func (NoopHook) End(JobProgressRef, time.Time)                           {}
func (NoopHook) StartEnumerating(JobProgressRef, time.Time)              {}
func (NoopHook) EndEnumerating(JobProgressRef, time.Time)                {}
func (NoopHook) StartUnitChunking(JobProgressRef, SourceUnit, time.Time) {}
func (NoopHook) EndUnitChunking(JobProgressRef, SourceUnit, time.Time)   {}
func (NoopHook) ReportError(JobProgressRef, error)                       {}
func (NoopHook) ReportUnit(JobProgressRef, SourceUnit)                   {}
func (NoopHook) ReportChunk(JobProgressRef, SourceUnit, *Chunk)          {}
func (NoopHook) Finish(JobProgressRef)                                   {}
