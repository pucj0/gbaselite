@echo off
setlocal EnableExtensions
chcp 65001 >nul
cd /d "%~dp0"

set "RELEASE_ARGS="
set "NO_PAUSE="

:parse_args
if "%~1"=="" goto run_release
if /I "%~1"=="--no-pause" set "NO_PAUSE=1"
if /I "%~1"=="--preview" set "RELEASE_ARGS=%RELEASE_ARGS% -Preview"
if /I "%~1"=="--self-test" set "RELEASE_ARGS=%RELEASE_ARGS% -SelfTest"
shift
goto parse_args

:run_release
echo GBaseLite one-click release
echo Project: %CD%
echo.

powershell.exe -NoProfile -ExecutionPolicy Bypass -File "%~dp0scripts\one-click-release.ps1" %RELEASE_ARGS%
set "EXIT_CODE=%ERRORLEVEL%"

echo.
if "%EXIT_CODE%"=="0" (
  echo Release completed successfully.
) else (
  echo Release failed with exit code %EXIT_CODE%.
)

if not defined NO_PAUSE pause
exit /b %EXIT_CODE%
