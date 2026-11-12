from __future__ import annotations

import argparse
import os
import sys

_E2E_ROOT = os.path.dirname(os.path.abspath(__file__))
if _E2E_ROOT not in sys.path:
    sys.path.insert(0, _E2E_ROOT)

from runners.orchestrator import Orchestrator


def main():
    parser = argparse.ArgumentParser(description="E2E test runner")
    parser.add_argument(
        "--tier",
        choices=["lightweight", "full", "nightly"],
        default="lightweight",
    )
    parser.add_argument("--models", default="qwen3-0.6b.yaml")
    parser.add_argument(
        "--model-yaml",
        default=None,
        help="Direct model yaml path (local mode)",
    )
    parser.add_argument("--pr-number", default=None)
    parser.add_argument("--pr-sha", default=None)
    parser.add_argument("--pr-repo", default=None)
    parser.add_argument("--report-dir", default=None)
    parser.add_argument("--workspace", default=None)
    parser.add_argument(
        "--local",
        action="store_true",
        help="Local mode: skip clone/install",
    )
    args = parser.parse_args()

    if args.local or not args.pr_number:
        args.local = True
    if args.model_yaml:
        args.models = os.path.basename(args.model_yaml)

    orch = Orchestrator(args)
    sys.exit(orch.run())


if __name__ == "__main__":
    main()
