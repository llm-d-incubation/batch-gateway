#!/usr/bin/env python3
from __future__ import annotations
"""
Generate JSONL batch input files with configurable ISL/OSL distributions
and system-prompt diversity.

Produces OpenAI-compatible batch input files with configurable:
- Input Sequence Length (ISL) distribution (lognormal, normal, uniform, fixed)
- Output Sequence Length (OSL) distribution (lognormal, normal, uniform, fixed)
- Number of distinct system prompts (for prefix-cache evaluation)
- System prompt length (tokens)
- Model name

Usage:
    python3 benchmarks/generate_prompts.py \
        --num-requests 1000 \
        --num-system-prompts 32 \
        --model "Qwen/Qwen3-8B" \
        --output job-a.jsonl

    # Generate all 3 benchmark jobs at once:
    python3 benchmarks/generate_prompts.py \
        --num-requests 1000 \
        --multi-job \
        --output-dir benchmarks/results/
"""

import argparse
import json
import math
import random
import sys
from pathlib import Path

import yaml

try:
    from faker import Faker
except ImportError:
    print("ERROR: faker is required. Install with: pip install faker", file=sys.stderr)
    sys.exit(1)


# System prompt templates — meaningfully different personas to test prefix-cache grouping.
SYSTEM_PROMPT_TEMPLATES = [
    (
        "You are a senior software engineer specializing in {lang}. "
        "You write clean, well-tested code following industry best practices. "
        "Always explain your reasoning before providing code. "
        "When asked about architecture decisions, consider scalability, "
        "maintainability, and performance tradeoffs."
    ),
    (
        "You are a data scientist with expertise in {field}. "
        "You communicate complex statistical concepts in plain language. "
        "Always state your assumptions explicitly and suggest ways to validate results. "
        "When presenting findings, lead with the actionable insight."
    ),
    (
        "You are a technical writer creating documentation for {audience}. "
        "You prioritize clarity over brevity and use concrete examples. "
        "Structure responses with headers and bullet points when appropriate. "
        "Avoid jargon unless the audience is clearly technical."
    ),
    (
        "You are a security researcher analyzing {domain} systems. "
        "You think adversarially and identify potential attack vectors. "
        "Always suggest mitigations alongside vulnerabilities you identify. "
        "Prioritize findings by severity and exploitability."
    ),
    (
        "You are a distributed systems architect designing {scale} services. "
        "You reason carefully about consistency, availability, and partition tolerance. "
        "When discussing tradeoffs, reference real-world systems as examples. "
        "Always consider failure modes and recovery strategies."
    ),
]

TEMPLATE_FILLS = {
    "lang": ["Go", "Rust", "Python", "TypeScript", "Java"],
    "field": ["machine learning", "natural language processing", "time series analysis",
              "causal inference", "computer vision"],
    "audience": ["API consumers", "platform operators", "open-source contributors",
                 "enterprise architects", "junior developers"],
    "domain": ["cloud-native", "web application", "embedded", "IoT", "financial"],
    "scale": ["planet-scale", "multi-region", "real-time", "event-driven", "edge computing"],
}


def sample_from_distribution(rng, distribution, mean, stdev, min_val=1, max_val=None):
    """Sample a positive integer from the specified distribution."""
    if distribution == "fixed":
        return int(mean)
    elif distribution == "lognormal":
        # Convert desired mean/stdev to lognormal mu/sigma parameters
        variance = stdev ** 2
        mu = math.log(mean ** 2 / math.sqrt(variance + mean ** 2))
        sigma = math.sqrt(math.log(1 + variance / mean ** 2))
        val = rng.lognormvariate(mu, sigma)
    elif distribution == "normal":
        val = rng.gauss(mean, stdev)
    elif distribution == "uniform":
        low = max(min_val, mean - stdev)
        high = mean + stdev
        val = rng.uniform(low, high)
    else:
        val = mean

    val = max(min_val, int(round(val)))
    if max_val is not None:
        val = min(val, max_val)
    return val


