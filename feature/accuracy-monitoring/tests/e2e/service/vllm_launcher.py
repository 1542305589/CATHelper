from __future__ import annotations

import hashlib
import json
import os
import subprocess
import sys
import time

from .process_manager import auto_select_port, terminate_process_tree, wait_for_url


_CORE_SCALARS: dict[str, str] = {
    "tensor_parallel_size": "tensor-parallel-size",
    "dtype": "dtype",
    "max_model_len": "max-model-len",
    "gpu_memory_utilization": "gpu-memory-utilization",
    "quantization": "quantization",
}

_SCALAR_OPTS: dict[str, str] = {
    "data_parallel_size": "data-parallel-size",
    "pipeline_parallel_size": "pipeline-parallel-size",
    "decode_context_parallel_size": "decode-context-parallel-size",
    "cp_kv_cache_interleave_size": "cp-kv-cache-interleave-size",
    "max_num_batched_tokens": "max-num-batched-tokens",
    "max_num_seqs": "max-num-seqs",
    "max_num_partial_prefills": "max-num-partial-prefills",
    "max_long_partial_prefills": "max-long-partial-prefills",
    "block_size": "block-size",
    "seed": "seed",
    "api_server_count": "api-server-count",
    "safetensors_load_strategy": "safetensors-load-strategy",
    "load_format": "load-format",
    "reasoning_parser": "reasoning-parser",
    "tool_call_parser": "tool-call-parser",
    "served_model_name": "served-model-name",
    "allowed_local_media_path": "allowed-local-media-path",
    "mm_processor_cache_gb": "mm-processor-cache-gb",
    "max_seq_len_to_capture": "max-seq-len-to-capture",
    "chat_template": "chat-template",
}

_JSON_OPTS: dict[str, str] = {
    "compilation_config": "compilation-config",
    "speculative_config": "speculative-config",
    "additional_config": "additional-config",
    "override_neuron_config": "override-neuron-config",
    "override_pooler_config": "override-pooler-config",
    "kv_transfer_config": "kv-transfer-config",
    "multimodal_config": "multimodal-config",
}

_BOOL_FLAGS: dict[str, str] = {
    "trust_remote_code": "trust-remote-code",
    "enable_expert_parallel": "enable-expert-parallel",
    "async_scheduling": "async-scheduling",
    "enforce_eager": "enforce-eager",
    "enable_lora": "enable-lora",
    "enable_prompt_adapter": "enable-prompt-adapter",
    "disable_log_stats": "disable-log-stats",
    "disable_log_requests": "disable-log-requests",
    "enable_auto_tool_choice": "enable-auto-tool-choice",
    "use_v2_block_manager": "use-v2-block-manager",
    "enable_chunked_prefill": "enable-chunked-prefill",
    "disable_fastapi_docs": "disable-fastapi-docs",
}

_BOOL_OPTIONAL: dict[str, str] = {
    "enable_prefix_caching": "enable-prefix-caching",
    "use_tqdm_status_bar": "use-tqdm-status-bar",
}


