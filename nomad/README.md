# Nomad jobs

放仓库里、不写敏感值的 Nomad job 定义。配合 `nomad job run -var=...` / `NOMAD_VAR_*` 注入环境相关参数。

## install-nomad-nodejob-exporter.hcl

`sysbatch` job，在每个节点上跑一次 `scripts/install-nomad-nodeJob-exporter.sh`,从 GitHub Releases 下载最新二进制并装好 systemd 服务。

### 用法

```bash
# 必填: datacenter
nomad job run \
  -var="datacenter=<your-dc>" \
  nomad/install-nomad-nodejob-exporter.hcl

# 全部参数
nomad job run \
  -var="datacenter=<your-dc>" \
  -var="node_pool=default" \
  -var="version_tag=2026-06-02-all" \
  -var="gh_token=ghp_xxx" \
  nomad/install-nomad-nodejob-exporter.hcl

# 等价: 用环境变量,不进 shell history
NOMAD_VAR_datacenter=<your-dc> \
NOMAD_VAR_gh_token=ghp_xxx \
  nomad job run nomad/install-nomad-nodejob-exporter.hcl
```

### 变量

| 变量 | 必填 | 默认 | 说明 |
| --- | --- | --- | --- |
| `datacenter` | 是 | — | Nomad datacenter,例如 `prod-e2b-dc` / `dev-e2b-dc` |
| `node_pool` | 否 | `default` | Nomad node pool |
| `version_tag` | 否 | `manual` | 写到 job `meta.version`,在 `nomad job status` 里区分批次 |
| `gh_token` | 否 | `""` | 仅在撞 GitHub API 限速时用 |
| `script_url` | 否 | main 分支 raw URL | 测试分支版本时用 `-var=script_url=...` 覆盖,合 main 后无需指定 |

### 关于 `gh_token`

仓库是 **public**,脚本里 `curl` GitHub Releases 不需要 token。但 `api.github.com` 未认证限速是 **60 次/小时/IP**,如果集群所有节点同时被 sysbatch 触发、又共用一个出口 IP,很容易撞到 `API rate limit exceeded`,表现为脚本第一步拿不到 `DOWNLOAD_URL` 直接失败。

这时传一个有 `public_repo` scope 的 PAT 进来,限速会提到 5000/小时。token 通过 `env { GH_TOKEN = var.gh_token }` 注入给 task,再透传给装机脚本。

⚠️ 不要把 token 写到 hcl 文件里再 commit。用 `-var=` 或 `NOMAD_VAR_*` 在跑的时候传。CLAUDE.md 顶部约定:token 不留在节点 shell 历史 / systemd unit。

### 失败时怎么看

task 失败的话,包装脚本会在退出前打印:

- `systemctl status nomad-nodeJob-exporter`
- `journalctl -u nomad-nodeJob-exporter` 最后 100 行
- `curl http://127.0.0.1:9106/metrics` 的 HTTP code

可以直接用以下命令拉日志:

```bash
# 找到失败 alloc
nomad job status install-nomad-nodejob-exporter

# 看具体某个 alloc 的输出
nomad alloc logs <alloc-id>
nomad alloc logs -stderr <alloc-id>
```

### 只在某台节点跑

把 hcl 里 `constraint` 的注释解开,改成想要的 `node.unique.name`。或者直接命令行加:

```bash
nomad job run \
  -var="datacenter=<your-dc>" \
  nomad/install-nomad-nodejob-exporter.hcl
# 然后在 hcl 里加 constraint 后再跑;或用 -var 控制(需要在 hcl 里加变量化的 constraint)
```

### 参考

- 装机脚本本身: [`scripts/install-nomad-nodeJob-exporter.sh`](../scripts/install-nomad-nodeJob-exporter.sh)
- 升级脚本: [`scripts/upgrade-nomad-nodeJob-exporter.sh`](../scripts/upgrade-nomad-nodeJob-exporter.sh)
- 发布方案对比: [`docs/RELEASE.md`](../docs/RELEASE.md)
