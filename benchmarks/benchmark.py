#!/usr/bin/env python3
"""
Benchmark: Batch Gateway Effectiveness in Shared Clusters

Orchestrates guidellm burst/idle cycles alongside batch workloads to measure
whether gated dispatch protects interactive latency while batch makes SLO progress.

Supports 6 scenarios:
  0 - Interactive only (baseline)
  1 - No batch-gateway (batch as regular requests)
  2 - Ungated batch (aggressive concurrency, no AIMD)
  3 - AIMD only (processor-side adaptive concurrency)
  4 - AIMD + llm-d Router flow control (two-layer protection)
  5 - Async processor (blocked on integration)

Usage:
    python3 benchmarks/benchmark.py \
        --context my-k8s-context \
        --scenarios 0 2 3 4 \
        --burst-rate 15 --idle-rate 1 \
        --burst-seconds 90 --idle-seconds 90 \
        --cycles 3 \
        --results-dir ./benchmarks/results/run-01
"""

import argparse
import csv
import datetime
import json
import os
import subprocess
import sys
import textwrap
import time
from dataclasses import dataclass, field
from pathlib import Path

import yaml

SCENARIO_NAMES = {
    0: "interactive-only",
    1: "no-batch-gateway",
    2: "ungated",
    3: "aimd",
    4: "aimd-flow-control",
    5: "async",
}

SCRIPT_DIR = Path(__file__).parent.resolve()


def load_profile():
    """Load default parameter profile if available."""
    profile_path = SCRIPT_DIR / "profiles" / "default.yaml"
    if profile_path.exists():
        with open(profile_path) as f:
            return yaml.safe_load(f)
    return {}


@dataclass
class BenchmarkConfig:
    context: str
    scenarios: list
    model: str
    burst_rate: int
    idle_rate: int
    burst_seconds: int
    idle_seconds: int
    cycles: int
    batch_size: int
    num_jobs: int
    prompt_tokens: int
    num_system_prompts: int
    results_dir: Path
    target: str


@dataclass
class PhaseMetrics:
    phase: str
    cycle: int
    ttft_p50: float = 0.0
    ttft_p95: float = 0.0
    ttft_p99: float = 0.0
    itl_p50: float = 0.0
    itl_p95: float = 0.0
    itl_p99: float = 0.0
    req_latency_p50: float = 0.0
    req_latency_p95: float = 0.0
    ok_rps: float = 0.0
    err_rps: float = 0.0
    error_rate: float = 0.0
    completed: int = 0
    errors: int = 0


@dataclass
class ScenarioResult:
    scenario: int
    name: str
    phases: list = field(default_factory=list)
    batch_timeline: list = field(default_factory=list)


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------


def log(msg):
    print(f"[{time.strftime('%H:%M:%S')}] {msg}", flush=True)


def kubectl(args, context, namespace=None, capture=True, check=True):
    cmd = ["kubectl", f"--context={context}"]
    if namespace:
        cmd.extend(["-n", namespace])
    cmd.extend(args)
    result = subprocess.run(cmd, capture_output=capture, text=True, check=check)
    return result.stdout.strip() if capture else None


def kubectl_apply(yaml_str, context, namespace):
    cmd = ["kubectl", f"--context={context}", "-n", namespace, "apply", "-f", "-"]
    subprocess.run(cmd, input=yaml_str, text=True, check=True)


def namespace_for_scenario(scenario):
    return f"batch-bench-s{scenario}"


# ---------------------------------------------------------------------------
# Cleanup
# ---------------------------------------------------------------------------


