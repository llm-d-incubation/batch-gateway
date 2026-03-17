# Batch Gateway Demo Overview

This directory contains demo files for testing the Batch Gateway system.

## Files

- **batch_input.jsonl**: Batch input file with 40 diverse inference requests distributed across two models: 20 requests for `sim-model` and 20 for `sim-model-b` (demonstrates multi-model routing with interweaved requests). Used for both complete processing and cancellation demos.
- **demo.http**: REST Client format file for VS Code REST Client plugin with two complete demo sequences.
- **curl_demo.md**: Fast-track demo guide using curl commands from the command line, with detailed examples for complete batch processing and cancellation workflows.

## Prerequisites

1. **Deploy the Batch Gateway**:

   ```bash
   make dev-deploy
   ```

   This will start:
   - API Server at <https://localhost:8000>
   - Processor at <http://localhost:9090>
   - Jaeger UI at <http://localhost:16686>
   - Metrics endpoints at <http://localhost:8081> (API) and <http://localhost:9090> (Processor)

2. **Choose Your Demo Tool**:
   - **Using demo.http**: Install the REST Client for Visual Studio Code extension (Ctrl+Shift+X / Cmd+Shift+X)
   - **Using curl_demo.md**: Ensure `curl` and `jq` are available on your system

## Architecture

The demo environment runs the following components in a Kubernetes cluster (kind):

```text
┌────────────────────────────────────────────────────────────────┐
│                   Kubernetes Cluster (kind)                    │
│                                                                │
│  ┌──────────────────────┐          ┌──────────────────────┐    │
│  │   API Server         │          │   Processor          │    │
│  │  (batch-gateway-     │          │  (batch-gateway-     │    │
│  │   apiserver)         │          │   processor)         │    |
│  │                      │          │                      │    │
│  │  • REST API (8000)   │          │  • Polling worker    │    │
│  │  • Metrics (8081)    │          │  • Metrics (9090)    │    │
│  └──────────┬───────────┘          └──────────┬───────────┘    │
│             │                                 │                │
│             │                                 │                │
│             ├─────────────┬───────────────────┤                │
│             │             │                   │                │
│  ┌──────────▼─────────┐   │   ┌───────────────▼──────────┐     │
│  │   PostgreSQL       │   │   │   Redis                  │     │
│  │                    │   │   │                          │     │
│  │  • Batch metadata  │   │   │  • Priority queue        │     │
│  │  • File metadata   │   │   │  • Progress tracking     │     │
│  │  • Persistent DB   │   │   │  • Event exchange        │     │
│  └────────────────────┘   │   └──────────────────────────┘     │
│                           │                                    │
│           ┌───────────────▼─────────────────────┐              │
│           │   File Storage (PVC or S3)          │              │
│           │                                     │              │
│           │  • Batch input files (.jsonl)       │              │
│           │  • Batch output files (results)     │              │
│           │  • Error files (failed requests)    │              │
│           └─────────────────────────────────────┘              │
│                                                                │
│  ┌──────────────────────────────────────────────────────────┐  |
│  │   Model Inference Services                               │  │
│  │                                                          │  │
│  │   ┌────────────────────┐       ┌────────────────────┐    │  │
│  │   │ vLLM Simulator     │       │ vLLM Simulator B   │    │  │
│  │   │  (sim-model)       │       │  (sim-model-b)     │    │  │
│  │   │                    │       │                    │    │  │
│  │   │ • 50ms TTFT        │       │ • 200ms TTFT       │    │  │
│  │   │ • 100ms token      │       │ • 500ms token      │    │  │
│  │   └─────────▲──────────┘       └─────────▲──────────┘    │  │
│  │             │                            │               │  │
│  └─────────────┼────────────────────────────┼───────────────┘  │
│                │                            │                  │
│                └────────────┬───────────────┘                  │
│                             │                                  │
│                             │                                  │
│                Inference requests from Processor               │
│                                                                │
│  ┌──────────────────────────────────────────────────────────┐  │
│  │   Observability                                          │  │
│  │                                                          │  │
│  │   ┌──────────────────────┐                               │  │
│  │   │ Jaeger               │                               │  │
│  │   │                      │                               │  │
│  │   │ • Distributed traces │◄───── All components          │  │
│  │   │ • UI (16686)         │       send traces             |  |
│  │   └──────────────────────┘                               │  │
│  └──────────────────────────────────────────────────────────┘  │
│                                                                │
└─────────────────────────────▲──────────────────────────────────┘
                              │
                              │ Port-forwards
                              │
                      ┌───────┴─────────┐
                      │  localhost      │
                      │                 │
                      │  :8000  (API)   │
                      │  :8081  (Obs)   │
                      │  :9090  (Proc)  │
                      │  :16686 (Jaeger)│
                      └─────────────────┘
```

