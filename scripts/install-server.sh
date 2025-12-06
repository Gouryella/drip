#!/bin/bash
set -e

# ============================================================================
# Configuration
# ============================================================================
GITHUB_REPO="Gouryella/drip"
INSTALL_DIR="${INSTALL_DIR:-/usr/local/bin}"
SERVICE_USER="${SERVICE_USER:-drip}"
CONFIG_DIR="/etc/drip"
WORK_DIR="/var/lib/drip"
VERSION="${VERSION:-}"
COMMAND_MADE_AVAILABLE=false

# Default values
DEFAULT_PORT=8443
DEFAULT_TCP_PORT_MIN=20000
DEFAULT_TCP_PORT_MAX=40000

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
CYAN='\033[0;36m'
BOLD='\033[1m'
NC='\033[0m'

# Language (default: en)
LANG_CODE="${LANG_CODE:-en}"

# ============================================================================
# Internationalization
# ============================================================================
declare -A MSG_EN
declare -A MSG_ZH

# English messages
MSG_EN=(
    ["banner_title"]="Drip Server - One-Click Installer"
    ["select_lang"]="Select language / 选择语言"
    ["lang_en"]="English"
    ["lang_zh"]="中文"
    ["checking_root"]="Checking root privileges..."
    ["need_root"]="This script requires root privileges. Please run with sudo."
    ["checking_os"]="Checking operating system..."
    ["unsupported_os"]="Unsupported operating system. Only Linux is supported."
    ["checking_arch"]="Checking system architecture..."
    ["unsupported_arch"]="Unsupported architecture. Only amd64 and arm64 are supported."
    ["checking_deps"]="Checking dependencies..."
    ["installing_deps"]="Installing dependencies..."
    ["deps_ok"]="Dependencies check passed"
    ["downloading"]="Downloading Drip server"
    ["download_failed"]="Download failed"
    ["download_ok"]="Download completed"
    ["installing"]="Installing binary..."
    ["install_ok"]="Binary installed successfully"
    ["config_title"]="Server Configuration"
    ["enter_domain"]="Enter your domain (e.g., tunnel.example.com)"
    ["domain_required"]="Domain is required"
    ["enter_port"]="Enter server port"
    ["enter_token"]="Enter authentication token (leave empty to generate)"
    ["token_generated"]="Token generated"
    ["enter_cert_path"]="Enter TLS certificate path (public key)"
    ["enter_key_path"]="Enter TLS private key path"
    ["cert_not_found"]="Certificate file not found"
    ["key_not_found"]="Private key file not found"
    ["cert_option_title"]="TLS Certificate Configuration"
    ["cert_option_certbot"]="Use Let's Encrypt (recommended)"
    ["cert_option_self"]="Generate self-signed certificate (10 years)"
    ["cert_option_provide"]="Provide your own certificate"
    ["enter_email"]="Enter email for Let's Encrypt (optional, press Enter to skip)"
    ["obtaining_cert"]="Obtaining Let's Encrypt certificate..."
    ["cert_obtained"]="Certificate obtained successfully"
    ["cert_failed"]="Failed to obtain certificate. Please check domain DNS settings."
    ["generating_cert"]="Generating self-signed ECDSA certificate..."
    ["cert_generated"]="Self-signed certificate generated"
    ["cert_warning"]="Note: Self-signed certificates require --insecure flag on client"
    ["cert_exists"]="Certificate already exists, skipping..."
    ["certbot_domain_note"]="Make sure your domain DNS points to this server before proceeding"
    ["creating_user"]="Creating service user..."
    ["user_exists"]="User already exists"
    ["user_created"]="Service user created"
    ["creating_service"]="Creating systemd service..."
    ["service_created"]="Systemd service created"
    ["configuring_firewall"]="Configuring firewall..."
    ["firewall_ok"]="Firewall configured"
    ["saving_config"]="Saving configuration..."
    ["config_saved"]="Configuration saved"
    ["start_now"]="Start service now?"
    ["starting_service"]="Starting service..."
    ["service_started"]="Service started successfully"
    ["service_failed"]="Service failed to start"
    ["install_complete"]="Installation completed!"
    ["client_info"]="Client connection info"
    ["server_addr"]="Server"
    ["token_label"]="Token"
    ["service_commands"]="Service management commands"
    ["cmd_start"]="Start service"
    ["cmd_stop"]="Stop service"
    ["cmd_restart"]="Restart service"
    ["cmd_status"]="Check status"
    ["cmd_logs"]="View logs"
    ["cmd_enable"]="Enable auto-start"
    ["yes"]="y"
    ["no"]="n"
    ["press_enter"]="Press Enter to continue..."
    ["tcp_port_range"]="TCP tunnel port range"
    ["enter_tcp_min"]="Enter minimum TCP port"
    ["enter_tcp_max"]="Enter maximum TCP port"
    ["invalid_port"]="Invalid port number"
    ["enter_public_port"]="Enter public port (for URL display, e.g., behind reverse proxy)"
    ["wildcard_cert_note"]="Note: For subdomain tunnels, you need a wildcard certificate (*.domain.com)"
    ["already_installed"]="Drip server is already installed"
    ["current_version"]="Current version"
    ["update_now"]="Update to the latest version?"
    ["updating"]="Updating..."
    ["update_ok"]="Update completed"
    ["skip_update"]="Skipping update"
    ["service_not_found"]="Systemd service not found. Proceeding with full configuration..."
    ["copying_cert"]="Copying certificate with proper permissions..."
    ["cert_copied"]="Certificate copied successfully"
    ["creating_renewal_hook"]="Creating certificate renewal hook..."
    ["renewal_hook_created"]="Renewal hook created"
)

