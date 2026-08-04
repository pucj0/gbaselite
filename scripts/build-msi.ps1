[CmdletBinding()]
param(
    [Parameter(Mandatory)][string]$Version,
    [Parameter(Mandatory)][string]$SourceDirectory,
    [string]$OutputPath,
    [string]$WixExecutable = "wix",
    [string]$WixUIExtension = "WixToolset.UI.wixext"
)

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

$consoleEncoding = [System.Text.UTF8Encoding]::new($false)
[Console]::InputEncoding = $consoleEncoding
[Console]::OutputEncoding = $consoleEncoding
$OutputEncoding = $consoleEncoding

function Remove-MsiMaintenanceBlocks {
    param([Parameter(Mandatory)][string]$MsiPath)

    $installer = $null
    $database = $null
    try {
        $flags = [System.Reflection.BindingFlags]::InvokeMethod
        $installer = New-Object -ComObject WindowsInstaller.Installer
        $database = $installer.GetType().InvokeMember(
            'OpenDatabase',
            $flags,
            $null,
            $installer,
            [object[]]@([string]$MsiPath, [int]1)
        )
        foreach ($propertyName in @('ARPNOMODIFY', 'ARPNOREPAIR')) {
            $view = $null
            try {
                $query = "DELETE FROM ``Property`` WHERE ``Property`` = '$propertyName'"
                $view = $database.GetType().InvokeMember('OpenView', $flags, $null, $database, [object[]]@($query))
                $view.GetType().InvokeMember('Execute', $flags, $null, $view, $null) | Out-Null
            } finally {
                if ($null -ne $view) {
                    [void][System.Runtime.InteropServices.Marshal]::FinalReleaseComObject($view)
                }
            }
        }
        $database.GetType().InvokeMember('Commit', $flags, $null, $database, $null) | Out-Null
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

$repositoryRoot = [System.IO.Path]::GetFullPath((Join-Path $PSScriptRoot ".."))
$sourceRoot = [System.IO.Path]::GetFullPath($SourceDirectory)
if (-not (Test-Path -LiteralPath (Join-Path $sourceRoot "gbaselite.exe"))) {
    throw "The MSI source directory must contain gbaselite.exe: $sourceRoot"
}
if ($Version -notmatch '^[0-9]+\.[0-9]+\.[0-9]+$') {
    throw "MSI versions must use numeric major.minor.patch format: $Version"
}
if ([string]::IsNullOrWhiteSpace($OutputPath)) {
    $OutputPath = Join-Path $repositoryRoot "dist\GBaseLite-windows-amd64.msi"
}
$OutputPath = [System.IO.Path]::GetFullPath($OutputPath)
New-Item -ItemType Directory -Path (Split-Path -Parent $OutputPath) -Force | Out-Null

$uiAssetScript = Join-Path $repositoryRoot "installer\wix\ui-assets.ps1"
$licenseSource = Join-Path $repositoryRoot "installer\wix\license.zh-CN.txt"
if (-not (Test-Path -LiteralPath $uiAssetScript) -or -not (Test-Path -LiteralPath $licenseSource)) {
    throw "The MSI Chinese license and UI asset generator are required"
}
. $uiAssetScript
$uiAssetOutput = Join-Path $repositoryRoot ".tmp\msi-ui-$Version"
$uiAssets = New-GBaseLiteMsiUiAssets -LicenseSourcePath $licenseSource -OutputDirectory $uiAssetOutput
foreach ($assetPath in @($uiAssets.LicenseRtf, $uiAssets.BannerBitmap, $uiAssets.DialogBitmap, $uiAssets.ApplicationIcon)) {
    if (-not (Test-Path -LiteralPath $assetPath) -or (Get-Item -LiteralPath $assetPath).Length -eq 0) {
        throw "The MSI UI asset was not generated: $assetPath"
    }
}
$rtfBytes = [System.IO.File]::ReadAllBytes($uiAssets.LicenseRtf)
if ($rtfBytes | Where-Object { $_ -gt 127 } | Select-Object -First 1) {
    throw "The generated Chinese license RTF must use ASCII-safe Unicode escapes"
}
$rtfText = [System.Text.Encoding]::ASCII.GetString($rtfBytes)
if ($rtfText -notmatch 'GBaseLite' -or $rtfText -notmatch '\\u-?[0-9]+\?') {
    throw "The generated Chinese license RTF does not contain Unicode license text"
}
$iconBytes = [System.IO.File]::ReadAllBytes($uiAssets.ApplicationIcon)
if ($iconBytes.Length -lt 150 -or [System.BitConverter]::ToUInt16($iconBytes, 0) -ne 0 -or
    [System.BitConverter]::ToUInt16($iconBytes, 2) -ne 1 -or
    [System.BitConverter]::ToUInt16($iconBytes, 4) -ne 9) {
    throw "The GBaseLite application icon must contain the complete multi-size icon set"
}
Add-Type -AssemblyName System.Drawing
$bannerImage = [System.Drawing.Image]::FromFile($uiAssets.BannerBitmap)
$dialogImage = [System.Drawing.Image]::FromFile($uiAssets.DialogBitmap)
try {
    if ($bannerImage.Width -ne 493 -or $bannerImage.Height -ne 58 -or
        $dialogImage.Width -ne 493 -or $dialogImage.Height -ne 312) {
        throw "The MSI UI bitmaps do not use the WiX-required dimensions"
    }
} finally {
    $dialogImage.Dispose()
    $bannerImage.Dispose()
}

$wix = Get-Command $WixExecutable -ErrorAction Stop
$dotnet = Get-Command "dotnet" -ErrorAction Stop
$sdks = & $dotnet.Source --list-sdks
if ($LASTEXITCODE -ne 0 -or -not $sdks) {
    throw "A .NET SDK is required to build the MSI custom action"
}

$customActionProject = Join-Path $repositoryRoot "installer\wix\custom-action\GBaseLite.CustomActions.csproj"
$customActionSourcePath = Join-Path $repositoryRoot "installer\wix\custom-action\CustomActions.cs"
$customActionSource = [System.IO.File]::ReadAllText($customActionSourcePath)
foreach ($requiredPattern in @(
    'var serverHost = "127.0.0.1";',
    'ReadConfigSectionValue(path, "server", "host", serverHost)',
	'ReadConfigSectionValue(path, "tls", "enabled", tlsEnabled)',
	'ReadConfigSectionValue(path, "tls", "cert_file", tlsCertFile)',
	'ReadConfigSectionValue(path, "tls", "key_file", tlsKeyFile)',
	'ReadConfigSectionValue(path, "tls", "require_secure_transport", requireSecureTransport)',
	'ReadConfigSectionValue(path, "log", "max_size_mb", logMaxSizeMB)',
	'ReadConfigSectionValue(path, "log", "retention_days", logRetentionDays)',
    'HardenRuntimePermissions(configPath, payload.DataPath, payload.LogPath);',
    'SetAccessRuleProtection(true, false)'
)) {
    if (-not $customActionSource.Contains($requiredPattern)) {
        throw "The MSI custom action is missing required security behavior: $requiredPattern"
    }
}
$customActionOutput = Join-Path $repositoryRoot ".tmp\msi-custom-action-$Version"
New-Item -ItemType Directory -Path $customActionOutput -Force | Out-Null
& $dotnet.Source build $customActionProject --configuration Release --output $customActionOutput --nologo
if ($LASTEXITCODE -ne 0) {
    throw "Unable to build the MSI custom action"
}
$customAction = Get-ChildItem -LiteralPath $customActionOutput -Filter "*.CA.dll" -File | Select-Object -First 1
if ($null -eq $customAction) {
    throw "The WiX custom action package was not generated"
}

$wixSource = Join-Path $repositoryRoot "installer\wix\Package.wxs"
$wixDocument = New-Object System.Xml.XmlDocument
$wixDocument.Load($wixSource)
$namespaceManager = New-Object System.Xml.XmlNamespaceManager($wixDocument.NameTable)
$namespaceManager.AddNamespace("wix", "http://wixtoolset.org/schemas/v4/wxs")
$packageNode = $wixDocument.SelectSingleNode("/wix:Wix/wix:Package", $namespaceManager)
$pathNode = $wixDocument.SelectSingleNode("//wix:Environment[@Id='AddInstallFolderToPath']", $namespaceManager)
$reinitializeProperty = $wixDocument.SelectSingleNode("//wix:Property[@Id='GBASE_REINITIALIZE']", $namespaceManager)
$confirmedProperty = $wixDocument.SelectSingleNode("//wix:Property[@Id='GBASE_REINITIALIZE_CONFIRMED']", $namespaceManager)
$installRegistryComponent = $wixDocument.SelectSingleNode("//wix:Component[@Id='InstallDirectoryRegistryComponent']", $namespaceManager)
$dataComponent = $wixDocument.SelectSingleNode("//wix:Component[@Id='DataDirectoryComponent']", $namespaceManager)
$logComponent = $wixDocument.SelectSingleNode("//wix:Component[@Id='LogDirectoryComponent']", $namespaceManager)
$installRegistrySearch = $wixDocument.SelectSingleNode("//wix:Property[@Id='GBASE_EXISTING_INSTALL_DIR']/wix:RegistrySearch[@Name='InstallDirectory']", $namespaceManager)
$dataRegistrySearch = $wixDocument.SelectSingleNode("//wix:Property[@Id='GBASE_DATA_DIR']/wix:RegistrySearch[@Name='DataDirectory']", $namespaceManager)
$logRegistrySearch = $wixDocument.SelectSingleNode("//wix:Property[@Id='GBASE_LOG_DIR']/wix:RegistrySearch[@Name='LogDirectory']", $namespaceManager)
$useExistingInstallFolder = $wixDocument.SelectSingleNode("//wix:CustomAction[@Id='UseExistingInstallFolder' and @Property='INSTALLFOLDER']", $namespaceManager)
$useExistingInstallFolderUI = $wixDocument.SelectSingleNode("//wix:InstallUISequence/wix:Custom[@Action='UseExistingInstallFolder']", $namespaceManager)
$useExistingInstallFolderExecute = $wixDocument.SelectSingleNode("//wix:InstallExecuteSequence/wix:Custom[@Action='UseExistingInstallFolder']", $namespaceManager)
$reinitializeCheck = $wixDocument.SelectSingleNode("//wix:Dialog[@Id='GBaseConfigDlg']/wix:Control[@Id='ReinitializeCheck' and @Property='GBASE_REINITIALIZE']", $namespaceManager)
$desktopShortcutProperty = $wixDocument.SelectSingleNode("//wix:Property[@Id='GBASE_DESKTOP_SHORTCUT']", $namespaceManager)
$desktopShortcutRegistrySearch = $wixDocument.SelectSingleNode("//wix:Property[@Id='GBASE_DESKTOP_SHORTCUT']/wix:RegistrySearch[@Id='FindExistingDesktopShortcut']", $namespaceManager)
$desktopShortcutCheck = $wixDocument.SelectSingleNode("//wix:Dialog[@Id='GBaseConfigDlg']/wix:Control[@Id='DesktopShortcutCheck' and @Property='GBASE_DESKTOP_SHORTCUT']", $namespaceManager)
$desktopShortcutComponent = $wixDocument.SelectSingleNode("//wix:Component[@Id='DesktopShortcutComponent']", $namespaceManager)
$desktopShortcut = $wixDocument.SelectSingleNode("//wix:Shortcut[@Id='GBaseLiteDesktopShortcut']", $namespaceManager)
$mainFeature = $wixDocument.SelectSingleNode("//wix:Feature[@Id='MainFeature']", $namespaceManager)
$desktopShortcutFeature = $wixDocument.SelectSingleNode("//wix:Feature[@Id='DesktopShortcutFeature' and @Level='2']/wix:ComponentRef[@Id='DesktopShortcutComponent']", $namespaceManager)
$desktopShortcutAddLocal = $wixDocument.SelectSingleNode("//wix:Dialog[@Id='GBaseConfigDlg']/wix:Control[@Id='Next']/wix:Publish[@Event='AddLocal' and @Value='DesktopShortcutFeature']", $namespaceManager)
$desktopShortcutRemove = $wixDocument.SelectSingleNode("//wix:Dialog[@Id='GBaseConfigDlg']/wix:Control[@Id='Next']/wix:Publish[@Event='Remove' and @Value='DesktopShortcutFeature']", $namespaceManager)
$maintenanceChangeNavigation = $wixDocument.SelectSingleNode("//wix:Publish[@Dialog='MaintenanceTypeDlg' and @Control='ChangeButton' and @Event='NewDialog' and @Value='GBaseConfigDlg']", $namespaceManager)
$maintenanceBackNavigation = $wixDocument.SelectSingleNode("//wix:Dialog[@Id='GBaseConfigDlg']/wix:Control[@Id='Back']/wix:Publish[@Event='NewDialog' and @Value='MaintenanceTypeDlg']", $namespaceManager)
$noRepairProperty = $wixDocument.SelectSingleNode("//wix:Property[@Id='ARPNOREPAIR']", $namespaceManager)
$noModifyProperty = $wixDocument.SelectSingleNode("//wix:Property[@Id='ARPNOMODIFY']", $namespaceManager)
$confirmationDialog = $wixDocument.SelectSingleNode("//wix:Dialog[@Id='GBaseReinitializeConfirmDlg']", $namespaceManager)
$upgradeNavigation = $wixDocument.SelectSingleNode("//wix:Publish[@Dialog='InstallDirDlg' and @Control='Next' and @Value='GBaseConfigDlg']", $namespaceManager)
$licenseVariable = $wixDocument.SelectSingleNode("//wix:WixVariable[@Id='WixUILicenseRtf']", $namespaceManager)
$bannerVariable = $wixDocument.SelectSingleNode("//wix:WixVariable[@Id='WixUIBannerBmp']", $namespaceManager)
$dialogVariable = $wixDocument.SelectSingleNode("//wix:WixVariable[@Id='WixUIDialogBmp']", $namespaceManager)
$arpIconProperty = $wixDocument.SelectSingleNode("//wix:Property[@Id='ARPPRODUCTICON']", $namespaceManager)
$applicationIcon = $wixDocument.SelectSingleNode("//wix:Icon[@Id='GBaseLiteApplicationIcon']", $namespaceManager)
$installedIconFile = $wixDocument.SelectSingleNode("//wix:File[@Id='GBaseLiteIconFile' and @Name='gbaselite.ico']", $namespaceManager)
$inactiveExampleConfig = $wixDocument.SelectSingleNode("//wix:File[@Id='ExampleConfigFile' or contains(@Source, 'config.example.yaml')]", $namespaceManager)
$startMenuShortcut = $wixDocument.SelectSingleNode("//wix:Shortcut[@Id='GBaseLiteStartMenuShortcut']", $namespaceManager)
$customBannerBinary = $wixDocument.SelectSingleNode("//wix:Binary[@Id='GBaseCustomBanner']", $namespaceManager)
$configBanner = $wixDocument.SelectSingleNode("//wix:Dialog[@Id='GBaseConfigDlg']/wix:Control[@Id='BannerBitmap' and @Type='Bitmap']", $namespaceManager)
$confirmationBanner = $wixDocument.SelectSingleNode("//wix:Dialog[@Id='GBaseReinitializeConfirmDlg']/wix:Control[@Id='BannerBitmap' and @Type='Bitmap']", $namespaceManager)
if ($null -eq $packageNode -or $packageNode.Language -ne "2052") {
    throw "The MSI package language must be Simplified Chinese (2052)"
}
if ($null -eq $pathNode -or $pathNode.Name -ne "Path" -or $pathNode.Value -ne "[INSTALLFOLDER]" -or
    $pathNode.Action -ne "set" -or $pathNode.Part -ne "last" -or $pathNode.System -ne "yes") {
    throw "The MSI must append INSTALLFOLDER to the system Path"
}
if ($null -eq $licenseVariable -or $licenseVariable.Value -ne '$(var.LicenseRtfPath)' -or
    $null -eq $bannerVariable -or $bannerVariable.Value -ne '$(var.BannerBitmapPath)' -or
    $null -eq $dialogVariable -or $dialogVariable.Value -ne '$(var.DialogBitmapPath)' -or
    $null -eq $customBannerBinary -or $customBannerBinary.SourceFile -ne '$(var.BannerBitmapPath)' -or
    $null -eq $configBanner -or $configBanner.Text -ne 'GBaseCustomBanner' -or
    $null -eq $confirmationBanner -or $confirmationBanner.Text -ne 'GBaseCustomBanner') {
    throw "The MSI must use the Chinese license and branded standard/custom dialog artwork"
}
if ($null -eq $arpIconProperty -or $arpIconProperty.Value -ne 'GBaseLiteApplicationIcon' -or
    $null -eq $applicationIcon -or $applicationIcon.SourceFile -ne '$(var.ApplicationIconPath)' -or
    $null -eq $installedIconFile -or $installedIconFile.Source -ne '$(var.ApplicationIconPath)' -or
    $null -eq $startMenuShortcut -or $startMenuShortcut.Target -ne '[#GBaseLiteExecutable]' -or
    $startMenuShortcut.Icon -ne 'GBaseLiteApplicationIcon') {
    throw "The MSI must install and register the GBaseLite application icon and Start menu shortcut"
}
if ($null -ne $inactiveExampleConfig) {
    throw "The MSI must not install an inactive config.example.yaml beside the executable"
}
if ($null -ne $noRepairProperty -or $null -ne $noModifyProperty) {
    throw "The MSI source must not disable maintenance repair or change"
}
if ($null -eq $desktopShortcutProperty -or -not [string]::IsNullOrEmpty($desktopShortcutProperty.GetAttribute('Value')) -or
    $null -eq $desktopShortcutRegistrySearch -or $desktopShortcutRegistrySearch.Bitness -ne 'always64' -or
    $null -eq $desktopShortcutCheck -or $desktopShortcutCheck.CheckBoxValue -ne '1' -or
    $null -eq $desktopShortcutComponent -or -not [string]::IsNullOrEmpty($desktopShortcutComponent.GetAttribute('Condition')) -or
    $null -eq $desktopShortcut -or $desktopShortcut.Target -ne '[#GBaseLiteExecutable]' -or
    $desktopShortcut.Icon -ne 'GBaseLiteApplicationIcon' -or
    $null -eq $mainFeature -or $mainFeature.Display -ne 'expand' -or
    $null -eq $desktopShortcutFeature -or
    $null -eq $desktopShortcutAddLocal -or $desktopShortcutAddLocal.Condition -ne 'GBASE_DESKTOP_SHORTCUT = "1"' -or
    $null -eq $desktopShortcutRemove -or $desktopShortcutRemove.Condition -ne 'GBASE_DESKTOP_SHORTCUT <> "1"' -or
    $null -eq $maintenanceChangeNavigation -or $maintenanceChangeNavigation.Condition -ne '1' -or
    $null -eq $maintenanceBackNavigation -or $maintenanceBackNavigation.Condition -ne 'Installed') {
    throw "The MSI desktop shortcut option must remain optional and unchecked by default"
}
if ($null -eq $reinitializeProperty -or -not [string]::IsNullOrEmpty($reinitializeProperty.GetAttribute("Value")) -or
    $null -eq $confirmedProperty -or -not [string]::IsNullOrEmpty($confirmedProperty.GetAttribute("Value"))) {
    throw "MSI data reinitialization properties must default to disabled"
}
if ($null -eq $installRegistryComponent -or $installRegistryComponent.Permanent -ne "yes" -or
    $installRegistryComponent.Bitness -ne "always64") {
    throw "MSI must retain the selected 64-bit installation directory for future installs"
}
if ($null -eq $dataComponent -or $dataComponent.Permanent -ne "yes" -or $dataComponent.NeverOverwrite -ne "yes" -or
    $dataComponent.Bitness -ne "always64" -or $null -eq $logComponent -or $logComponent.Permanent -ne "yes" -or
    $logComponent.NeverOverwrite -ne "yes" -or $logComponent.Bitness -ne "always64") {
    throw "MSI data and log components must be permanent and never overwritten"
}
if ($null -eq $installRegistrySearch -or $installRegistrySearch.Bitness -ne "always64" -or
    $null -eq $dataRegistrySearch -or $dataRegistrySearch.Bitness -ne "always64" -or
    $null -eq $logRegistrySearch -or $logRegistrySearch.Bitness -ne "always64" -or
    $null -eq $useExistingInstallFolder -or $useExistingInstallFolder.Value -ne "[GBASE_EXISTING_INSTALL_DIR]" -or
    $null -eq $useExistingInstallFolderUI -or $useExistingInstallFolderUI.After -ne "AppSearch" -or
    $null -eq $useExistingInstallFolderExecute -or $useExistingInstallFolderExecute.After -ne "AppSearch" -or
    $null -eq $reinitializeCheck -or $null -eq $confirmationDialog -or
    $null -eq $upgradeNavigation -or $upgradeNavigation.Condition -ne "NOT Installed") {
    throw "MSI upgrades must restore prior directories and show the preserve-data configuration UI"
}
& $wix.Source build $wixSource `
    -arch x64 `
    -culture zh-CN `
    -d "Version=$Version" `
    -d "SourceDirectory=$sourceRoot" `
    -d "CustomActionPath=$($customAction.FullName)" `
    -d "LicenseRtfPath=$($uiAssets.LicenseRtf)" `
    -d "BannerBitmapPath=$($uiAssets.BannerBitmap)" `
    -d "DialogBitmapPath=$($uiAssets.DialogBitmap)" `
    -d "ApplicationIconPath=$($uiAssets.ApplicationIcon)" `
    -ext $WixUIExtension `
    -pdbtype none `
    -out $OutputPath
if ($LASTEXITCODE -ne 0) {
    throw "WiX failed to build the MSI"
}
Remove-MsiMaintenanceBlocks -MsiPath $OutputPath

Get-Item -LiteralPath $OutputPath | Select-Object FullName, Length, LastWriteTime
