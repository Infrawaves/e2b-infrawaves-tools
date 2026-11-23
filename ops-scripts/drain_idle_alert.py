#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
drain_idle_alert.py

巡检指定状态节点上的沙箱, 多维度判定空闲并推送飞书告警。

设计要点:
  * 沙箱清单以 redis 为权威来源。
  * 多维度空闲判定:
    - 维度1 (Loki 日志):  N 分钟内无任何日志输出
    - 维度2 (CPU 指标):   Prometheus user CPU = 0 且 system CPU < 阈值
  * 综合判定: 日志空闲 且 (CPU空闲 或 无Prometheus数据) → 告警
    - 有 Prometheus 的节点: 双维度都空闲才告警(避免 Docker build 等误报)
    - 无 Prometheus 的新节点: 退化为纯日志判断(不因缺指标漏报)

数据来源:
  1) 节点状态:                admin REST API  GET /nodes
  2) 目标节点上的沙箱:       redis  key=sandbox:storage:*:sandboxes:*  (value JSON 含 nodeID)
  3) 沙箱近 N 分钟日志:      Loki   query_range  {sandboxID=~"..."}
  4) 沙箱 CPU 使用:          Prometheus  e2b_fc_process_cpu_seconds_total  (user/system)
  5) node_ip 展示补全:       Prometheus  e2b_sandbox_info  (按 node_id->node_ip)

判定: Loki 无日志 且 (CPU 不活跃 或 无 Prometheus 数据) → 告警。

去重: 状态文件记录每个 sandbox_id 的上次告警时间, COOLDOWN 内不重复推。

用法:
  python3 drain_idle_alert.py            # 巡检并按需推送飞书
  python3 drain_idle_alert.py --dry-run  # 只打印命中, 不推飞书、不写状态

