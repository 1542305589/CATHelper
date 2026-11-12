from __future__ import annotations

import importlib.util
import os
import shutil

_REQUIRED_PY_DEPS = ("pytest", "pytest_asyncio")


def check_env(model_cfg: dict) -> None:
    """Check vllm, python, git available. Check model_path exists."""
    if not shutil.which("vllm"):
        raise RuntimeError("vllm executable not found in PATH")
    if not shutil.which("git"):
        raise RuntimeError("git executable not found in PATH")
    model_path = model_cfg.get("model_path")
    if not model_path or not os.path.isdir(model_path):
        raise RuntimeError(
            f"model_path not found or not a directory: {model_path}"
        )


def check_python_deps() -> None:
    """Check test runtime dependencies (pytest, pytest-asyncio) importable."""
    missing = [
        mod for mod in _REQUIRED_PY_DEPS
        if importlib.util.find_spec(mod) is None
    ]
    if missing:
        raise RuntimeError(
            "missing Python test dependencies: "
            + ", ".join(missing)
            + " (pip install pytest pytest-asyncio)"
        )
