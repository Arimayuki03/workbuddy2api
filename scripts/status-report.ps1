# status-report.ps1 — 拉取 /healthz 与 /status 渲染中文状态摘要（启动服务.bat 菜单 9 调用）。
# 用 curl.exe 而非 Invoke-RestMethod：后者在 503（无可用账号）时抛异常，而 healthz
# 503 仍是有效响应（body 带 service/healthy 字段），必须能读回。
# 关键：PS5.1 默认按系统 ANSI 代码页（GBK）解码 curl 的 UTF-8 stdout，中文昵称会被
# 解成乱码并吞掉 JSON 引号，导致 ConvertFrom-Json 失败——显式设 OutputEncoding 为 UTF-8。
$OutputEncoding = [Console]::OutputEncoding = [System.Text.Encoding]::UTF8
$base = 'http://127.0.0.1:7863'
$proj = Split-Path $PSScriptRoot -Parent
$cfg  = Join-Path $proj 'config.json'

# 1) 探活：连接失败（服务未启动）时 curl 非零退出，给出明确指引
$hzRaw = & curl.exe -s -m 3 "$base/healthz" 2>$null
if ($LASTEXITCODE -ne 0 -or -not $hzRaw) {
    Write-Output "[状态] 服务未运行或无法连接（$base）。"
    Write-Output "[处理] 请先在菜单选 3 启动服务；已启动则选 5 查看日志排查。"
    exit 1
}
$hz = $hzRaw | ConvertFrom-Json
if (-not $hz -or -not $hz.service) {
    Write-Output "[错误] /healthz 响应解析失败（端口 7863 可能不是本网关）。"
    exit 1
}
Write-Output "--- /healthz 探活 ---"
Write-Output ("healthy={0}  total={1}  cn={2}  global={3}" -f $hz.healthy, $hz.total, $hz.realm_servable.cn, $hz.realm_servable.global)

# 2) /status 台账（需 api_key）
$key = $null
if (Test-Path $cfg) {
    try { $key = (Get-Content $cfg -Raw | ConvertFrom-Json).api_key } catch {}
}
if (-not $key) {
    Write-Output ""
    Write-Output "[提示] 未找到 config.json 或未配置 api_key，跳过 /status 台账。"
    exit 0
}
$stRaw = & curl.exe -s -m 5 -H "Authorization: Bearer $key" "$base/status" 2>$null
if ($LASTEXITCODE -ne 0 -or -not $stRaw) {
    Write-Output "[错误] /status 请求失败（api_key 不对，或服务刚停止）。"
    exit 1
}
$st = $stRaw | ConvertFrom-Json
if ($null -eq $st) {
    Write-Output "[错误] /status 响应解析失败。"
    exit 1
}

Write-Output ""
Write-Output "--- /status 台账 ---"
Write-Output ("账号: total={0}  healthy={1}  cooling={2}  disabled={3}  in_flight_full={4}" -f $st.total, $st.healthy, $st.cooling, $st.disabled, $st.in_flight_full)
Write-Output ("会话: sticky={0}  redis={1}" -f $st.sticky_sessions, $st.redis_mode)
$rt = $st.realm_totals
if ($rt) {
    Write-Output ("分域: cn [total={0} healthy={1} cooling={2} disabled={3}]   global [total={4} healthy={5} cooling={6} disabled={7}]" -f $rt.cn.total, $rt.cn.healthy, $rt.cn.cooling, $rt.cn.disabled, $rt.global.total, $rt.global.healthy, $rt.global.cooling, $rt.global.disabled)
}

Write-Output ""
Write-Output ("{0,-14} {1,-9} {2,-6} {3,9}  {4}" -f "昵称", "UID", "域", "积分", "状态")
foreach ($a in $st.accounts) {
    $nick = if ($a.nickname) { [string]$a.nickname } else { "(无昵称)" }
    if ($nick.Length -gt 14) { $nick = $nick.Substring(0, 14) }
    $uid = [string]$a.uid
    if ($uid.Length -gt 9) { $uid = $uid.Substring(0, 9) }
    if ($a.disabled) {
        $stat = "禁用 " + $(if ($a.disabled_reason) { $a.disabled_reason } else { "" })
    } elseif ($a.cooling) {
        $extra = ""
        if ($a.cool_remaining_sec -gt 0) {
            $mins = [math]::Round($a.cool_remaining_sec / 60)
            if ($mins -ge 60) { $extra = " (剩$([math]::Floor($mins / 60))小时$($mins % 60)分)" }
            else { $extra = " (剩$mins分钟)" }
        }
        $stat = "冷却 " + $a.reason + $extra
    } else {
        $stat = "可用"
        if ($a.in_flight -gt 0) { $stat = "可用 in_flight=$($a.in_flight)" }
    }
    if ($a.rate_limited_models) {
        $models = ($a.rate_limited_models | ForEach-Object { $_.model }) -join ","
        $stat = "$stat | 限流:$models"
    }
    Write-Output ("{0,-14} {1,-9} {2,-6} {3,9}  {4}" -f $nick, $uid, $a.realm, $a.credits, $stat)
}
Write-Output ""
Write-Output "[说明] cooling=冷却中 disabled=禁用 限流=模型级6004台账 sticky=粘性会话 redis=状态镜像模式"
