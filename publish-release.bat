@echo off
setlocal EnableExtensions
chcp 65001 >nul
cd /d "%~dp0"

set "PS_ARGS="
set "NO_PAUSE="

:parse_args
if "%~1"=="" goto run_publish
if /I "%~1"=="--no-pause" (
  set "NO_PAUSE=1"
  shift
  goto parse_args
)
if /I "%~1"=="-NoPause" (
  set "NO_PAUSE=1"
  shift
  goto parse_args
)
set "PS_ARGS=%PS_ARGS% %1"
shift
goto parse_args

:run_publish
echo GBaseLite isolated release publisher
echo GitHub:    https://github.com/pucj0/gbaselite
echo DockerHub: https://hub.docker.com/r/pucj/gbaselite
echo.

powershell.exe -NoProfile -ExecutionPolicy Bypass -File "%~dp0scripts\publish-release.ps1" %PS_ARGS%
set "EXIT_CODE=%ERRORLEVEL%"

echo.
if "%EXIT_CODE%"=="0" (
  echo Publish command completed successfully.
) else (
  echo Publish command failed with exit code %EXIT_CODE%.
)
if not defined NO_PAUSE pause
exit /b %EXIT_CODE%