def generate_system_prompts(num_prompts: int, seed: int, target_tokens: int) -> list[str]:
    """Generate distinct system prompts of approximately target_tokens length."""
    fake = Faker()
    fake.seed_instance(seed)

    target_chars = target_tokens * 4

    prompts = []
    for i in range(num_prompts):
        template = SYSTEM_PROMPT_TEMPLATES[i % len(SYSTEM_PROMPT_TEMPLATES)]
        for key, values in TEMPLATE_FILLS.items():
            if "{" + key + "}" in template:
                fill_value = values[i % len(values)]
                template = template.replace("{" + key + "}", fill_value)
                break

        # Extend to target length if needed
        if len(template) < target_chars:
            padding = fake.text(max_nb_chars=max(target_chars - len(template), 100))
            template = template + " " + padding

        # Inject unique prefix to ensure distinct prefix-cache groups
        prompt_text = f"[Persona {i:03d}] {template}"
        prompts.append(prompt_text[:target_chars].strip())

    return prompts


def generate_user_prompt(fake: Faker, target_chars: int) -> str:
    """Generate a user prompt of approximately target_chars length."""
    text = ""
    while len(text) < target_chars:
        text += " " + fake.text(max_nb_chars=min(target_chars - len(text) + 50, 1000))
    return text[:target_chars].strip()


def generate_jsonl(
    num_requests: int,
    num_system_prompts: int,
    system_prompt_tokens: int,
    model: str,
    seed: int,
    output_path: Path,
    isl_distribution: str,
    isl_mean: float,
    isl_stdev: float,
    isl_max: int,
    osl_distribution: str,
    osl_mean: float,
    osl_stdev: float,
    osl_max: int,
    id_prefix: str = "req",
):
    """Generate a JSONL batch input file with ISL/OSL distributions."""
    fake = Faker()
    fake.seed_instance(seed)
    rng = random.Random(seed)

    system_prompts = generate_system_prompts(num_system_prompts, seed, system_prompt_tokens)

    with open(output_path, "w") as f:
        for i in range(num_requests):
            system_prompt = system_prompts[i % num_system_prompts]

            # Sample ISL for this request (user prompt portion)
            isl_tokens = sample_from_distribution(rng, isl_distribution, isl_mean, isl_stdev, min_val=16, max_val=isl_max)
            target_chars = isl_tokens * 4
            user_prompt = generate_user_prompt(fake, target_chars)

            # Sample OSL (max_tokens) for this request
            osl_tokens = sample_from_distribution(rng, osl_distribution, osl_mean, osl_stdev, min_val=1, max_val=osl_max)

            line = {
                "custom_id": f"{id_prefix}-{i:04d}",
                "method": "POST",
                "url": "/v1/chat/completions",
                "body": {
                    "model": model,
                    "max_tokens": osl_tokens,
                    "messages": [
                        {"role": "system", "content": system_prompt},
                        {"role": "user", "content": user_prompt},
                    ],
                },
            }
            f.write(json.dumps(line, separators=(",", ":")) + "\n")

            if (i + 1) % 500 == 0:
                print(f"  Generated {i + 1}/{num_requests}", file=sys.stderr)

    print(f"Generated {num_requests} requests -> {output_path}", file=sys.stderr)
    print(f"  ISL: {isl_distribution} (mean={isl_mean}, stdev={isl_stdev})", file=sys.stderr)
    print(f"  OSL: {osl_distribution} (mean={osl_mean}, stdev={osl_stdev})", file=sys.stderr)


def load_profile():
    """Load default parameter profile if available."""
    profile_path = Path(__file__).parent / "profiles" / "default.yaml"
    if profile_path.exists():
        with open(profile_path) as f:
            return yaml.safe_load(f)
    return {}


