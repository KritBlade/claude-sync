@echo off
rem Fake antigravity-sync for the ccsync harness: logs its arguments, and exits with the code in
rem %FAKE_DIR%\antigravity-exit-code when that file exists.
echo antigravity-sync %*>> "%FAKE_DIR%\calls.log"
if not exist "%FAKE_DIR%\antigravity-exit-code" exit /b 0
set /p ANTIGRAVITY_EXIT_CODE=<"%FAKE_DIR%\antigravity-exit-code"
exit /b %ANTIGRAVITY_EXIT_CODE%
