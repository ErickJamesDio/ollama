@echo off
setlocal

set "ROOT=%~dp0"
set "RUNTIME=%ROOT%dist\windows-amd64\ollama.exe"
set "LLAMA_SERVER=%ROOT%dist\windows-amd64\lib\ollama\llama-server.exe"

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

echo Starting Ollama Desktop DEV...
start "Ollama Desktop DEV" cmd /k "cd /d ""%ROOT%"" && go run .\app\cmd\app -dev"

echo.
echo Development windows started. The Desktop process manages Ollama Server itself.
exit /b 0
