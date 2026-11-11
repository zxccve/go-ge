@echo off
rem Double-click me. Frees port 7001, starts the Go listener, writes a log.
rem The login now WORKS: PostgreSQL lookup -> BC_LOGINOK -> barrack built from
rem our own characters. What to look for is in the echo below.
setlocal
cd /d "%~dp0"

echo.
echo   Stopping whatever is listening on port 7001 (the Python barrack)...
for /f "tokens=5" %%p in ('netstat -ano ^| findstr ":7001 " ^| findstr "LISTENING"') do (
    taskkill /PID %%p /F >nul 2>&1
)
timeout /t 1 /nobreak >nul

if not exist "..\out\goserver.exe" (
    echo   ERROR: ..\out\goserver.exe is missing. Tell the agent to rebuild it.
    pause
    exit /b 1
)

echo   Starting the Go listener on port 7001.
echo.
echo   Leave this window open and log in with the game client.
echo   Expected result: the barrack appears with YOUR characters.
echo.
echo   TO ENTER THE ZONE you need the zone process as well, in a SECOND window:
echo     cd ..\src ^&^& python zone_server.py
echo   It must be listening on 7002, or the client is told to connect to nothing.
echo   The barrack hands it out\zone_team_^<id^>.json, so the team you picked is
echo   the team that spawns.
echo   To check the family level: the barrack should show a family level, and
echo   the log line "built cmd 10/16/17/26/37/41/57" proves those were built,
echo   not replayed. The presets panel should be EMPTY (this account has none).
echo   Log file: ..\logs\go_barrack_live.log
echo.
echo   Press Ctrl+C here when you are done.
echo.

"..\out\goserver.exe" -addr :7001 -log "..\logs\go_barrack_live.log"

echo.
echo   The listener stopped. You can close this window.
pause
