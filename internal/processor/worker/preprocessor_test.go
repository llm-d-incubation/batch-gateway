package worker

import (
	"bufio"
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
)

type errReader struct{}

func (errReader) Read(_ []byte) (int, error) {
	return 0, errors.New("boom")
}

func TestCheckPreProcessCancellation_ContextDone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var cancelRequested atomic.Bool
	err := checkPreProcessCancellation(ctx, &cancelRequested)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestCheckPreProcessCancellation_CancelRequested(t *testing.T) {
	ctx := context.Background()
	var cancelRequested atomic.Bool
	cancelRequested.Store(true)

	err := checkPreProcessCancellation(ctx, &cancelRequested)
	if !errors.Is(err, ErrCancelled) {
		t.Fatalf("expected ErrCancelled, got %v", err)
	}
}

func TestCheckPreProcessCancellation_None(t *testing.T) {
	ctx := context.Background()
	var cancelRequested atomic.Bool

	if err := checkPreProcessCancellation(ctx, &cancelRequested); err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
}

func TestReadNormalizedLine_EOFEmpty(t *testing.T) {
	r := bufio.NewReader(strings.NewReader(""))

	line, done, err := readNormalizedLine(r)
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if !done {
		t.Fatalf("expected done=true on empty EOF")
	}
	if line != nil {
		t.Fatalf("expected nil line on empty EOF, got %q", string(line))
	}
}

func TestReadNormalizedLine_AppendsTrailingNewline(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("{\"body\":{\"model\":\"m1\"}}"))

	line, done, err := readNormalizedLine(r)
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if done {
		t.Fatalf("expected done=false")
	}
	if got := string(line); got != "{\"body\":{\"model\":\"m1\"}}\n" {
		t.Fatalf("unexpected normalized line: %q", got)
	}
}

func TestReadNormalizedLine_ReaderError(t *testing.T) {
	r := bufio.NewReader(errReader{})

	line, done, err := readNormalizedLine(r)
	if err == nil {
		t.Fatalf("expected read error")
	}
	if done {
		t.Fatalf("expected done=false on read error")
	}
	if line != nil {
		t.Fatalf("expected nil line on read error")
	}
}

func TestExtractModelID_Valid(t *testing.T) {
	modelID, err := extractModelID([]byte("{\"body\":{\"model\":\"m1\"}}\n"))
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if modelID != "m1" {
		t.Fatalf("expected modelID m1, got %q", modelID)
	}
}

func TestExtractModelID_InvalidJSON(t *testing.T) {
	if _, err := extractModelID([]byte("{invalid\n")); err == nil {
		t.Fatalf("expected json error")
	}
}

func TestExtractModelID_EmptyModel(t *testing.T) {
	if _, err := extractModelID([]byte("{\"body\":{\"model\":\"\"}}\n")); err == nil {
		t.Fatalf("expected empty model error")
	}
}
