# E2B Nomad 与端口探测指标设计文档

## 概述

本文档描述 `nomad-nodeJob-exporter` 在节点上额外采集的两类**非 e2b 业务**指标：

1. **Nomad 视角（`nomad_*`）**——通过 `nomad` CLI 在本节点抓取自身的 role、templaterole、allocation 健康与资源使用，让 Prometheus 不依赖 Nomad 自带 telemetry 也能拿到调度层信息。
2. **端口探测（`node_port_listening`）**——TCP 连接探测节点上关键端口（5016、9090）是否在监听，补强"sock 层死活"的黑盒视角。

姊妹文档：

- [E2B Firecracker 进程指标设计.md](./E2B%20Firecracker%20进程指标设计.md)（系统进程视角）
- [E2B 沙箱与节点指标设计.md](./E2B%20沙箱与节点指标设计.md)（沙箱业务 + orchestrator 探活 + 节点容量）

### 在分层观测里的位置

按客户讲解时使用的 5 层分类：

| 层 | 视角 | 本文覆盖 | 文档归属 |
|----|------|---------|---------|
| 1 | OS 容量（HugeTLB / 磁盘） |  | 沙箱与节点指标 |
| **2** | **Nomad 调度** | **`nomad_*`、`node_port_listening`** | **本文档** |
| 3 | Orchestrator 控制面 |  | 沙箱与节点指标 |
| 4 | 沙箱业务 |  | 沙箱与节点指标 |
| 5 | Firecracker 进程 |  | Firecracker 进程指标 |

> 端口探测严格说不是 Nomad 自身的指标，但它探测的端口（5016、9090）都是 Nomad 拉起的服务端口，故归到本层。

## 设计原则

