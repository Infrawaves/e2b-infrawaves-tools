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

install 脚本已兼容升级:检测到现存安装时自动走升级路径,会保留 systemd unit 中已有的 `NOMAD_TOKEN`,只刷新二进制并 restart。脚本默认以 root 跑(节点上本就是 root 用户)。

🌍 海外节点(直连 GitHub):

```bash
curl -fsSL https://raw.githubusercontent.com/Infrawaves/e2b-infrawaves-tools/main/scripts/install-nomad-nodeJob-exporter.sh | bash
```

🇨🇳 国内节点(`raw.githubusercontent.com` 慢/不通,走 jsdelivr CDN,内容等价):

```bash
curl -fsSL https://cdn.jsdelivr.net/gh/Infrawaves/e2b-infrawaves-tools@main/scripts/install-nomad-nodeJob-exporter.sh | bash
```

批量装机/全集群同时撞 GitHub API 60/h/IP 限速时,传 `GH_TOKEN`(任意 PAT,公仓 read 权限即可)提到 5000/h。先 export 一次再用:

```bash
export GH_TOKEN=ghp_xxx
curl -fsSL https://cdn.jsdelivr.net/gh/Infrawaves/e2b-infrawaves-tools@main/scripts/install-nomad-nodeJob-exporter.sh | bash
```

⚠️ `upgrade-nomad-nodeJob-exporter.sh` 已废弃,功能并入 install。脚本仍保留以兼容旧文档,首行会打印提示走 install。

## 批量装机/升级(节点多时用 Nomad sysbatch)

[nomad/install-nomad-nodejob-exporter.hcl](nomad/install-nomad-nodejob-exporter.hcl) 是一个 `sysbatch` job,在每个节点上跑一次上面的 install 脚本。在 nomad server 节点(如 dev gateway)上执行:

```bash
nomad job run \
  -var="datacenter=prod-e2b-dc" \
  -var="version_tag=$(date +%Y%m%d-%H%M)" \
  nomad/install-nomad-nodejob-exporter.hcl
```

⚠️ **`version_tag` 必须每次变**(建议用时间戳)。Nomad 判定 job spec 是否变化基于内容 hash,不变就跳过已 complete 的节点 alloc,导致升级实际没生效。

按需追加变量:

```bash
-var="gh_token=ghp_xxx"      # 撞 GitHub API 60/h/IP 限速时
-var="node_pool=default"      # 默认 default
-var="script_url=https://.../<branch>/scripts/install-nomad-nodeJob-exporter.sh"   # 测分支版本时
```

只在某台节点跑,在 hcl 里 `group "install" {` 上面加 constraint:

```hcl
constraint {
  attribute = "${node.unique.name}"
  value     = "<node-name>"
}
```

详细说明见 [nomad/README.md](nomad/README.md)。

## 文档

- [可观测性路线图](docs/OBSERVABILITY.md) — 现有指标覆盖、与 e2b_val 内置 OTel 指标的对比、未来计划。
- [私仓发布与一键安装方案](docs/RELEASE.md) — token / 内网镜像 / 双仓三种方案对比。
- [Grafana 配置管理](grafana/README.md) — dashboards、datasources、alerts 的 GitOps 流程。
- [nomad-nodeJob-exporter 详细说明](nomad-nodeJob-exporter/readme.md)
