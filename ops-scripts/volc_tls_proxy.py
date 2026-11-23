#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
火山 TLS SearchLogs 签名代理 (volc-tls-proxy)

目的:让 Grafana 的 infinity 数据源能查火山 TLS 日志。火山 API 要 HMAC-SHA256(V4)
签名,infinity 做不了;本服务用 ECS 机器角色(ECSRole)免密取临时凭证并签名转发。

用法(Grafana infinity 数据源):
  POST http://<本机IP>:19200/search
  body: {"topic":"<TopicId>","query":"<检索分析语句>","from":"<ms或now-1h>","to":"<ms或now>","limit":1000}
  返回: {"columns":[...], "rows":[...]}  —— 扁平化的聚合结果,infinity 直接用。

鉴权:ECSRole(TLSAccessECSRole)自动取临时 AK/SK/Token,带缓存与提前刷新,无长期密钥。
部署:Nomad job 或 systemd,跑在能访问 100.96.0.96(ECS元数据) 且 Grafana 可达的机器(146)。
"""
import json, hashlib, hmac, datetime, time, threading, re
import urllib.request, urllib.error
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

# ============ 配置 ============
META_BASE = "http://100.96.0.96/latest/iam/security_credentials/"
ROLE_NAME = "TLSAccessECSRole"
REGION = "cn-hongkong"
SERVICE = "TLS"
HOST = "tls-cn-hongkong.ivolces.com"
LISTEN_PORT = 19200
# 允许的 topic 白名单(防止代理被滥用查任意 topic);留空=不限制
ALLOWED_TOPICS = {
    "52861e07-f3f4-4eaa-a451-0a7999da5e37",  # ALB 访问日志 e2b-hk-prod-alb
    "1017a24d-a245-4e6a-830d-c19e76aeb705",  # orchestrator/e2b 日志
}

# ============ 凭证缓存 ============
_cred_lock = threading.Lock()
_cred_cache = {"ak": None, "sk": None, "token": None, "exp": 0}


def get_cred():
    """取 ECSRole 临时凭证,带缓存。过期前 5 分钟刷新。"""
    now = time.time()
    with _cred_lock:
        if _cred_cache["ak"] and now < _cred_cache["exp"] - 300:
            return _cred_cache["ak"], _cred_cache["sk"], _cred_cache["token"]
        with urllib.request.urlopen(META_BASE + ROLE_NAME, timeout=5) as r:
            d = json.loads(r.read())
        _cred_cache["ak"] = d["AccessKeyId"]
        _cred_cache["sk"] = d["SecretAccessKey"]
        _cred_cache["token"] = d["SessionToken"]
        # ExpiredTime 形如 2026-11-04T09:16:04Z
        exp = d.get("ExpiredTime")
        try:
            _cred_cache["exp"] = datetime.datetime.strptime(
                exp, "%Y-%m-%dT%H:%M:%SZ").replace(tzinfo=datetime.timezone.utc).timestamp()
        except Exception:
            _cred_cache["exp"] = now + 1800
        return _cred_cache["ak"], _cred_cache["sk"], _cred_cache["token"]


def _sign(key, msg):
    return hmac.new(key, msg.encode("utf-8"), hashlib.sha256).digest()


def tls_search(topic, query, start_ms, end_ms, limit):
    ak, sk, token = get_cred()
    body = json.dumps({
        "TopicId": topic, "Query": query,
        "StartTime": int(start_ms), "EndTime": int(end_ms), "Limit": int(limit),
    }, ensure_ascii=False)
    body_bytes = body.encode("utf-8")
    now = datetime.datetime.utcnow()
    xdate = now.strftime("%Y%m%dT%H%M%SZ")
    ds = now.strftime("%Y%m%d")
    ph = hashlib.sha256(body_bytes).hexdigest()
    ch = f"host:{HOST}\nx-content-sha256:{ph}\nx-date:{xdate}\n"
    sh = "host;x-content-sha256;x-date"
    cr = "\n".join(["POST", "/SearchLogs", "", ch, sh, ph])
    scope = f"{ds}/{REGION}/{SERVICE}/request"
    sts = "\n".join(["HMAC-SHA256", xdate, scope, hashlib.sha256(cr.encode()).hexdigest()])
    kd = _sign(sk.encode(), ds); kr = _sign(kd, REGION); ksv = _sign(kr, SERVICE); ks = _sign(ksv, "request")
    sig = hmac.new(ks, sts.encode(), hashlib.sha256).hexdigest()
    authz = f"HMAC-SHA256 Credential={ak}/{scope}, SignedHeaders={sh}, Signature={sig}"
    req = urllib.request.Request(f"https://{HOST}/SearchLogs", data=body_bytes, method="POST")
    req.add_header("Host", HOST); req.add_header("X-Date", xdate)
    req.add_header("X-Content-Sha256", ph); req.add_header("Content-Type", "application/json")
    req.add_header("Authorization", authz); req.add_header("X-Security-Token", token)
    with urllib.request.urlopen(req, timeout=30) as r:
        return json.loads(r.read())


def parse_time(v, default_ms):
    """支持毫秒时间戳、now、now-1h/now-30m/now-7d 等 Grafana 相对时间。"""
    if v is None or v == "":
        return default_ms
    v = str(v).strip()
    if v.isdigit():
        return int(v)
    now = int(time.time() * 1000)
    if v == "now":
        return now
    m = re.match(r"now-(\d+)([smhd])", v)
    if m:
        n = int(m.group(1)); unit = m.group(2)
        mult = {"s": 1000, "m": 60000, "h": 3600000, "d": 86400000}[unit]
        return now - n * mult
    return default_ms


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *a):
        pass  # 静默,避免刷屏

    def _send(self, code, obj):
        b = json.dumps(obj, ensure_ascii=False).encode("utf-8")
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(b)))
        self.end_headers()
        self.wfile.write(b)

    def do_GET(self):
        if self.path == "/health":
            self._send(200, {"status": "ok"})
        else:
            self._send(404, {"error": "not found"})

    def do_POST(self):
        if self.path != "/search":
            self._send(404, {"error": "use POST /search"})
            return
        try:
            n = int(self.headers.get("Content-Length", 0))
            payload = json.loads(self.rfile.read(n) or b"{}")
        except Exception as e:
            self._send(400, {"error": f"bad json: {e}"})
            return
        topic = payload.get("topic")
        query = payload.get("query", "*")
        # 单引号占位符:Grafana infinity 的 inline body 含单引号会把 query 截断,
        # 故前端用 ~SQ~ 代替 SQL 字符串字面量的单引号,代理侧还原为 '。
        query = query.replace("~SQ~", "'")
        if not topic:
            self._send(400, {"error": "missing topic"})
            return
        if ALLOWED_TOPICS and topic not in ALLOWED_TOPICS:
            self._send(403, {"error": "topic not allowed"})
            return
        now = int(time.time() * 1000)
        start = parse_time(payload.get("from"), now - 3600000)
        end = parse_time(payload.get("to"), now)
        limit = int(payload.get("limit", 1000))
        import sys as _sys
        print(f"[req] from={payload.get('from')!r} to={payload.get('to')!r} -> start={start} end={end} q={str(payload.get('query'))[:60]}", file=_sys.stderr, flush=True)
        try:
            resp = tls_search(topic, query, start, end, limit)
        except urllib.error.HTTPError as e:
            self._send(502, {"error": "tls http error", "detail": e.read().decode("utf-8", "ignore")[:300]})
            return
        except Exception as e:
            self._send(502, {"error": f"tls call failed: {e}"})
            return
        # 扁平化:聚合结果 AnalysisResult.Data -> rows;否则原始 Logs
        ar = resp.get("AnalysisResult") or {}
        data = ar.get("Data")
        if data is not None:
            cols = ar.get("Schema") or (list(data[0].keys()) if data else [])
            self._send(200, {"columns": cols, "rows": data,
                             "analysis": True, "hitCount": resp.get("HitCount")})
        else:
            logs = resp.get("Logs") or []
            self._send(200, {"columns": list(logs[0].keys()) if logs else [],
                             "rows": logs, "analysis": False, "hitCount": resp.get("HitCount")})


if __name__ == "__main__":
    # 启动时预取一次凭证,快速失败
    try:
        get_cred()
        print(f"[volc-tls-proxy] ECSRole 凭证 OK,监听 :{LISTEN_PORT}")
    except Exception as e:
        print(f"[volc-tls-proxy] 警告:启动时取凭证失败({e}),运行时重试")
    srv = ThreadingHTTPServer(("0.0.0.0", LISTEN_PORT), Handler)
    srv.serve_forever()
