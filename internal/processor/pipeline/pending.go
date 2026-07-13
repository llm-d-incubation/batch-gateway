package pipeline

import (
	"context"
	"sync"
	"sync/atomic"
)

// PendingRequests tracks in-flight requests by RequestID.
// The dispatcher stores entries before dispatching; the collector
// resolves them when results arrive.
type PendingRequests struct {
	m     sync.Map
	count atomic.Int64
	done  chan struct{}
}

func NewPendingRequests() *PendingRequests {
	return &PendingRequests{done: make(chan struct{}, 1)}
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
		return true
	}
	val, ok := p.m.LoadAndDelete(result.RequestID)
	if !ok {
		return false
	}
	if p.count.Add(-1) == 0 {
		close(p.done)
	}
	msg, ok := val.(RequestItem)
	if !ok {
		return false
	}
	result.CustomID = msg.CustomID
	result.ModelID = msg.ModelID
	return true
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
