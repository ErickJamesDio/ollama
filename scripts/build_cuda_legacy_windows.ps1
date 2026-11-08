param(
    [string]$WorkRoot = "D:\OllamaBuild\cuda-legacy",
    [string]$ToolkitRoot = "",
    [ValidatePattern('^[0-9]+(;[0-9]+)*$')]
    [string]$CudaArchitectures = "61;75",
    [ValidateRange(1, 32)]
    [int]$Jobs = 4,
    [string]$ExpectedLlamaCommit = "",
    [switch]$CheckOnly,
    [switch]$KeepBuild
)

$ErrorActionPreference = "Stop"
$repoRoot = Split-Path -Parent $PSScriptRoot
$sourceRoot = Join-Path $repoRoot "llama\server"

function Resolve-FullPath {
    param([Parameter(Mandatory)][string]$Path)
    return [IO.Path]::GetFullPath($Path).TrimEnd('\')
}

function Assert-DriveD {
    param(
        [Parameter(Mandatory)][string]$Path,
        [Parameter(Mandatory)][string]$Label
    )

    $fullPath = Resolve-FullPath $Path
    if (-not $fullPath.StartsWith("D:\", [StringComparison]::OrdinalIgnoreCase)) {
        throw "$Label must be on D:. Received: $fullPath"
    }
    return $fullPath
}

function Find-VisualStudio {
    if ($env:VSINSTALLDIR -and (Test-Path -LiteralPath $env:VSINSTALLDIR)) {
        return (Resolve-FullPath $env:VSINSTALLDIR)
    }

    $programFilesX86 = [Environment]::GetEnvironmentVariable("ProgramFiles(x86)")
    if ($programFilesX86) {
        $vswhere = Join-Path $programFilesX86 "Microsoft Visual Studio\Installer\vswhere.exe"
        if (Test-Path -LiteralPath $vswhere) {
            $install = & $vswhere -latest -version '[17.0,18.0)' -products * `
                -requires Microsoft.VisualStudio.Component.VC.Tools.x86.x64 `
                -property installationPath 2>$null | Select-Object -First 1
            if ($install) {
                return (Resolve-FullPath $install)
            }
        }
    }

    return $null
}

function Enter-VisualStudioShell {
    param([Parameter(Mandatory)][string]$InstallPath)

    $devShell = Join-Path $InstallPath "Common7\Tools\Microsoft.VisualStudio.DevShell.dll"
    if (-not (Test-Path -LiteralPath $devShell)) {
        throw "Visual Studio Developer Shell not found: $devShell"
    }

    Import-Module $devShell
    Enter-VsDevShell -VsInstallPath $InstallPath -SkipAutomaticLocation `
        -DevCmdArguments "-arch=x64 -host_arch=x64 -no_logo"

    if (-not (Get-Command cl.exe -ErrorAction SilentlyContinue)) {
        throw "MSVC cl.exe was not available after entering the Visual Studio shell."
    }
}

function Find-Ninja {
    $ninja = Get-Command ninja.exe -ErrorAction SilentlyContinue | Select-Object -First 1
    if ($ninja) {
        return $ninja.Source
    }

    $candidates = @((Join-Path $env:ProgramFiles "CMake\bin\ninja.exe"))
    if ($script:vsInstall) {
        $candidates += Join-Path $script:vsInstall "Common7\IDE\CommonExtensions\Microsoft\CMake\Ninja\ninja.exe"
    }

    foreach ($candidate in $candidates) {
        if ($candidate -and (Test-Path -LiteralPath $candidate)) {
            return (Resolve-FullPath $candidate)
        }
    }

    return $null
}

function Find-Cuda12Toolkit {
    param([string]$ExplicitRoot)

    $candidates = @()
    if ($ExplicitRoot) {
        $candidates += $ExplicitRoot
    }

    $cudaVariables = Get-ChildItem Env: | Where-Object { $_.Name -match '^CUDA_PATH_V12_' } |
        Sort-Object Name -Descending
    foreach ($variable in $cudaVariables) {
        $candidates += $variable.Value
    }

    if ($env:CUDA_PATH) {
        $candidates += $env:CUDA_PATH
    }

    foreach ($minor in 9..0) {
        $candidates += "D:\NVIDIA\CUDA\v12.$minor"
        $candidates += "D:\Program Files\NVIDIA GPU Computing Toolkit\CUDA\v12.$minor"
        $candidates += "C:\Program Files\NVIDIA GPU Computing Toolkit\CUDA\v12.$minor"
    }

    foreach ($candidate in ($candidates | Where-Object { $_ } | Select-Object -Unique)) {
        $nvcc = Join-Path $candidate "bin\nvcc.exe"
        if (-not (Test-Path -LiteralPath $nvcc)) {
            continue
        }

        $versionText = (& $nvcc --version 2>&1 | Out-String)
        if ($LASTEXITCODE -ne 0 -or $versionText -notmatch 'release\s+12\.') {
            continue
        }

        $supportedArchitectures = (& $nvcc --list-gpu-arch 2>&1 | Out-String)
        foreach ($required in @("compute_61", "compute_75")) {
            if ($supportedArchitectures -notmatch [regex]::Escape($required)) {
                throw "CUDA toolkit at $candidate does not advertise required architecture $required."
            }
        }

        return [pscustomobject]@{
            Root = Resolve-FullPath $candidate
            Nvcc = Resolve-FullPath $nvcc
            Cuobjdump = Resolve-FullPath (Join-Path $candidate "bin\cuobjdump.exe")
            VersionText = $versionText.Trim()
        }
    }

    return $null
}

function Get-NvidiaState {
    $nvidiaSmi = Get-Command nvidia-smi.exe -ErrorAction SilentlyContinue | Select-Object -First 1
    if (-not $nvidiaSmi) {
        return @()
    }

    $rows = & $nvidiaSmi.Source --query-gpu=name,driver_version,compute_cap --format=csv,noheader 2>$null
    if ($LASTEXITCODE -ne 0) {
        return @()
    }

    return @($rows)
}

if (-not $IsWindows -or [Runtime.InteropServices.RuntimeInformation]::OSArchitecture -ne 'X64') {
    throw 'This build requires Windows x64 and PowerShell 7.'
}
if ($CudaArchitectures -ne "61;75") {
    throw "The portable CUDA Legacy contract requires exactly sm_61 + sm_75. Requested: $CudaArchitectures"
}
if (-not (Test-Path -LiteralPath $sourceRoot)) {
    throw "llama/server source was not found: $sourceRoot"
}
if (-not (Test-Path -LiteralPath (Join-Path $repoRoot "LLAMA_CPP_VERSION"))) {
    throw "LLAMA_CPP_VERSION was not found. Use the complete Ollama source tree."
}

$WorkRoot = Assert-DriveD $WorkRoot "WorkRoot"
$buildRoot = Join-Path $WorkRoot "build"
$stageRoot = Join-Path $WorkRoot "stage"
$packageRoot = Join-Path $WorkRoot "package"
$packageRuntime = Join-Path $packageRoot "runtimes\cuda_legacy"
$tempRoot = Join-Path $WorkRoot "temp"
$zipPath = Join-Path $WorkRoot "ollama-portable-cuda-legacy-sm61-sm75.zip"

$cmake = Get-Command cmake.exe -ErrorAction SilentlyContinue | Select-Object -First 1
if (-not $cmake) {
    throw "cmake.exe was not found in PATH."
}

$script:vsInstall = Find-VisualStudio
if (-not $script:vsInstall) {
    throw "Visual Studio 2022 with Desktop development with C++ was not found."
}
Enter-VisualStudioShell $script:vsInstall
foreach ($tool in @('git.exe', 'dumpbin.exe')) {
    if (-not (Get-Command $tool -ErrorAction SilentlyContinue)) { throw "Required tool not found: $tool" }
}
$compilerVersion = [Diagnostics.FileVersionInfo]::GetVersionInfo((Get-Command cl.exe).Source).FileVersion
Write-Host "MSVC compiler:       $compilerVersion"

$ninja = Find-Ninja
if (-not $ninja) {
    throw "ninja.exe was not found. Install Ninja or the Visual Studio CMake tools."
}
$env:Path = "$(Split-Path -Parent $ninja);$env:Path"

$cuda = Find-Cuda12Toolkit $ToolkitRoot
if (-not $cuda) {
    throw "CUDA Toolkit 12.x with compute_61 and compute_75 support was not found. Install it first or pass -ToolkitRoot. Do not use CUDA 13 for Pascal."
}
if (-not (Test-Path -LiteralPath $cuda.Cuobjdump)) {
    throw "cuobjdump.exe was not found in the selected CUDA toolkit: $($cuda.Cuobjdump)"
}

$gpuRows = Get-NvidiaState
$llamaVersion = (Get-Content -LiteralPath (Join-Path $repoRoot "LLAMA_CPP_VERSION") -Raw).Trim()

Write-Host "Repository:          $repoRoot"
Write-Host "llama.cpp version:   $llamaVersion"
Write-Host "Work root:           $WorkRoot"
Write-Host "CUDA toolkit:        $($cuda.Root)"
Write-Host "CUDA architectures:  $CudaArchitectures"
Write-Host "Visual Studio:       $script:vsInstall"
Write-Host "CMake:               $($cmake.Source)"
Write-Host "Ninja:               $ninja"
foreach ($row in $gpuRows) {
    Write-Host "GPU:                 $row"
}
if (-not $gpuRows) {
    Write-Warning "No NVIDIA GPU state was available. Cross-compilation may proceed, but physical inference validation remains pending."
}

if ($CheckOnly) {
    Write-Host "Prerequisite check passed. No build was started."
    exit 0
}

New-Item -ItemType Directory -Path $WorkRoot, $tempRoot -Force | Out-Null
$oldTemp = $env:TEMP
$oldTmp = $env:TMP
$oldCudaPath = $env:CUDA_PATH
$oldCudaCxx = $env:CUDACXX
$oldCl = $env:_CL_
$oldCcache = $env:GGML_CCACHE

try {
    $env:TEMP = $tempRoot
    $env:TMP = $tempRoot
    $env:CUDA_PATH = $cuda.Root
    $env:CUDACXX = $cuda.Nvcc
    $env:_CL_ = "/Z7"
    $env:GGML_CCACHE = "OFF"

    $cleanPaths = @($stageRoot, $packageRoot, $zipPath, "$zipPath.sha256")
    if (-not $KeepBuild) { $cleanPaths += $buildRoot }
    foreach ($path in $cleanPaths) {
        if (Test-Path -LiteralPath $path) {
            Remove-Item -LiteralPath $path -Recurse -Force
        }
    }

    New-Item -ItemType Directory -Path $buildRoot, $stageRoot, $packageRoot -Force | Out-Null

    # Fetch only the pinned source, rather than every upstream tag. CMake applies
    # this checkout's compatibility patches to this generated dependency tree.
    $llamaSource = Join-Path $buildRoot 'llama-source'
    if (-not (Test-Path -LiteralPath (Join-Path $llamaSource '.git'))) {
        & git.exe clone --depth 1 --single-branch --branch $llamaVersion https://github.com/ggml-org/llama.cpp.git $llamaSource
        if ($LASTEXITCODE -ne 0) { throw 'Could not fetch the pinned llama.cpp source.' }
    }
    $llamaCommit = (& git.exe -C $llamaSource rev-parse HEAD).Trim()
    if ($LASTEXITCODE -ne 0) { throw 'Could not resolve the llama.cpp commit.' }
    if ($ExpectedLlamaCommit -and $llamaCommit -ne $ExpectedLlamaCommit) {
        throw "llama.cpp identity mismatch: expected $ExpectedLlamaCommit, found $llamaCommit"
    }
    $tagCommit = (& git.exe -C $llamaSource rev-parse "$llamaVersion^{commit}").Trim()
    if ($LASTEXITCODE -ne 0 -or $llamaCommit -ne $tagCommit) { throw 'The cached llama.cpp source does not match LLAMA_CPP_VERSION.' }

    $configureArgs = @(
        "-S", $sourceRoot,
        "-B", $buildRoot,
        "-G", "Ninja",
        "-DCMAKE_BUILD_TYPE=Release",
        "-DCMAKE_INSTALL_PREFIX=$stageRoot",
        "-DFETCHCONTENT_SOURCE_DIR_LLAMA_CPP=$llamaSource",
        "-DCMAKE_CUDA_COMPILER=$($cuda.Nvcc)",
        "-DCUDAToolkit_ROOT=$($cuda.Root)",
        "-DCMAKE_CUDA_ARCHITECTURES=$CudaArchitectures",
        "-DCMAKE_CUDA_FLAGS=-Wno-deprecated-gpu-targets -t 2",
        "-DBUILD_SHARED_LIBS=ON",
        "-DGGML_BACKEND_DL=ON",
        "-DGGML_NATIVE=OFF",
        "-DGGML_OPENMP=OFF",
        "-DGGML_CCACHE=OFF",
        "-DGGML_CUDA=ON",
        "-DOLLAMA_RUNNER_DIR=cuda_legacy",
        "-DOLLAMA_GPU_BACKEND=cuda"
    )

    Write-Host "Configuring CUDA Legacy backend for the portable package..."
    & $cmake.Source @configureArgs
    if ($LASTEXITCODE -ne 0) {
        throw "CMake configure failed with exit code $LASTEXITCODE."
    }

    Write-Host "Building ggml-cuda for sm_61 and sm_75..."
    & $cmake.Source --build $buildRoot --target ggml-cuda --config Release --parallel $Jobs
    if ($LASTEXITCODE -ne 0) {
        throw "CUDA Legacy backend build failed with exit code $LASTEXITCODE."
    }

    Write-Host "Staging runtime DLLs..."
    & $cmake.Source --install $buildRoot --prefix $stageRoot --component llama-server --config Release
    if ($LASTEXITCODE -ne 0) {
        throw "CUDA Legacy install step failed with exit code $LASTEXITCODE."
    }

    $installedRuntime = Join-Path $stageRoot "lib\ollama\cuda_legacy"
    $backendDll = Join-Path $installedRuntime "ggml-cuda.dll"
    if (-not (Test-Path -LiteralPath $backendDll)) {
        throw "ggml-cuda.dll was not produced under $installedRuntime"
    }

    $cudaObjects = (& $cuda.Cuobjdump --list-elf $backendDll 2>&1 | Out-String)
    if ($LASTEXITCODE -ne 0) { throw 'cuobjdump could not inspect the produced backend.' }
    foreach ($architecture in ($CudaArchitectures -split ';')) {
        if ($cudaObjects -notmatch "sm_$architecture") {
            throw "cuobjdump did not find sm_$architecture in ggml-cuda.dll."
        }
    }

    New-Item -ItemType Directory -Path $packageRuntime -Force | Out-Null
    Copy-Item -Path (Join-Path $installedRuntime "*") -Destination $packageRuntime -Recurse -Force

    $runtimeFiles = Get-ChildItem -LiteralPath $packageRuntime -File
    foreach ($requiredPattern in @("ggml-cuda*.dll", "cudart64_*.dll", "cublas64_*.dll", "cublasLt64_*.dll")) {
        if (-not ($runtimeFiles | Where-Object { $_.Name -like $requiredPattern })) {
            throw "The package is missing required CUDA runtime file pattern: $requiredPattern"
        }
    }

    # Inspect the PE headers without executing GPU code on the CPU-only runner.
    foreach ($dll in $runtimeFiles | Where-Object Extension -eq '.dll') {
        $stream = [IO.File]::OpenRead($dll.FullName)
        $reader = [IO.BinaryReader]::new($stream)
        try {
            if ($reader.ReadUInt16() -ne 0x5A4D) { throw "Invalid DOS header: $($dll.Name)" }
            $stream.Position = 0x3C
            $peOffset = $reader.ReadInt32()
            $stream.Position = $peOffset
            if ($reader.ReadUInt32() -ne 0x4550 -or $reader.ReadUInt16() -ne 0x8664) {
                throw "Expected a Windows x64 DLL: $($dll.Name)"
            }
        } finally { $reader.Dispose() }
    }
    if ($runtimeFiles | Where-Object { $_.Extension -ne '.dll' -or $_.Name -match '^(ggml-base|ggml|llama)\.dll$' }) {
        throw 'The staged runtime contains files outside the additional backend and its DLL dependencies.'
    }
    $exports = (& dumpbin.exe /exports (Join-Path $packageRuntime 'ggml-cuda.dll') 2>&1 | Out-String)
    if ($LASTEXITCODE -ne 0 -or $exports -notmatch '\bggml_backend_init\b') {
        throw 'The produced DLL does not export the GGML backend entry point.'
    }
    $dependencyReport = foreach ($dll in $runtimeFiles) {
        "=== $($dll.Name) ==="
        $imports = & dumpbin.exe /dependents $dll.FullName
        if ($LASTEXITCODE -ne 0) { throw "Could not inspect imports: $($dll.Name)" }
        $imports
        foreach ($line in $imports) {
            if ($line -match '^\s+([A-Za-z0-9_.-]+\.dll)\s*$') {
                $name = $Matches[1]
                if ($name -in $runtimeFiles.Name) { continue }
                # The portable already supplies GGML base libraries; the NVIDIA
                # display driver supplies nvcuda.dll. Never bundle either here.
                if ($name -in @('ggml-base.dll', 'ggml.dll', 'nvcuda.dll')) { continue }
                if ($name -match '^(api-ms-win-|ext-ms-win-)') { continue }
                if ($name -notmatch '^(msvcp|vcruntime|concrt)' -and
                    (Test-Path -LiteralPath (Join-Path $env:SystemRoot "System32\$name"))) { continue }
                throw "Missing packaged DLL dependency: $($dll.Name) -> $name"
            }
        }
    }

    $gitCommit = "not available"
    $git = Get-Command git.exe -ErrorAction SilentlyContinue | Select-Object -First 1
    if ($git) {
        $candidateCommit = & $git.Source -C $repoRoot rev-parse HEAD 2>$null
        if ($LASTEXITCODE -eq 0 -and $candidateCommit) {
            $gitCommit = $candidateCommit.Trim()
        }
    }

    $buildInfo = @(
        "Ollama Portable CUDA Legacy runtime",
        "",
        "Ollama source: $gitCommit",
        "llama.cpp version: $llamaVersion",
        "llama.cpp commit: $llamaCommit",
        "CUDA architectures: $CudaArchitectures",
        "CUDA toolkit: $($cuda.Root)",
        "",
        "Target GPUs:",
        "- Pascal sm_61 (GTX 1050 Ti)",
        "- Turing sm_75 (RTX 2060 SUPER)",
        "",
        "Merge this package into the Ollama Portable root.",
        "It adds runtimes\cuda_legacy only.",
        "Do not replace runtimes\ollama-serve.exe, llama-server.exe, base GGML DLLs, Vulkan, CUDA 12, database, chats, MCP, or models."
    )
    $buildInfo | Set-Content -LiteralPath (Join-Path $packageRuntime "CUDA-LEGACY-BUILD-INFO.txt") -Encoding UTF8
    $cudaObjects | Set-Content -LiteralPath (Join-Path $packageRuntime 'CUDA-ARCHITECTURES.txt') -Encoding UTF8
    $dependencyReport | Set-Content -LiteralPath (Join-Path $packageRuntime 'DLL-DEPENDENCIES.txt') -Encoding UTF8
    $redistManifest = Join-Path $cuda.Root 'CUDA-REDIST-SHA256.json'
    if (Test-Path -LiteralPath $redistManifest) { Copy-Item -LiteralPath $redistManifest -Destination $packageRuntime }
    $licenses = Join-Path $cuda.Root 'redistribution-licenses'
    if (Test-Path -LiteralPath $licenses) { Copy-Item -LiteralPath $licenses -Destination (Join-Path $packageRuntime 'licenses') -Recurse }
    Get-ChildItem -LiteralPath $packageRuntime -File -Recurse | ForEach-Object {
        "{0}  {1}" -f (Get-FileHash -LiteralPath $_.FullName -Algorithm SHA256).Hash.ToLowerInvariant(),
            ([IO.Path]::GetRelativePath($packageRuntime, $_.FullName)).Replace('\', '/')
    } | Set-Content -LiteralPath (Join-Path $packageRuntime 'SHA256SUMS.txt') -Encoding UTF8

    if (Test-Path -LiteralPath $zipPath) {
        Remove-Item -LiteralPath $zipPath -Force
    }
    Compress-Archive -Path (Join-Path $packageRoot 'runtimes') -DestinationPath $zipPath -CompressionLevel Optimal
    $zip = [IO.Compression.ZipFile]::OpenRead($zipPath)
    try {
        foreach ($entry in $zip.Entries) {
            if ($entry.FullName.Replace('\', '/') -notmatch '^runtimes/cuda_legacy/') {
                throw "Unexpected ZIP entry: $($entry.FullName)"
            }
        }
        if (-not ($zip.Entries | Where-Object { $_.FullName.Replace('\', '/') -eq 'runtimes/cuda_legacy/ggml-cuda.dll' })) {
            throw 'The runtime ZIP does not contain the backend DLL.'
        }
    } finally { $zip.Dispose() }
    "{0}  {1}" -f (Get-FileHash -LiteralPath $zipPath -Algorithm SHA256).Hash.ToLowerInvariant(),
        [IO.Path]::GetFileName($zipPath) | Set-Content -LiteralPath "$zipPath.sha256" -Encoding UTF8

    Write-Host ""
    Write-Host "CUDA Legacy portable package completed:"
    Write-Host "Folder: $packageRoot"
    Write-Host "ZIP:    $zipPath"
} finally {
    $env:TEMP = $oldTemp
    $env:TMP = $oldTmp
    $env:CUDA_PATH = $oldCudaPath
    $env:CUDACXX = $oldCudaCxx
    $env:_CL_ = $oldCl
    $env:GGML_CCACHE = $oldCcache
}