class VllmLauncher:
    def __init__(self, model_cfg: dict, workspace: str, report_dir: str, *, port: int | None = None):
        self.model_cfg = model_cfg
        self.workspace = workspace
        self.report_dir = report_dir
        self.host = "0.0.0.0"
        self.port = int(port) if port is not None else auto_select_port()
        self.proc: subprocess.Popen | None = None
        self._injector = None
        self._log_file = None
        self._log_path = ""

    @staticmethod
    def _serialize_value(value) -> str:
        if isinstance(value, bool):
            return "true" if value else "false"
        if isinstance(value, (dict, list)):
            return json.dumps(value, separators=(",", ":"), ensure_ascii=False)
        return str(value)

    @staticmethod
    def _normalize_flag(key: str) -> str:
        return key.replace("_", "-")

    def build_cmd(self, *, middleware: bool, env_overrides: dict) -> list[str]:
        cfg = self.model_cfg
        cmd: list[str] = [
            "vllm", "serve", cfg["model_path"],
            "--host", self.host,
            "--port", str(self.port),
        ]

        for key, flag in _CORE_SCALARS.items():
            if cfg.get(key) is not None:
                cmd += [f"--{flag}", self._serialize_value(cfg[key])]

        served = cfg.get("served_name") or cfg.get("served_model_name")
        if served is None:
            served = os.path.basename(str(cfg["model_path"]).rstrip("/\\"))
        cmd += ["--served-model-name", str(served)]

        for key, flag in _SCALAR_OPTS.items():
            if cfg.get(key) is not None:
                cmd += [f"--{flag}", self._serialize_value(cfg[key])]

        for key, flag in _JSON_OPTS.items():
            val = cfg.get(key)
            if val is None:
                continue
            serialized = val if isinstance(val, str) else json.dumps(
                val, separators=(",", ":"), ensure_ascii=False
            )
            cmd += [f"--{flag}", serialized]

        for key, flag in _BOOL_FLAGS.items():
            if cfg.get(key):
                cmd += [f"--{flag}"]

        for key, flag in _BOOL_OPTIONAL.items():
            if cfg.get(key) is True:
                cmd += [f"--{flag}"]
            elif cfg.get(key) is False:
                cmd += [f"--no-{flag}"]

        cmd += self._build_extra_args()

        if middleware:
            cmd += ["--middleware", "anomaly_middleware.AnomalyMiddleware"]
        return cmd

    def _build_extra_args(self) -> list[str]:
        extra = self.model_cfg.get("extra_vllm_args")
        if not extra:
            return []
        out: list[str] = []
        if isinstance(extra, dict):
            for key, value in extra.items():
                flag = self._normalize_flag(str(key))
                if value is None or value is False:
                    continue
                if value is True:
                    out.append(f"--{flag}")
                else:
                    out += [f"--{flag}", self._serialize_value(value)]
        elif isinstance(extra, list):
            for el in extra:
                if isinstance(el, str):
                    out.append(el)
                elif isinstance(el, dict):
                    for key, value in el.items():
                        flag = self._normalize_flag(str(key))
                        if value is None or value is False:
                            continue
                        if value is True:
                            out.append(f"--{flag}")
                        else:
                            out += [f"--{flag}", self._serialize_value(value)]
                elif isinstance(el, (list, tuple)):
                    flag = self._normalize_flag(str(el[0]))
                    if len(el) == 1 or el[1] is None or el[1] is False:
                        if len(el) == 1:
                            out.append(f"--{flag}")
                        continue
                    if el[1] is True:
                        out.append(f"--{flag}")
                    else:
                        out += [f"--{flag}", self._serialize_value(el[1])]
        return out

    def _session_name(
        self, middleware: bool, env_overrides: dict, with_injector: bool
    ) -> str:
        parts = ["mw_on" if middleware else "mw_off"]
        if with_injector:
            parts.append("injector")
        if env_overrides:
            digest = hashlib.md5(
                json.dumps(env_overrides, sort_keys=True, default=str).encode()
            ).hexdigest()[:8]
            parts.append(digest)
        return "_".join(parts)

    def start(
        self,
        *,
        middleware: bool = True,
        env_overrides: dict | None = None,
        with_injector: bool = False,
        expect_fail: bool = False,
        fail_fast_timeout: float = 30.0,
        injector_port: int = 9999,
    ) -> None:
        env_overrides = env_overrides or {}
        env = os.environ.copy()
        env["VLLM_WORKER_MULTIPROC_METHOD"] = "spawn"
        env.update({k: str(v) for k, v in env_overrides.items()})

        existing_pp = env.get("PYTHONPATH", "")
        env["PYTHONPATH"] = self.workspace + (
            os.pathsep + existing_pp if existing_pp else ""
        )

        if with_injector:
            env["VLLM_ANOMALY_E2E_INJECTOR"] = f"http://127.0.0.1:{injector_port}"
            runtime_dir = os.path.join(
                os.path.dirname(os.path.abspath(__file__)), "_injector_runtime"
            )
            env["PYTHONPATH"] = runtime_dir + os.pathsep + env["PYTHONPATH"]

        cmd = self.build_cmd(middleware=middleware, env_overrides=env_overrides)
        session_name = self._session_name(middleware, env_overrides, with_injector)
        log_dir = os.path.join(self.report_dir, "logs")
        os.makedirs(log_dir, exist_ok=True)
        self._log_path = os.path.join(log_dir, f"{session_name}.log")
        self._log_file = open(self._log_path, "wb")

        popen_kwargs: dict = {
            "env": env,
            "stdout": self._log_file,
            "stderr": subprocess.STDOUT,
        }
        if sys.platform != "win32":
            popen_kwargs["start_new_session"] = True

        self.proc = subprocess.Popen(cmd, **popen_kwargs)

        if expect_fail:
            try:
                self._wait_for_fail(timeout=fail_fast_timeout)
            except Exception:
                # 进程未按预期退出：必须清理，否则残留进程占用 NPU 显存，
                # 会拖垮后续用例（OOM: free memory < gpu_memory_utilization）。
                self.stop()
                raise
        else:
            self._wait_for_health(
                timeout=float(self.model_cfg.get("startup_timeout_sec", 600))
            )

    def _wait_for_health(self, timeout: float) -> None:
        health_url = f"http://127.0.0.1:{self.port}/health"
        deadline = time.time() + timeout
        while time.time() < deadline:
            if self.proc is not None and self.proc.poll() is not None:
                raise RuntimeError(
                    f"vLLM process exited unexpectedly (code="
                    f"{self.proc.returncode}); see log: {self._log_path}"
                )
            if wait_for_url(health_url, timeout=2.0, interval=1.0):
                return
        raise TimeoutError(
            f"vLLM health check timed out after {timeout}s; see log: {self._log_path}"
        )

    def _wait_for_fail(self, timeout: float) -> None:
        deadline = time.time() + timeout
        while time.time() < deadline:
            rc = self.proc.poll() if self.proc is not None else None
            if rc is not None:
                if rc == 0:
                    raise AssertionError(
                        f"vLLM exited with code 0 but failure was expected; "
                        f"see log: {self._log_path}"
                    )
                return
            time.sleep(1.0)
        raise AssertionError(
            f"vLLM did not exit within {timeout}s but failure was expected; "
            f"see log: {self._log_path}"
        )

    @property
    def url(self) -> str:
        return f"http://127.0.0.1:{self.port}"

    @property
    def is_running(self) -> bool:
        return self.proc is not None and self.proc.poll() is None

    @property
    def pid(self) -> int | None:
        return self.proc.pid if self.proc is not None else None

    @property
    def log_path(self) -> str:
        return self._log_path

    def stop(self) -> None:
        if self.proc is not None:
            terminate_process_tree(self.proc.pid)
            try:
                self.proc.wait(timeout=10)
            except Exception:
                pass
            self.proc = None
        if self._log_file is not None:
            try:
                self._log_file.close()
            except Exception:
                pass
            self._log_file = None
