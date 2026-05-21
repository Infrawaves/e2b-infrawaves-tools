#!/usr/bin/env python3
"""针对在线 Prometheus 校验 Grafana dashboards 和告警规则。

从 .env 读取 PROM_URL 和 NODE_IP_SAMPLE,遍历 grafana/dashboards/**/*.json
和 grafana/alerts/*.yaml,提取每个 PromQL 表达式,并针对 Prometheus
/api/v1/query 执行,输出每个表达式的判定结果和汇总信息。

若所有表达式都返回 >=1 个序列(或本就是预期为空的计数器)则退出码为 0;
若有任何指标在 Prometheus 中不存在则退出码为 1。
"""
from __future__ import annotations

import json
import os
import re
import sys
import urllib.parse
import urllib.request
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
DASH_DIR = ROOT / "grafana" / "dashboards"
ALERT_DIR = ROOT / "grafana" / "alerts"


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
    for k in ("PROM_URL", "NODE_IP_SAMPLE"):
        if k in os.environ:
            env[k] = os.environ[k]
    return env


def prom_query(prom_url: str, expr: str) -> tuple[bool, int, str]:
    url = f"{prom_url.rstrip('/')}/api/v1/query?query={urllib.parse.quote(expr)}"
    try:
        with urllib.request.urlopen(url, timeout=10) as resp:
            body = json.load(resp)
    except Exception as e:
        return False, 0, f"http error: {e}"
    if body.get("status") != "success":
        return False, 0, f"prom error: {body.get('error', 'unknown')}"
    n = len(body["data"]["result"])
    return True, n, ""


def label_values(prom_url: str, label: str) -> list[str]:
    url = f"{prom_url.rstrip('/')}/api/v1/label/{label}/values"
    with urllib.request.urlopen(url, timeout=10) as resp:
        body = json.load(resp)
    return body.get("data", []) or []


METRIC_NAME_RE = re.compile(r"\b([a-zA-Z_:][a-zA-Z0-9_:]*)\s*(?:\{|\[|\()")
PROMQL_KEYWORDS = {
    "sum", "avg", "min", "max", "count", "rate", "irate", "increase",
    "topk", "bottomk", "by", "without", "on", "ignoring", "group_left",
    "group_right", "offset", "histogram_quantile", "label_replace",
    "absent", "absent_over_time", "delta", "deriv", "predict_linear",
    "stddev", "stdvar", "quantile", "round", "ceil", "floor", "abs",
    "and", "or", "unless", "if", "scalar", "vector", "time",
    "count_over_time", "sum_over_time", "avg_over_time", "max_over_time",
    "min_over_time", "rate_over_time", "changes", "resets",
}


def extract_metric_names(expr: str) -> set[str]:
    """启发式: 查找看起来像指标名的标识符。"""
    names = set()
    for m in METRIC_NAME_RE.finditer(expr):
        n = m.group(1)
        if n in PROMQL_KEYWORDS:
            continue
        if n[0].isdigit():
            continue
        if n in ("le", "node_ip"):
            continue
        names.add(n)
    return names


def walk_panels(panels):
    """递归遍历 rows / 子 panels。"""
    for p in panels or []:
        yield p
        for sub in walk_panels(p.get("panels")):
            yield sub


def extract_dashboard_exprs(path: Path):
    data = json.loads(path.read_text())
    out = []
    for panel in walk_panels(data.get("panels", [])):
        title = panel.get("title", "<no title>")
        for t in panel.get("targets") or []:
            expr = t.get("expr")
            if expr:
                out.append((title, expr))
    return out


