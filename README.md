This repo contains automation tools for serving E2B.

## Nomad NodeJob Exporter

监控 Nomad Node 级别任务的 Prometheus Exporter，采集节点上运行的 Job 相关指标。

### 一键安装

```bash
curl -fsSL https://raw.githubusercontent.com/Infrawaves/e2b-infrawaves-tools/gaomingxing/scripts/install-nomad-nodeJob-exporter.sh | sudo bash
```

### 升级

```bash
curl -fsSL https://raw.githubusercontent.com/Infrawaves/e2b-infrawaves-tools/gaomingxing/scripts/upgrade-nomad-nodeJob-exporter.sh | sudo bash
```

详细文档：[nomad-nodeJob-exporter/readme.md](nomad-nodeJob-exporter/readme.md)