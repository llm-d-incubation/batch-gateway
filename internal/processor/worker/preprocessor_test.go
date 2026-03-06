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
	err := checkCancellation(ctx, &cancelRequested)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestCheckPreProcessCancellation_CancelRequested(t *testing.T) {
	ctx := context.Background()
	var cancelRequested atomic.Bool
	cancelRequested.Store(true)

	err := checkCancellation(ctx, &cancelRequested)
	if !errors.Is(err, ErrCancelled) {
		t.Fatalf("expected ErrCancelled, got %v", err)
	}
}

func TestCheckPreProcessCancellation_None(t *testing.T) {
	ctx := context.Background()
	var cancelRequested atomic.Bool

	if err := checkCancellation(ctx, &cancelRequested); err != nil {
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

func TestExtractModelAndPrefixHash_Valid(t *testing.T) {
	modelID, prefixHash, err := extractModelAndPrefixHash([]byte("{\"body\":{\"model\":\"m1\"}}\n"))
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if modelID != "m1" {
		t.Fatalf("expected modelID m1, got %q", modelID)
	}
	if prefixHash != NoPrefixHash {
		t.Fatalf("expected NoPrefixHash (no system prompt), got %d", prefixHash)
	}
}

func TestExtractModelAndPrefixHash_WithSystemPrompt(t *testing.T) {
	line := []byte(`{"body":{"model":"m1","messages":[{"role":"system","content":"You are helpful."},{"role":"user","content":"Hi"}]}}` + "\n")
	modelID, prefixHash, err := extractModelAndPrefixHash(line)
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if modelID != "m1" {
		t.Fatalf("expected modelID m1, got %q", modelID)
	}
	if prefixHash == NoPrefixHash {
		t.Fatalf("expected non-zero prefixHash for system prompt")
	}

	// same system prompt should produce the same hash
	_, prefixHash2, _ := extractModelAndPrefixHash(line)
	if prefixHash != prefixHash2 {
		t.Fatalf("expected same prefixHash for identical system prompt, got %d vs %d", prefixHash, prefixHash2)
	}
}

func TestExtractModelAndPrefixHash_DifferentSystemPrompts(t *testing.T) {
	line1 := []byte(`{"body":{"model":"m1","messages":[{"role":"system","content":"Prompt A"}]}}` + "\n")
	line2 := []byte(`{"body":{"model":"m1","messages":[{"role":"system","content":"Prompt B"}]}}` + "\n")
	_, hash1, _ := extractModelAndPrefixHash(line1)
	_, hash2, _ := extractModelAndPrefixHash(line2)
	if hash1 == hash2 {
		t.Fatalf("expected different hashes for different system prompts, both got %d", hash1)
	}
}

func TestExtractModelAndPrefixHash_InvalidJSON(t *testing.T) {
	if _, _, err := extractModelAndPrefixHash([]byte("{invalid\n")); err == nil {
		t.Fatalf("expected json error")
	}
}

func TestExtractModelAndPrefixHash_EmptyModel(t *testing.T) {
	if _, _, err := extractModelAndPrefixHash([]byte("{\"body\":{\"model\":\"\"}}\n")); err == nil {
		t.Fatalf("expected empty model error")
	}
}
