#!/usr/bin/env python3
"""
Generate JSONL batch input files with Faker-based prompts and system-prompt diversity.

Produces OpenAI-compatible batch input files with configurable:
- Number of requests
- Number of distinct system prompts (for prefix-cache evaluation)
- Prompt token length
- Model name

Usage:
    python3 benchmarks/generate_prompts.py \
        --num-requests 1000 \
        --num-system-prompts 5 \
        --prompt-tokens 256 \
        --model "Qwen/Qwen3-8B" \
        --output job-a.jsonl

    # Generate all 3 benchmark jobs at once:
    python3 benchmarks/generate_prompts.py \
        --num-requests 1000 \
        --num-system-prompts 5 \
        --prompt-tokens 256 \
        --model "Qwen/Qwen3-8B" \
        --multi-job \
        --output-dir benchmarks/results/
"""

import argparse
import json
import sys
from pathlib import Path

import yaml

try:
    from faker import Faker
except ImportError:
    print("ERROR: faker is required. Install with: pip install faker", file=sys.stderr)
    sys.exit(1)


# System prompt templates — meaningfully different personas to test prefix-cache grouping.
# Each shares a common structure but different content, so FNV-32a hashes are distinct.
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

# Fill-in values for templates
TEMPLATE_FILLS = {
    "lang": ["Go", "Rust", "Python", "TypeScript", "Java"],
    "field": ["machine learning", "natural language processing", "time series analysis",
              "causal inference", "computer vision"],
    "audience": ["API consumers", "platform operators", "open-source contributors",
                 "enterprise architects", "junior developers"],
    "domain": ["cloud-native", "web application", "embedded", "IoT", "financial"],
    "scale": ["planet-scale", "multi-region", "real-time", "event-driven", "edge computing"],
}


def generate_system_prompts(num_prompts: int, seed: int) -> list[str]:
    """Generate distinct system prompts by filling templates."""
    fake = Faker()
    fake.seed_instance(seed)

    prompts = []
    for i in range(num_prompts):
        template = SYSTEM_PROMPT_TEMPLATES[i % len(SYSTEM_PROMPT_TEMPLATES)]
        # Find the placeholder key in this template
        for key, values in TEMPLATE_FILLS.items():
            if "{" + key + "}" in template:
                fill_value = values[i % len(values)]
                template = template.replace("{" + key + "}", fill_value)
                break
        prompts.append(template)

    return prompts


def generate_user_prompt(fake: Faker, target_chars: int) -> str:
    """Generate a user prompt of approximately target_chars length."""
    # faker.text() generates lorem-ipsum-like paragraphs
    # We may need multiple calls to reach the target length
    text = ""
    while len(text) < target_chars:
        text += " " + fake.text(max_nb_chars=min(target_chars - len(text) + 50, 1000))
    return text[:target_chars].strip()


def generate_jsonl(
    num_requests: int,
    num_system_prompts: int,
    prompt_tokens: int,
    model: str,
    seed: int,
    output_path: Path,
    id_prefix: str = "req",
):
    """Generate a JSONL batch input file."""
    fake = Faker()
    fake.seed_instance(seed)

    system_prompts = generate_system_prompts(num_system_prompts, seed)
    # Approximate: 1 token ≈ 4 characters
    target_chars = prompt_tokens * 4

    with open(output_path, "w") as f:
        for i in range(num_requests):
            system_prompt = system_prompts[i % num_system_prompts]
            user_prompt = generate_user_prompt(fake, target_chars)

            line = {
                "custom_id": f"{id_prefix}-{i:04d}",
                "method": "POST",
                "url": "/v1/chat/completions",
                "body": {
                    "model": model,
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

    parser = argparse.ArgumentParser(
        description="Generate JSONL batch input files for benchmarking"
    )
    parser.add_argument("--num-requests", type=int,
                        default=prompt_cfg.get("num_requests", 1000),
                        help="Number of requests per file (default: 1000)")
    parser.add_argument("--num-system-prompts", type=int,
                        default=prompt_cfg.get("num_system_prompts", 5),
                        help="Number of distinct system prompts (default: 5)")
    parser.add_argument("--prompt-tokens", type=int,
                        default=prompt_cfg.get("prompt_tokens", 256),
                        help="Approximate input tokens per user prompt (default: 256)")
    parser.add_argument("--model",
                        default=prompt_cfg.get("model", "Qwen/Qwen3-8B"),
                        help="Model name for requests (default: Qwen/Qwen3-8B)")
    parser.add_argument("--seed", type=int,
                        default=prompt_cfg.get("seed", 42),
                        help="Random seed for reproducibility (default: 42)")
    parser.add_argument("--output", type=Path, default=None,
                        help="Output JSONL file path (single-job mode)")
    parser.add_argument("--multi-job", action="store_true",
                        help="Generate 3 job files (job-a, job-b, job-c) with different SLO windows")
    parser.add_argument("--output-dir", type=Path, default=Path("benchmarks/results"),
                        help="Output directory for multi-job mode (default: benchmarks/results/)")

    args = parser.parse_args()

    if args.multi_job:
        # Generate 3 jobs with different characteristics for SLO-deadline ordering demo
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
                prompt_tokens=args.prompt_tokens,
                model=args.model,
                seed=args.seed + i,  # Different seed per job for variety
                output_path=output_path,
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
            prompt_tokens=args.prompt_tokens,
            model=args.model,
            seed=args.seed,
            output_path=args.output,
        )


if __name__ == "__main__":
    main()