def extract_alert_exprs(path: Path):
    """无外部依赖地解析告警 YAML(告警结构简单)。"""
    text = path.read_text()
    out = []
    cur_name = None
    cur_expr_lines: list[str] = []
    in_expr = False

    def flush():
        nonlocal cur_name, cur_expr_lines, in_expr
        if cur_name and cur_expr_lines:
            expr = " ".join(s.strip() for s in cur_expr_lines).strip()
            out.append((cur_name, expr))
        cur_expr_lines = []
        in_expr = False

    for line in text.splitlines():
        stripped = line.strip()
        m = re.match(r"^-?\s*alert:\s*(.+)$", stripped)
        if m:
            flush()
            cur_name = m.group(1).strip().strip("\"'")
            continue
        m = re.match(r"^expr:\s*(.*)$", stripped)
        if m:
            flush_value = m.group(1).strip()
            cur_expr_lines = []
            in_expr = True
            if flush_value and flush_value not in ("|", ">", "|-", ">-"):
                cur_expr_lines.append(flush_value.strip("\"'"))
                in_expr = False
            continue
        if in_expr:
            if re.match(r"^[a-zA-Z_]+:", stripped) or stripped.startswith("- "):
                in_expr = False
            else:
                if stripped:
                    cur_expr_lines.append(stripped)
    flush()
    return out


VAR_RE = re.compile(r"\$\{?([a-zA-Z_][a-zA-Z0-9_]*)\}?")


def substitute_vars(expr: str, node_ip: str) -> str:
    """将 $node_ip 替换为示例 IP;将其他任意 $var 替换为 ".+"。

    .+ 通配符可在 Grafana 模板展开成的 =~"..." 模式中正常匹配。
    """
    def repl(m):
        name = m.group(1)
        if name == "node_ip":
            return node_ip
        return ".+"
    return VAR_RE.sub(repl, expr)


def main() -> int:
    env = load_env()
    prom = env.get("PROM_URL")
    node_ip = env.get("NODE_IP_SAMPLE", "")
    if not prom:
        print("PROM_URL not set in .env", file=sys.stderr)
        return 2

    known_metrics = set()
    try:
        with urllib.request.urlopen(
            f"{prom.rstrip('/')}/api/v1/label/__name__/values", timeout=10
        ) as resp:
            known_metrics = set(json.load(resp).get("data", []))
    except Exception as e:
        print(f"failed to list metrics: {e}", file=sys.stderr)

    items: list[tuple[str, str, str, str]] = []  # (来源, 条目, 原始表达式, 解析后表达式)

    for path in sorted(DASH_DIR.rglob("*.json")):
        rel = path.relative_to(ROOT)
        for title, expr in extract_dashboard_exprs(path):
            items.append((str(rel), title, expr, substitute_vars(expr, node_ip)))

    for path in sorted(ALERT_DIR.glob("*.yaml")):
        rel = path.relative_to(ROOT)
        for name, expr in extract_alert_exprs(path):
            items.append((str(rel), f"alert:{name}", expr, substitute_vars(expr, node_ip)))

    print(f"Found {len(items)} expressions across "
          f"{len(list(DASH_DIR.rglob('*.json')))} dashboards "
          f"+ {len(list(ALERT_DIR.glob('*.yaml')))} alert files\n")

    ok = empty = missing = errored = 0
    rows = []

    for source, item, raw, resolved in items:
        used_metrics = extract_metric_names(resolved)
        unknown = sorted(m for m in used_metrics if m not in known_metrics)
        if unknown:
            verdict = f"MISSING METRIC: {', '.join(unknown)}"
            missing += 1
        else:
            success, n, err = prom_query(prom, resolved)
            if not success:
                verdict = f"ERROR: {err}"
                errored += 1
            elif n == 0:
                verdict = "EMPTY (0 series)"
                empty += 1
            else:
                verdict = f"OK ({n} series)"
                ok += 1
        rows.append((source, item, raw, verdict))

    sym = {"OK": "OK ", "EM": "WRN", "MI": "ERR", "ER": "ERR"}
    for source, item, raw, verdict in rows:
        tag = "OK" if verdict.startswith("OK") else "EM" if verdict.startswith("EMPTY") else "MI" if verdict.startswith("MISSING") else "ER"
        print(f"[{sym[tag]}] {source} :: {item}")
        print(f"      expr: {raw}")
        print(f"      -> {verdict}")
        print()

    total = ok + empty + missing + errored
    print("=" * 60)
    print(f"Total: {total}  OK: {ok}  Empty: {empty}  Missing-metric: {missing}  Errored: {errored}")
    return 1 if (missing + errored) else 0


if __name__ == "__main__":
    sys.exit(main())
