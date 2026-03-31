# Batch Gateway Demo with Curl

Fast-track demo using curl commands.

## Prerequisites

```bash
# Deploy the batch gateway
make dev-deploy

# Navigate to the demo directory
cd test/poc
```

## Demo 1: Complete Batch Processing (5 minutes)

```bash
# Step 1: Upload batch file
FILE_RESPONSE=$(curl -k -s -X POST https://localhost:8000/v1/files \
  -H "X-MaaS-Username: demo-user" \
  -H "Authorization: Bearer unused" \
  -F "purpose=batch" \
  -F "file=@batch_input.jsonl;type=application/jsonl")

FILE_ID=$(echo $FILE_RESPONSE | jq -r '.id')
echo "Uploaded file: $FILE_ID"

# Step 2: Create batch job
BATCH_RESPONSE=$(curl -k -s -X POST https://localhost:8000/v1/batches \
  -H "X-MaaS-Username: demo-user" \
  -H "Authorization: Bearer unused" \
  -H "Content-Type: application/json" \
  -d "{\"input_file_id\":\"$FILE_ID\",\"endpoint\":\"/v1/chat/completions\",\"completion_window\":\"24h\"}")

BATCH_ID=$(echo $BATCH_RESPONSE | jq -r '.id')
echo "Created batch: $BATCH_ID"

# Step 3: Monitor status (run this in a loop until completed)
watch -n 2 "curl -k -s https://localhost:8000/v1/batches/$BATCH_ID \
  -H 'X-MaaS-Username: demo-user' \
  -H 'Authorization: Bearer unused' | jq '.status'"

# Alternative: Poll once
curl -k -s https://localhost:8000/v1/batches/$BATCH_ID \
  -H "X-MaaS-Username: demo-user" \
  -H "Authorization: Bearer unused" | jq

# Step 4: Get output file ID and download results
OUTPUT_FILE_ID=$(curl -k -s https://localhost:8000/v1/batches/$BATCH_ID \
  -H "X-MaaS-Username: demo-user" \
  -H "Authorization: Bearer unused" | jq -r '.output_file_id')

curl -k -s https://localhost:8000/v1/files/$OUTPUT_FILE_ID/content \
  -H "X-MaaS-Username: demo-user" \
  -H "Authorization: Bearer unused" | jq

# Step 5: View metrics
# Option 1: Query Prometheus UI at http://localhost:9091
# Option 2: Direct metrics endpoints
curl -s http://localhost:8081/metrics | grep batch_gateway  # API Server
curl -s http://localhost:9090/metrics | grep batch_gateway  # Processor
```

## Demo 2: Batch Cancellation (2 minutes)

```bash
# Step 1: Upload batch file
FILE_RESPONSE=$(curl -k -s -X POST https://localhost:8000/v1/files \
  -H "X-MaaS-Username: demo-user" \
  -H "Authorization: Bearer unused" \
  -F "purpose=batch" \
  -F "file=@batch_input.jsonl;type=application/jsonl")

FILE_ID=$(echo $FILE_RESPONSE | jq -r '.id')
echo "Uploaded file: $FILE_ID"

# Step 2: Create batch job
BATCH_RESPONSE=$(curl -k -s -X POST https://localhost:8000/v1/batches \
  -H "X-MaaS-Username: demo-user" \
  -H "Authorization: Bearer unused" \
  -H "Content-Type: application/json" \
  -d "{\"input_file_id\":\"$FILE_ID\",\"endpoint\":\"/v1/chat/completions\",\"completion_window\":\"24h\"}")

BATCH_ID=$(echo $BATCH_RESPONSE | jq -r '.id')
echo "Created batch: $BATCH_ID"

# Step 3: Cancel immediately
curl -k -s -X POST https://localhost:8000/v1/batches/$BATCH_ID/cancel \
  -H "X-MaaS-Username: demo-user" \
  -H "Authorization: Bearer unused" | jq

# Step 4: Verify cancelled status
curl -k -s https://localhost:8000/v1/batches/$BATCH_ID \
  -H "X-MaaS-Username: demo-user" \
  -H "Authorization: Bearer unused" | jq '.status'

# Step 5: Get partial results (if any)
OUTPUT_FILE_ID=$(curl -k -s https://localhost:8000/v1/batches/$BATCH_ID \
  -H "X-MaaS-Username: demo-user" \
  -H "Authorization: Bearer unused" | jq -r '.output_file_id')

if [ "$OUTPUT_FILE_ID" != "null" ]; then
  curl -k -s https://localhost:8000/v1/files/$OUTPUT_FILE_ID/content \
    -H "X-MaaS-Username: demo-user" \
    -H "Authorization: Bearer unused" | jq
else
  echo "No output file generated (batch cancelled before any requests completed)"
fi
```

