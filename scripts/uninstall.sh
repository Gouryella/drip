#!/bin/bash
set -euo pipefail

# Drip unified uninstaller (client + server)

# -----------------------------------------------------------------------------
# Colors
# -----------------------------------------------------------------------------
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
CYAN='\033[0;36m'
NC='\033[0m'

# -----------------------------------------------------------------------------
# Runtime options
# -----------------------------------------------------------------------------
LANG_CODE="${LANG_CODE:-zh}"   # zh | en (UI language)
AUTO_YES=false                 # true => auto-confirm prompts
TARGET="prompt"                # client | server | both | prompt

# Prefer prompting/reading from real TTY to avoid "stuck" when piped (curl | bash)
TTY="/dev/tty"

has_tty() { [[ -e "$TTY" ]] && [[ -r "$TTY" ]] && [[ -w "$TTY" ]]; }

say() { echo -e "$*"; }
say_info() { say "${BLUE}[INFO]${NC} $*"; }
say_ok() { say "${GREEN}[✓]${NC} $*"; }
say_warn() { say "${YELLOW}[!]${NC} $*"; }
say_err() { say "${RED}[✗]${NC} $*"; }

# -----------------------------------------------------------------------------
# TTY helpers
# -----------------------------------------------------------------------------
# Write raw text to TTY if available (so prompts are always visible and never
# interpreted as printf format strings).
tty_put() {
  if has_tty; then
    printf "%b" "$1" > "$TTY"
  else
    printf "%b" "$1"
  fi
}

# Read user input from TTY first, fallback to stdin
tty_read() {
  local __var="$1"
  local __tmp=""
  if has_tty; then
    IFS= read -r __tmp < "$TTY" || true
  else
    IFS= read -r __tmp || true
  fi
  printf -v "$__var" "%s" "$__tmp"
}

# -----------------------------------------------------------------------------
# i18n messages (UI strings only)
# -----------------------------------------------------------------------------
msg() {
  local key="$1"
  if [[ "$LANG_CODE" == "zh" ]]; then
    case "$key" in
      title) echo "Drip 卸载脚本" ;;
      select_target) echo "选择卸载对象" ;;
      opt_client) echo "客户端" ;;
      opt_server) echo "服务器端" ;;
      opt_both) echo "客户端 + 服务器端" ;;
      confirm) echo "确认卸载？" ;;
      removing_client) echo "卸载客户端..." ;;
      removing_server) echo "卸载服务器端..." ;;
      done) echo "卸载完成" ;;
      remove_config) echo "删除配置目录？" ;;
      remove_data) echo "删除数据/日志目录？" ;;
      no_binary) echo "未发现可执行文件，跳过二进制删除" ;;
      stop_service) echo "停止并禁用 systemd 服务..." ;;
      remove_service) echo "删除 systemd 服务文件..." ;;
      clean_path) echo "清理 PATH 配置..." ;;
      skip) echo "跳过" ;;
      select_lang) echo "选择语言" ;;
      lang_en) echo "English" ;;
      lang_zh) echo "中文" ;;
      target_label) echo "目标" ;;
      need_root) echo "卸载服务器需要 root 权限（请用 sudo 运行）" ;;
      still_delete_shared) echo "检测到与服务器共用同一二进制，仍然删除？" ;;
      *) echo "$key" ;;
    esac
  else
    case "$key" in
      title) echo "Drip Uninstaller" ;;
      select_target) echo "Select what to uninstall" ;;
      opt_client) echo "Client" ;;
      opt_server) echo "Server" ;;
      opt_both) echo "Client + Server" ;;
      confirm) echo "Proceed with uninstall?" ;;
      removing_client) echo "Removing client..." ;;
      removing_server) echo "Removing server..." ;;
      done) echo "Uninstall completed" ;;
      remove_config) echo "Remove config directory as well?" ;;
      remove_data) echo "Remove data/log directory as well?" ;;
      no_binary) echo "No binary found, skipping binary removal" ;;
      stop_service) echo "Stopping and disabling systemd service..." ;;
      remove_service) echo "Removing systemd service file..." ;;
      clean_path) echo "Cleaning PATH entries..." ;;
      skip) echo "Skip" ;;
      select_lang) echo "Select language" ;;
      lang_en) echo "English" ;;
      lang_zh) echo "中文" ;;
      target_label) echo "Target" ;;
      need_root) echo "Server uninstall requires root (run with sudo)" ;;
      still_delete_shared) echo "Server uses the same binary. Delete anyway?" ;;
      *) echo "$key" ;;
    esac
  fi
}

repeat_char() { printf "%*s" "$2" "" | tr ' ' "$1"; }

print_panel() {
  local title="$1"; shift || true
  local width=58
  local bar; bar="$(repeat_char "=" "$width")"
  say ""
  say "${CYAN}${bar}${NC}"
  say "${CYAN}${title}${NC}"
  say "${CYAN}${bar}${NC}"
  for line in "$@"; do say "  $line"; done
  say "${CYAN}${bar}${NC}"
  say ""
}

