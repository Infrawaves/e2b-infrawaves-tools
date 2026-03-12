# E2B Firecracker 进程指标设计文档

## 概述

本文档描述了用于监控 E2B 环境中 **Firecracker 进程** 的 Prometheus 指标设计。这些指标通过 `nomad-nodeJob-exporter` 在每个节点上运行，从宿主机操作系统视角采集 Firecracker 进程的系统级数据。

### 架构定位

本指标系统严格遵循 **"系统进程视角"** 的设计原则，专注于从底层宿主机 `/proc` 文件系统采集纯粹的进程级数据。这为未来 **"业务沙箱视角"** 的指标（如 `e2b_sandbox_*` 系列，由控制面或沙箱内部 Agent 暴露）预留了清晰的设计空间。

| 视角层级 | 指标前缀 | 数据源 | 典型维度 |
|---------|---------|-------|---------|
| **系统进程视角** | `e2b_fc_process_*` | 宿主机 `/proc/<pid>/` | `node_ip`, `sandbox_id`, `pid` |
| 业务沙箱视角（未来） | `e2b_sandbox_*` | E2B 控制面 / 沙箱内 Agent | `sandbox_id`, `language`, `api_endpoint` |

**这种分离设计的核心价值**：当业务层报警（如 sandbox 响应延迟高）时，可以联动钻取到进程层指标（如 `e2b_fc_process_context_switches_total`）来诊断是否是宿主机资源争抢导致的。

## 设计原则

1. **单节点 Exporter 模式**：每个节点独立运行 exporter，避免单点故障
2. **语义分层**：明确区分进程层指标与业务层指标，避免未来混淆
3. **进程追踪能力**：每个指标携带 `pid`，支持排查僵尸进程或进程重启
4. **系统级数据源**：直接读取 `/proc/<pid>/` 文件系统，确保数据准确性和实时性

## 标签设计

所有进程指标使用统一的标签维度：

| 标签名 | 类型 | 说明 | 示例 |
|-------|------|------|------|
| `node_ip` | string | 裸金属宿主机 IP 地址 | `10.12.1.252` |
| `sandbox_id` | string | 从进程启动参数提取的业务标识 | `dddklj7hkiqc8biw6ezy` |
| `pid` | int | 宿主机上的真实进程 ID | `12345` |

## 指标定义

### 1. `e2b_fc_process_count`

| 属性 | 值 |
|------|-----|
| 类型 | Gauge |
| 标签 | `node_ip` |
| 说明 | 当前节点运行的 Firecracker 进程总数 |
| 数据源 | 进程列表统计 |
| 示例 | `e2b_fc_process_count{node_ip="10.12.1.252"} 45` |

**用途**：
- 监控每个节点上的 Firecracker 实例数量
- 追踪节点负载趋势
- 用于告警：当某个节点的 Firecracker 进程数超过阈值时触发告警

---

### 2. `e2b_fc_process_parse_errors_total`

| 属性 | 值 |
|------|-----|
| 类型 | Gauge |
| 标签 | `node_ip` |
| 说明 | 累计的 sandbox_id 解析失败次数（非 E2B 格式的 Firecracker 进程） |
| 数据源 | 命令行解析异常检测 |
| 示例 | `e2b_fc_process_parse_errors_total{node_ip="10.12.1.252"} 3` |

**用途**：
- 统计无法解析 sandbox_id 的 Firecracker 进程数量
- 识别非标准启动参数的 Firecracker 实例
- 用于告警：当解析错误数量持续增长时，可能存在启动参数变更或异常进程
- 确保 `e2b_fc_process_info` 等指标中的 `sandbox_id` 都是真实有效的

**告警示例**：
```prometheus
# 解析错误速率告警
rate(e2b_fc_process_parse_errors_total[5m]) > 0.1
```

---

### 3. `e2b_fc_process_info`

| 属性 | 值 |
|------|-----|
| 类型 | Gauge |
| 标签 | `node_ip`, `sandbox_id`, `pid` |
| 说明 | 进程基础信息映射（值固定为 1，用于标签查询） |
| 数据源 | 进程 `cmdline` |
| 示例 | `e2b_fc_process_info{node_ip="10.12.1.252",sandbox_id="dddklj7hkiqc8biw6ezy",pid="12345"} 1` |

**用途**：
- 提供进程到 sandbox_id 的映射查询能力
- 支持通过标签联动关联不同维度的指标
- 用于排查特定 sandbox 对应的 pid 信息

---

### 4. `e2b_fc_process_memory_rss_bytes`

| 属性 | 值 |
|------|-----|
| 类型 | Gauge |
| 标签 | `node_ip`, `sandbox_id`, `pid` |
| 说明 | 进程常驻物理内存大小（Resident Set Size） |
| 数据源 | `/proc/<pid>/statm`（第 2 列 × PageSize） |
| 示例 | `e2b_fc_process_memory_rss_bytes{...} 1073741824` |

