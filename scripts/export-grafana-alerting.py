#!/usr/bin/env python3
"""
export-grafana-alerting.py — 把 Grafana 托管告警的「规则 / 联系点 / 通知策略」从
provisioning API 原样导出到 grafana/alerts/provisioning/ 下,供版本管理与跨环境复制。

安全:联系点里的飞书 webhook url 含 bot token,导出时**脱敏**成 ${FEISHU_WEBHOOK_<name>}
占位符(真实 url 放目标环境的 .env / 密管,推送时由 import 脚本回填)。告警文案
payload.template 完整保留(它才是告警消息的真正来源)。

用法:
  set -a && . ./.env && set +a
  scripts/export-grafana-alerting.py            # 写入 grafana/alerts/provisioning/
  scripts/export-grafana-alerting.py --stdout   # 只打印摘要,不写文件

读取 .env: GRAFANA_URL / GRAFANA_USER / GRAFANA_PASS
"""
import os
import sys
import json
import base64
import urllib.request

OUT_DIR = "grafana/alerts/provisioning"


def api(path):
    U = os.environ["GRAFANA_URL"]
    auth = base64.b64encode(
        f"{os.environ['GRAFANA_USER']}:{os.environ['GRAFANA_PASS']}".encode()
    ).decode()
    req = urllib.request.Request(
        U + path, headers={"Authorization": "Basic " + auth, "Accept": "application/json"}
    )
    return json.load(urllib.request.urlopen(req, timeout=30))


def mask_contact_points(cps):
    """把 webhook url 等敏感字段换成占位符;返回(脱敏后的列表, 占位符→说明)。"""
    placeholders = {}
    for c in cps:
        s = c.get("settings") or {}
        if "url" in s and s["url"]:
            ph = "${FEISHU_WEBHOOK_%s}" % c.get("name", "cp").replace("-", "_").upper()
            placeholders[ph] = "原 webhook url(飞书 bot,已脱敏)"
            s["url"] = ph
        # 其它可能的密钥字段
        for k in ("password", "token", "secret", "authorization_credentials"):
            if s.get(k):
                s[k] = "${REDACTED_%s}" % k.upper()
    return cps, placeholders


def main():
    write = "--stdout" not in sys.argv
    rules = api("/api/v1/provisioning/alert-rules")
    cps = api("/api/v1/provisioning/contact-points")
    policies = api("/api/v1/provisioning/policies")

    cps, placeholders = mask_contact_points(cps)

    # 规则按 folder/ruleGroup 归组,稳定排序便于 diff
    rules_sorted = sorted(rules, key=lambda r: (r.get("ruleGroup", ""), r.get("uid", "")))

    if write:
        os.makedirs(OUT_DIR, exist_ok=True)
        json.dump(rules_sorted, open(f"{OUT_DIR}/alert-rules.json", "w", encoding="utf-8"),
                  ensure_ascii=False, indent=2)
        json.dump(cps, open(f"{OUT_DIR}/contact-points.json", "w", encoding="utf-8"),
                  ensure_ascii=False, indent=2)
        json.dump(policies, open(f"{OUT_DIR}/notification-policies.json", "w", encoding="utf-8"),
                  ensure_ascii=False, indent=2)
        print(f"wrote {OUT_DIR}/alert-rules.json ({len(rules_sorted)} rules)", file=sys.stderr)
        print(f"wrote {OUT_DIR}/contact-points.json ({len(cps)} contact points)", file=sys.stderr)
        print(f"wrote {OUT_DIR}/notification-policies.json", file=sys.stderr)
        if placeholders:
            print("脱敏占位符(需在目标环境 .env/密管提供真实值):", file=sys.stderr)
            for ph, desc in placeholders.items():
                print(f"  {ph}  — {desc}", file=sys.stderr)

    # 摘要
    for r in rules_sorted:
        print(f"  [{r.get('ruleGroup')}] {r.get('uid')}  {r.get('title')}")


if __name__ == "__main__":
    main()
