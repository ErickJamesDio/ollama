@echo off
setlocal

set "ROOT=%~dp0"
set "RUNTIME=%ROOT%dist\windows-amd64\ollama.exe"
set "LLAMA_SERVER=%ROOT%dist\windows-amd64\lib\ollama\llama-server.exe"
set "CGO_ENABLED=1"

where gcc.exe >nul 2>nul
if not errorlevel 1 goto gcc_ready

if exist "C:\msys64\ucrt64\bin\gcc.exe" (
    set "PATH=C:\msys64\ucrt64\bin;%PATH%"
) else (
    echo ERROR: gcc.exe was not found in PATH or C:\msys64\ucrt64\bin.
    echo Install a Windows GCC toolchain before starting Desktop DEV.
    pause
    exit /b 1
)

:gcc_ready

if not exist "%RUNTIME%" goto runtime_missing
if not exist "%LLAMA_SERVER%" goto runtime_missing
goto start_dev

:runtime_missing
echo ERROR: the prebuilt DEV runtime is missing:
echo   %RUNTIME%
echo   %LLAMA_SERVER%
echo.
echo Run scripts\prepare_dev_windows.ps1 explicitly or copy a compatible
echo dist\windows-amd64 folder into this checkout. This launcher never builds.
pause
exit /b 1

:start_dev
echo Starting Vite...
start "Ollama Vite DEV" cmd /k "cd /d ""%ROOT%app\ui\app"" && npm run dev"

echo Waiting for Vite on port 5173...
powershell -NoProfile -Command "$deadline=(Get-Date).AddSeconds(60); do { try { $response=Invoke-WebRequest -UseBasicParsing -Uri 'http://localhost:5173/' -TimeoutSec 2; if ($response.StatusCode -ge 200) { exit 0 } } catch {}; Start-Sleep -Milliseconds 500 } while ((Get-Date) -lt $deadline); exit 1"
if errorlevel 1 (
    echo.
    echo Vite did not become ready on port 5173 within 60 seconds.
    pause
    exit /b 1
)

echo Starting Ollama Desktop DEV...
start "Ollama Desktop DEV" cmd /k "cd /d ""%ROOT%"" && go run .\app\cmd\app -dev"

echo.
echo Development windows started. The Desktop process manages Ollama Server itself.
exit /b 0
