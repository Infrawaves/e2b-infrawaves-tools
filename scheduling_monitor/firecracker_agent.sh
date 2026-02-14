#!/bin/bash
#
# firecracker_agent.sh
# 持续监控 Firecracker 进程并写入 NFS 共享目录的 Agent
#
# 用法:
#   ./firecracker_agent.sh                  # 默认每 30 秒采集一次
#   ./firecracker_agent.sh -i 10            # 每 10 秒采集一次
#   ./firecracker_agent.sh -d /mnt/nfs      # 指定输出目录
#   ./firecracker_agent.sh -i 5 -d /data    # 组合使用
#
# 输出文件:
#   <output_dir>/<node_ip>_<timestamp>.txt
#   每次写入会覆盖上一次的文件
#

set -euo pipefail

# ---- 默认配置 ----
INTERVAL=30           # 采集间隔（秒）
OUTPUT_DIR="/mnt/nfs" # 输出目录

# ---- 参数解析 ----
usage() {
    echo "用法: $0 [-i 间隔秒数] [-d 输出目录] [-h]"
    echo ""
    echo "选项:"
    echo "  -i  采集间隔，单位秒（默认: 30）"
    echo "  -d  输出目录（默认: /mnt/nfs）"
    echo "  -h  显示帮助"
    exit 0
}

while getopts "i:d:h" opt; do
    case $opt in
        i) INTERVAL="$OPTARG" ;;
        d) OUTPUT_DIR="$OPTARG" ;;
        h) usage ;;
        *) usage ;;
    esac
done

# ---- 获取本机 IP ----
get_node_ip() {
    # 优先使用 hostname -I（Linux），取第一个非回环 IP
    if command -v hostname &>/dev/null; then
        local ip
        ip=$(hostname -I 2>/dev/null | awk '{print $1}')
        if [[ -n "$ip" ]]; then
            echo "$ip"
            return
        fi
    fi
    # 回退: 通过 ip 命令获取
    if command -v ip &>/dev/null; then
        local ip
        ip=$(ip -4 addr show scope global | grep -oP '(?<=inet\s)\d+(\.\d+){3}' | head -1)
        if [[ -n "$ip" ]]; then
            echo "$ip"
            return
        fi
    fi
    # 最终回退
    echo "unknown"
}

NODE_IP=$(get_node_ip)
PREV_FILE=""

# ---- 启动信息 ----
echo "============================================"
echo " Firecracker Process Monitor Agent"
echo "============================================"
echo " Node IP:     $NODE_IP"
echo " Interval:    ${INTERVAL}s"
echo " Output Dir:  $OUTPUT_DIR"
echo " Started at:  $(date '+%Y-%m-%d %H:%M:%S')"
echo "============================================"

# 检查输出目录
if [[ ! -d "$OUTPUT_DIR" ]]; then
    echo "[WARN] 输出目录 $OUTPUT_DIR 不存在，尝试创建..."
    mkdir -p "$OUTPUT_DIR" || {
        echo "[ERROR] 无法创建输出目录 $OUTPUT_DIR"
        exit 1
    }
fi

# ---- 信号处理：优雅退出 ----
cleanup() {
    echo ""
    echo "[$(date '+%Y-%m-%d %H:%M:%S')] Agent 收到退出信号，正在停止..."
    # 可选：退出时清理最后一份文件
    # [[ -n "$PREV_FILE" && -f "$PREV_FILE" ]] && rm -f "$PREV_FILE"
    exit 0
}
trap cleanup SIGINT SIGTERM

# ---- 主循环 ----
echo "[$(date '+%Y-%m-%d %H:%M:%S')] Agent 开始运行..."

while true; do
    TIMESTAMP=$(date '+%Y%m%d_%H%M%S')
    OUTPUT_FILE="${OUTPUT_DIR}/${NODE_IP}.txt"

    # 采集 firecracker 进程信息并写入文件
    {
        echo "# Node: $NODE_IP"
        echo "# Collected at: $(date '+%Y-%m-%d %H:%M:%S')"
        echo "# ---"
        ps aux | grep -i firecracker | grep -v grep || echo "# No firecracker processes found"
    } > "$OUTPUT_FILE"

    # 删除上一次的文件（覆盖逻辑）
    if [[ -n "$PREV_FILE" && -f "$PREV_FILE" && "$PREV_FILE" != "$OUTPUT_FILE" ]]; then
        rm -f "$PREV_FILE"
    fi

    PREV_FILE="$OUTPUT_FILE"

    echo "[$(date '+%Y-%m-%d %H:%M:%S')] 已写入: $OUTPUT_FILE"

    sleep "$INTERVAL"
done
