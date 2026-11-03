from __future__ import annotations

import time


class Timer:
    def __init__(self):
        self._start = None
        self._end = None

    def start(self) -> None:
        self._start = time.perf_counter()
        self._end = None

    def stop(self) -> None:
        self._end = time.perf_counter()

    @property
    def elapsed(self) -> float:
        end = self._end if self._end is not None else time.perf_counter()
        if self._start is None:
            return 0.0
        return end - self._start


async def measure_async(coro, repeats: int = 10) -> list:
    elapsed = []
    for _ in range(repeats):
        t = time.perf_counter()
        await coro()
        elapsed.append(time.perf_counter() - t)
    return elapsed
