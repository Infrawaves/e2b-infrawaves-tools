#!/usr/bin/env python3
"""将 grafana/alerts/firecracker.yaml 中的 Prometheus 格式告警规则
转换为 Grafana 托管的告警规则,并通过 provisioning API 推送。

- 从 .env 读取 GRAFANA_URL / GRAFANA_USER / GRAFANA_PASS
- 通过名称解析 Prometheus 数据源 UID(默认 "prometheus")
- 解析 / 创建告警文件夹(默认 "e2b-alerts")
- 为每条规则构建 3 阶段的 Grafana 告警规则(PromQL -> reduce(last) -> math)
- 通过 POST/PUT /api/v1/provisioning/alert-rules 进行 upsert
- 发送 X-Disable-Provenance 请求头,以便 UI 仍可编辑这些规则

从 Prometheus 风格的 `expr CMP VAL` 到 Grafana 的映射:
  data[0] = 在 Prometheus 数据源上执行的 LHS PromQL(即时查询)
  data[1] = 在 __expr__ 上执行的 reduce(last, A)
  data[2] = 在 __expr__ 上执行的 math `$B CMP VAL`(即告警条件)

像 `X > 0 and on(node_ip) Y == 1` 这样的复合表达式会被完整保留在
data[0] 中;data[2] 变为 `$B > 0`,只要结果非空就触发告警。
"""
from __future__ import annotations

import base64
import json
import os
import re
import sys
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
ALERT_FILE = ROOT / "grafana" / "alerts" / "firecracker.yaml"


def load_env() -> dict[str, str]:
    env_file = ROOT / ".env"
    env: dict[str, str] = {}
    if env_file.exists():
        for line in env_file.read_text().splitlines():
            line = line.strip()
            if not line or line.startswith("#") or "=" not in line:
                continue
            k, v = line.split("=", 1)
            env[k.strip()] = v.strip()
    for k in ("GRAFANA_URL", "GRAFANA_USER", "GRAFANA_PASS", "DS_NAME", "ALERT_FOLDER"):
        if k in os.environ:
            env[k] = os.environ[k]
    return env


def http(method: str, url: str, env: dict, body: dict | None = None,
         extra_headers: dict | None = None) -> tuple[int, dict | str]:
    data = json.dumps(body).encode() if body is not None else None
    auth = base64.b64encode(f"{env['GRAFANA_USER']}:{env['GRAFANA_PASS']}".encode()).decode()
    headers = {
        "Authorization": f"Basic {auth}",
        "Content-Type": "application/json",
        "X-Disable-Provenance": "true",
    }
    if extra_headers:
        headers.update(extra_headers)
    req = urllib.request.Request(url, data=data, method=method, headers=headers)
    try:
        with urllib.request.urlopen(req, timeout=15) as resp:
            raw = resp.read().decode()
            try:
                return resp.status, json.loads(raw) if raw else {}
            except json.JSONDecodeError:
                return resp.status, raw
    except urllib.error.HTTPError as e:
        raw = e.read().decode()
        try:
            return e.code, json.loads(raw) if raw else raw
        except json.JSONDecodeError:
            return e.code, raw


def get_ds_uid(env: dict, name: str) -> str:
    code, body = http("GET", f"{env['GRAFANA_URL']}/api/datasources/name/{name}", env)
    if code != 200:
        raise RuntimeError(f"datasource '{name}' lookup failed: {code} {body}")
    return body["uid"]


def get_or_create_folder(env: dict, title: str) -> str:
    code, body = http("GET", f"{env['GRAFANA_URL']}/api/folders", env)
    if code != 200:
        raise RuntimeError(f"folder list failed: {code} {body}")
    for f in body:
        if f.get("title") == title:
            return f["uid"]
    code, body = http("POST", f"{env['GRAFANA_URL']}/api/folders", env,
                      body={"title": title})
    if code not in (200, 201):
        raise RuntimeError(f"folder create failed: {code} {body}")
    return body["uid"]


