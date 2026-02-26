package worker

import (
	"context"
	"testing"
	"time"

	mockdb "github.com/llm-d-incubation/batch-gateway/internal/database/mock"
	mockfiles "github.com/llm-d-incubation/batch-gateway/internal/files_store/mock"
	"github.com/llm-d-incubation/batch-gateway/internal/inference"
	"github.com/llm-d-incubation/batch-gateway/internal/processor/config"
)

type fakeInferenceClient struct{}

func (f *fakeInferenceClient) Generate(ctx context.Context, req *inference.GenerateRequest) (*inference.GenerateResponse, *inference.ClientError) {
	return nil, nil
}

func validProcessorClients() *ProcessorClients {
	clients := NewProcessorClients(
		newMockBatchDBClient(),
		newMockFileDBClient(),
		mockfiles.NewMockBatchFilesClient(),
		mockdb.NewMockBatchPriorityQueueClient(),
		mockdb.NewMockBatchStatusClient(),
		mockdb.NewMockBatchEventChannelClient(),
		&fakeInferenceClient{},
	)
	return &clients
}

func TestNewProcessorClients_AssignsFields(t *testing.T) {
	clients := NewProcessorClients(
		newMockBatchDBClient(),
		newMockFileDBClient(),
		mockfiles.NewMockBatchFilesClient(),
		mockdb.NewMockBatchPriorityQueueClient(),
		mockdb.NewMockBatchStatusClient(),
		mockdb.NewMockBatchEventChannelClient(),
		&fakeInferenceClient{},
	)
	if clients.database == nil || clients.fileDatabase == nil || clients.files == nil || clients.priorityQueue == nil || clients.status == nil || clients.event == nil {
		t.Fatalf("expected all non-inference clients to be assigned")
	}
}

func TestProcessorClientsValidate_Table(t *testing.T) {
	base := validProcessorClients()
	tests := []struct {
		name    string
		mutate  func(c *ProcessorClients)
		wantErr bool
	}{
		{"ok", func(c *ProcessorClients) {}, false},
		{"missing database", func(c *ProcessorClients) { c.database = nil }, true},
		{"missing fileDatabase", func(c *ProcessorClients) { c.fileDatabase = nil }, true},
		{"missing files", func(c *ProcessorClients) { c.files = nil }, true},
		{"missing priorityQueue", func(c *ProcessorClients) { c.priorityQueue = nil }, true},
		{"missing status", func(c *ProcessorClients) { c.status = nil }, true},
		{"missing event", func(c *ProcessorClients) { c.event = nil }, true},
		{"missing inference", func(c *ProcessorClients) { c.inference = nil }, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := *base
			tt.mutate(&c)
			err := c.Validate()
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
	p := NewProcessor(cfg, &ProcessorClients{})

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