# Chinese messages
MSG_ZH=(
    ["banner_title"]="Drip 服务器 - 一键安装脚本"
    ["select_lang"]="Select language / 选择语言"
    ["lang_en"]="English"
    ["lang_zh"]="中文"
    ["checking_root"]="检查 root 权限..."
    ["need_root"]="此脚本需要 root 权限，请使用 sudo 运行。"
    ["checking_os"]="检查操作系统..."
    ["unsupported_os"]="不支持的操作系统，仅支持 Linux。"
    ["checking_arch"]="检查系统架构..."
    ["unsupported_arch"]="不支持的架构，仅支持 amd64 和 arm64。"
    ["checking_deps"]="检查依赖..."
    ["installing_deps"]="安装依赖..."
    ["deps_ok"]="依赖检查通过"
    ["downloading"]="下载 Drip 服务器"
    ["download_failed"]="下载失败"
    ["download_ok"]="下载完成"
    ["installing"]="安装二进制文件..."
    ["install_ok"]="二进制文件安装成功"
    ["config_title"]="服务器配置"
    ["enter_domain"]="输入你的域名（例如：tunnel.example.com）"
    ["domain_required"]="域名是必填项"
    ["enter_port"]="输入服务器端口"
    ["enter_token"]="输入认证令牌（留空自动生成）"
    ["token_generated"]="令牌已生成"
    ["enter_cert_path"]="输入 TLS 证书路径（公钥）"
    ["enter_key_path"]="输入 TLS 私钥路径"
    ["cert_not_found"]="证书文件未找到"
    ["key_not_found"]="私钥文件未找到"
    ["cert_option_title"]="TLS 证书配置"
    ["cert_option_certbot"]="使用 Let's Encrypt 自动获取（推荐）"
    ["cert_option_self"]="生成自签名证书（10 年有效期）"
    ["cert_option_provide"]="使用自己的证书"
    ["enter_email"]="输入 Let's Encrypt 邮箱（可选，直接回车跳过）"
    ["obtaining_cert"]="正在获取 Let's Encrypt 证书..."
    ["cert_obtained"]="证书获取成功"
    ["cert_failed"]="证书获取失败，请检查域名 DNS 是否正确指向本服务器"
    ["generating_cert"]="正在生成自签名 ECDSA 证书..."
    ["cert_generated"]="自签名证书已生成"
    ["cert_warning"]="提示：自签名证书需要客户端使用 --insecure 参数"
    ["cert_exists"]="证书已存在，跳过..."
    ["certbot_domain_note"]="请确保域名 DNS 已指向本服务器再继续"
    ["creating_user"]="创建服务用户..."
    ["user_exists"]="用户已存在"
    ["user_created"]="服务用户创建完成"
    ["creating_service"]="创建 systemd 服务..."
    ["service_created"]="systemd 服务创建完成"
    ["configuring_firewall"]="配置防火墙..."
    ["firewall_ok"]="防火墙配置完成"
    ["saving_config"]="保存配置..."
    ["config_saved"]="配置已保存"
    ["start_now"]="是否立即启动服务？"
    ["starting_service"]="正在启动服务..."
    ["service_started"]="服务启动成功"
    ["service_failed"]="服务启动失败"
    ["install_complete"]="安装完成！"
    ["client_info"]="客户端连接信息"
    ["server_addr"]="服务器"
    ["token_label"]="令牌"
    ["service_commands"]="服务管理命令"
    ["cmd_start"]="启动服务"
    ["cmd_stop"]="停止服务"
    ["cmd_restart"]="重启服务"
    ["cmd_status"]="查看状态"
    ["cmd_logs"]="查看日志"
    ["cmd_enable"]="开机启动"
    ["yes"]="y"
    ["no"]="n"
    ["press_enter"]="按 Enter 继续..."
    ["tcp_port_range"]="TCP 隧道端口范围"
    ["enter_tcp_min"]="输入最小 TCP 端口"
    ["enter_tcp_max"]="输入最大 TCP 端口"
    ["invalid_port"]="无效的端口号"
    ["enter_public_port"]="输入公开端口（用于 URL 显示，如在反向代理后）"
    ["wildcard_cert_note"]="注意：要支持子域名隧道，需要通配符证书（*.domain.com）"
    ["already_installed"]="Drip 服务器已安装"
    ["current_version"]="当前版本"
    ["update_now"]="是否更新到最新版本？"
    ["updating"]="正在更新..."
    ["update_ok"]="更新完成"
    ["skip_update"]="跳过更新"
    ["service_not_found"]="Systemd 服务未找到，将进行完整配置..."
    ["copying_cert"]="正在复制证书并设置权限..."
    ["cert_copied"]="证书复制成功"
    ["creating_renewal_hook"]="创建证书续期钩子..."
    ["renewal_hook_created"]="续期钩子创建完成"
)

