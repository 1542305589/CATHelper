from __future__ import annotations

import subprocess
import sys

_TEST_DEPS = [
    "pytest", "pytest-asyncio", "httpx", "openpyxl", "jinja2",
    "pyyaml", "numpy", "psutil", "openai",
]


def install_deps(workspace: str) -> None:
    """pip install -e . + test deps."""
    subprocess.run(
        [sys.executable, "-m", "pip", "install", "-e", "."],
        cwd=workspace,
        check=True,
    )
    subprocess.run(
        [sys.executable, "-m", "pip", "install", *_TEST_DEPS],
        check=True,
    )
