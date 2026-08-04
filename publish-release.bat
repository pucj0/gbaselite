@echo off
setlocal EnableExtensions
chcp 65001 >nul
cd /d "%~dp0"

echo GBaseLite isolated release publisher
echo GitHub:    https://github.com/pucj0/gbaselite
echo DockerHub: https://hub.docker.com/r/pucj/gbaselite
echo.

powershell.exe -NoProfile -ExecutionPolicy Bypass -File "%~dp0scripts\publish-release.ps1" %*
set "EXIT_CODE=%ERRORLEVEL%"

echo.
if "%EXIT_CODE%"=="0" (
  echo Publish command completed successfully.
) else (
  echo Publish command failed with exit code %EXIT_CODE%.
)
exit /b %EXIT_CODE%
