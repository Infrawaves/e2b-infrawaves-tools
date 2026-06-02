# Nomad jobs

## install-nomad-nodejob-exporter.hcl

`sysbatch`,在每个节点上跑 `scripts/install-nomad-nodeJob-exporter.sh`(从 GitHub Releases 拉最新二进制 + 装 systemd)。

### 用法

```bash
NOMAD_VAR_datacenter=prod-e2b-dc \
NOMAD_VAR_version_tag=$(date +%Y%m%d-%H%M) \
  nomad job run nomad/install-nomad-nodejob-exporter.hcl
```

⚠️ **`version_tag` 必须每次变**(建议用时间戳)。Nomad 看 job spec hash 没变就跳过已 complete 的节点 alloc,默认值 `manual` 第二次跑就不会重新调度。

要带 GitHub token(防 60/h/IP 限速):

```bash
NOMAD_VAR_datacenter=prod-e2b-dc \
NOMAD_VAR_gh_token=ghp_xxx \
  nomad job run nomad/install-nomad-nodejob-exporter.hcl
```

测试分支版本时覆盖 script_url:

```bash
NOMAD_VAR_datacenter=dev-e2b-dc \
NOMAD_VAR_script_url=https://raw.githubusercontent.com/Infrawaves/e2b-infrawaves-tools/<branch>/scripts/install-nomad-nodeJob-exporter.sh \
  nomad job run nomad/install-nomad-nodejob-exporter.hcl
```

只在某台节点跑:在 hcl 里 `group "install" {` 上面加:

```hcl
constraint {
  attribute = "${node.unique.name}"
  value     = "<node-name>"
}
```

### 失败怎么排查

```bash
nomad job status install-nomad-nodejob-exporter   # 看哪些 alloc 失败
nomad alloc logs <alloc-id>                        # 看 install 脚本输出
```

诊断都在装机脚本里:撞 GitHub API 限速时打印 HTTP code + 响应体 + rate_limit;systemd 启动失败时打印 `systemctl status`。

### 变量

| 变量 | 必填 | 默认 |
| --- | --- | --- |
| `datacenter` | 是 | — |
| `node_pool` | 否 | `default` |
| `version_tag` | 否 | `manual` |
| `gh_token` | 否 | `""` |
| `script_url` | 否 | main 分支 raw URL |

⚠️ token 不要写进 hcl 提交——用 `NOMAD_VAR_gh_token=...` 在跑时传。
