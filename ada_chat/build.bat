@echo off
rem 构建 Ada Chat：生成 bin\ada_chat.exe。
rem 发布参数与 MyGo 官方一致：-trimpath、去掉 inspector、生产标记、Windows GUI 子系统。
rem 用法：build.bat        构建并在双击运行时暂停
rem       build.bat -q     构建后直接返回（终端、CI 用）
setlocal
chcp 65001 >nul
cd /d "%~dp0"

set "OUT=bin\ada_chat.exe"
set "PKG=./cmd/ada_chat"
set "CODE=0"

where go >nul 2>nul
if errorlevel 1 goto :nogo

for /f "tokens=3" %%v in ('go version') do echo 使用 Go %%v

if not exist bin mkdir bin

echo 构建 %OUT% ...
set CGO_ENABLED=0
go build -trimpath -tags mygo_noinspector ^
  -ldflags "-s -w -H=windowsgui -X github.com/egoist/mygo.production=1" ^
  -o "%OUT%" "%PKG%"
if errorlevel 1 goto :fail

for %%f in ("%OUT%") do set "SIZE=%%~zf"
echo 完成：%CD%\%OUT%（%SIZE% 字节）
goto :done

:nogo
echo [错误] 没有找到 go 命令，请安装 Go 1.27 或更新版本：https://go.dev/dl/
set "CODE=1"
goto :done

:fail
echo [错误] 构建失败，上面的输出里能看到原因。
set "CODE=1"
goto :done

:done
if /i "%~1"=="-q" exit /b %CODE%
rem 双击运行时停一下，免得窗口一闪而过；从命令行调用时不打断。
echo %CMDCMDLINE% | find /i "/c" >nul
if not errorlevel 1 pause
exit /b %CODE%
