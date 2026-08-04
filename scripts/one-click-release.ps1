[CmdletBinding()]
param(
    [switch]$Preview,
    [switch]$SelfTest,
    [string]$TargetVersion
)

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

$consoleEncoding = [System.Text.UTF8Encoding]::new($false)
[Console]::InputEncoding = $consoleEncoding
[Console]::OutputEncoding = $consoleEncoding
$OutputEncoding = $consoleEncoding

$repositoryRoot = [System.IO.Path]::GetFullPath((Join-Path $PSScriptRoot ".."))
$temporaryRoot = [System.IO.Path]::GetFullPath((Join-Path $repositoryRoot ".tmp"))
$distRoot = [System.IO.Path]::GetFullPath((Join-Path $repositoryRoot "dist"))
$dockerSourceRoot = [System.IO.Path]::GetFullPath((Join-Path $repositoryRoot "docker"))
$utf8NoBom = $consoleEncoding

function Get-CurrentVersion {
    $source = Get-Content -Raw -LiteralPath (Join-Path $repositoryRoot "executor\executor.go")
    $match = [regex]::Match($source, 'const\s+Version\s*=\s*"([0-9]+\.[0-9]+\.[0-9]+)"')
    if (-not $match.Success) {
        throw "Unable to read the current version from executor/executor.go"
    }
    return $match.Groups[1].Value
}

function Get-NextVersion {
    param([Parameter(Mandatory)][string]$CurrentVersion)

    $match = [regex]::Match($CurrentVersion, '^(\d+)\.(\d+)\.(\d+)$')
    if (-not $match.Success) {
        throw "Unsupported current version: $CurrentVersion"
    }
    $major = [int]$match.Groups[1].Value
    $minor = [int]$match.Groups[2].Value
    $revision = [int]$match.Groups[3].Value
    if ($major -lt 1) {
        return "1.0.0"
    }
    if ($revision -ge 999) {
        $minor++
        return "$major.$minor.0"
    }
    $revision++
    return "$major.$minor.$($revision.ToString('000'))"
}

function Write-FileAtomically {
    param(
        [Parameter(Mandatory)][string]$Path,
        [Parameter(Mandatory)][string]$Contents
    )

    $fullPath = [System.IO.Path]::GetFullPath($Path)
    $temporaryPath = $fullPath + ".release-" + [guid]::NewGuid().ToString("N") + ".tmp"
    try {
        [System.IO.File]::WriteAllText($temporaryPath, $Contents, $utf8NoBom)
        Move-Item -LiteralPath $temporaryPath -Destination $fullPath -Force
    } finally {
        if (Test-Path -LiteralPath $temporaryPath) {
            Remove-Item -LiteralPath $temporaryPath -Force
        }
    }
}

function Replace-RequiredVersion {
    param(
        [Parameter(Mandatory)][string]$Path,
        [Parameter(Mandatory)][string]$OldVersion,
        [Parameter(Mandatory)][string]$NewVersion
    )

    $contents = [System.IO.File]::ReadAllText($Path)
    if (-not $contents.Contains($OldVersion)) {
        throw "Version $OldVersion was not found in $Path"
    }
    Write-FileAtomically -Path $Path -Contents $contents.Replace($OldVersion, $NewVersion)
}

function Update-Changelog {
    param(
        [Parameter(Mandatory)][string]$Path,
        [Parameter(Mandatory)][string]$Version
    )

    $contents = [System.IO.File]::ReadAllText($Path)
    $existingRelease = '(?m)^## \[' + [regex]::Escape($Version) + '\](?:\s+-.*)?\s*$'
    if ([regex]::IsMatch($contents, $existingRelease)) {
        return
    }
    $newLine = if ($contents.Contains("`r`n")) { "`r`n" } else { "`n" }
    $match = [regex]::Match($contents, '(?ms)^## \[Unreleased\]\s*(.*?)(?=^## \[)')
    if (-not $match.Success) {
        throw "Unable to locate the Unreleased section in CHANGELOG.md"
    }
    $notes = $match.Groups[1].Value.Trim()
    if ([string]::IsNullOrWhiteSpace($notes)) {
        $notes = "### Changed${newLine}${newLine}- Automated release build."
    }
    $replacement = "## [Unreleased]${newLine}${newLine}## [$Version] - $(Get-Date -Format 'yyyy-MM-dd')${newLine}${newLine}$notes${newLine}${newLine}"
    $updated = $contents.Remove($match.Index, $match.Length).Insert($match.Index, $replacement)
    Write-FileAtomically -Path $Path -Contents $updated
}

function Update-BinaryReleasePaths {
    param([Parameter(Mandatory)][string]$Version)

    $composePath = Join-Path $dockerSourceRoot "docker-compose.binary.yml"
    $composeContents = [System.IO.File]::ReadAllText($composePath)
    $composeReplacement = '      - ../dist/GBaseLite-' + $Version + '/gbaselite-linux-amd64:/home/bin/gbaselite:ro'
    $composePattern = '(?m)^\s*-\s+\.\./dist/GBaseLite-[^/]+/gbaselite-linux-amd64:/home/bin/gbaselite:ro\s*$'
    if (-not [regex]::IsMatch($composeContents, $composePattern)) {
        throw "Unable to locate the binary release path in $composePath"
    }
    $composeUpdated = [regex]::Replace(
        $composeContents,
        $composePattern,
        $composeReplacement
    )
    Write-FileAtomically -Path $composePath -Contents $composeUpdated
}

function Resolve-GoExecutable {
    $candidates = @()
    if (-not [string]::IsNullOrWhiteSpace($env:GBASELITE_GO)) {
        $candidates += $env:GBASELITE_GO
    }
    $candidates += "D:\env\Go\bin\go.exe"
    $command = Get-Command "go" -ErrorAction SilentlyContinue
    if ($null -ne $command) {
        $candidates += $command.Source
    }
    foreach ($candidate in $candidates) {
        if (Test-Path -LiteralPath $candidate -PathType Leaf) {
            return [System.IO.Path]::GetFullPath($candidate)
        }
    }
    throw "Go was not found. Install Go 1.22+ or set GBASELITE_GO."
}

function Test-DotnetSDK {
    param([Parameter(Mandatory)][string]$Executable)

    if (-not (Test-Path -LiteralPath $Executable -PathType Leaf)) {
        return $false
    }
    $sdks = & $Executable --list-sdks 2>$null
    return $LASTEXITCODE -eq 0 -and [bool]$sdks
}

