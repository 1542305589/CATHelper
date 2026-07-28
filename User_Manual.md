# CATHelper 使用手册 (User Manual)

> 本文档说明 CATHelper 的构建、安装、配置，以及底座（CATMonitor）与上层特性（EEP、Straggler）的协同用法。
> 功能概览见 [SPEC.md](SPEC.md)，版本记录见 [Release_Notes.md](Release_Notes.md)。
> 底座详细用法见 [CATMonitor/docs/User_Manual.md](CATMonitor/docs/User_Manual.md)，EEP 见 [feature/elastic-ep/README.md](feature/elastic-ep/README.md)，Straggler 见 [feature/straggler/README.md](feature/straggler/README.md)。

---

## 0. 快速上手（60 秒）

```bash
# 1. 构建底座
cd CATHelper/CATMonitor && make build

# 2. 启动底座守护进程（采集 + Prometheus :9100）
./bin/catmonitor daemon

# 3. （可选）启用故障订阅/KPI 输出：编辑配置设 faultsub.enabled / straggler_output.enabled: true 后重启 daemon
#    faultsub → 故障订阅 REST :9101 + Webhook；straggler_output → straggler_kpi_{date}.jsonl

# 4. 部署带容错能力的 vLLM 服务（NPU 服务器上）
cd CATHelper
bash feature/elastic-ep/examples/fault_tolerance_scale/ft_vllm_serve_qwen.sh \
    --dp-size 4 --fault-port 22867 --port 8006

# 5. 启动外部故障管理中心（订阅 CATMonitor 故障 + 引擎健康）
python feature/elastic-ep/examples/fault_tolerance_scale/scale_down_demo.py \
    --npu-ids 0,1,2,3 --catmonitor-host localhost --catmonitor-rest-port 9101 \
    --callback-port 9102 --advertise-url http://localhost:9102/fault_event --port 8006

# 6.（可选）慢节点检测：CATMonitor 启用 straggler_output 后，定时跑 straggler CLI
cd feature/straggler && go build -o slowNodeDetection .
./slowNodeDetection --kpi-jsonl-dir=/var/lib/catmonitor/straggler \
    --faultsub-url=http://localhost:9101 --baseline-hours=360 --detection-hours=1
```

## 目录

