package pipeline

import (
	"context"
	"sync/atomic"

	"github.com/llm-d/llm-d-batch-gateway/internal/shared/syncutil"
)

// PendingRequests tracks in-flight requests by RequestID.
// The dispatcher stores entries before dispatching; the collector
// resolves them when results arrive.
type PendingRequests struct {
	numRequests int64
	m           *syncutil.MutexMap[string, RequestItem]
	pending     atomic.Int64
	done        chan struct{}
}

func NewPendingRequests(numRequests int64) *PendingRequests {
	return &PendingRequests{
		numRequests: numRequests,
		m:           syncutil.NewMutexMap[string, RequestItem](),
		done:        make(chan struct{}, 1),
	}
}

// NumRequests returns the total number of requests in the job.
func (p *PendingRequests) NumRequests() int64 {
	return p.numRequests
}

// Store registers a request as pending an async result.
func (p *PendingRequests) Store(msg RequestItem) {
	p.m.Store(msg.RequestID, msg)
	p.pending.Add(1)
}

func (p *PendingRequests) decrement() {
	if p.pending.Add(-1) == 0 {
		select {
		case p.done <- struct{}{}:
		default:
		}
	}
}

// Resolve enriches a result with request metadata. Returns true if the
// result is accepted: either it already has metadata (cancels, inline errors)
// or it was found in the pending map (async inference results).
// Returns false only for broadcast results that belong to another job.
func (p *PendingRequests) Resolve(result *ResultItem) bool {
	if result.Error != nil {
		if _, ok := p.m.LoadAndDelete(result.RequestID); ok {
			p.decrement()
			return true
		}
		return result.CustomID != ""
	}
	if result.CustomID != "" {
		if _, ok := p.m.LoadAndDelete(result.RequestID); ok {
			p.decrement()
		}
		return true
	}
	if msg, ok := p.m.LoadAndDelete(result.RequestID); ok {
		p.decrement()
		result.CustomID = msg.CustomID
		result.ModelID = msg.ModelID
		result.SubmittedAt = msg.SubmittedAt
		return true
	}
	return false
}

// Wait blocks until all pending entries are resolved or ctx is cancelled.
func (p *PendingRequests) Wait(ctx context.Context) {
	for {
		if p.pending.Load() == 0 {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-p.done:
		}
	}
}

// IDs returns the request IDs of all still-pending entries.
func (p *PendingRequests) IDs() []string {
	return p.m.Keys()
}

// DrainUnresolved removes all remaining entries from the pending map and
// calls fn for each. Used after cancellation to emit error results for
// submitted-but-uncollected requests.
func (p *PendingRequests) DrainUnresolved(fn func(RequestItem)) {
	for _, id := range p.m.Keys() {
		if msg, ok := p.m.LoadAndDelete(id); ok {
			fn(msg)
			p.decrement()
		}
	}
}