# Get message by key
msg() {
    local key="$1"
    if [[ "$LANG_CODE" == "zh" ]]; then
        echo "${MSG_ZH[$key]:-$key}"
    else
        echo "${MSG_EN[$key]:-$key}"
    fi
}

# ============================================================================
# Output functions
# ============================================================================
print_info() { echo -e "${BLUE}[INFO]${NC} $1"; }
print_success() { echo -e "${GREEN}[✓]${NC} $1"; }
print_warning() { echo -e "${YELLOW}[!]${NC} $1"; }
print_error() { echo -e "${RED}[✗]${NC} $1"; }
print_step() { echo -e "${CYAN}[→]${NC} $1"; }

repeat_char() {
    local char="$1"
    local count="$2"
    local out=""
    for _ in $(seq 1 "$count"); do
        out+="$char"
    done
    echo "$out"
}

print_panel() {
    local title="$1"
    shift
    local width=58
    local bar
    bar=$(repeat_char "=" "$width")
    echo ""
    echo -e "${CYAN}${bar}${NC}"
    echo -e "${CYAN}${title}${NC}"
    echo -e "${CYAN}${bar}${NC}"
    for line in "$@"; do
        echo -e "  $line"
    done
    echo -e "${CYAN}${bar}${NC}"
    echo ""
}

print_subheader() {
    local title="$1"
    local width=58
    local bar
    bar=$(repeat_char "-" "$width")
    echo ""
    echo -e "${CYAN}${title}${NC}"
    echo -e "${CYAN}${bar}${NC}"
}

ensure_command_access() {
    hash -r 2>/dev/null || true
    command -v rehash >/dev/null 2>&1 && rehash || true

    if command -v drip >/dev/null 2>&1; then
        COMMAND_MADE_AVAILABLE=true
        return
    fi

    local target_path="${INSTALL_DIR}/drip"
    local preferred="/usr/local/bin"

    if [[ ":$PATH:" == *":$preferred:"* ]]; then
        if [[ -w "$preferred" ]]; then
            if ln -sf "$target_path" "$preferred/drip" 2>/dev/null; then
                COMMAND_MADE_AVAILABLE=true
                print_success "Made drip available at $preferred/drip"
            fi
        else
            if sudo ln -sf "$target_path" "$preferred/drip" 2>/dev/null; then
                COMMAND_MADE_AVAILABLE=true
                print_success "Made drip available at $preferred/drip"
            fi
        fi
    fi

    hash -r 2>/dev/null || true
    command -v rehash >/dev/null 2>&1 && rehash || true
}

# Print banner
print_banner() {
    echo -e "${GREEN}"
    cat << "EOF"
    ____       _
   / __ \_____(_)___
  / / / / ___/ / __ \
 / /_/ / /  / / /_/ /
/_____/_/  /_/ .___/
            /_/
EOF
    echo -e "${BOLD}$(msg banner_title)${NC}"
    echo ""
}

# Extract version from a drip binary, preferring the plain output when available
get_version_from_binary() {
    local binary="$1"
    local output=""
    local version=""

    output=$("$binary" version --short 2>/dev/null || true)
    if [[ -n "$output" ]]; then
        version=$(printf '%s\n' "$output" | awk -F': ' '/Version/ {print $2; exit}')
    fi

    if [[ -z "$version" ]]; then
        output=$("$binary" version 2>/dev/null || true)
        if [[ -n "$output" ]]; then
            output=$(printf '%s\n' "$output" | sed -E $'s/\x1b\\[[0-9;]*[A-Za-z]//g')
            version=$(printf '%s\n' "$output" | sed -nE 's/.*Version:[[:space:]]*([vV]?[0-9][^[:space:]]*).*/\1/p' | head -n1)
        fi
    fi

    echo "${version:-unknown}"
}

