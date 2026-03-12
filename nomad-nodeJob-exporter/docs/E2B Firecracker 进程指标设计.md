# E2B Firecracker 指标设计文档

## 概述

本文档描述了用于监控 E2B 环境中 Firecracker 进程的 Prometheus 指标设计。这些指标通过 `nomad-nodeJob-exporter` 在每个节点上运行，用于追踪 Firecracker 虚拟机实例的数量和运行状态。

## 设计原则

1. **单节点 Exporter 模式**：每个节点独立运行 exporter，避免单点故障
2. **低基数标签**：避免高基数标签（如唯一 ID 的无限增长）导致的性能问题
3. **语义清晰**：指标名称和标签设计符合 Prometheus 最佳实践

## 指标定义

### `firecracker_process_total`

| 属性 | 值 |
|------|-----|
| 类型 | Gauge |
| 标签 | 无 |
| 说明 | 当前节点运行的 Firecracker 进程总数 |
| 示例 | `firecracker_process_total 45` |

**用途**：
- 监控每个节点上的 Firecracker 实例数量
- 追踪节点负载趋势
- 用于告警：当某个节点的 Firecracker 进程数超过阈值时触发告警

### `firecracker_uptime_seconds`

| 属性 | 值 |
|------|-----|
| 类型 | Gauge |
| 标签 | `sandbox_id` |
| 说明 | 每个 Firecracker 实例的运行时长（秒） |
| 示例 | `firecracker_uptime_seconds{sandbox_id="dddklj7hkiqc8biw6ezy"} 345.2` |

**用途**：
- 监控单个沙箱的运行时长
- 识别长时间运行的实例（可能需要清理）
- 分析沙箱生命周期分布

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

### Fallback 机制

如果无法从命令行中提取 sandbox_id（例如格式不符合预期），则使用进程 PID 作为 fallback：

```go
if sandboxID == "" {
    sandboxID = pid
}
```

## Prometheus 查询示例

### 查询所有节点的 Firecracker 进程总数

```prometheus
sum(firecracker_process_total)
```

### 查询单个节点的 Firecracker 进程数

```prometheus
firecracker_process_total{instance="10.12.1.252:9106"}
```

### 查询运行时间超过 1 小时的沙箱

```prometheus
firecracker_uptime_seconds > 3600
```

### 计算平均沙箱运行时长

```prometheus
avg(firecracker_uptime_seconds)
```

### 查询每个节上最老的 5 个沙箱

```prometheus
topk(5, firecracker_uptime_seconds) by (instance)
```

## 告警规则示例

```yaml
groups:
  - name: firecracker
    interval: 30s
    rules:
      # - alert: HighFirecrackerProcessCount
      #   expr: firecracker_process_total > 100
      #   for: 5m
      #   labels:
      #     severity: warning
      #   annotations:
      #     summary: "节点 {{ $labels.instance }} 上的 Firecracker 进程数过高"
      #     description: "节点 {{ $labels.instance }} 有 {{ $value }} 个 Firecracker 进程运行"

      # - alert: LongRunningSandbox
      #   expr: firecracker_uptime_seconds > 86400
      #   for: 10m
      #   labels:
      #     severity: info
      #   annotations:
      #     summary: "发现长时间运行的沙箱"
      #     description: "沙箱 {{ $labels.sandbox_id }} 在 {{ $labels.instance }} 上已运行超过 24 小时"
```

## 实现细节

### 文件位置

- 指标实现：`checkFirecracker.go`
- 主程序：`main.go`

### 采集逻辑

1. 执行 `ps aux` 命令获取进程列表
2. 过滤包含 "firecracker" 的进程行
3. 解析每行的字段：
   - `PID`: 进程 ID（用于 fallback）
   - `ELAPSED`: 进程运行时长
   - `COMMAND`: 完整命令行（用于提取 sandbox_id）
4. 解析运行时长为秒数
5. 从命令行提取 sandbox_id
6. 更新 Prometheus 指标

### 支持的时间格式

`parsePsTime()` 函数支持多种 `ps aux` 输出的时间格式：

- `MM:SS` 或 `MM:SS.mmm`（分钟:秒）
- `HH:MM:SS`（小时:分钟:秒）
- `DD-HH:MM`（天-小时:分钟）

## 与现有指标的关系

本导出器同时提供以下指标类别：

| 指标类别 | 文件 | 说明 |
|---------|------|------|
| Nomad Allocation | `main.go` | 任务分配状态和资源使用 |
| Node Role | `main.go` | 节点点角色信息 |
| E2B Firecracker | `checkFirecracker.go` | Firecracker 进程监控（本文档） |

## 注意事项

1. **标签基数控制**：`sandbox_id` 在单节点模式下基数可控，但应避免跨节点聚合使用
2. **进程状态**：当前实现只跟踪进程的存在和运行时长，不追踪进程状态（running/stopped）
3. **更新频率**：指标在每次 Prometheus 抓取时更新（由 Prometheus 的 scrape_interval 控制）
