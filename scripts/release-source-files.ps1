# Source-only traversal: do not recurse into build outputs, databases or tool caches.
function Get-ReleaseGoSources {
    param([Parameter(Mandatory)][string]$Root)
    $excluded = @('data', 'logs', 'dist', 'bin', 'release', 'vendor', 'node_modules')
    foreach ($entry in Get-ChildItem -LiteralPath $Root -Force | Sort-Object Name) {
        if (($entry.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) { continue }
        if ($entry.PSIsContainer) {
            if ($entry.Name.StartsWith('.') -or $entry.Name -in $excluded) { continue }
            Get-ReleaseGoSources -Root $entry.FullName
        } elseif ($entry.Extension -eq '.go') {
            $entry.FullName
        }
    }
}

function Test-ReleaseGoFormat {
    param([Parameter(Mandatory)][string]$Root, [Parameter(Mandatory)][string]$Gofmt)
    $sources = @(Get-ReleaseGoSources -Root $Root)
    if ($sources.Count -eq 0) { throw "No project Go source files found in $Root" }
    $unformatted = @()
    for ($offset = 0; $offset -lt $sources.Count; $offset += 64) {
        $last = [Math]::Min($offset + 63, $sources.Count - 1)
        $batch = @($sources[$offset..$last])
        $unformatted += @(& $Gofmt -l @batch)
        if ($LASTEXITCODE -ne 0) { throw 'gofmt check failed' }
    }
    if ($unformatted.Count -gt 0) {
        throw "The following Go files require gofmt: $($unformatted -join ', ')"
    }
}