def cleanup_namespace(context, namespace):
    """Reset namespace state between runs: truncate DBs, restart deployments."""
    log(f"Cleaning up {namespace}")

    kubectl(["delete", "job", "--all", "--ignore-not-found"],
            context, namespace, check=False)
    kubectl(["delete", "configmap", "-l", "batch-benchmark=true",
             "--ignore-not-found"], context, namespace, check=False)

    # Scale down
    kubectl(["scale", "deployment",
             "batch-gateway-processor", "batch-gateway-apiserver",
             "--replicas=0"], context, namespace, check=False)
    time.sleep(8)

    # Truncate PostgreSQL
    kubectl(["run", "--rm", "-i", "pg-nuke", "--image=postgres:16",
             "--restart=Never", "--env=PGPASSWORD=benchmarkpw", "--",
             "psql", "-h", "postgresql", "-U", "postgres", "-d", "batchgateway",
             "-c", "TRUNCATE batch_items, file_items CASCADE;"],
            context, namespace, check=False)

    # Flush Redis
    kubectl(["run", "--rm", "-i", "redis-del", "--image=redis",
             "--restart=Never", "--",
             "sh", "-c",
             'for key in $(redis-cli -h redis-master KEYS "*"); do '
             'redis-cli -h redis-master DEL "$key"; done'],
            context, namespace, check=False)

    # Scale back up
    kubectl(["scale", "deployment",
             "batch-gateway-processor", "batch-gateway-apiserver",
             "--replicas=1"], context, namespace, check=False)

    kubectl(["rollout", "status", "deployment/batch-gateway-processor",
             "--timeout=60s"], context, namespace, check=False)
    time.sleep(5)
    log(f"  Cleanup complete for {namespace}")


# ---------------------------------------------------------------------------
# Batch submission
# ---------------------------------------------------------------------------


def _batch_submit_script():
    """Python script that runs inside a K8s Job to submit a batch."""
    return textwrap.dedent("""\
        import json, os, sys, time
        from urllib.request import urlopen, Request

        base_url = os.environ["BATCH_GATEWAY_URL"]
        input_file = os.environ["INPUT_FILE"]
        completion_window = os.environ["COMPLETION_WINDOW"]

        # Upload file
        with open(input_file, "rb") as f:
            file_data = f.read()

        boundary = "----BatchBoundary"
        body = (
            f"--{boundary}\\r\\n"
            f'Content-Disposition: form-data; name="purpose"\\r\\n\\r\\n'
            f"batch\\r\\n"
            f"--{boundary}\\r\\n"
            f'Content-Disposition: form-data; name="file"; filename="input.jsonl"\\r\\n'
            f"Content-Type: application/octet-stream\\r\\n\\r\\n"
        ).encode() + file_data + f"\\r\\n--{boundary}--\\r\\n".encode()
        req = Request(
            f"{base_url}/v1/files",
            data=body,
            headers={
                "Content-Type": f"multipart/form-data; boundary={boundary}",
                "Authorization": "Bearer benchmark",
            },
        )
        resp = json.loads(urlopen(req).read())
        file_id = resp["id"]
        print(f"Uploaded: {file_id}", flush=True)

        # Create batch
        batch_body = json.dumps({
            "input_file_id": file_id,
            "endpoint": "/v1/chat/completions",
            "completion_window": completion_window,
        }).encode()
        req = Request(
            f"{base_url}/v1/batches",
            data=batch_body,
            headers={
                "Content-Type": "application/json",
                "Authorization": "Bearer benchmark",
            },
        )
        resp = json.loads(urlopen(req).read())
        batch_id = resp["id"]
        print(f"Batch: {batch_id}", flush=True)

        # Poll until terminal
        timeout = 7200
        elapsed = 0
        while elapsed < timeout:
            req = Request(
                f"{base_url}/v1/batches/{batch_id}",
                headers={"Authorization": "Bearer benchmark"},
            )
            status = json.loads(urlopen(req).read())
            s = status["status"]
            c = status["request_counts"].get("completed", 0)
            t = status["request_counts"].get("total", 0)
            print(f"Batch {batch_id}: status={s} completed={c}/{t} ({elapsed}s)", flush=True)
            if s in ("completed", "failed", "cancelled", "expired"):
                print(f"Terminal: {s}", flush=True)
                break
            time.sleep(5)
            elapsed += 5
        else:
            print("Timed out", flush=True)
    """)


