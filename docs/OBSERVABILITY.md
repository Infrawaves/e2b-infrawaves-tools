# E2B 可观测性路线图

参考目标系统 [`e2b_val`](../) 的内置 OTel 指标(orchestrator host/sandbox metrics、ingress proxy、NFS proxy 等),
本仓库以 **节点侧旁路采集** 的方式补全 e2b_val 不暴露或不易暴露的视角。

## 5 层视图(对外讲解口径)

对客户/新人讲整体观测面时,按"故障从底向上传播"的顺序分 5 层。
每一层独立,某层全红时上层指标多半会跟着失真——告警**要按层定级**,避免单一故障引发雪崩告警。

| 层 | 视角 | 指标前缀 | 数据源 | 实现文件 | 详细文档 |
| --- | --- | --- | --- | --- | --- |
| **1** | **OS 容量(e2b 关键资源)** | `e2b_node_*` | `/sys/kernel/mm/hugepages`、`statfs(2)` | `checkNodeHost.go` | [沙箱与节点指标](../nomad-nodeJob-exporter/docs/E2B%20沙箱与节点指标设计.md) |
| **2** | **Nomad 调度 + 端口探活** | `nomad_*`、`node_port_listening` | `nomad` CLI、TCP probe | `checkNodeAllocationStatus.go`、`checkAllocationResource.go`、`checkNodeProcessPort.go` | [Nomad 与端口探测指标](../nomad-nodeJob-exporter/docs/E2B%20Nomad%20与端口探测指标设计.md) |
| **3** | **Orchestrator 控制面** | `e2b_orchestrator_*` | 本机 gRPC `SandboxService.List`(白盒) | `checkSandboxLeak.go` | [沙箱与节点指标](../nomad-nodeJob-exporter/docs/E2B%20沙箱与节点指标设计.md) |
| **4** | **沙箱业务(权威列表 + 一致性)** | `e2b_sandbox_*` | orchestrator gRPC + `/proc` 比对 | `checkSandboxLeak.go` | [沙箱与节点指标](../nomad-nodeJob-exporter/docs/E2B%20沙箱与节点指标设计.md) |
| **5** | **Firecracker 进程** | `e2b_fc_process_*` | `/proc/<pid>/` | `checkFirecracker.go` | [Firecracker 进程指标](../nomad-nodeJob-exporter/docs/E2B%20Firecracker%20进程指标设计.md) |
| 旁路 | 沙箱 → 物理节点映射 | — | NFS 共享目录 | `scheduling_monitor` | (本文档下方) |

> 说明:第 3 层和第 4 层在实现上是同一个 .go 文件、同一次 gRPC 调用产出的——**物理实现合在一起,语义分两层**。
> 文档归档以前缀为准便于检索;本表则按层叙述便于沟通。

## 现有覆盖(按实现分组)

