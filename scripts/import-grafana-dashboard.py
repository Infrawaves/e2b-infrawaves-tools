#!/usr/bin/env python3
"""
import-grafana-dashboard.py — 把从 Grafana API 原样导出的 dashboard(datasource 为
具体 uid)转换成本仓库约定的「可移植」格式:

  * panel / target / annotation 里的具体 datasource uid 全部替换成 ${DS_XXX} 变量;
  * 在 templating.list 顶部注入对应的 DS_XXX 数据源变量(type=datasource),
    这样换到别的 e2b 环境导入时,Grafana 会按数据源「类型」自动匹配,或由
    scripts/sync-grafana.sh 按名字回填 current uid;
  * 去掉 id / version 等随实例变化的易变字段,稳定 uid 不动(同步幂等靠它)。

设计原则:只做机械、可逆、可审计的替换,不改查询/阈值/布局。

datasource uid → 变量名 的映射 **按本集群实际数据源** 固定在 DS_MAP 里。接其他
e2b 环境时,只需在目标 Grafana 建同类型数据源,sync 脚本按「数据源名」解析 current,
或在此表补新环境的 uid。多个同类型数据源用带后缀的变量名区分(如 DS_POSTGRES_ENVS)。

用法:
  scripts/import-grafana-dashboard.py <导出的.json> [输出.json]
  # 不给输出路径时打印到 stdout
  cat raw.json | scripts/import-grafana-dashboard.py -   # 从 stdin 读

变量 → 数据源类型(供注入 templating 用):
  DS_PROMETHEUS         prometheus
  DS_LOKI               loki
  DS_CLICKHOUSE         grafana-clickhouse-datasource
  DS_INFINITY           yesoreyeram-infinity-datasource   (e2b-orchestrator)
  DS_INFINITY_VOLCANO   yesoreyeram-infinity-datasource   (volcano-tls-alb)
  DS_POSTGRES           grafana-postgresql-datasource     (Supabase 只读)
  DS_POSTGRES_ENVS      grafana-postgresql-datasource     (E2B 主库 envs)
"""
import json
import sys
import copy

# ---- 本集群 datasource uid → 变量名(接新环境时在此补 uid)----
DS_MAP = {
    # prometheus(可能存在多个 uid,统一归到 DS_PROMETHEUS)
    "afoh4xrv0awowc": "DS_PROMETHEUS",
    "cfmlanmx6zl6od": "DS_PROMETHEUS",
    # loki
    "afo6wk07j06iob": "DS_LOKI",
    # clickhouse
    "cfyzcpdk3t7uoa": "DS_CLICKHOUSE",
    # infinity
    "bfz0dki630zr4a": "DS_INFINITY",          # e2b-orchestrator
    "ag3a94v3e42kgf": "DS_INFINITY_VOLCANO",  # volcano-tls-alb
    # postgres
    "bfzscvk9p4bggb": "DS_POSTGRES",          # Supabase 只读(matrix 既定约定)
    "ag15yhfcdlse8f": "DS_POSTGRES_ENVS",     # E2B 主库 envs
    "dg15y1tujaarkf": "DS_POSTGRES_ENVS",     # E2B 主库 (envs) 同库别名
}

# 变量名 → Grafana 数据源插件类型(注入 templating 用)
VAR_TYPE = {
    "DS_PROMETHEUS": "prometheus",
    "DS_LOKI": "loki",
    "DS_CLICKHOUSE": "grafana-clickhouse-datasource",
    "DS_INFINITY": "yesoreyeram-infinity-datasource",
    "DS_INFINITY_VOLCANO": "yesoreyeram-infinity-datasource",
    "DS_POSTGRES": "grafana-postgresql-datasource",
    "DS_POSTGRES_ENVS": "grafana-postgresql-datasource",
}

# 变量名 → 下拉框显示标签
VAR_LABEL = {
    "DS_PROMETHEUS": "Prometheus 数据源",
    "DS_LOKI": "Loki 数据源",
    "DS_CLICKHOUSE": "ClickHouse 数据源",
    "DS_INFINITY": "Infinity 数据源 (orchestrator)",
    "DS_INFINITY_VOLCANO": "Infinity 数据源 (火山 ALB)",
    "DS_POSTGRES": "PostgreSQL 数据源 (Supabase 只读)",
    "DS_POSTGRES_ENVS": "PostgreSQL 数据源 (E2B envs 主库)",
}


def walk(obj, used):
    """递归把所有 datasource.uid 的具体 uid 替换成 ${DS_XXX},记录用到的变量名。"""
    if isinstance(obj, dict):
        # datasource 可能是 {"type":..,"uid":".."} 或直接字符串 uid
        ds = obj.get("datasource")
        if isinstance(ds, dict):
            uid = ds.get("uid")
            if uid in DS_MAP:
                var = DS_MAP[uid]
                ds["uid"] = "${%s}" % var
                used.add(var)
        elif isinstance(ds, str) and ds in DS_MAP:
            var = DS_MAP[ds]
            obj["datasource"] = "${%s}" % var
            used.add(var)
        for v in obj.values():
            walk(v, used)
    elif isinstance(obj, list):
        for v in obj:
            walk(v, used)


def make_ds_var(var):
    return {
        "current": {},
        "hide": 0,
        "includeAll": False,
        "label": VAR_LABEL.get(var, var),
        "multi": False,
        "name": var,
        "options": [],
        "query": VAR_TYPE[var],
        "refresh": 1,
        "regex": "",
        "skipUrlSync": False,
        "type": "datasource",
    }


def convert(d):
    d = copy.deepcopy(d)
    used = set()
    walk(d, used)

    # 注入缺失的 DS_* 变量到 templating.list 最前面
    d.setdefault("templating", {}).setdefault("list", [])
    tpl = d["templating"]["list"]
    existing = {v.get("name") for v in tpl if isinstance(v, dict)}
    new_vars = [make_ds_var(v) for v in sorted(used) if v not in existing]
    d["templating"]["list"] = new_vars + tpl

    # 清理随实例变化的易变字段(uid 保留,幂等靠它)
    d.pop("id", None)
    d.pop("version", None)
    d.pop("iteration", None)
    return d, sorted(used)


def main():
    if len(sys.argv) < 2:
        print(__doc__)
        sys.exit(2)
    src = sys.argv[1]
    raw = sys.stdin.read() if src == "-" else open(src, encoding="utf-8").read()
    d = json.loads(raw)
    # 允许传入 {"dashboard": {...}} 包装
    if "dashboard" in d and "panels" not in d:
        d = d["dashboard"]
    out, used = convert(d)
    text = json.dumps(out, ensure_ascii=False, indent=2) + "\n"
    if len(sys.argv) >= 3:
        open(sys.argv[2], "w", encoding="utf-8").write(text)
        print(f"wrote {sys.argv[2]}  (uid={out.get('uid')}, DS vars: {', '.join(used) or 'none'})",
              file=sys.stderr)
    else:
        sys.stdout.write(text)


if __name__ == "__main__":
    main()
