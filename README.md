# CATHelper

> CATHelper是CAT技术架构的主体部分，服务于鲲鹏和昇腾服务器，提供全栈故障指标采集、分析和容错恢复能力，方便被集成，以及使能大型生产环境的高可用特性开发。

**Computing Availability Tools** 系列的 helper 软件，采用"底座 + 上层特性"的分层架构：底座提供全栈指标采集与健康度评估能力，上层承载面向推理高可用场景的容错恢复特性。

| 项目 | 说明 |
|------|------|
| 版本号 | v0.1.0 |
| 发布时间 | 2026-07-28 |
| 许可证 | Apache-2.0 |

## 组成

```
CATHelper/
├── CATMonitor/            # 底座：全栈指标采集、健康度评估、Prometheus 导出守护进程
│   ├── internal/          #   采集核心 + 7 部件采集器 + 14 来源层
│   ├── features/          #   健康度 / Web 仪表盘 / 能效监控 / Prometheus 导出 / 故障订阅推送
│   └── configs/           #   catmonitor.yaml + metrics.yaml
└── feature/
    └── elastic-ep/        # 上层特性：推理大EP卡级弹性容错（EEP）
        ├── patches/       #   vLLM + vLLM-Ascend 容错框架补丁
        └── examples/      #   容错服务启动脚本 + 外部故障管理中心（订阅 CATMonitor）
```

### 底座 — [CATMonitor](CATMonitor/)

服务器运行指标采集、健康度评估与 Prometheus 导出守护进程。覆盖 CPU / 内存 / 硬盘 / GPU / NPU / 网卡 / 机箱共 7 个部件、204 个指标；输出 JSONL 落盘 + Prometheus `/metrics`；提供故障信息订阅/推送机制（`faultsub`）供上层特性获取故障事件。**可独立运行，也可作为 CATHelper 的一部分。** 构建用法见 [CATMonitor/README.md](CATMonitor/README.md)，使用手册见 [CATMonitor/docs/User_Manual.md](CATMonitor/docs/User_Manual.md)。

> CATMonitor 子目录为其主干快照，保持独立 Go module（`github.com/Computing-Availability-Tools/CATMonitor`），可在 `CATMonitor/` 内独立 `go build`/`make build`。

### 上层特性 — [EEP（推理卡级弹性容错）](feature/elastic-ep/)

推理大 EP 卡级弹性容错特性（Elastic EP）。实现 DP+EP 部署模式下卡故障后推理实例不退出，而是隔离故障卡所在 DP 域、重排专家后剩余 DP 继续提供推理服务，并支持网络闪断故障后请求重推恢复。当前仅支持 vLLM，后续计划支持 SGLang。详见 [feature/elastic-ep/README.md](feature/elastic-ep/README.md)。

EEP 的故障信息输入已与 CATMonitor 底座有机整合：通过 `faultsub` 订阅机制，CATMonitor 采集并判定 NPU 故障（卡掉线 / 健康状态 / 错误码 / HBM UCE / RoCE 链路等），经 HTTP Webhook 推送给 EEP 的外部故障管理中心，由其映射 NPU→DP rank 后下发容错指令。整合设计见 [EEP_combination_DESIGN.md](feature/elastic-ep/EEP_combination_DESIGN.md)。

## 路线图

- **推理慢节点满卡检测特性**：后续版本将新增该上层特性，基于 CATMonitor 采集的推理性能与卡级指标检测慢节点并满卡处理。

## 文档

| 文档 | 说明 |
|------|------|
| [SPEC.md](SPEC.md) | 功能规格说明书（面向使用者的整体功能介绍） |
| [User_Manual.md](User_Manual.md) | 使用手册（构建、安装、配置、底座与特性用法） |
| [Release_Notes.md](Release_Notes.md) | 版本发布记录 |
| [CATMonitor/](CATMonitor/) | 底座子项目（README / SPEC / DESIGN / 使用手册 / 指标清单） |
| [feature/elastic-ep/](feature/elastic-ep/) | EEP 特性子项目（SPEC / DESIGN / Release_Notes / 测试报告） |

## 快速上手

```bash
# 1. 构建并启动底座（采集 + Prometheus :9100 + 故障订阅 REST :9101）
cd CATMonitor && make build
# 编辑 configs/catmonitor.yaml，设 faultsub.enabled: true（按需）
./bin/catmonitor daemon

# 2. 部署带容错能力的 vLLM 服务（详见 feature/elastic-ep/README.md）
bash feature/elastic-ep/examples/fault_tolerance_scale/ft_vllm_serve_qwen.sh --dp-size 4 --fault-port 22867 --port 8006

# 3. 启动外部故障管理中心（订阅 CATMonitor 故障事件 + 引擎健康状态）
python feature/elastic-ep/examples/fault_tolerance_scale/scale_down_demo.py \
    --npu-ids 0,1,2,3 --catmonitor-host localhost --catmonitor-rest-port 9101 \
    --callback-port 9102 --advertise-url http://localhost:9102/fault_event --port 8006
```

> 完整使用说明见 [使用手册](User_Manual.md)，功能概览见 [SPEC.md](SPEC.md)。
