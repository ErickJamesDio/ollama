param(
    [string]$ToolkitRoot = 'D:\OllamaBuild\cuda-legacy\toolkit',
    [string]$DownloadRoot = 'D:\OllamaBuild\cuda-legacy\downloads'
)

# NVIDIA's redistributable archives avoid installing a driver or unrelated SDKs.
# All payload hashes below come from redistrib_12.6.3.json over verified HTTPS.
$ErrorActionPreference = 'Stop'
if (-not $IsWindows -or [Runtime.InteropServices.RuntimeInformation]::OSArchitecture -ne 'X64') {
    throw 'This installer requires Windows x64 and PowerShell 7.'
}
foreach ($path in @($ToolkitRoot, $DownloadRoot)) {
    if (-not ([IO.Path]::GetFullPath($path)).StartsWith('D:\', [StringComparison]::OrdinalIgnoreCase)) {
        throw 'CUDA CI installation and downloads must remain on D:.'
    }
    New-Item -ItemType Directory -Path $path -Force | Out-Null
}
$baseUrl = 'https://developer.download.nvidia.com/compute/cuda/redist'
$components = @(
    @{ Name = 'cuda_nvcc'; Version = '12.6.85'; Sha256 = '3fb9f76b87c37d02f947354be89b718ad5f2c76b6ab47995265bfa3a068a5e14' },
    @{ Name = 'cuda_cudart'; Version = '12.6.77'; Sha256 = '7a313bc0c93b1a50bb03aa9783a199ae70c3b66e2d8084da65e8254a8577b925' },
    @{ Name = 'cuda_cccl'; Version = '12.6.77'; Sha256 = '469b783b6731964170b3d27ae214e97e2a63ec93c57ace3ed4052e6d01261f2e' },
    @{ Name = 'cuda_cuobjdump'; Version = '12.6.77'; Sha256 = 'b9207c09a5d6b34e6bdd8ff2093b3d101ef85f08b34c8ba8d710050b522661ae' },
    @{ Name = 'libcublas'; Version = '12.6.4.1'; Sha256 = '1a87ec80f8c0e5a39badc87010d479930c5b63abd788b3a05bd688a5980a3d07' }
)
foreach ($component in $components) {
    $stem = "$($component.Name)-windows-x86_64-$($component.Version)-archive"
    $archive = Join-Path $DownloadRoot "$stem.zip"
    $url = "$baseUrl/$($component.Name)/windows-x86_64/$stem.zip"
    if (-not (Test-Path -LiteralPath $archive)) {
        & curl.exe --fail --location --retry 3 --output $archive $url
        if ($LASTEXITCODE -ne 0) { throw "Download failed: $url" }
    }
    if ((Get-FileHash -LiteralPath $archive -Algorithm SHA256).Hash -ne $component.Sha256) {
        throw "NVIDIA archive checksum mismatch: $stem. Refusing to extract."
    }
    $expanded = Join-Path $DownloadRoot $stem
    if (Test-Path -LiteralPath $expanded) { Remove-Item -LiteralPath $expanded -Recurse -Force }
    Expand-Archive -LiteralPath $archive -DestinationPath $expanded
    Copy-Item -Path (Join-Path $expanded "$stem\*") -Destination $ToolkitRoot -Recurse -Force
    if ($component.Name -in @('cuda_cudart', 'libcublas')) {
        $licenseRoot = Join-Path $ToolkitRoot "redistribution-licenses\$($component.Name)"
        New-Item -ItemType Directory -Path $licenseRoot -Force | Out-Null
        Get-ChildItem -LiteralPath (Join-Path $expanded $stem) -File |
            Where-Object Name -Match 'license|eula|copyright' |
            Copy-Item -Destination $licenseRoot
    }
    Remove-Item -LiteralPath $expanded -Recurse -Force
    Remove-Item -LiteralPath $archive
}
foreach ($file in @('bin\nvcc.exe', 'bin\cuobjdump.exe', 'include\cuda_runtime.h', 'include\crt\host_config.h', 'lib\x64\cuda.lib', 'lib\x64\cudart.lib', 'lib\x64\cublas.lib')) {
    if (-not (Test-Path -LiteralPath (Join-Path $ToolkitRoot $file))) {
        throw "Incomplete CUDA toolkit: $file"
    }
}
@{ release = '12.6.3'; components = $components } | ConvertTo-Json -Depth 5 |
    Set-Content -LiteralPath (Join-Path $ToolkitRoot 'CUDA-REDIST-SHA256.json') -Encoding UTF8
& (Join-Path $ToolkitRoot 'bin\nvcc.exe') --version
if ($LASTEXITCODE -ne 0) { throw 'The installed nvcc could not start.' }
Write-Host "Verified NVIDIA CUDA 12.6.3 components installed at $ToolkitRoot"