function Invoke-NativeCommand {
    param(
        [Parameter(Mandatory)][string]$Executable,
        [Parameter(Mandatory)][string[]]$Arguments,
        [Parameter(Mandatory)][string]$FailureMessage
    )

    & $Executable @Arguments | Out-Host
    $exitCode = $LASTEXITCODE
    if ($exitCode -ne 0) {
        throw "$FailureMessage (exit code $exitCode)"
    }
}

function Resolve-DotnetExecutable {
    $portable = Join-Path $temporaryRoot "dotnet-sdk\dotnet.exe"
    if (-not [string]::IsNullOrWhiteSpace($env:GBASELITE_DOTNET) -and (Test-DotnetSDK $env:GBASELITE_DOTNET)) {
        return [System.IO.Path]::GetFullPath($env:GBASELITE_DOTNET)
    }
    if (Test-DotnetSDK $portable) {
        return [System.IO.Path]::GetFullPath($portable)
    }
    $command = Get-Command "dotnet" -ErrorAction SilentlyContinue
    if ($null -ne $command -and (Test-DotnetSDK $command.Source)) {
        return $command.Source
    }

    New-Item -ItemType Directory -Path $temporaryRoot -Force | Out-Null
    $installer = Join-Path $temporaryRoot "dotnet-install.ps1"
    if (-not (Test-Path -LiteralPath $installer -PathType Leaf)) {
        Write-Host "Downloading the official Microsoft .NET installer..."
        Invoke-WebRequest -Uri "https://dot.net/v1/dotnet-install.ps1" -OutFile $installer
    }
    $signature = Get-AuthenticodeSignature -LiteralPath $installer
    if ($signature.Status -ne [System.Management.Automation.SignatureStatus]::Valid) {
        throw "The Microsoft .NET installer signature is not valid: $($signature.Status)"
    }
    Write-Host "Installing portable .NET 8 SDK under .tmp..."
    Invoke-NativeCommand -Executable "powershell.exe" -Arguments @(
        "-NoProfile",
        "-ExecutionPolicy", "Bypass",
        "-File", $installer,
        "-Channel", "8.0",
        "-InstallDir", (Split-Path -Parent $portable),
        "-NoPath"
    ) -FailureMessage "Unable to install the portable .NET SDK"
    if (-not (Test-DotnetSDK $portable)) {
        throw "Unable to install the portable .NET SDK"
    }
    return [System.IO.Path]::GetFullPath($portable)
}

function Resolve-WixExecutable {
    param([Parameter(Mandatory)][string]$DotnetExecutable)

    if (-not [string]::IsNullOrWhiteSpace($env:GBASELITE_WIX) -and (Test-Path -LiteralPath $env:GBASELITE_WIX -PathType Leaf)) {
        return [System.IO.Path]::GetFullPath($env:GBASELITE_WIX)
    }
    $portable = Join-Path $temporaryRoot "wix-cli\wix.exe"
    if (Test-Path -LiteralPath $portable -PathType Leaf) {
        return [System.IO.Path]::GetFullPath($portable)
    }
    $command = Get-Command "wix" -ErrorAction SilentlyContinue
    if ($null -ne $command) {
        return $command.Source
    }
    Write-Host "Installing portable WiX 5.0.2 under .tmp..."
    Invoke-NativeCommand -Executable $DotnetExecutable -Arguments @(
        "tool", "install",
        "--tool-path", (Split-Path -Parent $portable),
        "wix",
        "--version", "5.0.2"
    ) -FailureMessage "Unable to install portable WiX 5.0.2"
    if (-not (Test-Path -LiteralPath $portable -PathType Leaf)) {
        throw "Unable to install portable WiX 5.0.2"
    }
    return [System.IO.Path]::GetFullPath($portable)
}

function Resolve-WixUIExtension {
    param([Parameter(Mandatory)][string]$WixExecutable)

    if (-not [string]::IsNullOrWhiteSpace($env:GBASELITE_WIX_UI_EXTENSION) -and (Test-Path -LiteralPath $env:GBASELITE_WIX_UI_EXTENSION -PathType Leaf)) {
        return [System.IO.Path]::GetFullPath($env:GBASELITE_WIX_UI_EXTENSION)
    }
    $extension = Join-Path $env:USERPROFILE ".wix\extensions\WixToolset.UI.wixext\5.0.2\wixext5\WixToolset.UI.wixext.dll"
    if (-not (Test-Path -LiteralPath $extension -PathType Leaf)) {
        Write-Host "Installing the WiX Simplified Chinese UI extension..."
        Invoke-NativeCommand -Executable $WixExecutable -Arguments @(
            "extension", "add", "--global", "WixToolset.UI.wixext/5.0.2"
        ) -FailureMessage "Unable to install WixToolset.UI.wixext 5.0.2"
    }
    if (-not (Test-Path -LiteralPath $extension -PathType Leaf)) {
        throw "The WiX UI extension DLL was not found after installation"
    }
    return [System.IO.Path]::GetFullPath($extension)
}

function Read-MsiRows {
    param(
        [Parameter(Mandatory)]$Database,
        [Parameter(Mandatory)][string]$Query,
        [Parameter(Mandatory)][int]$ColumnCount
    )

    $flags = [System.Reflection.BindingFlags]::InvokeMethod
    $rows = @()
    $view = $null
    try {
        $view = $Database.GetType().InvokeMember("OpenView", $flags, $null, $Database, [object[]]@($Query))
        $view.GetType().InvokeMember("Execute", $flags, $null, $view, $null) | Out-Null
        while ($true) {
            $record = $view.GetType().InvokeMember("Fetch", $flags, $null, $view, $null)
            if ($null -eq $record) {
                break
            }
            try {
                $values = @()
                for ($index = 1; $index -le $ColumnCount; $index++) {
                    $values += $record.GetType().InvokeMember("StringData", [System.Reflection.BindingFlags]::GetProperty, $null, $record, [object[]]@([int]$index))
                }
                $rows += ,$values
            } finally {
                [void][System.Runtime.InteropServices.Marshal]::FinalReleaseComObject($record)
            }
        }
    } finally {
        if ($null -ne $view) {
            [void][System.Runtime.InteropServices.Marshal]::FinalReleaseComObject($view)
        }
    }
    return ,$rows
}

