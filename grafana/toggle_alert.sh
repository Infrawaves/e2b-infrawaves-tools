#!/usr/bin/env bash
# toggle_alert.sh — 暂停/启用/查看 Grafana 告警规则的 isPaused 状态
#
# 用法:
#   ./toggle_alert.sh <规则UID> <pause|resume|status>
#
# 示例:
#   ./toggle_alert.sh e2b-node-draining-empty status    # 查看当前 isPaused
#   ./toggle_alert.sh e2b-node-draining-empty pause     # 暂停(isPaused=true)
#   ./toggle_alert.sh e2b-node-draining-empty resume    # 启用(isPaused=false)
#
# 凭证(两种方式任选):
#   1) 环境变量:  export GF_URL=... GF_USER=admin GF_PASS=...
#   2) 不设 GF_PASS 时,脚本会用 read -s 交互式安全读取(不回显、不留 history)
#
# 说明:
#   * provisioning API 的 PUT 要求提交完整规则体,脚本先 GET 再改单字段回写,
#     不会误改查询/阈值/for。
#   * 必须带 X-Disable-Provenance 头,否则 Provisioned 规则会拒绝修改(403)。
#   * 状态 Normal/Pending/Firing 是评估结果,API 改不了;本脚本只控制 isPaused。

set -euo pipefail

# ---------------- 参数 ----------------
RULE_UID="${1:-}"
ACTION="${2:-}"

if [[ -z "$RULE_UID" || -z "$ACTION" ]]; then
  echo "用法: $0 <规则UID> <pause|resume|status>" >&2
  exit 2
fi

case "$ACTION" in
  pause|resume|status) ;;
  *) echo "错误: action 必须是 pause / resume / status,收到 '$ACTION'" >&2; exit 2 ;;
esac

# ---------------- 配置 / 凭证 ----------------
# GF_URL="${GF_URL:-http://150.5.144.132:3000}"
GF_URL="${GF_URL:-https://grafana.hk-prod-e2b.xiaobei.top}"
GF_USER="${GF_USER:-admin}"

if [[ -z "${GF_PASS:-}" ]]; then
  read -rs -p "Grafana 密码 (${GF_USER}@${GF_URL}): " GF_PASS
  echo
fi

API="${GF_URL}/api/v1/provisioning/alert-rules/${RULE_UID}"
TMP_CUR="$(mktemp /tmp/alert_rule.XXXXXX.json)"
TMP_NEW="$(mktemp /tmp/alert_rule_new.XXXXXX.json)"
TMP_RESP="$(mktemp /tmp/alert_rule_resp.XXXXXX.json)"
cleanup() { rm -f "$TMP_CUR" "$TMP_NEW" "$TMP_RESP"; }
trap cleanup EXIT

# ---------------- 1. 读取当前规则 ----------------
code=$(curl -s -u "${GF_USER}:${GF_PASS}" "$API" -o "$TMP_CUR" -w "%{http_code}")
if [[ "$code" != "200" ]]; then
  echo "读取规则失败: HTTP $code" >&2
  cat "$TMP_CUR" >&2 || true
  exit 1
fi

cur_paused=$(python3 -c "import json;print(json.load(open('$TMP_CUR')).get('isPaused'))")
title=$(python3 -c "import json;print(json.load(open('$TMP_CUR')).get('title'))")
echo "规则: $title"
echo "UID : $RULE_UID"
echo "当前 isPaused = $cur_paused"

# ---------------- status: 只读,到此为止 ----------------
if [[ "$ACTION" == "status" ]]; then
  exit 0
fi

# ---------------- 2. 计算目标值 ----------------
if [[ "$ACTION" == "pause" ]]; then
  target="True"
else
  target="False"
fi

if [[ "$cur_paused" == "$target" ]]; then
  echo "无需修改: isPaused 已是 $target"
  exit 0
fi

# ---------------- 3. 改单字段并 PUT ----------------
TARGET="$target" python3 -c "
import json, os
d = json.load(open('$TMP_CUR'))
d['isPaused'] = (os.environ['TARGET'] == 'True')
json.dump(d, open('$TMP_NEW', 'w'), ensure_ascii=False)
"

code=$(curl -s -u "${GF_USER}:${GF_PASS}" -X PUT "$API" \
  -H 'Content-Type: application/json' \
  -H 'X-Disable-Provenance: true' \
  --data-binary @"$TMP_NEW" \
  -o "$TMP_RESP" -w "%{http_code}")

if [[ "$code" != "200" ]]; then
  echo "写入失败: HTTP $code" >&2
  cat "$TMP_RESP" >&2 || true
  exit 1
fi

# ---------------- 4. 独立回读确认 ----------------
new_paused=$(curl -s -u "${GF_USER}:${GF_PASS}" "$API" \
  | python3 -c "import json,sys;print(json.load(sys.stdin).get('isPaused'))")

echo "写入成功 (HTTP 200)"
echo "现在 isPaused = $new_paused"

if [[ "$new_paused" == "$target" ]]; then
  echo "✓ 已${ACTION}: $title"
else
  echo "⚠ 回读值 ($new_paused) 与目标 ($target) 不一致,请检查" >&2
  exit 1
fi