# ============================================================================
# Language selection
# ============================================================================
select_language() {
    print_panel "$(msg select_lang)" \
        "${GREEN}1)${NC} English" \
        "${GREEN}2)${NC} 中文"

    read -p "Select [1]: " lang_choice < /dev/tty
    case "$lang_choice" in
        2)
            LANG_CODE="zh"
            ;;
        *)
            LANG_CODE="en"
            ;;
    esac
    echo ""
}

# ============================================================================
# System checks
# ============================================================================
check_root() {
    print_step "$(msg checking_root)"
    if [[ $EUID -ne 0 ]]; then
        print_error "$(msg need_root)"
        exit 1
    fi
    print_success "root"
}

check_os() {
    print_step "$(msg checking_os)"
    if [[ "$(uname)" != "Linux" ]]; then
        print_error "$(msg unsupported_os)"
        exit 1
    fi

    # Detect distribution (use subshell to avoid variable pollution)
    if [[ -f /etc/os-release ]]; then
        OS_NAME=$(. /etc/os-release && echo "$ID")
        OS_VERSION_ID=$(. /etc/os-release && echo "$VERSION_ID")
    else
        OS_NAME="unknown"
    fi

    print_success "Linux ($OS_NAME)"
}

check_arch() {
    print_step "$(msg checking_arch)"
    ARCH=$(uname -m)
    case "$ARCH" in
        x86_64)
            ARCH="amd64"
            ;;
        aarch64|arm64)
            ARCH="arm64"
            ;;
        *)
            print_error "$(msg unsupported_arch): $ARCH"
            exit 1
            ;;
    esac
    print_success "$ARCH"
}

