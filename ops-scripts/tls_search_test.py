#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""火山 TLS SearchLogs 签名调用测试 (用 ECSRole 临时凭证)"""
import json, hashlib, hmac, datetime, urllib.request, urllib.error

META = "http://100.96.0.96/latest/iam/security_credentials/TLSAccessECSRole"
REGION = "cn-hongkong"
SERVICE = "TLS"
HOST = "tls-cn-hongkong.ivolces.com"
TOPIC_ID = "1017a24d-a245-4e6a-830d-c19e76aeb705"


def get_cred():
    with urllib.request.urlopen(META, timeout=5) as r:
        d = json.loads(r.read())
    return d["AccessKeyId"], d["SecretAccessKey"], d["SessionToken"]


def _sign(key, msg):
    return hmac.new(key, msg.encode("utf-8"), hashlib.sha256).digest()


def search(ak, sk, token, query, start_ms, end_ms, limit=1):
    # 火山 TLS SearchLogs: POST /SearchLogs, body JSON
    body = json.dumps({
        "TopicId": TOPIC_ID,
        "Query": query,
        "StartTime": start_ms,
        "EndTime": end_ms,
        "Limit": limit,
    }, ensure_ascii=False)
    body_bytes = body.encode("utf-8")

    now = datetime.datetime.utcnow()
    xdate = now.strftime("%Y%m%dT%H%M%SZ")
    datestamp = now.strftime("%Y%m%d")
    payload_hash = hashlib.sha256(body_bytes).hexdigest()

    canonical_headers = (
        f"host:{HOST}\n"
        f"x-content-sha256:{payload_hash}\n"
        f"x-date:{xdate}\n"
    )
    signed_headers = "host;x-content-sha256;x-date"
    canonical_request = "\n".join([
        "POST", "/SearchLogs", "",
        canonical_headers, signed_headers, payload_hash,
    ])
    cred_scope = f"{datestamp}/{REGION}/{SERVICE}/request"
    string_to_sign = "\n".join([
        "HMAC-SHA256", xdate, cred_scope,
        hashlib.sha256(canonical_request.encode()).hexdigest(),
    ])
    k_date = _sign(sk.encode("utf-8"), datestamp)
    k_region = _sign(k_date, REGION)
    k_service = _sign(k_region, SERVICE)
    k_signing = _sign(k_service, "request")
    signature = hmac.new(k_signing, string_to_sign.encode(), hashlib.sha256).hexdigest()
    authz = (
        f"HMAC-SHA256 Credential={ak}/{cred_scope}, "
        f"SignedHeaders={signed_headers}, Signature={signature}"
    )
    req = urllib.request.Request(f"https://{HOST}/SearchLogs", data=body_bytes, method="POST")
    req.add_header("Host", HOST)
    req.add_header("X-Date", xdate)
    req.add_header("X-Content-Sha256", payload_hash)
    req.add_header("Content-Type", "application/json")
    req.add_header("Authorization", authz)
    req.add_header("X-Security-Token", token)
    try:
        with urllib.request.urlopen(req, timeout=15) as r:
            return r.status, json.loads(r.read())
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode("utf-8", "ignore")


if __name__ == "__main__":
    ak, sk, token = get_cred()
    print("ECSRole 临时凭证已获取 (AK 前缀:%s...)" % ak[:8])
    end = int(datetime.datetime.now().timestamp() * 1000)
    start = end - 3600 * 1000  # 近1h
    # 测试1: 简单检索 orchestration-api
    code, resp = search(ak, sk, token, 'service_name: orchestration-api', start, end, limit=1)
    print("=== 检索测试 HTTP", code, "===")
    if isinstance(resp, dict):
        print("ResultStatus:", resp.get("ResultStatus"))
        print("命中条数 HitCount/ListOver:", resp.get("HitCount"), resp.get("ListOver"))
        print("Analysis:", resp.get("Analysis"))
        logs = resp.get("Logs") or resp.get("AnalysisResult")
        print("返回样例:", json.dumps(logs, ensure_ascii=False)[:300] if logs else "(无)")
    else:
        print("响应:", str(resp)[:400])
