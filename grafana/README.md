# Grafana 配置管理

本目录用于以 **配置即代码** 的方式管理 E2B 集群可观测性所需的 Grafana 资源:

- `dashboards/` — Grafana Dashboard 的 JSON 模型,按业务域分子目录(如 `nomad/`, `firecracker/`, `host/`)。
- `provisioning/dashboards/` — Grafana 的 [dashboard provider](https://grafana.com/docs/grafana/latest/administration/provisioning/#dashboards) 配置 (`*.yaml`),把 `dashboards/` 目录挂进 Grafana。
- `provisioning/datasources/` — Prometheus / Loki / Tempo 等数据源的 provisioning 配置。
- `alerts/` — Alertmanager / Grafana Alerting 规则文件 (`*.yaml`)。
- `_legacy/` — 早期手工导出的 dashboard JSON 存档,**不参与同步**。新 dashboard 请放到 `dashboards/<area>/` 下,旧版仅保留作参考与回滚用。

## 设计原则

1. **JSON 文件来自 Grafana UI 的导出。** 编辑流程:在 UI 上调好 → Share → Export → 勾选 *Export for sharing externally* → 保存到本目录,提 PR review。
2. **Datasource 用变量**。模板中 datasource 引用统一写成 `${DS_PROMETHEUS}` 之类,部署时通过 provisioning 注入,避免 dashboard JSON 中硬编码 UID。
3. **不要在 JSON 里改 ID**。`id` 字段保持 `null`,`uid` 保持稳定(命名规范见下),Grafana 通过 `uid` 幂等 upsert。
4. **告警和面板分离**。Grafana 的"alert in panel"模式不利于版本管理,统一放在 `alerts/` 下,以 Prometheus rule 格式或 Grafana unified alerting YAML 表达。

## UID 命名规范

`<area>-<scope>-<name>`,全小写、kebab-case:

- `e2b-fc-overview` — Firecracker 进程总览
- `e2b-nomad-node` — Nomad 节点 / Allocation
- `e2b-host-resources` — 节点机器资源
- `e2b-sandbox-lifecycle` — 沙箱生命周期(规划中)

## 部署方式

### 方式一:Grafana provisioning(推荐,自管 Grafana)

把本目录挂载到 Grafana 容器,例如:

```yaml
# docker-compose.yaml 片段
grafana:
  image: grafana/grafana:11.3.0
  volumes:
    - ./grafana/dashboards:/var/lib/grafana/dashboards:ro
    - ./grafana/provisioning:/etc/grafana/provisioning:ro
```

`provisioning/dashboards/default.yaml` 已配置为从 `/var/lib/grafana/dashboards` 递归加载所有 JSON。

### 方式二:Grafana API 导入(已有 Grafana 实例)

仓库根目录提供两个同步脚本,从 `.env` 读取 `GRAFANA_URL` / `GRAFANA_USER` / `GRAFANA_PASS`,通过 HTTP API 推送:

- [scripts/sync-grafana.sh](../scripts/sync-grafana.sh) — 批量推送 `dashboards/**/*.json` 到 Grafana,自动解析数据源 UID、按需创建文件夹,以 `uid` 幂等 upsert。支持 `DRY_RUN=1` 仅校验。
- [scripts/sync-grafana-alerts.py](../scripts/sync-grafana-alerts.py) — 把 `alerts/*.yaml` 中的 Prometheus 风格告警规则转成 Grafana 托管告警,通过 `/api/v1/provisioning/alert-rules` 推送。

```bash
# 推送全部 dashboard
scripts/sync-grafana.sh
# 仅推送指定 dashboard
scripts/sync-grafana.sh fc-overview.json
# 推送告警规则
scripts/sync-grafana-alerts.py
```

## 告警规则的格式选择

`alerts/*.yaml` 使用 **Prometheus alerting rules 格式**(`alert/expr/for/labels/annotations`),而不是直接写 Grafana 原生的告警 JSON。原因:

1. **简洁性** —— Grafana 原生告警每条 ~50 行 JSON,嵌套 3 段查询模型(`datasourceUid`、`relativeTimeRange`、`reduce`、`math`),手写易错。Prom 格式 5 个字段就能描述一条规则。
2. **跨环境复用** —— Grafana 原生告警里的 `datasourceUid`、`folderUID` 都和实例绑定,写死在 YAML 里就锁死了某个 Grafana。`sync-grafana-alerts.py` 在运行时按名字反查 UID,同一份 YAML 可以推到多套 Grafana。
3. **与 Prometheus 自身告警语法一致** —— 熟悉 PromQL 的人不需要再学一套 Grafana 的 expression 模型。

`sync-grafana-alerts.py` 做的事就是这层适配:把 `expr CMP VAL` 拆成 Grafana 要求的 3 阶段查询(PromQL 即时查询 → `reduce(last)` → `math: $B CMP VAL`),复合表达式(如 `X > 0 and on(node_ip) Y == 1`)整段塞进第一阶段、第三阶段退化为 `$B > 0`。

### 方式三:Grizzly(可选)

如果团队规模扩大,可引入 [grizzly](https://github.com/grafana/grizzly) 做 GitOps 风格的同步,本目录结构与之兼容。

## 提交流程

1. 在 Grafana UI 编辑或新建 dashboard。
2. **Share → Export → Export for sharing externally** 下载 JSON。
3. 替换/新建到对应子目录,文件名与 `uid` 一致 (`fc-overview.json`)。
4. 检查 JSON 中的 `__inputs` 和 datasource UID 是否被参数化。
5. 提交 PR;CI 会跑 `jq` 语法校验。

---

## 目录结构(2026-11 全量纳管后)

```
grafana/
  dashboards/
    sandbox/     ① 创建成功率 / ② 恢复 / ③ 运行期 / ④ 问题模板 / ⑤ kill / ⑦ 矩阵
    firecracker/ fc 总览 / 集群健康 / 泄漏检测
    nomad/       节点分配 / Job 内存监控
    oom/         ⑥ orchestrator OOM(节点物理机 / 爆炸半径)
    loki/        日志应用面板
    _external/   非 e2b 面板存档(supabase / 火山 ALB / NodeExporter / loki-apps)——纯备份,按需推送
  alerts/
    firecracker.yaml              Prometheus 风格告警源(sync-grafana-alerts.py 推送)
    provisioning/                 线上 Grafana 托管告警的「实况导出」(export 脚本产出)
      alert-rules.json            11 条规则(含 PG 连通性 / 节点排空 / sandbox 创建&运行)
      contact-points.json         飞书 webhook 联系点(url 已脱敏为 ${FEISHU_WEBHOOK_*})
      notification-policies.json  通知路由(按 node_id / team_id 分组,repeat_interval)
  provisioning/                   自管 Grafana 的 datasource / dashboard provider 配置
```

仓库根 `ops-scripts/` 放运维告警脚本(飞书推送 / TLS 日志代理),敏感默认值已脱敏,靠环境变量注入。

> **已下线(线上 404)但仓库保留作历史**:`firecracker/cluster-overview.json`(uid `e2b-cluster-overview`,已并入 `fc-overview` → 现标题「Firecracker 与 集群总览」)、`nomad/nomad-node-allocations.json`(uid `e2b-nomad-node`)。`refresh-from-live.py` 不覆盖它们(线上已无),需要时手动删除或恢复。另 `_external/loki-apps.json` 与 `loki/sadlil-loki-apps-dashboard.json` 同 uid,前者是早期存档副本。

## 数据源变量映射(多环境可移植)

线上 dashboard 的 datasource 是具体 uid;入库前用 `scripts/import-grafana-dashboard.py` 统一替换成 `${DS_*}` 变量并注入 templating。映射(接新 e2b 环境时在脚本 `DS_MAP` 补该环境 uid):

| 变量 | 数据源类型 | 本集群数据源 |
| --- | --- | --- |
| `${DS_PROMETHEUS}` | prometheus | HK |
| `${DS_LOKI}` | loki | loki |
| `${DS_CLICKHOUSE}` | grafana-clickhouse-datasource | prod_e2b_clickhouse |
| `${DS_INFINITY}` | yesoreyeram-infinity-datasource | e2b-orchestrator |
| `${DS_INFINITY_VOLCANO}` | 同上 | volcano-tls-alb |
| `${DS_POSTGRES}` | grafana-postgresql-datasource | Supabase 只读 |
| `${DS_POSTGRES_ENVS}` | 同上 | E2B envs 主库 |

`sync-grafana.sh` 按**数据源名**回填 `DS_PROMETHEUS/DS_CLICKHOUSE/DS_LOKI` 的 current(名字在 `.env`:`DS_NAME`/`DS_CH_NAME`/`DS_LOKI_NAME`;本集群分别是 `HK`/`prod_e2b_clickhouse`/`loki`)。其余 DS 变量(infinity/postgres)由 Grafana 导入时按类型自动匹配。

## 工具脚本

| 脚本 | 用途 |
| --- | --- |
| `scripts/import-grafana-dashboard.py` | 线上导出的 dashboard(具体 uid)→ 仓库格式(`${DS_*}` 变量 + templating 注入),去掉易变 id/version |
| `scripts/export-grafana-alerting.py` | 导出线上托管告警规则/联系点/策略到 `alerts/provisioning/`,webhook url 自动脱敏 |
| `scripts/refresh-from-live.py` | **一键全量回拉**:把线上全部 dashboard 和告警刷回仓库(串起上面两个转换器)。dashboard 按 `UID_PATH` 映射写回 `dashboards/<area>/`,告警导出到 `alerts/provisioning/`。用 **Bearer token**(线上 admin 口令已轮换,Basic 不可用):`GRAFANA_URL=... GRAFANA_TOKEN=glsa_xxx python scripts/refresh-from-live.py`,可加 `--dashboards-only` / `--alerts-only`。新增线上面板时在脚本 `UID_PATH` 补一行 |
| `scripts/sync-grafana.sh` | 推 dashboards(`DRY_RUN=1` 校验) |
| `scripts/sync-grafana-alerts.py` | 推 `alerts/*.yaml`(Prom 风格→Grafana 三阶段) |
| `scripts/validate-grafana.py` | 跑全部 PromQL 对线上校验 |
| `grafana/toggle_alert.sh` | 暂停/启用/查看单条告警规则 isPaused |

> **注意**:`alerts/provisioning/*.json` 是线上实况的只读存档(便于审计/跨环境复制),当前**没有**自动回推脚本;修改托管告警仍走 provisioning API(见 `grafana/toggle_alert.sh` 的调用方式)。`contact-points.json` 里的 `${FEISHU_WEBHOOK_*}` 占位符需在目标环境用真实 webhook 替换后再导入。