check_dependencies() {
    print_step "$(msg checking_deps)"

    local deps_to_install=()

    # Check curl or wget
    if ! command -v curl &> /dev/null && ! command -v wget &> /dev/null; then
        deps_to_install+=("curl")
    fi

    # Install missing dependencies
    if [[ ${#deps_to_install[@]} -gt 0 ]]; then
        print_info "$(msg installing_deps)"

        if command -v apt-get &> /dev/null; then
            apt-get update -qq
            apt-get install -y -qq "${deps_to_install[@]}"
        elif command -v yum &> /dev/null; then
            yum install -y -q "${deps_to_install[@]}"
        elif command -v dnf &> /dev/null; then
            dnf install -y -q "${deps_to_install[@]}"
        elif command -v pacman &> /dev/null; then
            pacman -Sy --noconfirm "${deps_to_install[@]}"
        fi
    fi

    print_success "$(msg deps_ok)"
}

get_latest_version() {
    # Get latest version from GitHub API
    local api_url="https://api.github.com/repos/${GITHUB_REPO}/releases/latest"
    local version=""

    if command -v curl &> /dev/null; then
        version=$(curl -fsSL "$api_url" | grep '"tag_name":' | sed -E 's/.*"tag_name": *"([^"]+)".*/\1/' 2>/dev/null)
    else
        version=$(wget -qO- "$api_url" | grep '"tag_name":' | sed -E 's/.*"tag_name": *"([^"]+)".*/\1/' 2>/dev/null)
    fi

    if [[ -z "$version" ]]; then
        print_error "Failed to get latest version from GitHub"
        exit 1
    fi

    echo "$version"
}


check_existing_install() {
    local server_path="$INSTALL_DIR/drip"

    if [[ -f "$server_path" ]]; then
        local current_version=$(get_version_from_binary "$server_path")

        print_warning "$(msg already_installed): $server_path"
        print_info "$(msg current_version): $current_version"

        # Check remote version
        print_step "Checking for updates..."
        local latest_version=$(get_latest_version)

        if [[ "$current_version" == "$latest_version" ]]; then
            print_success "Already up to date ($current_version)"
            exit 0
        else
            print_info "Latest version: $latest_version"
            echo ""
            read -p "$(msg update_now) [Y/n]: " update_choice < /dev/tty
        fi

        if [[ "$update_choice" =~ ^[Nn]$ ]]; then
            print_info "$(msg skip_update)"
            exit 0
        fi

        IS_UPDATE=true

        # Stop service if running
        if systemctl is-active --quiet drip-server 2>/dev/null; then
            print_info "Stopping drip-server service..."
            systemctl stop drip-server
        fi
    fi
}

# ============================================================================
# Download and install
# ============================================================================
download_binary() {
    # Get latest version if not set
    if [[ -z "$VERSION" ]]; then
        VERSION=$(get_latest_version)
    fi

    if [[ "$IS_UPDATE" == true ]]; then
        print_step "$(msg updating)"
    else
        print_step "$(msg downloading)..."
    fi

    local binary_name="drip-${VERSION}-linux-${ARCH}"
    local download_url="https://github.com/${GITHUB_REPO}/releases/download/${VERSION}/${binary_name}"

    local tmp_file="/tmp/drip"

    if command -v curl &> /dev/null; then
        # Use -# for progress bar instead of -s (silent)
        if ! curl -f#L "$download_url" -o "$tmp_file"; then
            print_error "$(msg download_failed): $download_url"
            exit 1
        fi
    else
        # Use --show-progress to display download progress
        if ! wget --show-progress "$download_url" -O "$tmp_file" 2>&1 | grep -v "^$"; then
            print_error "$(msg download_failed): $download_url"
            exit 1
        fi
    fi

    chmod +x "$tmp_file"
    print_success "$(msg download_ok)"
}

install_binary() {
    print_step "$(msg installing)"

    mkdir -p "$INSTALL_DIR"
    mv /tmp/drip "$INSTALL_DIR/drip"
    chmod +x "$INSTALL_DIR/drip"

    if [[ "$IS_UPDATE" == true ]]; then
        print_success "$(msg update_ok): $INSTALL_DIR/drip"
    else
        print_success "$(msg install_ok): $INSTALL_DIR/drip"
    fi
}

# ============================================================================
# Configuration
# ============================================================================
generate_token() {
    # Generate a random 32-character token
    if command -v openssl &> /dev/null; then
        openssl rand -hex 16
    else
        head -c 16 /dev/urandom | xxd -p
    fi
}

configure_server() {
    print_subheader "$(msg config_title)"

    # Domain
    while true; do
        read -p "$(msg enter_domain): " DOMAIN < /dev/tty
        if [[ -n "$DOMAIN" ]]; then
            break
        fi
        print_error "$(msg domain_required)"
    done

    # Port
    read -p "$(msg enter_port) [$DEFAULT_PORT]: " PORT < /dev/tty
    PORT="${PORT:-$DEFAULT_PORT}"

    # Public port (for URL display, e.g., behind reverse proxy)
    read -p "$(msg enter_public_port) [$PORT]: " PUBLIC_PORT < /dev/tty
    PUBLIC_PORT="${PUBLIC_PORT:-$PORT}"

    # TCP port range
    echo ""
    print_info "$(msg tcp_port_range)"
    read -p "$(msg enter_tcp_min) [$DEFAULT_TCP_PORT_MIN]: " TCP_PORT_MIN < /dev/tty
    TCP_PORT_MIN="${TCP_PORT_MIN:-$DEFAULT_TCP_PORT_MIN}"

    read -p "$(msg enter_tcp_max) [$DEFAULT_TCP_PORT_MAX]: " TCP_PORT_MAX < /dev/tty
    TCP_PORT_MAX="${TCP_PORT_MAX:-$DEFAULT_TCP_PORT_MAX}"

    # Token
    echo ""
    read -p "$(msg enter_token): " TOKEN < /dev/tty
    if [[ -z "$TOKEN" ]]; then
        TOKEN=$(generate_token)
        print_success "$(msg token_generated): $TOKEN"
    fi

    # TLS certificate selection
    print_panel "$(msg cert_option_title)" \
        "${GREEN}1)${NC} $(msg cert_option_certbot)" \
        "${GREEN}2)${NC} $(msg cert_option_self)" \
        "${GREEN}3)${NC} $(msg cert_option_provide)"

    read -p "Select [1]: " cert_choice < /dev/tty

    case "$cert_choice" in
        2)
            # Self-signed certificate
            CERT_MODE="self_signed"
            CERT_PATH="${CONFIG_DIR}/server.crt"
            KEY_PATH="${CONFIG_DIR}/server.key"
            echo ""
            print_warning "$(msg cert_warning)"
            ;;
        3)
            # User provided certificate
            CERT_MODE="provided"
            echo ""
            while true; do
                read -p "$(msg enter_cert_path): " CERT_PATH < /dev/tty
                if [[ -f "$CERT_PATH" ]]; then
                    break
                fi
                print_error "$(msg cert_not_found): $CERT_PATH"
            done

            while true; do
                read -p "$(msg enter_key_path): " KEY_PATH < /dev/tty
                if [[ -f "$KEY_PATH" ]]; then
                    break
                fi
                print_error "$(msg key_not_found): $KEY_PATH"
            done
            print_success "Certificate files verified"
            ;;
        *)
            # Let's Encrypt (default)
            CERT_MODE="certbot"
            CERT_PATH="/etc/letsencrypt/live/${DOMAIN}/fullchain.pem"
            KEY_PATH="/etc/letsencrypt/live/${DOMAIN}/privkey.pem"
            echo ""
            print_warning "$(msg certbot_domain_note)"
            echo ""
            read -p "$(msg enter_email): " CERTBOT_EMAIL < /dev/tty
            ;;
    esac
}

