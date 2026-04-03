#!/bin/bash

# ==========================================
# 配置部分
# ==========================================

# 1. 设置二进制文件名称
BINARY_NAME="nomad-nodeJob-exporter"

# 2. 设置安装目录
INSTALL_DIR="/opt/nomad-nodeJob-exporter"

# 3. 设置服务名称
SERVICE_NAME="nomad-nodeJob-exporter"

# 4. 设置 Nomad Token (根据你的环境选择 dev 或 prod)
# 注意：在生产环境中，建议不要将 Token 硬编码在脚本中，可以通过参数传入或使用密钥管理工具
# NOMAD_TOKEN="91d967b9-cc7b-a61a-929e-c72ba465f631" # dev
# NOMAD_TOKEN="a5cd614a-1b91-a115-1714-df1e0d364231" # prod
NOMAD_TOKEN="ddf43f17-95f8-2253-c965-ea753bcbd652" # hk

# 5. 设置 Nomad 地址
NOMAD_ADDR="http://127.0.0.1:4646"

# 6. 设置 Metric 端口 (用于验证)
METRIC_PORT="9106"

# ==========================================
# 脚本逻辑开始
# ==========================================

echo ">>> 开始部署 ${SERVICE_NAME}..."

# 检查是否以 root 权限运行
if [ "$EUID" -ne 0 ]; then
  echo "错误：请使用 root 权限运行此脚本 (sudo ./deploy_exporter.sh)"
  exit 1
fi

# 1. 准备部署文件
echo ">>> 步骤 1/5: 创建目录 ${INSTALL_DIR}..."
mkdir -p ${INSTALL_DIR}

# 检查二进制文件是否存在 (假设二进制文件在当前目录下)
if [ ! -f "./${BINARY_NAME}" ]; then
    echo "错误：在当前目录下未找到二进制文件 ./${BINARY_NAME}"
    echo "请先编译 (go build ...) 并确保文件在脚本同级目录下。"
    exit 1
fi

echo ">>> 复制二进制文件到 ${INSTALL_DIR}..."
# 检查目标文件是否存在
if [ -f "${INSTALL_DIR}/${BINARY_NAME}" ]; then
    echo ">>> 检测到已存在文件，正在备份..."
    # 获取当前时间并格式化为 YYYYMMDDHHMMSS
    BACKUP_TIME=$(date +%Y%m%d%H%M%S)
    mv "${INSTALL_DIR}/${BINARY_NAME}" "${INSTALL_DIR}/${BINARY_NAME}.bak_${BACKUP_TIME}"
fi
cp ./${BINARY_NAME} ${INSTALL_DIR}/
chmod +x ${INSTALL_DIR}/${BINARY_NAME}

# 2. 创建 Systemd 服务文件
echo ">>> 步骤 2/5: 创建 Systemd 服务文件..."

cat > /etc/systemd/system/${SERVICE_NAME}.service <<EOF
[Unit]
Description=Nomad Service Exporter
After=network.target

[Service]
Type=simple
User=root
Group=root

# 设置重启策略
Restart=on-failure
RestartSec=5s

WorkingDirectory=${INSTALL_DIR}

# 添加 Nomad 地址和 Token
Environment="NOMAD_ADDR=${NOMAD_ADDR}"
Environment="NOMAD_TOKEN=${NOMAD_TOKEN}"

ExecStart=${INSTALL_DIR}/${BINARY_NAME}

[Install]
WantedBy=multi-user.target
EOF

# 3. 重载并启动服务
echo ">>> 步骤 3/5: 重载 systemd 配置..."
systemctl daemon-reload

echo ">>> 步骤 4/5: 启动服务并设置开机自启..."
systemctl enable ${SERVICE_NAME}
systemctl restart ${SERVICE_NAME}

# 4. 检查服务状态
echo ">>> 步骤 5/5: 检查服务状态..."
sleep 2 # 稍微等待一下让服务启动
if systemctl status ${SERVICE_NAME} | grep -q "active (running)"; then
    echo "✅ 服务启动成功!"
else
    echo "❌ 服务启动失败，请运行 'journalctl -u ${SERVICE_NAME} -f' 查看日志。"
    exit 1
fi

# 5. 验证服务
echo ">>> 正在验证 /metrics 接口..."
if curl http://127.0.0.1:${METRIC_PORT}/metrics; then
    echo "✅ 验证通过：Metrics 接口返回正常。"
    echo ">>> 部署完成！"
else
    echo "⚠️  警告：无法访问 Metrics 接口，请检查防火墙或服务日志。"
fi