def submit_batches(cfg, namespace):
    """Submit batch jobs using pre-generated JSONL files."""
    if cfg.num_jobs == 0:
        return

    completion_windows = ["30m", "2h", "24h"]
    job_names = ["job-a", "job-b", "job-c"]

    for i in range(min(cfg.num_jobs, 3)):
        name = job_names[i]
        window = completion_windows[i]
        jsonl_path = cfg.results_dir / f"{name}.jsonl"

        if not jsonl_path.exists():
            log(f"  Generating prompts for {name}...")
            subprocess.run([
                sys.executable, str(SCRIPT_DIR / "generate_prompts.py"),
                "--num-requests", str(cfg.batch_size),
                "--num-system-prompts", str(cfg.num_system_prompts),
                "--prompt-tokens", str(cfg.prompt_tokens),
                "--model", cfg.model,
                "--seed", str(42 + i),
                "--output", str(jsonl_path),
            ], check=True)

        log(f"  Submitting {name} (window={window}, size={cfg.batch_size})")
        script = _batch_submit_script()
        indented = "\n".join("          " + line for line in script.splitlines())

        cm_yaml = textwrap.dedent(f"""\
        apiVersion: v1
        kind: ConfigMap
        metadata:
          name: {name}-script
          labels:
            batch-benchmark: "true"
        data:
          script.py: |
        """) + indented + "\n"

        job_yaml = textwrap.dedent(f"""\
        apiVersion: batch/v1
        kind: Job
        metadata:
          name: {name}
          labels:
            batch-benchmark: "true"
        spec:
          backoffLimit: 0
          template:
            spec:
              restartPolicy: Never
              containers:
                - name: batch-submit
                  image: python:3.12-slim
                  env:
                    - name: BATCH_GATEWAY_URL
                      value: "http://batch-gateway-apiserver:8000"
                    - name: INPUT_FILE
                      value: "/data/{name}.jsonl"
                    - name: COMPLETION_WINDOW
                      value: "{window}"
                  command: ["python3", "-u", "/scripts/script.py"]
                  volumeMounts:
                    - name: script
                      mountPath: /scripts
                    - name: data
                      mountPath: /data
              volumes:
                - name: script
                  configMap:
                    name: {name}-script
                - name: data
                  configMap:
                    name: {name}-data
        """)

        # Upload the JSONL as a ConfigMap (for small files) or use PVC for large ones
        # For benchmark sizes (1000 requests), ConfigMap is fine
        kubectl(["create", "configmap", f"{name}-data",
                 f"--from-file={name}.jsonl={jsonl_path}",
                 "-l", "batch-benchmark=true"],
                cfg.context, namespace, check=False)
        kubectl_apply(cm_yaml, cfg.context, namespace)
        kubectl_apply(job_yaml, cfg.context, namespace)


# ---------------------------------------------------------------------------
# Interactive traffic (guidellm)
# ---------------------------------------------------------------------------


def start_interactive_traffic(cfg, namespace):
    """Start guidellm burst/idle cycles."""
    log(f"  Starting interactive traffic: {cfg.cycles} cycles, "
        f"burst@{cfg.burst_rate}/s for {cfg.burst_seconds}s, "
        f"idle@{cfg.idle_rate}/s for {cfg.idle_seconds}s")

    cycle_lines = []
    for c in range(1, cfg.cycles + 1):
        cycle_lines.extend([
            f'echo "=== Phase {c}: IDLE ({cfg.idle_rate} req/s, {cfg.idle_seconds}s) ==="',
            f'guidellm benchmark run --target "$T" $COMMON '
            f'--profile constant --rate {cfg.idle_rate} --max-seconds {cfg.idle_seconds} '
            f'--output-dir /results --outputs "idle-{c}.csv"',
            f'echo "=== Phase {c}: BURST ({cfg.burst_rate} req/s, {cfg.burst_seconds}s) ==="',
            f'guidellm benchmark run --target "$T" $COMMON '
            f'--profile constant --rate {cfg.burst_rate} --max-seconds {cfg.burst_seconds} '
            f'--output-dir /results --outputs "burst-{c}.csv"',
        ])

    script_lines = [
        f'T="{cfg.target}"',
        f'M="{cfg.model}"',
        f'COMMON="--request-format text_completions --model $M '
        f'--data prompt_tokens={cfg.prompt_tokens},output_tokens=512 '
        f'--processor $M --disable-console-interactive"',
        'mkdir -p /results',
    ] + cycle_lines + ['echo "=== Done ==="']

    indent = " " * 14
    script_block = "\n".join(indent + line for line in script_lines)

    yaml = f"""\
apiVersion: batch/v1
kind: Job
metadata:
  name: guidellm-interactive
  labels:
    batch-benchmark: "true"
spec:
  backoffLimit: 0
  template:
    spec:
      restartPolicy: Never
      containers:
        - name: guidellm
          image: ghcr.io/vllm-project/guidellm:latest
          env:
            - name: USER
              value: "guidellm"
            - name: HF_HUB_CACHE
              value: "/tmp/hf_cache"
          command:
            - sh
            - -c
            - |
{script_block}
          volumeMounts:
            - name: results
              mountPath: /results
      volumes:
        - name: results
          persistentVolumeClaim:
            claimName: benchmark-results
"""
    kubectl_apply(yaml, cfg.context, namespace)


