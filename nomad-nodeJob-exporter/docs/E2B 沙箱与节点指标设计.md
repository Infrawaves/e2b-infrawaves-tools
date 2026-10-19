# E2B 沙箱与节点指标设计文档

## 概述

本文档描述 `nomad-nodeJob-exporter` 在节点上额外采集的三类指标：

1. **沙箱视角（`e2b_sandbox_*`）**——通过本机 orchestrator gRPC `SandboxService.List` 拿到权威沙箱列表，与 `/proc` 扫描的 firecracker 进程比对，输出一致性 / 泄露 / 孤儿 / 存活时长 / 超 TTL 等指标。
2. **Orchestrator 探活（`e2b_orchestrator_*`）**——本机 orchestrator gRPC 是否可达、List RTT。
3. **节点指标（`e2b_node_*`）**——节点级 HugeTLB 池、关键路径的磁盘容量与部署二进制 MD5。

姊妹文档：[E2B Firecracker 进程指标设计.md](./E2B%20Firecracker%20进程指标设计.md)（系统进程视角，`e2b_fc_process_*` 系列）。

### 设计分层回顾

| 视角层级 | 指标前缀 | 数据源 | 文件 |
|---------|---------|-------|------|
| 系统进程视角 | `e2b_fc_process_*` | `/proc/<pid>/` | `checkFirecracker.go` |
| **业务沙箱视角（旁路）** | **`e2b_sandbox_*`** | **本机 orchestrator gRPC + 进程比对** | **`checkSandboxLeak.go`** |
| **Orchestrator 探活** | **`e2b_orchestrator_*`** | **gRPC `SandboxService.List` 调用结果** | **`checkSandboxLeak.go`** |
| **节点指标** | **`e2b_node_*`** | **`/sys/kernel/mm/hugepages` + `statfs(2)` + 本机二进制文件** | **`checkNodeHost.go`** |

> "旁路实现"是指：`e2b_sandbox_*` 不是 orchestrator / sandbox 自己上报的，而是 exporter 主动调 gRPC 拉权威列表，再与本地观察事实交叉验证。这种方式不依赖 OTLP 链路，控制面故障时仍能从 Prometheus 直接拉数据。

## 设计原则

1. **节点本地比对**：每个节点单独比对自己的 fc 与自己的 orchestrator，不需要全集群聚合 API，定位时直接拿 `node_ip` 钻取。
2. **gRPC 不可达 ≠ 0 沙箱**：orchestrator 不可达时不刷新 leak/orphan 数据，避免控制面故障被误读成"100% 泄露"。`e2b_orchestrator_reachable=0` 触发独立的 critical 告警。
3. **不依赖 protoc 工具链**：用 raw codec + `google.golang.org/protobuf/encoding/protowire` 手工解析 `SandboxListResponse` 中的字段。proto wire format 字段编号变化时，改一个常量即可，不需要重新生成 stub。代价是不感知字段重命名/类型变更——升级 e2b infra 时复核常量定义即可。
4. **节点容量无前置依赖**：`e2b_node_*` 与 orchestrator 解耦，即使 orchestrator 死了也能继续采集 HugeTLB / 磁盘剩余，给独立告警提供数据。

## 标签设计

| 标签 | 类型 | 说明 | 示例 |
|------|------|------|------|
| `node_ip` | string | 裸金属宿主机 IP | `10.0.0.1` |
| `sandbox_id` | string | 沙箱业务标识 | `dddklj7hkiqc8biw6ezy` |
| `pid` | int | 仅 `e2b_sandbox_leak` 携带，标识泄露的 fc 进程 | `12345` |
| `node_id` | string | orchestrator 的 `client_id` | `client-7` |
| `template_id` | string | 沙箱使用的模板 | `python3.11` |
| `team_id` | string | 沙箱所属 team | `team-abc` |
| `path` | string | `e2b_node_disk_*` 监控的路径 | `/`、`/mnt/nfs` |
| `binary` | string | `e2b_node_binary_info` 二进制名称 | `orchestrator`、`envd` |
| `md5` | string | `e2b_node_binary_info` 文件 MD5 | `1b3dda69ab27bf4bea7087c53a4c7e76` |
| `size_bytes` | string | HugeTLB 页大小（字节字符串） | `2097152`、`1073741824` |

