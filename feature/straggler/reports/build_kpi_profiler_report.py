#!/usr/bin/env python3
"""Build the KPI / Profiling report from a data directory and HTML template."""

import argparse
import collections
import datetime
import html
import json
from pathlib import Path

try:
    from zoneinfo import ZoneInfo
    LOCAL_TZ = ZoneInfo("Asia/Shanghai")
except Exception:
    LOCAL_TZ = None


def parse_local(value):
    dt = datetime.datetime.strptime(value, "%Y-%m-%d %H:%M:%S")
    if LOCAL_TZ is not None:
        return dt.replace(tzinfo=LOCAL_TZ)
    return dt


def parse_iso(value):
    return datetime.datetime.fromisoformat(value).replace(tzinfo=None)


def esc(value):
    return html.escape(str(value))


def load_json(data_dir, name, default):
    path = data_dir / name
    if not path.exists():
        return default
    return json.loads(path.read_text())


def top3_text(anomalies, by_ts):
    labels = {"power": "NPU功耗", "aicore_util": "AI Core利用率", "aicore_freq": "AI Core频率"}
    lines = []
    for metric in ("power", "aicore_util", "aicore_freq"):
        best = {}
        for item in by_ts.get(anomalies, []):
            if item["metric"] != metric:
                continue
            device = item["device"]
            if device not in best or abs(item["z"]) > abs(best[device]["z"]):
                best[device] = item
        values = sorted(best.values(), key=lambda item: abs(item["z"]), reverse=True)[:3]
        label = labels[metric]
        if not values:
            lines.append(f"<b>{label}：</b> 无")
        else:
            items = [f'worker:dev{item["device"]}(z={item["z"]:.2f})' for item in values]
            lines.append(f"<b>{label}：</b> " + ", ".join(items))
    return "<br>".join(lines)


def detail_html(ts, by_ts):
    devmap = collections.defaultdict(lambda: collections.defaultdict(list))
    for item in by_ts.get(ts, []):
        devmap[item["device"]][item["metric"]].append(item)
    rows = []
    for device in sorted(devmap):
        cells = []
        for metric in ("power", "aicore_util", "aicore_freq"):
            items = devmap[device].get(metric, [])
            if not items:
                cells.append("—")
            else:
                items = sorted(items, key=lambda item: (0 if item["kind"] == "time" else 1, -abs(item["z"])))
                cells.append("<br>".join(
                    f'{"时间维度" if item["kind"] == "time" else "空间维度"}: '
                    f'value={item["value"]:.2f}, median={item["median"]:.2f}, '
                    f'MAD={item["mad"]:.2f}, z={item["z"]:.2f}'
                    for item in items
                ))
        rows.append(
            f'<tr><td class="num">{device}</td><td>{cells[0]}</td>'
            f'<td>{cells[1]}</td><td>{cells[2]}</td></tr>'
        )
    return "".join(rows)


def profiler_summary(cycle):
    summary = cycle.get("summary") or {}
    if not any(summary.values()):
        return "无异常"
    parts = []
    for key, label in (("cal", "cal"), ("comm", "comm"), ("cpu", "cpu"), ("npu_bubble", "bubble")):
        if summary.get(key, 0):
            parts.append(f"{label}:{summary[key]}")
    return ";".join(parts)


