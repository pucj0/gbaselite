param(
 [Parameter(Mandatory=$true)][string]$BaselineExecutable,
 [Parameter(Mandatory=$true)][string]$CandidateExecutable,
 [int]$MemoryLimitMB=0,
 [int]$MaxProcs=0,
 [ValidateRange(100,1000000)][int]$Rows=10000,
 [ValidateRange(1,32)][int]$Workers=1,
 [ValidateRange(0,120)][int]$DurationSeconds=0
)
$ErrorActionPreference='Stop'
$taskRepo=[IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..'))
$taskOutput=Join-Path $taskRepo ('.tmp\resource-run-'+[guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $taskOutput | Out-Null
$taskProbe=Join-Path $taskOutput 'probe.exe'
Push-Location $taskRepo
try {
 & go build -o $taskProbe ./scripts/internal/resourceprobe
 if ($LASTEXITCODE -ne 0) { throw 'probe build failed' }
} finally { Pop-Location }
$taskEnvironment=@{}
foreach ($taskEntry in Get-ChildItem Env:) {
 if ($taskEntry.Name -like 'DB_*' -or $taskEntry.Name -in @('GOMEMLIMIT','GOMAXPROCS','GOGC')) {
  $taskEnvironment[$taskEntry.Name]=$taskEntry.Value
  [Environment]::SetEnvironmentVariable($taskEntry.Name,$null,'Process')
 }
}
$taskResults=@()
try {
 foreach ($taskCase in @(@{name='before';exe=$BaselineExecutable},@{name='after';exe=$CandidateExecutable})) {
  $taskExecutable=[IO.Path]::GetFullPath($taskCase.exe)
  if (!(Test-Path -LiteralPath $taskExecutable)) { throw "missing executable $taskExecutable" }
  $taskDirectory=Join-Path $taskOutput $taskCase.name
  New-Item -ItemType Directory -Path $taskDirectory | Out-Null
  $taskData=Join-Path $taskDirectory 'data'
  $taskLogs=Join-Path $taskDirectory 'logs'
  $taskPortListener=[Net.Sockets.TcpListener]::new([Net.IPAddress]::Loopback,0)
  $taskPortListener.Start()
  $taskPort=$taskPortListener.LocalEndpoint.Port
  $taskPortListener.Stop()
  $taskCaseMemory=0
  $taskCaseProcs=0
  if ($taskCase.name -eq 'after') { $taskCaseMemory=$MemoryLimitMB; $taskCaseProcs=$MaxProcs }
  $taskConfig=Join-Path $taskDirectory 'config.yaml'
  $taskConfigText=@"
server:
  host: 127.0.0.1
  port: $taskPort
  max_connections: $([Math]::Max(32,$Workers+1))
  write_buffer_kb: 8
  slow_query_ms: 0
resources:
  memory_limit_mb: $taskCaseMemory
  max_procs: $taskCaseProcs
storage:
  path: $($taskData.Replace('\','/'))
auth:
  username: root
  password: resource-probe-only
log:
  path: $($taskLogs.Replace('\','/'))
"@
  [IO.File]::WriteAllText($taskConfig,$taskConfigText)
  $taskServer=$null
  $taskClient=$null
  try {
   $taskServer=Start-Process -FilePath $taskExecutable -ArgumentList @('server','--config',('"'+$taskConfig+'"')) -PassThru -WindowStyle Hidden -RedirectStandardOutput (Join-Path $taskDirectory 'stdout.log') -RedirectStandardError (Join-Path $taskDirectory 'stderr.log')
   $taskClock=[Diagnostics.Stopwatch]::StartNew()
   $taskClient=Start-Process -FilePath $taskProbe -ArgumentList @('-address',"127.0.0.1:$taskPort",'-rows',$Rows,'-workers',$Workers,'-duration',($DurationSeconds.ToString()+'s')) -PassThru -WindowStyle Hidden -RedirectStandardOutput (Join-Path $taskDirectory 'sql.json') -RedirectStandardError (Join-Path $taskDirectory 'probe-error.log')
   $taskPeakWorking=[int64]0
   $taskPeakPrivate=[int64]0
   do {
    Start-Sleep -Milliseconds 100
    $taskServer.Refresh()
    if ($taskServer.HasExited) { throw 'isolated server exited unexpectedly' }
    $taskPeakWorking=[Math]::Max($taskPeakWorking,$taskServer.WorkingSet64)
    $taskPeakPrivate=[Math]::Max($taskPeakPrivate,$taskServer.PrivateMemorySize64)
    if ($taskClock.Elapsed.TotalSeconds -gt 180) { throw 'benchmark timeout' }
   } while (!$taskClient.HasExited)
   $taskClient.WaitForExit()
   if ($taskClient.ExitCode -ne 0) { throw ([IO.File]::ReadAllText((Join-Path $taskDirectory 'probe-error.log'))) }
   $taskServer.Refresh()
   $taskResult=[ordered]@{
    name=$taskCase.name
    memory_limit_mb=$taskCaseMemory
    max_procs=$taskCaseProcs
    cpu_seconds=$taskServer.TotalProcessorTime.TotalSeconds
    elapsed_seconds=$taskClock.Elapsed.TotalSeconds
    peak_working_set_bytes=[Math]::Max($taskPeakWorking,$taskServer.PeakWorkingSet64)
    sampled_peak_private_bytes=$taskPeakPrivate
    final_working_set_bytes=$taskServer.WorkingSet64
    sql=([IO.File]::ReadAllText((Join-Path $taskDirectory 'sql.json'))|ConvertFrom-Json)
   }
   $taskResults+=$taskResult
   $taskResult|ConvertTo-Json -Depth 8
  } finally {
   if ($null -ne $taskClient -and !$taskClient.HasExited) { Stop-Process -Id $taskClient.Id -Force; $taskClient.WaitForExit() }
   if ($null -ne $taskServer -and !$taskServer.HasExited) { Stop-Process -Id $taskServer.Id -Force; $taskServer.WaitForExit() }
   # Only isolated, generated data can be deleted; reports and logs remain.
   $taskResolvedData=[IO.Path]::GetFullPath($taskData)
   $taskExpectedRoot=[IO.Path]::GetFullPath($taskOutput)+[IO.Path]::DirectorySeparatorChar
   if (!$taskResolvedData.StartsWith($taskExpectedRoot,[StringComparison]::OrdinalIgnoreCase)) { throw 'unsafe cleanup path' }
   if (Test-Path -LiteralPath $taskResolvedData) { Remove-Item -LiteralPath $taskResolvedData -Recurse -Force }
  }
 }
 [IO.File]::WriteAllText((Join-Path $taskOutput 'results.json'),($taskResults|ConvertTo-Json -Depth 8))
 Write-Output "Report: $taskOutput"
} finally {
 foreach ($taskName in $taskEnvironment.Keys) { [Environment]::SetEnvironmentVariable($taskName,$taskEnvironment[$taskName],'Process') }
}