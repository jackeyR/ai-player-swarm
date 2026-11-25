@echo off
REM 演示脚本的 cmd 包装：绕过 PowerShell 执行策略。
REM   scripts\demo.cmd            1v1，保证能立刻排上
REM   scripts\demo.cmd -Five      5v5
REM   scripts\demo.cmd -Account me2
powershell -NoProfile -ExecutionPolicy Bypass -File "%~dp0demo.ps1" %*
exit /b %ERRORLEVEL%
