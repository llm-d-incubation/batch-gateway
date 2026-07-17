package pipeline

import (
	"fmt"
	"os"
	"testing"

	"github.com/llm-d/llm-d-batch-gateway/internal/processor/config"
	"github.com/llm-d/llm-d-batch-gateway/internal/processor/metrics"
)

func TestMain(m *testing.M) {
	cfg := config.NewConfig()
	if err := metrics.InitMetrics(*cfg); err != nil {
		fmt.Fprintf(os.Stderr, "failed to init metrics for pipeline tests: %v\n", err)
		os.Exit(1)
	}

	os.Exit(m.Run())
}

func makeItems(n int, model string) []RequestItem {
	items := make([]RequestItem, n)
	for i := range items {
		items[i] = RequestItem{
			RequestID: fmt.Sprintf("req-%s-%d", model, i),
			CustomID:  fmt.Sprintf("c-%d", i),
			ModelID:   model,
			Endpoint:  "/v1/chat/completions",
		}
	}
	return items
}

func tempFile(t *testing.T) *os.File {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "test-*.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func readFile(t *testing.T, f *os.File) []byte {
	t.Helper()
	data, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func countLines(data []byte) int {
	return len(splitLines(data))
}

func splitLines(data []byte) [][]byte {
	trimmed := trimBytes(data)
	if len(trimmed) == 0 {
		return nil
	}
	return splitByNewline(trimmed)
}

func trimBytes(data []byte) []byte {
	for len(data) > 0 && (data[len(data)-1] == '\n' || data[len(data)-1] == ' ') {
		data = data[:len(data)-1]
	}
	return data
}

func splitByNewline(data []byte) [][]byte {
	var lines [][]byte
	for len(data) > 0 {
		idx := indexOf(data, '\n')
		if idx < 0 {
			lines = append(lines, data)
			break
		}
		lines = append(lines, data[:idx])
		data = data[idx+1:]
	}
	return lines
}

func indexOf(data []byte, b byte) int {
	for i, v := range data {
		if v == b {
			return i
		}
	}
	return -1
}