> 注：`e2b_sandbox_info` 是高基数指标（每沙箱一条），不要在告警 expr 里用作 group key。

### 节点二进制 MD5

每次 scrape 检查本机 `/opt/orchestrator/orchestrator` 与 `/fc-envd/envd`，
以 info 风格输出 `e2b_node_binary_info{node_ip,binary,path,md5} 1`。
文件缺失或读取失败时记录日志且不输出该文件的指标；不同节点及二进制可按 `node_ip` 和 `binary` 对比。

## 沙箱视角指标

### 1. `e2b_sandbox_consistent_count`

| 属性 | 值 |
|------|-----|
| 类型 | Gauge |
| 标签 | `node_ip` |
| 说明 | A ∩ B 的数量：`/proc` 与 orchestrator 都看到的沙箱，正常运行 |
| 示例 | `e2b_sandbox_consistent_count{node_ip="10.0.0.1"} 38` |

### 2. `e2b_sandbox_leak_count` / `e2b_sandbox_leak`

| 属性 | 值 |
|------|-----|
| 类型 | Gauge |
| 标签 | `_count`: `node_ip`；`leak`: `node_ip`, `sandbox_id`, `pid` |
| 说明 | A \ B **中剔除 build 前缀后**的部分:`i-` 等用户沙箱 fc 进程在但 orchestrator 不再管理。**这些进程占着 HugeTLB / vCPU，是真正的资源泄露。** |
| 示例 | `e2b_sandbox_leak{node_ip="10.0.0.1",sandbox_id="i...",pid="12345"} 1` |

**为什么需要 per-pid 标签**：定位时直接 `kill <pid>` 即可，不必 SSH 上去再 `pgrep`。`_count` 用于告警阈值，`leak` 用于钻取细节。

