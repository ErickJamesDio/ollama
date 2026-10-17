@echo off
setlocal

set "ROOT=%~dp0"
set "RUNTIME=%ROOT%dist\windows-amd64\ollama.exe"
set "LLAMA_SERVER=%ROOT%dist\windows-amd64\lib\ollama\llama-server.exe"
set "CGO_ENABLED=1"

if exist "C:\msys64\ucrt64\bin\gcc.exe" (
    set "PATH=C:\msys64\ucrt64\bin;%PATH%"
) else (
    echo ERROR: C:\msys64\ucrt64\bin\gcc.exe was not found.
    echo Install the MSYS2 UCRT64 toolchain before starting Desktop DEV.
    pause
    exit /b 1
)

if not exist "%RUNTIME%" goto build_runtime
if not exist "%LLAMA_SERVER%" goto build_runtime
goto start_dev

:build_runtime
echo Preparing the native Ollama runtime. This first build can take several minutes...
powershell -NoProfile -ExecutionPolicy Bypass -File "%ROOT%scripts\prepare_dev_windows.ps1"
if errorlevel 1 (
    echo.
    echo Native runtime build failed. Review the errors above.
    pause
    exit /b 1
)

:start_dev
echo Starting Vite...
start "Ollama Vite DEV" cmd /k "cd /d ""%ROOT%app\ui\app"" && npm run dev"

echo Waiting for Vite on port 5173...
powershell -NoProfile -Command "$deadline=(Get-Date).AddSeconds(60); do { try { $response=Invoke-WebRequest -UseBasicParsing -Uri 'http://127.0.0.1:5173/' -TimeoutSec 2; if ($response.StatusCode -ge 200) { exit 0 } } catch {}; Start-Sleep -Milliseconds 500 } while ((Get-Date) -lt $deadline); exit 1"
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
