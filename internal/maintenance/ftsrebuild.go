package maintenance

import (
	"context"
	"log/slog"
	"time"

	"github.com/ragmux/ragmux/internal/store"
)

// Defaults of the FTS rebuild job.
const (
	// DefaultFTSRebuildPause is the wait between two batches of one store, so
	// a long rebuild leaves room for the ingest and search traffic around it
	// (the same pattern as the retention job's DefaultBatchPause).
	DefaultFTSRebuildPause = 100 * time.Millisecond
	// DefaultFTSRebuildPoll is how often the job looks for work nobody kicked
	// it for: a rebuild a dead process left half done, or one another replica
	// started and then lost.
	DefaultFTSRebuildPoll = time.Minute
	// ftsBusyRetry is the wait before a store another replica is rebuilding
	// is looked at again.
	ftsBusyRetry = 5 * time.Second
)

// FTSRebuilder keeps the per-store-language full-text column (chunks.fts,
// ADR-027) in step with the store's language. A language change marks the
// column not applied and resets the store's cursor; this job notices that
// (Kick, or its own poll), rewrites the column in batches and, when it is
// done, marks the language applied. Searches run on the 'simple' column the
// whole time. A process that dies mid-rebuild loses nothing: the cursor is
// stored with each batch and the next process continues from it.
type FTSRebuilder struct {
	Store *store.Store
	Log   *slog.Logger
	// BatchSize and Pause override DefaultFTSRebuildBatch and
	// DefaultFTSRebuildPause when positive; Poll overrides
	// DefaultFTSRebuildPoll. Tests set them.
	BatchSize int
	Pause     time.Duration
	Poll      time.Duration

	kick chan struct{}
}

// NewFTSRebuilder returns a rebuilder ready to Run.
func NewFTSRebuilder(st *store.Store, log *slog.Logger) *FTSRebuilder {
	return &FTSRebuilder{Store: st, Log: log, kick: make(chan struct{}, 1)}
}

// Kick asks the job to look for work now. It never blocks and may be called
// from any goroutine; kicks that arrive while a pass runs are merged into one
// more pass.
func (f *FTSRebuilder) Kick() {
	if f == nil {
		return
	}
	select {
	case f.kick <- struct{}{}:
	default:
	}
}

func (f *FTSRebuilder) log() *slog.Logger {
	if f.Log != nil {
		return f.Log
	}
	return slog.Default()
}

// Run works until ctx ends. It makes a pass at start (to resume what a dead
// process left), then on every Kick and every Poll.
func (f *FTSRebuilder) Run(ctx context.Context) {
	poll := f.Poll
	if poll <= 0 {
		poll = DefaultFTSRebuildPoll
	}
	t := time.NewTicker(poll)
	defer t.Stop()
	for {
		f.RunOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-f.kick:
		case <-t.C:
		}
	}
}

// RunOnce rebuilds every store that needs it, one store after the other, and
// returns when none is left (or ctx ended). A store another replica is
// rebuilding is skipped for now; a failing store does not hold up the others
// and is tried again by the next pass.
func (f *FTSRebuilder) RunOnce(ctx context.Context) {
	ids, err := f.Store.StoresNeedingFTSRebuild(ctx)
	if err != nil {
		if ctx.Err() == nil {
			f.log().Warn("fts rebuild: listing stores failed", "err", err)
		}
		return
	}
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		f.rebuildStore(ctx, id)
	}
}

// rebuildStore runs batches until the store is done, loses the lock to another
// replica, fails, or ctx ends.
func (f *FTSRebuilder) rebuildStore(ctx context.Context, id int64) {
	batch := f.BatchSize
	if batch <= 0 {
		batch = store.DefaultFTSRebuildBatch
	}
	pause := f.Pause
	if pause <= 0 {
		pause = DefaultFTSRebuildPause
	}
	total := 0
	start := time.Now()
	for {
		res, err := f.Store.RebuildFTSBatch(ctx, id, batch)
		if err != nil {
			if ctx.Err() == nil {
				f.log().Warn("fts rebuild: batch failed; it is retried by the next pass", "store_id", id, "err", err)
			}
			return
		}
		total += res.Rows
		switch {
		case res.Busy:
			// Another replica is on it; its progress is in the cursor either way.
			return
		case res.Done:
			if total > 0 {
				f.log().Info("fts rebuild finished", "store_id", id, "chunks", total, "took", time.Since(start).Round(time.Millisecond))
			}
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(pause):
		}
	}
}
