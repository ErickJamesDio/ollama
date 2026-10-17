param(
    [string]$CudaArchitectures = "61;75;89",
    [int]$Jobs = 8,
    [switch]$SkipTests
)

$ErrorActionPreference = "Stop"
$repoRoot = Split-Path -Parent $PSScriptRoot
$fullBuild = Join-Path $PSScriptRoot "build_windows.ps1"

Push-Location $repoRoot
try {
    if (-not $SkipTests) {
        Write-Host "Running Go tests..."
        & go test ./app/ui ./app/tools ./app/store
        if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
    }

    $env:OLLAMA_BUILD_PARALLEL = "$Jobs"
    $env:OLLAMA_CUDA_ARCHITECTURES = $CudaArchitectures

    Write-Host "Building Windows CPU, CUDA 12, CLI and Desktop app"
    Write-Host "CUDA architectures: $CudaArchitectures (61=Pascal, 75=Turing, 89=Ada Lovelace)"
    & $fullBuild cpu cuda12 ollama app
    if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }

    $distRoot = Join-Path $repoRoot "dist"
    $runtimeRoot = Join-Path $distRoot "windows-amd64"
    $appExe = Join-Path $distRoot "windows-ollama-app-amd64.exe"
    $portableRoot = Join-Path $distRoot "ollama-portable-windows-amd64"
    $portableZip = Join-Path $distRoot "ollama-portable-windows-amd64.zip"

    if (-not (Test-Path -LiteralPath $appExe)) {
        throw "Desktop app executable was not produced: $appExe"
    }
    if (-not (Test-Path -LiteralPath (Join-Path $runtimeRoot "ollama.exe"))) {
        throw "Ollama runtime was not produced: $runtimeRoot"
    }
    if (-not (Test-Path -LiteralPath (Join-Path $runtimeRoot "lib\ollama\cuda_v12"))) {
        throw "CUDA 12 runtime was not produced under: $runtimeRoot\lib\ollama"
    }

    Remove-Item -LiteralPath $portableRoot -Recurse -Force -ErrorAction SilentlyContinue
    Remove-Item -LiteralPath $portableZip -Force -ErrorAction SilentlyContinue
    New-Item -ItemType Directory -Path $portableRoot | Out-Null

    Copy-Item -Path (Join-Path $runtimeRoot "*") -Destination $portableRoot -Recurse -Force
    Copy-Item -LiteralPath $appExe -Destination (Join-Path $portableRoot "ollama app.exe") -Force

    @"
Ollama Desktop portable test build

1. Extract the complete ZIP to a local folder.
2. Run "ollama app.exe" from that folder.
3. Keep ollama.exe and lib\ollama beside the app executable.

CUDA architectures included: $CudaArchitectures
Models and chat data remain in the Windows user profile and are not bundled.
"@ | Set-Content -LiteralPath (Join-Path $portableRoot "README-PORTABLE.txt") -Encoding UTF8

    Compress-Archive -Path (Join-Path $portableRoot "*") -DestinationPath $portableZip -CompressionLevel Optimal

    Write-Host "Portable folder: $portableRoot"
    Write-Host "Portable ZIP:    $portableZip"
} finally {
    Pop-Location
}