function Test-ComposeConfiguration {
    $docker = Get-Command "docker" -ErrorAction SilentlyContinue
    if ($null -eq $docker) {
        Write-Warning "Docker CLI was not found; Compose configuration validation was skipped."
        return
    }

    $previousErrorPreference = $ErrorActionPreference
    $previousDockerConfig = $env:DOCKER_CONFIG
    $dockerConfigRoot = Join-Path $temporaryRoot "docker-config"
    New-Item -ItemType Directory -Path $dockerConfigRoot -Force | Out-Null
    $env:DOCKER_CONFIG = $dockerConfigRoot
    try {
        $ErrorActionPreference = "Continue"
        $composeVersionOutput = & $docker.Source compose version 2>&1
        $composeVersionExitCode = $LASTEXITCODE
    } finally {
        $ErrorActionPreference = $previousErrorPreference
    }
    if ($composeVersionExitCode -ne 0) {
        $env:DOCKER_CONFIG = $previousDockerConfig
        Write-Warning "Docker Compose was not found; Compose configuration validation was skipped."
        return
    }

    $validationRoot = Join-Path $temporaryRoot ("compose-validation-" + [guid]::NewGuid().ToString("N"))
    try {
        $validationDockerRoot = Join-Path $validationRoot "docker"
        New-Item -ItemType Directory -Path $validationRoot, $validationDockerRoot -Force | Out-Null
        New-Item -ItemType Directory -Path (Join-Path $validationRoot "dist") -Force | Out-Null
        foreach ($name in @("Dockerfile", "docker-compose.yml", "docker-compose.build.yml", "docker-compose.binary.yml")) {
            Copy-Item -LiteralPath (Join-Path $dockerSourceRoot $name) -Destination (Join-Path $validationDockerRoot $name)
        }
        Copy-Item -LiteralPath (Join-Path $dockerSourceRoot "config.example.yaml") -Destination (Join-Path $validationDockerRoot "config.example.yaml")
        Copy-Item -LiteralPath (Join-Path $dockerSourceRoot "temp.env.example") -Destination (Join-Path $validationDockerRoot "temp.env")
        $validationVersion = Get-CurrentVersion
        $validationBinaryDirectory = Join-Path $validationRoot "dist\GBaseLite-$validationVersion"
        New-Item -ItemType Directory -Path $validationBinaryDirectory -Force | Out-Null
        [System.IO.File]::WriteAllBytes((Join-Path $validationBinaryDirectory "gbaselite-linux-amd64"), [byte[]]@(0))

        foreach ($name in @("docker-compose.yml", "docker-compose.build.yml", "docker-compose.binary.yml")) {
            try {
                $ErrorActionPreference = "Continue"
                $composeOutput = & $docker.Source compose --env-file (Join-Path $validationDockerRoot "temp.env") -f (Join-Path $validationDockerRoot $name) config --quiet 2>&1
                $composeExitCode = $LASTEXITCODE
            } finally {
                $ErrorActionPreference = $previousErrorPreference
            }
            if ($composeExitCode -ne 0) {
                throw "Docker Compose validation failed for $name`n$($composeOutput -join [Environment]::NewLine)"
            }
        }
    } finally {
        $env:DOCKER_CONFIG = $previousDockerConfig
        if (Test-Path -LiteralPath $validationRoot) {
            Remove-Item -LiteralPath $validationRoot -Recurse -Force
        }
    }
}

