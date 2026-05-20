# 私仓发布与一键安装方案

仓库当前是 GitHub 私仓,直接 `curl https://api.github.com/repos/.../releases/latest` 会因为没有认证而 404。
本文档列出三种工程上可行的方案,按推荐度从高到低排序,运维方可按集群规模和安全要求选择。

## TL;DR 选哪个

| 方案 | 一键安装命令 | 谁能装 | 维护成本 | 推荐场景 |
| --- | --- | --- | --- | --- |
| A. **GH_TOKEN 注入** | `curl ... \| sudo GH_TOKEN=ghp_xxx bash` | 持有 token 的运维 | 低 | 节点 ≤ 几十台,运维少 |
| B. **内网 HTTP 镜像** | `curl ... \| sudo MIRROR_URL=https://mirror/exporter bash` | 内网任何节点 | 中(要部署镜像) | 节点几十~几百台,有内网 |
| C. **公开 release-only 仓库** | `curl ... \| sudo bash` | 任意公网节点 | 中(需要双仓 CI) | 客户/外部用户,源码必须保密 |

安装/升级脚本都已经支持 A 和 B,无需重新发布即可切换。

---

## 方案 A:GH_TOKEN 注入(已实现)

### 工作方式
- 创建一个 fine-grained PAT,权限 `Contents: read` on `Infrawaves/e2b-infrawaves-tools`。
- 安装脚本检测到 `GH_TOKEN` / `GITHUB_TOKEN` 环境变量时:
  1. 用 Bearer token 请求 `/releases/latest`
  2. 拿到 asset 的 `id`,通过 `https://api.github.com/repos/.../releases/assets/<id>` + `Accept: application/octet-stream` 下载二进制
- 不传 token 时,行为与公仓一致(用 `browser_download_url`)。

### 一键安装

```bash
curl -fsSL https://raw.githubusercontent.com/Infrawaves/e2b-infrawaves-tools/main/scripts/install-nomad-nodeJob-exporter.sh \
  | sudo GH_TOKEN=ghp_xxx bash
```

> 注意:`raw.githubusercontent.com` 对私仓也需要认证。脚本本身可以预先复制到节点,或通过另一条带 token 的 curl 拉:
> ```bash
> curl -fsSL -H "Authorization: Bearer ghp_xxx" \
>   https://raw.githubusercontent.com/Infrawaves/e2b-infrawaves-tools/main/scripts/install-nomad-nodeJob-exporter.sh \
>   | sudo GH_TOKEN=ghp_xxx bash
> ```

### Token 管理建议
- 用 **fine-grained PAT**,选定到本仓库,只勾 `Contents: read`,过期时间 90 天。
- 把 token 放进 1Password/Vault 等密码管理,不要写进 systemd unit 也不要回显到日志。
- 批量装机时,在跳板机上 export 一次即可。

### 已知坑
- 6+ 个月没更新 token 就会过期,届时一键安装会突然 404。
- token 一旦泄漏,可读取所有当前及未来的 release artifact;源码访问视 token 范围而定。

---

## 方案 B:内网 HTTP 镜像

### 工作方式
- 在内网搭一个 Nginx / MinIO / GCS bucket,镜像每次发布的二进制。
- 安装脚本传 `MIRROR_URL` 后,跳过 GitHub API,直接 GET 该 URL。

### 一键安装

```bash
curl -fsSL http://mirror.internal/scripts/install-nomad-nodeJob-exporter.sh \
  | sudo MIRROR_URL=http://mirror.internal/binaries/nomad-nodeJob-exporter-linux-amd64 bash
```

(脚本可以一并放到镜像上,这样连第一行 curl 都不用过公网。)

### 镜像同步策略

CI 在 release 完成后把 artifact 推到内网。两种实现:

**B.1 — GitHub Actions push 到 GCS / S3**

