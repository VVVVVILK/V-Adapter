@echo off
chcp 65001 >nul
echo === 构建 v-adapter (Windows amd64) ===
cd /d "%~dp0"
set GOOS=windows
set GOARCH=amd64
go build -o v-adapter.exe .
if errorlevel 1 (
  echo 构建失败
  pause
  exit /b 1
)
echo 构建完成: %~dp0v-adapter.exe
pause
