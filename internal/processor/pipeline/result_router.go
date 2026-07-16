package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/go-logr/logr"

	"github.com/llm-d/llm-d-batch-gateway/internal/shared/syncutil"
	batch_types "github.com/llm-d/llm-d-batch-gateway/internal/shared/types"
	"github.com/llm-d/llm-d-batch-gateway/pkg/clients/inference"
)

const defaultResultBuffer = 64

// BroadcasterGroup is a set of broadcasters for subscribe/unsubscribe.
// Lifecycle (Run/Wait) is owned by the registry that created them.
type BroadcasterGroup struct {
	broadcasters []*ResultBroadcaster
}

func NewBroadcasterGroup(broadcasters []*ResultBroadcaster) *BroadcasterGroup {
	return &BroadcasterGroup{broadcasters: broadcasters}
}

func (bs *BroadcasterGroup) Subscribe(ch chan<- ResultItem) {
	for _, b := range bs.broadcasters {
		b.Subscribe(ch)
	}
}

func (bs *BroadcasterGroup) Unsubscribe(ch chan<- ResultItem) {
	for _, b := range bs.broadcasters {
		b.Unsubscribe(ch)
	}
}

// ResultBroadcaster reads results from a shared async client and
// broadcasts to all subscribed channels.
type ResultBroadcaster struct {
	client      inference.AsyncInferenceClient
	subscribers *syncutil.MutexMap[chan<- ResultItem, struct{}]
	logger      logr.Logger
}

func NewResultBroadcaster(client inference.AsyncInferenceClient, logger logr.Logger) *ResultBroadcaster {
	return &ResultBroadcaster{
		client:      client,
		subscribers: syncutil.NewMutexMap[chan<- ResultItem, struct{}](),
		logger:      logger,
	}
}

// Subscribe registers dest to receive all results.
// dest should be buffered to avoid blocking the broadcaster.
func (b *ResultBroadcaster) Subscribe(dest chan<- ResultItem) {
	b.subscribers.Store(dest, struct{}{})
}

// Unsubscribe removes dest from the broadcast list.
func (b *ResultBroadcaster) Unsubscribe(dest chan<- ResultItem) {
	b.subscribers.Delete(dest)
}

// Run reads results and broadcasts to all subscribers.
func (b *ResultBroadcaster) Run(ctx context.Context) {
	incomingCh := make(chan *inference.GenerateResponse)

	go func() {
		defer close(incomingCh)
		backoff := 100 * time.Millisecond
		const maxBackoff = 10 * time.Second
		for ctx.Err() == nil {
			resp, err := b.client.GetResult(ctx)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				b.logger.Error(err, "GetResult failed, retrying", "backoff", backoff)
				select {
				case <-time.After(backoff):
				case <-ctx.Done():
					return
				}
				backoff = min(backoff*2, maxBackoff)
				continue
			}
			backoff = 100 * time.Millisecond
			select {
			case incomingCh <- resp:
			case <-ctx.Done():
				return
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return

		case resp, ok := <-incomingCh:
			if !ok {
				return
			}
			result := asyncResult(resp, b.logger)

			for _, ch := range b.subscribers.Keys() {
				b.safeChannelSend(result, ch)
			}
		}
	}
}

// safeChannelSend sends to ch and recovers from the panic if the channel
// was closed concurrently (subscriber unsubscribed). The only operation in
// this function is a channel send, so the only possible panic is
// "send on closed channel".
func (b *ResultBroadcaster) safeChannelSend(result ResultItem, ch chan<- ResultItem) {
	defer func() {
		if r := recover(); r != nil {
			if err, ok := r.(error); ok && err.Error() == "send on closed channel" {
				b.logger.Info("Broadcast send recovered (subscriber unsubscribed)",
					"requestID", result.RequestID)
			} else {
				panic(r)
			}
		}
	}()
	ch <- result
}

func asyncResult(resp *inference.GenerateResponse, logger logr.Logger) ResultItem {
	result := ResultItem{
		RequestID: resp.RequestID,
	}
	if resp.Response == nil {
		result.Error = &OutputError{Code: "server_error", Message: "async response has no body"}
		return result
	}
	var body map[string]any
	if err := json.Unmarshal(resp.Response, &body); err != nil {
		logger.Error(err, "Failed to unmarshal async response", "requestID", resp.RequestID)
		result.Error = &OutputError{
			Code:    "parse_error",
			Message: fmt.Sprintf("response body could not be parsed: %v", err),
		}
		return result
	}
	result.Response = &batch_types.ResponseData{
		StatusCode: 200,
		RequestID:  resp.RequestID,
		Body:       body,
	}
	return result
}
