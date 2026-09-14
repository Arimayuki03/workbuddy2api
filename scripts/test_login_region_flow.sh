#!/usr/bin/env bash
# test_login_region_flow.sh — login.sh global 分支注册地区自动完善的回归测试。
#
# 做法：从 login.sh 提取「国际版注册激活」块的 PYEOF python 源码，经 bash 重建为临时脚本
# 再执行——这一步走真实 heredoc 插值路径（$USER_ID/$TOKEN 由 bash 内插，验证能拿到真实值
# 而不是字面量），同时把块里的 GLOBAL_BASE 替换为本地桩服务器（不起真实网络）。三个场景：
#   S1 region required → 自动检测 SG → 提交地区 → register 重验成功 → trial 领取
#   S2 已激活 → 不提交地区 → trial 领取
#   S3 trial 已领（14051 幂等码）→ 输出「已领取过」
# 断言同时检查桩上游收到的 SUBMIT body 符合逆向格式
# （attributes.countryCode / countryFullName / countryName）。
#
# 依赖：login.sh global 分支的 marker 注释「# ─── 国际版注册激活」保持存在（提取锚点）。
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

LOGIN="$ROOT/login.sh"
[[ -f "$LOGIN" ]] || { echo "缺少 login.sh"; exit 1; }

TMP="$(mktemp -d)"
STUB_PID=""
cleanup() {
  [[ -n "$STUB_PID" ]] && kill "$STUB_PID" 2>/dev/null || true
  rm -rf "$TMP"
}
trap cleanup EXIT

# ─── 提取 login.sh 国际版注册激活块（heredoc 内部 python 源码）─────────────
extract_block() {
  awk -v mk='# ─── 国际版注册激活' '
    index($0, mk) { f = 1 }
    f && /^python3 - <<PYEOF$/ { inb = 1; next }
    inb && /^PYEOF$/ { exit }
    inb { print }' "$LOGIN"
}

# ─── 启动桩上游：$1 register 形态(need|ok)，$2 trial 形态(ok|14051)，$3=端口文件 ──
start_stub() {
  local rmode="$1" tmode="$2"
  rm -f "$TMP/port"  # 清掉上一场景的端口，避免读到过期值
  RSTUB_RMODE="$rmode" TSTUB_TMODE="$tmode" TSTUB_HITS="$TMP/hits" \
  python3 - "$TMP/port" <<'PYSTUB' &
import json, os, sys, http.server, socketserver

port_file = sys.argv[1]
rmode = os.environ["RSTUB_RMODE"]
tmode = os.environ["TSTUB_TMODE"]
hitsf = os.environ["TSTUB_HITS"]

class H(http.server.BaseHTTPRequestHandler):
    def log_message(self, *a):
        pass

    def _send(self, o, s=200, tag=None):
        if tag is None:
            tag = self.path
        with open(hitsf, "a") as f:
            f.write(tag + "\t" + json.dumps(o, ensure_ascii=False) + "\n")
        d = json.dumps(o).encode()
        self.send_response(s)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(d)))
        self.end_headers()
        self.wfile.write(d)

    def do_GET(self):
        if self.path.startswith("/auth/realms/copilot/overseas/user/register"):
            # 插值断言：userId 必须来自 bash 内插（req-u1），否则块里是字面量 $USER_ID。
            if "userId=req-u1" not in self.path:
                self._send({"code": -1, "msg": "interpolation failed"})
                return
            n = getattr(self.server, "reg", 0)
            self.server.reg = n + 1
            if rmode == "need" and n == 0:
                self._send({"code": 500, "msg": "register region required"})
            else:
                self._send({"code": 200, "msg": "register success"})
        else:
            self._send({"code": -1, "msg": "unknown GET"})

    def do_POST(self):
        ln = int(self.headers.get("Content-Length", 0))
        raw = self.rfile.read(ln).decode() if ln else "{}"
        body = json.loads(raw) if raw else {}
        if self.path == "/billing/area/get-country-code":
            lst = [
                {"EnName": "Hong Kong", "Name": "中国香港", "IOS2": "HK", "IOS3": "HKG", "Code": "852"},
                {"EnName": "Singapore", "Name": "新加坡", "IOS2": "SG", "IOS3": "SGP", "Code": "65"},
                {"EnName": "Thailand", "Name": "泰国", "IOS2": "TH", "IOS3": "THA", "Code": "66"},
            ]
            self._send({"code": 0, "data": json.dumps({"code": 0, "data": {"list": lst}})})
        elif self.path == "/billing/area/get-user-area-info":
            self._send({"code": 0, "data": json.dumps(
                {"code": 0, "msg": "ok", "data": {"IOS2": "SG", "enName": "Singapore"}})})
        elif self.path == "/console/login/account":
            with open(hitsf, "a") as f:
                f.write("SUBMIT\t" + json.dumps(body, ensure_ascii=False) + "\n")
            self._send({"code": 0, "msg": "OK"})
        elif self.path == "/billing/ide/trial":
            if tmode == "14051":
                self._send({"code": 14051, "msg": "has applied trial"}, tag="TRIAL")
            else:
                self._send({"code": 0, "msg": "OK"}, tag="TRIAL")
        else:
            self._send({"code": -1, "msg": "unknown POST"})