# ============================================================================
# Certificate
# ============================================================================
setup_certificate() {
    case "$CERT_MODE" in
        "certbot")
            obtain_letsencrypt_cert
            ;;
        "self_signed")
            generate_self_signed_cert
            ;;
        "provided")
            # Certificate already verified in configure_server
            print_success "Using provided certificate"
            ;;
    esac
}

obtain_letsencrypt_cert() {
    # Let's Encrypt original cert paths
    local le_cert_path="/etc/letsencrypt/live/${DOMAIN}/fullchain.pem"
    local le_key_path="/etc/letsencrypt/live/${DOMAIN}/privkey.pem"

    # We'll copy certs to CONFIG_DIR for proper permissions
    # Update global CERT_PATH and KEY_PATH
    CERT_PATH="${CONFIG_DIR}/server.crt"
    KEY_PATH="${CONFIG_DIR}/server.key"

    # Check if certificate already exists (either in Let's Encrypt or our copy)
    if [[ -f "$CERT_PATH" ]] && [[ -f "$KEY_PATH" ]]; then
        print_warning "$(msg cert_exists)"
        return
    fi

    # Check if Let's Encrypt cert exists but we just need to copy it
    if [[ -f "$le_cert_path" ]]; then
        print_info "Let's Encrypt certificate found, copying with proper permissions..."
        copy_letsencrypt_cert "$le_cert_path" "$le_key_path"
        return
    fi

    print_step "$(msg obtaining_cert)"

    # Install certbot if not present
    if ! command -v certbot &> /dev/null; then
        print_info "Installing certbot..."
        if command -v apt-get &> /dev/null; then
            apt-get update -qq
            apt-get install -y -qq certbot
        elif command -v yum &> /dev/null; then
            yum install -y -q certbot
        elif command -v dnf &> /dev/null; then
            dnf install -y -q certbot
        elif command -v pacman &> /dev/null; then
            pacman -Sy --noconfirm certbot
        fi
    fi

    # Stop services that might use port 80
    local nginx_was_running=false
    local apache_was_running=false

    if systemctl is-active --quiet nginx 2>/dev/null; then
        systemctl stop nginx
        nginx_was_running=true
    fi

    if systemctl is-active --quiet apache2 2>/dev/null; then
        systemctl stop apache2
        apache_was_running=true
    fi

    if systemctl is-active --quiet httpd 2>/dev/null; then
        systemctl stop httpd
        apache_was_running=true
    fi

    # Build certbot command
    local certbot_args="certonly --standalone -d ${DOMAIN}"
    if [[ -n "$CERTBOT_EMAIL" ]]; then
        certbot_args="$certbot_args --email ${CERTBOT_EMAIL}"
    else
        certbot_args="$certbot_args --register-unsafely-without-email"
    fi
    certbot_args="$certbot_args --agree-tos --non-interactive"

    # Obtain certificate
    if certbot $certbot_args; then
        print_success "$(msg cert_obtained)"
    else
        print_error "$(msg cert_failed)"
        # Restart services before exit
        [[ "$nginx_was_running" == true ]] && systemctl start nginx
        [[ "$apache_was_running" == true ]] && (systemctl start apache2 2>/dev/null || systemctl start httpd 2>/dev/null)
        exit 1
    fi

    # Restart services
    [[ "$nginx_was_running" == true ]] && systemctl start nginx
    [[ "$apache_was_running" == true ]] && (systemctl start apache2 2>/dev/null || systemctl start httpd 2>/dev/null)

    # Copy certificates with proper permissions
    copy_letsencrypt_cert "$le_cert_path" "$le_key_path"

    # Create renewal hook for automatic certificate updates
    create_certbot_renewal_hook
}

copy_letsencrypt_cert() {
    local le_cert="$1"
    local le_key="$2"

    print_step "$(msg copying_cert)"

    # Ensure config directory exists
    mkdir -p "$CONFIG_DIR"

    # Copy certificates
    cp -L "$le_cert" "$CERT_PATH"
    cp -L "$le_key" "$KEY_PATH"

    # Set proper permissions (readable by drip user)
    chmod 644 "$CERT_PATH"
    chmod 640 "$KEY_PATH"
    chown root:"$SERVICE_USER" "$KEY_PATH"

    print_success "$(msg cert_copied)"
    print_info "Certificate: $CERT_PATH"
    print_info "Private Key: $KEY_PATH"
}