function Test-ReleaseCandidate {
    param(
        [Parameter(Mandatory)][string]$OutputDirectory,
        [Parameter(Mandatory)][string]$Version
    )

    $required = @(
        "GBaseLite-windows-amd64.msi",
        "gbaselite-windows-amd64.zip",
        "gbaselite-linux-amd64.tar.gz",
        "gbaselite-linux-arm64.tar.gz",
        "gbaselite-linux-amd64",
        "gbaselite-linux-arm64",
        "checksums.txt"
    )
    foreach ($name in $required) {
        if (-not (Test-Path -LiteralPath (Join-Path $OutputDirectory $name) -PathType Leaf)) {
            throw "Required release artifact is missing: $name"
        }
    }

    $checksumEntries = @{}
    foreach ($line in Get-Content -LiteralPath (Join-Path $OutputDirectory "checksums.txt")) {
        if ($line -notmatch '^([0-9a-f]{64})  (.+)$') {
            throw "Invalid checksum line: $line"
        }
        $checksumEntries[$Matches[2]] = $Matches[1]
    }
    foreach ($name in $required | Where-Object { $_ -ne "checksums.txt" }) {
        if (-not $checksumEntries.ContainsKey($name)) {
            throw "checksums.txt does not contain $name"
        }
        $actual = (Get-FileHash -Algorithm SHA256 -LiteralPath (Join-Path $OutputDirectory $name)).Hash.ToLowerInvariant()
        if ($actual -ne $checksumEntries[$name]) {
            throw "Checksum mismatch for $name"
        }
    }

    Add-Type -AssemblyName System.IO.Compression.FileSystem
    $zipPath = Join-Path $OutputDirectory "gbaselite-windows-amd64.zip"
    $zip = [System.IO.Compression.ZipFile]::OpenRead($zipPath)
    try {
        $entries = @($zip.Entries | ForEach-Object { $_.FullName })
        foreach ($name in @("gbaselite.exe", "config.example.yaml", "README.md", "LICENSE", "start.bat", "stop.bat", "restart.bat")) {
            if ($name -notin $entries) {
                throw "Windows ZIP is missing $name"
            }
        }
        if ($entries -match '(^|/)(data|logs|bin|\.tmp|release)(/|$)|(^|/)(config\.yaml|\.env|temp\.env|gbaselite\.pid)$') {
            throw "Windows ZIP contains a forbidden runtime path"
        }
    } finally {
        $zip.Dispose()
    }

    $inspectionRoot = Join-Path $OutputDirectory "inspection"
    New-Item -ItemType Directory -Path $inspectionRoot -Force | Out-Null
    $windowsInspection = Join-Path $inspectionRoot "windows"
    Expand-Archive -LiteralPath $zipPath -DestinationPath $windowsInspection
    $reportedVersion = & (Join-Path $windowsInspection "gbaselite.exe") version
    if ($LASTEXITCODE -ne 0 -or $reportedVersion -ne "GBaseLite $Version") {
        throw "Packaged Windows binary reports an unexpected version: $reportedVersion"
    }

    foreach ($architecture in @("amd64", "arm64")) {
        $archive = Join-Path $OutputDirectory "gbaselite-linux-$architecture.tar.gz"
        $entries = & tar.exe -tzf $archive
        if ($LASTEXITCODE -ne 0) {
            throw "Unable to inspect $archive"
        }
        if ($entries -match '(^|/)(data|logs|bin|\.tmp|release)(/|$)|(^|/)(config\.yaml|\.env|temp\.env|gbaselite\.pid)$') {
            throw "$archive contains a forbidden runtime path"
        }
        $listing = & tar.exe -tvzf $archive
        foreach ($name in @("gbaselite", "start.sh", "stop.sh", "restart.sh")) {
            $line = $listing | Where-Object { $_ -match "\s$([regex]::Escape($name))$" } | Select-Object -First 1
            if ($null -eq $line -or $line -notmatch '^-rwxr-xr-x') {
                throw "$name is not executable in $archive"
            }
        }
    }

    $msiPath = Join-Path $OutputDirectory "GBaseLite-windows-amd64.msi"
    $installer = $null
    $database = $null
    try {
        $installer = New-Object -ComObject WindowsInstaller.Installer
        $database = $installer.GetType().InvokeMember("OpenDatabase", [System.Reflection.BindingFlags]::InvokeMethod, $null, $installer, [object[]]@([string]$msiPath, [int]0))
        $propertyRows = Read-MsiRows -Database $database -Query "SELECT ``Property``, ``Value`` FROM ``Property`` WHERE ``Property`` = 'ProductVersion' OR ``Property`` = 'ProductLanguage' OR ``Property`` = 'UpgradeCode' OR ``Property`` = 'SecureCustomProperties' OR ``Property`` = 'MsiHiddenProperties' OR ``Property`` = 'ARPPRODUCTICON' OR ``Property`` = 'ARPNOREPAIR' OR ``Property`` = 'ARPNOMODIFY' OR ``Property`` = 'GBASE_REINITIALIZE' OR ``Property`` = 'GBASE_REINITIALIZE_CONFIRMED' OR ``Property`` = 'GBASE_DESKTOP_SHORTCUT' OR ``Property`` = 'GBASE_CONFIGURE_JOURNALS' OR ``Property`` = 'GBASE_AUDIT_ENABLED' OR ``Property`` = 'GBASE_BINLOG_ENABLED' OR ``Property`` = 'GBASE_AUDIT_RETENTION_DAYS' OR ``Property`` = 'GBASE_BINLOG_RETENTION_DAYS'" -ColumnCount 2
        $properties = @{}
        foreach ($row in $propertyRows) {
            $properties[$row[0]] = $row[1]
        }
        if ($properties.ProductVersion -ne $Version -or $properties.ProductLanguage -ne "2052" -or $properties.UpgradeCode -ne "{75C0C2D1-6BB0-4C61-A4ED-F066B7427BE1}") {
            throw "MSI identity validation failed"
        }
        if ($properties.SecureCustomProperties -notmatch '(^|;)GBASE_ADMIN_PASSWORD(;|$)' -or
            $properties.SecureCustomProperties -notmatch '(^|;)GBASE_REINITIALIZE(;|$)' -or
            $properties.SecureCustomProperties -notmatch '(^|;)GBASE_REINITIALIZE_CONFIRMED(;|$)' -or
            $properties.SecureCustomProperties -notmatch '(^|;)GBASE_DESKTOP_SHORTCUT(;|$)' -or
            $properties.SecureCustomProperties -notmatch '(^|;)GBASE_CONFIGURE_JOURNALS(;|$)' -or
            $properties.SecureCustomProperties -notmatch '(^|;)GBASE_AUDIT_ENABLED(;|$)' -or
            $properties.SecureCustomProperties -notmatch '(^|;)GBASE_BINLOG_ENABLED(;|$)' -or
            $properties.SecureCustomProperties -notmatch '(^|;)GBASE_AUDIT_RETENTION_DAYS(;|$)' -or
            $properties.SecureCustomProperties -notmatch '(^|;)GBASE_BINLOG_RETENTION_DAYS(;|$)' -or
            $properties.MsiHiddenProperties -notmatch '(^|;)GBASE_ADMIN_PASSWORD(;|$)') {
            throw "MSI password properties are not secure and hidden"
        }
        if ($properties.ContainsKey("GBASE_REINITIALIZE") -or $properties.ContainsKey("GBASE_REINITIALIZE_CONFIRMED")) {
            throw "MSI data reinitialization must be disabled by default"
        }
        if ($properties.ContainsKey("GBASE_DESKTOP_SHORTCUT")) {
            throw "MSI desktop shortcut creation must be unchecked by default"
        }
        if ($properties.ContainsKey("GBASE_CONFIGURE_JOURNALS") -or
            $properties.ContainsKey("GBASE_AUDIT_ENABLED") -or
            $properties.ContainsKey("GBASE_BINLOG_ENABLED")) {
            throw "MSI audit and binlog options must be unchecked by default"
        }
        if ($properties.GBASE_AUDIT_RETENTION_DAYS -ne "7" -or $properties.GBASE_BINLOG_RETENTION_DAYS -ne "7") {
            throw "MSI audit and binlog retention must default to 7 days"
        }
        if ($properties.ContainsKey("ARPNOREPAIR") -or $properties.ContainsKey("ARPNOMODIFY")) {
            throw "MSI maintenance repair and change must remain enabled"
        }
        if ($properties.ARPPRODUCTICON -ne "GBaseLiteApplicationIcon") {
            throw "MSI does not register the GBaseLite application icon"
        }
        $environmentRows = Read-MsiRows -Database $database -Query "SELECT ``Name``, ``Value``, ``Component_`` FROM ``Environment``" -ColumnCount 3
        if (-not ($environmentRows | Where-Object { $_[0] -match 'Path$' -and $_[1] -eq '[~];[INSTALLFOLDER]' -and $_[2] -eq 'PathEnvironmentComponent' })) {
            throw "MSI does not append INSTALLFOLDER to the system Path"
        }
        $serviceRows = Read-MsiRows -Database $database -Query "SELECT ``Name``, ``Component_`` FROM ``ServiceInstall``" -ColumnCount 2
        if (-not ($serviceRows | Where-Object { $_[0] -eq 'GBaseLite' -and $_[1] -eq 'ProgramFilesComponent' })) {
            throw "MSI does not install the GBaseLite Windows service"
        }
        $binaryRows = Read-MsiRows -Database $database -Query "SELECT ``Name`` FROM ``Binary``" -ColumnCount 1
        foreach ($binaryName in @("GBaseCustomBanner", "WixUI_Bmp_Banner", "WixUI_Bmp_Dialog")) {
            if (-not ($binaryRows | Where-Object { $_[0] -eq $binaryName })) {
                throw "MSI is missing branded UI binary $binaryName"
            }
        }
        $iconRows = Read-MsiRows -Database $database -Query "SELECT ``Name`` FROM ``Icon``" -ColumnCount 1
        if (-not ($iconRows | Where-Object { $_[0] -eq 'GBaseLiteApplicationIcon' })) {
            throw "MSI application icon was not embedded"
        }
        $fileRows = Read-MsiRows -Database $database -Query "SELECT ``File``, ``FileName``, ``Component_`` FROM ``File`` WHERE ``File`` = 'GBaseLiteIconFile'" -ColumnCount 3
        if (-not ($fileRows | Where-Object { $_[0] -eq 'GBaseLiteIconFile' -and $_[1] -match 'gbaselite\.ico$' -and $_[2] -eq 'ProgramFilesComponent' })) {
            throw "MSI does not install gbaselite.ico with the program files"
        }
        $shortcutRows = Read-MsiRows -Database $database -Query "SELECT ``Shortcut``, ``Directory_``, ``Component_``, ``Target``, ``Icon_`` FROM ``Shortcut``" -ColumnCount 5
        if (-not ($shortcutRows | Where-Object { $_[0] -eq 'GBaseLiteStartMenuShortcut' -and $_[1] -eq 'GBaseLiteMenuFolder' -and $_[3] -eq '[#GBaseLiteExecutable]' -and $_[4] -eq 'GBaseLiteApplicationIcon' })) {
            throw "MSI does not install the branded Start menu shortcut"
        }
        if (-not ($shortcutRows | Where-Object { $_[0] -eq 'GBaseLiteDesktopShortcut' -and $_[1] -eq 'DesktopFolder' -and $_[3] -eq '[#GBaseLiteExecutable]' -and $_[4] -eq 'GBaseLiteApplicationIcon' })) {
            throw "MSI does not provide the optional branded desktop shortcut"
        }
        $desktopComponentRows = Read-MsiRows -Database $database -Query "SELECT ``Component``, ``Condition`` FROM ``Component`` WHERE ``Component`` = 'DesktopShortcutComponent'" -ColumnCount 2
        if (-not ($desktopComponentRows | Where-Object { $_[0] -eq 'DesktopShortcutComponent' -and [string]::IsNullOrEmpty($_[1]) })) {
            throw "MSI desktop shortcut component must be controlled by its optional feature"
        }
        $featureRows = Read-MsiRows -Database $database -Query "SELECT ``Feature``, ``Feature_Parent``, ``Level``, ``Display`` FROM ``Feature`` WHERE ``Feature`` = 'MainFeature' OR ``Feature`` = 'DesktopShortcutFeature'" -ColumnCount 4
        $mainFeature = $featureRows | Where-Object { $_[0] -eq 'MainFeature' } | Select-Object -First 1
        $desktopFeature = $featureRows | Where-Object { $_[0] -eq 'DesktopShortcutFeature' } | Select-Object -First 1
        if ($null -eq $mainFeature -or [int]$mainFeature[3] -eq 0 -or
            $null -eq $desktopFeature -or $desktopFeature[1] -ne 'MainFeature' -or
            [int]$desktopFeature[2] -ne 2 -or [int]$desktopFeature[3] -eq 0) {
            throw "MSI change mode requires a visible optional desktop shortcut feature"
        }
        $featureComponentRows = Read-MsiRows -Database $database -Query "SELECT ``Feature_``, ``Component_`` FROM ``FeatureComponents`` WHERE ``Component_`` = 'DesktopShortcutComponent'" -ColumnCount 2
        if (-not ($featureComponentRows | Where-Object { $_[0] -eq 'DesktopShortcutFeature' -and $_[1] -eq 'DesktopShortcutComponent' })) {
            throw "MSI desktop shortcut component is not owned by its optional feature"
        }
        $uiControlRows = Read-MsiRows -Database $database -Query "SELECT ``Dialog_``, ``Control``, ``Type``, ``Property``, ``Text`` FROM ``Control`` WHERE ``Dialog_`` = 'GBaseConfigDlg' OR ``Dialog_`` = 'GBaseJournalDlg' OR ``Dialog_`` = 'GBaseReinitializeConfirmDlg' OR ``Dialog_`` = 'LicenseAgreementDlg'" -ColumnCount 5
        if (-not ($uiControlRows | Where-Object { $_[0] -eq 'GBaseConfigDlg' -and $_[1] -eq 'BannerBitmap' -and $_[2] -eq 'Bitmap' -and $_[4] -eq 'GBaseCustomBanner' }) -or
            -not ($uiControlRows | Where-Object { $_[0] -eq 'GBaseJournalDlg' -and $_[1] -eq 'BannerBitmap' -and $_[2] -eq 'Bitmap' -and $_[4] -eq 'GBaseCustomBanner' }) -or
            -not ($uiControlRows | Where-Object { $_[0] -eq 'GBaseJournalDlg' -and $_[1] -eq 'AuditCheck' -and $_[2] -eq 'CheckBox' -and $_[3] -eq 'GBASE_AUDIT_ENABLED' }) -or
            -not ($uiControlRows | Where-Object { $_[0] -eq 'GBaseJournalDlg' -and $_[1] -eq 'BinlogCheck' -and $_[2] -eq 'CheckBox' -and $_[3] -eq 'GBASE_BINLOG_ENABLED' }) -or
            -not ($uiControlRows | Where-Object { $_[0] -eq 'GBaseJournalDlg' -and $_[1] -eq 'AuditRetentionEdit' -and $_[2] -eq 'Edit' -and $_[3] -eq 'GBASE_AUDIT_RETENTION_DAYS' }) -or
            -not ($uiControlRows | Where-Object { $_[0] -eq 'GBaseJournalDlg' -and $_[1] -eq 'BinlogRetentionEdit' -and $_[2] -eq 'Edit' -and $_[3] -eq 'GBASE_BINLOG_RETENTION_DAYS' }) -or
            -not ($uiControlRows | Where-Object { $_[0] -eq 'GBaseReinitializeConfirmDlg' -and $_[1] -eq 'BannerBitmap' -and $_[2] -eq 'Bitmap' -and $_[4] -eq 'GBaseCustomBanner' }) -or
            -not ($uiControlRows | Where-Object { $_[0] -eq 'GBaseConfigDlg' -and $_[1] -eq 'DesktopShortcutCheck' -and $_[2] -eq 'CheckBox' -and $_[3] -eq 'GBASE_DESKTOP_SHORTCUT' }) -or
            -not ($uiControlRows | Where-Object { $_[0] -eq 'LicenseAgreementDlg' -and $_[1] -eq 'LicenseText' -and $_[2] -eq 'ScrollableText' -and $_[4] -match 'GBaseLite' -and $_[4] -match '\\u-?[0-9]+\?' })) {
            throw "MSI dialogs do not contain the Chinese license, branded banners, and optional desktop shortcut control"
        }
        $componentRows = Read-MsiRows -Database $database -Query "SELECT ``Component``, ``Attributes`` FROM ``Component`` WHERE ``Component`` = 'InstallDirectoryRegistryComponent' OR ``Component`` = 'DataDirectoryComponent' OR ``Component`` = 'LogDirectoryComponent'" -ColumnCount 2
        $installRegistryComponent = $componentRows | Where-Object { $_[0] -eq "InstallDirectoryRegistryComponent" } | Select-Object -First 1
        if ($null -eq $installRegistryComponent -or (([int]$installRegistryComponent[1]) -band 16) -eq 0) {
            throw "MSI does not permanently remember the selected installation directory"
        }
        foreach ($componentName in @("DataDirectoryComponent", "LogDirectoryComponent")) {
            $component = $componentRows | Where-Object { $_[0] -eq $componentName } | Select-Object -First 1
            if ($null -eq $component) {
                throw "MSI is missing preserve-data component $componentName"
            }
            $attributes = [int]$component[1]
            if (($attributes -band 16) -eq 0 -or ($attributes -band 128) -eq 0) {
                throw "MSI component $componentName is not permanent and never-overwrite"
            }
        }
        $sequenceRows = Read-MsiRows -Database $database -Query "SELECT ``Action``, ``Condition`` FROM ``InstallExecuteSequence`` WHERE ``Action`` = 'FinalizeInstallation'" -ColumnCount 2
        if (-not ($sequenceRows | Where-Object { $_[1] -match 'REMOVE' -and $_[1] -match 'ALL' })) {
            throw "MSI finalization is not excluded from uninstall"
        }
        $appSearchRows = Read-MsiRows -Database $database -Query "SELECT ``Property``, ``Signature_`` FROM ``AppSearch`` WHERE ``Property`` = 'GBASE_EXISTING_INSTALL_DIR' OR ``Property`` = 'GBASE_DATA_DIR' OR ``Property`` = 'GBASE_LOG_DIR' OR ``Property`` = 'GBASE_DESKTOP_SHORTCUT'" -ColumnCount 2
        if (-not ($appSearchRows | Where-Object { $_[0] -eq 'GBASE_EXISTING_INSTALL_DIR' -and $_[1] -eq 'FindExistingInstallDirectory' }) -or
            -not ($appSearchRows | Where-Object { $_[0] -eq 'GBASE_DATA_DIR' -and $_[1] -eq 'FindExistingDataDirectory' }) -or
            -not ($appSearchRows | Where-Object { $_[0] -eq 'GBASE_LOG_DIR' -and $_[1] -eq 'FindExistingLogDirectory' }) -or
            -not ($appSearchRows | Where-Object { $_[0] -eq 'GBASE_DESKTOP_SHORTCUT' -and $_[1] -eq 'FindExistingDesktopShortcut' })) {
            throw "MSI upgrades do not restore registered directories and desktop shortcut state"
        }
        $installDirectoryLocatorRows = Read-MsiRows -Database $database -Query "SELECT ``Signature_``, ``Type`` FROM ``RegLocator`` WHERE ``Signature_`` = 'FindExistingInstallDirectory'" -ColumnCount 2
        $installDirectoryLocator = $installDirectoryLocatorRows | Select-Object -First 1
        if ($null -eq $installDirectoryLocator -or (([int]$installDirectoryLocator[1]) -band 2) -eq 0) {
            throw "MSI must restore the persisted installation directory as a raw registry value"
        }
        $registryRows = Read-MsiRows -Database $database -Query "SELECT ``Name``, ``Value``, ``Component_`` FROM ``Registry`` WHERE ``Name`` = 'InstallDirectory'" -ColumnCount 3
        if (-not ($registryRows | Where-Object { $_[0] -eq 'InstallDirectory' -and $_[1] -eq '[INSTALLFOLDER]' -and $_[2] -eq 'InstallDirectoryRegistryComponent' })) {
            throw "MSI does not persist the selected installation directory"
        }
        $customActionRows = Read-MsiRows -Database $database -Query "SELECT ``Action``, ``Source``, ``Target`` FROM ``CustomAction`` WHERE ``Action`` = 'UseExistingInstallFolder'" -ColumnCount 3
        if (-not ($customActionRows | Where-Object { $_[0] -eq 'UseExistingInstallFolder' -and $_[1] -eq 'INSTALLFOLDER' -and $_[2] -eq '[GBASE_EXISTING_INSTALL_DIR]' })) {
            throw "MSI does not apply the previously registered installation directory"
        }
        $installUIRows = Read-MsiRows -Database $database -Query "SELECT ``Action``, ``Condition`` FROM ``InstallUISequence`` WHERE ``Action`` = 'UseExistingInstallFolder'" -ColumnCount 2
        $installExecuteRows = Read-MsiRows -Database $database -Query "SELECT ``Action``, ``Condition`` FROM ``InstallExecuteSequence`` WHERE ``Action`` = 'UseExistingInstallFolder'" -ColumnCount 2
        if (-not ($installUIRows | Where-Object { $_[1] -eq 'GBASE_EXISTING_INSTALL_DIR' }) -or
            -not ($installExecuteRows | Where-Object { $_[1] -eq 'GBASE_EXISTING_INSTALL_DIR' })) {
            throw "MSI does not restore the installation directory in UI and silent installs"
        }
        $controlEventRows = Read-MsiRows -Database $database -Query "SELECT ``Dialog_``, ``Control_``, ``Event``, ``Argument``, ``Condition`` FROM ``ControlEvent`` WHERE ``Dialog_`` = 'InstallDirDlg' OR ``Dialog_`` = 'GBaseConfigDlg' OR ``Dialog_`` = 'GBaseJournalDlg' OR ``Dialog_`` = 'GBaseReinitializeConfirmDlg' OR ``Dialog_`` = 'MaintenanceTypeDlg'" -ColumnCount 5
        if (-not ($controlEventRows | Where-Object { $_[0] -eq 'InstallDirDlg' -and $_[1] -eq 'Next' -and $_[2] -eq 'NewDialog' -and $_[3] -eq 'GBaseConfigDlg' -and $_[4] -eq 'NOT Installed' })) {
            throw "MSI major upgrades do not show the data-preservation configuration page"
        }
        if (-not ($controlEventRows | Where-Object { $_[0] -eq 'GBaseConfigDlg' -and $_[1] -eq 'Next' -and $_[2] -eq 'NewDialog' -and $_[3] -eq 'GBaseJournalDlg' }) -or
            -not ($controlEventRows | Where-Object { $_[0] -eq 'GBaseJournalDlg' -and $_[1] -eq 'Next' -and $_[2] -eq 'SpawnDialog' -and $_[3] -eq 'GBaseInvalidRetentionDlg' -and $_[4] -match '365' }) -or
            -not ($controlEventRows | Where-Object { $_[0] -eq 'GBaseJournalDlg' -and $_[1] -eq 'Next' -and $_[2] -eq '[GBASE_CONFIGURE_JOURNALS]' -and $_[3] -eq '1' }) -or
            -not ($controlEventRows | Where-Object { $_[0] -eq 'GBaseJournalDlg' -and $_[1] -eq 'Next' -and $_[2] -eq 'NewDialog' -and $_[3] -eq 'GBaseReinitializeConfirmDlg' -and $_[4] -match 'GBASE_REINITIALIZE = "1"' }) -or
            -not ($controlEventRows | Where-Object { $_[0] -eq 'GBaseJournalDlg' -and $_[1] -eq 'Next' -and $_[2] -eq 'NewDialog' -and $_[3] -eq 'VerifyReadyDlg' -and $_[4] -match 'GBASE_REINITIALIZE' })) {
            throw "MSI audit/binlog page and data reinitialization routing are incomplete"
        }
        if (-not ($controlEventRows | Where-Object { $_[0] -eq 'GBaseReinitializeConfirmDlg' -and $_[1] -eq 'Confirm' -and $_[2] -eq '[GBASE_REINITIALIZE_CONFIRMED]' -and $_[3] -eq '1' })) {
            throw "MSI data reinitialization confirmation does not set the confirmation property"
        }
        if (-not ($controlEventRows | Where-Object { $_[0] -eq 'GBaseReinitializeConfirmDlg' -and $_[1] -eq 'Back' -and $_[2] -eq 'NewDialog' -and $_[3] -eq 'GBaseJournalDlg' })) {
            throw "MSI data reinitialization confirmation does not return to the audit/binlog page"
        }
        if (-not ($controlEventRows | Where-Object { $_[0] -eq 'GBaseConfigDlg' -and $_[1] -eq 'Next' -and $_[2] -eq 'AddLocal' -and $_[3] -eq 'DesktopShortcutFeature' -and $_[4] -eq 'GBASE_DESKTOP_SHORTCUT = "1"' }) -or
            -not ($controlEventRows | Where-Object { $_[0] -eq 'GBaseConfigDlg' -and $_[1] -eq 'Next' -and $_[2] -eq 'Remove' -and $_[3] -eq 'DesktopShortcutFeature' -and $_[4] -eq 'GBASE_DESKTOP_SHORTCUT <> "1"' }) -or
            -not ($controlEventRows | Where-Object { $_[0] -eq 'MaintenanceTypeDlg' -and $_[1] -eq 'ChangeButton' -and $_[2] -eq 'NewDialog' -and $_[3] -eq 'GBaseConfigDlg' }) -or
            -not ($controlEventRows | Where-Object { $_[0] -eq 'MaintenanceTypeDlg' -and $_[1] -eq 'RepairButton' -and $_[2] -eq 'NewDialog' -and $_[3] -eq 'VerifyReadyDlg' })) {
            throw "MSI maintenance change and repair flows are incomplete"
        }
    } finally {
        if ($null -ne $database) {
            [void][System.Runtime.InteropServices.Marshal]::FinalReleaseComObject($database)
        }
        if ($null -ne $installer) {
            [void][System.Runtime.InteropServices.Marshal]::FinalReleaseComObject($installer)
        }
        [GC]::Collect()
        [GC]::WaitForPendingFinalizers()
    }
}

