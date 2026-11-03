from __future__ import annotations

import argparse
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))))


def main():
    parser = argparse.ArgumentParser(
        description="Generate anomaly data for E2E injection tests"
    )
    parser.add_argument("--tokenizer", required=True, help="Model path for tokenizer")
    parser.add_argument("--model-name", default=None, help="Model name for cache dir")
    parser.add_argument(
        "--output-dir",
        default=None,
        help="Cache dir (default: data/anomalies/<model_name>/)",
    )
    parser.add_argument("--seed", type=int, default=42)
    args = parser.parse_args()

    model_name = args.model_name or os.path.basename(args.tokenizer.rstrip("/\\"))

    from tests.e2e.data.anomaly_data_builder import build_all

    data = build_all(
        model_path=args.tokenizer,
        model_name=model_name,
        cache_dir=args.output_dir,
        seed=args.seed,
    )

    cache_dir = args.output_dir or os.path.join(
        os.path.dirname(os.path.abspath(__file__)), model_name
    )

    print(f"Generated anomaly data for model: {model_name}")
    print(f"Cache directory: {cache_dir}")
    for name, payload in data.items():
        n_tokens = len(payload["logprobs"][0])
        topk = len(payload["logprobs"][0][0]) if n_tokens > 0 else 0
        print(f"  {name}: {n_tokens} tokens x {topk} topk")


if __name__ == "__main__":
    main()