# ---------------------------------------------------------------------------
# Monitoring
# ---------------------------------------------------------------------------


def get_batch_progress(context, namespace):
    """Aggregate batch progress across all batch jobs."""
    completed, total = 0, 0
    for name in ["job-a", "job-b", "job-c"]:
        try:
            out = kubectl(["logs", f"job/{name}", "--tail=5"],
                         context, namespace, check=False)
            for line in reversed(out.split("\n")):
                if "completed=" in line and "/" in line.split("completed=")[1]:
                    parts = line.split("completed=")[1].split()[0].split("/")
                    completed += int(parts[0])
                    total += int(parts[1])
                    break
        except Exception:
            pass
    return completed, total


def get_current_phase(context, namespace):
    """Get current guidellm phase from job logs."""
    try:
        out = kubectl(["logs", "job/guidellm-interactive", "--tail=20"],
                     context, namespace, check=False)
        phase = "unknown"
        for line in out.split("\n"):
            if line.startswith("==="):
                phase = line.strip("= ")
        return phase
    except Exception:
        return "unknown"


def monitor_scenario(cfg, scenario, namespace):
    """Monitor batch progress during interactive traffic, return timeline."""
    timeline = []
    start = time.time()
    total_duration = cfg.cycles * (cfg.burst_seconds + cfg.idle_seconds) + 300

    while True:
        elapsed = time.time() - start

        completed, total = get_batch_progress(cfg.context, namespace)
        phase = get_current_phase(cfg.context, namespace)

        timeline.append({
            "elapsed": round(elapsed),
            "completed": completed,
            "total": total,
            "phase": phase,
        })

        log(f"  [s{scenario}] {phase} | batch: {completed}/{total} | {int(elapsed)}s")

        # Check if guidellm is done
        try:
            gs = kubectl(["get", "pods", "-l", "job-name=guidellm-interactive",
                         "-o", "jsonpath={.items[0].status.phase}"],
                        cfg.context, namespace, check=False)
            if gs in ("Succeeded", "Failed"):
                log(f"  [s{scenario}] Interactive traffic complete")
                break
        except Exception:
            pass

        if elapsed > total_duration:
            log(f"  [s{scenario}] Timeout, stopping monitor")
            break

        time.sleep(10)

    return timeline


# ---------------------------------------------------------------------------
# Result collection
# ---------------------------------------------------------------------------


def collect_results(cfg, scenario, namespace):
    """Collect guidellm CSVs from the PVC via a helper pod."""
    dest = cfg.results_dir / f"scenario-{scenario}"
    dest.mkdir(parents=True, exist_ok=True)

    helper = f"results-helper-s{scenario}"
    helper_yaml = textwrap.dedent(f"""\
    apiVersion: v1
    kind: Pod
    metadata:
      name: {helper}
      labels:
        batch-benchmark: "true"
    spec:
      restartPolicy: Never
      containers:
        - name: helper
          image: busybox
          command: ["sleep", "300"]
          volumeMounts:
            - name: results
              mountPath: /results
      volumes:
        - name: results
          persistentVolumeClaim:
            claimName: benchmark-results
    """)

    kubectl_apply(helper_yaml, cfg.context, namespace)
    kubectl(["wait", "--for=condition=Ready", f"pod/{helper}", "--timeout=30s"],
            cfg.context, namespace, check=False)
    time.sleep(2)

    # List and copy CSV files
    try:
        file_list = kubectl(["exec", helper, "--", "ls", "/results"],
                           cfg.context, namespace, check=False)
        for fname in file_list.split():
            if fname.endswith(".csv"):
                local_path = dest / fname
                subprocess.run([
                    "kubectl", f"--context={cfg.context}", "-n", namespace,
                    "cp", f"{helper}:/results/{fname}", str(local_path)
                ], check=False)
        log(f"  Collected results to {dest}")
    except Exception as e:
        log(f"  WARNING: Failed to collect results: {e}")

    # Cleanup helper
    kubectl(["delete", "pod", helper, "--ignore-not-found"],
            cfg.context, namespace, check=False)

    return dest


