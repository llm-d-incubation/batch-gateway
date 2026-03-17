package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	db "github.com/llm-d-incubation/batch-gateway/internal/database/api"
	mockdb "github.com/llm-d-incubation/batch-gateway/internal/database/mock"
	mockfiles "github.com/llm-d-incubation/batch-gateway/internal/files_store/mock"
	"github.com/llm-d-incubation/batch-gateway/internal/processor/config"
	"github.com/llm-d-incubation/batch-gateway/internal/shared/openai"
	batch_types "github.com/llm-d-incubation/batch-gateway/internal/shared/types"
	"github.com/llm-d-incubation/batch-gateway/internal/util/clientset"
	ucom "github.com/llm-d-incubation/batch-gateway/internal/util/com"
)

// -------------------------
// Test 1: Ingestion
// - local input.jsonl exact copy (line-by-line)
// - plan offsets/lengths are correct (ReadAt matches original line bytes)
// - model_map.json consistency
// -------------------------

func TestPreProcess_BuildsPlansAndModelMap_OffsetsCorrect(t *testing.T) {
	ctx := testLoggerCtx()

	workDir := t.TempDir()
	cfg := config.NewConfig()
	cfg.WorkDir = workDir
	dbClient := newMockBatchDBClient()
	fileDBClient := newMockFileDBClient()
	filesClient := mockfiles.NewMockBatchFilesClient()

	// Build remote input in mock files store
	tenantID := uniqueTestFolder(t, "tenantA/job-inputs")
	folder, err := ucom.GetFolderNameByTenantID(tenantID)
	if err != nil {
		t.Fatalf("GetFolderNameByTenantID: %v", err)
	}
	cleanMockFilesFolder(t, folder)
	filename := "input.jsonl"
	models := []string{
		"m1", "m2", "m1", "m3",
		"m2", "m2", "m1",
		// include characters requiring sanitization (safe name logic)
		"org/model-A:1",
		"org/model-A?1", // collision candidate with above depending on sanitization
	}

	lines := makeInputLines(models)
	var remoteBuf bytes.Buffer
	for _, ln := range lines {
		remoteBuf.Write(ln)
	}

	if _, err := filesClient.Store(ctx, filename, folder, 0, 0, bytes.NewReader(remoteBuf.Bytes())); err != nil {
		t.Fatalf("files.Store: %v", err)
	}

	// Create DB item for "input file metadata"
	inputFileID := "file-123"
	fileSpec := &openai.FileObject{Filename: filename}
	fileItem := &db.FileItem{
		BaseIndexes: db.BaseIndexes{ID: inputFileID, TenantID: tenantID},
		BaseContents: db.BaseContents{
			Spec: mustJSON(t, fileSpec),
		},
	}
	if err := fileDBClient.DBStore(ctx, fileItem); err != nil {
		t.Fatalf("DBStore file item: %v", err)
	}

	clients := &clientset.Clientset{
		BatchDB: dbClient,
		FileDB:  fileDBClient,
		File:    filesClient,
	}
	p := mustNewProcessor(t, cfg, clients)

	// Build JobInfo (only BatchSpec.InputFileID is used in preProcessJob)
	jobID := "job-abc"
	jobInfo := &batch_types.JobInfo{
		JobID: jobID,
		BatchJob: &openai.Batch{
			ID: jobID,
			BatchSpec: openai.BatchSpec{
				InputFileID: inputFileID,
			},
			BatchStatusInfo: openai.BatchStatusInfo{
				Status: openai.BatchStatusInProgress,
			},
		},
		TenantID: tenantID,
	}

	var cancelRequested atomic.Bool
	if err := p.preProcessJob(ctx, jobInfo, &cancelRequested); err != nil {
		t.Fatalf("preProcessJob: %v", err)
	}

	// 1) local input exists and equals remoteBuf
	localInput, err := p.jobInputFilePath(jobID, tenantID)
	if err != nil {
		t.Fatalf("jobInputFilePath: %v", err)
	}
	gotLocal, err := os.ReadFile(localInput)
	if err != nil {
		t.Fatalf("read local input: %v", err)
	}
	if !bytes.Equal(gotLocal, remoteBuf.Bytes()) {
		t.Fatalf("local input != remote input (bytes differ)")
	}

	// 2) model_map.json exists and is consistent
	jobRootDir, err := p.jobRootDir(jobID, tenantID)
	if err != nil {
		t.Fatalf("jobRootDir: %v", err)
	}
	mapPath := filepath.Join(jobRootDir, modelMapFileName)
	mapBytes, err := os.ReadFile(mapPath)
	if err != nil {
		t.Fatalf("read model_map.json: %v", err)
	}
	var mm modelMapFile
	if err := json.Unmarshal(mapBytes, &mm); err != nil {
		t.Fatalf("unmarshal model_map.json: %v", err)
	}

	// quick sanity:
	if mm.LineCount != int64(len(lines)) {
		t.Fatalf("LineCount mismatch: got=%d want=%d", mm.LineCount, len(lines))
	}
	for model, safe := range mm.ModelToSafe {
		back, ok := mm.SafeToModel[safe]
		if !ok || back != model {
			t.Fatalf("model_map not bijective: model=%q safe=%q back=%q ok=%v", model, safe, back, ok)
		}
	}

	// 3) plan files exist and offsets/length map back to exact original line bytes (ReadAt)
	f, err := os.Open(localInput)
	if err != nil {
		t.Fatalf("open local input for ReadAt: %v", err)
	}
	defer f.Close()

	plansDir := filepath.Join(jobRootDir, "plans")
	for safeID := range mm.SafeToModel {
		planPath := filepath.Join(plansDir, safeID+".plan")
		if _, err := os.Stat(planPath); err != nil {
			t.Fatalf("missing plan file for safeID=%q: %v", safeID, err)
		}
		entries := testReadPlanEntries(t, planPath)

		// For each entry, read input.jsonl slice and ensure it is a valid JSON line ending with '\n'
		for _, e := range entries {
			chunk := readAtExact(t, f, e.Offset, e.Length)
			if len(chunk) == 0 || chunk[len(chunk)-1] != '\n' {
				t.Fatalf("entry does not end with newline: safeID=%q off=%d len=%d", safeID, e.Offset, e.Length)
			}
			trimmed := bytes.TrimSuffix(chunk, []byte{'\n'})
			var req planRequestLine
			if err := json.Unmarshal(trimmed, &req); err != nil {
				t.Fatalf("entry not valid json: safeID=%q off=%d len=%d err=%v", safeID, e.Offset, e.Length, err)
			}
			model := req.Body.Model
			if model == "" {
				t.Fatalf("entry missing body.model: safeID=%q off=%d", safeID, e.Offset)
			}

			// And ensure that this model maps to this safeID in model_map.json
			expectedSafe := mm.ModelToSafe[model]
			if expectedSafe != safeID {
				t.Fatalf("plan safeID mismatch: model=%q expectedSafe=%q gotSafe=%q", model, expectedSafe, safeID)
			}
		}
	}
}

