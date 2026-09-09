$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
. (Join-Path $PSScriptRoot 'release-source-files.ps1')
$workspace = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..'))
$allowed = [IO.Path]::GetFullPath((Join-Path $workspace '.tmp')) + [IO.Path]::DirectorySeparatorChar
$fixture = Join-Path $allowed ('release-source-test-' + [guid]::NewGuid().ToString('N'))
try {
    $included = @('root.go', 'mvcc/store.go', 'replication/node.go', 'failover/router.go', 'nested space/platform_windows.go', 'nested space/platform_linux.go')
    $ignored = @('.tmp/gomodcache/dependency.go', '.git/cache.go', 'data/private.go', 'logs/log.go', 'dist/generated.go', 'bin/cache.go', 'release/cache.go', 'vendor/external.go', 'node_modules/tool.go')
    foreach ($relative in @($included + $ignored)) {
        $path = Join-Path $fixture $relative
        New-Item -ItemType Directory -Path (Split-Path -Parent $path) -Force | Out-Null
        $content = if ($relative -in $included) { "package sample`n" } else { 'deliberately invalid third-party/cache source' }
        [IO.File]::WriteAllText($path, $content, [Text.UTF8Encoding]::new($false))
    }
    $actual = @(Get-ReleaseGoSources -Root $fixture)
    if ($actual.Count -ne $included.Count) { throw "Unexpected source count: $($actual.Count)" }
    foreach ($relative in $included) {
        if ([IO.Path]::GetFullPath((Join-Path $fixture $relative)) -notin $actual) { throw "Missing project source: $relative" }
    }
    $gofmt = (Get-Command gofmt -ErrorAction Stop).Source
    Test-ReleaseGoFormat -Root $fixture -Gofmt $gofmt
    $badSource = Join-Path $fixture 'mvcc/store.go'
    [IO.File]::WriteAllText($badSource, "package sample`nfunc sample( ){ }`n", [Text.UTF8Encoding]::new($false))
    $rejected = $false
    try { Test-ReleaseGoFormat -Root $fixture -Gofmt $gofmt } catch {
        if (-not $_.Exception.Message.Contains($badSource)) { throw }
        $rejected = $true
    }
    if (-not $rejected) { throw 'Unformatted project source was not rejected' }
    Write-Host 'Release source scope self-test passed (cache exclusions, new packages, spaces, platform files, format rejection).'
} finally {
    $resolved = [IO.Path]::GetFullPath($fixture)
    if (-not $resolved.StartsWith($allowed, [StringComparison]::OrdinalIgnoreCase)) { throw 'Unsafe fixture cleanup path' }
    if (Test-Path -LiteralPath $resolved) { Remove-Item -LiteralPath $resolved -Recurse -Force }
}
