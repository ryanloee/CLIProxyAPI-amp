#!/bin/bash
# CLIProxyAPI Build Script
# 编译并打包所有输出产物

set -e

PROJECT_DIR="$(cd "$(dirname "$0")" && pwd)"
BINARY_NAME="cli-proxy-api"
VERSION=${VERSION:-"dev-$(date +%Y%m%d%H%M%S)"}
OUTPUT_DIR="${PROJECT_DIR}/dist"

export PATH="/c/Go/bin:$PATH"

echo "========================================="
echo " CLIProxyAPI Build Script"
echo " Version: ${VERSION}"
echo "========================================="

# 清理旧产物
echo "[1/5] Cleaning old artifacts..."
rm -rf "${OUTPUT_DIR}"
mkdir -p "${OUTPUT_DIR}"

# 编译
echo "[2/5] Building..."
cd "${PROJECT_DIR}"

BUILD_FLAGS="-s -w -X main.Version=${VERSION}"
BUILD_TAGS=""

echo "  Building for windows/amd64..."
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build \
    -ldflags "${BUILD_FLAGS}" \
    ${BUILD_TAGS} \
    -o "${OUTPUT_DIR}/${BINARY_NAME}.exe" \
    ./cmd/server/

echo "  Build successful: ${OUTPUT_DIR}/${BINARY_NAME}.exe"

# 复制配置文件
echo "[3/5] Copying config files..."
if [ -f "config.example.yaml" ]; then
    cp config.example.yaml "${OUTPUT_DIR}/config.example.yaml"
    echo "  Copied config.example.yaml"
else
    echo "  Warning: config.example.yaml not found"
fi

# 创建默认空配置
if [ ! -f "${OUTPUT_DIR}/config.yaml" ]; then
    cat > "${OUTPUT_DIR}/config.yaml" << 'CFGEOF'
# CLIProxyAPI Configuration
# See config.example.yaml for full options

port: 8317
debug: false
auth-dir: "~/.cli-proxy-api"

api-keys:
  - "your-api-key-here"

remote-management:
  allow-remote: false
  secret-key: ""
  disable-control-panel: false

# Codebuddy (Tencent) API Key
# codebuddy-api-key:
#   - api-key: "your-codebuddy-api-key"
#     base-url: "https://api.lkeap.cloud.tencent.com/coding/v3"

# OAuth login: use *-login.bat/sh scripts or --xxx-login flag
CFGEOF
    echo "  Created default config.yaml"
fi

# 复制文档
echo "[4/5] Copying documentation..."
if [ -f "README.md" ]; then
    cp README.md "${OUTPUT_DIR}/README.md"
fi
if [ -f "LICENSE" ]; then
    cp LICENSE "${OUTPUT_DIR}/LICENSE"
fi

# 创建启动脚本和所有 OAuth 登录脚本
echo "[5/5] Creating startup scripts..."

# 主启动脚本
cat > "${OUTPUT_DIR}/start.bat" << 'EOF'
@echo off
chcp 65001 >nul 2>&1
echo Starting CLIProxyAPI...
cli-proxy-api.exe -config config.yaml
pause
EOF

cat > "${OUTPUT_DIR}/start.sh" << 'EOF'
#!/bin/bash
echo "Starting CLIProxyAPI..."
./cli-proxy-api -config config.yaml
EOF
chmod +x "${OUTPUT_DIR}/start.sh"

# 统一生成所有 OAuth 登录脚本
create_login_script() {
    local provider="$1"
    local flag="$2"
    local desc="$3"

    cat > "${OUTPUT_DIR}/${provider}-login.bat" << BATEOF
@echo off
chcp 65001 >nul 2>&1
echo ${desc} OAuth Login...
cli-proxy-api.exe -config config.yaml ${flag}
pause
BATEOF

    cat > "${OUTPUT_DIR}/${provider}-login.sh" << SHEOF
#!/bin/bash
echo "${desc} OAuth Login..."
./cli-proxy-api -config config.yaml ${flag}
SHEOF
    chmod +x "${OUTPUT_DIR}/${provider}-login.sh"
}

create_login_script "gemini"      "-login"                "Gemini"
create_login_script "claude"      "-claude-login"         "Claude"
create_login_script "codex"       "-codex-login"          "Codex"
create_login_script "antigravity" "-antigravity-login"    "Antigravity"
create_login_script "kimi"        "-kimi-login"           "Kimi"
create_login_script "codebuddy"   "-codebuddy-login"      "Codebuddy"

# 打包
echo ""
echo "========================================="
echo " Build Complete!"
echo "========================================="
echo ""
echo " Output directory: ${OUTPUT_DIR}/"
echo ""
ls -lh "${OUTPUT_DIR}/"
echo ""
echo " Usage:"
echo "   1. Edit config.yaml with your API keys"
echo "   2. Run start.bat (Windows) or start.sh (Linux/Mac)"
echo "   3. OAuth login: run *-login.bat (e.g. codebuddy-login.bat)"
echo "   4. Management panel: http://localhost:8317/management.html"
echo ""

# 可选：创建 zip 包
if [ "$1" = "--zip" ] || [ "$1" = "-z" ]; then
    ZIP_NAME="cli-proxy-api-windows-amd64-${VERSION}.zip"
    echo "Creating zip archive: ${ZIP_NAME}"
    cd "${OUTPUT_DIR}"
    zip -r "${PROJECT_DIR}/${ZIP_NAME}" ./*
    cd "${PROJECT_DIR}"
    echo "Archive created: ${PROJECT_DIR}/${ZIP_NAME}"
    ls -lh "${PROJECT_DIR}/${ZIP_NAME}"
fi
