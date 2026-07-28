# CATHelper

> Computing Availability Tools 系列 helper 软件。

## 组成

- **[CATMonitor](CATMonitor/)** — CATHelper 的底座：服务器运行指标采集、健康度评估与 Prometheus 导出守护进程。**可独立运行，也可作为 CATHelper 的一部分。** 构建用法见 [CATMonitor/README.md](CATMonitor/README.md)，使用手册见 [CATMonitor/docs/User_Manual.md](CATMonitor/docs/User_Manual.md)。

> CATMonitor 子目录为其主干快照，保持独立 Go module（`github.com/Computing-Availability-Tools/CATMonitor`），可在 `CATMonitor/` 内独立 `go build`/`make build`。