# ---------------------------------------------------------------------------
# Report generation
# ---------------------------------------------------------------------------


def parse_guidellm_csv(csv_path):
    """Parse a guidellm output CSV into PhaseMetrics."""
    # TODO: Implement CSV parsing based on guidellm output format
    # This will be fleshed out in PR 2 when we validate against real guidellm output
    return PhaseMetrics(phase="unknown", cycle=0)


def generate_html_report(cfg, results):
    """Generate an HTML comparison report across scenarios."""
    report_path = cfg.results_dir / "report.html"

    # Build summary data
    scenario_rows = []
    for result in results:
        scenario_rows.append(f"<tr><td>{result.scenario}</td><td>{result.name}</td>"
                           f"<td>{len(result.batch_timeline)} data points</td></tr>")

    timelines_json = {}
    for result in results:
        timelines_json[result.name] = result.batch_timeline

    html = textwrap.dedent(f"""\
    <!DOCTYPE html>
    <html>
    <head>
        <title>Batch Gateway Benchmark Report</title>
        <script src="https://cdn.jsdelivr.net/npm/chart.js"></script>
        <script src="https://cdn.jsdelivr.net/npm/chartjs-plugin-annotation@3"></script>
        <style>
            body {{ font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', sans-serif;
                   margin: 40px; max-width: 1200px; background: #fafafa; color: #333; }}
            h1 {{ color: #1a1a1a; border-bottom: 2px solid #e5e5e5; padding-bottom: 12px; }}
            h2 {{ color: #444; margin-top: 40px; }}
            .card {{ background: white; border-radius: 8px; padding: 24px; margin: 20px 0;
                    box-shadow: 0 1px 3px rgba(0,0,0,0.1); }}
            table {{ border-collapse: collapse; width: 100%; margin: 20px 0; }}
            th, td {{ padding: 10px 16px; text-align: left; border-bottom: 1px solid #eee; }}
            th {{ background: #f5f5f5; font-weight: 600; }}
            .chart-container {{ background: white; border-radius: 8px; padding: 20px;
                              margin: 20px 0; box-shadow: 0 1px 3px rgba(0,0,0,0.1); }}
            canvas {{ max-height: 400px; }}
        </style>
    </head>
    <body>
        <h1>Batch Gateway Benchmark Report</h1>

        <div class="card">
            <h2 style="margin-top:0">Configuration</h2>
            <table>
                <tr><td><strong>Model</strong></td><td>{cfg.model}</td></tr>
                <tr><td><strong>Interactive</strong></td>
                    <td>{cfg.burst_rate} req/s burst ({cfg.burst_seconds}s),
                        {cfg.idle_rate} req/s idle ({cfg.idle_seconds}s),
                        {cfg.cycles} cycles</td></tr>
                <tr><td><strong>Batch</strong></td>
                    <td>{cfg.num_jobs} jobs x {cfg.batch_size} requests</td></tr>
                <tr><td><strong>Prompts</strong></td>
                    <td>{cfg.prompt_tokens} tokens, {cfg.num_system_prompts} system prompts</td></tr>
                <tr><td><strong>Scenarios</strong></td>
                    <td>{', '.join(f's{r.scenario} ({r.name})' for r in results)}</td></tr>
            </table>
        </div>

        <h2>Summary</h2>
        <table>
            <tr><th>#</th><th>Scenario</th><th>Status</th></tr>
            {''.join(scenario_rows)}
        </table>

        <h2>Batch Completion Timeline</h2>
        <div class="chart-container">
            <canvas id="timelineChart"></canvas>
        </div>

        <script>
        const timelines = {json.dumps(timelines_json)};
        const colors = ['#6b7280', '#ef4444', '#f59e0b', '#3b82f6', '#22c55e', '#8b5cf6'];
        const datasets = [];
        let idx = 0;
        for (const [name, data] of Object.entries(timelines)) {{
            datasets.push({{
                label: name,
                data: data.map(d => ({{x: d.elapsed, y: d.completed}})),
                borderColor: colors[idx % colors.length],
                fill: false, tension: 0.1, pointRadius: 2, borderWidth: 2,
            }});
            idx++;
        }}

        new Chart(document.getElementById('timelineChart'), {{
            type: 'line',
            data: {{ datasets }},
            options: {{
                responsive: true,
                plugins: {{
                    title: {{ display: true, text: 'Batch Requests Completed Over Time' }}
                }},
                scales: {{
                    x: {{ type: 'linear', title: {{ display: true, text: 'Time (s)' }} }},
                    y: {{ title: {{ display: true, text: 'Completed' }}, beginAtZero: true }}
                }}
            }}
        }});
        </script>

        <div class="card">
            <h2 style="margin-top:0">Next Steps</h2>
            <p>Detailed per-phase TTFT/ITL breakdown and infrastructure metrics
            will be added in PR 2 (scenarios 0-2) and PR 3 (scenarios 3-4).</p>
        </div>
    </body>
    </html>
    """)

    report_path.write_text(html)
    log(f"Report written to {report_path}")
    return report_path