function Copy-ReleaseToDist {
    param(
        [Parameter(Mandatory)][string]$SourceDirectory,
        [Parameter(Mandatory)][string]$Version
    )

    $releaseRoot = Join-Path $distRoot "GBaseLite-$Version"
    New-Item -ItemType Directory -Path $releaseRoot -Force | Out-Null
    $artifacts = @(
        "GBaseLite-windows-amd64.msi",
        "gbaselite-windows-amd64.zip",
        "gbaselite-linux-amd64.tar.gz",
        "gbaselite-linux-arm64.tar.gz",
        "gbaselite-linux-amd64",
        "gbaselite-linux-arm64",
        "checksums.txt"
    )
    $sbom = Join-Path $SourceDirectory "sbom.spdx.json"
    if (Test-Path -LiteralPath $sbom -PathType Leaf) {
        $artifacts += "sbom.spdx.json"
    } elseif (Test-Path -LiteralPath (Join-Path $releaseRoot "sbom.spdx.json") -PathType Leaf) {
        Remove-Item -LiteralPath (Join-Path $releaseRoot "sbom.spdx.json") -Force
    }
    foreach ($name in $artifacts) {
        Copy-Item -LiteralPath (Join-Path $SourceDirectory $name) -Destination (Join-Path $releaseRoot $name) -Force
    }
    return $releaseRoot
}

