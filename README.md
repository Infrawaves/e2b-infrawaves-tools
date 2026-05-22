# e2b-infrawaves-tools

E2B 集群的运维与可观测性工具集。

## 目录

| 路径 | 说明 |
| --- | --- |
| [nomad-nodeJob-exporter/](nomad-nodeJob-exporter/) | Prometheus exporter,采集节点 Nomad allocation + Firecracker 进程指标 |
| [scheduling_monitor/](scheduling_monitor/) | 沙箱 → 物理节点映射的离线分析工具 |
| [grafana/](grafana/) | Grafana dashboard / datasource / alert 的 IaC 定义 |
| [scripts/](scripts/) | 节点装机和升级脚本 |
| [docs/](docs/) | 设计文档(可观测性路线图、私仓发布方案) |

## 一键安装 / 升级

install 脚本已兼容升级:检测到现存安装时自动走升级路径,会保留 systemd unit 中已有的 `NOMAD_TOKEN`,只刷新二进制并 restart。

```bash
curl -fsSL https://raw.githubusercontent.com/Infrawaves/e2b-infrawaves-tools/main/scripts/install-nomad-nodeJob-exporter.sh | sudo bash
```

> 🇨🇳 国内节点 `raw.githubusercontent.com` 拉取较慢,可改用 jsdelivr CDN(等价):
>
> ```bash
> curl -fsSL https://cdn.jsdelivr.net/gh/Infrawaves/e2b-infrawaves-tools@main/scripts/install-nomad-nodeJob-exporter.sh | sudo bash
> ```

⚠️ `upgrade-nomad-nodeJob-exporter.sh` 已废弃,功能并入 install。脚本仍保留以兼容旧文档,首行会打印提示走 install。

## 文档

- [可观测性路线图](docs/OBSERVABILITY.md) — 现有指标覆盖、与 e2b_val 内置 OTel 指标的对比、未来计划。
- [私仓发布与一键安装方案](docs/RELEASE.md) — token / 内网镜像 / 双仓三种方案对比。
- [Grafana 配置管理](grafana/README.md) — dashboards、datasources、alerts 的 GitOps 流程。
- [nomad-nodeJob-exporter 详细说明](nomad-nodeJob-exporter/readme.md)