**Request Flow:**

1. **Create Batch**: User → API Server → PostgreSQL (metadata) + Redis (queue) + File Storage (input file)
2. **Process Batch**: Processor polls Redis → reads batch from PostgreSQL → reads input from File Storage → sends requests to vLLM simulators → writes results to File Storage → updates PostgreSQL + Redis
3. **Retrieve Results**: User → API Server → PostgreSQL (batch status) + File Storage (output file)
3. **Monitor**: All components send traces to Jaeger; metrics exposed on /metrics endpoints

## Demo Sequences

### Sequence 1: Complete Batch Processing Flow

This demo shows the full lifecycle of a batch job:

1. **Upload batch input file** (40 requests)
2. **Create batch job** specifying the input file
3. **Monitor batch status** by polling the batch endpoint
4. **Download results** when processing completes
5. **View system metrics** from API server and processor

### Sequence 2: Batch Cancellation Flow

This demo shows how to cancel a running batch job:

1. **Upload batch input file** (same 40-request file)
2. **Create batch job**
3. **Check initial status**
4. **Cancel the batch** immediately
5. **Verify cancelled status**
6. **Download partial results** (completed requests before cancellation)

## Batch Status Flow

```text
validating → in_progress → finalizing → completed
            ↓
         cancelling → cancelled
            ↓
         failed
```

## Expected Timings

- **File upload**: < 1 second
- **Batch creation**: < 1 second
- **Processing 40 requests**: ~15-30 seconds (depends on mock simulator settings)

## Request Format

Each line in the JSONL files follows the OpenAI Batch API format:

```json
{
  "custom_id": "req-001",
  "method": "POST",
  "url": "/v1/chat/completions",
  "body": {
    "model": "sim-model",
    "max_tokens": 100,
    "messages": [
      {"role": "user", "content": "What is machine learning?"}
    ]
  }
}
```

## Request Topics

The `batch_input.jsonl` file contains 40 requests covering diverse machine learning topics, with requests interweaved between two models:

- **sim-model**: Odd-numbered requests (1, 3, 5, ..., 39) - 20 total
- **sim-model-b**: Even-numbered requests (2, 4, 6, ..., 40) - 20 total

Topics covered:

- Machine learning fundamentals (requests 1-20)
- Natural language processing (requests 21-40)

Both models are mock simulators configured in the dev deployment to demonstrate multi-model routing.

## Monitoring

### Jaeger Traces

Open <http://localhost:16686> in your browser to view distributed traces:

- Select service: `batch-gateway`
- Search by batch ID to see the full request flow
- View span details to see timing and errors

### Prometheus Metrics

View metrics at:

- API Server: <http://localhost:8081/metrics>
- Processor: <http://localhost:9090/metrics>

Key metrics to watch:

- `batch_gateway_api_http_requests_total`: Total API requests
- `batch_gateway_api_batch_jobs_total`: Total batch jobs created
- `batch_gateway_processor_jobs_processed_total`: Total jobs processed
- `batch_gateway_processor_job_duration_seconds`: Job processing time
- `batch_gateway_processor_inference_duration_seconds`: Inference request time

### Health Endpoints

- API Server Health: <http://localhost:8081/health>
- Processor Health: <http://localhost:9090/health>

## Troubleshooting

### Connection Refused

- Ensure the batch gateway is deployed: `make dev-deploy`
- Check that port forwarding is active (should happen automatically after deploy)

### TLS Certificate Errors

- The demo uses self-signed certificates
- REST Client and cURL commands use `-k` / insecure mode for testing
- This is normal for local development

### No Results After Completion

- Check that mock models are configured in the gateway
- View processor logs: `kubectl logs -l app.kubernetes.io/component=processor -n default`
- Check Jaeger traces for errors

### Batch Stuck in Processing

- View processor metrics to see if it's processing: <http://localhost:9090/metrics>
- Check processor health: <http://localhost:9090/health>
- View processor logs for errors
