[CmdletBinding()]
param(
    [string]$Version,
    [string]$OutputDirectory = "dist",
    [string]$GoExecutable = "go",
    [switch]$SkipChecks,
    [switch]$SkipMSI,
    [string]$WixExecutable = "wix",
    [string]$WixUIExtension = "WixToolset.UI.wixext"
)

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

$consoleEncoding = [System.Text.UTF8Encoding]::new($false)
[Console]::InputEncoding = $consoleEncoding
[Console]::OutputEncoding = $consoleEncoding
$OutputEncoding = $consoleEncoding

$repositoryRoot = [System.IO.Path]::GetFullPath((Join-Path $PSScriptRoot ".."))
$temporaryRoot = [System.IO.Path]::GetFullPath((Join-Path $repositoryRoot ".tmp"))
$stageRoot = Join-Path $temporaryRoot ("release-" + [guid]::NewGuid().ToString("N"))
$outputRoot = if ([System.IO.Path]::IsPathRooted($OutputDirectory)) {
    [System.IO.Path]::GetFullPath($OutputDirectory)
} else {
    [System.IO.Path]::GetFullPath((Join-Path $repositoryRoot $OutputDirectory))
}

if ([string]::IsNullOrWhiteSpace($Version)) {
    $versionSource = Get-Content -Raw -LiteralPath (Join-Path $repositoryRoot "executor\executor.go")
    $versionMatch = [regex]::Match($versionSource, 'const\s+Version\s*=\s*"([^"]+)"')
    if (-not $versionMatch.Success) {
        throw "Unable to determine the version from executor/executor.go"
    }
    $Version = $versionMatch.Groups[1].Value
}
if ($Version -notmatch '^[0-9]+\.[0-9]+\.[0-9]+(?:[-+][0-9A-Za-z.-]+)?$') {
    throw "Invalid release version: $Version"
}

function Invoke-GoCommand {
    param([Parameter(Mandatory)][string[]]$Arguments)
    & $GoExecutable @Arguments
    if ($LASTEXITCODE -ne 0) {
        throw "Go command failed: go $($Arguments -join ' ')"
    }
}

function Get-SHA256Hex {
    param([Parameter(Mandatory)][string]$Path)

    $stream = [System.IO.File]::OpenRead($Path)
    try {
        $algorithm = [System.Security.Cryptography.SHA256]::Create()
        try {
            $bytes = $algorithm.ComputeHash($stream)
            return ([System.BitConverter]::ToString($bytes)).Replace("-", "").ToLowerInvariant()
        } finally {
            $algorithm.Dispose()
        }
    } finally {
        $stream.Dispose()
    }
}


function Build-GoTarget {
    param(
        [Parameter(Mandatory)][string]$TargetOS,
        [Parameter(Mandatory)][string]$TargetArchitecture,
        [Parameter(Mandatory)][string]$OutputPath
    )
    $previousGOOS = $env:GOOS
    $previousGOARCH = $env:GOARCH
    $previousCGO = $env:CGO_ENABLED
    try {
        $env:GOOS = $TargetOS
        $env:GOARCH = $TargetArchitecture
        $env:CGO_ENABLED = "0"
        Invoke-GoCommand @("build", "-trimpath", "-ldflags=-s -w", "-o", $OutputPath, "./cmd/gbaselite")
    } finally {
        $env:GOOS = $previousGOOS
        $env:GOARCH = $previousGOARCH
        $env:CGO_ENABLED = $previousCGO
    }
}

