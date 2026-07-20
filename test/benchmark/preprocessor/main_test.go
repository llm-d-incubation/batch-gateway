//go:build bench

package preprocessor_test

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
		fmt.Fprintf(os.Stderr, "failed to init metrics: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}
