# Resource Lifecycle

This document describes how each type of object in the batch gateway is created and deleted,
following the [OpenAI Batch API](https://platform.openai.com/docs/api-reference/batch) behavior.

## Batch

- **Creation:** Created when the user calls `POST /v1/batches`. Exists in database only, no physical files.
- **Deletion:** Never deleted — retained for audit and history. OpenAI does not expose a `DELETE /batches` endpoint either.

## Batch Event

- **Creation:** Created by batch gateway service. Exists in events queue.
- **Deletion:** Automatically deleted after the TTL expires (default: **30 days**, configurable via `batch_event_ttl_seconds`).

## Input File

- **Creation:** Created when the user calls `POST /v1/files` with `purpose=batch`.
- **Deletion:** Deleted by two triggers, both resulting in a full deletion (physical file + database record):
  - **User** calls `DELETE /v1/files/{file_id}` — deleted immediately.
  - **Cleanup service** removes the file on expiration.

    Expiration is resolved in this order:
    1) User-provided `expires_after` when calling `POST /v1/files` (`seconds` must be between **3600 (1 hour)** and **2592000 (30 days)**):
      ```
      -F expires_after[anchor]="created_at"
      -F expires_after[seconds]=86400
      ```
    2) Otherwise, falls back to configuration `default_file_expiration_seconds` (default: **90 days**).

    Deletion Rules:
    - If expiration **> 0**, the **cleanup service** deletes the input file after that many seconds.
    - If expiration **<= 0**, input files are **never deleted**.

## Output File and Error File

- **Creation:** Created by the batch processor after a batch job completes. If a batch is cancelled, **partial results** from requests that completed before cancellation are also written to the output file.
- **Deletion:** Deleted by two triggers, both resulting in a full deletion (physical file + database record):
  - **User** calls `DELETE /v1/files/{file_id}` — deleted immediately.
  - **Cleanup service** removes the file on expiration.

    Expiration is resolved in this order:
    1) User-provided `output_expires_after` when creating the batch via `POST /v1/batches` (`anchor` is the output/error file creation time, not the batch creation time; `seconds` must be between **3600 (1 hour)** and **2592000 (30 days)**):
      ```json
      "output_expires_after": {
          "anchor": "created_at",
          "seconds": 3600
      }
      ```
    2) Otherwise, falls back to configuration `default_output_expiration_seconds` (default: **90 days**).

    Deletion Rules:
    - If expiration **> 0**, the **cleanup service** deletes the output and error file after that many seconds.
    - If expiration **<= 0**, output and error files are **never deleted**.
