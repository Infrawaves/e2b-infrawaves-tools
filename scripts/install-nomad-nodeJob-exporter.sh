#!/bin/bash
set -e

REPO_OWNER="Infrawaves"
REPO_NAME="e2b-infrawaves-tools"
ASSET_NAME="nomad-nodeJob-exporter-linux-amd64"
INSTALL_PATH="/opt/nomad-nodeJob-exporter"
BINARY_NAME="nomad-nodeJob-exporter"
SERVICE_NAME="nomad-nodeJob-exporter"
SERVICE_FILE="/etc/systemd/system/${SERVICE_NAME}.service"

echo "=== Nomad NodeJob Exporter 安装/升级脚本 ==="
echo

# 检测当前是 install 还是 upgrade,后续路径上有差异
if [ -f "${INSTALL_PATH}/${BINARY_NAME}" ] || [ -f "$SERVICE_FILE" ]; then
  MODE="upgrade"
  echo "模式: 升级(检测到已有安装)"
else
  MODE="install"
  echo "模式: 全新安装"
fi
echo

# 1. 获取最新 Release 的下载链接
echo "1. 检查最新版本..."
AUTH_HEADER=()
if [ -n "${GH_TOKEN:-}" ]; then
  AUTH_HEADER=(-H "Authorization: token ${GH_TOKEN}")
fi

API_RESP=$(mktemp)
HTTP_CODE=$(curl -s -o "$API_RESP" -w "%{http_code}" \
  -H "Accept: application/vnd.github.v3+json" \
  "${AUTH_HEADER[@]}" \
  "https://api.github.com/repos/${REPO_OWNER}/${REPO_NAME}/releases/latest")

DOWNLOAD_URL=$(jq -r --arg asset "$ASSET_NAME" '.assets[]? | select(.name == $asset) | .browser_download_url' < "$API_RESP")

if [ -z "$DOWNLOAD_URL" ]; then
  echo "错误: 无法获取下载链接 (HTTP $HTTP_CODE)"
  echo "----- GitHub API response -----"
  cat "$API_RESP"
  echo
  echo "----- rate limit -----"
  curl -s "${AUTH_HEADER[@]}" https://api.github.com/rate_limit | jq '.resources.core' 2>&1 || true
  rm -f "$API_RESP"
  exit 1
fi
rm -f "$API_RESP"

echo "下载链接: $DOWNLOAD_URL"
echo

# 2. 检查是否已安装，备份原有文件
if [ -f "${INSTALL_PATH}/${BINARY_NAME}" ]; then
  echo "2. 检测到已安装版本，备份中..."
  BACKUP_FILE="${INSTALL_PATH}/${BINARY_NAME}.backup.$(date +%Y%m%d_%H%M%S)"
  cp "${INSTALL_PATH}/${BINARY_NAME}" "$BACKUP_FILE"
  echo "备份已保存到: $BACKUP_FILE"
  echo
else
  echo "2. 全新安装"
  echo
fi

# 3. 下载并安装二进制文件
echo "3. 下载二进制文件..."
TEMP_FILE="/tmp/${ASSET_NAME}"
curl -L -o "$TEMP_FILE" "$DOWNLOAD_URL"

echo "安装到: ${INSTALL_PATH}"
mkdir -p "$INSTALL_PATH"
chmod +x "$TEMP_FILE"
mv "$TEMP_FILE" "${INSTALL_PATH}/${BINARY_NAME}"
echo "安装完成"
echo

# 4. 获取 Nomad Token
# 升级时优先保留 systemd unit 里现有的 token(可能是手动改过的);
# 全新安装才从 nomad 配置回退读取。
echo "4. 获取 Nomad Token..."
NOMAD_TOKEN=""
NOMAD_CONFIG_PATH="/opt/nomad/config/default.hcl"

if [ -f "$SERVICE_FILE" ]; then
  EXISTING_TOKEN=$(grep -E '^Environment="NOMAD_TOKEN=' "$SERVICE_FILE" | sed -E 's/^Environment="NOMAD_TOKEN=([^"]*)".*/\1/')
  if [ -n "$EXISTING_TOKEN" ]; then
    NOMAD_TOKEN="$EXISTING_TOKEN"
    echo "✓ 沿用 $SERVICE_FILE 中已有的 Token (长度 ${#NOMAD_TOKEN})"
  fi
fi

if [ -z "$NOMAD_TOKEN" ] && [ -f "$NOMAD_CONFIG_PATH" ]; then
  NOMAD_TOKEN=$(grep -E '^\s*token\s*=\s*"([^"]+)"' "$NOMAD_CONFIG_PATH" | sed -E 's/^\s*token\s*=\s*"([^"]+)".*/\1/')
  if [ -n "$NOMAD_TOKEN" ]; then
    echo "✓ 从 $NOMAD_CONFIG_PATH 获取到 Token (长度 ${#NOMAD_TOKEN})"
  fi
fi

if [ -z "$NOMAD_TOKEN" ]; then
  echo "! 未找到 Token,请手动配置"
fi
echo

# 5. 创建 systemd 服务文件
echo "5. 配置 systemd 服务..."
cat > "$SERVICE_FILE" <<EOF
[Unit]
Description=Nomad Service Exporter
After=network.target

[Service]
Type=simple
User=root
Group=root
Restart=on-failure
RestartSec=5s
WorkingDirectory=/opt/nomad-nodeJob-exporter

# 添加 Nomad 地址和 Token
Environment="NOMAD_ADDR=http://127.0.0.1:4646"
Environment="NOMAD_TOKEN=$NOMAD_TOKEN"

ExecStart=/opt/nomad-nodeJob-exporter/nomad-nodeJob-exporter

[Install]
WantedBy=multi-user.target
EOF
echo "服务文件已创建: $SERVICE_FILE"
echo

# 6. 重载 systemd 配置
echo "6. 重载 systemd 配置..."
systemctl daemon-reload
echo

# 7. 启动服务
echo "7. 启动服务..."
if systemctl is-active --quiet "$SERVICE_NAME"; then
  systemctl restart "$SERVICE_NAME"
  echo "服务已重启"
else
  systemctl start "$SERVICE_NAME"
  echo "服务已启动"
fi

# 8. 设置开机自启
echo "8. 设置开机自启..."
systemctl enable "$SERVICE_NAME"
echo

# 9. 验证服务状态
echo "9. 验证服务状态..."
sleep 2
if systemctl is-active --quiet "$SERVICE_NAME"; then
  echo "✓ 服务运行正常"
else
  echo "✗ 服务启动失败"
  systemctl status "$SERVICE_NAME"
  exit 1
fi

# 10. 测试指标接口
echo
echo "10. 测试指标接口..."
if curl -s -f http://127.0.0.1:9106/metrics > /dev/null; then
  echo "✓ 指标接口正常 (http://127.0.0.1:9106/metrics)"
else
  echo "✗ 指标接口异常"
fi

echo
if [ "$MODE" = "upgrade" ]; then
  echo "=== 升级完成 ==="
  echo "如遇问题,可回滚: sudo cp ${BACKUP_FILE:-<backup>} ${INSTALL_PATH}/${BINARY_NAME} && sudo systemctl restart $SERVICE_NAME"
else
  echo "=== 安装完成 ==="
fi
echo

# 11. 检查 Token 配置
if [ -z "$NOMAD_TOKEN" ]; then
  echo "警告: 未找到 Nomad Token，请手动配置后重启服务:"
  echo "  sudo sed -i 's/NOMAD_TOKEN=/NOMAD_TOKEN=<your-token>/' $SERVICE_FILE"
  echo "  sudo systemctl restart $SERVICE_NAME"
fi
