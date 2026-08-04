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

$arguments = @{
    OutputDirectory = $OutputDirectory
    GoExecutable = $GoExecutable
    WixExecutable = $WixExecutable
    WixUIExtension = $WixUIExtension
}
if ($Version) { $arguments.Version = $Version }
if ($SkipChecks) { $arguments.SkipChecks = $true }
if ($SkipMSI) { $arguments.SkipMSI = $true }

& (Join-Path $PSScriptRoot "build-release.ps1") @arguments
exit $LASTEXITCODE
