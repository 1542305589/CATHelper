# KPI / Profiling Report

This directory keeps the report layout and builder, not a generated snapshot.

## Files

- `kpi_profiler_report.html.tpl` - HTML/CSS/JavaScript layout, including the detail modal.
- `build_kpi_profiler_report.py` - reads a data directory and writes a self-contained HTML.
- `../center/chart.umd.min.js` - Chart.js from the center console, inlined into the generated HTML.

## Data Directory

The builder expects these files:

```text
meta.json
kpi.json
anomalies.json
profiler.json
series.json
vllm.json
```

### `meta.json`

```json
{
  "business": "DeepSeekV4Flash_88",
  "start": "2026-11-16 09:00:00",
  "end": "2026-11-17 09:00:00",
  "devices": 16,
  "title": "DeepSeekV4Flash_88 硬件资源 / 业务 Profiling 检测结果汇总",
  "thresholds": {
    "power_time": 4,
    "power_space": 4,
    "util_time": 4,
    "util_space": 4,
    "freq_time": 3,
    "freq_space": 3
  },
  "profiler_thresholds": "cal 1.30; cpu 2.50; comm 1.30"
}
```

`title` and `profiler_thresholds` are optional.

### Other Files

- `kpi.json`: `{"runs": [...]}`
- `anomalies.json`: list of anomaly rows with `ts`, `device`, `metric`, `kind`, `value`, `median`, `mad`, `z`
- `profiler.json`: `{"cycles": [...]}`
- `series.json`: `{"power": {"0": [[ms, value], ...]}, "aicore_util": {...}, "aicore_freq": {...}}`
- `vllm.json`: `{"engines": {"0": [{"ts": ..., "ttft": ..., "tpot": ...}, ...]}}`

## Build

```bash
python3 build_kpi_profiler_report.py \
  --data-dir /path/to/report_data \
  --out /path/to/report.html
```

The generated HTML is self-contained. `../center/chart.umd.min.js` is inlined, and the report does not need external files at view time.

## Data Collection

The builder only renders. Data collection is separate:

- KPI anomalies and detection runs: center `kpi.db` / center APIs.
- Profiling cycles: center business history API.
- Hardware series: center KPI series API.
- TTFT/TPOT: center `/metrics?from=...&to=...` after persistence is enabled.

Do not store large generated HTML or chart data in this directory. Keep one data directory per report range.
