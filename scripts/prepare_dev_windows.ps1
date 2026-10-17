param(
    [string]$CudaArchitectures = "89",
    [int]$Jobs = 8
)

$ErrorActionPreference = "Stop"
$repoRoot = Split-Path -Parent $PSScriptRoot
$fullBuild = Join-Path $PSScriptRoot "build_windows.ps1"

Push-Location $repoRoot
try {
    $vs2022 = "C:\Program Files (x86)\Microsoft Visual Studio\2022\BuildTools"
    $vs2022DevShell = Join-Path $vs2022 "Common7\Tools\Microsoft.VisualStudio.DevShell.dll"
    if (Test-Path -LiteralPath $vs2022DevShell) {
        Import-Module $vs2022DevShell
        Enter-VsDevShell -VsInstallPath $vs2022 -SkipAutomaticLocation -DevCmdArguments "-arch=x64 -host_arch=x64 -no_logo"
        $env:VSINSTALLDIR = $vs2022
    }

    $env:OLLAMA_BUILD_PARALLEL = "$Jobs"
    $steps = @("cpu")

    $cuda12 = Get-ChildItem "C:\Program Files\NVIDIA GPU Computing Toolkit\CUDA\v12*\bin\nvcc.exe" -ErrorAction SilentlyContinue |
        Sort-Object FullName -Descending |
        Select-Object -First 1
    if ($cuda12) {
        $env:OLLAMA_CUDA_ARCHITECTURES = $CudaArchitectures
        $steps += "cuda12"
        Write-Host "CUDA 12 detected; building CUDA architecture $CudaArchitectures"
    } else {
        Write-Warning "CUDA 12 toolkit was not found; preparing a CPU runtime for DEV"
    }

    $steps += "ollama"
    & $fullBuild @steps
    if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }

    $runtime = Join-Path $repoRoot "dist\windows-amd64\ollama.exe"
    $llamaServer = Join-Path $repoRoot "dist\windows-amd64\lib\ollama\llama-server.exe"
    if (-not (Test-Path -LiteralPath $runtime) -or -not (Test-Path -LiteralPath $llamaServer)) {
        throw "DEV runtime was not produced under dist\windows-amd64"
    }

    Write-Host "DEV runtime ready: $runtime"
} finally {
    Pop-Location
}