- [1. 环境与依赖](#1-环境与依赖)
- [2. 构建与安装](#2-构建与安装)
- [3. 底座用法（CATMonitor）](#3-底座用法catmonitor)
- [4. 故障订阅机制（faultsub）](#4-故障订阅机制faultsub)
- [5. 上层特性用法（EEP）](#5-上层特性用法eep)
- [6. 慢节点检测用法（Straggler）](#6-慢节点检测用法straggler)
- [7. 端到端部署示例](#7-端到端部署示例)
- [8. 配置参考](#8-配置参考)
- [9. 排错与常见问题](#9-排错与常见问题)

---

## 1. 环境与依赖

### 1.1 底座（CATMonitor）

| 依赖 | 说明 |
|------|------|
| Go 1.21+ | 构建底座守护进程与 Web 仪表盘 |
| CANN SDK（`libdcmi.so`） | 可选，NPU 服务器用 `-tags dcmi` 启用 DCMI CGo 采集 |
| `nvidia-smi` / `npu-smi` / `hccn_tool` / `ipmitool` / `smartctl` | 可选，对应采集器无该命令时优雅降级 |

### 1.2 上层特性（EEP）

| 依赖 | 说明 |
|------|------|
| 华为昇腾 A3 服务器 | EEP 当前版本仅支持 A3 |
| vLLM + vLLM-Ascend v0.18.0 | 容错框架基于该版本打补丁 |
| Python 3.10+ | 运行外部故障管理中心 |
| `requests` / `zmq` / `msgspec` | Python 依赖（容错通信） |
| CATMonitor daemon（启用 `faultsub`） | **新增运行依赖**，提供 NPU 故障事件推送 |

> EEP 的安装（拉取 vLLM 镜像、打补丁、安装 vllm/vllm-ascend）详见 [feature/elastic-ep/README.md §安装](feature/elastic-ep/README.md)。

---

## 2. 构建与安装

### 2.1 构建底座

```bash
cd CATHelper/CATMonitor

# 默认构建（无 CGo，无 NPU/GPU 真机也能编译）
make build                         # 产物 ./bin/catmonitor

# NPU 服务器启用 DCMI 采集
go build -tags dcmi -o bin/catmonitor ./cmd/catmonitor

# Web 仪表盘二进制（可选）
go build -o features/web/bin/catmonitor-web ./features/web
```

> CATMonitor 子目录为独立 Go module，在 `CATMonitor/` 内即可独立构建。

### 2.2 安装底座为系统服务（Linux systemd）

```bash
sudo CATMonitor/scripts/install.sh
sudo systemctl start catmonitor
sudo systemctl enable catmonitor
sudo systemctl status catmonitor
sudo journalctl -u catmonitor -f
```

### 2.3 准备 EEP 特性

EEP 为 Python + vLLM 补丁形态，无需 Go 构建。按 [feature/elastic-ep/README.md](feature/elastic-ep/README.md) 的安装步骤拉取镜像、打补丁、安装 vllm/vllm-ascend 后即可使用。

---

## 3. 底座用法（CATMonitor）

底座的完整用法（命令行、Web 仪表盘、能效监控、Prometheus 导出、健康度评分、优雅降级、扩展）见 [CATMonitor/docs/User_Manual.md](CATMonitor/docs/User_Manual.md)。此处仅列常用命令：

```bash
catmonitor daemon                 # 守护进程（持续采集 + Prometheus :9100）
catmonitor collect -o table        # 单次采集快照
catmonitor health                  # 单次健康检查
catmonitor list                    # 采集器清单
catmonitor version                 # 版本
```

### 3.1 启用故障订阅（承上）

编辑 `configs/catmonitor.yaml`（或 `/etc/catmonitor/catmonitor.yaml`），开启 `faultsub`：

```yaml
faultsub:
  enabled: true
  rest_addr: ":9101"
  webhook_timeout: 5s
  webhook_retry: 1
  event_buffer: 1024
  defaults:
    debounce_ms: 0
    min_severity: "warning"
  rules:
    card_drop: true
    npu_health: true
    npu_error_code: true
    hbm_uce: true
    ddr_uce: true
    roce_link_down: true
    driver_unhealthy: false
```

启动 daemon 后将额外提供：
- **故障订阅 REST API** `:9101`（注册/查询订阅、快照、事件回补，见 §4）
- **HTTP Webhook 推送**：检测到故障时主动 POST `FaultEvent` 到订阅者声明的回调 URL

> 未启用时 daemon 行为与原版完全一致（零回归）。

---

## 4. 故障订阅机制（faultsub）

faultsub 是底座向上层特性提供故障信息的通道。详见 [CATMonitor/features/faultsub/faultsub_SPEC.md](CATMonitor/features/faultsub/faultsub_SPEC.md)。

### 4.1 工作模型

```
CATMonitor 采集周期 → FaultDetector 判定故障 → Dispatcher
   ├── HTTP Webhook POST FaultEvent → 订阅者回调 URL（主推送）
   └── 写环形缓冲 → REST 拉取（兜底/回补）
```

事件为**变迁驱动**：故障新出现或恢复时推送一次，持续故障不重复推送。

### 4.2 订阅 REST API（`:9101`）

| 方法 | 路径 | 用途 |
|------|------|------|
| POST | `/faultsub/subscriptions` | 注册订阅（声明回调 URL / 故障类型 / NPU / 去抖 / 严重级别） |
| GET | `/faultsub/subscriptions` | 列出订阅 |
| GET | `/faultsub/subscriptions/{id}` | 查看单个订阅 |
| DELETE | `/faultsub/subscriptions/{id}` | 注销订阅 |
| GET | `/faultsub/snapshot` | 各 NPU 最新活跃故障快照 |
| GET | `/faultsub/events?since=&type=&npu_id=` | 近期事件回补 |
| GET | `/faultsub/types` | 支持的故障类型 |
| GET | `/-/healthy` · `/-/ready` | 健康探针 |

### 4.3 注册订阅示例

```bash
curl -X POST http://localhost:9101/faultsub/subscriptions \
  -H 'Content-Type: application/json' \
  -d '{
    "types": ["card_drop","npu_error_code","hbm_uce","roce_link_down"],
    "components": ["npu"],
    "npu_ids": ["0","1","2","3"],
    "delivery": "webhook",
    "endpoint": "http://10.0.0.5:9102/fault_event",
    "debounce_ms": 0,
    "min_severity": "warning"
  }'
# → {"id":"sub-0001","created_at":"..."}
```

### 4.4 FaultEvent 消息格式

```json
{
  "event_id": "a1b2c3d4...",
  "type": "card_drop",
  "component": "npu",
  "npu_id": "3",
  "severity": "critical",
  "detail": { "error_codes": "0x40f84e00", "card_drop": "1" },
  "timestamp": "2026-07-28T10:30:00Z",
  "recovered": false
}
```

推送时 HTTP 头含 `Content-Type: application/json`、`X-CatMonitor-Event: <type>`、`X-CatMonitor-EventID: <id>`，订阅者回 `2xx` 即视为成功；非 2xx 或超时由 CATMonitor 按配置重试 1 次。

### 4.5 支持的故障类型

| 类型 | 说明 | 严重级别 |
|------|------|---------|
| `card_drop` | NPU 卡掉线 / 设备未就绪（DCMI -8012） | critical |
| `npu_health` | NPU 健康状态 Alarm/Critical | warning→critical |
| `npu_error_code` | NPU 上报错误码 | warning |
| `hbm_uce` | HBM 双 bit ECC（不可纠正） | critical |
| `ddr_uce` | DDR 双 bit ECC | critical |
| `roce_link_down` | RoCE 链路 down / 异常 | warning |
| `driver_unhealthy` | NPU 驱动健康异常 | warning |

---

## 5. 上层特性用法（EEP）

EEP 的完整用法见 [feature/elastic-ep/README.md](feature/elastic-ep/README.md)。此处给出与底座协同的关键步骤。

### 5.1 启动带容错能力的 vLLM 服务

```bash
bash feature/elastic-ep/examples/fault_tolerance_scale/ft_vllm_serve_qwen.sh \
    --dp-size 4 --redundant-experts 48 --fault-port 22867 --recovery-timeout 120 --port 8006
```

参数详见 [feature/elastic-ep/SPEC.md §5.1.3](feature/elastic-ep/SPEC.md)。

### 5.2 启动外部故障管理中心

外部故障管理中心（`scale_down_demo.py`）含两条故障检测路径：

- **路径①**：订阅 CATMonitor 故障事件（HTTP Webhook），经 NPU→DP 映射后下发 pause/scale_down/retry
- **路径②**：ZMQ SUB 订阅 vLLM 引擎健康状态（端口 `--external-fault-notify-port`），引擎 dead 时下发 scale_down

```bash
python feature/elastic-ep/examples/fault_tolerance_scale/scale_down_demo.py \
    --npu-ids 0,1,2,3 \
    --catmonitor-host localhost --catmonitor-rest-port 9101 \
    --callback-host 0.0.0.0 --callback-port 9102 \
    --advertise-url http://localhost:9102/fault_event \
    --external-fault-notify-port 22867 --port 8006 --recovery-timeout 120
```

参数详见 [feature/elastic-ep/SPEC.md §5.1.4](feature/elastic-ep/SPEC.md)。

> **跨机部署**：`--catmonitor-host` 填 CATMonitor 所在主机 IP，`--advertise-url` 填本机可达地址（`http://<本机可达IP>:9102/fault_event`），确保 CATMonitor 能反向 POST 回来。

### 5.3 手动容错（REST API）

不启动外部故障管理中心时，vLLM 内的容错框架仍会拦截异常并等待容错命令，可手动操作：

```bash
# 查询容错状态
curl http://localhost:8006/fault_tolerance/status

# 重试（瞬时故障）
curl -X POST http://localhost:8006/fault_tolerance/apply \
    -H 'Content-Type: application/json' \
    -d '{"instruction":"retry","params":{"timeout":30}}'

# 缩容（移除故障 DP rank）
curl -X POST http://localhost:8006/fault_tolerance/apply \
    -H 'Content-Type: application/json' \
    -d '{"instruction":"scale_down","params":{"timeout":30,"exclude_dp_ranks":[2]}}'
```

REST API 规格详见 [feature/elastic-ep/SPEC.md §5.3](feature/elastic-ep/SPEC.md)。

---

## 6. 慢节点检测用法（Straggler）

Straggler 是两道防线慢节点检测器（独立 Go module，`feature/straggler/`）。完整用法见 [feature/straggler/README.md](feature/straggler/README.md)，整合设计见 [straggler_combination_DESIGN.md](feature/straggler/straggler_combination_DESIGN.md)。

### 6.1 启用 CATMonitor KPI 输出（第一道数据来源）

编辑 `catmonitor.yaml`，开启 `straggler_output`（替代 straggler 自带 `kpi_collect.sh`）：

```yaml
straggler_output:
  enabled: true
  data_dir: /var/lib/catmonitor/straggler   # 产出 straggler_kpi_{date}.jsonl
  retention: 360h            # 保留 15 天（匹配基线窗口）
  flush_interval: 60s
```

daemon 启动后会持续写 `straggler_kpi_{YYYY-MM-DD}.jsonl`（每时刻含全部卡的 11 项 KPI）。

### 6.2 运行 straggler 检测（CLI/定时）

```bash
cd feature/straggler
go build -o slowNodeDetection .
./slowNodeDetection \
    --kpi-jsonl-dir=/var/lib/catmonitor/straggler \
    --faultsub-url=http://localhost:9101 \
    --baseline-hours=360 --detection-hours=1 \
    [degradation=0.3]
```

读最近 15 天 KPI → 第一道检测 → 报告（`npu_resource_detection_result.json` + 文本报告）+ 命中卡回注 faultsub。建议由 cron/定时器周期调度（如每 1h）。

### 6.3 第二道 Profiler 检测（按需，独立）

KPI 未发现异常但怀疑性能问题时，启用 Profiler：

```bash
./slowNodeDetection path=/data/profiler_output degradation=0.3
# 读 ascend_pytorch_profiler_*.db，检测慢计算/慢通信/慢CPU/NPU Bubble
```

可与第一道联合：`--kpi-jsonl-dir=... path=/data/profiler_output`（先 KPI，未命中则 fallback Profiler）。

### 6.4 回注 faultsub 与闭环

`--faultsub-url` 非空时，straggler 把每个命中卡作为 `straggler_detected` 事件 POST 给 faultsub（`POST /faultsub/events`），由 faultsub 推送给订阅者（EEP/运维）。root_cause（thermal_throttle/network_link_issue/straggler/...）放在 `detail`，订阅者据此决策卡隔离/排查。

---

## 7. 端到端部署示例

### 7.1 单机部署（CATMonitor 与 vLLM 同机）

```bash
# 终端 1：底座（启用 faultsub）
cd CATHelper/CATMonitor && make build
# 编辑 configs/catmonitor.yaml 设 faultsub.enabled: true
./bin/catmonitor daemon

# 终端 2：vLLM 容错服务
bash feature/elastic-ep/examples/fault_tolerance_scale/ft_vllm_serve_qwen.sh \
    --dp-size 4 --fault-port 22867 --port 8006

# 终端 3：外部故障管理中心
python feature/elastic-ep/examples/fault_tolerance_scale/scale_down_demo.py \
    --npu-ids 0,1,2,3 --catmonitor-host localhost --catmonitor-rest-port 9101 \
    --callback-port 9102 --advertise-url http://localhost:9102/fault_event --port 8006

# 终端 4：发送推理请求（服务就绪后）
curl http://localhost:8006/v1/chat/completions ...   # 略
```

### 7.2 跨机部署（CATMonitor 与 vLLM 分离）

| 角色 | 主机 | 关键配置 |
|------|------|---------|
| CATMonitor daemon | 10.0.0.10 | `faultsub.enabled: true`，`rest_addr: ":9101"` |
| vLLM + 故障管理中心 | 10.0.0.5 | `--catmonitor-host 10.0.0.10 --catmonitor-rest-port 9101 --advertise-url http://10.0.0.5:9102/fault_event` |

确保 `10.0.0.10` → `10.0.0.5:9102` 网络可达（CATMonitor 反向 POST webhook）。

### 7.3 故障注入验证

```bash
# 模拟 NPU 故障：kill 某个 Worker 进程
kill -9 <worker_pid>

# 观察链路：CATMonitor 检测卡掉线 → 推送 FaultEvent → 故障管理中心
#          pause → scale_down → vLLM 在剩余健康 NPU 上恢复推理
```

---

## 8. 配置参考

### 8.1 底座配置（`catmonitor.yaml`）

完整字段说明见 [CATMonitor/docs/User_Manual.md §2](CATMonitor/docs/User_Manual.md)。关键字段：

| 字段 | 说明 |
|------|------|
| `server.type` | `auto`（自动判定服务器类型） |
| `collectors.*.enabled` / `.interval` | 各采集器开关与周期 |
| `storage.data_dir` / `max_file_age` / `rotation` | JSONL 数据目录、保留时长、轮转 |
| `collection.min_priority` | `low`（全采，默认）/ `medium` / `high` |
| `faultsub.enabled` | 是否启用故障订阅推送（默认 false） |
| `faultsub.rest_addr` | 订阅 REST API 监听地址（默认 `:9101`） |
| `faultsub.webhook_timeout` / `webhook_retry` | webhook 推送超时与重试次数 |
| `faultsub.rules.*` | 各故障判定规则开关 |
| `straggler_output.enabled` | 是否启用慢节点检测 KPI 文件输出（默认 false） |
| `straggler_output.data_dir` | KPI 文件目录（默认 `/var/lib/catmonitor/straggler`） |
| `straggler_output.retention` | KPI 文件保留期（默认 15 天，匹配基线窗口） |
| `straggler_output.flush_interval` | 内存缓冲 flush 周期（默认 60s） |

### 8.2 端口一览

| 端口 | 提供方 | 用途 |
|------|--------|------|
| `9100` | CATMonitor | Prometheus `/metrics` |
| `9101` | CATMonitor（faultsub） | 故障订阅 REST API（含 `POST /faultsub/events` ingest） |
| `9527` | CATMonitor-web | Web 仪表盘（占用时自动 +1） |
| `9102` | EEP 故障管理中心 | 接收 CATMonitor webhook |
| `8006` | vLLM | 推理服务 + 容错 REST API |
| `22867` | vLLM | 引擎健康 ZMQ PUB |
| — | straggler CLI | 无监听端口；读 KPI 文件 + `--faultsub-url` 回注（定时调度） |

---

## 9. 排错与常见问题

| 现象 | 原因 / 解决 |
|------|-------------|
| 故障管理中心收不到 FaultEvent | 检查 `faultsub.enabled: true`；确认注册订阅成功（`GET /faultsub/subscriptions`）；确认回调 URL 可达（跨机用可达 IP） |
| `:9101` 无法访问 | faultsub 未启用，编辑配置设 `faultsub.enabled: true` 后重启 daemon |
| vLLM 收不到容错指令 | 检查 `--port` 与故障管理中心一致；vLLM 启动时带容错参数（`--enable-fault-tolerance`） |
| 端口冲突 | 9100/9101/9102/8006/22867 均可配置，错开即可 |
| DCMI/NPU CGo 编译失败 | `dcmi_cgo.go` 在 `-tags dcmi` 后，需真机 CANN SDK；默认构建排除即可 |
| GPU/NPU/Chassis 无数据 | 对应命令缺失即优雅降级返回空，非错误；NPU 仍输出 `npu_num=0` |
| 容错缩容失败 | 见 [feature/elastic-ep/Release_Notes.md §已知问题](feature/elastic-ep/Release_Notes.md)；冗余专家数须满足约束 |
| 持续故障重复推送 | faultsub 为变迁驱动（仅出现/恢复推送）；如仍重复检查 `debounce_ms` 是否为 0 |
| straggler 无 KPI 文件可读 | 检查 `straggler_output.enabled: true`；确认 `data_dir` 与 `--kpi-jsonl-dir` 一致；保留期内有数据 |
| straggler 回注 faultsub 失败 | 检查 `--faultsub-url` 正确且 faultsub 已启用；faultsub 不可用时 straggler 仍出报告（best-effort 回注） |
| straggler 构建失败（modernc.org/sqlite） | 首次 `go mod tidy` 拉取依赖；离线环境需预置模块缓存 |

> 底座更多排错见 [CATMonitor/docs/User_Manual.md §11](CATMonitor/docs/User_Manual.md)，EEP 已知问题见 [feature/elastic-ep/Release_Notes.md](feature/elastic-ep/Release_Notes.md)，Straggler 见 [feature/straggler/README.md](feature/straggler/README.md)。

---

*文档版本：v2.0 · 对应 CATHelper v0.2.0*
