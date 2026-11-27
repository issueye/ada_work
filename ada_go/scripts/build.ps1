# 编译入口程序到本地目录。默认产出当前平台的 bin/<命令>[.exe]。
# 产物不入库（.gitignore 已忽略 *.exe）；发布时用 -GOOS/-GOARCH 交叉编译。
param(
    [string]$OutputDir = "bin",
    [string]$GOOS = "",
    [string]$GOARCH = "",
    [switch]$StripDebug
)

$ErrorActionPreference = "Stop"
$root = Split-Path -Parent $PSScriptRoot
Push-Location $root
try {
    $goosValue = if ($GOOS) { $GOOS } else { (& go env GOOS).Trim() }
    $goarchValue = if ($GOARCH) { $GOARCH } else { (& go env GOARCH).Trim() }
    $ext = if ($goosValue -eq "windows") { ".exe" } else { "" }

    $commands = @("ada", "ada-demo", "ada-ts-demo")
    if (-not $OutputDir) { throw "-OutputDir 不能为空" }
    # 允许绝对路径;相对路径一律相对仓库根,避免受调用方 cwd 影响。
    $outAbs = if ([IO.Path]::IsPathRooted($OutputDir)) { $OutputDir } else { Join-Path $root $OutputDir }
    New-Item -ItemType Directory -Force -Path $outAbs | Out-Null

    # 交叉编译会改变 go env；只在本脚本进程里临时设置，避免污染当前 shell。
    $oldGOOS = $env:GOOS
    $oldGOARCH = $env:GOARCH
    $oldCGO = $env:CGO_ENABLED
    $env:GOOS = $goosValue
    $env:GOARCH = $goarchValue
    $env:CGO_ENABLED = "0"
    try {
        $ldflags = if ($StripDebug) { "-s -w" } else { "" }
        foreach ($cmd in $commands) {
            $out = Join-Path $outAbs ($cmd + $ext)
            Write-Host "build $cmd -> $out ($goosValue/$goarchValue)"
            $buildArgs = @("build", "-trimpath")
            if ($ldflags) { $buildArgs += @("-ldflags", $ldflags) }
            $buildArgs += @("-o", $out, "./cmd/$cmd")
            & go @buildArgs
            if ($LASTEXITCODE -ne 0) {
                throw "编译 $cmd 失败（exit $LASTEXITCODE）"
            }
            $size = (Get-Item $out).Length
            Write-Host ("  产物 {0:N2} MB" -f ($size / 1MB))
        }
    }
    finally {
        $env:GOOS = $oldGOOS
        $env:GOARCH = $oldGOARCH
        $env:CGO_ENABLED = $oldCGO
    }

    Write-Host "完成：$outAbs"
}
finally {
    Pop-Location
}