```yaml
# .github/workflows/nomad-nodeJob-exporter-build.yml 增加一个 step
- name: Upload to internal mirror (GCS)
  if: github.event_name == 'workflow_dispatch' && github.event.inputs.release == 'release'
  run: |
    echo "$GCP_SA_KEY" | gcloud auth activate-service-account --key-file=-
    gsutil cp nomad-nodeJob-exporter/nomad-nodeJob-exporter-linux-amd64 \
      gs://infrawaves-tools-mirror/nomad-nodeJob-exporter/latest
  env:
    GCP_SA_KEY: ${{ secrets.GCP_SA_KEY }}
```

然后 GCS bucket 设为 IP 白名单或公开可读(因为只是构建产物)。

**B.2 — 内网拉取**(无 GHA 推权限时)

跑一个定时任务节点(cron 每 30 分钟),用 `GH_TOKEN` 拉最新 release 同步到本地镜像。脚本约 30 行 bash。

### 优点
- 节点完全脱离公网即可装机。
- 下载延迟低,装机速度更快。
- token 集中在一个 sync 节点,普通节点不接触 token。

### 缺点
- 多了一处镜像基础设施;镜像挂了就全员装不了。

---

## 方案 C:公开 release-only 仓库(双仓)

### 工作方式
- 新建公开仓库 `Infrawaves/e2b-tools-releases`,只放二进制(无源码)。
- 私仓 CI 在 release 时把 artifact 推到公仓:`gh release create --repo Infrawaves/e2b-tools-releases ...`
- 安装脚本中的 `REPO_OWNER/REPO_NAME` 默认指向公仓,无需 token。

### 一键安装

```bash
curl -fsSL https://raw.githubusercontent.com/Infrawaves/e2b-tools-releases/main/install-nomad-nodeJob-exporter.sh \
  | sudo bash
```

### CI 改造

在现有 release step 后加一个:

```yaml
- name: Mirror release to public repo
  if: github.event_name == 'workflow_dispatch' && github.event.inputs.release == 'release'
  run: |
    gh release create latest nomad-nodeJob-exporter/nomad-nodeJob-exporter-linux-amd64 \
      --repo Infrawaves/e2b-tools-releases \
      --target main \
      --title "Latest" \
      --notes "Mirrored from $GITHUB_SHA" \
      || gh release upload latest nomad-nodeJob-exporter/nomad-nodeJob-exporter-linux-amd64 \
           --repo Infrawaves/e2b-tools-releases --clobber
  env:
    GH_TOKEN: ${{ secrets.RELEASE_MIRROR_TOKEN }}
```

需要一个有 `Contents: write` on `e2b-tools-releases` 的 PAT,作为 secret 注入。

### 优点
- 用户侧零配置,curl-pipe 跟公仓一样。
- 源码继续保持私有。
- token 只在 CI 上,节点不接触。

### 缺点
- 二进制公开了 — 反编译能拿到代码意图,得评估这部分是否可接受。
  - 当前 exporter 仅处理 nomad token 和 firecracker 进程读取,二进制泄漏风险有限。
- 多维护一个仓和一个 token。

---

## 推荐路径

1. **现在(节点 ~10 台)**:用方案 A,token 注入。脚本已经支持,改一下 README 加个用法示例就行。
2. **节点超过 30 台或要无外网装机**:加方案 B,搭一个内网 mirror。脚本不用动。
3. **要给客户/外部用户用**:启用方案 C,做双仓 mirror。

三个方案不互斥,A+B 一起部署最稳:正常走内网,降级走 token。

## 安全注意事项

- 任何方案都不要把 token 写进 systemd unit 文件(`Environment="GITHUB_TOKEN=..."`),装完就清。
- 如果脚本本身放在私仓,**第一条** `curl` 也需要认证,否则就拉不到脚本。两条思路:
  - 把 `scripts/*.sh` 单独放公仓或内网镜像。
  - 在跳板机上预下脚本,只传 token 进 bash。
- release 二进制建议用 `cosign` 签名,装机时校验,防止 mirror 被替换。当前未实施,作为后续 TODO。
