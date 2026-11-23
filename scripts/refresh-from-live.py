#!/usr/bin/env python3
"""
refresh-from-live.py — 从线上 Grafana 全量刷新本仓库的 dashboard / 告警资产。

它是「把线上实况拉回仓库」的一次性同步器,串起仓库已有的两个转换器:
  * dashboard: 复用 import-grafana-dashboard.py 的 convert()(datasource uid→${DS_*}、
    注入 templating、去掉易变 id/version),按 uid 写回既有的 dashboards/<area>/ 路径。
  * 告警: 复用 export-grafana-alerting.py 的 mask_contact_points()(webhook 脱敏),
    导出 alert-rules / contact-points / notification-policies 到 alerts/provisioning/。

与 export-grafana-alerting.py 的区别:本脚本用 **Bearer token** 而非 Basic auth
(线上 admin 口令已轮换,Basic 不可用;临时服务账号 token 可用),且一次把 dashboard
和告警都刷新,UID→路径映射固定在 UID_PATH 里(新增 dashboard 时在此补一行)。

用法:
  GRAFANA_URL=http://150.5.144.132:3000 GRAFANA_TOKEN=glsa_xxx \
    python scripts/refresh-from-live.py            # 刷新 dashboards + 告警
  ... python scripts/refresh-from-live.py --alerts-only
  ... python scripts/refresh-from-live.py --dashboards-only

环境变量:
  GRAFANA_URL    线上 Grafana 根地址
  GRAFANA_TOKEN  Bearer token(服务账号),或
  GRAFANA_USER/GRAFANA_PASS  Basic auth(口令有效时可用)
"""
import os
import sys
import json
import base64
import urllib.request

HERE = os.path.dirname(os.path.abspath(__file__))
REPO = os.path.dirname(HERE)
sys.path.insert(0, HERE)

# 复用仓库已有转换器,保证产出格式与手工 import/export 完全一致
import importlib.util


def _load(mod_file, mod_name):
    spec = importlib.util.spec_from_file_location(mod_name, os.path.join(HERE, mod_file))
    m = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(m)
    return m


imp = _load("import-grafana-dashboard.py", "import_grafana_dashboard")
exp = _load("export-grafana-alerting.py", "export_grafana_alerting")

# uid → 仓库内 dashboard 路径(相对 grafana/dashboards/)。
# e2b 自建面板纳管;_external 的第三方面板(supabase/火山/node-exporter/loki-apps)
# 也拉一份存档,与仓库既有布局一致。新增面板在此补一行即可。
UID_PATH = {
    # sandbox 业务域
    "e2b-sbx-create-teams": "sandbox/e2b-sbx-create-teams.json",
    "e2b-sbx-resume":       "sandbox/e2b-sbx-resume.json",
    "e2b-sbx-runtime":      "sandbox/e2b-sbx-runtime.json",
    "e2b-sbx-template":     "sandbox/e2b-sbx-template.json",
    "e2b-sbx-kill":         "sandbox/e2b-sbx-kill.json",
    "e2b-sandbox-matrix":   "sandbox/e2b-sandbox-matrix.json",
    # firecracker
    "e2b-fc-overview":      "firecracker/fc-overview.json",
    "e2b-fc-leak":          "firecracker/sandbox-leak.json",
    # nomad
    "adkxj8z":              "nomad/e2b-nomad-job-memory.json",
    # oom
    "de8d91c6-ec95-4bed-8fff-45f62941b722": "oom/e2b-node-orch-oom.json",
    "e2b-orch-oom":         "oom/e2b-orch-oom.json",
    # loki
    "sadlil-loki-apps-dashboard": "loki/sadlil-loki-apps-dashboard.json",
    # 第三方面板存档(_external,纯备份)
    "rYdddlPWk":            "_external/node-exporter-full.json",
    "supabase-pg-health":   "_external/supabase-pg-health.json",
    "d402d94e-da48-48e4-ac52-53026b96a004": "_external/supabase-project-monitor.json",
    "volcano-alb-http":     "_external/volcano-alb-http.json",
}

