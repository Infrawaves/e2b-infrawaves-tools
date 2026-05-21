# CLAUDE.md

E2B 集群运维 / 可观测性工具集。本文件给未来的 Claude 会话提供项目地图、约束和常见动作。

## ⚠️ 红线指标(已接入同事手机告警,不要改)

下列指标的**名称、标签集、语义**都已被外部告警引用,改动会触发误报或漏报:

- `node_port_listening`(Gauge,labels: `port`, `node_ip`) — 见 `nomad-nodeJob-exporter/checkNodeProcessPort.go`
- `nomad_allocation_up`(Gauge,labels: `service`, `node_id`, `node_name`, `status`, `desired_status`) — 见 `nomad-nodeJob-exporter/main.go`
- `nomad_allocation_memory_limit` / `nomad_allocation_memory_usage` / `nomad_allocation_memory_usage_percentage` — 同上,labels: `allocation_id`, `job_name`, `task_name`, `node_id`, `node_name`

需要补口径或加维度时,**新增**指标(例如 `e2b_node_port_listening_v2`),保留旧指标双发一段时间,而不是 in-place 改动。

## 仓库结构

| 路径 | 用途 |
| --- | --- |
| `nomad-nodeJob-exporter/` | Go Prometheus exporter,单二进制,`/metrics` 监听 `:9106` |
| `scheduling_monitor/` | 沙箱 → 物理节点离线分析(Python + bash agent) |
| `grafana/dashboards/<area>/*.json` | Dashboard JSON,UID 命名 `e2b-<area>-<name>`,datasource 用 `${DS_PROMETHEUS}` |
| `grafana/alerts/firecracker.yaml` | Prometheus rule 风格告警,推送时由脚本转 Grafana 托管告警 |
| `grafana/_legacy/` | 历史备份,**不参与同步** |
| `grafana/provisioning/` | 自管 Grafana 用的 datasource / dashboard provider 配置 |
| `scripts/sync-grafana.sh` | 推 dashboards |
| `scripts/sync-grafana-alerts.py` | 推 alerts(Prom rule → Grafana 三阶段查询模型) |
| `scripts/validate-grafana.py` | 跑全部 PromQL,报 OK/EMPTY/MISSING-METRIC/ERROR |
| `scripts/install-nomad-nodeJob-exporter.sh` / `upgrade-*.sh` | 节点装机/升级,支持 `GH_TOKEN` 与 `MIRROR_URL` |
| `docs/OBSERVABILITY.md` | 指标设计与路线图(短期/中期/长期) |
| `docs/RELEASE.md` | 私仓发布的三种方案对比 |
| `nomad-nodeJob-exporter/docs/` | 中文设计文档(Firecracker 进程指标 / 沙箱与节点指标) |

## Exporter 架构(改动时必读)

`main.go::updateMetrics()` 是每次 scrape 的总入口。子系统(都在同一个 binary 里):

- `checkFirecracker.go` → `e2b_fc_process_*`,扫 `/proc`,从 `--api-sock` 路径解析 `sandbox_id`
- `checkSandboxLeak.go` → `e2b_sandbox_*` + `e2b_orchestrator_*`,默认连本机 `127.0.0.1:9090` orchestrator gRPC,raw codec + `protowire` 手工解析
- `checkNodeHost.go` → `e2b_node_hugepages_*` / `e2b_node_disk_*`,读 `/sys/kernel/mm/hugepages` 与 `statfs`
- `checkNodeProcessPort.go` → `node_port_listening`(红线)
- `checkAllocationResource.go` / `checkNodeAllocationStatus.go` → `nomad_allocation_*`(其中 memory_* 与 up 是红线)+ `nomad_node_role` / `templaterole`

每次 scrape 都 `Reset()` 全部 GaugeVec 后再写入(在 `updateMetrics` 顶部)。新增指标必须:① 注册到 `registerMetrics()`,② 在 `updateMetrics()` 顶部加 `Reset()`,否则旧 series 不会消失。

## 指标命名约定

- `e2b_fc_*` — Firecracker 进程视角,labels 含 `node_ip` + `sandbox_id` + `pid`
- `e2b_sandbox_*` — 沙箱级,labels 至少有 `node_ip` + `sandbox_id`
- `e2b_node_*` — 节点级聚合,只有 `node_ip`(避免高基数)
- `e2b_orchestrator_*` — 直连 orchestrator gRPC 探测出的状态
- `nomad_*` — Nomad 自身视角,**不要**加 `sandbox_id`

