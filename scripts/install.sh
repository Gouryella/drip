#!/bin/bash
set -euo pipefail

# Unified installer wrapper for Drip client and server
# Chooses language first, then lets the user pick client or server.

GITHUB_REPO="Gouryella/drip"
RAW_BASE="${RAW_BASE:-}"
VERSION="${VERSION:-}"
ALLOW_LATEST="${ALLOW_LATEST:-false}"
INSTALLER_SHA256="${INSTALLER_SHA256:-}"
CLIENT_INSTALLER_SHA256="${CLIENT_INSTALLER_SHA256:-}"
SERVER_INSTALLER_SHA256="${SERVER_INSTALLER_SHA256:-}"

LANG_CODE="${LANG_CODE:-}"
TARGET=""
TARGET_ARGS=()
TEMP_DIRS=()

# Colors
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
BOLD='\033[1m'
NC='\033[0m'

SCRIPT_DIR=""
if SCRIPT_DIR_TMP=$(cd "$(dirname "${BASH_SOURCE[0]:-$0}")" && pwd 2>/dev/null); then
    SCRIPT_DIR="$SCRIPT_DIR_TMP"
fi

# ============================================================================
# Internationalization
# ============================================================================
msg_en() {
    case "$1" in
        banner_title) echo "Drip Installer";;
        select_lang) echo "Select language / 选择语言";;
        lang_en) echo "English";;
        lang_zh) echo "中文";;
        select_target) echo "Select install target";;
        target_client) echo "Client";;
        target_server) echo "Server";;
        invalid_choice) echo "Invalid choice, using default";;
        downloading_installer) echo "Downloading installer script...";;
        download_failed) echo "Failed to download installer script";;
        checksum_required) echo "Installer checksum is required for remote downloads";;
        checksum_failed) echo "Installer checksum verification failed";;
        version_required) echo "Refusing to download from an unpinned branch. Set VERSION=vX.Y.Z or pass --version vX.Y.Z.";;
        help_title) echo "Usage";;
        help_line1) echo "  install.sh --version vX.Y.Z [--lang en|zh] [--client|--server] [args...]";;
        help_line2) echo "Remote installer downloads require INSTALLER_SHA256, CLIENT_INSTALLER_SHA256, SERVER_INSTALLER_SHA256, or --installer-checksum.";;
        help_line3) echo "Arguments after the target are passed to the installer (e.g. --checksum for release archive verification).";;
        *) echo "$1";;
    esac
}

msg_zh() {
    case "$1" in
        banner_title) echo "Drip 安装器";;
        select_lang) echo "Select language / 选择语言";;
        lang_en) echo "English";;
        lang_zh) echo "中文";;
        select_target) echo "选择安装目标";;
        target_client) echo "客户端";;
        target_server) echo "服务器";;
        invalid_choice) echo "输入无效，使用默认选项";;
        downloading_installer) echo "正在下载安装脚本...";;
        download_failed) echo "下载安装脚本失败";;
        checksum_required) echo "远程下载安装脚本需要提供 SHA256 校验值";;
        checksum_failed) echo "安装脚本 SHA256 校验失败";;
        version_required) echo "拒绝从未固定的分支下载安装脚本。请设置 VERSION=vX.Y.Z 或传入 --version vX.Y.Z。";;
        help_title) echo "用法";;
        help_line1) echo "  install.sh --version vX.Y.Z [--lang en|zh] [--client|--server] [args...]";;
        help_line2) echo "远程下载安装脚本需要 INSTALLER_SHA256、CLIENT_INSTALLER_SHA256、SERVER_INSTALLER_SHA256 或 --installer-checksum。";;
        help_line3) echo "目标之后的参数会透传给对应安装脚本（例如用于 release 归档校验的 --checksum）。";;
        *) echo "$1";;
    esac
}

msg() {
    if [[ "$LANG_CODE" == "zh" ]]; then
        msg_zh "$1"
    else
        msg_en "$1"
    fi
}

# ============================================================================
# Helpers
# ============================================================================
prompt_input() {
    local __prompt="$1"
    local __var_name="$2"
    printf "%s" "$__prompt"
    IFS= read -r "$__var_name" < /dev/tty
}

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

usage() {
    echo -e "${BOLD}$(msg help_title):${NC}"
    echo "$(msg help_line1)"
    echo "$(msg help_line2)"
    echo "$(msg help_line3)"
}

