package worker

import (
	"context"
	"testing"
	"time"

	"github.com/llm-d-incubation/batch-gateway/internal/processor/config"
	"github.com/llm-d-incubation/batch-gateway/internal/util/clientset"
)

func TestClientsetFields_Assigned(t *testing.T) {
	cs := validProcessorClients()
	if cs.BatchDB == nil || cs.FileDB == nil || cs.File == nil || cs.Queue == nil || cs.Status == nil || cs.Event == nil || cs.Inference == nil {
		t.Fatalf("expected all clients to be assigned")
	}
}

func TestNewProcessor_InvalidNumWorkers(t *testing.T) {
	cfg := config.NewConfig()
	cfg.NumWorkers = 0
	_, err := NewProcessor(cfg, &clientset.Clientset{})
	if err == nil {
		t.Fatalf("expected error for NumWorkers=0")
	}
}

func TestNewProcessor_InvalidGlobalConcurrency(t *testing.T) {
	cfg := config.NewConfig()
	cfg.GlobalConcurrency = -1
	_, err := NewProcessor(cfg, &clientset.Clientset{})
	if err == nil {
		t.Fatalf("expected error for GlobalConcurrency=-1")
	}
}

func TestProcessorPrepare_ReturnsValidationError(t *testing.T) {
	cfg := config.NewConfig()
	p := mustNewProcessor(t, cfg, &clientset.Clientset{})

	if err := p.prepare(context.Background()); err == nil {
		t.Fatalf("expected validation error")
	}
}

func TestProcessorRun_ContextCanceled_ReturnsNil(t *testing.T) {
	cfg := config.NewConfig()
	cfg.PollInterval = 5 * time.Millisecond
	clients := validProcessorClients()
	p := mustNewProcessor(t, cfg, clients)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := p.Run(ctx, nil); err != nil {
		t.Fatalf("expected nil on canceled context run, got %v", err)
	}
}

func TestProcessorStop_DoneAndContextPaths(t *testing.T) {
	cfg := config.NewConfig()
	p := mustNewProcessor(t, cfg, validProcessorClients())

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
	p := mustNewProcessor(t, cfg, validProcessorClients())

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