累计量后缀 `_total` + Counter 语义。当前部分 `_total` 是 GaugeVec 表达 counter,是技术债,改名属于破坏性变更——和「红线指标」类似,要先双发。

## 关键环境/部署事实(易踩坑)

- **orchestrator gRPC 端口是 `9090`**(不是 `5008`/`5007`)。`5007` 是 proxy。dev/prod Nomad job `orchestrator.hcl` 是源头真相。
- **dev e2b CLI/SDK 跑在 dev gateway 节点上**(具体主机名/IP 见 `.env` 的 `DEV_GATEWAY_*`,`.env.example` 列出 keys)。`E2B_DOMAIN/API_URL/FORCE_HTTP/API_KEY/ACCESS_TOKEN` 写在该节点的 `/root/.bashrc`,但 `ssh ... e2b ...` 是非交互 shell **不会**读 `.bashrc` —— 命令里要显式 `export`。dev API URL / E2B domain 见 `.env` 的 `DEV_API_URL` / `DEV_E2B_DOMAIN`。Python SDK 起沙箱用 `Sandbox.create(template_id, timeout=...)`(不是 `Sandbox(template=...)`)。
- **沙箱在哪个节点**:在 `DEV_SANDBOX_NODES` 列出的节点上 `pgrep -fa <sandbox_id>`,找到 `firecracker --api-sock /tmp/fc-<sandbox_id>-*.sock` 的进程。
- **Exporter `:9106`**;`-oneshot` 一次性 dump 到 stdout 用于调试。
- **`DISABLE_SANDBOX_LEAK_CHECK=1`** 关 leak check(非 client 节点用)。

## Grafana 同步(`.env` 配置在仓库根目录,`.gitignored`)

`.env.example` 列出全部 keys。两个入口脚本:

```bash
scripts/sync-grafana.sh                # 推全部 dashboard,文件夹默认 "e2b"
scripts/sync-grafana.sh fc-overview.json   # 单文件
DRY_RUN=1 scripts/sync-grafana.sh      # 只校验
scripts/sync-grafana-alerts.py         # 推告警,文件夹默认 "e2b-alerts"
python3 scripts/validate-grafana.py    # 跑所有 PromQL 对线上 Prometheus 校验
```

要点:
- Dashboard 中 datasource **写成 `${DS_PROMETHEUS}` 变量**,不要硬编码 UID。`sync-grafana.sh` 在推送时按 `DS_NAME` 反查 UID 并写入 `templating.list[].current`。
- Alerts 用 Prometheus rule 格式(`alert/expr/for/labels/annotations`),`sync-grafana-alerts.py` 转成 Grafana 三阶段(query → reduce(last) → math)。复合表达式(如 `X > 0 and on(node_ip) Y == 1`)整段塞 stage 0,stage 2 退化为 `$B > 0`。
- Grafana 托管告警**会触发但不会通知**,直到 contact point + notification policy 在 UI 配好——脚本不管这一段。
- 部分 `e2b_sandbox_*` 在某些环境下 MISSING-METRIC 是预期(沙箱泄露检测尚未全集群部署),validate 报这条要 diff main 才能判断是否是新引入的回归。

## 编辑/工作约定

- 改完 dashboard JSON 必须从 Grafana UI 走 **Share → Export → Export for sharing externally**,不要手改 `id`,`uid` 必须稳定(脚本靠 uid 幂等)。新 dashboard 落到 `dashboards/<area>/<uid>.json`。
- 改告警:改 `grafana/alerts/firecracker.yaml`,跑 `DRY_RUN=1 scripts/sync-grafana-alerts.py` 看 diff,再正推。
- 改 exporter 指标名/标签前问:**是否在红线列表里?是否在告警 yaml / dashboard JSON 里被引用?** 用 `grep -r '<metric>' grafana/` 全文确认。
- Go 编译:`cd nomad-nodeJob-exporter && GOOS=linux GOARCH=amd64 go build`(已有的 `nomad-nodeJob-exporter-linux-amd64` 是上一次产物,不要直接覆盖,先备份)。
- Python 没有 lockfile;脚本只用 stdlib,不要引第三方依赖以免装机困难。

## 不要做

- 不要 in-place 改红线指标(见顶部)。
- 不要在 `e2b_node_*` 上加高基数标签(`sandbox_id`/`pid`),会拖垮 Prometheus。
- 不要在 dashboard JSON 里硬编码 datasource UID。
- 不要把 `GH_TOKEN` 写进 systemd unit 或留在节点 shell 历史里(见 `docs/RELEASE.md` 的安全注意)。
- 不要假设 orchestrator 端口是上游默认的 5008,本仓库环境固定 9090。