func TestPreProcess_SystemPrompts_PrefixHashAndSortOrder(t *testing.T) {
	ctx := testLoggerCtx()

	workDir := t.TempDir()
	cfg := config.NewConfig()
	cfg.WorkDir = workDir
	dbClient := newMockBatchDBClient()
	fileDBClient := newMockFileDBClient()
	filesClient := mockfiles.NewMockBatchFilesClient()

	tenantID := uniqueTestFolder(t, "tenantA/job-sys-prompt")
	folder, err := ucom.GetFolderNameByTenantID(tenantID)
	if err != nil {
		t.Fatalf("GetFolderNameByTenantID: %v", err)
	}
	cleanMockFilesFolder(t, folder)

	specs := []inputLineSpec{
		{Model: "m1", SystemPrompt: "You are a helpful assistant."},
		{Model: "m1", SystemPrompt: "You are a code reviewer."},
		{Model: "m1", SystemPrompt: "You are a helpful assistant."}, // same as [0]
		{Model: "m1", SystemPrompt: ""},                             // no system prompt
	}

	lines := makeInputLinesWithSystemPrompts(specs)
	var remoteBuf bytes.Buffer
	for _, ln := range lines {
		remoteBuf.Write(ln)
	}

	filename := "input.jsonl"
	if _, err := filesClient.Store(ctx, filename, folder, 0, 0, bytes.NewReader(remoteBuf.Bytes())); err != nil {
		t.Fatalf("files.Store: %v", err)
	}

	inputFileID := "file-sys-prompt"
	fileSpec := &openai.FileObject{Filename: filename}
	fileItem := &db.FileItem{
		BaseIndexes:  db.BaseIndexes{ID: inputFileID, TenantID: tenantID},
		BaseContents: db.BaseContents{Spec: mustJSON(t, fileSpec)},
	}
	if err := fileDBClient.DBStore(ctx, fileItem); err != nil {
		t.Fatalf("DBStore file item: %v", err)
	}

	cs := &clientset.Clientset{
		BatchDB: dbClient,
		FileDB:  fileDBClient,
		File:    filesClient,
	}
	p := mustNewProcessor(t, cfg, cs)

	jobID := "job-sys-prompt"
	jobInfo := &batch_types.JobInfo{
		JobID: jobID,
		BatchJob: &openai.Batch{
			ID: jobID,
			BatchSpec: openai.BatchSpec{
				InputFileID: inputFileID,
			},
			BatchStatusInfo: openai.BatchStatusInfo{
				Status: openai.BatchStatusInProgress,
			},
		},
		TenantID: tenantID,
	}

	var cancelRequested atomic.Bool
	if err := p.preProcessJob(ctx, jobInfo, &cancelRequested); err != nil {
		t.Fatalf("preProcessJob: %v", err)
	}

	jobRootDir, err := p.jobRootDir(jobID, tenantID)
	if err != nil {
		t.Fatalf("jobRootDir: %v", err)
	}

	mm, err := readModelMap(jobRootDir)
	if err != nil {
		t.Fatalf("readModelMap: %v", err)
	}

	safeID := mm.ModelToSafe["m1"]
	planPath := filepath.Join(jobRootDir, "plans", safeID+".plan")
	entries := testReadPlanEntries(t, planPath)
	if len(entries) != len(specs) {
		t.Fatalf("expected %d entries, got %d", len(specs), len(entries))
	}

	// Collect hashes to verify properties
	hashBySpec := make([]uint32, len(specs))
	for i, e := range entries {
		// Read actual line from local input to identify which spec it corresponds to
		localInput, err := p.jobInputFilePath(jobID, tenantID)
		if err != nil {
			t.Fatalf("jobInputFilePath: %v", err)
		}
		f, err := os.Open(localInput)
		if err != nil {
			t.Fatalf("open local input: %v", err)
		}
		chunk := readAtExact(t, f, e.Offset, e.Length)
		f.Close()
		_ = chunk
		hashBySpec[i] = e.PrefixHash
	}

	// Entries must be sorted by PrefixHash (ascending)
	for i := 1; i < len(entries); i++ {
		if entries[i].PrefixHash < entries[i-1].PrefixHash {
			t.Fatalf("entries not sorted by PrefixHash: [%d]=%d > [%d]=%d",
				i-1, entries[i-1].PrefixHash, i, entries[i].PrefixHash)
		}
	}

	// The entry with no system prompt should have PrefixHash == NoPrefixHash
	foundZero := false
	for _, e := range entries {
		if e.PrefixHash == NoPrefixHash {
			foundZero = true
			break
		}
	}
	if !foundZero {
		t.Fatalf("expected at least one entry with NoPrefixHash (no system prompt)")
	}

	// Entries with system prompts should have PrefixHash != NoPrefixHash
	nonZeroCount := 0
	for _, e := range entries {
		if e.PrefixHash != NoPrefixHash {
			nonZeroCount++
		}
	}
	if nonZeroCount != 3 {
		t.Fatalf("expected 3 entries with non-zero PrefixHash, got %d", nonZeroCount)
	}

	// Two entries with identical system prompt ("You are a helpful assistant.") must share the same hash
	hashCounts := map[uint32]int{}
	for _, e := range entries {
		hashCounts[e.PrefixHash]++
	}
	foundDuplicate := false
	for h, c := range hashCounts {
		if h != NoPrefixHash && c >= 2 {
			foundDuplicate = true
			break
		}
	}
	if !foundDuplicate {
		t.Fatalf("expected at least one non-zero PrefixHash shared by 2+ entries, got counts: %v", hashCounts)
	}
}