def threshold_text(meta):
    th = meta.get("thresholds") or {}
    power_time = th.get("power_time", 4)
    power_space = th.get("power_space", 4)
    util_time = th.get("util_time", 4)
    util_space = th.get("util_space", 4)
    freq_time = th.get("freq_time", 3)
    freq_space = th.get("freq_space", 3)
    return (
        f"NPU功耗：时间阈值 {power_time:g} / 空间阈值 {power_space:g}<br>"
        f"AI Core利用率：时间阈值 {util_time:g} / 空间阈值 {util_space:g}<br>"
        f"AI Core频率：时间阈值 {freq_time:g} / 空间阈值 {freq_space:g}"
    )


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--data-dir", required=True, type=Path)
    parser.add_argument("--out", type=Path, default=Path("kpi_profiler_report.html"))
    parser.add_argument("--template", type=Path, default=Path(__file__).with_name("kpi_profiler_report.html.tpl"))
    parser.add_argument("--chart-js", type=Path, default=Path(__file__).resolve().parent.parent / "center" / "chart.umd.min.js")
    args = parser.parse_args()

    meta = load_json(args.data_dir, "meta.json", {})
    kpi = load_json(args.data_dir, "kpi.json", {})
    anomalies = load_json(args.data_dir, "anomalies.json", [])
    profiler = load_json(args.data_dir, "profiler.json", {"cycles": []})
    series = load_json(args.data_dir, "series.json", {})
    vllm_raw = load_json(args.data_dir, "vllm.json", {"engines": {}})

    business = meta.get("business", "")
    start_text = meta["start"]
    end_text = meta["end"]
    start = parse_local(start_text)
    end = parse_local(end_text)
    start_naive = start.replace(tzinfo=None)
    end_naive = end.replace(tzinfo=None)
    start_sec = int(start.timestamp())
    end_sec = int(end.timestamp())
    start_ms = start_sec * 1000
    end_ms = end_sec * 1000
    devices = int(meta.get("devices", 16))
    title = meta.get("title") or f"{business} 硬件资源 / 业务 Profiling 检测结果汇总"
    profiler_thresholds = meta.get("profiler_thresholds", "cal 1.30; cpu 2.50; comm 1.30")

    def anomaly_ts_sec(item):
        value = item["ts"]
        if isinstance(value, str):
            return int(parse_local(value).timestamp())
        return int(value)

    anomalies = [item for item in anomalies if start_sec <= anomaly_ts_sec(item) <= end_sec]
    for item in anomalies:
        value = item["ts"]
        if isinstance(value, str):
            item["ts"] = value
        else:
            item["ts"] = datetime.datetime.fromtimestamp(int(value), LOCAL_TZ).strftime("%Y-%m-%d %H:%M:%S")

    by_ts = collections.defaultdict(list)
    for item in anomalies:
        by_ts[item["ts"]].append(item)

    def parse_ts(value):
        return datetime.datetime.strptime(value, "%Y-%m-%d %H:%M:%S")

    triggered_ts = set()
    cycles = profiler.get("cycles", [])
    for cycle in cycles:
        if cycle.get("trigger_reason") != "kpi":
            continue
        started = parse_iso(cycle["started_at"])
        candidates = [ts for ts in by_ts if 0 <= (started - parse_ts(ts)).total_seconds() <= 300]
        if candidates:
            triggered_ts.add(max(candidates, key=parse_ts))

    summary = []
    for ts in sorted(by_ts, reverse=True):
        items = by_ts[ts]
        summary.append({
            "ts": ts,
            "devices": devices,
            "flagged": len({item["device"] for item in items}),
            "top3": top3_text(ts, by_ts),
            "triggered": ts in triggered_ts,
        })

    summary_trs = "".join(
        f'<tr class="{"selected" if i == 0 else ""}" data-ts="{esc(item["ts"])}" '
        f'onclick="selectAnomaly(this)"><td>{esc(item["ts"])}</td>'
        f'<td class="num">{item["devices"]}</td><td class="num">{item["flagged"]}</td>'
        f'<td>{threshold_text(meta)}</td><td class="top3">{item["top3"]}</td>'
        f'<td class="num">{"是" if item["triggered"] else "否"}</td></tr>'
        for i, item in enumerate(summary)
    )

    reason_labels = {
        "timer": "② 计时器周期触发",
        "kpi": "① 硬件资源指标检测连续3次异常触发",
        "manual": "manual",
    }
    profiler_rows = []
    for cycle in cycles:
        started = datetime.datetime.fromisoformat(cycle["started_at"]).replace(tzinfo=None)
        if not (start_naive <= started <= end_naive):
            continue
        reason = reason_labels.get(cycle.get("trigger_reason", ""), cycle.get("trigger_reason", ""))
        profiler_rows.append(
            f'<tr><td>{started.strftime("%Y-%m-%d %H:%M:%S")}</td>'
            f'<td class="num">{cycle.get("duration_ms", 0) / 1000:.1f}s</td>'
            f'<td class="num">{esc(cycle.get("world_size", ""))}</td>'
            f'<td>{esc(reason)}</td><td>{esc(profiler_thresholds)}</td>'
            f'<td>{esc(profiler_summary(cycle))}</td></tr>'
        )
    profiler_rows.sort()

    hw_series = {
        metric: {device: points[::10] for device, points in devices_map.items()}
        for metric, devices_map in series.items()
    }

    vllm_series = {}
    for metric in ("ttft", "tpot"):
        vllm_series[metric] = {}
        for engine, rows in vllm_raw.get("engines", {}).items():
            buckets = collections.defaultdict(list)
            for row in rows:
                if int(row["ts"]) > end_sec:
                    continue
                bucket = int(row["ts"]) // 60 * 60
                buckets[bucket].append(row[metric])
            points = [
                [bucket * 1000, round(sum(values) / len(values), 4)]
                for bucket, values in sorted(buckets.items())
            ]
            vllm_series[metric][engine] = points[::10]

    detail_map = {ts: detail_html(ts, by_ts) for ts in by_ts}
    template = args.template.read_text()
    replacements = {
        "{{TITLE}}": esc(title),
        "{{RANGE}}": f"统计范围：{start_text} ~ {end_text} CST",
        "{{META}}": f"检测次数：{len(kpi.get('runs', []))}；异常数：{len(summary)}",
        "{{SUMMARY_ROWS}}": summary_trs,
        "{{PROFILER_ROWS}}": "".join(profiler_rows),
        "{{DETAIL_HTML_JSON}}": json.dumps(detail_map, ensure_ascii=False, separators=(",", ":")),
        "{{HW_SERIES_JSON}}": json.dumps(hw_series, ensure_ascii=False, separators=(",", ":")),
        "{{VLLM_SERIES_JSON}}": json.dumps(vllm_series, ensure_ascii=False, separators=(",", ":")),
        "{{HW_START}}": str(start_ms),
        "{{HW_END}}": str(end_ms),
        "{{CHART_LIB}}": args.chart_js.read_text(),
    }
    for key, value in replacements.items():
        template = template.replace(key, value)

    args.out.parent.mkdir(parents=True, exist_ok=True)
    args.out.write_text(template)
    print(args.out)
    print(f"summary {len(summary)} profiler {len(profiler_rows)}")


if __name__ == "__main__":
    main()
