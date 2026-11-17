<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8">
<title>{{TITLE}}</title>
<style>
  :root { --border:#d9e2ec; --head:#e8eef8; --text:#1f2937; --muted:#6b7280; --accent:#1f4e78; }
  * { box-sizing:border-box; }
  body { margin:0; background:#fff; color:var(--text); font-family:-apple-system,BlinkMacSystemFont,"Segoe UI","PingFang SC","Microsoft YaHei",sans-serif; }
  .page { max-width:1600px; margin:0 auto; padding:22px 26px 30px; }
  h1 { font-size:23px; margin:0 0 6px; color:var(--accent); }
  .range { color:var(--muted); font-size:13px; margin-bottom:4px; }
  .meta { color:var(--muted); font-size:13px; margin-bottom:16px; }
  h2 { font-size:16px; margin:20px 0 9px; color:var(--accent); }
  table { width:100%; border-collapse:collapse; font-size:13px; }
  th,td { border:1px solid var(--border); padding:7px 9px; vertical-align:top; }
  th { background:var(--head); font-weight:600; white-space:nowrap; }
  td.num { text-align:center; white-space:nowrap; }
  tbody tr:nth-child(even) td { background:#f8fafc; }
  .summary-table th { height:42px; }
  #hwSummary tbody tr { cursor:pointer; }
  #hwSummary tbody tr:hover td { background:#eef4ff; }
  #hwSummary tbody tr.selected td { background:#dbeafe; }
  .top3 { white-space:normal; line-height:1.55; font-size:12px; min-width:520px; }
  .hint { color:var(--muted); font-size:12px; margin:8px 0 0; }
  .charts { display:grid; gap:12px; margin:14px 0 8px; }
  .charts.two { grid-template-columns:repeat(2,minmax(0,1fr)); }
  .charts.three { grid-template-columns:repeat(3,minmax(0,1fr)); }
  .chart-card { border:1px solid var(--border); border-radius:8px; padding:8px 10px 6px; background:#fbfdff; min-width:0; }
  .chart-title { font-size:13px; font-weight:600; color:var(--accent); margin:0 0 4px; }
  .chart-box { position:relative; height:240px; }
  .detail-table { font-size:12px; }
  .detail-table th,.detail-table td { padding:5px 7px; }
  .detail-modal { width:min(92vw,1200px); max-height:80vh; padding:0; border:1px solid var(--border); border-radius:10px; box-shadow:0 20px 60px rgba(15,23,42,.28); }
  .detail-modal::backdrop { background:rgba(15,23,42,.45); }
  .modal-head { position:sticky; top:0; z-index:1; display:flex; align-items:center; justify-content:space-between; gap:12px; padding:12px 16px; background:#fff; border-bottom:1px solid var(--border); }
  .modal-head h2 { margin:0; font-size:15px; }
  .modal-close { border:0; width:30px; height:30px; border-radius:6px; background:#eef2f7; color:#334155; cursor:pointer; font-size:20px; line-height:1; }
  .modal-close:hover { background:#dbe7f5; }
  .modal-body { max-height:calc(80vh - 56px); overflow:auto; padding:12px 16px 16px; }
  .detail-modal .notes { margin-top:12px; }
  .notes { margin-top:16px; padding:12px 14px; background:#fff8e6; border:1px solid #f1d58a; border-radius:8px; font-size:12px; line-height:1.7; color:#4b5563; }
  .notes b { color:#92400e; }
  @media print { body { -webkit-print-color-adjust:exact; print-color-adjust:exact; } .page { break-inside:avoid; } .detail-modal { display:none !important; } }
</style>
<script>{{CHART_LIB}}</script>
</head>
<body><div class="page">
<h1>DeepSeekV4Flash_88 硬件资源 / 业务 Profiling 检测结果汇总</h1>
<div class="range">{{RANGE}}</div>

<div class="charts two">
  <div class="chart-card"><div class="chart-title">TTFT（s）</div><div class="chart-box"><canvas id="chartTTFT"></canvas></div></div>
  <div class="chart-card"><div class="chart-title">TPOT（s）</div><div class="chart-box"><canvas id="chartTPOT"></canvas></div></div>
</div>
<div class="charts three">
  <div class="chart-card"><div class="chart-title">NPU功耗</div><div class="chart-box"><canvas id="chartPower"></canvas></div></div>
  <div class="chart-card"><div class="chart-title">AI Core利用率</div><div class="chart-box"><canvas id="chartUtil"></canvas></div></div>
  <div class="chart-card"><div class="chart-title">AI Core频率</div><div class="chart-box"><canvas id="chartFreq"></canvas></div></div>
</div>

<h2>1. 硬件资源指标异常详情</h2>
<div class="meta">{{META}}</div>
<table id="hwSummary" class="summary-table"><thead><tr><th>检测时间</th><th>总卡数</th><th>总异常卡数</th><th>检测阈值（z-score）</th><th>top3硬件资源指标异常</th><th>是否触发profiling检测</th></tr></thead><tbody>{{SUMMARY_ROWS}}</tbody></table>
<div class="notes">
<b>注：</b><br>
1) 硬件资源指标检测中 z-score 使用 median/MAD（中位数绝对偏差）作为基准，中位数和 MAD 对异常值更稳健，计算公式：z = (value - median) / σ，其中 σ = MAD / 0.6745；公式中的 0.6745 是正态分布下 MAD 与标准差的换算系数（MAD ≈ 0.6745σ），使得 median / MAD 基准下的 z-score 与普通 z-score 量纲一致。<br>
2) MAD 低于下限时用下限代替，防止 z 偏大。<br>
3) 时间维度表示同一设备相对自身历史参考窗口；空间维度表示同一检测轮次内设备之间的对比。<br>
4) top3硬件资源指标异常按 |z| 排序，最多列出 3 张设备；同一设备同一指标同时存在 time/space 异常时，只取 |z| 较大的一条；不足 3 张按实际数量列出，无异常写“无”。<br>
</div>
<dialog id="detailModal" class="detail-modal" aria-labelledby="detailTitle">
<div class="modal-head">
<h2 id="detailTitle">硬件资源指标异常明细（按设备合并）</h2>
<button type="button" class="modal-close" onclick="closeDetail()" aria-label="关闭">×</button>
</div>
<div class="modal-body">
<table class="detail-table"><thead><tr><th>设备</th><th>NPU功耗</th><th>AI Core利用率</th><th>AI Core频率</th></tr></thead><tbody id="detailBody"></tbody></table>
<div class="notes">
<b>注：</b><br>
1) 每个单元格中：value 为当前检测窗口的指标值；median 为参考窗口的中位数；MAD 为参考窗口的中位数绝对偏差；z 为稳健 z-score，|z| 越大表示偏离参考越明显，负值表示低于参考中位数；“—”表示该设备该指标无异常。
</div>
</div>
</dialog>

<h2>2. 业务Profiling指标检测【触发方式：① 硬件资源指标检测连续3次异常触发；② 计时器周期触发】</h2>
<table><thead><tr><th>检测时间</th><th>耗时</th><th>总卡数</th><th>触发原因</th><th>检测阈值</th><th>异常摘要</th></tr></thead><tbody>{{PROFILER_ROWS}}</tbody></table>
<div class="notes">
<b>注：</b><br>
1) cal 是慢计算检测，检测 KERNEL_AICORE 类型算子平均耗时。<br>
2) cpu 是慢CPU检测，检测算子 host 侧执行时间。<br>
3) comm 是慢通信检测，检测通信组集合通信带宽。
</div>
</div>
<script>

const hwSeries = {{HW_SERIES_JSON}};
const vllmSeries = {{VLLM_SERIES_JSON}};
const CHART_COLORS = ['#2563eb','#dc2626','#16a34a','#d97706','#9333ea','#0891b2','#db2777','#65a30d','#0f766e','#be123c','#4f46e5','#b45309','#15803d','#7e22ce','#0369a1','#a16207'];
function fmtTs(ms) {
  const d = new Date(ms);
  const p = n => String(n).padStart(2, '0');
  return `${p(d.getMonth()+1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`;
}

function buildVLLMChart(id, metric) {
  const byEngine = vllmSeries[metric] || {};
  const datasets = Object.keys(byEngine).sort().map((eng, i) => ({
    label: 'engine ' + eng,
    data: byEngine[eng].map(([x,y]) => ({x,y})),
    borderColor: CHART_COLORS[i % CHART_COLORS.length],
    backgroundColor: CHART_COLORS[i % CHART_COLORS.length],
    borderWidth: 1.2, pointRadius: 0, pointHitRadius: 4, tension: 0.15, spanGaps: true
  }));
  new Chart(document.getElementById(id), {
    type: 'line', data: {datasets}, options: {
      responsive: true, maintainAspectRatio: false, animation: false,
      parsing: false, normalized: true,
      interaction: {mode:'nearest', intersect:false},
      scales: {
        x: {type:'linear', ticks:{maxTicksLimit:6, callback:v=>fmtTs(v)}, grid:{display:false}},
        y: {beginAtZero:false, ticks:{maxTicksLimit:5, callback:v=>Number(v).toFixed(2)}}
      },
      plugins: {
        legend: {position:'bottom', labels:{boxWidth:8, font:{size:9}}},
        tooltip: {callbacks:{title:items=>items.length?fmtTs(items[0].parsed.x):'', label:ctx=>ctx.dataset.label+': '+ctx.parsed.y}}
      }
    }
  });
}

function buildHWChart(id, metric) {
  const byDev = hwSeries[metric] || {};
  const datasets = Object.keys(byDev).sort((a,b)=>Number(a)-Number(b)).map((dev,i) => ({
    label: 'dev' + dev,
    data: byDev[dev].map(([x,y]) => ({x,y})),
    borderColor: CHART_COLORS[i % CHART_COLORS.length],
    backgroundColor: CHART_COLORS[i % CHART_COLORS.length],
    borderWidth: 1.2, pointRadius: 0, pointHitRadius: 4, tension: 0.15, spanGaps: true
  }));
  new Chart(document.getElementById(id), {
    type: 'line', data: {datasets}, options: {
      responsive: true, maintainAspectRatio: false, animation: false,
      parsing: false, normalized: true,
      interaction: {mode:'nearest', intersect:false},
      scales: {
        x: {type:'linear', min:{{HW_START}}, max:{{HW_END}}, bounds:'data', grace:0, ticks:{maxTicksLimit:5, callback:v=>fmtTs(v)}, grid:{display:false}},
        y: {beginAtZero:false, ticks:{maxTicksLimit:5}}
      },
      plugins: {
        decimation: {enabled:true, algorithm:'lttb', samples:800},
        legend: {position:'bottom', labels:{boxWidth:8, font:{size:9}}},
        tooltip: {callbacks:{title:items=>items.length?fmtTs(items[0].parsed.x):'', label:ctx=>ctx.dataset.label+': '+ctx.parsed.y}}
      }
    }
  });
}
try {
  buildVLLMChart('chartTTFT','ttft');
  buildVLLMChart('chartTPOT','tpot');
  buildHWChart('chartPower','power');
  buildHWChart('chartUtil','aicore_util');
  buildHWChart('chartFreq','aicore_freq');
} catch (e) { console.error('chart init failed', e); }

const detailHTML = {{DETAIL_HTML_JSON}};
const summaryRows = Array.from(document.querySelectorAll('#hwSummary tbody tr'));
const detailModal = document.getElementById('detailModal');
function selectAnomaly(row, showModal = true) {
  summaryRows.forEach(r => r.classList.remove('selected'));
  row.classList.add('selected');
  const ts = row.getAttribute('data-ts');
  document.getElementById('detailTitle').textContent = `硬件资源指标异常明细（按设备合并，对应检测：${ts}）`;
  document.getElementById('detailBody').innerHTML = detailHTML[ts] || '<tr><td colspan="4">无异常</td></tr>';
  if (showModal) {
    if (!detailModal.open) detailModal.showModal();
    detailModal.querySelector('.modal-close').focus();
  }
}
function closeDetail() {
  if (detailModal.open) detailModal.close();
}
detailModal.addEventListener('click', e => {
  if (e.target === detailModal) closeDetail();
});
if (summaryRows.length) selectAnomaly(summaryRows[0], false);
</script>
</body></html>