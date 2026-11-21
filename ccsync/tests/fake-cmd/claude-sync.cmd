@echo off
rem Fake claude-sync for the ccsync harness: logs its arguments and prints canned output from %FAKE_DIR%.
echo %*>> "%FAKE_DIR%\calls.log"
if "%~1"=="pull" if exist "%FAKE_DIR%\pull.txt" type "%FAKE_DIR%\pull.txt"
if "%~1"=="diff" type "%FAKE_DIR%\diff.txt"
exit /b 0
