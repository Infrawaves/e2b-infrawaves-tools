#!/usr/bin/env bash
# 通过 HTTP API 将 grafana/dashboards/**/*.json 推送到 Grafana 实例。
#
# - 从 .env 读取 GRAFANA_URL / GRAFANA_USER / GRAFANA_PASS
# - 保留 panel 中的 ${DS_PROMETHEUS} 引用,让 Grafana 运行时按 dashboard 顶部的
#   "数据源" 下拉框(DS_PROMETHEUS template 变量)动态解析,这样切换数据源会立即
#   生效;只有 dashboard 里 DS_PROMETHEUS 变量的 current 默认值会被设置成解析到
#   的 Prometheus UID,避免首次打开时变量为空
# - 通过名称解析 Prometheus 数据源 UID(默认 "prometheus") 用作 current 默认值
# - 同理解析 ClickHouse 数据源 UID(默认 "prod_e2b_clickhouse",可用 DS_CH_NAME 覆盖)
#   回填 DS_CLICKHOUSE 变量;目标实例无此数据源时跳过,不阻断同步
# - 同理解析 Loki 数据源 UID(默认 "Loki",可用 DS_LOKI_NAME 覆盖)回填 DS_LOKI
#   变量;目标实例无此数据源时跳过,不阻断同步
# - 确保文件夹存在(默认 "e2b"),然后以 overwrite=true 的方式 upsert 每个
#   dashboard;JSON 中的 uid 是幂等性的关键
#
# 用法:
#   scripts/sync-grafana.sh                # 推送全部(默认文件夹 "e2b")
#   FOLDER=e2b scripts/sync-grafana.sh fc-overview.json
#   DRY_RUN=1 scripts/sync-grafana.sh      # 仅校验,不写入

set -euo pipefail
cd "$(dirname "$0")/.."

if [ -f .env ]; then set -a; . ./.env; set +a; fi
: "${GRAFANA_URL:?GRAFANA_URL not set}"
: "${GRAFANA_USER:?GRAFANA_USER not set}"
: "${GRAFANA_PASS:?GRAFANA_PASS not set}"

DS_NAME="${DS_NAME:-prometheus}"
# ClickHouse 数据源名,用于回填 dashboard 顶部的 DS_CLICKHOUSE 变量 current 默认值。
# 目标实例若没有该数据源(例如纯 Prometheus 环境),解析失败不阻断同步。
DS_CH_NAME="${DS_CH_NAME:-prod_e2b_clickhouse}"
# Loki 数据源名,用于回填 dashboard 顶部的 DS_LOKI 变量 current 默认值。
# 目标实例若没有该数据源,解析失败不阻断同步。
DS_LOKI_NAME="${DS_LOKI_NAME:-Loki}"
FOLDER="${FOLDER:-e2b}"
DRY_RUN="${DRY_RUN:-0}"

curl_g() { curl -sS -u "$GRAFANA_USER:$GRAFANA_PASS" -H "Content-Type: application/json" "$@"; }

ds_uid="$(curl_g "$GRAFANA_URL/api/datasources/name/$DS_NAME" | python3 -c 'import json,sys;print(json.load(sys.stdin)["uid"])')"
echo "datasource '$DS_NAME' uid: $ds_uid"

# ClickHouse UID 可选解析:失败(数据源不存在/返回非 JSON)时置空,python 侧会跳过回填
ds_ch_uid="$(curl_g "$GRAFANA_URL/api/datasources/name/$DS_CH_NAME" 2>/dev/null | python3 -c 'import json,sys
try:
    print(json.load(sys.stdin).get("uid",""))
except Exception:
    print("")' 2>/dev/null || true)"
if [ -n "$ds_ch_uid" ]; then
  echo "datasource '$DS_CH_NAME' uid: $ds_ch_uid"
else
  echo "datasource '$DS_CH_NAME' not found (skip DS_CLICKHOUSE backfill)"
fi

# Loki UID 可选解析:失败(数据源不存在/返回非 JSON)时置空,python 侧会跳过回填
ds_loki_uid="$(curl_g "$GRAFANA_URL/api/datasources/name/$DS_LOKI_NAME" 2>/dev/null | python3 -c 'import json,sys
try:
    print(json.load(sys.stdin).get("uid",""))
except Exception:
    print("")' 2>/dev/null || true)"
if [ -n "$ds_loki_uid" ]; then
  echo "datasource '$DS_LOKI_NAME' uid: $ds_loki_uid"