func TestWatchCancel_SetsFlag_AndUpdatesCancellingOnce(t *testing.T) {
	ctx := testLoggerCtx()

	dbClient := newSpyBatchDB(newMockBatchDBClient())
	statusClient := mockdb.NewMockBatchStatusClient()
	eventClient := mockdb.NewMockBatchEventChannelClient()

	jobID := "job-cancel-1"
	initialStatus := openai.BatchStatusInfo{Status: openai.BatchStatusInProgress}
	jobItem := &db.BatchItem{
		BaseIndexes: db.BaseIndexes{
			ID: jobID,
			Tags: db.Tags{
				"tenant": "tenantA",
			},
		},
		BaseContents: db.BaseContents{
			Spec:   mustJSON(t, openai.BatchSpec{InputFileID: "unused-for-watch-cancel"}),
			Status: mustJSON(t, initialStatus),
		},
	}
	if err := dbClient.DBStore(ctx, jobItem); err != nil {
		t.Fatalf("DBStore job item: %v", err)
	}

	p := mustNewProcessor(t, config.NewConfig(), &clientset.Clientset{})
	updater := NewStatusUpdater(dbClient, statusClient, 86400)

	evCh, err := eventClient.ECConsumerGetChannel(ctx, jobID)
	if err != nil {
		t.Fatalf("ECConsumerGetChannel: %v", err)
	}
	defer evCh.CloseFn()

	var cancelRequested atomic.Bool
	var cancellingOnce sync.Once

	// Start watching cancel in background
	params := &jobExecutionParams{
		eventWatcher:    evCh,
		updater:         updater,
		jobItem:         jobItem,
		inferCancelFn:   func() {},
		cancelRequested: &cancelRequested,
		cancellingOnce:  &cancellingOnce,
	}
	go p.watchCancel(ctx, params)

	// Send cancel twice; status update should still happen once due to sync.Once.
	_, _ = eventClient.ECProducerSendEvents(ctx, []db.BatchEvent{
		{ID: jobID, Type: db.BatchEventCancel, TTL: 60},
	})
	_, _ = eventClient.ECProducerSendEvents(ctx, []db.BatchEvent{
		{ID: jobID, Type: db.BatchEventCancel, TTL: 60},
	})

	deadline := time.Now().Add(2 * time.Second)
	for !params.cancelRequested.Load() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !params.cancelRequested.Load() {
		t.Fatalf("cancelRequested was not set")
	}

	deadline = time.Now().Add(2 * time.Second)
	for dbClient.StatusCalls(openai.BatchStatusCancelling) < 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if dbClient.StatusCalls(openai.BatchStatusCancelling) != 1 {
		t.Fatalf("expected cancelling update exactly once, got=%d", dbClient.StatusCalls(openai.BatchStatusCancelling))
	}
}