**用途**：
- 监控单个 Firecracker 实例的内存占用
- 识别内存泄漏或异常增长的实例
- 配合节点级内存指标分析内存分布

---

### 5. `e2b_fc_process_cpu_seconds_total`

| 属性 | 值 |
|------|-----|
| 类型 | Gauge |
| 标签 | `node_ip`, `sandbox_id`, `pid`, `mode` |
| 说明 | 进程累计消耗的 CPU 时间 |
| 数据源 | `/proc/<pid>/stat`（utime/stime） |
| mode 值 | `user` / `system` |
| 示例 | `e2b_fc_process_cpu_seconds_total{...,mode="user"} 1234.56` |

**用途**：
- 追踪进程 CPU 使用历史
- 计算 CPU 使用率：`rate(e2b_fc_process_cpu_seconds_total[1m])`
- 分析用户态与内核态 CPU 时间占比

---

### 6. `e2b_fc_process_uptime_seconds`

| 属性 | 值 |
|------|-----|
| 类型 | Gauge |
| 标签 | `node_ip`, `sandbox_id`, `pid` |
| 说明 | 进程从启动到当前的运行时长（秒） |
| 数据源 | `/proc/<pid>/stat`（启动时间与当前时间差） |
| 示例 | `e2b_fc_process_uptime_seconds{...} 345.2` |

**用途**：
- 监控单个沙箱的运行时长
- 识别长时间运行的实例（可能需要清理）
- 分析沙箱生命周期分布

---

### 7. `e2b_fc_process_threads`

| 属性 | 值 |
|------|-----|
| 类型 | Gauge |
| 标签 | `node_ip`, `sandbox_id`, `pid` |
| 说明 | 进程包含的线程数 |
| 数据源 | `/proc/<pid>/status` 中的 `Threads` 字段 |
| 示例 | `e2b_fc_process_threads{...} 8` |

**用途**：
- 监控进程的线程数量
- 识别异常线程增长的进程
- 配合 CPU 指标分析并发程度

---

### 8. `e2b_fc_process_open_fds`

| 属性 | 值 |
|------|-----|
| 类型 | Gauge |
| 标签 | `node_ip`, `sandbox_id`, `pid` |
| 说明 | 进程当前打开的文件描述符数量 |
| 数据源 | `/proc/<pid>/fd/` 目录文件数 |
| 示例 | `e2b_fc_process_open_fds{...} 256` |

**用途**：
- 监控文件描述符使用情况
- 预警接近系统 limit（`ulimit -n`）的进程
- 诊断文件描述符泄漏

---

### 9. `e2b_fc_process_io_bytes_total`

| 属性 | 值 |
|------|-----|
| 类型 | Gauge |
| 标签 | `node_ip`, `sandbox_id`, `pid`, `operation` |
| 说明 | 进程累计的磁盘 I/O 字节数 |
| 数据源 | `/proc/<pid>/io` 中的 `read_bytes` / `write_bytes` |
| operation 值 | `read` / `write` |
| 示例 | `e2b_fc_process_io_bytes_total{...,operation="read"} 1048576` |

**用途**：
- 追踪进程磁盘 I/O 使用历史
- 计算 I/O 速率：`rate(e2b_fc_process_io_bytes_total[1m])`
- 识别 I/O 密集型实例

---

### 10. `e2b_fc_process_io_ops_total`

| 属性 | 值 |
|------|-----|
| 类型 | Gauge |
| 标签 | `node_ip`, `sandbox_id`, `pid`, `operation` |
| 说明 | 进程累计的磁盘 I/O 操作次数 |
| 数据源 | `/proc/<pid>/io` 中的 `read_count` / `write_count` |
| operation 值 | `read` / `write` |
| 示例 | `e2b_fc_process_io_ops_total{...,operation="read"} 1024` |

**用途**：
- 追踪 I/O 操作频率
- 计算平均 I/O 大小：`rate(bytes) / rate(count)`
- 识别频繁小 I/O 的性能瓶颈

---

### 11. `e2b_fc_process_context_switches_total`

| 属性 | 值 |
|------|-----|
| 类型 | Gauge |
| 标签 | `node_ip`, `sandbox_id`, `pid`, `type` |
| 说明 | 进程上下文切换累计次数 |
| 数据源 | `/proc/<pid>/status` 中的 `voluntary_ctxt_switches` / `nonvoluntary_ctxt_switches` |
| type 值 | `voluntary` / `involuntary` |
| 示例 | `e2b_fc_process_context_switches_total{...,type="involuntary"} 5000` |

**用途**：
- **诊断 CPU 争抢**：`involuntary` 切换激增表示进程被操作系统强制调度，说明宿主机 CPU 争抢严重
- 分析进程调度行为
- 配合 CPU 使用率指标深入诊断性能问题