# ============================================================================
# Selection
# ============================================================================
select_language() {
    echo -e "${CYAN}$(msg select_lang)${NC}"
    echo -e "  ${GREEN}1)${NC} $(msg lang_en)"
    echo -e "  ${GREEN}2)${NC} $(msg lang_zh)"

    prompt_input "Select [1]: " lang_choice
    case "$lang_choice" in
        2) LANG_CODE="zh" ;;
        1|"") LANG_CODE="en" ;;
        *) LANG_CODE="en";;
    esac
    echo ""
}

select_target() {
    echo -e "${CYAN}$(msg select_target)${NC}"
    echo -e "  ${GREEN}1)${NC} $(msg target_client)"
    echo -e "  ${GREEN}2)${NC} $(msg target_server)"

    prompt_input "Select [1]: " target_choice
    case "$target_choice" in
        2) TARGET="server" ;;
        1|"") TARGET="client" ;;
        *) echo -e "${YELLOW}$(msg invalid_choice)${NC}"; TARGET="client" ;;
    esac
    echo ""
}

# ============================================================================
# Runner helpers
# ============================================================================
cleanup_temp_dirs() {
    if [[ ${#TEMP_DIRS[@]} -eq 0 ]]; then
        return
    fi

    local dir
    for dir in "${TEMP_DIRS[@]}"; do
        if [[ -n "$dir" && -d "$dir" ]]; then
            rm -rf "$dir"
        fi
    done
}

trap cleanup_temp_dirs EXIT HUP INT TERM

is_truthy() {
    case "$(printf '%s' "$1" | tr '[:upper:]' '[:lower:]')" in
        1|true|yes|y) return 0 ;;
        *) return 1 ;;
    esac
}

ensure_pinned_version() {
    if [[ -n "$RAW_BASE" ]]; then
        return
    fi

    if [[ -n "$VERSION" ]]; then
        RAW_BASE="https://raw.githubusercontent.com/${GITHUB_REPO}/${VERSION}/scripts"
        return
    fi

    if is_truthy "$ALLOW_LATEST"; then
        RAW_BASE="https://raw.githubusercontent.com/${GITHUB_REPO}/main/scripts"
        echo -e "${YELLOW}Using main because --allow-latest was set; checksum verification is still required.${NC}"
        return
    fi

    echo -e "${YELLOW}$(msg version_required)${NC}"
    exit 1
}

sha256_file() {
    local file="$1"
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$file" | awk '{print $1}'
    elif command -v shasum >/dev/null 2>&1; then
        shasum -a 256 "$file" | awk '{print $1}'
    elif command -v openssl >/dev/null 2>&1; then
        openssl dgst -sha256 "$file" | awk '{print $NF}'
    else
        echo "sha256sum, shasum, or openssl is required" >&2
        exit 1
    fi
}

verify_sha256() {
    local file="$1"
    local expected="$2"
    local actual

    expected=$(printf '%s' "$expected" | tr '[:upper:]' '[:lower:]' | tr -d '[:space:]')
    if [[ ! "$expected" =~ ^[0-9a-f]{64}$ ]]; then
        echo -e "${YELLOW}Invalid SHA256 value${NC}"
        exit 1
    fi

    actual=$(sha256_file "$file")
    actual=$(printf '%s' "$actual" | tr '[:upper:]' '[:lower:]' | tr -d '[:space:]')
    if [[ "$actual" != "$expected" ]]; then
        echo -e "${YELLOW}$(msg checksum_failed)${NC}"
        echo "Expected: $expected"
        echo "Actual:   $actual"
        exit 1
    fi
}

make_temp_dir() {
    local tmp_root="${TMPDIR:-/tmp}"
    local tmp_dir
    tmp_dir=$(mktemp -d "${tmp_root%/}/drip-install.XXXXXX")
    chmod 700 "$tmp_dir"
    TEMP_DIRS+=("$tmp_dir")
    echo "$tmp_dir"
}

download_and_run() {
    local url="$1"
    local expected_sha256="$2"
    shift
    shift

    if [[ -z "$expected_sha256" ]]; then
        echo -e "${YELLOW}$(msg checksum_required)${NC}"
        exit 1
    fi

    echo -e "${CYAN}$(msg downloading_installer)${NC} $url"

    local tmp_dir
    local tmp_file
    tmp_dir=$(make_temp_dir)
    tmp_file="${tmp_dir}/installer.sh"

    if command -v curl >/dev/null 2>&1; then
        if ! curl -fsSL "$url" -o "$tmp_file"; then
            echo -e "${YELLOW}$(msg download_failed): $url${NC}"
            exit 1
        fi
    elif command -v wget >/dev/null 2>&1; then
        if ! wget -qO "$tmp_file" "$url"; then
            echo -e "${YELLOW}$(msg download_failed): $url${NC}"
            exit 1
        fi
    else
        echo "curl or wget is required"
        exit 1
    fi

    verify_sha256 "$tmp_file" "$expected_sha256"
    chmod +x "$tmp_file"
    LANG_CODE="$LANG_CODE" VERSION="$VERSION" ALLOW_LATEST="$ALLOW_LATEST" SKIP_LANG_PROMPT=true "$tmp_file" "$@"
}

run_client() {
    local local_script=""
    if [[ -n "$SCRIPT_DIR" && -f "${SCRIPT_DIR}/install-client.sh" ]]; then
        local_script="${SCRIPT_DIR}/install-client.sh"
    fi

    if [[ -n "$local_script" ]]; then
        LANG_CODE="$LANG_CODE" VERSION="$VERSION" ALLOW_LATEST="$ALLOW_LATEST" SKIP_LANG_PROMPT=true "$local_script" "${TARGET_ARGS[@]}"
    else
        ensure_pinned_version
        local checksum="${CLIENT_INSTALLER_SHA256:-$INSTALLER_SHA256}"
        download_and_run "${RAW_BASE}/install-client.sh" "$checksum" "${TARGET_ARGS[@]}"
    fi
}

run_server() {
    local local_script=""
    if [[ -n "$SCRIPT_DIR" && -f "${SCRIPT_DIR}/install-server.sh" ]]; then
        local_script="${SCRIPT_DIR}/install-server.sh"
    fi

    if [[ -n "$local_script" ]]; then
        LANG_CODE="$LANG_CODE" VERSION="$VERSION" ALLOW_LATEST="$ALLOW_LATEST" SKIP_LANG_PROMPT=true "$local_script" "${TARGET_ARGS[@]}"
    else
        ensure_pinned_version
        local checksum="${SERVER_INSTALLER_SHA256:-$INSTALLER_SHA256}"
        download_and_run "${RAW_BASE}/install-server.sh" "$checksum" "${TARGET_ARGS[@]}"
    fi
}

# ============================================================================
# Main
# ============================================================================
parse_args() {
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --lang)
                if [[ $# -ge 2 ]]; then
                    LANG_CODE="$2"
                    shift 2
                else
                    shift
                fi
                ;;
            --version)
                if [[ $# -ge 2 ]]; then
                    VERSION="$2"
                    shift 2
                else
                    shift
                fi
                ;;
            --installer-checksum|--installer-sha256)
                if [[ $# -ge 2 ]]; then
                    INSTALLER_SHA256="$2"
                    shift 2
                else
                    shift
                fi
                ;;
            --client-installer-checksum|--client-installer-sha256)
                if [[ $# -ge 2 ]]; then
                    CLIENT_INSTALLER_SHA256="$2"
                    shift 2
                else
                    shift
                fi
                ;;
            --server-installer-checksum|--server-installer-sha256)
                if [[ $# -ge 2 ]]; then
                    SERVER_INSTALLER_SHA256="$2"
                    shift 2
                else
                    shift
                fi
                ;;
            --allow-latest)
                ALLOW_LATEST=true
                shift
                ;;
            --client|client|-c)
                TARGET="client"
                shift
                ;;
            --server|server|-s)
                TARGET="server"
                shift
                ;;
            --help|-h)
                usage
                exit 0
                ;;
            *)
                TARGET_ARGS+=("$1")
                shift
                ;;
        esac
    done
}

main() {
    parse_args "$@"

    clear
    print_banner

    [[ -z "$LANG_CODE" ]] && select_language
    [[ -z "$TARGET" ]] && select_target

    # Default to English if someone skips selection without setting LANG_CODE
    LANG_CODE="${LANG_CODE:-en}"

    if [[ "$TARGET" == "server" ]]; then
        run_server
    else
        run_client
    fi
}

main "$@"
