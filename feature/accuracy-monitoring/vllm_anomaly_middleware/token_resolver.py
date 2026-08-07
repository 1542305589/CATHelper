"""TokenTextResolver + tokenizer 获取（spec §4 / plan Task 2）。

同进程部署（--middleware）：vLLM 已缓存模型 tokenizer，本地 from_pretrained 命中、零外网。
model_hint（请求体 model 字段）解析失败 → loopback GET /v1/models 取 served id 兜底。
裸 served 名（如 Qwen3-0.6B，HF 缓存键为 Qwen/Qwen3-0.6B）→ HF 缓存扫描补全完整 repo id。
均失败 → None（软降级，strip 退回 null/bytes）。
"""
from __future__ import annotations

import logging
from typing import Any, List, Optional, Tuple

logger = logging.getLogger("vllm_anomaly_middleware")


def _from_pretrained(path: str, **kwargs: Any) -> Any:
    """transformers.AutoTokenizer.from_pretrained 的间接层（便于测试 monkeypatch）。

    默认补 trust_remote_code=True（与 token_categorizer.py 对齐，覆盖 Qwen/GLM
    自定义 tokenizer）；调用方显式传入值时不覆盖。
    """
    from transformers import AutoTokenizer  # vLLM 依赖，必在

    kwargs.setdefault("trust_remote_code", True)
    return AutoTokenizer.from_pretrained(path, **kwargs)


async def _fetch_served_model_id(server: Optional[Tuple]) -> Optional[str]:
    """loopback GET /v1/models → 返回首个 served model id；失败 None。"""
    if not server:
        return None
    try:
        host, port = server[0], int(server[1])
    except (TypeError, ValueError, IndexError):
        return None
    url = f"http://{host}:{port}/v1/models"
    import httpx

    try:
        async with httpx.AsyncClient(timeout=5.0) as c:
            r = await c.get(url)
        if r.status_code != 200:
            return None
        data = r.json()
    except Exception as exc:
        logger.warning("loopback /v1/models 失败: %s", exc)
        return None
    items = data.get("data") if isinstance(data, dict) else None
    if isinstance(items, list) and items:
        first = items[0]
        if isinstance(first, dict):
            mid = first.get("id")
            if isinstance(mid, str) and mid:
                return mid
    return None


async def acquire_tokenizer(
    model_hint: str, server: Optional[Tuple], explicit: Optional[str] = None
) -> Optional[Any]:
    """返回 tokenizer 对象或 None。

    顺序：from_pretrained(explicit/env) → from_pretrained(model_hint) → /v1/models served id
          → from_pretrained(served) → HF 缓存扫描补全完整 repo id → None。
    """
    if explicit:
        try:
            return _from_pretrained(explicit, local_files_only=True)
        except Exception as exc:
            logger.info("from_pretrained(explicit %r) 失败, 尝试后续策略: %s", explicit, exc)
    if model_hint:
        try:
            return _from_pretrained(model_hint, local_files_only=True)
        except Exception as exc:
            logger.info("from_pretrained(%r) 失败, 尝试 /v1/models: %s", model_hint, exc)
    try:
        mid = await _fetch_served_model_id(server)
    except Exception as exc:
        logger.warning("获取 served model id 失败: %s", exc)
        mid = None
    logger.info("loopback /v1/models -> served_model_id=%r", mid)
    if mid:
        try:
            return _from_pretrained(mid, local_files_only=True)
        except Exception as exc:
            logger.warning("from_pretrained(served %r) 失败, 尝试缓存扫描: %s", mid, exc)
    # 缓存扫描兜底：裸 served 名 → 完整 HF repo id（如 Qwen3-0.6B → Qwen/Qwen3-0.6B）
    seen: set = set()
    for hint in (model_hint, mid):
        if not hint or hint in seen:
            continue
        seen.add(hint)
        for repo_id in _scan_hf_cache_candidates(hint):
            if repo_id == hint:
                continue  # 已试过
            logger.info("cache scan: %r -> %r", hint, repo_id)
            try:
                return _from_pretrained(repo_id, local_files_only=True)
            except Exception as exc:
                logger.warning("from_pretrained(cache %r) 失败: %s", repo_id, exc)
    return None


def _scan_hf_cache_candidates(hint: str) -> List[str]:
    """扫描 HF 缓存，返回 repo_id 以 /<hint> 结尾或等于 <hint> 的候选（短优先）。

    场景：vLLM --model Qwen3-0.6B（裸名），HF 缓存键为 Qwen/Qwen3-0.6B。
    huggingface_hub 不可用（非 vLLM 部署环境）→ 返回 []。
    """
    if not hint:
        return []
    try:
        from huggingface_hub import scan_cache_dir
    except Exception as exc:
        logger.info("huggingface_hub 不可用, 跳过缓存扫描: %s", exc)
        return []
    try:
        cache = scan_cache_dir()
    except Exception as exc:
        logger.warning("scan_cache_dir 失败: %s", exc)
        return []
    candidates = []
    for repo in cache.repos:
        rid = repo.repo_id
        if rid == hint or rid.endswith("/" + hint):
            candidates.append(rid)
    candidates.sort(key=len)
    return candidates


class TokenTextResolver:
    """token_id -> 单 token surface 文本。仅被 ASGI 事件循环调用；进程内单例。"""

    def __init__(self, tokenizer: Any) -> None:
        self._tok = tokenizer
        self._cache: dict = {}

    def resolve(self, token_id: Any) -> Optional[str]:
        if token_id is None:
            return None
        try:
            tid = int(token_id)
        except (TypeError, ValueError):
            return None
        if tid in self._cache:
            return self._cache[tid]
        try:
            s = self._tok.decode([tid])
        except Exception:
            s = ""
        if not s:
            self._cache[tid] = None
            return None
        self._cache[tid] = s
        return s