if ($SelfTest) {
    $cases = @{
        "0.9.0" = "1.0.0"
        "1.0.0" = "1.0.001"
        "1.0.001" = "1.0.002"
        "1.0.999" = "1.1.0"
        "1.1.0" = "1.1.001"
        "1.2.009" = "1.2.010"
    }
    foreach ($current in $cases.Keys) {
        $actual = Get-NextVersion $current
        if ($actual -ne $cases[$current]) {
            throw "Version self-test failed: $current -> $actual, expected $($cases[$current])"
        }
    }
    $nativeOutput = Invoke-NativeCommand -Executable $env:ComSpec -Arguments @(
        "/d", "/c", "echo one-click release native output self-test"
    ) -FailureMessage "Native output self-test failed"
    if ($null -ne $nativeOutput) {
        throw "Native command output leaked into a resolver return value"
    }
    Write-Host "One-click release self-test passed."
    exit 0
}

$currentVersion = Get-CurrentVersion
$nextVersion = if ([string]::IsNullOrWhiteSpace($TargetVersion)) {
    Get-NextVersion $currentVersion
} else {
    if ($TargetVersion -notmatch '^(\d+)\.(\d+)\.(\d+)$') {
        throw "Target version must use numeric major.minor.revision format: $TargetVersion"
    }
    $revision = ([regex]::Match($TargetVersion, '^(\d+)\.(\d+)\.(\d+)$')).Groups[3].Value
    if ([int]$revision -ne 0 -and $revision.Length -lt 3) {
        throw "Non-zero target revisions must use at least three digits: $TargetVersion"
    }
    $TargetVersion
}
Write-Host "Current version: $currentVersion"
Write-Host "Target version:  $nextVersion"
if ($Preview) {
    Write-Host "Preview only; no files were changed."
    exit 0
}