function Copy-PortableFiles {
    param(
        [Parameter(Mandatory)][string]$Destination,
        [Parameter(Mandatory)][ValidateSet("windows", "linux")][string]$Platform
    )
    Copy-Item -LiteralPath (Join-Path $repositoryRoot "README.md") -Destination $Destination
    Copy-Item -LiteralPath (Join-Path $repositoryRoot "LICENSE") -Destination $Destination
    Copy-Item -LiteralPath (Join-Path $repositoryRoot "docker\config.example.yaml") -Destination (Join-Path $Destination "config.example.yaml")
    $scriptPattern = if ($Platform -eq "windows") { "*.bat" } else { "*.sh" }
    $scriptDirectory = Join-Path $repositoryRoot ("scripts\" + $Platform)
    Get-ChildItem -LiteralPath $scriptDirectory -Filter $scriptPattern -File |
        Copy-Item -Destination $Destination
}

function New-PortableArchive {
    param(
        [Parameter(Mandatory)][ValidateSet("zip", "tar.gz")][string]$Format,
        [Parameter(Mandatory)][string]$Source,
        [Parameter(Mandatory)][string]$Destination
    )
    Invoke-GoCommand @(
        "run", "./scripts/internal/archive",
        "-format", $Format,
        "-source", $Source,
        "-output", $Destination
    )
}

Push-Location $repositoryRoot
$previousGoCache = $env:GOCACHE
try {
    New-Item -ItemType Directory -Path $temporaryRoot -Force | Out-Null
    New-Item -ItemType Directory -Path $stageRoot -Force | Out-Null
    New-Item -ItemType Directory -Path $outputRoot -Force | Out-Null
    $env:GOCACHE = Join-Path $temporaryRoot "gocache"

    if (-not $SkipChecks) {
        $goDirectory = Split-Path -Parent ([System.IO.Path]::GetFullPath($GoExecutable))
        $gofmt = Join-Path $goDirectory "gofmt.exe"
        if (-not (Test-Path -LiteralPath $gofmt)) {
            $gofmt = "gofmt"
        }
        $unformatted = & $gofmt -l .
        if ($LASTEXITCODE -ne 0) {
            throw "gofmt check failed"
        }
        if ($unformatted) {
            throw "The following Go files require gofmt: $($unformatted -join ', ')"
        }
        Invoke-GoCommand @("test", "./...", "-count=1")
        Invoke-GoCommand @("vet", "./...")
    }

    $windowsStage = Join-Path $stageRoot "windows-amd64"
    $linuxAMD64Stage = Join-Path $stageRoot "linux-amd64"
    $linuxARM64Stage = Join-Path $stageRoot "linux-arm64"
    New-Item -ItemType Directory -Path $windowsStage, $linuxAMD64Stage, $linuxARM64Stage | Out-Null

    Build-GoTarget -TargetOS "windows" -TargetArchitecture "amd64" -OutputPath (Join-Path $windowsStage "gbaselite.exe")
    Build-GoTarget -TargetOS "linux" -TargetArchitecture "amd64" -OutputPath (Join-Path $linuxAMD64Stage "gbaselite")
    Build-GoTarget -TargetOS "linux" -TargetArchitecture "arm64" -OutputPath (Join-Path $linuxARM64Stage "gbaselite")
    Invoke-GoCommand @("run", "./scripts/internal/inspectelf", "-file", (Join-Path $linuxAMD64Stage "gbaselite"), "-machine", "amd64")
    Invoke-GoCommand @("run", "./scripts/internal/inspectelf", "-file", (Join-Path $linuxARM64Stage "gbaselite"), "-machine", "arm64")
    Copy-PortableFiles -Destination $windowsStage -Platform "windows"
    Copy-PortableFiles -Destination $linuxAMD64Stage -Platform "linux"
    Copy-PortableFiles -Destination $linuxARM64Stage -Platform "linux"

    $linuxAMD64Binary = Join-Path $outputRoot "gbaselite-linux-amd64"
    $linuxARM64Binary = Join-Path $outputRoot "gbaselite-linux-arm64"
    Copy-Item -LiteralPath (Join-Path $linuxAMD64Stage "gbaselite") -Destination $linuxAMD64Binary -Force
    Copy-Item -LiteralPath (Join-Path $linuxARM64Stage "gbaselite") -Destination $linuxARM64Binary -Force

    $windowsArchive = Join-Path $outputRoot "gbaselite-windows-amd64.zip"
    $linuxAMD64Archive = Join-Path $outputRoot "gbaselite-linux-amd64.tar.gz"
    $linuxARM64Archive = Join-Path $outputRoot "gbaselite-linux-arm64.tar.gz"
    foreach ($path in @($windowsArchive, $linuxAMD64Archive, $linuxARM64Archive)) {
        if (Test-Path -LiteralPath $path) {
            Remove-Item -LiteralPath $path -Force
        }
    }
    New-PortableArchive -Format "zip" -Source $windowsStage -Destination $windowsArchive
    New-PortableArchive -Format "tar.gz" -Source $linuxAMD64Stage -Destination $linuxAMD64Archive
    New-PortableArchive -Format "tar.gz" -Source $linuxARM64Stage -Destination $linuxARM64Archive

    $msiPath = Join-Path $outputRoot "GBaseLite-windows-amd64.msi"
    if (-not $SkipMSI) {
        $wixCommand = Get-Command $WixExecutable -ErrorAction SilentlyContinue
        $dotnetCommand = Get-Command "dotnet" -ErrorAction SilentlyContinue
        if ($null -ne $wixCommand -and $null -ne $dotnetCommand) {
            & (Join-Path $PSScriptRoot "build-msi.ps1") -Version $Version -SourceDirectory $windowsStage -OutputPath $msiPath -WixExecutable $wixCommand.Source -WixUIExtension $WixUIExtension
            if ($LASTEXITCODE -ne 0) {
                throw "MSI build failed"
            }
        } else {
            Write-Warning "WiX and a .NET SDK are required for MSI generation; portable packages will still be produced."
        }
    }

    $sbomPath = Join-Path $outputRoot "sbom.spdx.json"
    $syft = Get-Command "syft" -ErrorAction SilentlyContinue
    if ($null -ne $syft) {
        & $syft.Source ("dir:" + $repositoryRoot) "-o" ("spdx-json=" + $sbomPath)
        if ($LASTEXITCODE -ne 0) {
            throw "SBOM generation failed"
        }
    } elseif (Test-Path -LiteralPath $sbomPath) {
        Remove-Item -LiteralPath $sbomPath -Force
    }

    $artifacts = Get-ChildItem -LiteralPath $outputRoot -File | Where-Object {
        $_.Name -in @(
            (Split-Path -Leaf $windowsArchive),
            (Split-Path -Leaf $linuxAMD64Archive),
            (Split-Path -Leaf $linuxARM64Archive),
            (Split-Path -Leaf $linuxAMD64Binary),
            (Split-Path -Leaf $linuxARM64Binary),
            (Split-Path -Leaf $msiPath),
            (Split-Path -Leaf $sbomPath)
        )
    } | Sort-Object Name
    $checksumLines = foreach ($artifact in $artifacts) {
        $hash = Get-SHA256Hex -Path $artifact.FullName
        "$hash  $($artifact.Name)"
    }
    $checksumPath = Join-Path $outputRoot "checksums.txt"
    [System.IO.File]::WriteAllLines($checksumPath, $checksumLines, [System.Text.UTF8Encoding]::new($false))

    Write-Host "Created GBaseLite $Version release artifacts:"
    Get-Item -LiteralPath @($artifacts.FullName; $checksumPath) |
        Select-Object Name, Length, LastWriteTime |
        Format-Table -AutoSize
} finally {
    Pop-Location
    $env:GOCACHE = $previousGoCache
    $normalizedStage = [System.IO.Path]::GetFullPath($stageRoot)
    $allowedPrefix = $temporaryRoot.TrimEnd([System.IO.Path]::DirectorySeparatorChar) + [System.IO.Path]::DirectorySeparatorChar
    if ((Test-Path -LiteralPath $normalizedStage) -and $normalizedStage.StartsWith($allowedPrefix, [System.StringComparison]::OrdinalIgnoreCase)) {
        Remove-Item -LiteralPath $normalizedStage -Recurse -Force
    }
}
