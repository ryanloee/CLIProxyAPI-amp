@echo off
setlocal enabledelayedexpansion

set "PATH=C:\Go\bin;%PATH%"

set "PROJECT_DIR=%~dp0"
set "BINARY_NAME=cli-proxy-api"
for /f %%a in ('powershell -command "Get-Date -Format yyyyMMddHHmmss"') do set "VERSION=dev-%%a"
if not "%VERSION_ENV%"=="" set "VERSION=%VERSION_ENV%"
set "OUTPUT_DIR=%PROJECT_DIR%dist"

echo =========================================
echo  CLIProxyAPI Build Script
echo  Version: %VERSION%
echo =========================================

echo [1/5] Cleaning...
if exist "%OUTPUT_DIR%" rmdir /s /q "%OUTPUT_DIR%"
mkdir "%OUTPUT_DIR%"

echo [2/5] Building windows/amd64...
set "CGO_ENABLED=0"
set "GOOS=windows"
set "GOARCH=amd64"
go build -ldflags "-s -w -X main.Version=%VERSION%" -o "%OUTPUT_DIR%\%BINARY_NAME%.exe" .\cmd\server\
if !errorlevel! neq 0 (
    echo   Build FAILED
    pause
    exit /b 1
)
echo   Build OK: %OUTPUT_DIR%\%BINARY_NAME%.exe

echo [3/5] Copying configs...
if exist "%PROJECT_DIR%config.example.yaml" copy /y "%PROJECT_DIR%config.example.yaml" "%OUTPUT_DIR%\" >nul
if not exist "%OUTPUT_DIR%\config.yaml" (
    echo port: 8317> "%OUTPUT_DIR%\config.yaml"
    echo debug: false>> "%OUTPUT_DIR%\config.yaml"
    echo auth-dir: "~/.cli-proxy-api">> "%OUTPUT_DIR%\config.yaml"
    echo api-keys:>> "%OUTPUT_DIR%\config.yaml"
    echo   - "your-api-key-here">> "%OUTPUT_DIR%\config.yaml"
    echo remote-management:>> "%OUTPUT_DIR%\config.yaml"
    echo   allow-remote: false>> "%OUTPUT_DIR%\config.yaml"
    echo   secret-key: "">> "%OUTPUT_DIR%\config.yaml"
    echo   disable-control-panel: false>> "%OUTPUT_DIR%\config.yaml"
)

echo [4/5] Copying docs...
if exist "%PROJECT_DIR%README.md" copy /y "%PROJECT_DIR%README.md" "%OUTPUT_DIR%\" >nul
if exist "%PROJECT_DIR%LICENSE" copy /y "%PROJECT_DIR%LICENSE" "%OUTPUT_DIR%\" >nul

echo [5/5] Creating start.bat...
powershell -Command "Set-Content -Path '%OUTPUT_DIR%\start.bat' -Value '@echo off', 'chcp 65001 >nul 2>&1', 'cli-proxy-api.exe -config config.yaml', 'pause'"

echo.
echo =========================================
echo  Build Complete!
echo =========================================
echo.
dir /b "%OUTPUT_DIR%\"
echo.
echo  Management panel: http://localhost:8317/management.html
echo.

if "%~1"=="--zip" goto :zip
if "%~1"=="-z" goto :zip
pause
exit /b 0

:zip
set "ZIP_NAME=cli-proxy-api-windows-amd64-%VERSION%.zip"
echo Creating %ZIP_NAME% ...
powershell -Command "Compress-Archive -Path '%OUTPUT_DIR%\*' -DestinationPath '%PROJECT_DIR%%ZIP_NAME%' -Force"
echo Done: %PROJECT_DIR%%ZIP_NAME%
pause