// TestWatchCancel_CancelsInferContext verifies that when a cancel event arrives,
// watchCancel calls the inferCancelFn, which cancels the inference context so that
// in-flight HTTP requests are aborted immediately.
func TestWatchCancel_CancelsInferContext(t *testing.T) {
	ctx := testLoggerCtx()

	dbClient := newMockBatchDBClient()
	statusClient := mockdb.NewMockBatchStatusClient()
	eventClient := mockdb.NewMockBatchEventChannelClient()

	jobID := "job-cancel-infer-ctx"
	jobItem := &db.BatchItem{
		BaseIndexes: db.BaseIndexes{ID: jobID, TenantID: "tenantA"},
		BaseContents: db.BaseContents{
			Status: mustJSON(t, openai.BatchStatusInfo{Status: openai.BatchStatusInProgress}),
		},
	}
	if err := dbClient.DBStore(ctx, jobItem); err != nil {
		t.Fatalf("DBStore: %v", err)
	}

	updater := NewStatusUpdater(dbClient, statusClient, 86400)

	evCh, err := eventClient.ECConsumerGetChannel(ctx, jobID)
	if err != nil {
		t.Fatalf("ECConsumerGetChannel: %v", err)
	}
	defer evCh.CloseFn()

	var cancelRequested atomic.Bool
	var cancellingOnce sync.Once

	inferCtx, inferCancelFn := context.WithCancel(ctx)

	params := &jobExecutionParams{
		eventWatcher:    evCh,
		updater:         updater,
		jobItem:         jobItem,
		inferCancelFn:   inferCancelFn,
		cancelRequested: &cancelRequested,
		cancellingOnce:  &cancellingOnce,
	}
	go p_watchCancelHelper(t, ctx, params)

	// Send cancel event
	_, _ = eventClient.ECProducerSendEvents(ctx, []db.BatchEvent{
		{ID: jobID, Type: db.BatchEventCancel, TTL: 60},
	})

	// Wait for inferCtx to be cancelled
	select {
	case <-inferCtx.Done():
		// success — inferCancelFn was called
	case <-time.After(2 * time.Second):
		t.Fatal("inferCtx was not cancelled within 2s after cancel event")
	}

	if !params.cancelRequested.Load() {
		t.Fatal("cancelRequested was not set")
	}
}

// p_watchCancelHelper is a test helper that calls watchCancel on a fresh Processor.
func p_watchCancelHelper(t *testing.T, ctx context.Context, params *jobExecutionParams) {
	t.Helper()
	p := mustNewProcessor(t, config.NewConfig(), &clientset.Clientset{})
	p.watchCancel(ctx, params)
}

func TestPreProcess_CancelFlag_ReturnsErrCancelled(t *testing.T) {
	ctx := testLoggerCtx()

	workDir := t.TempDir()
	cfg := config.NewConfig()
	cfg.WorkDir = workDir
	dbClient := newMockBatchDBClient()
	fileDBClient := newMockFileDBClient()
	filesClient := mockfiles.NewMockBatchFilesClient()

	clients := &clientset.Clientset{
		BatchDB: dbClient,
		FileDB:  fileDBClient,
		File:    filesClient,
	}
	p := mustNewProcessor(t, cfg, clients)

	jobID := "job-preprocess-cancel"
	inputFileID := "file-preprocess-cancel"
	tenantID := uniqueTestFolder(t, "tenantA/preprocess-cancel")
	folder, err := ucom.GetFolderNameByTenantID(tenantID)
	if err != nil {
		t.Fatalf("GetFolderNameByTenantID: %v", err)
	}
	cleanMockFilesFolder(t, folder)

	models := make([]string, 0, 2000)
	for i := 0; i < 2000; i++ {
		switch i % 3 {
		case 0:
			models = append(models, "mA")
		case 1:
			models = append(models, "mB")
		default:
			models = append(models, "mC")
		}
	}
	lines := makeInputLines(models)
	var remoteBuf bytes.Buffer
	for _, ln := range lines {
		remoteBuf.Write(ln)
	}

	if _, err := filesClient.Store(ctx, "input.jsonl", folder, 0, 0, bytes.NewReader(remoteBuf.Bytes())); err != nil {
		t.Fatalf("files.Store: %v", err)
	}
	fileSpec := &openai.FileObject{Filename: "input.jsonl"}
	if err := fileDBClient.DBStore(ctx, &db.FileItem{
		BaseIndexes: db.BaseIndexes{ID: inputFileID, TenantID: tenantID},
		BaseContents: db.BaseContents{
			Spec: mustJSON(t, fileSpec),
		},
	}); err != nil {
		t.Fatalf("DBStore file item: %v", err)
	}

	jobInfo := &batch_types.JobInfo{
		JobID: jobID,
		BatchJob: &openai.Batch{
			ID: jobID,
			BatchSpec: openai.BatchSpec{
				InputFileID: inputFileID,
			},
			BatchStatusInfo: openai.BatchStatusInfo{
				Status: openai.BatchStatusInProgress,
			},
		},
		TenantID: tenantID,
	}

	var cancelRequested atomic.Bool
	cancelRequested.Store(true)
	err = p.preProcessJob(ctx, jobInfo, &cancelRequested)
	if !errors.Is(err, ErrCancelled) {
		t.Fatalf("expected ErrCancelled, got: %v", err)
	}
}

