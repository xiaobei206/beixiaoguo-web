#!/bin/sh
# Install/update Beixiaoguo as a persistent OpenWrt procd service.
# The binary and service definitions persist across reboot; runtime state and logs stay in RAM.
set -eu

REPO="xiaobei206/beixiaoguo-web"
VERSION_URL="https://raw.githubusercontent.com/${REPO}/main/VERSION"
INIT_URL="https://raw.githubusercontent.com/${REPO}/main/scripts/beixiaoguo.init"
WATCHDOG_URL="https://raw.githubusercontent.com/${REPO}/main/scripts/beixiaoguo-watchdog.sh"
APP_BIN="/usr/bin/beixiaoguo"
INIT_SCRIPT="/etc/init.d/beixiaoguo"
WATCHDOG_BIN="/usr/bin/beixiaoguo-watchdog.sh"
WORK_DIR="/tmp/beixiaoguo-install.$$"
APP_STAGE="/usr/bin/.beixiaoguo.new.$$"
INIT_STAGE="/etc/init.d/.beixiaoguo.new.$$"
WATCHDOG_STAGE="/usr/bin/.beixiaoguo-watchdog.new.$$"

say() {
  printf '%s\n' "$*"
}

fail() {
  say "错误：$*" >&2
}

cleanup() {
  rm -rf "$WORK_DIR"
  rm -f "$APP_STAGE" "$INIT_STAGE" "$WATCHDOG_STAGE"
}
trap cleanup EXIT HUP INT TERM

if [ "$(id -u)" != "0" ]; then
  fail "请通过 root SSH 会话运行此脚本"
  exit 1
fi
if ! command -v wget >/dev/null 2>&1; then
  fail "找不到 wget"
  exit 1
fi
if ! command -v sha256sum >/dev/null 2>&1; then
  fail "找不到 sha256sum，无法校验下载文件"
  exit 1
fi
if [ ! -x /etc/rc.common ] || [ ! -f /lib/functions/procd.sh ] || ! command -v ubus >/dev/null 2>&1; then
  fail "未检测到 OpenWrt / iStoreOS 的 procd 服务管理环境"
  exit 1
fi

ARCH="$(uname -m)"
case "$ARCH" in
  aarch64|arm64)
    ASSET_ARCH="arm64"
    ;;
  x86_64|amd64)
    ASSET_ARCH="amd64"
    ;;
  *)
    fail "暂不支持设备架构：$ARCH"
    exit 1
    ;;
esac
LEGACY_BIN="/tmp/beixiaoguo/beixiaoguo-linux-${ASSET_ARCH}"

VERSION="$(wget -T 15 -qO- "$VERSION_URL" | tr -d '\r\n')"
case "$VERSION" in
  [0-9]*.[0-9]*.[0-9]*) ;;
  *)
    fail "无法读取有效版本号：$VERSION"
    exit 1
    ;;
esac

ASSET="beixiaoguo-v${VERSION}-linux-${ASSET_ARCH}"
SUMS="beixiaoguo-v${VERSION}-SHA256SUMS.txt"
RELEASE_URL="https://github.com/${REPO}/releases/download/v${VERSION}"

mkdir -p "$WORK_DIR"
say "正在下载贝小果 v${VERSION}（${ARCH}）……"
wget -T 60 -qO "${WORK_DIR}/${ASSET}" "${RELEASE_URL}/${ASSET}"
wget -T 30 -qO "${WORK_DIR}/${SUMS}" "${RELEASE_URL}/${SUMS}"

if ! awk -v asset="$ASSET" '
  $2 == asset { print; count++ }
  END { if (count != 1) exit 1 }
' "${WORK_DIR}/${SUMS}" > "${WORK_DIR}/selected.sha256"; then
  fail "SHA-256 清单中没有且仅有一个当前架构文件：$ASSET"
  exit 1
fi
if ! (cd "$WORK_DIR" && sha256sum -c selected.sha256); then
  fail "程序 SHA-256 校验失败，旧服务未被停止"
  exit 1
fi

# Download and syntax-check service definitions before touching the running app.
wget -T 15 -qO "${WORK_DIR}/beixiaoguo.init" "$INIT_URL"
wget -T 15 -qO "${WORK_DIR}/beixiaoguo-watchdog.sh" "$WATCHDOG_URL"
if ! sh -n "${WORK_DIR}/beixiaoguo.init" || ! sh -n "${WORK_DIR}/beixiaoguo-watchdog.sh"; then
  fail "开机服务或健康监控脚本语法检查失败，旧服务未被停止"
  exit 1
fi

