package worker

import (
	"context"
	"testing"
	"time"

	mockdb "github.com/llm-d-incubation/batch-gateway/internal/database/mock"
	mockfiles "github.com/llm-d-incubation/batch-gateway/internal/files_store/mock"
	"github.com/llm-d-incubation/batch-gateway/internal/inference"
	"github.com/llm-d-incubation/batch-gateway/internal/processor/config"
	"github.com/llm-d-incubation/batch-gateway/internal/util/clientset"
)

type fakeInferenceClient struct{}

func (f *fakeInferenceClient) Generate(ctx context.Context, req *inference.GenerateRequest) (*inference.GenerateResponse, *inference.ClientError) {
	return nil, nil
}

func validProcessorClients() *clientset.Clientset {
	return &clientset.Clientset{
		BatchDB:   newMockBatchDBClient(),
		FileDB:    newMockFileDBClient(),
		File:      mockfiles.NewMockBatchFilesClient(),
		Queue:     mockdb.NewMockBatchPriorityQueueClient(),
		Status:    mockdb.NewMockBatchStatusClient(),
		Event:     mockdb.NewMockBatchEventChannelClient(),
		Inference: &fakeInferenceClient{},
	}
}

func TestClientsetFields_Assigned(t *testing.T) {
	cs := validProcessorClients()
	if cs.BatchDB == nil || cs.FileDB == nil || cs.File == nil || cs.Queue == nil || cs.Status == nil || cs.Event == nil || cs.Inference == nil {
		t.Fatalf("expected all clients to be assigned")
	}
}

func TestClientsetValidate_Table(t *testing.T) {
	base := validProcessorClients()
	tests := []struct {
		name    string
		mutate  func(c *clientset.Clientset)
		wantErr bool
	}{
		{"ok", func(c *clientset.Clientset) {}, false},
		{"missing BatchDB", func(c *clientset.Clientset) { c.BatchDB = nil }, true},
		{"missing FileDB", func(c *clientset.Clientset) { c.FileDB = nil }, true},
		{"missing File", func(c *clientset.Clientset) { c.File = nil }, true},
		{"missing Queue", func(c *clientset.Clientset) { c.Queue = nil }, true},
		{"missing Status", func(c *clientset.Clientset) { c.Status = nil }, true},
		{"missing Event", func(c *clientset.Clientset) { c.Event = nil }, true},
		{"missing Inference", func(c *clientset.Clientset) { c.Inference = nil }, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := *base
			tt.mutate(&c)
			err := ValidateClientset(&c)
			if tt.wantErr && err == nil {
				t.Fatalf("expected error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestProcessorPrepare_ReturnsValidationError(t *testing.T) {
	cfg := config.NewConfig()
	p := NewProcessor(cfg, &clientset.Clientset{})

	if err := p.prepare(context.Background()); err == nil {
		t.Fatalf("expected validation error")
	}
}

func TestProcessorRun_ContextCanceled_ReturnsNil(t *testing.T) {
	cfg := config.NewConfig()
	cfg.PollInterval = 5 * time.Millisecond
	clients := validProcessorClients()
	p := NewProcessor(cfg, clients)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := p.Run(ctx); err != nil {
		t.Fatalf("expected nil on canceled context run, got %v", err)
	}
}

func TestProcessorStop_DoneAndContextPaths(t *testing.T) {
	cfg := config.NewConfig()
	p := NewProcessor(cfg, validProcessorClients())

	// done path
	p.Stop(context.Background())

	// context-done path
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p.Stop(ctx)
}

func TestProcessorTokenHelpers(t *testing.T) {
	cfg := config.NewConfig()
	cfg.NumWorkers = 1
	cfg.PollInterval = 5 * time.Millisecond
	p := NewProcessor(cfg, validProcessorClients())

	if !p.acquire(context.Background()) {
		t.Fatalf("expected acquire true")
	}
	p.releaseForNextPoll()

	if !p.acquire(context.Background()) {
		t.Fatalf("expected acquire true second time")
	}
	p.release()

	if !p.acquire(context.Background()) {
		t.Fatalf("expected acquire before releaseAndWaitPollInterval")
	}
	if !p.releaseAndWaitPollInterval(context.Background()) {
		t.Fatalf("expected wait true with active context")
	}

	if !p.acquire(context.Background()) {
		t.Fatalf("expected acquire before canceled releaseAndWaitPollInterval")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if p.releaseAndWaitPollInterval(ctx) {
		t.Fatalf("expected false when context canceled")
	}
}

func TestProcessorRelease_GracefulWhenChannelFull(t *testing.T) {
	cfg := config.NewConfig()
	cfg.NumWorkers = 1
	p := NewProcessor(cfg, validProcessorClients())

	// channel is already full (pre-filled in NewProcessor), release should not panic
	p.release()

	// verify the token channel is still at capacity
	if len(p.tokens) != cfg.NumWorkers {
		t.Fatalf("expected token channel length %d, got %d", cfg.NumWorkers, len(p.tokens))
	}
}