> **build VM 不算泄露,也不单独建计数指标**:template build 的 fc VM 的 sandbox_id 以 build 前缀(上游 `InstanceBuildPrefix="b"`)开头,起真实 fc 但走 template-manager 构建路径、不进 `SandboxService.List`,因此永远落在 A\B 里。早期版本把它们误判为泄露(全 `b-`、高速轮换、不累积、orphan 恒 0,见 issue #12),现已从 leak 中剔除。
>
> build 的可见性靠 per-process 的 `e2b_fc_process_*{vm_kind="build"}`(见下文 firecracker 进程指标)。**不单独出 build 计数/卡死指标**,因为:① build sandbox_id 高速轮换(一次 build 顺序起多个 `b-` 沙箱;issue #12 实测 30min 内 505 个不同 id、任一时刻仅约 10 个),瞬时数 ≠ build 任务数;② build fc 生命周期短,周期 scrape 采样混叠、严重低估。需要瞬时数用 `count(e2b_fc_process_info{vm_kind="build"})`(理解其为快照、会低估);卡死用 `e2b_fc_process_uptime_seconds{vm_kind="build"}` 在告警 `e2b-build-vm-stuck` 判阈值(阈值留 yaml)。要**可靠**的 build 活跃度/任务数需从 template-manager 取,见 issue #13。

### 3. `e2b_sandbox_orphan_count` / `e2b_sandbox_orphan`

| 属性 | 值 |
|------|-----|
| 类型 | Gauge |
| 标签 | `_count`: `node_ip`；`orphan`: `node_ip`, `sandbox_id` |
| 说明 | B \ A：orchestrator 认为活着，但本机 fc 已经没了。**控制面状态过期，需要走 admin API 清理或强 kill。** |
| 示例 | `e2b_sandbox_orphan{node_ip="10.0.0.1",sandbox_id="..."} 1` |

### 4. `e2b_sandbox_info`

| 属性 | 值 |
|------|-----|
| 类型 | Gauge（值固定 1） |
| 标签 | `node_ip`, `sandbox_id`, `node_id`, `template_id`, `team_id` |
| 说明 | 沙箱身份信息映射，用于 join 业务 dashboard（`team × template × node`） |
| 数据源 | orchestrator `RunningSandbox.config` |
| 示例 | `e2b_sandbox_info{node_ip="10.0.0.1",sandbox_id="...",node_id="client-7",template_id="python3.11",team_id="team-abc"} 1` |

**用途**：

- 算每个 team / template 的沙箱数：`count by(team_id) (e2b_sandbox_info)`
- 把 `e2b_fc_process_*` 通过 `on(sandbox_id) group_left(team_id, template_id)` 关联到业务维度

### 5. `e2b_sandbox_age_seconds`

| 属性 | 值 |
|------|-----|
| 类型 | Gauge |
| 标签 | `node_ip`, `sandbox_id` |
| 说明 | `now - RunningSandbox.start_time`，沙箱已存活秒数 |
| 数据源 | orchestrator `RunningSandbox.start_time`（`google.protobuf.Timestamp`） |
| 示例 | `e2b_sandbox_age_seconds{...} 14523` |

**与 `e2b_fc_process_uptime_seconds` 的区别**：`uptime` 是 fc 进程启动时间（`/proc/<pid>/stat`），`age` 是 orchestrator 记录的沙箱开始时间。**通常一致，差异显著时说明 fc 进程被重启了但 orchestrator 视图未变**。

### 6. `e2b_sandbox_overrun_seconds`

| 属性 | 值 |
|------|-----|
| 类型 | Gauge |
| 标签 | `node_ip`, `sandbox_id` |
| 说明 | 沙箱已超过 `SandboxConfig.max_sandbox_length` 的秒数；0 表示在限额内；max=0 的沙箱（无声明上限）不上报 |
| 数据源 | `age - max_sandbox_length * 3600` |
| 示例 | `e2b_sandbox_overrun_seconds{...} 1800` |

**典型成因**：

- 客户端持续 keepalive
- orchestrator TTL 清理逻辑失败
- 业务 SDK 没正确处理 `close`

## 沙箱配置画像指标

> ⚠️ **数据源分两类，稳定性不同**：
> - **A 类**（`vcpu` / `ram_mb` / `end_time`）取自 `RunningSandbox` 顶层字段，上游长期稳定。
> - **B 类**（版本、`total_disk_size_mb`、`auto_pause`、`allow_internet_access`、`auto_resume`）取自 `RunningSandbox.config` 子消息，**上游已标记 deprecated**（注释原文：Redis is the source of truth；该 Config 仍被填充只是为了 rollout 期间兼容旧 API）。将来某次 orchestrator 升级后 B 类字段可能停止填充而变空 / 为 0，告警不要强依赖，见 issue #13。
>
> `state` 字段**不提供**：gRPC `List` 只返回正在运行的沙箱，state 恒为 `running` 无区分度；真实 state 只在 Redis catalog 层，取它需引 Redis 客户端并打破 exporter「只看本机」模型，故放弃。

### 7. `e2b_sandbox_build_info`

| 属性 | 值 |
|------|-----|
| 类型 | Gauge（值固定 1） |
| 标签 | `node_ip`, `sandbox_id`, `kernel_version`, `firecracker_version`, `envd_version` |
| 说明 | 沙箱构建版本画像。标签为空表示该 orchestrator 版本未填充对应 Config 字段 |
| 数据源 | `SandboxConfig.kernel_version` / `firecracker_version` / `envd_version`（B 类，deprecated） |
| 示例 | `e2b_sandbox_build_info{sandbox_id="...",kernel_version="6.1.102",firecracker_version="1.10.1",envd_version="0.2.4"} 1` |

**用途**：版本漂移排查。单独成 info 而不塞进 `e2b_sandbox_info`，避免给已高基数的 info 再叠三个字符串标签使基数翻倍。版本变更本身意味着沙箱重建（`sandbox_id` 会变），不会在同一 series 上翻动。

### 8. `e2b_sandbox_vcpu` / `e2b_sandbox_ram_mb` / `e2b_sandbox_disk_mb`

| 属性 | 值 |
|------|-----|
| 类型 | Gauge |
| 标签 | `node_ip`, `sandbox_id` |
| 说明 | 沙箱配置的 vCPU 数 / 内存 MB / 总磁盘 MB。值为 0（未知）时不上报，避免误导容量视图 |
| 数据源 | `RunningSandbox.vcpu`（A 类）、`RunningSandbox.ram_mb`（A 类）、`SandboxConfig.total_disk_size_mb`（B 类，deprecated） |
| 示例 | `e2b_sandbox_vcpu{...} 2` / `e2b_sandbox_ram_mb{...} 2048` / `e2b_sandbox_disk_mb{...} 5120` |

**用途**：按 team / template 聚合资源申请量，与节点实际 `nomad_allocation_memory_*` 对照看超售。

### 9. `e2b_sandbox_end_time_seconds`

| 属性 | 值 |
|------|-----|
| 类型 | Gauge |
| 标签 | `node_ip`, `sandbox_id` |
| 说明 | `RunningSandbox.end_time` 的 Unix 秒；0 表示未设置（无计划到期） |
| 数据源 | `RunningSandbox.end_time`（A 类，`google.protobuf.Timestamp`） |
| 示例 | `e2b_sandbox_end_time_seconds{...} 1718000000` |

**与 `overrun` 的区别**：`overrun` 由 `config.max_sandbox_length` 推算，`end_time` 是 orchestrator 实际记录的到期时刻，可做一致性校验（`end_time - start_time` 应 ≈ `max_sandbox_length`）。

### 10. `e2b_sandbox_allow_internet` / `e2b_sandbox_auto_pause`

| 属性 | 值 |
|------|-----|
| 类型 | Gauge（0/1） |
| 标签 | `node_ip`, `sandbox_id` |
| 说明 | 是否允许访问公网 / 是否超时自动暂停。恒上报 0 或 1 |
| 数据源 | `SandboxConfig.allow_internet_access` / `auto_pause`（B 类，deprecated） |
| 示例 | `e2b_sandbox_allow_internet{...} 1` / `e2b_sandbox_auto_pause{...} 0` |

> 注：deprecated 来源下 `false` 与「字段未填充」在 wire 上无法区分，统一记 0。若全集群此指标恒为 0，先怀疑是 B 类字段停填（见顶部 issue #13）而非配置真的全关。

### 11. `e2b_sandbox_auto_resume_timeout_seconds`

| 属性 | 值 |
|------|-----|
| 类型 | Gauge |
| 标签 | `node_ip`, `sandbox_id`, `policy` |
| 说明 | 自动恢复超时秒数；`policy` 标签携带恢复策略（如 `off` / `any`）。仅当沙箱携带 `auto_resume` 子消息时上报 |
| 数据源 | `SandboxAutoResumeConfig.timeout_seconds` + `policy`（B 类，deprecated） |
| 示例 | `e2b_sandbox_auto_resume_timeout_seconds{sandbox_id="...",policy="any"} 300` |

## Orchestrator 探活指标

### 12. `e2b_orchestrator_reachable`

| 属性 | 值 |
|------|-----|
| 类型 | Gauge |
| 标签 | `node_ip` |
| 说明 | 本次 scrape 时本机 orchestrator gRPC `SandboxService.List` 是否成功返回，1=可达 0=不可达 |
| 示例 | `e2b_orchestrator_reachable{node_ip="10.0.0.1"} 1` |

> 默认地址 `127.0.0.1:9090`，可用 `ORCHESTRATOR_ADDR` 覆盖。**dev/prod 都是 9090**，旧文档里写的 5008 已经废弃。

### 13. `e2b_orchestrator_list_duration_seconds`

| 属性 | 值 |
|------|-----|
| 类型 | Gauge |
| 标签 | `node_ip` |
| 说明 | 上次 `SandboxService.List` 的端到端时延（dial + invoke + parse），仅成功时更新 |
| 示例 | `e2b_orchestrator_list_duration_seconds{node_ip="10.0.0.1"} 0.043` |

**告警**：长期 > 0.5s 通常意味着 orchestrator 内部阻塞或沙箱列表过大，参见 `e2b-orchestrator-list-slow`。

## 节点容量指标

### 14. `e2b_node_hugepages_total` / `_free` / `_reserved`

| 属性 | 值 |
|------|-----|
| 类型 | Gauge |
| 标签 | `node_ip`, `size_bytes` |
| 说明 | HugeTLB 池总数 / 空闲数 / reserved 数（每页大小一组） |
| 数据源 | `/sys/kernel/mm/hugepages/hugepages-<N>kB/{nr,free,resv}_hugepages` |
| 示例 | `e2b_node_hugepages_free{node_ip="10.0.0.1",size_bytes="2097152"} 200` |

**为什么必要**：HugeTLB 是 e2b 的硬瓶颈。`e2b_fc_process_memory_hugetlb_bytes` 只能告诉你"已分配的页"，不知道"还能分配多少"。`(total - free)` 是已用，`free` 是新沙箱可用。HugeTLB 用尽时新沙箱启动直接失败，**没有这个指标会把"启动失败"误认为是 orchestrator bug**。

**告警示例**：

```prometheus
# 使用率 > 90% 持续 5min
(e2b_node_hugepages_total - e2b_node_hugepages_free) / e2b_node_hugepages_total > 0.9
  and e2b_node_hugepages_total > 0
```

参见 `e2b-node-hugepages-saturated`。

### 15. `e2b_node_disk_free_bytes` / `_total_bytes`

| 属性 | 值 |
|------|-----|
| 类型 | Gauge |
| 标签 | `node_ip`, `path` |
| 说明 | 给定路径所在文件系统的可用 / 总字节数（`statfs(2)` 的 `f_bavail × f_bsize` / `f_blocks × f_bsize`） |
| 数据源 | `syscall.Statfs` |
| 示例 | `e2b_node_disk_free_bytes{node_ip="10.0.0.1",path="/mnt/nfs"} 8.5e+12` |

**默认监控路径**：`/`（rootfs）和 `/mnt/nfs`（e2b 模板共享盘）。可用 `NODE_DISK_PATHS=/path1:/path2`（冒号分隔）覆盖。

**为什么用 `statfs` 而不是依赖 node_exporter 的 `node_filesystem_avail_bytes`**：

- node_exporter 默认会过滤 NFS 挂载（`--collector.filesystem.fs-types-exclude` 包含 `nfs.*`），需要单独配置。
- 我们只关心 e2b 关键路径，单独导出更直观，告警 expr 不必处理 `mountpoint` label 拼接。

**注意**：路径不存在的节点会跳过这个 path，不会写假 0——避免"NFS 没挂载"被误读成"NFS 写满了"。

## Sandbox 列表 gRPC 字段编号

`checkSandboxLeak.go` 用 protowire 直接解析，关键 tag：

| 消息 | 字段 | 编号 | 类型 | 来源类 |
|------|------|------|------|--------|
| `SandboxListResponse` | `sandboxes` | 1 | repeated message | — |
| `RunningSandbox` | `config` | 1 | message | — |
| `RunningSandbox` | `client_id` | 2 | string | A |
| `RunningSandbox` | `start_time` | 3 | google.protobuf.Timestamp | A |
| `RunningSandbox` | `end_time` | 4 | google.protobuf.Timestamp | A |
| `RunningSandbox` | `vcpu` | 8 | int64 varint | A |
| `RunningSandbox` | `ram_mb` | 9 | int64 varint | A |
| `SandboxConfig` | `template_id` | 1 | string | B |
| `SandboxConfig` | `kernel_version` | 3 | string | B |
| `SandboxConfig` | `firecracker_version` | 4 | string | B |
| `SandboxConfig` | `sandbox_id` | 6 | string | B |
| `SandboxConfig` | `envd_version` | 10 | string | B |
| `SandboxConfig` | `team_id` | 13 | string | B |
| `SandboxConfig` | `max_sandbox_length` | 14 | int64 varint（小时） | B |
| `SandboxConfig` | `total_disk_size_mb` | 15 | int64 varint（MB） | B |
| `SandboxConfig` | `auto_pause` | 18 | bool varint | B |
| `SandboxConfig` | `allow_internet_access` | 21 | bool varint | B |
| `SandboxConfig` | `auto_resume` | 24 | SandboxAutoResumeConfig message | B |
| `SandboxAutoResumeConfig` | `policy` | 1 | string | B |
| `SandboxAutoResumeConfig` | `timeout_seconds` | 2 | uint64 varint | B |
| `Timestamp` | `seconds` | 1 | int64 varint | — |

> **A = `RunningSandbox` 顶层字段（稳定）；B = `RunningSandbox.config` 子消息（上游已 deprecated，可能停填）**，见本页「沙箱配置画像指标」顶部说明。字段编号已对照 `e2b-dev/runtime` 的 `packages/shared/pkg/grpc/orchestrator/orchestrator.pb.go` 核实。升级上游时若调整 proto 字段编号，需同步改 `checkSandboxLeak.go` 中的 `tag*` 常量。

## Prometheus 查询示例

### 沙箱泄露聚合

```prometheus
# 全集群泄露总量
sum(e2b_sandbox_leak_count)

# 按节点查看 Top 10 泄露最严重的节点
topk(10, e2b_sandbox_leak_count)

# 列出所有泄露的 (node_ip, sandbox_id, pid),拿来 SSH 上去 kill
e2b_sandbox_leak == 1
```

### 业务维度联动

```prometheus
# 把每个沙箱的内存(vsize)按 team 聚合
sum by (team_id) (
  e2b_fc_process_memory_vsize_bytes
    * on(sandbox_id) group_left(team_id) e2b_sandbox_info
)

# 哪个 template 的沙箱平均存活最久
avg by (template_id) (
  e2b_sandbox_age_seconds
    * on(sandbox_id) group_left(template_id) e2b_sandbox_info
)
```

### TTL 超限的沙箱

```prometheus
# 已超 max_sandbox_length 的沙箱
e2b_sandbox_overrun_seconds > 0

# 超过 1 小时的"赖活"沙箱
e2b_sandbox_overrun_seconds > 3600
```

### 节点容量水位

```prometheus
# HugeTLB 使用率(每节点每页大小)
1 - (e2b_node_hugepages_free / e2b_node_hugepages_total)

# 各节点 NFS 剩余空间
e2b_node_disk_free_bytes{path="/mnt/nfs"}
```

## 告警规则示例

完整定义见 [`grafana/alerts/firecracker.yaml`](../../grafana/alerts/firecracker.yaml)。

| 告警 uid | 触发条件 | 严重度 |
|---------|---------|--------|
| `e2b-orchestrator-unreachable` | `e2b_orchestrator_reachable == 0` 持续 5m | critical |
| `e2b-orchestrator-list-slow` | `e2b_orchestrator_list_duration_seconds > 0.5` 持续 5m | warning |
| `e2b-node-hugepages-saturated` | HugeTLB 使用率 > 90% 持续 5m | critical |
| `e2b-node-disk-saturated` | 磁盘使用率 > 90% 持续 10m | warning |
| `e2b-sandbox-overrun` | `e2b_sandbox_overrun_seconds > 0` 持续 5m | warning |
| `e2b-sandbox-too-old` | `e2b_sandbox_age_seconds > 4h` 持续 5m | info |
| `e2b-fc-zombie` | `e2b_fc_process_state_count{state="Z"} > 0` 持续 5m | warning |
| `e2b-fc-uninterruptible` | `e2b_fc_process_state_count{state="D"} > 0` 持续 5m | warning |

## 运维 SOP

### 出现 leak

1. Grafana 看 `E2B / Sandbox Leak Detection` dashboard，按 `node_ip` 钻取。
2. 查询 `e2b_sandbox_leak{node_ip="X"}` 拿到 (sandbox_id, pid)。
3. SSH 到节点：
   ```bash
   cat /proc/<pid>/cmdline   # 二次确认是 firecracker、sandbox_id 匹配
   kill <pid>                # 优雅退出;若进入 D 状态则只能等 IO 解除
   ```
4. 持续触发同一节点 → 软隔离该 client（不接新沙箱），等流量退干净后重启 orchestrator。

### 出现 orphan

1. 拿 `e2b_sandbox_orphan{node_ip="X"}` 中的 `sandbox_id`。
2. 走 e2b 集群 admin API 强 kill：
   ```bash
   POST /admin/teams/{teamID}/sandboxes/{sandboxID}/kill
   ```
   `team_id` 可从 `e2b_sandbox_info` 获取。
3. 若 admin API 也失败，直接清 orchestrator 内部状态（详见 e2b/infra 运维手册）。

### Orchestrator 不可达

1. 先看 `e2b_orchestrator_list_duration_seconds` 历史趋势，区分"突然断"还是"逐渐变慢"。
2. SSH 到节点 `nomad job status orchestrator` / `journalctl -u nomad -f` 看 orchestrator 是否 panic / OOM。
3. 该节点的 leak/orphan 数据这段时间不可信，**不要据此 kill 任何进程**。

## 配置

| 环境变量 | 默认值 | 说明 |
|---------|--------|------|
| `ORCHESTRATOR_ADDR` | `127.0.0.1:9090` | 本机 orchestrator gRPC 地址 |
| `DISABLE_SANDBOX_LEAK_CHECK` | 未设置 | 设为 `1` 关闭沙箱泄露检测（非 client 节点） |
| `NODE_DISK_PATHS` | `/:/mnt/nfs` | 监控的磁盘路径，冒号分隔 |
| `NODE_IP` | 自动从 eth0 取 | 标签里使用的节点 IP |

## 注意事项

1. **wire format 字段编号是隐式契约**：升级 e2b 仓库 `orchestrator.proto` 时若动了 `SandboxConfig.sandbox_id`(=6) / `team_id`(=13) / `max_sandbox_length`(=14) 这些编号，必须同步更新 `checkSandboxLeak.go` 的 `tag*` 常量。建议在 e2b PR review 中加一条"修改 proto 字段编号需同步通知 infrawaves-tools"。
2. **gRPC 地址漂移历史**：早期文档写的是 5008/5007，**实际部署 dev/prod 都是 9090**。新告警和文档已统一。
3. **`e2b_sandbox_info` 是高基数指标**：长沙箱 ID + team + template 组合，告警 expr 中不要直接 group by `sandbox_id`，应通过 `_count` 系列触发后再 join `_info` 拿明细。
4. **节点容量指标无 `sandbox_id`**：故意保持低基数，避免和 `_info` 直接相乘炸时序。
5. **DISABLE_SANDBOX_LEAK_CHECK 的副作用**：关闭沙箱泄露检测会让 `e2b_orchestrator_*` / `e2b_sandbox_*` 全部不上报，但 `e2b_node_*` 与 `e2b_fc_process_*` 不受影响。