func TestHandleCancelled_CleansDir_UpdatesCancelled(t *testing.T) {
	ctx := testLoggerCtx()

	workDir := t.TempDir()
	cfg := config.NewConfig()
	cfg.WorkDir = workDir

	dbClient := newMockBatchDBClient()
	statusClient := mockdb.NewMockBatchStatusClient()
	clients := &clientset.Clientset{
		BatchDB: dbClient,
		Status:  statusClient,
	}
	p := mustNewProcessor(t, cfg, clients)

	jobID := "job-handle-cancelled"
	jobItem := &db.BatchItem{
		BaseIndexes: db.BaseIndexes{
			ID:       jobID,
			TenantID: "tenantA",
			Tags: db.Tags{
				"tenant": "tenantA",
			},
		},
		BaseContents: db.BaseContents{
			Status: mustJSON(t, openai.BatchStatusInfo{
				Status: openai.BatchStatusCancelling,
			}),
		},
	}
	if err := dbClient.DBStore(ctx, jobItem); err != nil {
		t.Fatalf("DBStore job item: %v", err)
	}

	jobDir, err := p.jobRootDir(jobID, jobItem.TenantID)
	if err != nil {
		t.Fatalf("jobRootDir: %v", err)
	}
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		t.Fatalf("MkdirAll jobDir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(jobDir, "dummy.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile dummy: %v", err)
	}

	updater := NewStatusUpdater(dbClient, statusClient, 86400)

	if err := p.handleCancelled(ctx, &jobExecutionParams{
		updater: updater,
		jobItem: jobItem,
	}); err != nil {
		t.Fatalf("handleCancelled: %v", err)
	}

	if _, err := os.Stat(jobDir); err == nil {
		t.Fatalf("expected job dir removed, still exists: %s", jobDir)
	}

	jobs, _, _, err := dbClient.DBGet(ctx, &db.BatchQuery{BaseQuery: db.BaseQuery{IDs: []string{jobID}}}, true, 0, 1)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("DBGet job after cancel: err=%v len=%d", err, len(jobs))
	}
}

func TestRunPollingLoop_ExpiredJob_UpdatesExpiredStatus(t *testing.T) {
	ctx := testLoggerCtx()

	cfg := config.NewConfig()
	cfg.PollInterval = 5 * time.Millisecond
	cfg.NumWorkers = 1

	pq := &spyPQ{inner: mockdb.NewMockBatchPriorityQueueClient()}
	dbClient := newSpyBatchDB(newMockBatchDBClient())
	statusClient := mockdb.NewMockBatchStatusClient()
	jobID := "job-expired-1"

	jobItem := &db.BatchItem{
		BaseIndexes: db.BaseIndexes{
			ID:       jobID,
			TenantID: "tenantA",
			Tags: db.Tags{
				"tenant": "tenantA",
			},
		},
		BaseContents: db.BaseContents{
			Spec: mustJSON(t, openai.BatchSpec{
				InputFileID: "unused",
			}),
			Status: mustJSON(t, openai.BatchStatusInfo{
				Status: openai.BatchStatusInProgress,
			}),
		},
	}
	if err := dbClient.DBStore(ctx, jobItem); err != nil {
		t.Fatalf("DBStore job item: %v", err)
	}
	if err := pq.PQEnqueue(ctx, &db.BatchJobPriority{
		ID:  jobID,
		SLO: time.Now().Add(-1 * time.Second),
	}); err != nil {
		t.Fatalf("PQEnqueue task: %v", err)
	}

	clients := &clientset.Clientset{
		BatchDB: dbClient,
		Queue:   pq,
		Status:  statusClient,
	}
	p := mustNewProcessor(t, cfg, clients)

	runCtx, cancel := context.WithTimeout(ctx, 40*time.Millisecond)
	defer cancel()
	if err := p.runPollingLoop(runCtx); err != nil {
		t.Fatalf("runPollingLoop: %v", err)
	}

	if dbClient.StatusCalls(openai.BatchStatusExpired) < 1 {
		t.Fatalf("expected expired status update at least once")
	}
}