# ---------------------------------------------------------------------------
# Scenario runners
# ---------------------------------------------------------------------------


def run_scenario(cfg, scenario):
    """Run a single benchmark scenario end-to-end."""
    name = SCENARIO_NAMES[scenario]
    namespace = namespace_for_scenario(scenario)
    log(f"━━━ Scenario {scenario}: {name} ━━━")

    if scenario == 5:
        log("  ERROR: Scenario 5 (async) is blocked on async-processor integration")
        return ScenarioResult(scenario=scenario, name=name)

    # Verify namespace exists
    try:
        kubectl(["get", "ns", namespace], cfg.context)
    except subprocess.CalledProcessError:
        log(f"  ERROR: Namespace {namespace} does not exist. Run setup.sh first.")
        return ScenarioResult(scenario=scenario, name=name)

    # Cleanup previous run
    if scenario >= 2:
        cleanup_namespace(cfg.context, namespace)

    # Submit batch (scenarios 2-4 only)
    if scenario >= 2:
        submit_batches(cfg, namespace)
        time.sleep(10)

    # Start interactive traffic (all scenarios)
    start_interactive_traffic(cfg, namespace)
    time.sleep(30)  # Let guidellm validate and start

    # Monitor
    if scenario >= 2:
        timeline = monitor_scenario(cfg, scenario, namespace)
    else:
        # Scenarios 0-1: just wait for guidellm to finish
        timeline = []
        total_wait = cfg.cycles * (cfg.burst_seconds + cfg.idle_seconds) + 120
        log(f"  Waiting up to {total_wait}s for interactive traffic to complete...")
        start = time.time()
        while time.time() - start < total_wait:
            try:
                gs = kubectl(["get", "pods", "-l", "job-name=guidellm-interactive",
                             "-o", "jsonpath={.items[0].status.phase}"],
                            cfg.context, namespace, check=False)
                if gs in ("Succeeded", "Failed"):
                    break
            except Exception:
                pass
            time.sleep(10)

    # Collect results
    collect_results(cfg, scenario, namespace)

    return ScenarioResult(scenario=scenario, name=name, batch_timeline=timeline)


# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------