$versionFiles = @(
    (Join-Path $repositoryRoot "executor\executor.go"),
    (Join-Path $repositoryRoot "README.md"),
    (Join-Path $dockerSourceRoot "docker-compose.yml"),
    (Join-Path $dockerSourceRoot "docker-compose.binary.yml"),
    (Join-Path $dockerSourceRoot "temp.env.example"),
    (Join-Path $repositoryRoot ".github\workflows\release.yml"),
    (Join-Path $repositoryRoot "CHANGELOG.md")
)
$originalFiles = @{}
foreach ($path in $versionFiles) {
    if (-not (Test-Path -LiteralPath $path -PathType Leaf)) {
        throw "Required version file is missing: $path"
    }
    $originalFiles[$path] = [System.IO.File]::ReadAllBytes($path)
}
foreach ($name in @("Dockerfile", "entrypoint.sh", "docker-compose.build.yml", "docker-compose.binary.yml", "config.example.yaml", "temp.env.example")) {
    $path = Join-Path $dockerSourceRoot $name
    if (-not (Test-Path -LiteralPath $path -PathType Leaf)) {
        throw "Required Docker release file is missing: $path"
    }
}

New-Item -ItemType Directory -Path $temporaryRoot -Force | Out-Null
$candidateRoot = Join-Path $temporaryRoot ("one-click-release-$nextVersion-" + [guid]::NewGuid().ToString("N"))
$candidateOutput = Join-Path $candidateRoot "output"
$versionUpdated = $false
$releaseSucceeded = $false
$previousPath = $env:PATH
$previousDotnetRoot = $env:DOTNET_ROOT

