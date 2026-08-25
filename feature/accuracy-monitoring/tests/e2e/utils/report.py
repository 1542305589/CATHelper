from __future__ import annotations

import os

from jinja2 import Environment, FileSystemLoader


def render_report(results: list, meta: dict, template_dir: str, output_dir: str) -> str:
    env = Environment(loader=FileSystemLoader(template_dir), autoescape=False)
    template = env.get_template("report_template.md")
    meta.setdefault("run_id", "")
    meta.setdefault("tier", "")
    meta.setdefault("models", [])
    meta.setdefault("pr_number", None)
    meta.setdefault("pr_sha", "")
    meta.setdefault("start_time", "")
    meta.setdefault("duration", "")
    meta.setdefault("total", len(results))
    meta.setdefault("passed", 0)
    meta.setdefault("failed", 0)
    meta.setdefault("skipped", 0)
    content = template.render(results=results, **meta)
    os.makedirs(output_dir, exist_ok=True)
    out_path = os.path.join(output_dir, "report.md")
    with open(out_path, "w", encoding="utf-8") as f:
        f.write(content)
    return out_path
