@echo off
title workbuddy2api
cd /d "%~dp0"

rem ====== 预检：Go 是否安装 ======
where go >nul 2>nul
if errorlevel 1 (
    echo [错误] 未找到 go 命令，请先安装 Go 并加入 PATH。
    goto :pause_end
)

rem ====== 首次用到工具时自动编译 ======
if not exist "credit.exe" (
    echo 首次运行：编译 credit 工具...
    go build -o credit.exe ./cmd/credit
)
if not exist "login.exe" (
    echo 首次运行：编译 login 工具...
    go build -o login.exe ./cmd/login
)
if not exist "signin.exe" (
    echo 首次运行：编译 signin 工具...
    go build -o signin.exe ./cmd/signin
)
if not exist "trial.exe" (
    echo 首次运行：编译 trial 工具...
    go build -o trial.exe ./cmd/trial
)
if not exist "task.exe" (
    echo 首次运行：编译 task 工具...
    go build -o task.exe ./cmd/task
)

:menu
cls
echo.
echo  ============================================
echo     workbuddy2api 管理菜单
echo  ============================================
echo     [1] 查询积分 / 查看账号
echo     [2] 加入用户（国内版 cn）
echo     [3] 启动服务（后台）
echo     [4] 停止服务
echo     [5] 查看服务日志
echo     [6] 手动签到（批量全部账号）
echo     [7] 加入国际版用户（global）
echo     [8] 领取国际版加油包（trial）
echo     [9] 查看服务状态（/status 台账）
echo    [10] 手动执行定时任务（不影响自动排程）
echo     [q] 退出
echo  ============================================
echo.
set /p c=  请选择:
if /i "%c%"=="1" goto :query
if /i "%c%"=="2" goto :add
if /i "%c%"=="3" goto :start
if /i "%c%"=="4" goto :stop
if /i "%c%"=="5" goto :log
if /i "%c%"=="6" goto :signin
if /i "%c%"=="7" goto :add_global
if /i "%c%"=="8" goto :trial
if /i "%c%"=="9" goto :status
if /i "%c%"=="10" goto :task
if /i "%c%"=="q" goto :end
goto :menu

:task
cls
echo.
echo  ============================================
echo     手动执行定时任务
echo     （立即跑一次，不影响常驻服务的自动排程；
echo       幂等性由上游/防抖判定兜底，重复跑安全）
echo  ============================================
echo     [1] 令牌保活（按需 refresh 全部账号）
echo     [2] 每日签到
echo     [3] 猫猫旅行巡检
echo     [4] 活跃上报（N 连发 + 领猫联动）
echo     [5] 开学季任务（python）
echo     [6] 夜猫子任务（python）
echo     [7] 全部按顺序跑一遍
echo     [q] 返回主菜单
echo  ============================================
echo.
set /p t=  请选择:
if /i "%t%"=="1" (.\task.exe keepalive & goto :task_done)
if /i "%t%"=="2" (.\task.exe checkin & goto :task_done)
if /i "%t%"=="3" (.\task.exe travel & goto :task_done)
if /i "%t%"=="4" (.\task.exe activity & goto :task_done)
if /i "%t%"=="5" (.\task.exe school & goto :task_done)
if /i "%t%"=="6" (.\task.exe cat & goto :task_done)
if /i "%t%"=="7" (.\task.exe all & goto :task_done)
if /i "%t%"=="q" goto :menu
goto :task

:task_done
echo.
echo  [提示] school/cat 需要 python：解释器名不是 python3 时先 set WB2A_PYTHON=python
echo  [提示] 服务日志里看不到本次执行（这是独立进程），结果直接打在上方输出中。
echo.
pause
goto :task

:query
echo.
echo  ===== 积分查询 / 账号列表 =====
.\credit.exe -pretty
echo.
pause
goto :menu

:add
echo.
echo  ===== 加入用户（国内版 cn，WorkBuddy OAuth 登录） =====
echo  将自动打开浏览器完成授权，登录后自动落盘到 auths 目录。
echo.
.\login.exe join
call :join_result
goto :menu

:add_global
echo.
echo  ===== 加入国际版用户（global，workbuddy.ai） =====
echo  将自动打开浏览器完成授权；国际版账号无每日签到，登录后自动落盘。
echo.
.\login.exe join --realm=global
call :join_result
goto :menu

:join_result
if errorlevel 2 (
    echo.
    echo  已中止，未添加账号。
) else (
    echo.
    echo  加入完成。若服务运行中，需重启才能加载新账号（选 3 前先选 4 停止，再选 3 启动）。
)
echo.
pause
goto :eof

:signin
echo.
echo  ===== 手动签到（批量全部账号，国际版账号自动判为不适用） =====
echo  遍历 auths\ 下所有 workbuddy-*.json 账号签到。
echo.
.\signin.exe auths
if errorlevel 1 (
    echo.
    echo  [提示] 签到失败或 auths 目录无账号。请先选 2 加入用户。
)
echo.
pause
goto :menu