def main():
    profile = load_profile()
    bench_cfg = profile.get("benchmark", {})
    prompt_cfg = profile.get("prompt", {})

    parser = argparse.ArgumentParser(
        description="Batch Gateway Benchmark Orchestrator"
    )
    parser.add_argument("--context", required=True, help="kubectl context")
    parser.add_argument("--scenarios", type=int, nargs="+", default=[2],
                        help="Scenarios to run (default: [2])")
    parser.add_argument("--model",
                        default=bench_cfg.get("model", "Qwen/Qwen3-8B"),
                        help="Model name (default: Qwen/Qwen3-8B)")
    parser.add_argument("--burst-rate", type=int,
                        default=bench_cfg.get("burst_rate", 15),
                        help="Requests/s during burst (default: 15)")
    parser.add_argument("--idle-rate", type=int,
                        default=bench_cfg.get("idle_rate", 1),
                        help="Requests/s during idle (default: 1)")
    parser.add_argument("--burst-seconds", type=int,
                        default=bench_cfg.get("burst_seconds", 90),
                        help="Duration of burst phase (default: 90)")
    parser.add_argument("--idle-seconds", type=int,
                        default=bench_cfg.get("idle_seconds", 90),
                        help="Duration of idle phase (default: 90)")
    parser.add_argument("--cycles", type=int,
                        default=bench_cfg.get("cycles", 3),
                        help="Number of burst/idle cycles (default: 3)")
    parser.add_argument("--batch-size", type=int,
                        default=bench_cfg.get("batch_size", 1000),
                        help="Requests per batch job (default: 1000)")
    parser.add_argument("--num-jobs", type=int,
                        default=bench_cfg.get("num_jobs", 3),
                        help="Concurrent batch jobs (default: 3)")
    parser.add_argument("--prompt-tokens", type=int,
                        default=prompt_cfg.get("prompt_tokens", 256),
                        help="Input tokens per prompt (default: 256)")
    parser.add_argument("--num-system-prompts", type=int,
                        default=prompt_cfg.get("num_system_prompts", 5),
                        help="Distinct system prompts (default: 5)")
    parser.add_argument("--results-dir", type=Path,
                        default=Path("benchmarks/results/latest"),
                        help="Output directory (default: benchmarks/results/latest)")
    parser.add_argument("--target",
                        default="http://llm-d-inference-gateway-istio",
                        help="Inference gateway URL (default: http://llm-d-inference-gateway-istio)")

    args = parser.parse_args()
    args.results_dir.mkdir(parents=True, exist_ok=True)

    cfg = BenchmarkConfig(
        context=args.context,
        scenarios=args.scenarios,
        model=args.model,
        burst_rate=args.burst_rate,
        idle_rate=args.idle_rate,
        burst_seconds=args.burst_seconds,
        idle_seconds=args.idle_seconds,
        cycles=args.cycles,
        batch_size=args.batch_size,
        num_jobs=args.num_jobs,
        prompt_tokens=args.prompt_tokens,
        num_system_prompts=args.num_system_prompts,
        results_dir=args.results_dir,
        target=args.target,
    )

    log("=== Batch Gateway Benchmark ===")
    log(f"Scenarios: {[f'{s} ({SCENARIO_NAMES[s]})' for s in cfg.scenarios]}")
    log(f"Traffic: {cfg.burst_rate} req/s burst ({cfg.burst_seconds}s), "
        f"{cfg.idle_rate} req/s idle ({cfg.idle_seconds}s), {cfg.cycles} cycles")
    log(f"Batch: {cfg.num_jobs} jobs x {cfg.batch_size} requests")
    log(f"Results: {cfg.results_dir}")

    # Validate scenarios
    for s in cfg.scenarios:
        if s not in SCENARIO_NAMES:
            log(f"ERROR: Unknown scenario {s}. Valid: 0-5")
            sys.exit(1)

    # Run each scenario
    results = []
    for scenario in cfg.scenarios:
        result = run_scenario(cfg, scenario)
        results.append(result)

    # Save timelines
    for result in results:
        if result.batch_timeline:
            timeline_path = cfg.results_dir / f"scenario-{result.scenario}-timeline.json"
            timeline_path.write_text(json.dumps(result.batch_timeline, indent=2))

    # Generate report
    generate_html_report(cfg, results)

    # Write machine-readable run metadata
    metadata = {
        "profile": "default",
        "parameters": {
            "model": cfg.model,
            "burst_rate": cfg.burst_rate,
            "idle_rate": cfg.idle_rate,
            "burst_seconds": cfg.burst_seconds,
            "idle_seconds": cfg.idle_seconds,
            "cycles": cfg.cycles,
            "batch_size": cfg.batch_size,
            "num_jobs": cfg.num_jobs,
            "prompt_tokens": cfg.prompt_tokens,
            "num_system_prompts": cfg.num_system_prompts,
        },
        "versions": {
            "batch_gateway": "unknown",
            "router": "unknown",
            "vllm": "unknown",
        },
        "timestamp": datetime.datetime.utcnow().isoformat() + "Z",
    }
    metadata_path = cfg.results_dir / "run-metadata.json"
    with open(metadata_path, "w") as f:
        json.dump(metadata, f, indent=2)

    log("=== Benchmark complete ===")
    log(f"Results:  {cfg.results_dir}")
    log(f"Report:   {cfg.results_dir / 'report.html'}")
    log(f"Metadata: {metadata_path}")


if __name__ == "__main__":
    main()