| 维度 | 来源 | 实现 |
| --- | --- | --- |
| Nomad allocation 健康 | `nomad-nodeJob-exporter` | `nomad_allocation_up`, `nomad_allocation_cpu/memory_*` |
| 节点 role / templaterole | `nomad-nodeJob-exporter` | `nomad_node_role`, `nomad_node_templaterole` |
| 关键端口监听探测 | `nomad-nodeJob-exporter` | `node_port_listening`(5016 / 9090,黑盒) |
| Firecracker 进程级 | `nomad-nodeJob-exporter` | `e2b_fc_process_*` (count / mem / cpu / io / fds / ctx switches / state) |
| 节点容量(HugeTLB / 磁盘) | `nomad-nodeJob-exporter` | `e2b_node_hugepages_*`、`e2b_node_disk_*` |
| Orchestrator 探活 | `nomad-nodeJob-exporter` | `e2b_orchestrator_reachable`、`e2b_orchestrator_list_duration_seconds` |
| 沙箱 → 物理节点映射 | `scheduling_monitor` | NFS 共享目录 + 离线脚本 |
| **沙箱泄露检测** | `nomad-nodeJob-exporter` | 见下文 [沙箱泄露检测](#沙箱泄露检测) |

## 沙箱泄露检测

每次 scrape,exporter 在节点本地比对:

- **集合 A**:`/proc` 扫描出的 firecracker 进程的 `sandbox_id`(从 `--api-sock` 路径解析)
- **集合 B**:本机 orchestrator gRPC `SandboxService.List`(127.0.0.1:9090)返回的 `sandbox_id`

| 集合关系 | 含义 | 指标 |
| --- | --- | --- |
| A ∩ B | 正常 | `e2b_sandbox_consistent_count{node_ip}` |
| A \ B | **泄露 fc**:orchestrator 不再管理,但进程还在,占 HugeTLB / vCPU | `e2b_sandbox_leak_count{node_ip}`、`e2b_sandbox_leak{node_ip,sandbox_id,pid}` |
| B \ A | **孤儿沙箱**:orchestrator 认为活着,fc 已没了 | `e2b_sandbox_orphan_count{node_ip}`、`e2b_sandbox_orphan{node_ip,sandbox_id}` |
| — | gRPC 是否能连通 | `e2b_orchestrator_reachable{node_ip}` 0/1 |

设计要点:

- **节点本地比对**:每个节点单独比对自己的 fc 和自己的 orchestrator,不需要全集群聚合 API,也能定位具体节点。
- **orchestrator 不可达时不发布数据**:避免 orchestrator 故障时 leak 看起来 100%,误导报警。`e2b_orchestrator_reachable=0` 即触发独立的 critical 告警。
- **不依赖 protoc**:用 raw codec + `google.golang.org/protobuf/encoding/protowire` 手工解析 `SandboxListResponse` 中的 `sandbox_id` 字段。proto wire format 编号变化时(目前 `SandboxConfig.sandbox_id=6`),改一个常量即可,不需要重新生成 stub。代价是不感知字段重命名/类型变更 — 由 e2b infra 升级时复核。
- **告警**:`grafana/alerts/firecracker.yaml` 中已加 `E2BFirecrackerLeak`、`E2BSandboxOrphan`、`E2BOrchestratorUnreachable`,持续 10 分钟才触发,屏蔽启停过渡期。
- **关闭开关**:非 client 节点(没跑 orchestrator 和 fc)可通过 `DISABLE_SANDBOX_LEAK_CHECK=1` 关掉。

定位与处置(运维 SOP):

1. Grafana 看 `E2B / Sandbox Leak Detection` dashboard,按 `node_ip` 钻取。
2. 用 `e2b_sandbox_leak{sandbox_id,pid}` 拿到 leak 进程 PID。
3. SSH 到节点,先 `cat /proc/<pid>/cmdline` 二次确认是 firecracker、且 sandbox_id 匹配,再 `kill <pid>`。
4. 如果是 orphan(B \ A),走 e2b 集群 admin API 强 kill 该 sandbox(`POST /admin/teams/{teamID}/sandboxes/kill`)或直接清 orchestrator 状态。
5. 持续触发同一节点 → 该 client 进入软隔离,不接新沙箱,等流量退干净后重启 orchestrator。

## 与 e2b_val 内置指标的关系

e2b_val 的 orchestrator 通过 OTLP 上报到 Mimir/Tempo/Loki:

- `orchestrator_sandbox_cpu_total`, `..._cpu_used`, `..._ram_*`, `..._disk_*` — 沙箱内部视角
- `orchestrator_host_*` — orchestrator 进程所在节点的 CPU/内存/磁盘
- `orchestrator_ingress_proxy_*` — 入口代理连接数 / 时长 / 阻断
- `orchestrator_nfs_proxy_*` — NFS 流量、open files、读写
- `nomad_*` — 由 nomad 自带 telemetry 暴露(集群层面)

旁路 exporter 提供的差异价值:
1. **进程级 cgroup 外信息**:RSS、HugeTLB、context switches、open fds、I/O syscalls,这些都不在 envd/orchestrator 上报范围内。
2. **不依赖 OTLP 链路**:Mimir/Tempo 出问题时仍能通过 Prometheus 直接拉取节点。
3. **物理拓扑视角**:绑定 `node_ip`,而 e2b_val 的指标以 `team_id`/`sandbox_id` 为主,不易回到节点。

## 短期(优先级高)

### 1. 节点级 firecracker 资源汇总
**为什么**:目前每个 fc 进程一条时序,高基数,Prometheus 长尾压力大;聚合面板需要 sum-over-instance。
**怎么做**:在 `updateFirecrackerMetrics` 中额外发布 `e2b_fc_node_*`(无 `sandbox_id`/`pid` 标签)聚合指标 — count、hugetlb_total、cpu_seconds_total、threads_total。仍保留进程级以便定位单沙箱问题,但用 recording rule 或直接 sum 都可以;直接 exporter 出聚合指标更省事。

### 2. Firecracker socket 健康探测
**为什么**:`--api-sock` 是 Firecracker 控制面;sock 存在但不响应代表进程僵死,而 `e2b_fc_process_count` 仍 +1。
**怎么做**:`checkFirecracker.go` 已经能拿到 sock 路径,加一次 `unix.Connect` 探测,导出 `e2b_fc_socket_responsive{node_ip,sandbox_id}`。

### 3. HugeTLB 节点容量 ✅ 已完成

实现:`checkNodeHost.go` 读 `/sys/kernel/mm/hugepages/hugepages-<N>kB/{nr,free,resv}_hugepages`,
导出 `e2b_node_hugepages_{total,free,reserved}`,标签 `node_ip` + `size_bytes`(字节字符串)。
配套告警 `e2b-node-hugepages-saturated` 已上线。详见 [沙箱与节点指标设计](../nomad-nodeJob-exporter/docs/E2B%20沙箱与节点指标设计.md#9-e2b_node_hugepages_total--_free--_reserved)。

### 4. Conntrack / TCP 连接数
**为什么**:沙箱网络通过 NAT 出口,conntrack 表满会导致建连失败但不会有明显日志。
**怎么做**:读 `/proc/sys/net/netfilter/nf_conntrack_count` 和 `nf_conntrack_max`,导出 `e2b_node_conntrack_{count,max}`。

## 中期

### 5. Sandbox 生命周期事件流(Loki / 日志)
e2b_val 的 orchestrator 已经有 `sbxlogger`,但本仓库 fleet 部分场景日志量大且未集中。建议起 `vector` 或 `promtail` 把 nomad logs / firecracker stderr 推到 Loki,然后在 Grafana 上做 "create → ready → close" 的 timeline。

### 6. Nomad node pool 容量水位
`nomad-nodepool-apm` 已有插件做扩缩容,但缺一个面板告诉运维"build 池现在 12/20 节点,平均 fc 数 X"。
**做法**:exporter 增加调用 `/v1/nodes?pool=...` 的逻辑,导出 `nomad_nodepool_{capacity,available,fc_per_node}`。

### 7. NFS proxy 客户端可见性
e2b_val 的 NFS proxy 自身有 metrics,但只能看 proxy 端;在节点上加 `mount` 状态 + `/proc/self/mountstats` 解析,导出每个 NFS 挂载点的 RTT / retrans / 丢包,定位"挂载侧"问题。

### 8. PSI (Pressure Stall Information)
`/proc/pressure/{cpu,memory,io}`,导出 `e2b_node_pressure_{cpu,memory,io}_{some,full}`。比 load average 更能反映"用户态被挤的程度"。

## 长期 / 架构级

### 9. 把 exporter 拆成 sidecar + node-agent
现在所有逻辑在一个二进制内,Nomad 查询和 firecracker 进程扫描频率耦合(一个 scrape 就触发两类),scrape 慢时会拖累。
**做法**:firecracker 扫描和 hugepage/conntrack 走 node-agent (5s loop, in-memory cache);Nomad 查询走 sidecar(30s loop)。两个 `/metrics` 端口,避免长尾互相影响。

### 10. eBPF 增强(可选)
`bcc` / `bpftrace` 抓 fc 进程的 syscall 失败率、网络 retransmit、磁盘 latency,补齐 `/proc` 看不到的内核态信息。需要评估部署复杂度。

### 11. 接入 Tempo trace
e2b_val 已经有 OTLP collector;如果运维链路上线,可让 exporter 的 Nomad API 调用(`getAllocations`、`getAllocationResourceInfo`)也带 trace,定位"为什么这次 scrape 这么慢"。

## 部署矩阵

| 组件 | 部署单元 | 频率 | 数据流向 |
| --- | --- | --- | --- |
| `nomad-nodeJob-exporter` | systemd, 每个 client 节点一份 | scrape 触发 | Prometheus → Grafana |
| `scheduling_monitor` agent | systemd / cron | 5s | 共享 NFS 目录,离线分析 |
| `fc-socket-probe`(规划) | 同上 exporter,合并到一个二进制 | scrape 触发 | 同上 |
| Loki 客户端(规划) | vector / promtail | streaming | Loki → Grafana |

## 命名规范

- `e2b_fc_*`:Firecracker 进程或沙箱视角,`node_ip` + `sandbox_id` + `pid` 标签。
- `e2b_node_*`:节点级聚合,只有 `node_ip` 标签(避免高基数)。
- `nomad_*`:Nomad 自身视角(allocation / job / node),不要在这里加 `sandbox_id`。
- 所有累计量后缀 `_total` 并使用 Counter 语义(目前部分用了 GaugeVec 表达 counter — 是技术债,后续切到 `CounterVec`)。

## 后续 PR 建议

每条路线图项一个 PR,先发 #1 / #2 / #3 节点容量类(改动量小、价值高),再做 #5 日志聚合(改动量大)。