create_certbot_renewal_hook() {
    print_step "$(msg creating_renewal_hook)"

    local hook_dir="/etc/letsencrypt/renewal-hooks/deploy"
    mkdir -p "$hook_dir"

    cat > "${hook_dir}/drip-server.sh" << 'HOOK_EOF'
#!/bin/bash
# Drip Server certificate renewal hook
# This script copies renewed certificates and restarts the service

DOMAIN_DIR="/etc/letsencrypt/live"
CONFIG_DIR="/etc/drip"
SERVICE_USER="drip"

# Find the domain from the renewed certificate
for domain_path in "$DOMAIN_DIR"/*; do
    if [[ -d "$domain_path" ]]; then
        domain=$(basename "$domain_path")
        le_cert="${domain_path}/fullchain.pem"
        le_key="${domain_path}/privkey.pem"

        if [[ -f "$le_cert" ]] && [[ -f "$le_key" ]]; then
            # Copy certificates
            cp -L "$le_cert" "${CONFIG_DIR}/server.crt"
            cp -L "$le_key" "${CONFIG_DIR}/server.key"

            # Set proper permissions
            chmod 644 "${CONFIG_DIR}/server.crt"
            chmod 640 "${CONFIG_DIR}/server.key"
            chown root:"$SERVICE_USER" "${CONFIG_DIR}/server.key"

            # Restart service
            systemctl restart drip-server 2>/dev/null || true
            break
        fi
    fi
done
HOOK_EOF

    chmod +x "${hook_dir}/drip-server.sh"
    print_success "$(msg renewal_hook_created)"
}

generate_self_signed_cert() {
    # Check if certificate already exists
    if [[ -f "$CERT_PATH" ]] && [[ -f "$KEY_PATH" ]]; then
        print_warning "$(msg cert_exists)"
        return
    fi

    print_step "$(msg generating_cert)"

    # Ensure config directory exists
    mkdir -p "$CONFIG_DIR"

    # Generate 10-year self-signed ECDSA certificate
    # Country: US (United States)
    openssl ecparam -genkey -name prime256v1 -out "$KEY_PATH" 2>/dev/null

    openssl req -new -x509 \
        -key "$KEY_PATH" \
        -out "$CERT_PATH" \
        -days 3650 \
        -subj "/C=US/ST=California/L=San Francisco/O=Drip Tunnel/OU=Server/CN=${DOMAIN}" \
        -addext "subjectAltName=DNS:${DOMAIN},DNS:*.${DOMAIN}" \
        2>/dev/null

    # Set proper permissions
    chmod 600 "$KEY_PATH"
    chmod 644 "$CERT_PATH"

    print_success "$(msg cert_generated)"
    print_info "Certificate: $CERT_PATH"
    print_info "Private Key: $KEY_PATH"
}

# ============================================================================
# System setup
# ============================================================================
create_service_user() {
    print_step "$(msg creating_user)"

    if id "$SERVICE_USER" &>/dev/null; then
        print_warning "$(msg user_exists): $SERVICE_USER"
    else
        useradd -r -s /bin/false "$SERVICE_USER"
        print_success "$(msg user_created): $SERVICE_USER"
    fi
}

create_directories() {
    mkdir -p "$CONFIG_DIR"
    mkdir -p "$WORK_DIR"
    chown "$SERVICE_USER:$SERVICE_USER" "$WORK_DIR"
}

create_systemd_service() {
    print_step "$(msg creating_service)"

    cat > /etc/systemd/system/drip-server.service << EOF
[Unit]
Description=Drip Tunnel Server
After=network.target
Documentation=https://github.com/Gouryella/drip

[Service]
Type=simple
User=${SERVICE_USER}
Group=${SERVICE_USER}
Restart=on-failure
RestartSec=5s

# Resource limits
LimitNOFILE=65536
LimitNPROC=4096

# Working directory
WorkingDirectory=${WORK_DIR}

# Load environment variables from file
EnvironmentFile=${CONFIG_DIR}/server.env

# Start command (uses environment variables)
ExecStart=${INSTALL_DIR}/drip server

# Logging
StandardOutput=journal
StandardError=journal
SyslogIdentifier=drip-server

[Install]
WantedBy=multi-user.target
EOF

    systemctl daemon-reload
    print_success "$(msg service_created)"
}

configure_firewall() {
    print_step "$(msg configuring_firewall)"

    local ports_to_open=("$PORT" "$TCP_PORT_MIN:$TCP_PORT_MAX")

    # UFW
    if command -v ufw &> /dev/null; then
        ufw allow "$PORT/tcp" >/dev/null 2>&1 || true
        ufw allow "${TCP_PORT_MIN}:${TCP_PORT_MAX}/tcp" >/dev/null 2>&1 || true
    fi

    # firewalld
    if command -v firewall-cmd &> /dev/null; then
        firewall-cmd --permanent --add-port="$PORT/tcp" >/dev/null 2>&1 || true
        firewall-cmd --permanent --add-port="${TCP_PORT_MIN}-${TCP_PORT_MAX}/tcp" >/dev/null 2>&1 || true
        firewall-cmd --reload >/dev/null 2>&1 || true
    fi

    print_success "$(msg firewall_ok)"
}

save_config() {
    print_step "$(msg saving_config)"

    cat > "$CONFIG_DIR/server.env" << EOF
# Drip Server Configuration
# Generated: $(date)
# DO NOT SHARE THIS FILE - Contains sensitive information

# Server settings
DRIP_PORT=${PORT}
DRIP_PUBLIC_PORT=${PUBLIC_PORT}
DRIP_DOMAIN=${DOMAIN}
DRIP_TOKEN=${TOKEN}

# TLS certificate paths
DRIP_TLS_CERT=${CERT_PATH}
DRIP_TLS_KEY=${KEY_PATH}

# TCP tunnel port range
DRIP_TCP_PORT_MIN=${TCP_PORT_MIN}
DRIP_TCP_PORT_MAX=${TCP_PORT_MAX}
EOF

    chmod 640 "$CONFIG_DIR/server.env"
    chown root:"$SERVICE_USER" "$CONFIG_DIR/server.env"
    print_success "$(msg config_saved): $CONFIG_DIR/server.env"
}

# ============================================================================
# Service management
# ============================================================================
start_service() {
    read -p "$(msg start_now) [Y/n]: " start_choice < /dev/tty

    if [[ "$start_choice" =~ ^[Nn]$ ]]; then
        return
    fi

    print_step "$(msg starting_service)"

    systemctl enable drip-server >/dev/null 2>&1
    systemctl start drip-server

    sleep 2

    if systemctl is-active --quiet drip-server; then
        print_success "$(msg service_started)"
    else
        print_error "$(msg service_failed)"
        echo ""
        journalctl -u drip-server -n 20 --no-pager
        exit 1
    fi
}

# ============================================================================
# Final output
# ============================================================================
show_completion() {
    print_panel "$(msg install_complete)"

    echo -e "${CYAN}$(msg client_info):${NC}"
    echo -e "  ${BOLD}$(msg server_addr):${NC} ${DOMAIN}:${PORT}"
    echo -e "  ${BOLD}$(msg token_label):${NC}  ${TOKEN}"
    echo ""

    echo -e "${CYAN}$(msg service_commands):${NC}"
    echo -e "  ${GREEN}$(msg cmd_start):${NC}    systemctl start drip-server"
    echo -e "  ${GREEN}$(msg cmd_stop):${NC}     systemctl stop drip-server"
    echo -e "  ${GREEN}$(msg cmd_restart):${NC}  systemctl restart drip-server"
    echo -e "  ${GREEN}$(msg cmd_status):${NC}   systemctl status drip-server"
    echo -e "  ${GREEN}$(msg cmd_logs):${NC}     journalctl -u drip-server -f"
    echo -e "  ${GREEN}$(msg cmd_enable):${NC}   systemctl enable drip-server"
    echo ""
}

# ============================================================================
# Main
# ============================================================================
main() {
    clear
    print_banner
    select_language

    echo -e "${BOLD}────────────────────────────────────────────${NC}"

    check_root
    check_os
    check_arch
    check_dependencies
    check_existing_install

    echo ""
download_binary
install_binary
ensure_command_access

# If updating, check if systemd service exists
if [[ "$IS_UPDATE" == true ]]; then
        # Check if systemd service file exists
        if [[ -f /etc/systemd/system/drip-server.service ]]; then
            echo ""
            print_step "Restarting drip-server service..."
            systemctl start drip-server

            if systemctl is-active --quiet drip-server; then
                print_success "$(msg service_started)"
            else
                print_error "$(msg service_failed)"
                journalctl -u drip-server -n 20 --no-pager
                exit 1
            fi

            echo ""
            local new_version=$(get_version_from_binary "$INSTALL_DIR/drip")
            print_panel "$(msg update_ok)"
            print_info "$(msg current_version): $new_version"
            echo ""
            exit 0
        else
            # Service file doesn't exist, need to configure
            print_warning "$(msg service_not_found)"
            IS_UPDATE=false
        fi
    fi

    echo ""
    configure_server

    echo ""
    setup_certificate
    create_service_user
    create_directories
    create_systemd_service
    configure_firewall
    save_config

    echo ""
    start_service

    show_completion
}

# Run
main "$@"