# nomad-node-allocations.json 线上 uid(仓库有该文件,确认线上 uid 后补)
# cluster-overview.json 同理。留空表示本次不强制覆盖,避免误删手工存档。


def _auth_header():
    tok = os.environ.get("GRAFANA_TOKEN")
    if tok:
        return "Bearer " + tok
    u, p = os.environ.get("GRAFANA_USER"), os.environ.get("GRAFANA_PASS")
    if u and p:
        return "Basic " + base64.b64encode(f"{u}:{p}".encode()).decode()
    sys.exit("需要 GRAFANA_TOKEN 或 GRAFANA_USER/GRAFANA_PASS")


def api(path):
    url = os.environ["GRAFANA_URL"].rstrip("/") + path
    req = urllib.request.Request(
        url, headers={"Authorization": _auth_header(), "Accept": "application/json"})
    return json.load(urllib.request.urlopen(req, timeout=60))


def refresh_dashboards():
    out_root = os.path.join(REPO, "grafana", "dashboards")
    items = api("/api/search?type=dash-db&limit=5000")
    live = {x["uid"]: x for x in items}
    done, skipped = [], []
    for uid, rel in UID_PATH.items():
        if uid not in live:
            skipped.append((uid, rel, "线上不存在"))
            continue
        full = api(f"/api/dashboards/uid/{uid}")
        dash = full["dashboard"]
        conv, used = imp.convert(dash)
        dst = os.path.join(out_root, rel)
        os.makedirs(os.path.dirname(dst), exist_ok=True)
        with open(dst, "w", encoding="utf-8") as f:
            f.write(json.dumps(conv, ensure_ascii=False, indent=2) + "\n")
        done.append((uid, rel, ", ".join(used) or "none"))
    # 线上有但未纳管的面板
    unmanaged = [u for u in live if u not in UID_PATH]
    print(f"\n== dashboards: 刷新 {len(done)} 个 ==", file=sys.stderr)
    for uid, rel, ds in done:
        print(f"  ✓ {rel}  (uid={uid}; DS: {ds})", file=sys.stderr)
    for uid, rel, why in skipped:
        print(f"  - 跳过 {rel}  ({why})", file=sys.stderr)
    if unmanaged:
        print(f"  ⚠ 线上未纳管(UID_PATH 未列): {', '.join(unmanaged)}", file=sys.stderr)


def refresh_alerts():
    out_dir = os.path.join(REPO, "grafana", "alerts", "provisioning")
    os.makedirs(out_dir, exist_ok=True)
    rules = api("/api/v1/provisioning/alert-rules")
    cps = api("/api/v1/provisioning/contact-points")
    policies = api("/api/v1/provisioning/policies")
    cps, placeholders = exp.mask_contact_points(cps)
    rules_sorted = sorted(rules, key=lambda r: (r.get("ruleGroup", ""), r.get("uid", "")))
    json.dump(rules_sorted, open(f"{out_dir}/alert-rules.json", "w", encoding="utf-8"),
              ensure_ascii=False, indent=2)
    json.dump(cps, open(f"{out_dir}/contact-points.json", "w", encoding="utf-8"),
              ensure_ascii=False, indent=2)
    json.dump(policies, open(f"{out_dir}/notification-policies.json", "w", encoding="utf-8"),
              ensure_ascii=False, indent=2)
    print(f"\n== alerts: {len(rules_sorted)} 规则 / {len(cps)} 联系点 ==", file=sys.stderr)
    for r in rules_sorted:
        print(f"  [{r.get('ruleGroup')}] {r.get('uid')}  {r.get('title')}", file=sys.stderr)
    for ph, desc in placeholders.items():
        print(f"  脱敏 {ph} — {desc}", file=sys.stderr)


def main():
    if not os.environ.get("GRAFANA_URL"):
        sys.exit("需要 GRAFANA_URL")
    args = set(sys.argv[1:])
    do_dash = "--alerts-only" not in args
    do_alert = "--dashboards-only" not in args
    if do_dash:
        refresh_dashboards()
    if do_alert:
        refresh_alerts()


if __name__ == "__main__":
    main()
