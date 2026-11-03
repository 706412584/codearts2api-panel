#!/usr/bin/env bash
# 本地部署 codearts2api-panel 到 :7866（Git Bash / Windows）。
#
# 设计要点（与协作约定一致）：
#   1. 先构建到 .new，构建或测试失败即退出——绝不在失败后停服务，
#      避免网关处于不可控的停机窗口；
#   2. 构建与测试全绿后，才「停 → 备份 → 替换 → 启 → 校验」一气呵成；
#   3. Windows 上不能覆盖运行中的 exe，顺序必须是先停再 mv，不能热替换；
#   4. 末尾 curl /healthz 必须返回 200 才算成功，否则以非零码退出。
#
# 环境变量：
#   PORT=7866        目标端口
#   SKIP_TESTS=1     跳过 go test（仅构建，快速迭代时用）
#   CONFIG=config.json  传给进程的配置文件（相对 bin/）
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN="$ROOT/bin"
EXE="$BIN/codearts2api-panel.exe"
NEW="$EXE.new"
CONFIG="${CONFIG:-config.json}"
PORT="${PORT:-7866}"
SKIP_TESTS="${SKIP_TESTS:-0}"

log() { printf '[deploy] %s\n' "$*"; }
listen_pids() { netstat -ano 2>/dev/null | grep ":$PORT " | grep LISTENING | awk '{print $NF}' | sort -u || true; }

# 1) 构建到 .new（失败即退出，服务不受影响）
log "构建 -> $NEW"
( cd "$ROOT" && go build -trimpath -o "$NEW" ./cmd/server )

# 2) 测试（可跳过）
if [ "$SKIP_TESTS" != "1" ]; then
	log "运行 go test ./..."
	( cd "$ROOT" && go test ./... )
fi

# 3) 停旧进程：taskkill 失败回落 Stop-Process -Force，并等端口释放
pids="$(listen_pids)"
if [ -n "$pids" ]; then
	for pid in $pids; do
		log "停止 PID=$pid"
		taskkill //F //PID "$pid" >/dev/null 2>&1 ||
			powershell -NoProfile -Command "Stop-Process -Id $pid -Force" >/dev/null 2>&1 || true
	done
	for _ in $(seq 1 10); do
		[ -z "$(listen_pids)" ] && break
		sleep 1
	done
else
	log "无监听进程，跳过停止"
fi

# 4) 备份现有二进制（时间戳命名，天然避免与并发部署互删）
if [ -f "$EXE" ]; then
	bak="$EXE.bak-$(date +%Y%m%d-%H%M%S)"
	cp "$EXE" "$bak"
	log "备份 -> $bak"
fi

# 5) 替换（此时旧进程已停，Windows 允许覆盖）
mv -f "$NEW" "$EXE"

# 6) 启动
log "启动 $EXE -config $CONFIG"
( cd "$BIN" && powershell -NoProfile -Command \
	"Start-Process -FilePath '.\\codearts2api-panel.exe' -ArgumentList '-config','$CONFIG' -WindowStyle Hidden" )

# 7) 健康校验：/healthz 必须 200
for _ in $(seq 1 15); do
	code="$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$PORT/healthz" || true)"
	if [ "$code" = "200" ]; then
		log "healthz=200 OK（:$PORT）"
		exit 0
	fi
	sleep 1
done
log "健康检查失败：http://127.0.0.1:$PORT/healthz 未在超时内返回 200"
exit 1
