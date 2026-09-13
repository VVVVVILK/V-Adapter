@echo off
chcp 65001 >nul
echo === 交叉编译 v-adapter (Linux amd64) ===
cd /d "%~dp0"
set GOOS=linux
set GOARCH=amd64
set CGO_ENABLED=0
go build -trimpath -ldflags "-s -w" -o v-adapter .
if errorlevel 1 (
  echo 编译失败
  pause
  exit /b 1
)
echo 编译完成: %~dp0v-adapter  ^(上传到服务器 chmod +x 后运行^)
pause
