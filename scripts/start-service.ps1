# start-service.ps1 - start wb2api service hidden (detached, no window)
$proj = Split-Path $PSScriptRoot -Parent
$exe = Join-Path $proj 'wb2api.exe'
$logs = Join-Path $proj 'logs'
if (-not (Test-Path $logs)) { New-Item -ItemType Directory -Path $logs | Out-Null }
# Go 标准 log 默认写 stderr，所以只重定向 stderr 到 server.log。
# （server.log 即服务日志；stdout 基本无输出，不需重定向。）
$out = Join-Path $logs 'server.log'
Start-Process -FilePath $exe -WindowStyle Hidden -RedirectStandardError $out
