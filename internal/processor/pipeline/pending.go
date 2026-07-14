package pipeline

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/llm-d/llm-d-batch-gateway/internal/shared/syncutil"
)

// PendingRequests tracks in-flight requests by RequestID.
// The dispatcher stores entries before dispatching; the collector
// resolves them when results arrive.
type PendingRequests struct {
	m     *syncutil.MutexMap[string, RequestItem]
	count atomic.Int64
	once  sync.Once
	done  chan struct{}
}

func NewPendingRequests() *PendingRequests {
	return &PendingRequests{
		m:    syncutil.NewMutexMap[string, RequestItem](),
		done: make(chan struct{}),
	}
}

func (p *PendingRequests) Store(msg RequestItem) {
	p.m.Store(msg.RequestID, msg)
	p.count.Add(1)
}

// Resolve enriches a result with request metadata. Returns true if the
// result is accepted: either it already has metadata (cancels, inline errors)
// or it was found in the pending map (async inference results).
// Returns false only for broadcast results that belong to another job.
func (p *PendingRequests) Resolve(result *ResultItem) bool {
	if result.CustomID != "" {
		// Already has metadata (inline error, cancel). Still remove from
		// the pending map so Wait() doesn't block forever.
		if _, ok := p.m.LoadAndDelete(result.RequestID); ok {
			if p.count.Add(-1) == 0 {
				p.once.Do(func() { close(p.done) })
			}
		}
		return true
	}
	if msg, ok := p.m.LoadAndDelete(result.RequestID); ok {
		if p.count.Add(-1) == 0 {
			p.once.Do(func() { close(p.done) })
		}
		result.CustomID = msg.CustomID
		result.ModelID = msg.ModelID
		return true
	}
	return false
}

// Wait blocks until all pending entries are resolved or ctx is cancelled.
func (p *PendingRequests) Wait(ctx context.Context) {
	if p.count.Load() == 0 {
		return
	}
	select {
	case <-ctx.Done():
	case <-p.done:
	}
}
