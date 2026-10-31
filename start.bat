@echo off
rem The console must be UTF-8, otherwise the Chinese in this file will be randomly encoded under the
rem GBK terminal.
chcp 65001 >nul 2>&1
rem ARTEX Guardian Startup Script (Windows)
rem
rem Usage:
rem   start.bat Run in the foreground (Ctrl-C stops) start.bat -addr :9000 Additional parameters are
rem   passed to artex as they are
rem
rem It does only one thing: it runs artex.exe, and after the process exits, it uses the exit code to
rem decide whether to restart.
rem
rem   0 User stops normally -> exit loop 75 Program request restart -> restart immediately (page
rem   clicked "refresh one-click" or "roll back") Other Crash -> restart after avoidance (1->2->4...
rem   up to 60 seconds)
rem
rem Downloading, SHA256 validation, and reinstalling are not done here, all done by artex itself at
rem startup (selfupdate package).

setlocal enabledelayedexpansion
cd /d "%~dp0"

set "BIN=artex.exe"
if not exist "%BIN%" (
	echo [artex] 找不到可执行文件 %BIN% 1>&2
	exit /b 1
)

set "RESTART_CODE=75"
set "MAX_DELAY=60"
set /a delay=1

:loop
"%BIN%" %*
set "code=!ERRORLEVEL!"

if "!code!"=="0" (
	echo [artex] 正常退出
	exit /b 0
)

if "!code!"=="%RESTART_CODE%" (
	rem Update/rollback ready: After rebooting, artex will complete the reload at startup.
	echo [artex] 请求重启（应用新版本）…
	set /a delay=1
	goto loop
)

echo [artex] 异常退出 ^(code=!code!^)，!delay!s 后重启 1>&2
rem timeout fails in the redirected console, and ping is used to resolve it (N+1 times for N seconds
rem of delay).
set /a pings=!delay!+1
ping -n !pings! 127.0.0.1 >nul 2>&1
set /a delay=!delay!*2
if !delay! gtr %MAX_DELAY% set /a delay=%MAX_DELAY%
goto loop
