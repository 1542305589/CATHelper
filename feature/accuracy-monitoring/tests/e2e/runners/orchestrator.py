from __future__ import annotations

import os
import sys
import time
import subprocess
import xml.etree.ElementTree as ET

_E2E_ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))
if _E2E_ROOT not in sys.path:
    sys.path.insert(0, _E2E_ROOT)
_REPO_ROOT = os.path.abspath(os.path.join(_E2E_ROOT, "..", ".."))

from utils.report import render_report
from .code_fetch import fetch_code
from .env_check import check_env, check_python_deps
from .env_install import install_deps


class Orchestrator:
    def __init__(self, args):
        self.args = args
        self.tier = args.tier
        self.models = args.models
        self.pr_number = args.pr_number
        self.pr_sha = args.pr_sha
        self.pr_repo = args.pr_repo
        self.report_dir = args.report_dir
        self.workspace = args.workspace
        self.local = getattr(args, "local", False) or not args.pr_number

    def run(self) -> int:
        check_python_deps()
        run_id = time.strftime("%Y%m%d-%H%M%S")
        report_dir = self.report_dir or os.path.join(
            "tests", "e2e", "reports", run_id
        )
        os.makedirs(report_dir, exist_ok=True)
        if not self.local:
            model_cfg = self._load_model_cfg()
            check_env(model_cfg)
            workspace = self.workspace or os.path.join(
                "/vllm-workspace", "accuracy-monitoring"
            )
            fetch_code(
                workspace, self.pr_number, self.pr_sha, self.pr_repo,
                local=False,
            )
            install_deps(workspace)
            os.chdir(workspace)
        else:
            os.chdir(_REPO_ROOT)
        return self.run_pytest(report_dir, run_id)

    def run_pytest(self, report_dir: str, run_id: str) -> int:
        start = time.time()
        rc = 0
        for model_yaml in self._resolve_model_yamls():
            model_name = os.path.splitext(os.path.basename(model_yaml))[0]
            junit_path = os.path.join(
                report_dir, f"junit-{run_id}-{model_name}.xml"
            )
            cmd = [
                sys.executable, "-m", "pytest", "tests/e2e/tests/",
                "-m", self.tier,
                f"--model-yaml={model_yaml}",
                f"--report-dir={report_dir}",
                f"--junit-xml={junit_path}",
                "-p", "tests.e2e.markers",
                "--tb=short",
            ]
            result = subprocess.run(cmd)
            if result.returncode != 0:
                rc = 1
        duration = time.time() - start
        self.render_report(report_dir, run_id, duration)
        return rc

    def render_report(self, report_dir: str, run_id: str, duration: float) -> None:
        results = self._parse_junit(report_dir)
        meta = {
            "run_id": run_id,
            "tier": self.tier,
            "models": [
                m.strip() for m in self.models.split(",") if m.strip()
            ],
            "pr_number": self.pr_number,
            "pr_sha": self.pr_sha or "",
            "start_time": time.strftime("%Y-%m-%d %H:%M:%S"),
            "duration": f"{duration:.1f}s",
            "total": len(results),
            "passed": sum(1 for r in results if r["status"] == "passed"),
            "failed": sum(1 for r in results if r["status"] == "failed"),
            "skipped": sum(1 for r in results if r["status"] == "skipped"),
        }
        template_dir = os.path.abspath(
            os.path.join(os.path.dirname(__file__), "..", "utils")
        )
        render_report(results, meta, template_dir, report_dir)

    @staticmethod
    def _parse_junit(report_dir: str) -> list:
        results = []
        if not os.path.isdir(report_dir):
            return results
        for name in os.listdir(report_dir):
            if not (name.startswith("junit-") and name.endswith(".xml")):
                continue
            tree = ET.parse(os.path.join(report_dir, name))
            root = tree.getroot()
            for tc in root.iter("testcase"):
                status = "passed"
                fail_summary = ""
                fail = tc.find("failure")
                err = tc.find("error")
                if fail is not None:
                    status = "failed"
                    fail_summary = (fail.get("message") or fail.text or "")[:200]
                elif err is not None:
                    status = "failed"
                    fail_summary = (err.get("message") or err.text or "")[:200]
                elif tc.find("skipped") is not None:
                    status = "skipped"
                    skip = tc.find("skipped")
                    fail_summary = (skip.get("message") or "")[:200]
                results.append({
                    "funcname": tc.get("name", ""),
                    "tc_id": tc.get("classname", ""),
                    "priority": "",
                    "model": "",
                    "status": status,
                    "duration": tc.get("time", "0"),
                    "fail_summary": fail_summary,
                })
        return results

    def _resolve_model_yamls(self) -> list:
        if getattr(self.args, "model_yaml", None):
            return [self.args.model_yaml]
        models_dir = os.path.join("tests", "e2e", "models")
        return [
            os.path.join(models_dir, m.strip())
            for m in self.models.split(",")
            if m.strip()
        ]

    def _load_model_cfg(self) -> dict:
        import yaml
        path = self._resolve_model_yamls()[0]
        with open(path) as f:
            return yaml.safe_load(f)
