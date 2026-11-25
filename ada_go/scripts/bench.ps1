# 基准预算门禁(AGENTS.md §6.3)。
#
# 用法:
#   pwsh scripts/bench.ps1                          # 只校验绝对预算(CI 用,跨机器稳定)
#   pwsh scripts/bench.ps1 -WriteBaseline b.txt     # 记录本机基线
#   pwsh scripts/bench.ps1 -Baseline b.txt          # 同机 ±15% 回归比对
#
# 为什么不把 ±15% 设为 CI 默认:基准随机器与 CPU 频率波动,
# 跨机器的固定基线会把「换了台快机器」误报成回归。基线文件只在同机比较时有意义。

param([string]$Baseline = "", [string]$WriteBaseline = "")

$ErrorActionPreference = "Stop"
$root = Split-Path -Parent (Split-Path -Parent $MyInvocation.MyCommand.Path)
Set-Location $root

# 预算数值来自 AGENTS.md §6.3 与 kernel-api.md §6。
$budget = [ordered]@{
    "BenchmarkServiceResolve"       = 1000        # 1 μs:服务解析是热路径
    "BenchmarkEmitSingleListener"   = 20000       # 20 μs:AGENTS §6.3 调用开销
    "BenchmarkWaterfallThreeLayers" = 20000       # 20 μs:三层拦截链
    "BenchmarkMountFiftyPlugins"    = 150000000   # 150 ms:AGENTS §6.3 冷启动
}

$out = & go test -run xxx -bench . -benchmem -count=1 ./platform/kernel 2>&1
$out | Write-Host
if ($LASTEXITCODE -ne 0) {
    Write-Host "基准执行失败。" -ForegroundColor Red
    exit 1
}

$measured = @{}
foreach ($line in $out) {
    if ($line -match '^(Benchmark[A-Za-z0-9_]+)(?:-\d+)?\s+\d+\s+([\d.]+) ns/op') {
        $measured[$Matches[1]] = [double]$Matches[2]
    }
}

$fail = 0
Write-Host ""
foreach ($name in $budget.Keys) {
    if (-not $measured.ContainsKey($name)) {
        Write-Host "缺少基准 $name" -ForegroundColor Red
        $fail = 1
        continue
    }
    $ns = $measured[$name]
    $max = [double]$budget[$name]
    $ok = $ns -le $max
    $status = if ($ok) { "OK" } else { "超预算" }
    Write-Host ("{0,-32} {1,12:N0} ns/op  预算 {2,12:N0}  {3}" -f $name, $ns, $max, $status)
    if (-not $ok) { $fail = 1 }
}

if ($WriteBaseline -ne "") {
    $rows = foreach ($name in $budget.Keys) {
        if ($measured.ContainsKey($name)) { "$name $($measured[$name])" }
    }
    $rows | Set-Content -Encoding utf8 $WriteBaseline
    Write-Host "`n基线已写入: $WriteBaseline(换机器后重新生成)"
}

if ($Baseline -ne "" -and (Test-Path $Baseline)) {
    Write-Host "`n== 与基线比较(±15%)"
    foreach ($line in Get-Content $Baseline) {
        if ($line -match '^(\S+)\s+([\d.]+)\s*$') {
            $name = $Matches[1]
            $base = [double]$Matches[2]
            if (-not $measured.ContainsKey($name)) { continue }
            $ratio = $measured[$name] / $base
            if ($ratio -gt 1.15) {
                Write-Host ("{0,-32} {1,8:P1}  回归" -f $name, $ratio) -ForegroundColor Red
                $fail = 1
            } elseif ($ratio -lt 0.85) {
                Write-Host ("{0,-32} {1,8:P1}  变快(请更新基线)" -f $name, $ratio) -ForegroundColor Yellow
            } else {
                Write-Host ("{0,-32} {1,8:P1}  OK" -f $name, $ratio)
            }
        }
    }
} elseif ($Baseline -ne "") {
    Write-Host "基线文件不存在: $Baseline" -ForegroundColor Red
    exit 1
}

if ($fail -ne 0) {
    Write-Host "`n基准门禁失败。" -ForegroundColor Red
    exit 1
}
Write-Host "`n基准门禁通过。" -ForegroundColor Green
