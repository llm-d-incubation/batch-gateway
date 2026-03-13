# Batch Gateway Demo

This directory contains demo files for testing the Batch Gateway system.

## Files

- **batch_input.jsonl**: Batch input file with 80 diverse inference requests using the `sim-model` (mock simulator model).
- **batch_input_two_models.jsonl**: Batch input file with 80 requests distributed across two models: 40 requests for `sim-model` and 40 for `sim-model-b` (demonstrates multi-model routing).
- **batch_input_cancel.jsonl**: Smaller batch input file with 10 requests for testing batch cancellation.
- **demo.txt**: REST Client format file for VS Code REST Client plugin with two complete demo sequences.

## Prerequisites

1. **Deploy the Batch Gateway**:
   ```bash
   make dev-deploy
   ```

   This will start:
   - API Server at https://localhost:8000
   - Processor at http://localhost:9090
   - Jaeger UI at http://localhost:16686
   - Metrics endpoints at http://localhost:8081 (API) and http://localhost:9090 (Processor)

2. **Install VS Code REST Client Extension**:
   - Open VS Code Extensions (Ctrl+Shift+X / Cmd+Shift+X)
   - Search for "REST Client" by Huachao Mao
   - Install the extension

## Demo Sequences

### Sequence 1: Complete Batch Processing Flow

This demo shows the full lifecycle of a batch job:

1. **Upload batch input file** (80 requests)
2. **Create batch job** specifying the input file
3. **Monitor batch status** by polling the batch endpoint
4. **Download results** when processing completes
5. **View system metrics** from API server and processor

**Steps**:
1. Open `demo.txt` in VS Code
2. Click "Send Request" above step 1.1 to upload the file
3. Click "Send Request" above step 1.2 to create the batch
4. Click "Send Request" above step 1.3 to check status (repeat until status = "completed")
5. Click "Send Request" above step 1.5 to download results
6. Click "Send Request" above steps 1.8-1.11 to view metrics and health

### Sequence 2: Batch Cancellation Flow

This demo shows how to cancel a running batch job:

1. **Upload smaller batch input file** (10 requests for quick testing)
2. **Create batch job**
3. **Check initial status**
4. **Cancel the batch** immediately
5. **Verify cancelled status**
6. **Download partial results** (completed requests before cancellation)

**Steps**:
1. Open `demo.txt` in VS Code
2. Scroll to "DEMO SEQUENCE 2"
3. Click "Send Request" above step 2.1 to upload the cancellation demo file
4. Click "Send Request" above step 2.2 to create the batch
5. **Immediately** click "Send Request" above step 2.4 to cancel
6. Click "Send Request" above step 2.5 to verify cancellation
7. Click "Send Request" above step 2.6 to see partial results (if any)

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

The `batch_input.jsonl` file contains 80 requests covering diverse topics:

- Machine learning fundamentals (requests 1-20)
- Natural language processing (requests 21-40)
- Computer vision (requests 41-60)
- Distributed training and optimization (requests 61-80)

All requests use the `sim-model` which is a mock simulator configured in the dev deployment.

## Monitoring

### Jaeger Traces
Open http://localhost:16686 in your browser to view distributed traces:
- Select service: `batch-gateway-apiserver` or `batch-gateway-processor`
- Search by batch ID to see the full request flow
- View span details to see timing and errors

### Prometheus Metrics
View metrics at:
- API Server: http://localhost:8081/metrics
- Processor: http://localhost:9090/metrics

Key metrics to watch:
- `batch_gateway_api_http_requests_total`: Total API requests
- `batch_gateway_api_batch_jobs_total`: Total batch jobs created
- `batch_gateway_processor_jobs_processed_total`: Total jobs processed
- `batch_gateway_processor_job_duration_seconds`: Job processing time
- `batch_gateway_processor_inference_duration_seconds`: Inference request time

### Health Endpoints
- API Server Health: http://localhost:8081/health
- Processor Health: http://localhost:9090/health

## Using cURL Instead of REST Client

If you prefer using cURL from the command line, you can extract the requests from `demo.txt` and run them manually. For example:

```bash
# Upload file
curl -k -X POST https://localhost:8000/v1/files \
  -H "X-MaaS-Username: demo-user" \
  -H "Authorization: Bearer unused" \
  -F "purpose=batch" \
  -F "file=@batch_input.jsonl;type=application/jsonl"

# Create batch (replace FILE_ID)
curl -k -X POST https://localhost:8000/v1/batches \
  -H "X-MaaS-Username: demo-user" \
  -H "Authorization: Bearer unused" \
  -H "Content-Type: application/json" \
  -d '{"input_file_id":"FILE_ID","endpoint":"/v1/chat/completions","completion_window":"24h"}'

# Check status (replace BATCH_ID)
curl -k https://localhost:8000/v1/batches/BATCH_ID \
  -H "X-MaaS-Username: demo-user" \
  -H "Authorization: Bearer unused"
```

## Cleanup

To clean up batches and files after testing, uncomment and use the cleanup requests in section 3 of `demo.txt`.

**Note**: You can only delete batches in terminal states: `expired`, `failed`, `completed`, or `cancelled`.

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
- View processor logs: `kubectl logs -l app=batch-gateway-processor -n default`
- Check Jaeger traces for errors

### Batch Stuck in Processing
- View processor metrics to see if it's processing: http://localhost:9090/metrics
- Check processor health: http://localhost:9090/health
- View processor logs for errors
