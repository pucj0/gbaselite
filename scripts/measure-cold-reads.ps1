param(
    [ValidateRange(1000,1000000)][int]$Rows = 100000,
    [ValidateRange(10,10000)][int]$Iterations = 250,
    [ValidateRange(0,1024)][int]$PageCacheMiB = 4
)
$ErrorActionPreference = 'Stop'
$workspaceRoot = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..'))
$probeRoot = Join-Path $workspaceRoot ('.tmp/cold-probe-' + [Guid]::NewGuid().ToString('N'))
$dataDirectory = [IO.Path]::GetFullPath((Join-Path $probeRoot 'data'))
$allowedPrefix = [IO.Path]::GetFullPath($probeRoot).TrimEnd('\','/') + [IO.Path]::DirectorySeparatorChar
if (-not $dataDirectory.StartsWith($allowedPrefix, [StringComparison]::OrdinalIgnoreCase)) { throw 'Unsafe isolated data path' }
New-Item -ItemType Directory -Path $probeRoot -Force | Out-Null
$binary = Join-Path $probeRoot 'coldprobe.exe'
$originalCache = $env:GOCACHE
$env:GOCACHE = Join-Path $workspaceRoot '.tmp/gocache'
$processes = [Collections.Generic.List[object]]::new()
try {
    Push-Location $workspaceRoot
    try {
        & go build -o $binary ./scripts/internal/coldprobe
        if ($LASTEXITCODE -ne 0) { throw 'Cold probe build failed' }
    } finally { Pop-Location }
    $results = [Collections.Generic.List[object]]::new()
    foreach ($mode in @('seed', 'full', 'cold')) {
        $stdout = Join-Path $probeRoot ($mode + '.jsonl')
        $stderr = Join-Path $probeRoot ($mode + '.stderr.log')
        $arguments = @('-directory', ('"' + $dataDirectory + '"'), '-mode', $mode, '-rows', $Rows, '-iterations', $Iterations, '-cache-bytes', ([int64]$PageCacheMiB * 1MB), '-hold', '2s')
        $process = Start-Process -FilePath $binary -ArgumentList $arguments -WorkingDirectory $workspaceRoot -WindowStyle Hidden -RedirectStandardOutput $stdout -RedirectStandardError $stderr -PassThru
        $processes.Add($process)
        $samples = [Collections.Generic.List[object]]::new()
        $timer = [Diagnostics.Stopwatch]::StartNew()
        while (-not $process.HasExited) {
            $phase = 'starting'
            if (Test-Path -LiteralPath $stdout) {
                $lastLine = Get-Content -LiteralPath $stdout -Tail 1 -ErrorAction SilentlyContinue
                if ($lastLine) { try { $phase = ($lastLine | ConvertFrom-Json).phase } catch {} }
            }
            $current = Get-Process -Id $process.Id -ErrorAction SilentlyContinue
            if ($current) {
                $samples.Add([pscustomobject]@{ elapsed_ms = $timer.ElapsedMilliseconds; phase = $phase; working_set_bytes = $current.WorkingSet64; private_bytes = $current.PrivateMemorySize64; cpu_seconds = $current.CPU })
            }
            Start-Sleep -Milliseconds 100
            $process.Refresh()
        }
        $process.WaitForExit()
        if ($process.ExitCode -ne 0) { throw "$mode probe failed: $(Get-Content -LiteralPath $stderr -Raw)" }
        $reports = @(Get-Content -LiteralPath $stdout | ForEach-Object { $_ | ConvertFrom-Json })
        $phases = @($samples | Group-Object phase | ForEach-Object {
            $last = $_.Group[-1]
            [pscustomobject]@{ phase = $_.Name; samples = $_.Count; last_working_set_bytes = $last.working_set_bytes; last_private_bytes = $last.private_bytes; last_cpu_seconds = $last.cpu_seconds; observed_peak_working_set_bytes = ($_.Group | Measure-Object working_set_bytes -Maximum).Maximum }
        })
        $entry = [pscustomobject]@{ mode = $mode; elapsed_ms = $timer.ElapsedMilliseconds; observed_peak_working_set_bytes = ($samples | Measure-Object working_set_bytes -Maximum).Maximum; observed_peak_private_bytes = ($samples | Measure-Object private_bytes -Maximum).Maximum; phases = $phases; reports = $reports; process_samples = $samples.ToArray() }
        $results.Add($entry)
        $entry | ConvertTo-Json -Depth 16 | Set-Content -LiteralPath (Join-Path $probeRoot ($mode + '.measurement.json')) -Encoding utf8
    }
    $report = [pscustomobject]@{ recorded_at = [DateTimeOffset]::Now.ToString('o'); rows = $Rows; payload_bytes = 256; iterations = $Iterations; page_cache_mib = $PageCacheMiB; sampling_interval_ms = 100; hold_each_phase_seconds = 2; protocol = 'In-process SQL engine; one client; result values verified'; caveats = 'New process per mode, same database and OS filesystem cache; full is run before cold; Go GC before phase samples; no OS cache flush; measured peaks can miss sub-100ms spikes; Close/persistence excluded from read probes'; runs = $results.ToArray() }
    $reportPath = Join-Path $probeRoot 'results.json'
    $report | ConvertTo-Json -Depth 18 | Set-Content -LiteralPath $reportPath -Encoding utf8
    Write-Output $reportPath
} finally {
    foreach ($process in $processes) { if (-not $process.HasExited) { Stop-Process -Id $process.Id -ErrorAction SilentlyContinue } }
    if (Test-Path -LiteralPath $dataDirectory) {
        $resolvedData = (Resolve-Path -LiteralPath $dataDirectory).Path
        if (-not $resolvedData.StartsWith($allowedPrefix, [StringComparison]::OrdinalIgnoreCase)) { throw 'Refusing to remove data outside isolated probe directory' }
        Remove-Item -LiteralPath $resolvedData -Recurse -Force
    }
    $env:GOCACHE = $originalCache
}
