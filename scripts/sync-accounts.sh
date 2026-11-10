#!/usr/bin/env bash
# 在 PC 网关与手机 App 之间同步 CodeArts 账号凭据，防止「互相顶号」。
#
# 背景（为什么需要这个脚本）
#   华为云的 refresh_token 是**一次性**的：每刷新成功一次，服务端签发新的、作废旧值，
#   而新值只落在**发起刷新的那一侧**本地。两侧各刷各的，就必然互相作废：
#     A 侧刷新成功 → 作废旧 refresh_token → B 侧拿旧值刷新被拒
#     → RefreshTokenExpiredError → pool.Disable() → UI 显示「已封禁」
#   （见 internal/pool/pool.go:480、internal/upstream/client.go:341）
#
# 本脚本做什么
#   逐账号比对两侧的 updated_at，**取较新的一侧覆盖另一侧**，然后：
#     1. 把合并后的凭据推回两侧；
#     2. 调 /admin/api/reload 让运行中的网关重新读凭据；
#     3. 对被覆盖的账号调 /admin/api/accounts/enable，清掉 state.json 里的禁用标记
#        （reload 只换凭据，不清 disabled）。
#
# 用法
#   scripts/sync-accounts.sh                 # 同步（自动选 PJA110）
#   scripts/sync-accounts.sh --dry-run       # 只报告差异，不动任何东西
#   scripts/sync-accounts.sh -s <serial>     # 指定设备
#
# 前置
#   - 手机已 root（KernelSU/Magisk），否则读不到 App 私有目录；
#   - 手机 App 已安装并运行过（config.json 存在）；
#   - PC 网关在跑（可选：不在跑就只做文件同步，不 reload）。
#
# 环境变量
#   PC_PORT=7866         PC 网关端口
#   PC_AUTH_DIR=bin/auths
#   APP_PKG=com.codearts.panel
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
APP_PKG="${APP_PKG:-com.codearts.panel}"
PC_PORT="${PC_PORT:-7866}"
PC_AUTH_DIR="${PC_AUTH_DIR:-$ROOT/bin/auths}"
DRY_RUN=0
FILES_ONLY=0
SERIAL=""

while [ $# -gt 0 ]; do
	case "$1" in
		--dry-run) DRY_RUN=1 ;;
		--files-only) FILES_ONLY=1 ;;
		-s|--serial) SERIAL="${2:-}"; shift ;;
		-h|--help) sed -n '2,32p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
		*) echo "未知参数: $1" >&2; exit 2 ;;
	esac
	shift
done

log() { printf '[sync] %s\n' "$*"; }
die() { printf '[sync] 错误: %s\n' "$*" >&2; exit 1; }

# ---------------------------------------------------------------- 选设备
pick_device() {
	if [ -n "$SERIAL" ]; then
		adb -s "$SERIAL" get-state >/dev/null 2>&1 || die "设备 $SERIAL 未连接"
		echo "$SERIAL"; return
	fi
	# 优先 PJA110，其次第一台在线设备
	for s in $(adb devices | awk 'NR>1 && $2=="device"{print $1}'); do
		m="$(adb -s "$s" shell getprop ro.product.model 2>/dev/null | tr -d '\r')"
		[ "$m" = "PJA110" ] && { echo "$s"; return; }
	done
	local first
	first="$(adb devices | awk 'NR>1 && $2=="device"{print $1; exit}')"
	[ -n "$first" ] || die "没有在线的 adb 设备"
	echo "$first"
}

DEV="$(pick_device)"
log "设备: $DEV"

# ---------------------------------------------------------------- 前置检查
adb -s "$DEV" shell "pm path $APP_PKG" >/dev/null 2>&1 || die "设备上未安装 $APP_PKG"
adb -s "$DEV" shell "su -c id" 2>/dev/null | grep -q "uid=0" || die "设备无 root（读不到 App 私有目录）"

PHONE_FILES="/data/data/$APP_PKG/files"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

# Windows 版 adb 不认 MSYS 风格路径（/tmp/...、/d/...），凡是传给 adb 的本地路径
# 都要转成 Windows 形式（C:\...）。Git-Bash 有 cygpath；非 Windows 上原样不变。
winpath() {
	if command -v cygpath >/dev/null 2>&1; then cygpath -w "$1"; else printf '%s' "$1"; fi
}

# 把手机凭据目录整目录拉到本地（root 先复制到 /data/local/tmp，再 pull——pull 不支持 su）
log "读取手机凭据…"
if ! adb -s "$DEV" shell "su -c 'test -d $PHONE_FILES/auths'" >/dev/null 2>&1; then
	die "手机上没有 $PHONE_FILES/auths（App 还没导入过账号？先在面板里加一个账户再同步）"
fi
if ! adb -s "$DEV" shell "su -c 'rm -rf /data/local/tmp/.sync-auths && cp -r $PHONE_FILES/auths /data/local/tmp/.sync-auths && chmod -R 755 /data/local/tmp/.sync-auths'"; then
	die "复制手机凭据到 /data/local/tmp 失败"