:trial
echo.
echo  ===== 领取国际版加油包（trial，仅 global 账号） =====
echo  遍历 auths\ 全部账号，CN 账号自动跳过（N/A）。
echo.
.\trial.exe auths
echo.
pause
goto :menu

:status
echo.
echo  ===== 服务状态 =====
powershell -NoProfile -ExecutionPolicy Bypass -File "%~dp0scripts\status-report.ps1"
echo.
echo  ===== 实时积分余额（credit.exe 直查上游） =====
.\credit.exe -pretty
echo.
pause
goto :menu

:start
echo.
echo  ===== 检查服务状态... =====
set PID=
netstat -ano | findstr ":7863" | findstr "LISTENING" >nul 2>nul
if errorlevel 1 goto :run
for /f "tokens=5" %%p in ('netstat -ano ^| findstr ":7863" ^| findstr "LISTENING"') do set PID=%%p

rem 端口被占用 → 健康检查（上游 /healthz 响应带 service:"workbuddy2api" 身份字段）
curl -s -m 3 http://127.0.0.1:7863/healthz | findstr /i "workbuddy2api" >nul 2>nul
if not errorlevel 1 goto :healthy

rem 端口占用但 healthz 不通 → 查进程归属
set PROCLINE=
for /f "delims=" %%n in ('tasklist /FI "PID eq %PID%" 2^>nul') do set "PROCLINE=%%n"
echo %PROCLINE% | findstr /i "wb2api.exe" >nul 2>nul
if not errorlevel 1 goto :stale

rem 其他程序占用
echo  [状态] 端口 7863 被其他程序占用（PID %PID%），非本项目。
echo  [处理] 不会强行结束别家程序；直接启动可能因端口冲突而失败。
echo.
set /p r=  仍然启动? (y/n):
if /i not "%r%"=="y" goto :menu
goto :run

:healthy
echo  [状态] 服务已在运行且健康（PID %PID%，/healthz 正常）。
echo  [处理] y=强制重启（先停掉旧进程再启动）  其它=返回菜单
echo.
set /p r=  请选择 (y/n):
if /i not "%r%"=="y" goto :menu
echo 正在停止旧服务...
taskkill /F /IM wb2api.exe >nul 2>nul
timeout /t 1 /nobreak >nul 2>nul
goto :run

:stale
echo  [状态] 端口 7863 被占用，但 /healthz 无响应（疑似残留僵死进程，PID %PID%）。
echo  [处理] y=强制清理并重启  其它=返回菜单
echo.
set /p r=  请选择 (y/n):
if /i not "%r%"=="y" goto :menu
echo 正在清理残留进程...
taskkill /F /PID %PID% >nul 2>nul
taskkill /F /IM wb2api.exe >nul 2>nul
timeout /t 1 /nobreak >nul 2>nul
goto :run

:run
rem 编译服务（首次或源码更新后）
if not exist wb2api.exe (
    echo 首次运行：编译服务...
    go build -o wb2api.exe ./cmd/server
)
if not exist logs mkdir logs
set "LOG=%~dp0logs\server.log"
set "EXE=%~dp0wb2api.exe"
echo.
echo  ===== 后台启动服务 =====
echo  日志文件 : logs\server.log
echo.
powershell -NoProfile -ExecutionPolicy Bypass -File "%~dp0scripts\start-service.ps1"
echo  服务已后台启动，正在等待端口 7863 就绪...
rem 最多等 10 秒，轮询 healthz（匹配 service 身份字段，注意服务无账号时 /healthz 返回 503 也算就绪）
set /a n=0
:waithealth
curl -s -m 2 http://127.0.0.1:7863/healthz | findstr /i "workbuddy2api" >nul 2>nul
if not errorlevel 1 goto :health_ok
set /a n+=1
if %n% geq 10 goto :health_timeout
timeout /t 1 /nobreak >nul 2>nul
goto :waithealth
:health_ok
echo  [成功] 服务已就绪，可正常使用。
goto :run_done
:health_timeout
echo  [警告] 端口未在 10 秒内就绪，请选 5 查看日志确认。
:run_done
echo.
echo  服务在后台运行，窗口可继续操作。
echo  查看日志选 5，停止服务选 4。
echo.
pause
goto :menu


:stop
echo.
echo  ===== 停止服务 =====
taskkill /F /IM wb2api.exe >nul 2>nul
if errorlevel 1 (
    echo  未检测到运行中的服务（wb2api.exe）。
) else (
    echo  服务已停止。
)
echo.
pause
goto :menu

:log
echo.
echo  ===== 服务日志 (最后 200 行，完整见 logs\server.log) =====
if not exist "%~dp0logs\server.log" (
    echo  尚无日志文件（服务可能未启动过）。
) else (
    rem type 在 GBK 代码页控制台会把 UTF-8 日志解成乱码；走 PowerShell 显式 UTF-8 读，
    rem 其输出经 Unicode 控制台 API 渲染，任何代码页下都正确。
    powershell -NoProfile -Command "Get-Content -LiteralPath '%~dp0logs\server.log' -Encoding UTF8 -Tail 200"
)
echo.
pause
goto :menu

:pause_end
pause
goto :end

:end
exit /b 0