func TestRunPollingLoop_DBTransient_ReEnqueuesTask(t *testing.T) {
	ctx := testLoggerCtx()

	cfg := config.NewConfig()
	cfg.PollInterval = 5 * time.Millisecond
	cfg.NumWorkers = 1

	pq := &spyPQ{inner: mockdb.NewMockBatchPriorityQueueClient()}
	innerDB := mockdb.NewMockDBClient(
		func(b *db.BatchItem) string { return b.ID },
		func(q *db.BatchQuery) *db.BaseQuery { return &q.BaseQuery },
	)
	dbClient := &dbGetErrWrapper{
		inner: innerDB,
		err:   errors.New("db transient"),
	}
	statusClient := mockdb.NewMockBatchStatusClient()
	jobID := "job-db-transient-1"

	if err := pq.PQEnqueue(ctx, &db.BatchJobPriority{
		ID:  jobID,
		SLO: time.Now().Add(1 * time.Hour),
	}); err != nil {
		t.Fatalf("PQEnqueue task: %v", err)
	}
	initialEnqueueCalls := pq.EnqueueCalls()

	clients := &clientset.Clientset{
		BatchDB: dbClient,
		Queue:   pq,
		Status:  statusClient,
	}
	p := mustNewProcessor(t, cfg, clients)

	runCtx, cancel := context.WithTimeout(ctx, 40*time.Millisecond)
	defer cancel()
	if err := p.runPollingLoop(runCtx); err != nil {
		t.Fatalf("runPollingLoop: %v", err)
	}

	if pq.EnqueueCalls() <= initialEnqueueCalls {
		t.Fatalf("expected task re-enqueue on transient DB error")
	}
}

func TestRunPollingLoop_NotRunnableJob_SkipsWithoutStatusUpdate(t *testing.T) {
	ctx := testLoggerCtx()

	cfg := config.NewConfig()
	cfg.PollInterval = 5 * time.Millisecond
	cfg.NumWorkers = 1

	pq := &spyPQ{inner: mockdb.NewMockBatchPriorityQueueClient()}
	dbClient := newSpyBatchDB(newMockBatchDBClient())
	statusClient := mockdb.NewMockBatchStatusClient()
	jobID := "job-not-runnable-1"

	jobItem := &db.BatchItem{
		BaseIndexes: db.BaseIndexes{
			ID:       jobID,
			TenantID: "tenantA",
			Tags: db.Tags{
				"tenant": "tenantA",
			},
		},
		BaseContents: db.BaseContents{
			Spec: mustJSON(t, openai.BatchSpec{
				InputFileID: "unused",
			}),
			// completed is terminal and not runnable
			Status: mustJSON(t, openai.BatchStatusInfo{
				Status: openai.BatchStatusCompleted,
			}),
		},
	}
	if err := dbClient.DBStore(ctx, jobItem); err != nil {
		t.Fatalf("DBStore job item: %v", err)
	}
	if err := pq.PQEnqueue(ctx, &db.BatchJobPriority{
		ID:  jobID,
		SLO: time.Now().Add(1 * time.Hour),
	}); err != nil {
		t.Fatalf("PQEnqueue task: %v", err)
	}

	clients := &clientset.Clientset{
		BatchDB: dbClient,
		Queue:   pq,
		Status:  statusClient,
	}
	p := mustNewProcessor(t, cfg, clients)

	runCtx, cancel := context.WithTimeout(ctx, 40*time.Millisecond)
	defer cancel()
	if err := p.runPollingLoop(runCtx); err != nil {
		t.Fatalf("runPollingLoop: %v", err)
	}

	// no persistent status transition should be attempted for not-runnable jobs.
	if dbClient.StatusCalls(openai.BatchStatusCompleted) > 0 || dbClient.StatusCalls(openai.BatchStatusFailed) > 0 || dbClient.StatusCalls(openai.BatchStatusExpired) > 0 {
		t.Fatalf("expected no status updates for not-runnable job")
	}
}

// ---------------------------------------------------------------------------
// stream: true rejection
// ---------------------------------------------------------------------------