else
  echo "datasource '$DS_LOKI_NAME' not found (skip DS_LOKI backfill)"
fi

folder_uid="$(curl_g "$GRAFANA_URL/api/folders" \
  | python3 -c "import json,sys; t='$FOLDER'; [print(x['uid']) for x in json.load(sys.stdin) if x['title']==t]" | head -n1)"
if [ -z "$folder_uid" ]; then
  echo "creating folder '$FOLDER'..."
  folder_uid="$(curl_g -X POST "$GRAFANA_URL/api/folders" \
    -d "{\"title\":\"$FOLDER\"}" \
    | python3 -c 'import json,sys;print(json.load(sys.stdin)["uid"])')"
fi
echo "folder '$FOLDER' uid: $folder_uid"

# 待推送的文件: grafana/dashboards/ 下的所有 JSON,或按命令行参数中的 basename 过滤
files=()
while IFS= read -r line; do files+=("$line"); done < <(find grafana/dashboards -type f -name '*.json' | sort)
if [ "$#" -gt 0 ]; then
  filtered=()
  for needle in "$@"; do
    for f in "${files[@]}"; do
      [ "$(basename "$f")" = "$needle" ] && filtered+=("$f")
    done
  done
  files=("${filtered[@]}")
fi

ok=0; fail=0
for f in "${files[@]}"; do
  echo
  echo "=== $f ==="
  payload="$(DASH_FILE="$f" DS_UID="$ds_uid" DS_NAME="$DS_NAME" DS_CH_UID="$ds_ch_uid" DS_CH_NAME="$DS_CH_NAME" DS_LOKI_UID="$ds_loki_uid" DS_LOKI_NAME="$DS_LOKI_NAME" FOLDER_UID="$folder_uid" python3 -c "
import json, os, sys
d = json.loads(open(os.environ['DASH_FILE']).read())
d['id'] = None
# 保留 panel 中的 \${DS_PROMETHEUS} / \${DS_CLICKHOUSE} 引用不动,Grafana 会按
# templating 变量的 current 解析。把 current 设成解析到的真实 UID,确保首次
# 打开 dashboard 时变量已有有效默认值;切换下拉框依然会让所有 panel 切到
# 新选中的数据源。
ds_uid = os.environ['DS_UID']
ds_name = os.environ['DS_NAME']
ds_ch_uid = os.environ.get('DS_CH_UID', '')
ds_ch_name = os.environ.get('DS_CH_NAME', '')
ds_loki_uid = os.environ.get('DS_LOKI_UID', '')
ds_loki_name = os.environ.get('DS_LOKI_NAME', '')
for v in d.get('templating', {}).get('list', []):
    if v.get('type') != 'datasource':
        continue
    if v.get('name') == 'DS_PROMETHEUS':
        v['current'] = {'selected': True, 'text': ds_name, 'value': ds_uid}
    elif v.get('name') == 'DS_CLICKHOUSE' and ds_ch_uid:
        v['current'] = {'selected': True, 'text': ds_ch_name, 'value': ds_ch_uid}
    elif v.get('name') == 'DS_LOKI' and ds_loki_uid:
        v['current'] = {'selected': True, 'text': ds_loki_name, 'value': ds_loki_uid}
print(json.dumps({'dashboard': d, 'folderUid': os.environ['FOLDER_UID'], 'overwrite': True, 'message': 'sync from repo'}))
")"
  if [ "$DRY_RUN" = "1" ]; then
    title="$(echo "$payload" | python3 -c "import json,sys;print(json.load(sys.stdin)['dashboard']['title'])" 2>/dev/null || echo unknown)"
    echo "  DRY_RUN: would push title='$title'"
    ok=$((ok+1)); continue
  fi
  resp="$(curl_g -X POST "$GRAFANA_URL/api/dashboards/db" -d "$payload")"
  if echo "$resp" | python3 -c 'import json,sys;d=json.load(sys.stdin); sys.exit(0 if d.get("status")=="success" else 1)' 2>/dev/null; then
    url="$(echo "$resp" | python3 -c 'import json,sys;print(json.load(sys.stdin).get("url",""))')"
    echo "  OK -> $GRAFANA_URL$url"
    ok=$((ok+1))
  else
    echo "  FAIL: $resp"
    fail=$((fail+1))
  fi
done

echo
echo "=== summary: $ok ok, $fail failed ==="
[ "$fail" -eq 0 ]