# ----- 最小化的 Prometheus 规则 YAML 解析器(无外部依赖) -----
def parse_rules(text: str) -> tuple[str, str, list[dict]]:
    """返回 (group_name, group_interval, [rule, ...])。

    Rule dict 的键: alert, uid(可选), expr, for, labels, annotations。
    若 YAML 中未显式指定 uid,则由 slug(alert) 推导;告警名为中文时
    必须显式指定 uid(全 ASCII),否则推导出的 UID 会是空串。
    """
    group_name = ""
    group_interval = "1m"
    rules: list[dict] = []
    cur: dict | None = None
    in_annotations = False
    in_labels = False
    expr_continuation = False

    def flush():
        nonlocal cur
        if cur:
            rules.append(cur)
            cur = None

    for raw in text.splitlines():
        line = raw.rstrip()
        stripped = line.strip()

        m = re.match(r"^\s*-\s*name:\s*(.+)$", line)
        if m:
            group_name = m.group(1).strip().strip("\"'")
            in_annotations = in_labels = expr_continuation = False
            continue
        m = re.match(r"^\s*interval:\s*(.+)$", line)
        if m and not cur:
            group_interval = m.group(1).strip().strip("\"'")
            continue

        m = re.match(r"^\s*-\s*alert:\s*(.+)$", line)
        if m:
            flush()
            cur = {"alert": m.group(1).strip().strip("\"'"), "expr": "",
                   "for": "0s", "labels": {}, "annotations": {}}
            in_annotations = in_labels = expr_continuation = False
            continue
        if cur is None:
            continue

        m = re.match(r"^\s*uid:\s*(.+)$", line)
        if m:
            cur["uid"] = m.group(1).strip().strip("\"'")
            in_annotations = in_labels = expr_continuation = False
            continue

        m = re.match(r"^\s*expr:\s*(.*)$", line)
        if m:
            v = m.group(1).strip()
            cur["expr"] = v.strip("\"'")
            in_annotations = in_labels = False
            expr_continuation = True
            continue
        m = re.match(r"^\s*for:\s*(.+)$", line)
        if m:
            cur["for"] = m.group(1).strip().strip("\"'")
            in_annotations = in_labels = expr_continuation = False
            continue
        if re.match(r"^\s*labels:\s*$", line):
            in_labels = True
            in_annotations = expr_continuation = False
            continue
        if re.match(r"^\s*annotations:\s*$", line):
            in_annotations = True
            in_labels = expr_continuation = False
            continue
        if expr_continuation and stripped and not re.match(r"^[a-zA-Z_]+:", stripped):
            cur["expr"] = (cur["expr"] + " " + stripped).strip()
            continue
        m = re.match(r"^\s+([a-zA-Z_][a-zA-Z0-9_]*):\s*(.+)$", line)
        if m and (in_labels or in_annotations):
            k, v = m.group(1), m.group(2).strip().strip("\"'")
            (cur["labels"] if in_labels else cur["annotations"])[k] = v
            continue

    flush()
    return group_name, group_interval, rules


# ----- PromQL 表达式 -> (lhs, op, rhs) 拆分器 -----
TOP_LEVEL_OPS = [">=", "<=", "==", "!=", ">", "<"]


def split_threshold(expr: str) -> tuple[str, str, float | None]:
    """查找顶层(括号深度为 0、方括号深度为 0)的比较运算符,
    若 rhs 可解析为数字则返回 (lhs, op, rhs_as_number);
    否则返回 (whole_expr, ">", 0),只要结果非空就触发告警。
    """
    depth_paren = depth_bracket = depth_brace = 0
    i = 0
    while i < len(expr):
        c = expr[i]
        if c == "(":
            depth_paren += 1
        elif c == ")":
            depth_paren -= 1
        elif c == "[":
            depth_bracket += 1
        elif c == "]":
            depth_bracket -= 1
        elif c == "{":
            depth_brace += 1
        elif c == "}":
            depth_brace -= 1
        elif depth_paren == depth_bracket == depth_brace == 0:
            for op in TOP_LEVEL_OPS:
                if expr[i:i + len(op)] == op:
                    # 扫描 `<` 时避免误匹配多字符 `<=`
                    if op in ("<", ">") and expr[i + 1:i + 2] == "=":
                        continue
                    # ` and ` / ` or ` 等不在此列表中,无需处理
                    lhs = expr[:i].strip()
                    rhs = expr[i + len(op):].strip()
                    try:
                        return lhs, op, float(rhs)
                    except ValueError:
                        # rhs 是另一个序列,例如复合表达式
                        return expr, ">", 0.0
        i += 1
    return expr, ">", 0.0


def slug(name: str) -> str:
    # 去除产品前缀,使 UID 形如 "e2b-<thing>" 而非 "e2b-e2b-<thing>"
    s = re.sub(r"^E2B", "", name)
    # 在连续大写字母与后续大写+小写之间切分("FirecrackerExporter" -> "Firecracker-Exporter")
    s = re.sub(r"([A-Z]+)([A-Z][a-z])", r"\1-\2", s)
    # 在小写/数字与后续大写之间切分("ExporterDown" -> "Exporter-Down")
    s = re.sub(r"([a-z0-9])([A-Z])", r"\1-\2", s)
    s = re.sub(r"[^a-zA-Z0-9]+", "-", s).strip("-").lower()
    return ("e2b-" + s)[:40]