1. **使用 `nomad` CLI 而非 HTTP API**：CLI 自动继承 systemd 环境变量（`NOMAD_ADDR`、`NOMAD_TOKEN`），免去 exporter 单独管理 token；代价是依赖 nomad 二进制在 `$PATH`，且 CLI 输出格式在 Nomad 版本升级时可能变化（**升级 Nomad 必做兼容性回归**）。
2. **节点本地 self 视图**：只调 `nomad node status -self`，每个节点只汇报自己 allocate 的任务，跨节点汇总交给 Prometheus 做聚合。避免每个 exporter 都去全集群拉数据，浪费 Nomad API 配额。
3. **端口探测是黑盒补强**：`e2b_orchestrator_reachable` 是白盒（gRPC 连得通且能 invoke 业务方法），`node_port_listening{port="9090"}` 是黑盒（TCP listen 即视为存活）。**两者同时出现 0 ≠ 同一种故障**——见 [与 `e2b_orchestrator_reachable` 的差异](#与-e2b_orchestrator_reachable-的差异)。
4. **能力检查随 role 变**：不同节点角色（api / orchestrator / template-manager）期望的 allocation 集合不同，本工具内置了角色 → 必需服务的映射；新增角色或新增服务都要改这里。

## 标签设计

| 标签 | 类型 | 说明 |
|------|------|------|
| `service` | string | task group 名（约定上等于服务名，如 `api-service`、`client-orchestrator`） |
| `node_id` | string | Nomad node ID（UUID） |
| `node_name` | string | Nomad node 名（如 `client-7`） |
| `status` | string | allocation 实际状态：`running` / `pending` / `failed` / `not_found` |
| `desired_status` | string | allocation 期望状态：`run` / `stop` / `evict` |
| `role` | string | 节点 role（来自 node Meta，如 `api`、`orchestrator`、`build`） |
| `templaterole` | string | 节点 templaterole（如 `template-manager`） |
| `allocation_id` | string | 单次分配的 UUID |
| `job_name` | string | Nomad job 名 |
| `task_name` | string | task 名（一个 task group 可有多个 task） |
| `port` | string | 探测的端口号字符串 |
| `node_ip` | string | 节点 IP（端口探测用） |

> `allocation_id` 在分配重启时会变，**会推高 Prometheus 时序基数**——这是已知技术债，参考 [OBSERVABILITY.md](../../docs/OBSERVABILITY.md) 中长期改进项。

## 指标定义

### 1. `nomad_node_role`

| 属性 | 值 |
|------|-----|
| 类型 | Gauge（值固定 1，用作标签查询） |
| 标签 | `role`, `node_id`, `node_name` |
| 说明 | 节点 role（来自 `nomad node status -self` 的 Meta 段） |
| 数据源 | `nomad` CLI |
| 示例 | `nomad_node_role{role="api",node_id="...",node_name="api-3"} 1` |

**用途**：

- 把其他指标按节点角色聚合：`count by(role) (nomad_node_role)`、配合 `on(node_id) group_left(role)` 给 fc 进程指标添 role 标签。
- 巡检集群每类节点数量是否符合预期。

### 2. `nomad_node_templaterole`

| 属性 | 值 |
|------|-----|
| 类型 | Gauge |
| 标签 | `templaterole`, `node_id`, `node_name` |
| 说明 | 节点 templaterole（独立于 role 的次维度，主要标记 `template-manager`） |
| 示例 | `nomad_node_templaterole{templaterole="template-manager",...} 1` |

**为什么和 role 分开**：在 e2b 部署里，部分节点同时承担 `role=client` + `templaterole=template-manager`，两个维度正交，合成一个标签会丢失信息。

### 3. `nomad_allocation_up`

| 属性 | 值 |
|------|-----|
| 类型 | Gauge |
| 标签 | `service`, `node_id`, `node_name`, `status`, `desired_status` |
| 说明 | 角色对应的必需服务 allocation 健康状态：1=running 0=异常或未找到 |
| 数据源 | `nomad node status -self` 的 Allocations 段 + 内置 role→service 映射 |
| 示例 | `nomad_allocation_up{service="api-service",status="running",desired_status="run",...} 1` |

**判定**：`desired_status=run AND status=running` → 1，其他 → 0。`not_found` 状态表示该 role 期望的服务在本节点上完全没有 allocation 记录。

**典型告警**：

```prometheus
# 关键服务持续 down 5 分钟
nomad_allocation_up == 0
```

### 4. `nomad_allocation_cpu_usage`

| 属性 | 值 |
|------|-----|
| 类型 | Gauge |
| 标签 | `allocation_id`, `job_name`, `task_name`, `node_id`, `node_name` |
| 说明 | allocation 实时 CPU 使用量（MHz） |
| 数据源 | `nomad alloc status -verbose <allocID>` |
| 示例 | `nomad_allocation_cpu_usage{...} 824` |

### 5. `nomad_allocation_cpu_limit`

| 属性 | 值 |
|------|-----|
| 类型 | Gauge |
| 标签 | 同上 |
| 说明 | allocation 配置的 CPU 上限（MHz） |
| 示例 | `nomad_allocation_cpu_limit{...} 2000` |

### 6. `nomad_allocation_cpu_usage_percentage`

| 属性 | 值 |
|------|-----|
| 类型 | Gauge |
| 标签 | 同上 |
| 说明 | CPU 使用率百分比 = `usage / limit * 100`（exporter 端预计算） |
| 示例 | `nomad_allocation_cpu_usage_percentage{...} 41.2` |

> 已经是 limit=0 时跳过上报这个指标，避免除零生成 `+Inf` / `NaN`。

### 7. `nomad_allocation_memory_usage`

| 属性 | 值 |
|------|-----|
| 类型 | Gauge |
| 标签 | 同上 |
| 说明 | allocation 实时内存使用量（字节） |
| 示例 | `nomad_allocation_memory_usage{...} 268435456` |

### 8. `nomad_allocation_memory_limit`

| 属性 | 值 |
|------|-----|
| 类型 | Gauge |
| 标签 | 同上 |
| 说明 | allocation 配置的内存上限（字节） |

### 9. `nomad_allocation_memory_usage_percentage`

| 属性 | 值 |
|------|-----|
| 类型 | Gauge |
| 标签 | 同上 |
| 说明 | 内存使用率百分比（同上预计算逻辑） |

### 10. `node_port_listening`

| 属性 | 值 |
|------|-----|
| 类型 | Gauge |
| 标签 | `port`, `node_ip` |
| 说明 | 端口是否在监听：1=listening 0=未监听 |
| 数据源 | `net.DialTimeout` TCP 探测 → 失败回退 `ss -tuln` → 再失败回退 `netstat -tuln` |
| 探测端口 | `5016`、`9090`（**硬编码**，要改需重新构建发版） |
| 示例 | `node_port_listening{port="9090",node_ip="10.0.0.1"} 1` |

**为什么选这两个端口**：

- `9090` — 本机 orchestrator gRPC（同 `e2b_orchestrator_reachable` 的目标）
- `5016` — 节点上 e2b API 相关服务端口

**探测多种地址**：
为兼容 IPv4-only / IPv6-only / dual-stack 三种监听方式，依次尝试 `:port`、`0.0.0.0:port`、`[::]:port`，任一成功即视为 listening。`Dial` 全失败时再退到 `ss` / `netstat` 解析输出，避免某些场景下 firewall / SO_REUSEADDR 导致 dial 误判。

## 与 `e2b_orchestrator_reachable` 的差异

两者都看 9090 端口，但语义层级不同——**同时观察可以分阶定位故障**：

| 现象 | `node_port_listening{port="9090"}` | `e2b_orchestrator_reachable` | 含义 |
|------|------------------------------------|------------------------------|------|
| 都是 1 | listening | gRPC 通且能 invoke | 健康 |
| port=1, reachable=0 | listening | invoke 失败 | **进程在但应用层卡住**（panic、死锁、依赖阻塞） |
| port=0, reachable=0 | 未监听 | 不通 | **进程没起来**（OOM、未拉起、被 nomad evict） |
| port=0, reachable=1 | 未监听 | 通 | 不可能（一致性 bug，需排查 exporter 自身） |

告警里建议两条都设：黑盒(`node_port_listening==0`)用来抓"服务没拉起"，白盒(`e2b_orchestrator_reachable==0`)用来抓"应用层卡住"。

## 角色与必需服务映射

`checkNodeAllocationStatus.go` 里硬编码的 role → 必需服务清单（**节点 Meta 决定哪些服务被巡检**）：

| Meta 字段 | 值 | 必需服务 |
|----------|----|---------|
| `role` | `api` | `api-service`、`client-proxy`、`otel-collector` |
| `role` | `orchestrator` | `client-orchestrator`、`otel-collector` |
| `templaterole` | `template-manager` | `template-manager` |

> 当前清单已比注释里的早期版本（含 `logs-collector`、`loki-service`）做过精简——历史选择是为了避免在没装日志栈的客户环境上误报。新增角色或新增必需服务，都要改这个文件并发版。

## 配置

| 环境变量 | 默认值 | 说明 |
|---------|--------|------|
| `NOMAD_ADDR` | `http://127.0.0.1:4646` | Nomad HTTP API 地址（CLI 透传） |
| `NOMAD_TOKEN` | （空） | Nomad ACL token |
| `NODE_IP` | 自动从 eth0 取 | 端口探测和 nomad 指标的 node_ip 标签 |

> **注意**：在手动管理的 gateway 类节点上跑这个 exporter，必须先 `export` `/root/.bashrc` 里的 `E2B_*` 与 `NOMAD_*` 环境变量，否则 CLI 鉴权会失败(`ssh host '...'` 是非交互 shell，不读 `.bashrc`)。

部署细节同 readme，systemd unit 自动 source `/etc/default/nomad` / `/etc/profile.d/*`。

## Prometheus 查询示例

### 集群拓扑

```prometheus
# 各角色节点数
count by (role) (nomad_node_role)

# template-manager 节点列表
nomad_node_templaterole{templaterole="template-manager"}
```

### 关键服务巡检

```prometheus
# 当前未运行的关键服务(每节点每服务一行)
nomad_allocation_up == 0

# 按服务汇总不健康节点数
count by (service) (nomad_allocation_up == 0)
```

### 资源水位

```prometheus
# 每个 allocation 的 CPU 使用率
nomad_allocation_cpu_usage_percentage

# 内存使用率 Top 10 的 allocation
topk(10, nomad_allocation_memory_usage_percentage)

# 给 fc 进程指标补上 role 标签(联动其它视角)
e2b_fc_process_count
  * on(node_id) group_left(role)
    (count by(node_id, role) (nomad_node_role))
```

### 端口探活

```prometheus
# 任一关键端口未监听的节点
node_port_listening == 0

# 9090 端口未监听 ∩ orchestrator gRPC 不可达 → 进程没拉起
(node_port_listening{port="9090"} == 0)
  and on(node_ip)
  (e2b_orchestrator_reachable == 0)
```

## 告警规则示例

完整定义见 [`grafana/alerts/firecracker.yaml`](../../grafana/alerts/firecracker.yaml) 与 `grafana/alerts/nomad.yaml`（如有）。

| 场景 | 表达式 | 严重度 |
|------|-------|-------|
| 关键服务 down | `nomad_allocation_up == 0` 持续 5m | critical |
| Allocation 抖动 | `changes(nomad_allocation_up[10m]) > 3` | warning |
| 端口未监听 | `node_port_listening == 0` 持续 5m | critical |
| 内存接近爆 | `nomad_allocation_memory_usage_percentage > 90` 持续 10m | warning |
| CPU 长时间打满 | `nomad_allocation_cpu_usage_percentage > 90` 持续 10m | warning |

> `e2b-orchestrator-unreachable`（白盒）和"端口未监听"（黑盒）是互补告警，**都不应该被合并掉**。

## 注意事项

1. **端口列表硬编码在代码里**：`checkNodeProcessPort.go` 中 `ports := []int{5016, 9090}`。要加端口须改代码并重新发版；不接受环境变量动态配置——这是有意为之，避免运行期端口列表漂移导致告警基线错位。
2. **`nomad` CLI 必须在 `$PATH`**：systemd 默认环境一般 OK，手动调试时确认 `which nomad` 能解析到。
3. **CLI 输出格式不稳定**：当前解析依赖 `nomad node status -verbose -self` 的字段顺序（task group 是第 4 列，desired 第 6 列）。Nomad 升级时需要回归这部分代码。
4. **`allocation_id` 是高基数标签**：每次 alloc 重启都会变，长期累积会拖慢 Prometheus 查询。建议生产环境用 `metric_relabel_configs` 把 `allocation_id` drop 掉，只保留 `job_name`/`task_name`，需要明细时再开。
5. **资源百分比是字符串解析的**：`checkAllocationResource.go` 解析 `nomad alloc status` 文本中的"123 MHz / 2000 MHz"格式。已知坑：早期版本会出现"0 / 0"或单位是 GiB 的 case，limit=0 时直接跳过百分比指标。
6. **本文档不覆盖 e2b 业务指标**：`nomad_*` 严格保持 Nomad 自身视角，不要在这里加 `sandbox_id` 或 `template_id`——业务维度走 [E2B 沙箱与节点指标设计](./E2B%20沙箱与节点指标设计.md) 中的 `e2b_sandbox_info` join。
