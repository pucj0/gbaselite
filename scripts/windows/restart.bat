@echo off
setlocal EnableExtensions
title GBaseLite - Restart

call :detect_pause %*
call :resolve_layout
if errorlevel 1 (
  set "EXIT_CODE=2"
  goto :finish
)

if not exist "%CONFIG_FILE%" (
  if not exist "%EXAMPLE_CONFIG%" (
    echo ERROR: config.example.yaml was not found in "%APP_DIR%".
    set "EXIT_CODE=2"
    goto :finish
  )
  copy /y "%EXAMPLE_CONFIG%" "%CONFIG_FILE%" >nul
  if errorlevel 1 (
    echo ERROR: Unable to create "%CONFIG_FILE%".
    set "EXIT_CODE=2"
    goto :finish
  )
  echo Created "%CONFIG_FILE%".
  echo Set a strong auth.password, then run restart.bat again.
  set "EXIT_CODE=2"
  goto :finish
)

pushd "%APP_DIR%" >nul
"%GBASELITE_EXE%" restart --config "%CONFIG_FILE%"
set "EXIT_CODE=%ERRORLEVEL%"
popd >nul
if not "%EXIT_CODE%"=="0" echo ERROR: GBaseLite failed to restart. Review the message above and the logs directory.
goto :finish

:resolve_layout
set "SCRIPT_DIR=%~dp0"
if exist "%SCRIPT_DIR%gbaselite.exe" (
  set "APP_DIR=%SCRIPT_DIR%"
  set "GBASELITE_EXE=%SCRIPT_DIR%gbaselite.exe"
  set "CONFIG_FILE=%SCRIPT_DIR%config.yaml"
  set "EXAMPLE_CONFIG=%SCRIPT_DIR%config.example.yaml"
  exit /b 0
)
for %%I in ("%SCRIPT_DIR%..\..") do set "APP_DIR=%%~fI"
set "GBASELITE_EXE=%APP_DIR%\bin\gbaselite.exe"
set "CONFIG_FILE=%APP_DIR%\config.yaml"
set "EXAMPLE_CONFIG=%APP_DIR%\config.example.yaml"
if exist "%GBASELITE_EXE%" exit /b 0
echo ERROR: gbaselite.exe was not found.
echo Checked "%SCRIPT_DIR%gbaselite.exe" and "%GBASELITE_EXE%".
echo Build it first with: go build -trimpath -o .\bin\gbaselite.exe .\cmd\gbaselite
exit /b 1

:detect_pause
set "PAUSE_ON_EXIT=0"
if /i "%~1"=="--pause" set "PAUSE_ON_EXIT=1"
if /i "%~1"=="--no-pause" exit /b 0
if "%PAUSE_ON_EXIT%"=="1" exit /b 0
set "ORIGINAL_CMDLINE=%CMDCMDLINE%"
setlocal EnableDelayedExpansion
echo(!ORIGINAL_CMDLINE!| "%SystemRoot%\System32\findstr.exe" /i /c:"%~nx0" >nul
if not errorlevel 1 (
  endlocal
  set "PAUSE_ON_EXIT=1"
  exit /b 0
)
endlocal
exit /b 0

:finish
if not defined EXIT_CODE set "EXIT_CODE=0"
if "%PAUSE_ON_EXIT%"=="1" (
  echo.
  pause
)
exit /b %EXIT_CODE%