## One-Liner Commands

```bash
# List all batches
curl -k -s https://localhost:8000/v1/batches \
  -H "X-MaaS-Username: demo-user" \
  -H "Authorization: Bearer unused" | jq

# List all files
curl -k -s https://localhost:8000/v1/files \
  -H "X-MaaS-Username: demo-user" \
  -H "Authorization: Bearer unused" | jq

# Get batch status
curl -k -s https://localhost:8000/v1/batches/$BATCH_ID \
  -H "X-MaaS-Username: demo-user" \
  -H "Authorization: Bearer unused" | jq '.status'

# Download file content
curl -k -s https://localhost:8000/v1/files/$FILE_ID/content \
  -H "X-MaaS-Username: demo-user" \
  -H "Authorization: Bearer unused"

# API Server health
curl -s http://localhost:8081/health | jq

# Processor health
curl -s http://localhost:9090/health | jq

# View traces in Jaeger
open http://localhost:16686

# View metrics in Prometheus
open http://localhost:9091
```

## Useful Monitoring Commands

```bash
# Watch batch status in real-time
watch -n 2 "curl -k -s https://localhost:8000/v1/batches/$BATCH_ID \
  -H 'X-MaaS-Username: demo-user' \
  -H 'Authorization: Bearer unused' | jq '{id:.id,status:.status,request_counts:.request_counts}'"

# Monitor API server metrics (direct)
watch -n 5 "curl -s http://localhost:8081/metrics | grep -E '(batch_gateway_api_http_requests_total|batch_gateway_api_batch_jobs_total)'"

# Monitor processor metrics (direct)
watch -n 5 "curl -s http://localhost:9090/metrics | grep -E '(batch_gateway_processor_jobs_processed_total|batch_gateway_processor_job_duration_seconds)'"

# Query Prometheus for metrics (requires PromQL)
curl -s 'http://localhost:9091/api/v1/query?query=batch_gateway_processor_jobs_processed_total' | jq '.data.result'

# View processor logs
kubectl logs -l app.kubernetes.io/component=processor -n default -f

# View API server logs
kubectl logs -l app.kubernetes.io/component=apiserver -n default -f
```

## Cleanup

```bash
# Delete file
curl -k -s -X DELETE https://localhost:8000/v1/files/$FILE_ID \
  -H "X-MaaS-Username: demo-user" \
  -H "Authorization: Bearer unused"
```

## Troubleshooting

### Q: Connection refused

```bash
# Check if kind cluster is running
kind get clusters

# Check if services are deployed
kubectl get pods -n default

# Redeploy if needed
make dev-deploy
```

**Note**: The kind cluster uses `extraPortMappings` to map NodePort services directly to localhost. No separate kubectl port-forward is needed.

### Q: Batch stuck in validating

```bash
# Check processor is running
kubectl get pods -l app=batch-gateway-processor -n default

# View processor logs
kubectl logs -l app=batch-gateway-processor -n default --tail=50
```

### Q: Need to reset everything

```bash
# Delete all resources and redeploy
make dev-clean
make dev-deploy
```
