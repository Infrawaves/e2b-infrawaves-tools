#!/bin/bash
set -e

REPO_OWNER="Infrawaves"
REPO_NAME="e2b-infrawaves-tools"
ASSET_NAME="nomad-nodeJob-exporter-linux-amd64"
INSTALL_PATH="/opt/nomad-nodeJob-exporter"
BINARY_NAME="nomad-nodeJob-exporter"
SERVICE_NAME="nomad-nodeJob-exporter"
SERVICE_FILE="/etc/systemd/system/${SERVICE_NAME}.service"

echo "=== Nomad NodeJob Exporter 安装脚本 ==="
echo

# 1. 获取最新 Release 的下载链接
echo "1. 检查最新版本..."
DOWNLOAD_URL=$(curl -s \
  -H "Accept: application/vnd.github.v3+json" \
  "https://api.github.com/repos/${REPO_OWNER}/${REPO_NAME}/releases/latest" |
  jq -r --arg asset "$ASSET_NAME" '.assets[] | select(.name == $asset) | .browser_download_url')

if [ -z "$DOWNLOAD_URL" ]; then
  echo "错误: 无法获取下载链接"
  exit 1
fi

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

# 4. 尝试从 Nomad 配置文件中获取 Token
echo "4. 尝试获取 Nomad Token..."
NOMAD_TOKEN=""
NOMAD_CONFIG_PATH="/opt/nomad/config/default.hcl"

if [ -f "$NOMAD_CONFIG_PATH" ]; then
  # 尝试从配置文件中提取 token
  NOMAD_TOKEN=$(grep -E '^\s*token\s*=\s*"([^"]+)"' "$NOMAD_CONFIG_PATH" | sed -E 's/^\s*token\s*=\s*"([^"]+)".*/\1/')
  if [ -n "$NOMAD_TOKEN" ]; then
    echo "✓ 从 $NOMAD_CONFIG_PATH 获取到 Token: ${NOMAD_TOKEN:0:16}..."
  else
    echo "! 未找到 Token，请手动配置"
  fi
else
  echo "! 未找到 Nomad 配置文件 $NOMAD_CONFIG_PATH，请手动配置"
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
echo "=== 安装完成 ==="
echo

# 11. 检查 Token 配置
if [ -z "$NOMAD_TOKEN" ]; then
  echo "警告: 未找到 Nomad Token，请手动配置后重启服务:"
  echo "  sudo sed -i 's/NOMAD_TOKEN=/NOMAD_TOKEN=<your-token>/' $SERVICE_FILE"
  echo "  sudo systemctl restart $SERVICE_NAME"
fi
