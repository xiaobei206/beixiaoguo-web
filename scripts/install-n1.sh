#!/bin/sh
# Download and install the latest Beixiaoguo Linux release using RAM-only paths.
set -eu

REPO="xiaobei206/beixiaoguo-web"
VERSION_URL="https://raw.githubusercontent.com/${REPO}/main/VERSION"
APP_DIR="/tmp/beixiaoguo"
APP_BIN="${APP_DIR}/beixiaoguo-linux-arm64"
LOG_FILE="/tmp/beixiaoguo.log"
WORK_DIR="/tmp/beixiaoguo-install.$$"

say() {
  printf '%s\n' "$*"
}

fail() {
  say "错误：$*" >&2
}

cleanup() {
  rm -rf "$WORK_DIR"
}
trap cleanup EXIT HUP INT TERM

if ! command -v wget >/dev/null 2>&1; then
  fail "找不到 wget"
  exit 1
fi
if ! command -v sha256sum >/dev/null 2>&1; then
  fail "找不到 sha256sum，无法校验下载文件"
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

mkdir -p "$WORK_DIR" "$APP_DIR"
say "正在下载贝小果 v${VERSION}（${ARCH}）……"
wget -T 60 -qO "${WORK_DIR}/${ASSET}" "${RELEASE_URL}/${ASSET}"
wget -T 30 -qO "${WORK_DIR}/${SUMS}" "${RELEASE_URL}/${SUMS}"

# The release checksum manifest contains both CPU architectures, while this
# device downloads only one binary. Select exactly the matching entry first.
if ! awk -v asset="$ASSET" '
  $2 == asset { print; count++ }
  END { if (count != 1) exit 1 }
' "$WORK_DIR/$SUMS" > "$WORK_DIR/selected.sha256"; then
  fail "SHA-256 清单中没有且仅有一个当前架构文件：$ASSET"
  exit 1
fi

if ! (cd "$WORK_DIR" && sha256sum -c selected.sha256); then
  fail "ARM/AMD64 程序 SHA-256 校验失败，已停止安装；旧程序未被停止"
  exit 1
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

if [ -x "$APP_BIN" ]; then
  cp "$APP_BIN" "${WORK_DIR}/previous"
fi

stop_old_processes
cp "${WORK_DIR}/${ASSET}" "${APP_BIN}.new"
chmod 755 "${APP_BIN}.new"
mv -f "${APP_BIN}.new" "$APP_BIN"

nohup "$APP_BIN" >"$LOG_FILE" 2>&1 </dev/null &
HEALTH=""
attempt=0
while [ "$attempt" -lt 20 ]; do
  attempt=$((attempt + 1))
  HEALTH="$(wget -T 3 -qO- http://127.0.0.1:8780/api/v1/health 2>/dev/null || true)"
  if [ -n "$HEALTH" ]; then
    break
  fi
  sleep 1
done

if [ -z "$HEALTH" ]; then
  fail "新版本启动失败，最近日志如下："
  tail -n 60 "$LOG_FILE" 2>/dev/null || true
  if [ -f "${WORK_DIR}/previous" ]; then
    stop_old_processes
    cp "${WORK_DIR}/previous" "${APP_BIN}.rollback"
    chmod 755 "${APP_BIN}.rollback"
    mv -f "${APP_BIN}.rollback" "$APP_BIN"
    nohup "$APP_BIN" >"$LOG_FILE" 2>&1 </dev/null &
    say "已恢复更新前的程序"
  fi
  exit 1
fi

say "贝小果 v${VERSION} 已启动。"
say "访问地址：http://N1_IP:8780"
say "健康检查：$HEALTH"
say "运行日志：$LOG_FILE"