class S(socketserver.ThreadingMixIn, http.server.HTTPServer):
    daemon_threads = True

srv = S(("127.0.0.1", 0), H)
open(port_file, "w").write(str(srv.server_address[1]))
srv.serve_forever()
PYSTUB
  local pid=$!
  for _ in $(seq 1 50); do
    [[ -s "$TMP/port" ]] && break
    sleep 0.1
  done
  [[ -s "$TMP/port" ]] || { echo "桩服务器未就绪"; exit 1; }
  STUB_PID="$pid"
}

# ─── 重建临时 bash 脚本执行构建块（真实 heredoc 插值），输出碰桩结果 ────────
run_block() {
  local base="$1"
  local block
  block="$(extract_block | sed "s|^GLOBAL_BASE = .*|GLOBAL_BASE = \"$base\"|")"
  [[ -n "$block" ]] || { echo "提取构建块失败（marker 锚点是否还在？）"; exit 1; }
  local script="$TMP/run.sh"
  {
    echo 'USER_ID="req-u1"'
    echo 'TOKEN="tok-test"'
    echo 'python3 - <<PYEOF'
    echo "$block"
    echo 'PYEOF'
  } > "$script"
  bash "$script" 2>&1
}

fail() { echo "  ✗ $1"; FAILED=1; }

# ─── 场景 1：region required → 自动检测 SG → 提交 → 重验 → trial ───────────
start_stub need ok
run_block "http://127.0.0.1:$(cat "$TMP/port")" > "$TMP/out1"
kill "$STUB_PID" 2>/dev/null; STUB_PID=""
kill "$(jobs -p)" 2>/dev/null || true
echo "── 场景1 region required → 自动完善 ——"
cat "$TMP/out1"

FAILED=0
grep -q "需完善注册地区" "$TMP/out1" || fail "未提示需完善地区"
grep -q "地区已完善: SG Singapore" "$TMP/out1" || fail "未检测并完善 SG"
grep -q "国际版 trial: 已激活" "$TMP/out1" || fail "trial 未激活"
grep -q "SUBMIT" "$TMP/hits" || fail "未提交地区到 /console/login/account"
grep -c "userId=req-u1" "$TMP/hits" | grep -q "^2$" || fail "register 应有且仅有 2 次（重验）"
if [[ "$FAILED" == "0" ]]; then
  python3 - "$TMP/hits" <<'PYASRT'
import json, sys
line = next(l for l in open(sys.argv[1]) if l.startswith("SUBMIT\t"))
body = json.loads(line.split("\t", 1)[1])
attrs = body["attributes"]
assert attrs["countryCode"] == ["65"], attrs
assert attrs["countryFullName"] == ["Singapore"], attrs
assert attrs["countryName"] == ["SG"], attrs
PYASRT
fi
[[ "$FAILED" == "0" ]] && echo "  ✓ 场景1 通过" || { echo "场景1 失败"; exit 1; }

# ─── 场景 2：已激活 → 跳过提交 → trial ────────────────────────────────────
: > "$TMP/hits"
start_stub ok ok
run_block "http://127.0.0.1:$(cat "$TMP/port")" > "$TMP/out2"
kill "$STUB_PID" 2>/dev/null; STUB_PID=""
kill "$(jobs -p)" 2>/dev/null || true
echo "── 场景2 已激活 ——"
cat "$TMP/out2"

FAILED=0
grep -q "注册激活: 成功" "$TMP/out2" || fail "已激活未打印成功"
grep -q "国际版 trial: 已激活" "$TMP/out2" || fail "trial 未激活"
grep -q "SUBMIT" "$TMP/hits" && fail "已激活不应提交地区"
[[ "$FAILED" == "0" ]] && echo "  ✓ 场景2 通过" || { echo "场景2 失败"; exit 1; }

# ─── 场景 3：trial 已领（14051）→ 幂等输出 ─────────────────────────────────
: > "$TMP/hits"
start_stub ok 14051
run_block "http://127.0.0.1:$(cat "$TMP/port")" > "$TMP/out3"
kill "$STUB_PID" 2>/dev/null; STUB_PID=""
kill "$(jobs -p)" 2>/dev/null || true
echo "── 场景3 trial 已领 14051 ——"
cat "$TMP/out3"

FAILED=0
grep -q "（已领取过）" "$TMP/out3" || fail "未打印已领取过"
grep -q "TRIAL" "$TMP/hits" || fail "未请求 trial"
grep -q "14051" "$TMP/out3" && fail "14051 不应作为失败回显"
[[ "$FAILED" == "0" ]] && echo "  ✓ 场景3 通过" || { echo "场景3 失败"; exit 1; }

echo "全部通过：登录脚本 global 分支自动完善地区流程 OK"