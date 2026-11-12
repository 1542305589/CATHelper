from __future__ import annotations

import os
import subprocess


def fetch_code(workspace: str, pr_number: str = None, pr_sha: str = None,
               pr_repo: str = None, local: bool = False) -> str:
    """Clone PR code to workspace. If local=True, return workspace as-is."""
    if local:
        return workspace
    if not pr_repo:
        raise RuntimeError("pr_repo is required when not in local mode")
    if os.path.exists(workspace):
        subprocess.run(
            ["git", "fetch", "origin"], cwd=workspace, check=False,
        )
    else:
        subprocess.run(
            ["git", "clone", f"https://github.com/{pr_repo}", workspace],
            check=True,
        )
    if pr_sha:
        subprocess.run(
            ["git", "checkout", pr_sha], cwd=workspace, check=True,
        )
    return workspace
