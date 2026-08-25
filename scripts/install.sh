#!/bin/bash
set -euo pipefail

# Unified installer wrapper for Drip client and server
# Chooses language first, then lets the user pick client or server.

GITHUB_REPO="${GITHUB_REPO:-nickolasdeluca/drip-ex}"
RAW_BASE="${RAW_BASE:-https://raw.githubusercontent.com/${GITHUB_REPO}/main/scripts}"

LANG_CODE="${LANG_CODE:-}"
TARGET=""
TARGET_ARGS=()

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
        help_title) echo "Usage";;
        help_line1) echo "  install.sh [--lang en|zh] [--client|--server] [args...]";;
        help_line2) echo "Arguments after the target are passed to the installer (e.g. --uninstall for client).";;
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
        help_title) echo "用法";;
        help_line1) echo "  install.sh [--lang en|zh] [--client|--server] [args...]";;
        help_line2) echo "目标之后的参数会透传给对应安装脚本（例如客户端的 --uninstall）。";;
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
download_and_run() {
    local url="$1"
    shift

    echo -e "${CYAN}$(msg downloading_installer)${NC} $url"

    local tmp_file
    tmp_file=$(mktemp "/tmp/drip-install-XXXX")

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

    chmod +x "$tmp_file"
    LANG_CODE="$LANG_CODE" SKIP_LANG_PROMPT=true "$tmp_file" "$@"
    rm -f "$tmp_file"
}

run_client() {
    local local_script=""
    if [[ -n "$SCRIPT_DIR" && -f "${SCRIPT_DIR}/install-client.sh" ]]; then
        local_script="${SCRIPT_DIR}/install-client.sh"
    fi

    if [[ -n "$local_script" ]]; then
        LANG_CODE="$LANG_CODE" SKIP_LANG_PROMPT=true "$local_script" "${TARGET_ARGS[@]}"
    else
        download_and_run "${RAW_BASE}/install-client.sh" "${TARGET_ARGS[@]}"
    fi
}

run_server() {
    local local_script=""
    if [[ -n "$SCRIPT_DIR" && -f "${SCRIPT_DIR}/install-server.sh" ]]; then
        local_script="${SCRIPT_DIR}/install-server.sh"
    fi

    if [[ -n "$local_script" ]]; then
        LANG_CODE="$LANG_CODE" SKIP_LANG_PROMPT=true "$local_script" "${TARGET_ARGS[@]}"
    else
        download_and_run "${RAW_BASE}/install-server.sh" "${TARGET_ARGS[@]}"
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
