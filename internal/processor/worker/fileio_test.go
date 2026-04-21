package worker

import (
	"testing"

	"github.com/llm-d-incubation/batch-gateway/internal/processor/config"
	"github.com/llm-d-incubation/batch-gateway/internal/util/clientset"
)

func TestJobRootDir_EmptyTenantID_ReturnsError(t *testing.T) {
	p := mustNewProcessor(t, config.NewConfig(), &clientset.Clientset{})

	if _, err := p.jobRootDir("job-1", ""); err == nil {
		t.Fatalf("expected error for empty tenantID")
	}
}

func TestJobInputFilePath_PropagatesJobRootDirError(t *testing.T) {
	p := mustNewProcessor(t, config.NewConfig(), &clientset.Clientset{})

	if _, err := p.jobInputFilePath("job-1", ""); err == nil {
		t.Fatalf("expected error from jobRootDir when tenantID is empty")
	}
}

func TestCreateLocalInputFile_PropagatesPathError(t *testing.T) {
	p := mustNewProcessor(t, config.NewConfig(), &clientset.Clientset{})

	f, path, err := p.createLocalInputFile("job-1", "")
	if err == nil {
		t.Fatalf("expected error for empty tenantID")
	}
	if f != nil {
		t.Fatalf("expected nil file on error")
	}
	if path != "" {
		t.Fatalf("expected empty path on path-resolution error, got %q", path)
	}
}
