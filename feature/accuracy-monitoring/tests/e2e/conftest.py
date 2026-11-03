from __future__ import annotations

import json
import os
import shutil
import time

import pytest
import yaml

from .service.injector_server import InjectorServer
from .service.vllm_launcher import VllmLauncher

_CASE_REGISTRY = os.path.join(os.path.dirname(os.path.abspath(__file__)), "cases_registry.yaml")
_FAILURE_FILE = "failures.log"


def pytest_addoption(parser):
    parser.addoption(
        "--model-yaml",
        default=os.path.join(os.path.dirname(__file__), "models", "qwen3-0.6b.yaml"),
        help="Path to model YAML config file",
    )
    parser.addoption(
        "--report-dir",
        default=None,
        help="Directory for test reports and diagnostics",
    )
    parser.addoption(
        "--port",
        type=int,
        default=None,
        help="Fixed port for the vLLM service (default: auto-select a free port; "
        "env VLLM_E2E_PORT overrides when --port is absent)",
    )


def pytest_configure(config):
    for marker, desc in [
        ("P0", "priority P0"),
        ("P1", "priority P1"),
        ("P2", "priority P2"),
        ("lightweight", "lightweight tier (PR-triggered)"),
        ("full", "full tier"),
        ("nightly", "nightly tier"),
        ("inject", "requires sidecar injection"),
        ("fail_fast", "expects service startup failure"),
    ]:
        config.addinivalue_line("markers", f"{marker}: {desc}")


# ---------------------------------------------------------------------------
# Test ordering (design §3.2 / §8.4)
# ---------------------------------------------------------------------------

def pytest_collection_modifyitems(items):
    """Reorder collected tests by cases_registry.yaml `order` (grouped by
    service signature). Tests not listed in the registry keep their original
    relative order and are appended at the end."""
    try:
        with open(_CASE_REGISTRY, encoding="utf-8") as f:
            registry = yaml.safe_load(f) or {}
    except Exception:
        return

    order_of = {}
    for name, meta in registry.items():
        if isinstance(meta, dict) and "order" in meta:
            order_of[name] = meta["order"]

    items.sort(key=lambda it: order_of.get(_case_name(it), float("inf")))


def _case_name(item) -> str:
    name = getattr(item, "name", "")
    return name.split("[")[0]


# ---------------------------------------------------------------------------
# Session-scoped fixtures
# ---------------------------------------------------------------------------

@pytest.fixture(scope="session")
def model_yaml(request) -> dict:
    path = request.config.getoption("--model-yaml")
    with open(path) as f:
        return yaml.safe_load(f)


@pytest.fixture(scope="session")
def workspace() -> str:
    return os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))


@pytest.fixture(scope="session")
def report_dir(request) -> str:
    d = request.config.getoption("--report-dir")
    if d is None:
        run_id = time.strftime("%Y%m%d_%H%M%S")
        d = os.path.join(os.path.dirname(os.path.abspath(__file__)), "reports", run_id)
    os.makedirs(d, exist_ok=True)
    os.makedirs(os.path.join(d, "logs"), exist_ok=True)
    os.makedirs(os.path.join(d, "diagnostics"), exist_ok=True)
    return d


@pytest.fixture(scope="session")
def baseline_store(report_dir):
    from .utils.baseline import BaselineStore

    return BaselineStore.for_run(report_dir)


@pytest.fixture(scope="session")
def injector() -> InjectorServer:
    server = InjectorServer(host="127.0.0.1", port=9999)
    server.start()
    yield server
    server.stop()


@pytest.fixture(scope="session")
def service_port(request) -> int | None:
    opt = request.config.getoption("--port")
    if opt is not None:
        return int(opt)
    env = os.environ.get("VLLM_E2E_PORT")
    if env:
        try:
            return int(env)
        except ValueError:
            raise ValueError(f"VLLM_E2E_PORT must be an integer, got: {env!r}")
    return None


@pytest.fixture(scope="session")
def vllm_service_factory(injector, workspace, report_dir, service_port):
    """Single-active vLLM service (design §8.4). Same signature is reused;
    a new signature stops the previous instance before starting a new one."""
    active = {"key": None, "launcher": None}

    def get(signature: dict) -> VllmLauncher:
        key = json.dumps(signature, sort_keys=True, default=str)
        if active["key"] == key:
            return active["launcher"]

        if active["launcher"] is not None:  # swap out: stop old first
            try:
                active["launcher"].stop()
            except Exception:
                pass
            active["key"] = None
            active["launcher"] = None

        cfg = signature["model"]
        ws = signature.get("workspace", workspace)
        launcher = VllmLauncher(cfg, ws, report_dir, port=service_port)
        launcher.start(
            middleware=signature.get("middleware", True),
            env_overrides=signature.get("env", {}),
            with_injector=signature.get("with_injector", False),
            expect_fail=signature.get("expect_fail", False),
            fail_fast_timeout=signature.get("fail_fast_timeout", 30.0),
            injector_port=injector.port,
        )
        if not signature.get("expect_fail", False):
            active["key"] = key
            active["launcher"] = launcher
        return launcher

    yield get

    if active["launcher"] is not None:
        try:
            active["launcher"].stop()
        except Exception:
            pass


# ---------------------------------------------------------------------------
# Function-scoped fixtures
# ---------------------------------------------------------------------------