fi
adb -s "$DEV" pull /data/local/tmp/.sync-auths "$(winpath "$WORK/phone")" >/dev/null 2>&1 || die "从设备拉取凭据失败"
adb -s "$DEV" shell "su -c 'rm -rf /data/local/tmp/.sync-auths'" >/dev/null 2>&1 || true
[ -n "$(ls -A "$WORK/phone" 2>/dev/null)" ] || die "手机凭据目录是空的"

mkdir -p "$WORK/pc"
cp "$PC_AUTH_DIR"/codearts-*.json "$WORK/pc/" 2>/dev/null || true

# ---------------------------------------------------------------- 比对与合并
# Python 负责真正的决策：逐账号取 updated_at 较新的一侧；缺的一侧直接补。
log "比对 updated_at…"
# Windows 上的 Python 同样认不了 MSYS 路径（/tmp/... 会被解析成 C:\tmp\...），
# 所以要传 Windows 形式。MSYS 的 /tmp 与 cygpath 给出的 Windows 目录是同一个，
# 因此 bash 之后用 $WORK/... 仍能读到 Python 写出的文件。
python - "$(winpath "$WORK/pc")" "$(winpath "$WORK/phone")" "$(winpath "$WORK/merged")" "$DRY_RUN" <<'PY'
import json, os, sys, glob

pc_dir, phone_dir, out_dir, dry = sys.argv[1], sys.argv[2], sys.argv[3], sys.argv[4] == '1'
os.makedirs(out_dir, exist_ok=True)

def load(d):
    out = {}
    for f in glob.glob(os.path.join(d, 'codearts-*.json')):
        try:
            a = json.load(open(f, encoding='utf-8'))
        except Exception:
            continue
        uid = a.get('user_id') or os.path.basename(f)[len('codearts-'):-len('.json')]
        a['_file'] = f
        out[uid] = a
    return out

pc, ph = load(pc_dir), load(phone_dir)
only_pc = sorted(set(pc) - set(ph))
only_ph = sorted(set(ph) - set(pc))
both = sorted(set(pc) & set(ph))

print(f'  PC 独有 {len(only_pc)} 个；手机独有 {len(only_ph)} 个；共有 {len(both)} 个')

decisions = {}  # uid -> (winner, reason)
for uid in only_pc:
    decisions[uid] = (pc[uid], 'PC 独有 → 补到手机')
for uid in only_ph:
    decisions[uid] = (ph[uid], '手机独有 → 补到 PC')
for uid in both:
    p, q = pc[uid].get('updated_at', 0) or 0, ph[uid].get('updated_at', 0) or 0
    if p == q:
        decisions[uid] = (pc[uid], '一致')
    elif p > q:
        decisions[uid] = (pc[uid], f'PC 较新（{p} > {q}）→ 覆盖手机')
    else:
        decisions[uid] = (ph[uid], f'手机较新（{q} > {p}）→ 覆盖 PC')

for uid, (a, why) in sorted(decisions.items()):
    a = dict(a)
    a.pop('_file', None)
    json.dump(a, open(os.path.join(out_dir, f'codearts-{uid}.json'), 'w', encoding='utf-8'),
              ensure_ascii=False, indent=2)
    print(f'  {uid[:16]}… {why}')

# 供 shell 判断是否需要动作
json.dump(sorted(decisions), open(os.path.join(out_dir, '..', 'uids.json'), 'w'))
PY

MERGED="$WORK/merged"
COUNT="$(ls "$MERGED"/codearts-*.json 2>/dev/null | wc -l | tr -d ' ')"
[ "$COUNT" -gt 0 ] || die "合并后没有任何账号"
log "合并结果: $COUNT 个账号"

if [ "$DRY_RUN" = "1" ]; then
	log "--dry-run：以上为将执行的动作，未修改任何文件。"
	exit 0
fi

