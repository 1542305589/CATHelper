from __future__ import annotations

import contextlib
import os
import signal
import socket
import subprocess
import sys
import time
import urllib.request


def auto_select_port() -> int:
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def wait_for_url(url: str, timeout: float, interval: float = 1.0) -> bool:
    deadline = time.time() + timeout
    while time.time() < deadline:
        try:
            with urllib.request.urlopen(url, timeout=min(interval, 5.0)) as r:
                if r.status == 200:
                    return True
        except Exception:
            pass
        time.sleep(interval)
    return False


def terminate_process_tree(pid: int) -> None:
    try:
        import psutil
    except ImportError:
        psutil = None

    if psutil is not None:
        try:
            parent = psutil.Process(pid)
        except psutil.NoSuchProcess:
            return
        children = parent.children(recursive=True)
        try:
            parent.terminate()
            parent.wait(timeout=60)
        except (psutil.NoSuchProcess, psutil.TimeoutExpired):
            with contextlib.suppress(psutil.NoSuchProcess):
                parent.kill()
        for child in children:
            with contextlib.suppress(psutil.NoSuchProcess):
                child.terminate()
        _, still_alive = psutil.wait_procs(children, timeout=10)
        for child in still_alive:
            with contextlib.suppress(psutil.NoSuchProcess):
                child.kill()
        return

    if sys.platform == "win32":
        subprocess.run(
            ["taskkill", "/F", "/T", "/PID", str(pid)],
            check=False,
        )
        return

    try:
        os.killpg(os.getpgid(pid), signal.SIGTERM)
    except ProcessLookupError:
        return
    except Exception:
        with contextlib.suppress(ProcessLookupError):
            os.kill(pid, signal.SIGKILL)