@pytest.fixture
def vllm_service_b(vllm_service_factory, model_yaml):
    return vllm_service_factory(
        {"model": model_yaml, "middleware": True, "env": {}, "with_injector": True}
    )


@pytest.fixture
def vllm_service_no_mw(vllm_service_factory, model_yaml):
    return vllm_service_factory(
        {"model": model_yaml, "middleware": False, "env": {}, "with_injector": False}
    )


@pytest.fixture
async def http_client(vllm_service_b, served_name):
    from .client.http_client import HttpClient

    client = HttpClient(vllm_service_b.url, served_name)
    yield client
    await client.aclose()


@pytest.fixture
def metrics_client(vllm_service_b):
    from .metrics.prometheus_client import PrometheusClient

    return PrometheusClient(f"{vllm_service_b.url}/anomaly/metrics")


@pytest.fixture
def served_name(model_yaml) -> str:
    return model_yaml["served_name"]


@pytest.fixture
def anomaly_data(model_yaml) -> dict:
    try:
        from .data.anomaly_data_builder import build_all

        return build_all(model_yaml["model_path"], model_yaml["served_name"])
    except Exception:
        pass

    from .data.anomaly_data_builder import (
        build_detection_error,
        build_garbled,
        build_inf_logprob,
        build_nan_value,
        build_rare_character,
        build_repetition,
    )

    import random

    random.seed(42)

    by_cat: dict[str, list[int]] = {"other": list(range(1, 50))}
    tk2cat: dict[str, str] = {str(i): "other" for i in range(1, 50)}
    for i in range(50, 100):
        tk2cat[str(i)] = "chinese_cjk"
        by_cat.setdefault("chinese_cjk", []).append(i)
    for i in range(100, 150):
        tk2cat[str(i)] = "numbers"
        by_cat.setdefault("numbers", []).append(i)

    rare = build_rare_character(tk2cat, 150)
    garbled = build_garbled(tk2cat, 150)
    repetition = build_repetition(tk2cat, 150)

    return {
        "rare_character": rare,
        "garbled": garbled,
        "repetition": repetition,
        "nan_value": build_nan_value(),
        "inf_logprob": build_inf_logprob(),
        "detection_error": build_detection_error(),
    }


# ---------------------------------------------------------------------------
# Autouse cleanup
# ---------------------------------------------------------------------------

@pytest.fixture(autouse=True)
def _clear_injector_after_test(injector):
    yield
    injector.clear()


# ---------------------------------------------------------------------------
# Failure diagnostics + stop-on-failure (design §9)
# ---------------------------------------------------------------------------

@pytest.hookimpl(hookwrapper=True)
def pytest_runtest_makereport(item, call):
    outcome = yield
    report = outcome.get_result()
    if report.when == "call" and report.failed:
        _collect_diagnostics(item, report)
        _write_failure_summary(item, report)
        # TODO(debug): temporarily disabled stop-on-failure to surface all failures
        # item.session.shouldstop = (
        #     f"E2E run aborted: {item.name} failed "
        #     f"(error written to {os.path.basename(_FAILURE_FILE)})"
        # )


def _failure_file_path(report_dir) -> str:
    return os.path.join(report_dir, "failures.log")


def _write_failure_summary(item, report):
    try:
        report_dir = item.config.getoption("--report-dir")
        if report_dir is None:
            return
        os.makedirs(report_dir, exist_ok=True)
        with open(_failure_file_path(report_dir), "a", encoding="utf-8") as f:
            f.write(f"=== {item.name} ===\n")
            f.write(str(report.longrepr) if report.longrepr else "")
            f.write("\n\n")
    except Exception:
        pass


def _collect_diagnostics(item, report):
    try:
        report_dir = item.config.getoption("--report-dir")
        if report_dir is None:
            return

        diag_dir = os.path.join(report_dir, "diagnostics", item.name)
        os.makedirs(diag_dir, exist_ok=True)

        with open(os.path.join(diag_dir, "traceback.txt"), "w", encoding="utf-8") as f:
            f.write(str(report.longrepr) if report.longrepr else "")

        injector = item.funcargs.get("injector")
        if injector is not None:
            try:
                state = injector.get_state()
                with open(os.path.join(diag_dir, "injector_state.json"), "w") as f:
                    json.dump(state, f, indent=2, default=str)
            except Exception:
                pass

        metrics_client = item.funcargs.get("metrics_client")
        if metrics_client is not None:
            try:
                snapshot = metrics_client.snapshot()
                with open(os.path.join(diag_dir, "metrics.txt"), "w", encoding="utf-8") as f:
                    f.write(snapshot)
            except Exception:
                pass

        vllm_service = item.funcargs.get("vllm_service_b")
        if vllm_service is not None:
            try:
                with open(vllm_service.log_path, "r", encoding="utf-8", errors="replace") as src:
                    lines = src.readlines()[-200:]
                with open(os.path.join(diag_dir, "stderr.log"), "w", encoding="utf-8") as f:
                    f.writelines(lines)
            except Exception:
                pass

        http_client = item.funcargs.get("http_client")
        if http_client is not None:
            try:
                with open(os.path.join(diag_dir, "request.json"), "w") as f:
                    json.dump(http_client.last_request, f, indent=2, default=str)
                with open(os.path.join(diag_dir, "response.json"), "w") as f:
                    json.dump(http_client.last_response, f, indent=2, default=str)
            except Exception:
                pass
    except Exception:
        pass
