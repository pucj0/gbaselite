[CmdletBinding()]
param(
    [Parameter(Position = 0)][string]$Version,
    [string]$SourceRef = "HEAD",
    [string]$GitRepository = "https://github.com/pucj0/gbaselite.git",
    [string]$DockerRepository = "pucj/gbaselite",
    [switch]$DryRun,
    [switch]$PrepareOnly,
    [switch]$Publish,
    [switch]$ReplaceArtifacts,
    [switch]$KeepWorktree,
    [switch]$SelfTest
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
$expectedGitRepository = "https://github.com/pucj0/gbaselite.git"
$expectedDockerRepository = "pucj/gbaselite"

function Assert-ReleaseVersion {
    param([Parameter(Mandatory)][string]$Value)

    $match = [regex]::Match($Value, '^(\d+)\.(\d+)\.(\d+)$')
    if (-not $match.Success) {
        throw "Version must use numeric major.minor.revision format: $Value"
    }
    $revision = $match.Groups[3].Value
    if ([int]$revision -ne 0 -and $revision.Length -lt 3) {
        throw "Non-zero revisions must use at least three digits: $Value"
    }
}

function Invoke-CapturedCommand {
    param(
        [Parameter(Mandatory)][string]$Executable,
        [Parameter(Mandatory)][string[]]$Arguments,
        [string]$WorkingDirectory = $repositoryRoot
    )

    $previousPreference = $ErrorActionPreference
    Push-Location $WorkingDirectory
    try {
        $ErrorActionPreference = "Continue"
        $output = @(& $Executable @Arguments 2>&1)
        $exitCode = $LASTEXITCODE
    } finally {
        $ErrorActionPreference = $previousPreference
        Pop-Location
    }
    return [pscustomobject]@{
        ExitCode = $exitCode
        Output   = [object[]]$output
    }
}

function Invoke-CheckedCommand {
    param(
        [Parameter(Mandatory)][string]$Executable,
        [Parameter(Mandatory)][string[]]$Arguments,
        [Parameter(Mandatory)][string]$FailureMessage,
        [string]$WorkingDirectory = $repositoryRoot
    )

    $result = Invoke-CapturedCommand -Executable $Executable -Arguments $Arguments -WorkingDirectory $WorkingDirectory
    if (@($result.Output).Count -gt 0) {
        $result.Output | Out-Host
    }
    if ($result.ExitCode -ne 0) {
        throw "$FailureMessage (exit code $($result.ExitCode))"
    }
}

function Get-RequiredOutput {
    param(
        [Parameter(Mandatory)][string]$Executable,
        [Parameter(Mandatory)][string[]]$Arguments,
        [Parameter(Mandatory)][string]$FailureMessage,
        [string]$WorkingDirectory = $repositoryRoot
    )

    $result = Invoke-CapturedCommand -Executable $Executable -Arguments $Arguments -WorkingDirectory $WorkingDirectory
    if ($result.ExitCode -ne 0) {
        if (@($result.Output).Count -gt 0) {
            $result.Output | Out-Host
        }
        throw "$FailureMessage (exit code $($result.ExitCode))"
    }
    $lines = @($result.Output | ForEach-Object { $_.ToString() })
    return ,$lines
}

function Test-LocalGitRef {
    param(
        [Parameter(Mandatory)][string]$GitExecutable,
        [Parameter(Mandatory)][string]$Reference
    )

    $result = Invoke-CapturedCommand -Executable $GitExecutable -Arguments @(
        "-C", $repositoryRoot, "show-ref", "--verify", "--quiet", $Reference
    )
    if ($result.ExitCode -eq 0) {
        return $true
    }
    if ($result.ExitCode -eq 1) {
        return $false
    }
    throw "Unable to inspect local Git reference $Reference"
}

function Assert-SafeTemporaryPath {
    param([Parameter(Mandatory)][string]$Path)

    $resolved = [System.IO.Path]::GetFullPath($Path)
    $allowedPrefix = $temporaryRoot.TrimEnd([System.IO.Path]::DirectorySeparatorChar) +
        [System.IO.Path]::DirectorySeparatorChar
    if (-not $resolved.StartsWith($allowedPrefix, [System.StringComparison]::OrdinalIgnoreCase)) {
        throw "Unsafe temporary path: $resolved"
    }
    return $resolved
}

function Assert-SafeReleaseDestination {
    param(
        [Parameter(Mandatory)][string]$Path,
        [Parameter(Mandatory)][string]$ReleaseVersion
    )

    $resolved = [System.IO.Path]::GetFullPath($Path)
    $expected = [System.IO.Path]::GetFullPath((Join-Path $distRoot "GBaseLite-$ReleaseVersion"))
    if (-not $resolved.Equals($expected, [System.StringComparison]::OrdinalIgnoreCase)) {
        throw "Unsafe release destination: $resolved"
    }
    return $resolved
}

function Copy-ReleaseArtifacts {
    param(
        [Parameter(Mandatory)][string]$Source,
        [Parameter(Mandatory)][string]$Destination,
        [Parameter(Mandatory)][string]$StagingDirectory,
        [switch]$Replace
    )

    $stagedRelease = Join-Path $StagingDirectory "artifacts"
    Copy-Item -LiteralPath $Source -Destination $stagedRelease -Recurse
    if (Test-Path -LiteralPath $Destination) {
        if (-not $Replace) {
            throw "Release output already exists: $Destination. Use -ReplaceArtifacts only after verifying it can be replaced."
        }
        Remove-Item -LiteralPath $Destination -Recurse -Force
    }
    New-Item -ItemType Directory -Path (Split-Path -Parent $Destination) -Force | Out-Null
    Move-Item -LiteralPath $stagedRelease -Destination $Destination
}

function Test-DockerImage {
    param(
        [Parameter(Mandatory)][string]$DockerExecutable,
        [Parameter(Mandatory)][string]$Worktree,
        [Parameter(Mandatory)][string]$ReleaseVersion,
        [Parameter(Mandatory)][string]$Identifier
    )

    $image = "gbaselite-release-verify:$ReleaseVersion-$Identifier"
    $container = "gbaselite-release-verify-$Identifier"
    try {
        Invoke-CheckedCommand -Executable $DockerExecutable -Arguments @(
            "buildx", "build",
            "--platform", "linux/amd64",
            "--load",
            "--tag", $image,
            "--file", (Join-Path $Worktree "docker\Dockerfile"),
            $Worktree
        ) -FailureMessage "Unable to build the local Docker verification image"

        Invoke-CheckedCommand -Executable $DockerExecutable -Arguments @(
            "run", "--detach", "--rm",
            "--name", $container,
            "--env", "DB_PASSWORD=release-verification-password",
            $image
        ) -FailureMessage "Unable to start the Docker verification container"

        $deadline = [DateTime]::UtcNow.AddSeconds(90)
        while ([DateTime]::UtcNow -lt $deadline) {
            $healthResult = Invoke-CapturedCommand -Executable $DockerExecutable -Arguments @(
                "inspect", "--format", "{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}", $container
            )
            if ($healthResult.ExitCode -ne 0) {
                throw "The Docker verification container stopped before becoming healthy"
            }
            $health = ($healthResult.Output | Select-Object -First 1).ToString().Trim()
            if ($health -eq "healthy") {
                $runtimeUID = Get-RequiredOutput -Executable $DockerExecutable -Arguments @(
                    "exec", $container, "sh", "-c",
                    "sed -n 's/^Uid:[[:space:]]*\([0-9]*\).*/\1/p' /proc/1/status"
                ) -FailureMessage "Unable to inspect the Docker database process UID"
                if ($runtimeUID.Count -ne 1 -or $runtimeUID[0].Trim() -ne "10001") {
                    throw "The Docker database process must run as UID 10001"
                }
                Write-Host "Docker verification container is healthy."
                return
            }
            if ($health -eq "unhealthy" -or $health -eq "exited" -or $health -eq "dead") {
                $logs = Invoke-CapturedCommand -Executable $DockerExecutable -Arguments @("logs", $container)
                if (@($logs.Output).Count -gt 0) {
                    $logs.Output | Out-Host
                }
                throw "Docker verification container entered state $health"
            }
            Start-Sleep -Seconds 2
        }
        throw "Docker verification container did not become healthy within 90 seconds"
    } finally {
        $containerCheck = Invoke-CapturedCommand -Executable $DockerExecutable -Arguments @(
            "container", "inspect", $container
        )
        if ($containerCheck.ExitCode -eq 0) {
            [void](Invoke-CapturedCommand -Executable $DockerExecutable -Arguments @("rm", "--force", $container))
        }
        [void](Invoke-CapturedCommand -Executable $DockerExecutable -Arguments @("image", "rm", $image))
    }
}

function Invoke-DockerPush {
    param(
        [Parameter(Mandatory)][string]$DockerExecutable,
        [Parameter(Mandatory)][string]$Worktree,
        [Parameter(Mandatory)][string]$ReleaseVersion,
        [Parameter(Mandatory)][string]$Repository,
        [Parameter(Mandatory)][string]$Revision
    )

    $parts = $ReleaseVersion.Split(".")
    $majorMinor = "$($parts[0]).$($parts[1])"
    Invoke-CheckedCommand -Executable $DockerExecutable -Arguments @(
        "buildx", "build",
        "--platform", "linux/amd64,linux/arm64",
        "--push",
        "--tag", ("{0}:{1}" -f $Repository, $ReleaseVersion),
        "--tag", ("{0}:{1}" -f $Repository, $majorMinor),
        "--tag", ("{0}:latest" -f $Repository),
        "--label", "org.opencontainers.image.source=https://github.com/pucj0/gbaselite",
        "--label", "org.opencontainers.image.version=$ReleaseVersion",
        "--label", "org.opencontainers.image.revision=$Revision",
        "--file", (Join-Path $Worktree "docker\Dockerfile"),
        $Worktree
    ) -FailureMessage "Unable to build and push the Docker Hub release"

    $manifestLines = Get-RequiredOutput -Executable $DockerExecutable -Arguments @(
        "buildx", "imagetools", "inspect", "--raw", ("{0}:{1}" -f $Repository, $ReleaseVersion)
    ) -FailureMessage "Unable to inspect the pushed Docker Hub manifest"
    $manifest = (($manifestLines -join [Environment]::NewLine) | ConvertFrom-Json)
    $platforms = @($manifest.manifests | ForEach-Object {
        if ($null -ne $_.platform -and $_.platform.os -ne "unknown") {
            "$($_.platform.os)/$($_.platform.architecture)"
        }
    })
    foreach ($expectedPlatform in @("linux/amd64", "linux/arm64")) {
        if ($expectedPlatform -notin $platforms) {
            throw "Docker Hub manifest is missing $expectedPlatform"
        }
    }
}

function Assert-DockerfilePlatformArguments {
    param([Parameter(Mandatory)][string]$Path)

    $contents = [System.IO.File]::ReadAllText($Path)
    if ($contents -notmatch '(?m)^FROM --platform=\$BUILDPLATFORM\s+') {
        throw "Dockerfile builder stage must run on BUILDPLATFORM"
    }
    foreach ($argument in @("TARGETOS", "TARGETARCH")) {
        if ($contents -notmatch "(?m)^ARG $argument\s*$") {
            throw "Dockerfile must declare ARG $argument without a fixed default"
        }
        if ($contents -match "(?m)^ARG $argument\s*=") {
            throw "Dockerfile must not override the automatic $argument build argument"
        }
    }
    if ($contents -notmatch 'GOOS=\$\{TARGETOS\}' -or $contents -notmatch 'GOARCH=\$\{TARGETARCH\}') {
        throw "Dockerfile must build with the automatic TARGETOS and TARGETARCH values"
    }
}

function Invoke-SelfTest {
    Assert-DockerfilePlatformArguments -Path (Join-Path $repositoryRoot "docker\Dockerfile")
    foreach ($value in @("1.0.0", "1.0.001", "2.4.999")) {
        Assert-ReleaseVersion $value
    }
    foreach ($value in @("1", "1.0", "v1.0.0", "1.0.1", "1.0.abc")) {
        $failed = $false
        try {
            Assert-ReleaseVersion $value
        } catch {
            $failed = $true
        }
        if (-not $failed) {
            throw "Self-test expected an invalid version: $value"
        }
    }
    if ($expectedGitRepository -ne "https://github.com/pucj0/gbaselite.git") {
        throw "Unexpected GitHub repository default"
    }
    if ($expectedDockerRepository -ne "pucj/gbaselite") {
        throw "Unexpected Docker repository default"
    }
    Write-Host "Publish release self-test passed."
}

if ($SelfTest) {
    Invoke-SelfTest
    exit 0
}

if ([string]::IsNullOrWhiteSpace($Version)) {
    throw "Version is required. Example: publish-release.bat -Version 1.0.0 -DryRun"
}
Assert-ReleaseVersion $Version

$modeCount = 0
if ($DryRun) { $modeCount++ }
if ($PrepareOnly) { $modeCount++ }
if ($Publish) { $modeCount++ }
if ($modeCount -ne 1) {
    throw "Choose exactly one mode: -DryRun, -PrepareOnly, or -Publish"
}
if ($GitRepository -ne $expectedGitRepository) {
    throw "This release script is bound to $expectedGitRepository"
}
if ($DockerRepository -ne $expectedDockerRepository) {
    throw "This release script is bound to $expectedDockerRepository"
}

$git = Get-Command "git" -ErrorAction SilentlyContinue
if ($null -eq $git) {
    throw "Git was not found"
}
$repositoryResult = Invoke-CapturedCommand -Executable $git.Source -Arguments @(
    "-C", $repositoryRoot, "rev-parse", "--show-toplevel"
)
if ($repositoryResult.ExitCode -ne 0 -or @($repositoryResult.Output).Count -eq 0) {
    throw "$repositoryRoot is not a Git repository"
}
$actualRoot = [System.IO.Path]::GetFullPath($repositoryResult.Output[0].ToString().Trim())
if (-not $actualRoot.Equals($repositoryRoot, [System.StringComparison]::OrdinalIgnoreCase)) {
    throw "Run the script from the GBaseLite repository root: $actualRoot"
}

$status = Get-RequiredOutput -Executable $git.Source -Arguments @(
    "-C", $repositoryRoot, "status", "--porcelain=v1", "--untracked-files=all"
) -FailureMessage "Unable to inspect the Git worktree"
if ($status.Count -gt 0) {
    throw ("The source worktree must be clean before publishing:" + [Environment]::NewLine +
        ($status -join [Environment]::NewLine))
}

[void](Get-RequiredOutput -Executable $git.Source -Arguments @(
    "-C", $repositoryRoot, "rev-parse", "--verify", "$SourceRef^{commit}"
) -FailureMessage "Source ref does not resolve to a commit: $SourceRef")

foreach ($key in @("user.name", "user.email")) {
    $configured = Get-RequiredOutput -Executable $git.Source -Arguments @(
        "-C", $repositoryRoot, "config", "--get", $key
    ) -FailureMessage "Git $key is not configured"
    if ($configured.Count -eq 0 -or [string]::IsNullOrWhiteSpace($configured[0])) {
        throw "Git $key is not configured"
    }
}

$branchName = "release/v$Version"
$tagName = "v$Version"
Invoke-CheckedCommand -Executable $git.Source -Arguments @(
    "check-ref-format", "--branch", $branchName
) -FailureMessage "Invalid release branch name"

if (Test-LocalGitRef -GitExecutable $git.Source -Reference "refs/heads/$branchName") {
    throw "Local release branch already exists: $branchName"
}
if (Test-LocalGitRef -GitExecutable $git.Source -Reference "refs/tags/$tagName") {
    throw "Local release tag already exists: $tagName"
}

if ($Publish -or $DryRun) {
    $remoteRefs = Get-RequiredOutput -Executable $git.Source -Arguments @(
        "ls-remote", $GitRepository,
        "refs/heads/$branchName",
        "refs/tags/$tagName"
    ) -FailureMessage "Unable to reach $GitRepository"
    if ($remoteRefs.Count -gt 0) {
        throw ("The remote release branch or tag already exists:" + [Environment]::NewLine +
            ($remoteRefs -join [Environment]::NewLine))
    }
}

$releaseDestination = Assert-SafeReleaseDestination -Path (Join-Path $distRoot "GBaseLite-$Version") -ReleaseVersion $Version
if ((Test-Path -LiteralPath $releaseDestination) -and -not $ReplaceArtifacts) {
    throw "Release output already exists: $releaseDestination. Use -ReplaceArtifacts only for an intentional rebuild."
}

$docker = $null
if ($Publish -or $DryRun) {
    $docker = Get-Command "docker" -ErrorAction SilentlyContinue
    if ($null -eq $docker) {
        throw "Docker was not found"
    }
    Invoke-CheckedCommand -Executable $docker.Source -Arguments @("info") -FailureMessage "Docker daemon is unavailable"
    Invoke-CheckedCommand -Executable $docker.Source -Arguments @("buildx", "version") -FailureMessage "Docker Buildx is unavailable"
}

Write-Host ""
Write-Host "Release plan"
Write-Host "  Source:        $SourceRef"
Write-Host "  Branch:        $branchName"
Write-Host "  Tag:           $tagName"
Write-Host "  GitHub:        $GitRepository"
Write-Host "  Docker Hub:    $DockerRepository"
Write-Host "  Artifacts:     $releaseDestination"
Write-Host "  Mode:          $(if ($Publish) { 'publish' } elseif ($PrepareOnly) { 'prepare-only' } else { 'dry-run' })"
Write-Host ""

if ($DryRun) {
    Write-Host "Dry run completed; no branch, tag, artifact, image, or remote state was changed."
    exit 0
}

New-Item -ItemType Directory -Path $temporaryRoot -Force | Out-Null
$identifier = [guid]::NewGuid().ToString("N")
$stagingRoot = Assert-SafeTemporaryPath -Path (Join-Path $temporaryRoot "publish-$tagName-$identifier")
$worktreeRoot = Join-Path $stagingRoot "worktree"
$worktreeAdded = $false
$completed = $false

try {
    New-Item -ItemType Directory -Path $stagingRoot -Force | Out-Null
    Invoke-CheckedCommand -Executable $git.Source -Arguments @(
        "-C", $repositoryRoot,
        "worktree", "add",
        "-b", $branchName,
        $worktreeRoot,
        $SourceRef
    ) -FailureMessage "Unable to create the isolated release worktree"
    $worktreeAdded = $true

    if ([string]::IsNullOrWhiteSpace($env:GBASELITE_DOTNET)) {
        $portableDotnet = Join-Path $temporaryRoot "dotnet-sdk\dotnet.exe"
        if (Test-Path -LiteralPath $portableDotnet -PathType Leaf) {
            $env:GBASELITE_DOTNET = $portableDotnet
        }
    }
    if ([string]::IsNullOrWhiteSpace($env:GBASELITE_WIX)) {
        $portableWix = Join-Path $temporaryRoot "wix-cli\wix.exe"
        if (Test-Path -LiteralPath $portableWix -PathType Leaf) {
            $env:GBASELITE_WIX = $portableWix
        }
    }

    Invoke-CheckedCommand -Executable "powershell.exe" -Arguments @(
        "-NoProfile",
        "-ExecutionPolicy", "Bypass",
        "-File", (Join-Path $worktreeRoot "scripts\one-click-release.ps1"),
        "-TargetVersion", $Version
    ) -WorkingDirectory $worktreeRoot -FailureMessage "The existing complete release packaging workflow failed"

    $allowedChanges = @(
        "CHANGELOG.md",
        "README.md",
        ".github/workflows/release.yml",
        "docker/docker-compose.binary.yml",
        "docker/docker-compose.yml",
        "docker/temp.env.example",
        "executor/executor.go"
    )
    $releaseStatus = Get-RequiredOutput -Executable $git.Source -Arguments @(
        "-C", $worktreeRoot, "status", "--porcelain=v1", "--untracked-files=all"
    ) -FailureMessage "Unable to inspect release branch changes"
    foreach ($line in $releaseStatus) {
        if ($line.Length -lt 4) {
            throw "Unexpected Git status output: $line"
        }
        $path = $line.Substring(3).Trim().Replace("\", "/")
        if ($path.Contains(" -> ")) {
            $path = $path.Split(@(" -> "), [System.StringSplitOptions]::None)[1]
        }
        if ($path -notin $allowedChanges) {
            throw "Unexpected release branch change: $path"
        }
    }

    $gitAddArguments = @("-C", $worktreeRoot, "add", "--") + $allowedChanges
    Invoke-CheckedCommand -Executable $git.Source -Arguments $gitAddArguments -FailureMessage "Unable to stage release version files"

    Invoke-CheckedCommand -Executable $git.Source -Arguments @(
        "-C", $worktreeRoot,
        "commit", "--allow-empty",
        "-m", "release: $tagName"
    ) -FailureMessage "Unable to create the release commit"

    $releaseSource = Join-Path $worktreeRoot "dist\GBaseLite-$Version"
    if (-not (Test-Path -LiteralPath $releaseSource -PathType Container)) {
        throw "The packaging workflow did not create $releaseSource"
    }
    Copy-ReleaseArtifacts -Source $releaseSource -Destination $releaseDestination -StagingDirectory $stagingRoot -Replace:$ReplaceArtifacts

    $revision = (Get-RequiredOutput -Executable $git.Source -Arguments @(
        "-C", $worktreeRoot, "rev-parse", "HEAD"
    ) -FailureMessage "Unable to resolve the release commit")[0].Trim()

    if ($Publish) {
        Test-DockerImage -DockerExecutable $docker.Source -Worktree $worktreeRoot -ReleaseVersion $Version -Identifier $identifier
        Invoke-DockerPush -DockerExecutable $docker.Source -Worktree $worktreeRoot -ReleaseVersion $Version -Repository $DockerRepository -Revision $revision
    }

    Invoke-CheckedCommand -Executable $git.Source -Arguments @(
        "-C", $worktreeRoot,
        "tag", "--annotate", $tagName,
        "--message", "GBaseLite $Version"
    ) -FailureMessage "Unable to create the release tag"

    if ($Publish) {
        Invoke-CheckedCommand -Executable $git.Source -Arguments @(
            "-C", $worktreeRoot,
            "push", "--atomic", $GitRepository,
            ("refs/heads/{0}:refs/heads/{0}" -f $branchName),
            ("refs/tags/{0}:refs/tags/{0}" -f $tagName)
        ) -FailureMessage "Unable to atomically push the GitHub release branch and tag"
    }

    $completed = $true
    Write-Host ""
    Write-Host "Release $Version completed."
    Write-Host "Artifacts: $releaseDestination"
    if ($Publish) {
        Write-Host "Docker Hub: https://hub.docker.com/r/$DockerRepository"
        Write-Host "GitHub: https://github.com/pucj0/gbaselite/releases/tag/$tagName"
        Write-Host "GitHub Actions will create the release assets and GHCR image asynchronously."
    } else {
        Write-Host "Prepared local branch $branchName and tag $tagName; no remote push occurred."
    }
} catch {
    Write-Warning "Release failed. The isolated worktree is retained for inspection: $worktreeRoot"
    throw
} finally {
    if ($completed -and -not $KeepWorktree -and $worktreeAdded) {
        Invoke-CheckedCommand -Executable $git.Source -Arguments @(
            "-C", $repositoryRoot, "worktree", "remove", "--force", $worktreeRoot
        ) -FailureMessage "Unable to remove the completed release worktree"
        if (Test-Path -LiteralPath $stagingRoot) {
            Remove-Item -LiteralPath $stagingRoot -Recurse -Force
        }
    }
}