def to_grafana_rule(rule: dict, prom_uid: str, folder_uid: str,
                    group_name: str, group_interval: str) -> dict:
    expr = rule["expr"]
    lhs, op, rhs = split_threshold(expr)
    grafana_op = {
        ">": ">", ">=": ">=", "<": "<", "<=": "<=", "==": "==", "!=": "!=",
    }[op]
    math_expr = f"$B {grafana_op} {rhs}"

    uid = rule.get("uid") or slug(rule["alert"])
    if not uid:
        raise ValueError(f"无法为告警生成 UID:'{rule['alert']}'。"
                         "中文等非 ASCII 名称请在 YAML 中显式提供 uid 字段。")

    return {
        "uid": uid,
        "title": rule["alert"],
        "ruleGroup": group_name,
        "folderUID": folder_uid,
        "noDataState": "OK",
        "execErrState": "Error",
        "for": rule.get("for", "0s"),
        "orgID": 1,
        "condition": "C",
        "data": [
            {
                "refId": "A",
                "queryType": "",
                "relativeTimeRange": {"from": 600, "to": 0},
                "datasourceUid": prom_uid,
                "model": {
                    "editorMode": "code",
                    "expr": lhs,
                    "instant": True,
                    "intervalMs": 1000,
                    "maxDataPoints": 43200,
                    "refId": "A",
                },
            },
            {
                "refId": "B",
                "queryType": "",
                "relativeTimeRange": {"from": 0, "to": 0},
                "datasourceUid": "__expr__",
                "model": {
                    "type": "reduce",
                    "expression": "A",
                    "reducer": "last",
                    "refId": "B",
                    "settings": {"mode": "dropNN"},
                },
            },
            {
                "refId": "C",
                "queryType": "",
                "relativeTimeRange": {"from": 0, "to": 0},
                "datasourceUid": "__expr__",
                "model": {
                    "type": "math",
                    "expression": math_expr,
                    "refId": "C",
                },
            },
        ],
        "annotations": rule.get("annotations", {}),
        "labels": rule.get("labels", {}),
        "isPaused": False,
    }


def upsert_rule(env: dict, rule: dict) -> str:
    uid = rule["uid"]
    code, _body = http("GET", f"{env['GRAFANA_URL']}/api/v1/provisioning/alert-rules/{uid}",
                       env)
    if code == 200:
        code, body = http("PUT", f"{env['GRAFANA_URL']}/api/v1/provisioning/alert-rules/{uid}",
                          env, body=rule)
        if code in (200, 202):
            return "updated"
    code, body = http("POST", f"{env['GRAFANA_URL']}/api/v1/provisioning/alert-rules",
                      env, body=rule)
    if code in (200, 201, 202):
        return "created"
    raise RuntimeError(f"rule {uid} push failed: {code} {body}")


def main() -> int:
    env = load_env()
    for k in ("GRAFANA_URL", "GRAFANA_USER", "GRAFANA_PASS"):
        if k not in env:
            print(f"{k} not set", file=sys.stderr)
            return 2
    ds_name = env.get("DS_NAME", "prometheus")
    folder_title = env.get("ALERT_FOLDER", "e2b-alerts")
    dry_run = os.environ.get("DRY_RUN", "0") == "1"

    prom_uid = get_ds_uid(env, ds_name)
    print(f"datasource '{ds_name}' uid: {prom_uid}")
    folder_uid = get_or_create_folder(env, folder_title)
    print(f"folder '{folder_title}' uid: {folder_uid}")

    text = ALERT_FILE.read_text()
    group_name, group_interval, rules = parse_rules(text)
    print(f"parsed {len(rules)} rules from {ALERT_FILE.relative_to(ROOT)} "
          f"(group={group_name}, interval={group_interval})")

    ok = fail = 0
    for r in rules:
        gr = to_grafana_rule(r, prom_uid, folder_uid, group_name, group_interval)
        if dry_run:
            print(f"  DRY_RUN  {gr['uid']:<48} for={gr['for']} cond={gr['data'][2]['model']['expression']}")
            ok += 1
            continue
        try:
            verb = upsert_rule(env, gr)
            print(f"  {verb:<8} {gr['uid']:<48} {r['alert']}")
            ok += 1
        except Exception as e:
            print(f"  FAIL     {gr['uid']:<48} {e}")
            fail += 1

    print(f"\n=== summary: {ok} ok, {fail} failed ===")
    return 1 if fail else 0


if __name__ == "__main__":
    sys.exit(main())