try {
    $versionUpdated = $true
    Replace-RequiredVersion -Path (Join-Path $repositoryRoot "executor\executor.go") -OldVersion $currentVersion -NewVersion $nextVersion
    Replace-RequiredVersion -Path (Join-Path $repositoryRoot "README.md") -OldVersion $currentVersion -NewVersion $nextVersion
    Replace-RequiredVersion -Path (Join-Path $dockerSourceRoot "docker-compose.yml") -OldVersion $currentVersion -NewVersion $nextVersion
    Replace-RequiredVersion -Path (Join-Path $dockerSourceRoot "temp.env.example") -OldVersion $currentVersion -NewVersion $nextVersion
    Update-BinaryReleasePaths -Version $nextVersion
    Replace-RequiredVersion -Path (Join-Path $repositoryRoot ".github\workflows\release.yml") -OldVersion $currentVersion -NewVersion $nextVersion
    Update-Changelog -Path (Join-Path $repositoryRoot "CHANGELOG.md") -Version $nextVersion

    $goExecutable = Resolve-GoExecutable
    $dotnetExecutable = Resolve-DotnetExecutable
    $wixExecutable = Resolve-WixExecutable -DotnetExecutable $dotnetExecutable
    $wixUIExtension = Resolve-WixUIExtension -WixExecutable $wixExecutable
    $env:DOTNET_ROOT = Split-Path -Parent $dotnetExecutable
    $env:PATH = (Split-Path -Parent $wixExecutable) + ";" + $env:DOTNET_ROOT + ";" + $previousPath

    Test-ComposeConfiguration

    New-Item -ItemType Directory -Path $candidateOutput -Force | Out-Null
    Write-Host "Building and testing GBaseLite $nextVersion..."
    & (Join-Path $PSScriptRoot "build-release.ps1") `
        -Version $nextVersion `
        -OutputDirectory $candidateOutput `
        -GoExecutable $goExecutable `
        -WixExecutable $wixExecutable `
        -WixUIExtension $wixUIExtension
    if ($LASTEXITCODE -ne 0) {
        throw "The release build failed"
    }

    Test-ReleaseCandidate -OutputDirectory $candidateOutput -Version $nextVersion
    $publishedDirectory = Copy-ReleaseToDist -SourceDirectory $candidateOutput -Version $nextVersion

    $pidPath = Join-Path $repositoryRoot "data\gbaselite.pid"
    if (Test-Path -LiteralPath $pidPath -PathType Leaf) {
        & (Join-Path $repositoryRoot "bin\gbaselite.exe") healthcheck --host 127.0.0.1 --port 3307
        if ($LASTEXITCODE -ne 0) {
            throw "The running GBaseLite service is no longer healthy"
        }
    }

    $releaseSucceeded = $true
    Write-Host ""
    Write-Host "GBaseLite $nextVersion was published to $publishedDirectory"
    Get-Content -LiteralPath (Join-Path $publishedDirectory "checksums.txt")
} catch {
    if ($versionUpdated) {
        foreach ($path in $originalFiles.Keys) {
            [System.IO.File]::WriteAllBytes($path, $originalFiles[$path])
        }
        Write-Warning "Version files were restored to $currentVersion."
    }
    Write-Warning "Failed candidate files were retained at $candidateRoot"
    throw
} finally {
    $env:PATH = $previousPath
    $env:DOTNET_ROOT = $previousDotnetRoot
    if ($releaseSucceeded -and (Test-Path -LiteralPath $candidateRoot)) {
        $resolvedCandidate = [System.IO.Path]::GetFullPath($candidateRoot)
        $allowedPrefix = $temporaryRoot.TrimEnd([System.IO.Path]::DirectorySeparatorChar) + [System.IO.Path]::DirectorySeparatorChar
        if ($resolvedCandidate.StartsWith($allowedPrefix, [System.StringComparison]::OrdinalIgnoreCase)) {
            Remove-Item -LiteralPath $resolvedCandidate -Recurse -Force
        }
    }
}