def main():
    profile = load_profile()
    prompt_cfg = profile.get("prompt", {})
    isl_cfg = prompt_cfg.get("isl", {})
    osl_cfg = prompt_cfg.get("osl", {})

    parser = argparse.ArgumentParser(
        description="Generate JSONL batch input files for benchmarking"
    )
    parser.add_argument("--num-requests", type=int,
                        default=prompt_cfg.get("num_requests", 1000),
                        help="Number of requests per file (default: 1000)")
    parser.add_argument("--num-system-prompts", type=int,
                        default=prompt_cfg.get("num_system_prompts", 32),
                        help="Number of distinct system prompts (default: 32)")
    parser.add_argument("--system-prompt-tokens", type=int,
                        default=prompt_cfg.get("system_prompt_tokens", 2048),
                        help="Approximate tokens per system prompt (default: 2048)")
    parser.add_argument("--model",
                        default=prompt_cfg.get("model", "Qwen/Qwen3-8B"),
                        help="Model name for requests (default: Qwen/Qwen3-8B)")
    parser.add_argument("--seed", type=int,
                        default=prompt_cfg.get("seed", 42),
                        help="Random seed for reproducibility (default: 42)")

    # ISL (Input Sequence Length) distribution
    parser.add_argument("--isl-distribution",
                        choices=["fixed", "lognormal", "normal", "uniform"],
                        default=isl_cfg.get("distribution", "lognormal"),
                        help="ISL distribution type (default: lognormal)")
    parser.add_argument("--isl-mean", type=float,
                        default=isl_cfg.get("mean", 1500),
                        help="ISL distribution mean in tokens (default: 1500)")
    parser.add_argument("--isl-stdev", type=float,
                        default=isl_cfg.get("stdev", 1200),
                        help="ISL distribution stdev in tokens (default: 1200)")
    parser.add_argument("--isl-max", type=int,
                        default=isl_cfg.get("max", 4096),
                        help="ISL maximum (clamp to model context length, default: 4096)")

    # OSL (Output Sequence Length) distribution
    parser.add_argument("--osl-distribution",
                        choices=["fixed", "lognormal", "normal", "uniform"],
                        default=osl_cfg.get("distribution", "lognormal"),
                        help="OSL distribution type (default: lognormal)")
    parser.add_argument("--osl-mean", type=float,
                        default=osl_cfg.get("mean", 500),
                        help="OSL distribution mean in tokens (default: 500)")
    parser.add_argument("--osl-stdev", type=float,
                        default=osl_cfg.get("stdev", 400),
                        help="OSL distribution stdev in tokens (default: 400)")
    parser.add_argument("--osl-max", type=int,
                        default=osl_cfg.get("max", 2048),
                        help="OSL maximum (clamp output length, default: 2048)")

    # Legacy compat
    parser.add_argument("--prompt-tokens", type=int, default=None,
                        help="(Legacy) Fixed ISL — equivalent to --isl-distribution=fixed --isl-mean=N")

    parser.add_argument("--output", type=Path, default=None,
                        help="Output JSONL file path (single-job mode)")
    parser.add_argument("--multi-job", action="store_true",
                        help="Generate 3 job files (job-a, job-b, job-c) with different SLO windows")
    parser.add_argument("--output-dir", type=Path, default=Path("benchmarks/results"),
                        help="Output directory for multi-job mode (default: benchmarks/results/)")

    args = parser.parse_args()

    # Legacy --prompt-tokens overrides ISL to fixed mode
    if args.prompt_tokens is not None:
        args.isl_distribution = "fixed"
        args.isl_mean = args.prompt_tokens
        args.isl_stdev = 0

    if args.multi_job:
        args.output_dir.mkdir(parents=True, exist_ok=True)
        jobs = [
            ("job-a", "tight SLO (30m)"),
            ("job-b", "moderate SLO (2h)"),
            ("job-c", "relaxed SLO (24h)"),
        ]
        for i, (name, description) in enumerate(jobs):
            print(f"\n=== Generating {name}: {description} ===", file=sys.stderr)
            output_path = args.output_dir / f"{name}.jsonl"
            generate_jsonl(
                num_requests=args.num_requests,
                num_system_prompts=args.num_system_prompts,
                system_prompt_tokens=args.system_prompt_tokens,
                model=args.model,
                seed=args.seed + i,
                output_path=output_path,
                isl_distribution=args.isl_distribution,
                isl_mean=args.isl_mean,
                isl_stdev=args.isl_stdev,
                isl_max=args.isl_max,
                osl_distribution=args.osl_distribution,
                osl_mean=args.osl_mean,
                osl_stdev=args.osl_stdev,
                osl_max=args.osl_max,
                id_prefix=name,
            )
        print(f"\nAll jobs generated in {args.output_dir}/", file=sys.stderr)
        print("Submit with completion_window: job-a=30m, job-b=2h, job-c=24h", file=sys.stderr)
    else:
        if args.output is None:
            args.output = Path("batch-input.jsonl")
        args.output.parent.mkdir(parents=True, exist_ok=True)
        generate_jsonl(
            num_requests=args.num_requests,
            num_system_prompts=args.num_system_prompts,
            system_prompt_tokens=args.system_prompt_tokens,
            model=args.model,
            seed=args.seed,
            output_path=args.output,
            isl_distribution=args.isl_distribution,
            isl_mean=args.isl_mean,
            isl_stdev=args.isl_stdev,
            isl_max=args.isl_max,
            osl_distribution=args.osl_distribution,
            osl_mean=args.osl_mean,
            osl_stdev=args.osl_stdev,
            osl_max=args.osl_max,
        )


if __name__ == "__main__":
    main()
