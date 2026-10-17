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

    Write-Host "Build completed. Outputs are under: $repoRoot\dist"
} finally {
    Pop-Location
}
