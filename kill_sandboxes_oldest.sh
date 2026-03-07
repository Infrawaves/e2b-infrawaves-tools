#!/bin/bash
#
# kill_sandboxes_oldest.sh
# 按创建时间升序，删除指定模板下最早创建的 N 个沙箱。
#
# 用法: ./kill_sandboxes_oldest.sh <template_name> [num_to_delete]
#   template_name:   模板别名，用于过滤 e2b sandbox list（grep 匹配）
#   num_to_delete:   要删除的沙箱数量，默认为 5
#

TEMPLATE="${1:?Usage: $0 <template_name> [num_to_delete]}"
NUM_TO_DELETE="${2:-5}"

echo "=== E2B Sandbox Cleanup ==="
echo "Template: $TEMPLATE"
echo "Number to delete: $NUM_TO_DELETE"
echo ""

# 去掉 ANSI 颜色控制字符的函数
strip_ansi() {
  sed 's/\x1b\[[0-9;]*[a-zA-Z]//g'
}

# 获取沙箱列表（e2b sandbox list 输出本身已按创建时间升序排列）
# 取最前面的 N 个即为最早创建的
raw=$(e2b sandbox list 2>/dev/null | strip_ansi | grep "$TEMPLATE")
total=$(echo "$raw" | grep -c .)
sandboxes=$(echo "$raw" | head -n "$NUM_TO_DELETE")

echo "Found $total running '$TEMPLATE' sandboxes."

if [ "$total" -eq 0 ]; then
  echo "No sandboxes found. Nothing to do."
  exit 0
fi

if [ "$NUM_TO_DELETE" -ge "$total" ]; then
  echo "WARNING: Requested to delete $NUM_TO_DELETE but only $total exist."
  echo "Aborting to avoid deleting ALL sandboxes."
  exit 1
fi

echo ""
echo "Sandboxes to be deleted:"
echo "---------------------------------------------------"
echo "$sandboxes" | awk '{print "  " $1}'
echo ""
echo "Proceeding to delete..."
echo ""

echo "$sandboxes" | awk '{print $1}' | while read -r sandbox_id; do
  [ -z "$sandbox_id" ] && continue
  echo "Killing: $sandbox_id"
  e2b sandbox kill "$sandbox_id"
  if [ $? -eq 0 ]; then
    echo "  done"
  else
    echo "  failed"
  fi
done

echo ""
echo "Done."
