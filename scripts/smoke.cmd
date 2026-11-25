@echo off
REM 冒烟脚本的 cmd 包装：绕过 PowerShell 执行策略，双击/命令行都能跑。
REM 参数原样透传，例如：
REM   scripts\smoke.cmd -Plan plans\smoke.yaml -TimeoutSec 180
powershell -NoProfile -ExecutionPolicy Bypass -File "%~dp0smoke.ps1" %*
exit /b %ERRORLEVEL%
