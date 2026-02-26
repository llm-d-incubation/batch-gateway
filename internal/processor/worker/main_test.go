package worker

import (
	"fmt"
	"os"
	"testing"

	"github.com/llm-d-incubation/batch-gateway/internal/processor/config"
	"github.com/llm-d-incubation/batch-gateway/internal/processor/metrics"
)

// initialize metrics for worker tests
func TestMain(m *testing.M) {
	cfg := config.NewConfig()
	if err := metrics.InitMetrics(*cfg); err != nil {
		fmt.Fprintf(os.Stderr, "failed to init metrics for worker tests: %v\n", err)
		os.Exit(1)
	}

	os.Exit(m.Run())
}