依赖: 只用标准库 + 系统里的 redis-cli(已确认 ops 机上有)。
建议 crontab 每 5 分钟一次。
"""

import argparse
import base64
import json
import os
import subprocess
import sys
import time
import urllib.parse
import urllib.request

# ============================ 配置区 ============================

# --- 节点状态过滤 (admin API) ---
NODES_API      = os.environ.get("NODES_API", "http://192.168.0.146:3000/nodes")
ADMIN_TOKEN    = os.environ.get("ADMIN_TOKEN", "")  # 必填: orchestrator admin token, 从环境变量注入
NODE_STATUS    = os.environ.get("NODE_STATUS", "draining")  # draining / ready / 逗号分隔多个

# --- redis: 目标节点上的沙箱清单(权威来源) ---
REDIS_HOST    = os.environ.get("REDIS_HOST", "192.168.0.147")
REDIS_PORT    = os.environ.get("REDIS_PORT", "6379")
REDIS_PASS    = os.environ.get("REDIS_PASS", "")   # 无密码留空
REDIS_PATTERN = os.environ.get("REDIS_PATTERN", "sandbox:storage:*:sandboxes:*")
REDIS_CLI     = os.environ.get("REDIS_CLI", "redis-cli")

# --- Grafana (Loki + Prometheus 数据源代理) ---
GRAFANA_URL   = os.environ.get("GRAFANA_URL", "http://150.5.144.132:3000")
GRAFANA_USER  = os.environ.get("GRAFANA_USER", "admin")
GRAFANA_PASS  = os.environ.get("GRAFANA_PASS", "")  # 从环境变量注入, 勿硬编码

# Loki 数据源
LOKI_DS_UID   = os.environ.get("LOKI_DS_UID", "afo6wk07j06iob")
LOKI_QUERY_URL = os.environ.get(
    "LOKI_QUERY_URL",
    f"{GRAFANA_URL}/api/datasources/proxy/uid/{LOKI_DS_UID}/loki/api/v1/query_range",
)

# Prometheus 数据源 (仅用于 node_ip 补全)
PROM_DS_UID   = os.environ.get("PROM_DS_UID", "afoh4xrv0awowc")
PROM_QUERY_URL = os.environ.get(
    "PROM_QUERY_URL",
    f"{GRAFANA_URL}/api/datasources/proxy/uid/{PROM_DS_UID}/api/v1/query",
)

# --- 判定参数 ---
WINDOW_MIN   = int(os.environ.get("WINDOW_MIN", "10"))  # 日志观察窗口(分钟)
CPU_SYS_THRESHOLD = float(os.environ.get("CPU_SYS_THRESHOLD", "10"))  # system CPU 阈值(秒), 超过视为活跃

# --- 飞书 webhook (群机器人) ---
# 真实 webhook(含 bot token)勿入库,从环境变量 LARK_WEBHOOK 注入。
LARK_WEBHOOK = os.environ.get("LARK_WEBHOOK", "")

# --- 去重 ---
STATE_FILE   = os.environ.get("STATE_FILE", "/mnt/nfs/gaomingxing/ops/.drain_idle_state.json")
COOLDOWN_SEC = int(os.environ.get("COOLDOWN_SEC", "1800"))   # 同一沙箱重复告警冷却(秒)

HTTP_TIMEOUT = int(os.environ.get("HTTP_TIMEOUT", "30"))

# Loki 正则匹配单次最大沙箱数(URL 长度限制)
LOKI_BATCH   = int(os.environ.get("LOKI_BATCH", "80"))

# ============================ HTTP helpers ============================

def _grafana_headers():
    headers = {}
    if GRAFANA_USER:
        token = base64.b64encode(f"{GRAFANA_USER}:{GRAFANA_PASS}".encode()).decode()
        headers["Authorization"] = "Basic " + token
    return headers


def _http_get(url, headers=None, timeout=HTTP_TIMEOUT):
    req = urllib.request.Request(url, headers=headers or {})
    with urllib.request.urlopen(req, timeout=timeout) as r:
        return r.read().decode("utf-8")


def prom_query(promql):
    url = PROM_QUERY_URL + "?" + urllib.parse.urlencode({"query": promql})
    data = json.loads(_http_get(url, headers=_grafana_headers()))
    if data.get("status") != "success":
        raise RuntimeError(f"Prometheus query failed: {json.dumps(data)[:300]}")
    return data["data"]["result"]


def loki_query_range(logql, start_ns, end_ns, limit=1, direction="backward"):
    params = urllib.parse.urlencode({
        "query": logql,
        "start": str(start_ns),
        "end": str(end_ns),
        "limit": str(limit),
        "direction": direction,
    })
    url = LOKI_QUERY_URL + "?" + params
    data = json.loads(_http_get(url, headers=_grafana_headers()))
    if data.get("status") != "success":
        raise RuntimeError(f"Loki query failed: {json.dumps(data)[:300]}")
    return data["data"]["result"]


# ============================ 数据获取 ============================

def get_target_nodes():
    """返回 {node_id: sandboxCount}, 按 NODE_STATUS 过滤。"""
    raw = _http_get(NODES_API, headers={"X-Admin-Token": ADMIN_TOKEN})
    nodes = json.loads(raw)
    wanted = {s.strip().lower() for s in NODE_STATUS.split(",")}
    return {
        n["id"]: n.get("sandboxCount", 0)
        for n in nodes
        if n.get("status", "").lower() in wanted
    }


def _redis_cli(*args):
    cmd = [REDIS_CLI, "-h", REDIS_HOST, "-p", str(REDIS_PORT)]
    if REDIS_PASS:
        cmd += ["-a", REDIS_PASS]
    cmd += list(args)
    return subprocess.run(cmd, capture_output=True, text=True, timeout=HTTP_TIMEOUT)


def get_sandboxes_from_redis(target_ids):
    """
    扫描 redis 里所有沙箱, 过滤出 nodeID 在目标集合中的。
    返回 {sandbox_id: {sandbox_id, node_id, alias, vcpu, ram_mb, start, end}}
    """
    if not target_ids:
        return {}
    scan = _redis_cli("--scan", "--pattern", REDIS_PATTERN)
    if scan.returncode != 0:
        raise RuntimeError(f"redis --scan 失败: {scan.stderr.strip()[:300]}")
    keys = [k for k in scan.stdout.splitlines() if k.strip()]
    out = {}
    CHUNK = 200
    for i in range(0, len(keys), CHUNK):
        batch = keys[i:i + CHUNK]
        res = _redis_cli("MGET", *batch)
        if res.returncode != 0:
            continue
        for val in res.stdout.splitlines():
            val = val.strip()
            if not val or val == "":
                continue
            try:
                obj = json.loads(val)
            except ValueError:
                continue
            nid = obj.get("nodeID") or obj.get("nodeId")
            sid = obj.get("sandboxID") or obj.get("sandboxId")
            if not sid or nid not in target_ids:
                continue
            out[sid] = {
                "sandbox_id": sid,
                "node_id": nid,
                "alias": obj.get("alias", ""),
                "vcpu": obj.get("vCpu"),
                "ram_mb": obj.get("ramMB"),
                "start": obj.get("startTime", ""),
                "end": obj.get("endTime", ""),
            }
    return out


def get_idle_sandboxes_via_loki(sandbox_ids):
    """
    用 Loki 批量查询, 返回在 WINDOW_MIN 内无日志输出的沙箱 ID 集合。
    分批查询以避免 URL 过长。
    """
    if not sandbox_ids:
        return set()
    now_ns = int(time.time()) * 1_000_000_000
    start_ns = now_ns - WINDOW_MIN * 60 * 1_000_000_000

    sids = list(sandbox_ids)
    has_logs = set()

    for i in range(0, len(sids), LOKI_BATCH):
        batch = sids[i:i + LOKI_BATCH]
        regex = "|".join(batch)
        logql = '{sandboxID=~"' + regex + '"}'
        try:
            results = loki_query_range(logql, start_ns, now_ns, limit=1)
            for r in results:
                sid = r.get("stream", {}).get("sandboxID", "")
                if sid:
                    has_logs.add(sid)
        except Exception as e:
            print(f"[warn] Loki 批量查询失败(batch {i}): {e}", file=sys.stderr)

    return set(sids) - has_logs


def get_cpu_active_sandboxes(sandbox_ids):
    """
    查询 Prometheus, 返回 (cpu_active: set, has_prom_data: set)。
    cpu_active:    窗口内 user CPU > 0 或 system CPU > 阈值的沙箱
    has_prom_data: 在 Prometheus 中有 CPU 数据的沙箱
    """
    cpu_active = set()
    has_prom_data = set()
    if not sandbox_ids:
        return cpu_active, has_prom_data
    try:
        # user CPU
        for r in prom_query(f'increase(e2b_fc_process_cpu_seconds_total{{mode="user"}}[{WINDOW_MIN}m])'):
            sid = r["metric"].get("sandbox_id", "")
            if sid in sandbox_ids:
                has_prom_data.add(sid)
                if float(r["value"][1]) > 0:
                    cpu_active.add(sid)
        # system CPU
        for r in prom_query(f'increase(e2b_fc_process_cpu_seconds_total{{mode="system"}}[{WINDOW_MIN}m])'):
            sid = r["metric"].get("sandbox_id", "")
            if sid in sandbox_ids:
                has_prom_data.add(sid)
                if float(r["value"][1]) > CPU_SYS_THRESHOLD:
                    cpu_active.add(sid)
    except Exception as e:
        print(f"[warn] Prometheus CPU 查询失败(退化为纯日志判断): {e}", file=sys.stderr)
    return cpu_active, has_prom_data


def get_node_ip_map():
    """{node_id: node_ip}, 用于展示补全(来自 Prometheus e2b_sandbox_info)。"""
    out = {}
    try:
        for s in prom_query("e2b_sandbox_info"):
            m = s["metric"]
            if m.get("node_id") and m.get("node_ip"):
                out[m["node_id"]] = m["node_ip"]
    except Exception as e:  # noqa: BLE001 - 补全失败不影响主流程
        print(f"[warn] node_ip 补全失败: {e}", file=sys.stderr)
    return out


# ============================ 状态 / 通知 ============================

def load_state():
    try:
        with open(STATE_FILE) as f:
            return json.load(f)
    except (OSError, ValueError):
        return {}


def save_state(state):
    tmp = STATE_FILE + ".tmp"
    with open(tmp, "w") as f:
        json.dump(state, f, indent=2)
    os.replace(tmp, STATE_FILE)


def send_lark(hits):
    if not LARK_WEBHOOK:
        print("[warn] LARK_WEBHOOK 未配置, 跳过推送", file=sys.stderr)
        return
    lines = []
    for h in hits:
        node_ip = h.get("node_ip") or "未知"
        node = f"{h['node_id']} ({node_ip})"
        alias = f"\n  alias: {h['alias']}" if h.get("alias") else ""
        res = ""
        if h.get("vcpu") or h.get("ram_mb"):
            res = f"\n  资源: {h.get('vcpu', '?')}vCPU / {h.get('ram_mb', '?')}MB"
        time_info = ""
        if h.get("start"):
            time_info = f"\n  启动: {h['start'][:19]}Z"
        if h.get("end"):
            time_info += f"  到期: {h['end'][:19]}Z"
        reason = h.get("reason", f"{WINDOW_MIN}分钟内无日志输出")
        lines.append(
            f"• sandbox: {h['sandbox_id']}{alias}\n"
            f"  node: {node}{res}{time_info}\n"
            f"  原因: {reason}"
        )
    status_label = NODE_STATUS.upper()
    text = f"⚠️ [{status_label}] 节点上发现空闲/无输出沙箱 ({len(hits)} 个)\n\n" + "\n\n".join(lines)
    payload = json.dumps({"msg_type": "text", "content": {"text": text}}).encode()
    req = urllib.request.Request(LARK_WEBHOOK, data=payload,
                                headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=HTTP_TIMEOUT) as r:
        print("[info] 飞书推送:", r.read().decode()[:200])


# ============================ 主流程 ============================

def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--dry-run", action="store_true", help="只打印, 不推飞书、不写状态")
    args = ap.parse_args()

    print(f"[info] 过滤节点状态: {NODE_STATUS}, 日志窗口: {WINDOW_MIN}min")

    target_nodes = get_target_nodes()
    if not target_nodes:
        print(f"[info] 当前没有 {NODE_STATUS} 状态的节点, 结束。")
        return
    print(f"[info] 目标节点 ({len(target_nodes)}): {target_nodes}")

    sandboxes = get_sandboxes_from_redis(set(target_nodes))
    print(f"[info] redis 查到目标节点上沙箱 {len(sandboxes)} 个")

    # 一致性检查
    found_per_node = {}
    for info in sandboxes.values():
        found_per_node[info["node_id"]] = found_per_node.get(info["node_id"], 0) + 1
    for nid, cnt in target_nodes.items():
        got = found_per_node.get(nid, 0)
        if got != cnt:
            print(f"[warn] 节点 {nid}: API sandboxCount={cnt}, redis 查到 {got}")

    if not sandboxes:
        print("[info] 目标节点上无沙箱, 结束。")
        return

    all_sids = set(sandboxes.keys())

    # 维度1: Loki 日志
    loki_idle_sids = get_idle_sandboxes_via_loki(all_sids)
    print(f"[info] Loki: {WINDOW_MIN}min 内无日志 {len(loki_idle_sids)}/{len(sandboxes)}")

    # 维度2: Prometheus CPU
    cpu_active, has_prom = get_cpu_active_sandboxes(all_sids)
    print(f"[info] Prometheus: 有CPU数据 {len(has_prom)}, CPU活跃 {len(cpu_active)}")

    # 综合判定: 日志空闲 且 (CPU不活跃 或 无Prometheus数据)
    final_idle = set()
    for sid in loki_idle_sids:
        if sid in cpu_active:
            continue  # CPU 活跃, 跳过(可能在做 Docker build 等)
        final_idle.add(sid)

    # 统计被 CPU 维度救回的沙箱
    rescued = loki_idle_sids - final_idle
    if rescued:
        print(f"[info] CPU活跃排除(日志空闲但CPU忙): {len(rescued)} 个")
        for sid in list(rescued)[:10]:
            print(f"    排除: {sid}  alias={sandboxes[sid].get('alias','')}")

    print(f"[info] 最终空闲沙箱: {len(final_idle)}/{len(sandboxes)}")

    # node_ip 补全
    ip_map = get_node_ip_map()

    hits = []
    for sid in final_idle:
        info = dict(sandboxes[sid])
        info["node_ip"] = ip_map.get(info["node_id"], "")
        # 标记判定依据
        if sid in has_prom:
            info["reason"] = f"{WINDOW_MIN}min内无日志+CPU空闲"
        else:
            info["reason"] = f"{WINDOW_MIN}min内无日志(无CPU数据)"
        hits.append(info)

    if not hits:
        print("[info] 所有沙箱均有日志输出, 无告警。")
        if not args.dry_run:
            save_state({})
        return

    # 按 node_id 排序, 方便查看
    hits.sort(key=lambda h: (h["node_id"], h["sandbox_id"]))

    print(f"[info] 命中 {len(hits)} 个:")
    for h in hits:
        print(f"    {h['sandbox_id']}  {h['node_id']}({h.get('node_ip','')})  alias={h.get('alias','')}")

    if args.dry_run:
        print("[dry-run] 不推送、不写状态。")
        return

    now = time.time()
    state = load_state()
    fresh, new_state = [], {}
    for h in hits:
        key = h["sandbox_id"]
        last = state.get(key)
        if last is None or (now - last) >= COOLDOWN_SEC:
            fresh.append(h)
            new_state[key] = now
        else:
            new_state[key] = last
    save_state(new_state)

    if fresh:
        print(f"[info] 需推送(过冷却) {len(fresh)} 个")
        send_lark(fresh)
    else:
        print("[info] 命中的沙箱都在冷却期内, 本轮不推。")


if __name__ == "__main__":
    main()