# ---------------------------------------------------------------- 写回两侧
log "备份并写回手机…"
adb -s "$DEV" shell "su -c 'cp -r $PHONE_FILES/auths /data/local/tmp/.sync-backup-\$(date +%s)'" >/dev/null 2>&1 || true
adb -s "$DEV" shell "rm -rf /data/local/tmp/.sync-new" >/dev/null 2>&1
adb -s "$DEV" push "$(winpath "$MERGED")" /data/local/tmp/.sync-new >/dev/null 2>&1 || die "推送合并凭据失败"
# 目录属主（chown 需要，先取出来再拼进 su 命令，避免在 su -c 字符串里嵌套 adb）
OWNER="$(adb -s "$DEV" shell "stat -c '%u %g' $PHONE_FILES/auths" | tr -d '\r')"
FUID="${OWNER%% *}"; FGID="${OWNER##* }"
adb -s "$DEV" shell "su -c '
  rm -f $PHONE_FILES/auths/*.json
  cp /data/local/tmp/.sync-new/*.json $PHONE_FILES/auths/
  chown $FUID:$FGID $PHONE_FILES/auths/*.json
  chmod 600 $PHONE_FILES/auths/*.json
  rm -rf /data/local/tmp/.sync-new
  ls $PHONE_FILES/auths/ | wc -l
'"

log "备份并写回 PC…"
if [ -d "$PC_AUTH_DIR" ]; then
	BAK="$PC_AUTH_DIR.bak-sync-$(date +%s)"
	cp -r "$PC_AUTH_DIR" "$BAK" 2>/dev/null || true
	rm -f "$PC_AUTH_DIR"/codearts-*.json
	cp "$MERGED"/codearts-*.json "$PC_AUTH_DIR/"
	log "  PC 备份在 $BAK"
fi

# ---------------------------------------------------------------- 让运行中的网关重读
if [ "$FILES_ONLY" = "1" ]; then
	log "--files-only：只同步了文件，未联系运行中的网关（下次启动即读取新凭据）。"
	exit 0
fi

# 读 admin_key。路径同样要转 Windows 形式，否则 Windows 版 Python 找不到文件、
# 返回空 key，脚本会误判「PC 网关未运行」而跳过 reload/enable。
pc_key() { python -c "import json,sys;print(json.load(open(sys.argv[1],encoding='utf-8')).get('admin_key',''))" "$(winpath "$ROOT/bin/config.json")" 2>/dev/null || true; }
phone_key() { adb -s "$DEV" shell "su -c 'cat $PHONE_FILES/config.json'" 2>/dev/null | python -c "import json,sys;print(json.load(sys.stdin).get('admin_key',''))" 2>/dev/null || true; }

PC_KEY="$(pc_key)"
# 用 http_code 判断存活，而不是 curl 的退出码——Git-Bash 里的 Windows curl 有时
# 会为 `-o /dev/null` 报 exit 23（写文件失败），明明端点正常却判成「未运行」。
# deploy-local.sh 用的也是这个写法。
pc_up() { [ "$(curl -s -m 3 -o /dev/null -w '%{http_code}' "http://127.0.0.1:$PC_PORT/healthz" 2>/dev/null)" = "200" ]; }
if [ -n "$PC_KEY" ] && pc_up; then
	log "PC 网关重载凭据…"
	curl -s -m 30 -X POST -H "Authorization: Bearer $PC_KEY" "http://127.0.0.1:$PC_PORT/admin/api/reload" | head -c 200; echo
	# tr -d '\r'：Windows 版 python 的 print 输出 CRLF，命令替换只吃掉 LF，
	# 残留的 \r 会让 uid 变成 "xxx\r"，服务端找不到账号、enable 静默失效。
	for uid in $(python -c "import json,sys;print('\n'.join(json.load(open(sys.argv[1]))))" "$(winpath "$WORK/uids.json")" | tr -d '\r'); do
		curl -s -m 10 -X POST -H "Authorization: Bearer $PC_KEY" -H 'Content-Type: application/json' \
			-d "{\"uid\":\"$uid\"}" "http://127.0.0.1:$PC_PORT/admin/api/accounts/enable" >/dev/null
	done
	log "  PC 已重载并清除禁用标记"
else
	log "PC 网关未运行，跳过 reload（下次启动即读取新凭据）"
fi

PH_KEY="$(phone_key)"
phone_up() { [ "$(adb -s "$DEV" shell "curl -s -m 3 -o /dev/null -w '%{http_code}' http://127.0.0.1:17866/healthz" 2>/dev/null | tr -d '\r')" = "200" ]; }
if [ -n "$PH_KEY" ] && phone_up; then
	log "手机网关重载凭据…"
	adb -s "$DEV" shell "curl -s -m 30 -X POST -H 'Authorization: Bearer $PH_KEY' http://127.0.0.1:17866/admin/api/reload" | head -c 200; echo
	for uid in $(python -c "import json,sys;print('\n'.join(json.load(open(sys.argv[1]))))" "$(winpath "$WORK/uids.json")" | tr -d '\r'); do
		adb -s "$DEV" shell "curl -s -m 10 -X POST -H 'Authorization: Bearer $PH_KEY' -H 'Content-Type: application/json' -d '{\"uid\":\"$uid\"}' http://127.0.0.1:17866/admin/api/accounts/enable" >/dev/null
	done
	log "  手机已重载并清除禁用标记"
else
	log "手机网关未运行，跳过 reload（下次启动即读取新凭据）"
fi

log "完成。"
log "提示：本脚本只是事后修补。要根治顶号，请在**从属那一侧**勾选面板的"
log "      「只读模式（不自动刷新 token）」——它让自动路径（巡检/请求/余额采样）"
log "      只校验不轮换，只有手动「保活」才刷新。或者干脆只在一侧部署。"