## Sandbox ID 提取逻辑

Sandbox ID 从 Firecracker 进程的命令行参数中提取：

### 命令行示例

```
/fc-versions/v1.12.1_a41d3fb/firecracker --api-sock /tmp/fc-idddklj7hkiqc8biw6ezy-lhhgt23sy3fjwy7zb9mc.sock
```

### 提取规则

1. 在命令行中查找 `--api-sock` 参数
2. 获取参数后的 socket 路径
3. 从 socket 路径中提取 sandbox_id

**Socket 路径格式**：`/tmp/fc-{sandboxID}-{randomID}.sock`

**正则表达式**：`/fc-([^-]+)-[^/]*\.sock`

**提取结果**：
- `sandboxID` = `dddklj7hkiqc8biw6ezy`
- `randomID` = `lhhgt23sy3fjwy7zb9mc`

### 解析失败处理

如果无法从命令行中提取 sandbox_id（例如格式不符合预期），则**跳过该进程的详细指标上报**，仅累计解析错误计数器。这样可以确保所有上报的 `sandbox_id` 都是真实有效的，便于后续数据分析。

```go
if sandboxID == "" {
    // 仅增加解析错误计数，跳过该进程
    e2b_fc_process_parse_errors_total.WithLabelValues(node_ip).Inc()
    continue
}
```

同时，新增一个指标来跟踪解析失败的进程数量（见下方指标定义表）。

## Prometheus 查询示例

### 查询所有节点的 Firecracker 进程总数

```prometheus
sum(e2b_fc_process_count)
```

### 查询单个节点的 Firecracker 进程数

```prometheus
e2b_fc_process_count{node_ip="10.12.1.252"}
```

### 查询运行时间超过 1 小时的沙箱

```prometheus
e2b_fc_process_uptime_seconds > 3600
```

### 计算平均沙箱运行时长

```prometheus
avg(e2b_fc_process_uptime_seconds)
```

### 查询每个节点最老的 5 个沙箱

```prometheus
topk(5, e2b_fc_process_uptime_seconds) by (node_ip)
```

### 计算进程 CPU 使用率

```prometheus
# 进程级 CPU 使用率
rate(e2b_fc_process_cpu_seconds_total{mode="user"}[1m]) +
rate(e2b_fc_process_cpu_seconds_total{mode="system"}[1m])
```

### 查询高内存占用的沙箱（Top 10）

```prometheus
topk(10, e2b_fc_process_memory_rss_bytes)
```

### 诊断 CPU 争抢（非自愿上下文切换激增）

```prometheus
# 非自愿上下文切换速率
rate(e2b_fc_process_context_switches_total{type="involuntary"}[5m]) > 100
```

### 业务层到进程层的联动分析（未来示例）

假设未来有业务层指标 `e2b_sandbox_request_latency_seconds`，当响应延迟异常高时，联动排查：

```prometheus
# 1. 找出高延迟的 sandbox
high_latency_sandboxes = e2b_sandbox_request_latency_seconds > 1

# 2. 联动查看这些 sandbox 的进程层指标
involuntary_switches = rate(e2b_fc_process_context_switches_total{type="involuntary"}[5m])

# 3. 通过 sandbox_id Join 分析结果
high_latency_with_switches = high_latency_sandboxes
  * on(sandbox_id) group_left(involuntary_switches)
    (involuntary_switches > 100)
```

## 告警规则示例

```yaml
groups:
  - name: e2b_fc_process
    interval: 30s
    rules:
      # 节点进程数过高告警
      - alert: HighFirecrackerProcessCount
        expr: e2b_fc_process_count > 100
        for: 5m
        labels:
          severity: warning
        annotations:
          summary: "节点 {{ $labels.node_ip }} 上的 Firecracker 进程数过高"
          description: "节点 {{ $labels.node_ip }} 有 {{ $value }} 个 Firecracker 进程运行"

      # 长时间运行沙箱告警
      - alert: LongRunningSandbox
        expr: e2b_fc_process_uptime_seconds > 86400
        for: 10m
        labels:
          severity: info
        annotations:
          summary: "发现长时间运行的沙箱"
          description: "沙箱 {{ $labels.sandbox_id }} 在节点 {{ $labels.node_ip }} 上已运行超过 24 小时，PID: {{ $labels.pid }}"

      # 内存占用过高告警
      - alert: HighMemoryUsageSandbox
        expr: e2b_fc_process_memory_rss_bytes > 4294967296  # 4GB
        for: 5m
        labels:
          severity: warning
        annotations:
          summary: "沙箱内存占用过高"
          description: "沙箱 {{ $labels.sandbox_id }} (PID: {{ $labels.pid }}) 内存占用超过 4GB: {{ $value | humanize }}B"

      # 文件描述符泄漏告警
      - alert: HighOpenFds
        expr: e2b_fc_process_open_fds > 1000
        for: 10m
        labels:
          severity: warning
        annotations:
          summary: "进程打开过多文件描述符"
          description: "进程 {{ $labels.pid }} (sandbox: {{ $labels.sandbox_id }}) 打开了 {{ $value }} 个文件描述符"

      # CPU 争抢告警（非自愿上下文切换激增）
      - alert: HighCPUContention
        expr: rate(e2b_fc_process_context_switches_total{type="involuntary"}[5m]) > 100
        for: 5m
        labels:
          severity: warning
        annotations:
          summary: "检测到 CPU 争抢"
          description: "进程 {{ $labels.pid }} (sandbox: {{ $labels.sandbox_id }}) 非自愿上下文切换速率过高 ({{ $value }}/s)，说明宿主机 CPU 争抢严重"
```

