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

## 一键安装(公仓场景)

```bash
curl -fsSL https://raw.githubusercontent.com/Infrawaves/e2b-infrawaves-tools/main/scripts/install-nomad-nodeJob-exporter.sh | sudo bash
```

## 一键安装(私仓场景)

仓库当前是私仓,需要用 GitHub PAT 或内网镜像。详见 [docs/RELEASE.md](docs/RELEASE.md)。

```bash
# 方式 A:GH_TOKEN 认证(节点少,运维直连 GitHub)
curl -fsSL -H "Authorization: Bearer $GH_TOKEN" \
  https://raw.githubusercontent.com/Infrawaves/e2b-infrawaves-tools/main/scripts/install-nomad-nodeJob-exporter.sh \
  | sudo GH_TOKEN=$GH_TOKEN bash

# 方式 B:内网镜像
curl -fsSL http://mirror.internal/install-nomad-nodeJob-exporter.sh \
  | sudo MIRROR_URL=http://mirror.internal/binaries/nomad-nodeJob-exporter-linux-amd64 bash
```

## 升级

```bash
curl -fsSL https://raw.githubusercontent.com/Infrawaves/e2b-infrawaves-tools/main/scripts/upgrade-nomad-nodeJob-exporter.sh | sudo bash
# 私仓同样支持 GH_TOKEN / MIRROR_URL
```

## 文档

- [可观测性路线图](docs/OBSERVABILITY.md) — 现有指标覆盖、与 e2b_val 内置 OTel 指标的对比、未来计划。
- [私仓发布与一键安装方案](docs/RELEASE.md) — token / 内网镜像 / 双仓三种方案对比。
- [Grafana 配置管理](grafana/README.md) — dashboards、datasources、alerts 的 GitOps 流程。
- [nomad-nodeJob-exporter 详细说明](nomad-nodeJob-exporter/readme.md)
