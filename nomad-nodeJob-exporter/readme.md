## 使用方式

### 服务模式（默认）
持续运行 HTTP 服务，监听 `:9106` 端口：
```bash
./nomad-nodeJob-exporter
```

### 单次模式
一次性采集指标并以 Prometheus 格式输出到 stdout：
```bash
./nomad-nodeJob-exporter -oneshot
```

## 环境变量

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `NOMAD_ADDR` | `http://127.0.0.1:4646` | Nomad HTTP API |
| `NOMAD_TOKEN` | (空) | Nomad ACL token |
| `ORCHESTRATOR_ADDR` | `127.0.0.1:9090` | 本机 e2b orchestrator gRPC,沙箱泄露检测用 |
| `DISABLE_SANDBOX_LEAK_CHECK` | (未设置) | 设为 `1` 可关闭沙箱泄露检测(例如非 client 节点) |
| `NODE_IP` | 自动取 eth0 | 标签使用的节点 IP |

## 沙箱泄露检测

每次 scrape 时,exporter 会:

1. 扫 `/proc` 得到本机所有 firecracker 进程 → sandbox_id 集合 A
2. 调用本机 orchestrator `gRPC SandboxService.List` → sandbox_id 集合 B
3. 输出三类指标:
   - `e2b_sandbox_consistent_count` — A ∩ B,正常运行
   - `e2b_sandbox_leak_count` + `e2b_sandbox_leak{sandbox_id,pid}` — A \ B,fc 进程泄露(orchestrator 不知道,占着 HugeTLB)
   - `e2b_sandbox_orphan_count` + `e2b_sandbox_orphan{sandbox_id}` — B \ A,orchestrator 状态过期(沙箱已死,记录未清)
   - `e2b_orchestrator_reachable` — 0/1,gRPC 不通时 leak/orphan 数据不刷新,避免误报

详见 [docs/OBSERVABILITY.md](../docs/OBSERVABILITY.md#沙箱泄露检测)。

## 部署步骤

### 一键安装

在目标服务器上执行以下命令，自动下载最新版本、配置 systemd 服务并启动：

```bash
curl -fsSL https://raw.githubusercontent.com/Infrawaves/e2b-infrawaves-tools/main/scripts/install-nomad-nodeJob-exporter.sh | sudo bash
```

安装脚本会自动从 `/opt/nomad/config/default.hcl` 中读取 Nomad Token。如果找不到配置文件，需要手动配置 Token 后重启服务。

### 升级

在目标服务器上执行以下命令，自动下载最新版本、备份现有版本并升级：

```bash
curl -fsSL https://raw.githubusercontent.com/Infrawaves/e2b-infrawaves-tools/main/scripts/upgrade-nomad-nodeJob-exporter.sh | sudo bash
```

升级脚本会：
- 自动备份当前版本（带时间戳）
- 下载并安装最新版本
- 重启服务并验证状态
- 如遇问题可回滚到备份版本

### 手动部署

1. 编译二进制文件
   ```bash
   <!-- GOOS=linux GOARCH=amd64 go build -o nomad-nodeJob-exporter main.go -->
   GOOS=linux GOARCH=amd64 go build -o nomad-nodeJob-exporter
   ```

2. 准备部署文件
将编译好的 nomad-nodeJob-exporter 二进制文件上传到目标服务器。我们把它放在 /opt/nomad-nodeJob-exporter/ 目录下。

# 在目标服务器上创建目录
mkdir -p /opt/nomad-nodeJob-exporter

<!-- # 上传文件后，赋予执行权限 -->
<!-- chmod +x /opt/nomad-nodeJob-exporter/nomad-nodeJob-exporter -->

3. 创建 Systemd 服务单元文件
创建一个 Systemd 配置文件来管理该服务。

vim /etc/systemd/system/nomad-nodeJob-exporter.service
```
[Unit]
Description=Nomad Service Exporter
After=network.target

[Service]
Type=simple
User=root
Group=root

# 设置重启策略
# on-failure: 表示仅在非正常退出时重启（如崩溃、信号终止等）
# always: 无论退出状态如何，都重启服务
Restart=on-failure
RestartSec=5s

WorkingDirectory=/opt/nomad-nodeJob-exporter

# 添加 Nomad 地址和 Token
Environment="NOMAD_ADDR=http://127.0.0.1:4646"
Environment="NOMAD_TOKEN=<YOUR_NOMAD_TOKEN>"

ExecStart=/opt/nomad-nodeJob-exporter/nomad-nodeJob-exporter

Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
```

4. 启动并设置开机自启
执行以下命令来重载配置、启动服务并设置开机自启：

# 1. 重载 systemd 配置
systemctl daemon-reload

# 2. 启动服务
systemctl start nomad-nodeJob-exporter

# 3. 设置开机自启
systemctl enable nomad-nodeJob-exporter

# 4. 查看服务状态
systemctl status nomad-nodeJob-exporter
如果状态显示 Active: active (running)，说明部署成功。

5. 验证服务
使用 curl 测试一下 /metrics 接口是否正常返回数据：

curl http://127.0.0.1:9106/metrics