## 实现细节

### 文件位置

- 指标实现：`checkFirecracker.go`
- 主程序：`main.go`

### 采集逻辑

1. **进程发现**：扫描 `/proc` 目录，识别所有 Firecracker 进程（通过 `cmdline` 或进程名判断）
2. **基本信息采集**：从 `/proc/<pid>/cmdline` 提取 sandbox_id，记录进程基本信息
3. **系统指标采集**：对每个进程，读取对应的 `/proc` 文件：
   - `/proc/<pid>/stat` - CPU 时间、启动时间
   - `/proc/<pid>/statm` - 内存统计
   - `/proc/<pid>/status` - 线程数、上下文切换
   - `/proc/<pid>/io` - I/O 统计
   - `/proc/<pid>/fd/` - 文件描述符数
4. **指标更新**：更新所有 Prometheus Gauge/Counter 指标

### 数据源参考

| 数据文件 | 字段 | 用途 |
|---------|------|------|
| `/proc/<pid>/stat` | utime, stime, start_time | CPU 使用、运行时长 |
| `/proc/<pid>/statm` | rss (第2列) | 内存占用 |
| `/proc/<pid>/status` | Threads, voluntary_ctxt_switches, nonvoluntary_ctxt_switches | 线程数、上下文切换 |
| `/proc/<pid>/io` | read_bytes, write_bytes, read_count, write_count | I/O 统计 |
| `/proc/<pid>/fd/` | 目录项数 | 打开的文件描述符数 |

### 内存计算

RSS（Resident Set Size）需要乘以系统页面大小：

```go
pageSize := os.Getpagesize()  // 通常为 4096 (4KB)
rssBytes := rssPages * pageSize
```

## 与现有指标的关系

本导出器同时提供以下指标类别：

| 指标类别 | 文件 | 指标前缀 | 说明 |
|---------|------|---------|------|
| Nomad Allocation | `main.go` | `nomad_alloc_*` | 任务分配状态和资源使用 |
| Node Role | `main.go` | `node_role_*` | 节点角色信息 |
| E2B Firecracker Process | `checkFirecracker.go` | `e2b_fc_process_*` | Firecracker 进程系统监控（本文档） |

## 注意事项

1. **标签基数控制**：
   - `sandbox_id` 在单节点模式下基数可控
   - 避免在跨节点聚合查询时直接使用高基数标签作为 group key
   - 建议使用 `by (node_ip)` 等低基数维度进行聚合

2. **数据准确性**：
   - 直接读取 `/proc` 文件系统，数据实时准确
   - 不会出现 `ps` 命令可能的字段解析问题

3. **更新频率**：
   - 指标在每次 Prometheus 抓取时更新（由 `scrape_interval` 控制）
   - Counter 类型指标（CPU 时间、I/O、上下文切换）会累计增长

4. **进程生命周期**：
   - 进程退出时，对应的指标会自然消失（下次抓取不再上报）
   - 如果需要追踪进程退出事件，需要额外的机制

5. **权限要求**：
   - 读取 `/proc/<pid>/` 需要 CAP_SYS_PTRACE 能力或 root 杭限
   - 确保 exporter 运行在适当的权限下

## 架构演进建议

### 阶段一：当前（进程层指标）

本设计实现的纯进程层监控，聚焦于系统资源使用情况。

### 阶段二：未来（业务层指标）

建议在未来 E2B 控制面或沙箱内实现 `e2b_sandbox_*` 系列指标：

| 指标 | 说明 |
|------|------|
| `e2b_sandbox_request_duration_seconds` | 请求延迟 |
| `e2b_sandbox_requests_total` | 请求计数 |
| `e2b_sandbox_errors_total` | 错误计数 |
| `e2b_sandbox_active_connections` | 活跃连接数 |
| `e2b_sandbox_language` | 执行语言（标签） |

### 联动监控示例

通过 `on(sandbox_id) group_left` 将业务层与进程层指标关联，实现全链路诊断。
