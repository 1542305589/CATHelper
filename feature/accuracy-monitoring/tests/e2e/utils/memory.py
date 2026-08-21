from __future__ import annotations

import os
import time

try:
    import psutil
    _HAS_PSUTIL = True
except ImportError:
    _HAS_PSUTIL = False


def get_rss_mb(pid: int = None) -> float:
    if pid is None:
        pid = os.getpid()
    if _HAS_PSUTIL:
        try:
            return psutil.Process(pid).memory_info().rss / 1024 / 1024
        except Exception:
            pass
    status_path = f"/proc/{pid}/status"
    if not os.path.exists(status_path):
        raise RuntimeError(
            f"cannot get RSS for pid {pid}: psutil unavailable and /proc not present"
        )
    with open(status_path) as f:
        for line in f:
            if line.startswith("VmRSS:"):
                return int(line.split()[1]) / 1024
    raise RuntimeError(f"cannot read VmRSS for pid {pid}")


def sample_memory(pid: int, interval: float = 1.0, duration: float = 60.0) -> list:
    samples = []
    deadline = time.time() + duration
    while time.time() < deadline:
        samples.append(get_rss_mb(pid))
        time.sleep(interval)
    return samples