func TestExtractAndValidateLine_StreamTrue_ReturnsError(t *testing.T) {
	line := []byte(`{"custom_id":"r1","body":{"model":"gpt-4","stream":true,"messages":[{"role":"user","content":"hi"}]}}` + "\n")
	_, err := extractAndValidateLine(line)
	if err == nil {
		t.Fatal("expected error for stream: true, got nil")
	}
	if !strings.Contains(err.Error(), "streaming is not supported") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestExtractAndValidateLine_StreamFalse_OK(t *testing.T) {
	line := []byte(`{"custom_id":"r1","body":{"model":"gpt-4","stream":false,"messages":[{"role":"user","content":"hi"}]}}` + "\n")
	meta, err := extractAndValidateLine(line)
	if err != nil {
		t.Fatalf("unexpected error for stream: false: %v", err)
	}
	if meta.ModelID != "gpt-4" {
		t.Fatalf("expected model gpt-4, got %s", meta.ModelID)
	}
	if meta.CustomID != "r1" {
		t.Fatalf("expected custom_id r1, got %s", meta.CustomID)
	}
}

func TestExtractAndValidateLine_StreamOmitted_OK(t *testing.T) {
	line := []byte(`{"custom_id":"r1","body":{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}}` + "\n")
	meta, err := extractAndValidateLine(line)
	if err != nil {
		t.Fatalf("unexpected error when stream is omitted: %v", err)
	}
	if meta.ModelID != "gpt-4" {
		t.Fatalf("expected model gpt-4, got %s", meta.ModelID)
	}
}

func TestExtractAndValidateLine_EmptyCustomID_ReturnsError(t *testing.T) {
	line := []byte(`{"custom_id":"","body":{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}}` + "\n")
	_, err := extractAndValidateLine(line)
	if err == nil {
		t.Fatal("expected error for empty custom_id, got nil")
	}
	if !strings.Contains(err.Error(), "custom_id is required") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestExtractAndValidateLine_MissingCustomID_ReturnsError(t *testing.T) {
	line := []byte(`{"body":{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}}` + "\n")
	_, err := extractAndValidateLine(line)
	if err == nil {
		t.Fatal("expected error for missing custom_id, got nil")
	}
	if !strings.Contains(err.Error(), "custom_id is required") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestPreProcess_StreamTrue_FailsJob(t *testing.T) {
	ctx := testLoggerCtx()

	workDir := t.TempDir()
	cfg := config.NewConfig()
	cfg.WorkDir = workDir
	dbClient := newMockBatchDBClient()
	fileDBClient := newMockFileDBClient()
	filesClient := mockfiles.NewMockBatchFilesClient()

	tenantID := uniqueTestFolder(t, "tenantA/stream-reject")
	folder, err := ucom.GetFolderNameByTenantID(tenantID)
	if err != nil {
		t.Fatalf("GetFolderNameByTenantID: %v", err)
	}
	cleanMockFilesFolder(t, folder)

	var remoteBuf bytes.Buffer
	remoteBuf.WriteString(`{"custom_id":"r1","body":{"model":"m1","messages":[{"role":"user","content":"ok"}]}}` + "\n")
	remoteBuf.WriteString(`{"custom_id":"r2","body":{"model":"m1","stream":true,"messages":[{"role":"user","content":"bad"}]}}` + "\n")
	remoteBuf.WriteString(`{"custom_id":"r3","body":{"model":"m1","messages":[{"role":"user","content":"ok2"}]}}` + "\n")

	filename := "input.jsonl"
	if _, err := filesClient.Store(ctx, filename, folder, 0, 0, bytes.NewReader(remoteBuf.Bytes())); err != nil {
		t.Fatalf("files.Store: %v", err)
	}

	inputFileID := "file-stream-reject"
	fileSpec := &openai.FileObject{Filename: filename}
	fileItem := &db.FileItem{
		BaseIndexes:  db.BaseIndexes{ID: inputFileID, TenantID: tenantID},
		BaseContents: db.BaseContents{Spec: mustJSON(t, fileSpec)},
	}
	if err := fileDBClient.DBStore(ctx, fileItem); err != nil {
		t.Fatalf("DBStore file item: %v", err)
	}

	clients := &clientset.Clientset{
		BatchDB: dbClient,
		FileDB:  fileDBClient,
		File:    filesClient,
	}
	p := mustNewProcessor(t, cfg, clients)

	jobID := "job-stream-reject"
	jobInfo := &batch_types.JobInfo{
		JobID: jobID,
		BatchJob: &openai.Batch{
			ID: jobID,
			BatchSpec: openai.BatchSpec{
				InputFileID: inputFileID,
			},
			BatchStatusInfo: openai.BatchStatusInfo{
				Status: openai.BatchStatusInProgress,
			},
		},
		TenantID: tenantID,
	}

	var cancelRequested atomic.Bool
	err = p.preProcessJob(ctx, jobInfo, &cancelRequested)
	if err == nil {
		t.Fatal("expected preProcessJob to fail for input with stream: true")
	}
	if !strings.Contains(err.Error(), "streaming is not supported") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestPreProcess_DuplicateCustomID_FailsJob(t *testing.T) {
	ctx := testLoggerCtx()

	workDir := t.TempDir()
	cfg := config.NewConfig()
	cfg.WorkDir = workDir
	dbClient := newMockBatchDBClient()
	fileDBClient := newMockFileDBClient()
	filesClient := mockfiles.NewMockBatchFilesClient()

	tenantID := uniqueTestFolder(t, "tenantA/dup-custom-id")
	folder, err := ucom.GetFolderNameByTenantID(tenantID)
	if err != nil {
		t.Fatalf("GetFolderNameByTenantID: %v", err)
	}
	cleanMockFilesFolder(t, folder)

	var remoteBuf bytes.Buffer
	remoteBuf.WriteString(`{"custom_id":"req-1","body":{"model":"m1","messages":[{"role":"user","content":"a"}]}}` + "\n")
	remoteBuf.WriteString(`{"custom_id":"req-2","body":{"model":"m1","messages":[{"role":"user","content":"b"}]}}` + "\n")
	remoteBuf.WriteString(`{"custom_id":"req-1","body":{"model":"m1","messages":[{"role":"user","content":"c"}]}}` + "\n")

	filename := "input.jsonl"
	if _, err := filesClient.Store(ctx, filename, folder, 0, 0, bytes.NewReader(remoteBuf.Bytes())); err != nil {
		t.Fatalf("files.Store: %v", err)
	}

	inputFileID := "file-dup-custom-id"
	fileSpec := &openai.FileObject{Filename: filename}
	fileItem := &db.FileItem{
		BaseIndexes:  db.BaseIndexes{ID: inputFileID, TenantID: tenantID},
		BaseContents: db.BaseContents{Spec: mustJSON(t, fileSpec)},
	}
	if err := fileDBClient.DBStore(ctx, fileItem); err != nil {
		t.Fatalf("DBStore file item: %v", err)
	}

	clients := &clientset.Clientset{
		BatchDB: dbClient,
		FileDB:  fileDBClient,
		File:    filesClient,
	}
	p := mustNewProcessor(t, cfg, clients)

	jobID := "job-dup-custom-id"
	jobInfo := &batch_types.JobInfo{
		JobID: jobID,
		BatchJob: &openai.Batch{
			ID: jobID,
			BatchSpec: openai.BatchSpec{
				InputFileID: inputFileID,
			},
			BatchStatusInfo: openai.BatchStatusInfo{
				Status: openai.BatchStatusInProgress,
			},
		},
		TenantID: tenantID,
	}

	var cancelRequested atomic.Bool
	err = p.preProcessJob(ctx, jobInfo, &cancelRequested)
	if err == nil {
		t.Fatal("expected preProcessJob to fail for duplicate custom_id")
	}
	if !strings.Contains(err.Error(), "duplicate custom_id") {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(err.Error(), "req-1") {
		t.Fatalf("error should mention the duplicated custom_id value: %v", err)
	}
}

func TestPreProcess_UniqueCustomIDs_Succeeds(t *testing.T) {
	ctx := testLoggerCtx()

	workDir := t.TempDir()
	cfg := config.NewConfig()
	cfg.WorkDir = workDir
	dbClient := newMockBatchDBClient()
	fileDBClient := newMockFileDBClient()
	filesClient := mockfiles.NewMockBatchFilesClient()

	tenantID := uniqueTestFolder(t, "tenantA/unique-custom-id")
	folder, err := ucom.GetFolderNameByTenantID(tenantID)
	if err != nil {
		t.Fatalf("GetFolderNameByTenantID: %v", err)
	}
	cleanMockFilesFolder(t, folder)

	var remoteBuf bytes.Buffer
	remoteBuf.WriteString(`{"custom_id":"req-1","body":{"model":"m1","messages":[{"role":"user","content":"a"}]}}` + "\n")
	remoteBuf.WriteString(`{"custom_id":"req-2","body":{"model":"m1","messages":[{"role":"user","content":"b"}]}}` + "\n")
	remoteBuf.WriteString(`{"custom_id":"req-3","body":{"model":"m1","messages":[{"role":"user","content":"c"}]}}` + "\n")

	filename := "input.jsonl"
	if _, err := filesClient.Store(ctx, filename, folder, 0, 0, bytes.NewReader(remoteBuf.Bytes())); err != nil {
		t.Fatalf("files.Store: %v", err)
	}

	inputFileID := "file-unique-custom-id"
	fileSpec := &openai.FileObject{Filename: filename}
	fileItem := &db.FileItem{
		BaseIndexes:  db.BaseIndexes{ID: inputFileID, TenantID: tenantID},
		BaseContents: db.BaseContents{Spec: mustJSON(t, fileSpec)},
	}
	if err := fileDBClient.DBStore(ctx, fileItem); err != nil {
		t.Fatalf("DBStore file item: %v", err)
	}

	clients := &clientset.Clientset{
		BatchDB: dbClient,
		FileDB:  fileDBClient,
		File:    filesClient,
	}
	p := mustNewProcessor(t, cfg, clients)

	jobID := "job-unique-custom-id"
	jobInfo := &batch_types.JobInfo{
		JobID: jobID,
		BatchJob: &openai.Batch{
			ID: jobID,
			BatchSpec: openai.BatchSpec{
				InputFileID: inputFileID,
			},
			BatchStatusInfo: openai.BatchStatusInfo{
				Status: openai.BatchStatusInProgress,
			},
		},
		TenantID: tenantID,
	}

	var cancelRequested atomic.Bool
	if err := p.preProcessJob(ctx, jobInfo, &cancelRequested); err != nil {
		t.Fatalf("expected preProcessJob to succeed with unique custom_ids, got: %v", err)
	}
}
