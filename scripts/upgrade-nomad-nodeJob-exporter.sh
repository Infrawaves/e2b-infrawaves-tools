#!/bin/bash
set -e

REPO_OWNER="Infrawaves"
REPO_NAME="e2b-infrawaves-tools"
ASSET_NAME="nomad-nodeJob-exporter-linux-amd64"
INSTALL_PATH="/opt/nomad-nodeJob-exporter"
BINARY_NAME="nomad-nodeJob-exporter"
SERVICE_NAME="nomad-nodeJob-exporter"

echo "=== Nomad NodeJob Exporter 升级脚本 ==="
echo

# 1. 检查是否已安装
if [ ! -f "${INSTALL_PATH}/${BINARY_NAME}" ]; then
  echo "错误: 未检测到已安装版本，请先运行安装脚本"
  echo "执行: curl -fsSL https://raw.githubusercontent.com/Infrawaves/e2b-infrawaves-tools/gaomingxing/scripts/install-nomad-nodeJob-exporter.sh | sudo bash"
  exit 1
fi

# 2. 获取最新 Release 的下载链接
echo "1. 检查最新版本..."
DOWNLOAD_URL=$(curl -s \
  -H "Accept: application/vnd.github.v3+json" \
  "https://api.github.com/repos/${REPO_OWNER}/${REPO_NAME}/releases/tags/v1.0.0" |
  jq -r --arg asset "$ASSET_NAME" '.assets[] | select(.name == $asset) | .browser_download_url')

if [ -z "$DOWNLOAD_URL" ]; then
  echo "错误: 无法获取下载链接"
  exit 1
fi

echo "下载链接: $DOWNLOAD_URL"
echo

# 3. 备份现有版本
echo "2. 备份现有版本..."
BACKUP_FILE="${INSTALL_PATH}/${BINARY_NAME}.backup.$(date +%Y%m%d_%H%M%S)"
cp "${INSTALL_PATH}/${BINARY_NAME}" "$BACKUP_FILE"
echo "备份已保存到: $BACKUP_FILE"
echo

# 4. 下载并安装新版本
echo "3. 下载新版本..."
TEMP_FILE="/tmp/${ASSET_NAME}"
curl -L -o "$TEMP_FILE" "$DOWNLOAD_URL"

echo "安装到: ${INSTALL_PATH}"
chmod +x "$TEMP_FILE"
mv "$TEMP_FILE" "${INSTALL_PATH}/${BINARY_NAME}"
echo "安装完成"
echo

# 5. 重启服务
echo "4. 重启服务..."
systemctl restart "$SERVICE_NAME"
echo

# 6. 验证服务状态
echo "5. 验证服务状态..."
sleep 2
if systemctl is-active --quiet "$SERVICE_NAME"; then
  echo "✓ 服务运行正常"
else
  echo "✗ 服务启动失败"
  systemctl status "$SERVICE_NAME"
  exit 1
fi

# 7. 测试指标接口
echo
echo "6. 测试指标接口..."
if curl -s -f http://127.0.0.1:9106/metrics > /dev/null; then
  echo "✓ 指标接口正常 (http://127.0.0.1:9106/metrics)"
else
  echo "✗ 指标接口异常"
fi

echo
echo "=== 升级完成 ==="
echo
echo "备份文件: $BACKUP_FILE"
echo "如遇到问题，可回滚: sudo cp $BACKUP_FILE ${INSTALL_PATH}/${BINARY_NAME} && sudo systemctl restart $SERVICE_NAME"
