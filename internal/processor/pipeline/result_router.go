package pipeline

import (
	"context"
	"encoding/json"
	"fmt"

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
	type resultMsg struct {
		resp *inference.GenerateResponse
		err  error
	}
	incomingCh := make(chan resultMsg)

	go func() {
		defer close(incomingCh)
		for {
			resp, err := b.client.GetResult(ctx)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				incomingCh <- resultMsg{err: err}
				return
			}
			incomingCh <- resultMsg{resp: resp}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return

		case msg, ok := <-incomingCh:
			if !ok {
				return
			}
			if msg.err != nil {
				b.logger.Error(msg.err, "GetResult failed, stopping broadcaster")
				return
			}
			result := asyncResult(msg.resp, b.logger)

			b.subscribers.Range(func(ch chan<- ResultItem, _ struct{}) bool {

				defer func() {
					if r := recover(); r != nil {
						b.logger.Info("Broadcast send recovered (subscriber likely unsubscribed)",
							"requestID", result.RequestID, "panic", r)
					}
				}()

				ch <- result

				return true
			})
		}
	}
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
