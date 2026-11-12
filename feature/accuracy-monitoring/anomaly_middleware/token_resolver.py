"""TokenTextResolver + tokenizer 获取（spec §4 / plan Task 2）。

同进程部署（--middleware）：vLLM 已缓存模型 tokenizer，本地 from_pretrained 命中、零外网。
启动期同步加载：env → argv(--tokenizer) → argv(--model) → HF 缓存扫描。
均失败 → raise（启动期 fail-fast）。
"""
from __future__ import annotations

import sys
from dataclasses import dataclass
from typing import Any, List, Optional, Tuple

from .logging import get_logger

logger = get_logger()


def _from_pretrained(path: str, **kwargs: Any) -> Any:
    """transformers.AutoTokenizer.from_pretrained 的间接层（便于测试 monkeypatch）。

    默认补 trust_remote_code=True（与 token_categorizer.py 对齐，覆盖 Qwen/GLM
    自定义 tokenizer）；调用方显式传入值时不覆盖。
    """
    from transformers import AutoTokenizer  # vLLM 依赖，必在

    kwargs.setdefault("trust_remote_code", True)
    return AutoTokenizer.from_pretrained(path, **kwargs)


@dataclass
class VllmArgvInfo:
    """`vllm serve` 命令行解析结果。"""

    model: Optional[str] = None
    tokenizer: Optional[str] = None
    host: str = "127.0.0.1"
    port: int = 8000


# vLLM serve 中常见的需要消费下一个参数的 flag（非穷举）。
# 未列出的 --flag 视为布尔开关（不消费值），其后的非 - 开头参数仍可被识别为 model。
# 如果某个带值 flag 未在此集合中，其值可能被误认为 model 位置参数——
# argv 解析失败时仍有 HF 缓存兜底。
_VALUE_FLAGS = frozenset({
    "--served-model-name", "--middleware", "--download-dir",
    "--dtype", "--quantization", "--revision", "--tokenizer-revision",
    "--tokenizer-mode", "--chat-template", "--response-role",
    "--uvicorn-log-level", "--api-key", "--max-model-len",
    "--max-num-seqs", "--max-num-batched-token", "--block-size",
    "--swap-space", "--tensor-parallel-size", "--pipeline-parallel-size",
    "--gpu-memory-utilization", "--distributed-executor-backend",
    "--max-loras", "--max-lora-rank", "--max-cpu-loras",
    "--load-format", "--config-format", "--kv-cache-dtype",
    "--hf-overrides", "--quantization-param-path", "--model",
})


def parse_vllm_argv(argv: Optional[List[str]] = None) -> Optional[VllmArgvInfo]:
    """解析 `vllm serve <model> ... --tokenizer <path> ... --host H --port P`。

    返回 VllmArgvInfo(model, tokenizer, host, port) 或 None（非 vllm serve 命令）。
    支持 `--flag value` 与 `--flag=value` 两种形式。model 为 serve 后首个位置参数。
    """
    argv = list(sys.argv if argv is None else argv)
    try:
        serve_idx = argv.index("serve")
    except ValueError:
        return None

    info = VllmArgvInfo()
    i = serve_idx + 1
    while i < len(argv):
        a = argv[i]

        # --flag=value 形式（自包含）
        if a.startswith("--") and "=" in a:
            key, val = a.split("=", 1)
            if key == "--tokenizer":
                info.tokenizer = val
            elif key == "--host":
                info.host = val
            elif key == "--port":
                try:
                    info.port = int(val)
                except ValueError:
                    pass
            elif key == "--model" and info.model is None:
                info.model = val
            i += 1
            continue

        # --flag value 形式（需消费下一个参数）
        if a == "--tokenizer" and i + 1 < len(argv):
            info.tokenizer = argv[i + 1]
            i += 2
            continue
        if a == "--host" and i + 1 < len(argv):
            info.host = argv[i + 1]
            i += 2
            continue
        if a == "--port" and i + 1 < len(argv):
            try:
                info.port = int(argv[i + 1])
            except ValueError:
                pass
            i += 2
            continue
        if a == "--model" and i + 1 < len(argv):
            if info.model is None:
                info.model = argv[i + 1]
            i += 2
            continue

        # 其他 --flag
        if a.startswith("-"):
            if a in _VALUE_FLAGS and i + 1 < len(argv):
                i += 2  # 跳过 flag + 其值
            else:
                i += 1  # 布尔开关或未知 flag
            continue

        # 位置参数 → model（取首个）
        if info.model is None:
            info.model = a
        i += 1

    return info


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


def acquire_tokenizer_sync(explicit: Optional[str] = None) -> Any:
    """返回 tokenizer 对象。启动期同步调用。

    优先级：explicit(env) → --tokenizer(argv) → --model(argv) → HF 缓存扫描
    → 全部失败 raise（提示设置 VLLM_ANOMALY_TOKENIZER_MODEL）。
    """
    candidates: List[Tuple[str, str]] = []

    # 1. 显式 env
    if explicit:
        candidates.append((explicit, "explicit(env)"))

    # 2-3. argv: --tokenizer → --model
    info = parse_vllm_argv()
    if info is not None:
        if info.tokenizer:
            candidates.append((info.tokenizer, "--tokenizer(argv)"))
        if info.model:
            candidates.append((info.model, "serve <model>(argv)"))

    # 逐个尝试（去重）
    seen: set = set()
    for path, label in candidates:
        if path in seen:
            continue
        seen.add(path)
        try:
            logger.info("尝试加载 tokenizer (%s): %r", label, path)
            return _from_pretrained(path, local_files_only=True)
        except Exception as exc:
            logger.info("from_pretrained(%s %r) 失败: %s", label, path, exc)

    # 4. HF 缓存扫描兜底
    hints = [explicit]
    if info is not None and info.model:
        hints.append(info.model)
    for hint in hints:
        if not hint:
            continue
        for repo_id in _scan_hf_cache_candidates(hint):
            if repo_id in seen:
                continue
            seen.add(repo_id)
            logger.info("cache scan: %r -> %r", hint, repo_id)
            try:
                return _from_pretrained(repo_id, local_files_only=True)
            except Exception as exc:
                logger.warning("from_pretrained(cache %r) 失败: %s", repo_id, exc)

    raise RuntimeError(
        "tokenizer 加载失败: 无法从 env/argv/HF 缓存路径加载 tokenizer，"
        "请显式设置环境变量 VLLM_ANOMALY_TOKENIZER_MODEL 为 "
        "`vllm serve <model>` 的实际值"
        "（或 `--tokenizer` 的值，如使用独立 tokenizer）。"
    )


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