print_subheader() {
  local title="$1"
  local width=58
  local bar; bar="$(repeat_char "-" "$width")"
  say ""
  say "${CYAN}${title}${NC}"
  say "${CYAN}${bar}${NC}"
}

print_menu() {
  local title="$1"; shift
  say ""
  say "${CYAN}------------------------------${NC}"
  say "${CYAN}${title}${NC}"
  say "${CYAN}------------------------------${NC}"
  for line in "$@"; do say "  $line"; done
  say ""
}

# Ask an interactive y/n question (always visible even when piped)
prompt_yes() {
  local prompt="$1"
  local default_no="${2:-false}" # true => default No

  if [[ "$AUTO_YES" == true ]]; then
    echo "y"; return
  fi

  local suffix="[Y/n]"
  local default_ans="y"
  if [[ "$default_no" == "true" ]]; then
    suffix="[y/N]"
    default_ans="n"
  fi

  while true; do
    tty_put "${prompt} ${suffix} "
    local ans; tty_read ans
    ans="$(echo "${ans}" | tr '[:upper:]' '[:lower:]' | tr -d ' \t')"

    if [[ -z "$ans" ]]; then
      echo "$default_ans"; return
    fi
    case "$ans" in
      y|yes) echo "y"; return ;;
      n|no)  echo "n"; return ;;
      *) say_warn "Please enter y or n." ;;
    esac
  done
}

# -----------------------------------------------------------------------------
# Privilege helpers
# -----------------------------------------------------------------------------
need_root() { [[ "${EUID:-0}" -eq 0 ]]; }

run_root() {
  if need_root; then
    "$@"
  else
    command -v sudo >/dev/null 2>&1 || return 1
    sudo "$@"
  fi
}

# Remove a file/dir, using sudo when needed
remove_path() {
  local path="$1"
  [[ -z "$path" ]] && return 0
  [[ ! -e "$path" ]] && return 0

  if [[ -w "$path" ]]; then
    rm -rf "$path" || true
  else
    run_root rm -rf "$path" || true
  fi
}

# Clean PATH entries by removing a dedicated marked block (safer than grep -v dir)
cleanup_shell_rc() {
  local candidates=("$HOME/.zshrc" "$HOME/.bash_profile" "$HOME/.bashrc" "$HOME/.config/fish/config.fish")
  for file in "${candidates[@]}"; do
    [[ -f "$file" ]] || continue
    if grep -q ">>> Drip client" "$file" 2>/dev/null; then
      local tmp; tmp="$(mktemp)"
      awk '
        />>> Drip client/ {inblock=1; next}
        /<<< Drip client/ {inblock=0; next}
        !inblock {print}
      ' "$file" > "$tmp" || true
      mv "$tmp" "$file" || true
    fi
  done
}

# Locate client binary "drip" (best-effort)
find_client_binary() {
  local candidate=""
  candidate="$(command -v drip 2>/dev/null || true)"
  if [[ -n "$candidate" ]] && [[ -x "$candidate" ]]; then
    echo "$candidate"; return
  fi
  for candidate in "/usr/local/bin/drip" "/usr/bin/drip" "/opt/drip/drip" "$HOME/.local/bin/drip"; do
    [[ -x "$candidate" ]] && { echo "$candidate"; return; }
  done
}

# Extract actual binary path from systemd ExecStart (server install truth source)
get_systemd_exec_binary() {
  local unit="drip-server.service"
  command -v systemctl >/dev/null 2>&1 || return 0
  local val; val="$(systemctl show -p ExecStart "$unit" --value 2>/dev/null || true)"
  [[ -z "$val" ]] && return 0

  # Typical format includes "path=/path/to/bin"
  local bin
  bin="$(echo "$val" | sed -n 's/.*path=\([^ ;]*\).*/\1/p' | head -n1)"
  [[ -n "$bin" ]] && echo "$bin"
}

# Get real unit file path, if present
get_systemd_unit_path() {
  local unit="drip-server.service"
  command -v systemctl >/dev/null 2>&1 || return 0
  systemctl show -p FragmentPath "$unit" --value 2>/dev/null || true
}

select_language() {
  # Avoid blocking when no TTY (e.g., non-interactive CI)
  if ! has_tty; then return; fi

  print_panel "$(msg select_lang)" \
    "${GREEN}1)${NC} $(msg lang_en)" \
    "${GREEN}2)${NC} $(msg lang_zh)"
  tty_put "Select [1]: "
  local choice; tty_read choice
  case "$choice" in
    2) LANG_CODE="zh" ;;
    *) LANG_CODE="en" ;;
  esac
}

