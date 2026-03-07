#!/bin/bash
# Kill all sandboxes matching a given template alias.
#
# Usage: ./kill_sandboxes_batch.sh <template_name>
#
# - template_name: 模板别名，用于过滤 e2b sandbox list 的结果（grep 匹配）。
# - 依赖 e2b CLI，需已登录且能执行 e2b sandbox list / e2b sandbox kill。
# - 当匹配到的 sandbox 数量 > 50 时，以每批 8 个并行执行 kill；否则逐个顺序执行。

set -e

TEMPLATE="${1:?Usage: $0 <template_name>}"
BATCH_SIZE=8
BATCH_THRESHOLD=50

echo "Fetching sandbox list for template: $TEMPLATE ..."
SANDBOX_IDS=$(e2b sandbox list 2>/dev/null | sed 's/\x1b\[[0-9;]*m//g' | grep "$TEMPLATE" | awk '{print $1}')

if [ -z "$SANDBOX_IDS" ]; then
    echo "No sandboxes found with template $TEMPLATE"
    exit 0
fi

COUNT=$(echo "$SANDBOX_IDS" | wc -l | tr -d ' ')
echo "Found $COUNT sandbox(es) with template $TEMPLATE"
echo ""

if [ "$COUNT" -gt "$BATCH_THRESHOLD" ]; then
    echo "Batch mode: killing $BATCH_SIZE sandboxes in parallel"
    echo ""
    i=0
    for sid in $SANDBOX_IDS; do
        echo "Killing sandbox: $sid"
        e2b sandbox kill "$sid" &
        i=$((i + 1))
        if [ $((i % BATCH_SIZE)) -eq 0 ]; then
            wait
        fi
    done
    wait
else
    for sid in $SANDBOX_IDS; do
        echo "Killing sandbox: $sid"
        e2b sandbox kill "$sid"
    done
fi

echo ""
echo "Done. Killed $COUNT sandbox(es)."