# Stage everything on its target filesystem so the final rename is atomic.
cp "${WORK_DIR}/${ASSET}" "$APP_STAGE"
chmod 755 "$APP_STAGE"
cp "${WORK_DIR}/beixiaoguo.init" "$INIT_STAGE"
chmod 755 "$INIT_STAGE"
cp "${WORK_DIR}/beixiaoguo-watchdog.sh" "$WATCHDOG_STAGE"
chmod 755 "$WATCHDOG_STAGE"

# Preserve an existing persistent binary, or migrate the currently running
# legacy /tmp binary on this boot so a failed update can be rolled back.
if [ -x "$APP_BIN" ]; then
  cp "$APP_BIN" "${WORK_DIR}/previous"
elif [ -x "$LEGACY_BIN" ]; then
  cp "$LEGACY_BIN" "${WORK_DIR}/previous"
fi

stop_old_processes() {
  for proc in /proc/[0-9]*; do
    [ -e "$proc/exe" ] || continue
    pid="${proc#/proc/}"
    exe="$(readlink "$proc/exe" 2>/dev/null || true)"
    case "${exe##*/}" in
      beixiaoguo|beixiaoguo-linux-arm64|beixiaoguo-linux-amd64|beixiaoguo-v*-linux-arm64|beixiaoguo-v*-linux-amd64)
        kill "$pid" 2>/dev/null || true
        ;;
    esac
  done
  sleep 1
}

restore_previous() {
  [ -x "$INIT_SCRIPT" ] && "$INIT_SCRIPT" stop >/dev/null 2>&1 || true
  stop_old_processes
  if [ -f "${WORK_DIR}/previous" ]; then
    cp "${WORK_DIR}/previous" "${APP_BIN}.rollback.$$"
    chmod 755 "${APP_BIN}.rollback.$$"
    mv -f "${APP_BIN}.rollback.$$" "$APP_BIN"
    if [ -x "$INIT_SCRIPT" ] && [ -x "$WATCHDOG_BIN" ]; then
      "$INIT_SCRIPT" enable >/dev/null 2>&1 || true
      "$INIT_SCRIPT" start >/dev/null 2>&1 || true
      say "已恢复更新前的程序并尝试重新启动服务"
    else
      say "已恢复旧程序文件，但服务定义不完整；请保留本次诊断输出"
      nohup "$APP_BIN" >/tmp/beixiaoguo.log 2>&1 </dev/null &
    fi
  else
    [ -x "$INIT_SCRIPT" ] && "$INIT_SCRIPT" disable >/dev/null 2>&1 || true
    [ -x "$INIT_SCRIPT" ] && "$INIT_SCRIPT" stop >/dev/null 2>&1 || true
    rm -f "$APP_BIN" "$WATCHDOG_BIN"
    say "本次没有可回滚的旧程序，已停止并禁用失败的服务"
  fi
}

[ -x "$INIT_SCRIPT" ] && "$INIT_SCRIPT" stop >/dev/null 2>&1 || true
stop_old_processes

if ! mv -f "$APP_STAGE" "$APP_BIN"; then
  fail "无法安装程序到 $APP_BIN"
  restore_previous
  exit 1
fi
if ! mv -f "$WATCHDOG_STAGE" "$WATCHDOG_BIN"; then
  fail "无法安装健康监控脚本"
  restore_previous
  exit 1
fi
if ! mv -f "$INIT_STAGE" "$INIT_SCRIPT"; then
  fail "无法安装 procd 服务脚本"
  restore_previous
  exit 1
fi

if ! "$INIT_SCRIPT" enable; then
  fail "服务开机自启注册失败"
  restore_previous
  exit 1
fi
"$INIT_SCRIPT" restart || true

HEALTH=""
attempt=0
while [ "$attempt" -lt 25 ]; do
  attempt=$((attempt + 1))
  HEALTH="$(wget -T 3 -qO- http://127.0.0.1:8780/api/v1/health 2>/dev/null || true)"
  if [ -n "$HEALTH" ]; then
    break
  fi
  sleep 1
done

if [ -z "$HEALTH" ]; then
  fail "新服务启动失败或健康检查未通过，最近相关日志如下："
  logread 2>/dev/null | grep -i beixiaoguo | tail -n 60 || true
  restore_previous
  exit 1
fi

rm -f "$LEGACY_BIN"
say "贝小果 v${VERSION} 已启动，且已注册开机自启。"
say "程序位置：$APP_BIN（重启后保留）"
say "访问地址：http://N1_IP:8780"
say "健康检查：$HEALTH"
say "服务状态：$INIT_SCRIPT status"
say "查看日志：logread | grep -i beixiaoguo（OpenWrt RAM 日志）"
say "守护机制：procd 异常退出自动拉起；本机健康检查连续失败 3 次后请求重启服务实例"