select_target() {
  # If no TTY, default to client to avoid hang
  if ! has_tty; then
    TARGET="client"
    return
  fi

  print_menu "$(msg select_target)" \
    "${GREEN}1)${NC} $(msg opt_client)" \
    "${GREEN}2)${NC} $(msg opt_server)" \
    "${GREEN}3)${NC} $(msg opt_both)"
  tty_put "Select [1]: "
  local choice; tty_read choice
  case "$choice" in
    2) TARGET="server" ;;
    3) TARGET="both" ;;
    *) TARGET="client" ;;
  esac
}

remove_client() {
  print_subheader "$(msg removing_client)"

  local binary_path=""
  binary_path="$(find_client_binary || true)"

  # If server uses the same binary, avoid accidental removal unless user confirms
  local server_bin=""
  server_bin="$(get_systemd_exec_binary || true)"

  if [[ -n "$binary_path" ]]; then
    if [[ -n "$server_bin" ]] && [[ "$server_bin" == "$binary_path" ]]; then
      say_warn "Detected shared binary with server: $binary_path"
      if [[ "$(prompt_yes "$(msg still_delete_shared)" true)" == "y" ]]; then
        remove_path "$binary_path"
        say_ok "Removed binary: $binary_path"
      else
        say_info "$(msg skip)"
      fi
    else
      remove_path "$binary_path"
      say_ok "Removed binary: $binary_path"
    fi

    cleanup_shell_rc
    say_ok "$(msg clean_path)"
  else
    say_warn "$(msg no_binary)"
  fi

  if [[ "$(prompt_yes "$(msg remove_config)" true)" == "y" ]]; then
    remove_path "$HOME/.drip"
    remove_path "$HOME/.config/drip"
    say_ok "Client config removed"
  else
    say_info "$(msg skip)"
  fi
}

remove_server() {
  print_subheader "$(msg removing_server)"

  if ! need_root; then
    say_warn "$(msg need_root)"
  fi

  local unit="drip-server.service"

  if command -v systemctl >/dev/null 2>&1; then
    say_info "$(msg stop_service)"
    run_root systemctl stop drip-server 2>/dev/null || true
    run_root systemctl disable drip-server 2>/dev/null || true
  fi

  say_info "$(msg remove_service)"
  local unit_path=""
  unit_path="$(get_systemd_unit_path || true)"

  # Remove unit file from common locations
  if [[ -n "$unit_path" ]] && [[ -e "$unit_path" ]]; then
    remove_path "$unit_path"
  fi
  remove_path "/etc/systemd/system/${unit}"
  remove_path "/lib/systemd/system/${unit}"
  remove_path "/usr/lib/systemd/system/${unit}"

  if command -v systemctl >/dev/null 2>&1; then
    run_root systemctl daemon-reload 2>/dev/null || true
    run_root systemctl reset-failed 2>/dev/null || true
  fi

  # Remove server binary using the real ExecStart path first
  local server_bin=""
  server_bin="$(get_systemd_exec_binary || true)"
  if [[ -n "$server_bin" ]]; then
    remove_path "$server_bin"
    say_ok "Removed server binary: $server_bin"
  else
    # Fallback guesses
    remove_path "/usr/local/bin/drip-server"
    remove_path "/usr/bin/drip-server"
    remove_path "/usr/local/bin/drip"
  fi

  if [[ "$(prompt_yes "$(msg remove_config)" true)" == "y" ]]; then
    remove_path "/etc/drip"
    say_ok "Server config removed"
  else
    say_info "$(msg skip)"
  fi

  if [[ "$(prompt_yes "$(msg remove_data)" true)" == "y" ]]; then
    remove_path "/var/lib/drip"
    remove_path "/var/log/drip"
    say_ok "Server data/log removed"
  else
    say_info "$(msg skip)"
  fi
}

parse_args() {
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --yes|-y) AUTO_YES=true ;;
      --client) TARGET="client" ;;
      --server) TARGET="server" ;;
      --all|--both) TARGET="both" ;;
      --lang=*) LANG_CODE="${1#*=}" ;;
      --lang) shift; LANG_CODE="${1:-$LANG_CODE}" ;;
      *) ;;
    esac
    shift
  done
}

main() {
  parse_args "$@"

  # Note: If you run via `curl ... | bash`, TTY prompts still work.
  select_language
  print_panel "$(msg title)"

  if [[ "$TARGET" == "prompt" ]]; then
    select_target
  fi

  local target_desc=""
  case "$TARGET" in
    client) target_desc="$(msg opt_client)" ;;
    server) target_desc="$(msg opt_server)" ;;
    both) target_desc="$(msg opt_both)" ;;
  esac
  print_subheader "$(msg target_label): ${target_desc}"

  # This is where older versions "looked stuck": it was waiting for y/n input.
  if [[ "$(prompt_yes "$(msg confirm)" false)" != "y" ]]; then
    say_info "$(msg skip)"
    exit 0
  fi

  case "$TARGET" in
    client) remove_client ;;
    server) remove_server ;;
    both)
      remove_client
      remove_server
      ;;
  esac

  say_ok "$(msg done)"
}

main "$@"
