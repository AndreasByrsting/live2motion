@echo off
chcp 65001 >nul
title live2motion
powershell -NoProfile -ExecutionPolicy Bypass -File "%~dp0scripts\run-windows.ps1"
echo.
echo 服务已停止，按任意键关闭窗口。
pause >nul
