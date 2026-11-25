# 编译并打包发布物:
#   dist/<name>/bin/       入口二进制
#   dist/<name>/plugins/   由编译进二进制的 builtins 自动导出的 plugin.json
#   dist/<name>/presets/   只引用包内 plugins/workspace/data 的发布 preset
#   dist/<name>/workspace/ 默认工作区与 docs 只读示例
#
# 关键约束:Go 插件清单不是从 examples/ 手抄,而是从 builtins.Plugins() 导出,
# 因此发布包的 Go 清单与二进制内置插件集合一致;TS 插件按目录复制源码与清单
# (内置 text-helper/shell-helper + examples 里的示例脚本插件),运行时由包内 esbuild 编译。
param(
    [string]$Version = "",
    [string]$GOOS = "",
    [string]$GOARCH = "",
    [string]$OutputDir = "dist",
    [string]$LLMEndpoint = "",
    [string]$LLMModel = "",
    [switch]$SkipBuild,
    [switch]$NoZip,
    [switch]$StripDebug
)

$ErrorActionPreference = "Stop"
$root = Split-Path -Parent $PSScriptRoot
Push-Location $root
try {
    $goosValue = if ($GOOS) { $GOOS } else { (& go env GOOS).Trim() }
    $goarchValue = if ($GOARCH) { $GOARCH } else { (& go env GOARCH).Trim() }
    $hostGOOS = (& go env GOOS).Trim()
    $hostGOARCH = (& go env GOARCH).Trim()
    $ext = if ($goosValue -eq "windows") { ".exe" } else { "" }
    $commit = (& git rev-parse --short HEAD).Trim()
    $ver = if ($Version) { $Version } else { "dev-$commit" }
    if ($ver -notmatch '^[A-Za-z0-9][A-Za-z0-9._-]*$') {
        throw "版本号只能包含字母、数字、点、下划线与短横线: $ver"
    }
    $pkgName = "ada-$ver-$goosValue-$goarchValue"
    if ($LLMEndpoint -and -not $LLMModel) {
        throw "-LLMEndpoint 与 -LLMModel 必须同时提供"
    }
    if ($LLMModel -and -not $LLMEndpoint) {
        throw "-LLMEndpoint 与 -LLMModel 必须同时提供"
    }

    if (-not $OutputDir) { throw "-OutputDir 不能为空" }
    # 允许绝对路径;相对路径一律相对仓库根,避免受调用方 cwd 影响。
    $outRoot = if ([IO.Path]::IsPathRooted($OutputDir)) { $OutputDir } else { Join-Path $root $OutputDir }
    New-Item -ItemType Directory -Force -Path $outRoot | Out-Null
    $pkg = Join-Path $outRoot $pkgName
    $binDir = Join-Path $pkg "bin"
    if ((Test-Path $pkg) -and $SkipBuild) {
        if (-not (Test-Path $binDir)) {
            throw "-SkipBuild 要求已有 $binDir"
        }
        # 复用 bin;清掉上次生成的数据,避免旧 preset/清单残留。
        foreach ($sub in @("plugins", "presets", "data", "workspace", "docs")) {
            $old = Join-Path $pkg $sub
            if (Test-Path $old) { Remove-Item -LiteralPath $old -Recurse -Force }
        }
    } elseif (Test-Path $pkg) {
        $resolved = (Resolve-Path $pkg).Path
        $prefix = $outRoot.TrimEnd([IO.Path]::DirectorySeparatorChar) + [IO.Path]::DirectorySeparatorChar
        if (-not $resolved.StartsWith($prefix, [StringComparison]::OrdinalIgnoreCase)) {
            throw "拒绝删除非输出目录: $resolved"
        }
        Remove-Item -LiteralPath $resolved -Recurse -Force
    }
    New-Item -ItemType Directory -Force -Path $pkg | Out-Null

    # 装配基目录锚点:ada-rpc-agent 从 preset 目录向上找 go.mod 或 .ada-base。
    # 发布包没有 go.mod，靠这个文件让 preset 里 data/workspace 的相对路径落在包根。
    Set-Content -LiteralPath (Join-Path $pkg ".ada-base") -Value "ada_go_v2 package root" -Encoding utf8

    if (-not $SkipBuild) {
        # 用当前宿主(PowerShell 5.1 或 pwsh 7)启动子脚本:两者语法已兼容,
        # 不在没有 pwsh 的机器上硬编码解释器名。
        $shell = (Get-Process -Id $PID).Path
        $buildArgs = @("-NoProfile", "-File", (Join-Path $root "scripts/build.ps1"),
            "-OutputDir", $binDir, "-GOOS", $goosValue, "-GOARCH", $goarchValue)
        if ($StripDebug) { $buildArgs += "-StripDebug" }
        & $shell @buildArgs
        if ($LASTEXITCODE -ne 0) {
            throw "编译失败(exit $LASTEXITCODE)"
        }
    }
    foreach ($cmd in @("ada", "ada-rpc-agent", "ada-demo", "ada-ts-demo")) {
        $exe = Join-Path $binDir ($cmd + $ext)
        if (-not (Test-Path $exe)) {
            throw "缺少二进制: $exe"
        }
    }

    # 从 compiled builtins 生成清单;不复制 examples 里的手写清单。
    $plugins = Join-Path $pkg "plugins"
    & go run ./scripts/tools/builtins-dump -out $plugins
    if ($LASTEXITCODE -ne 0) {
        throw "导出内置插件清单失败(exit $LASTEXITCODE)"
    }

    # TS 插件不在 builtins-dump 的导出范围,按目录复制源码与清单:
    #   - plugins/tool 下的内置 TS 工具(text-helper/shell-helper);
    #   - examples/<示例>/plugins/<id> 下 kind=ts 的示例脚本插件。
    # 示例插件按清单自动发现,新增/改名示例时不需要改本脚本;它们默认不在 release
    # preset 的 allowlist 里,因此随包发布但不启用,启用方式与其他未启用插件一致。
    $tsPluginDirs = [ordered]@{
        "text-helper"   = (Join-Path $root "plugins/tool/text-helper")
        "shell-helper"  = (Join-Path $root "plugins/tool/shell-helper")
        "memory-helper" = (Join-Path $root "plugins/tool/memory-helper")
        # http-helper 是联网工具:默认随 agent-release 启用;不想给模型网络能力时
        # 在 preset 的 disable 里加回它(见 docs/tool-http-helper.md)。
        "http-helper"   = (Join-Path $root "plugins/tool/http-helper")
        # plugin-helper / skill-helper 是开发工具(插件生成、技能读写),随发布 preset 默认启用。
        "plugin-helper" = (Join-Path $root "plugins/tool/plugin-helper")
        "skill-helper"  = (Join-Path $root "plugins/tool/skill-helper")
    }
    $examplesRoot = Join-Path $root "examples"
    foreach ($mf in @(Get-ChildItem $examplesRoot -Recurse -Filter "plugin.json" -File)) {
        $rel = $mf.FullName.Substring($examplesRoot.Length + 1).Replace("\", "/")
        # 只收 examples/<示例>/plugins/<id>/plugin.json;examples 其它层级的清单是开发用
        # Go 示例清单,已由 builtins-dump 导出,重复复制会覆盖自动生成的同名清单。
        if ($rel -notmatch "^[^/]+/plugins/[^/]+/plugin.json$") { continue }
        $manifest = Get-Content $mf.FullName -Raw | ConvertFrom-Json
        if ($manifest.kind -ne "ts") { continue }
        $id = Split-Path -Leaf $mf.DirectoryName
        if ($manifest.id -ne $id) {
            throw "示例插件 examples/$rel 的 id($($manifest.id))与目录名($id)不一致"
        }
        if ($tsPluginDirs.Contains($id)) {
            throw "TS 插件 id 重复: $id($rel)"
        }
        $tsPluginDirs[$id] = $mf.DirectoryName
    }
    foreach ($id in $tsPluginDirs.Keys) {
        $src = $tsPluginDirs[$id]
        $dest = Join-Path $plugins $id
        # 与 builtins 导出的 Go 清单同名时宁可失败:同名插件会让 loader 按目录覆盖,
        # 发布包到底跑哪份实现就变得不可预测。
        if (Test-Path $dest) {
            throw "TS 插件 $id 与 builtins 导出的插件同名"
        }
        New-Item -ItemType Directory -Force -Path $dest | Out-Null
        Copy-Item (Join-Path $src "*") -Destination $dest -Recurse -Force
        $entry = (Get-Content (Join-Path $src "plugin.json") -Raw | ConvertFrom-Json).entry
        if (-not $entry -or -not (Test-Path (Join-Path $dest $entry))) {
            throw "TS 插件 $id 的入口缺失: $src/plugin.json 的 entry($entry)不在复制结果里"
        }
    }

    $ids = @(Get-ChildItem $plugins -Directory | Select-Object -ExpandProperty Name | Sort-Object)
    if ($ids.Count -eq 0) {
        throw "没有导出任何内置插件"
    }

    $agentBase = @(
        "agent-loop", "compaction-basic", "fs-local", "llm-replay",
        "seam-fs", "seam-llm", "seam-prompts", "seam-session", "seam-tools",
        "session-memory", "session-stats", "system-prompt", "text-helper", "memory-helper", "tool-helper"
    )
    # shell 栈只在 agent preset 里打开;workspace preset 保持无命令执行。
    $shellAllow = @(
        "seam-shell", "shell-local", "shell-guard", "seam-sandbox", "sandbox-none", "shell-helper"
    )
        # 插件开发工具随默认 agent preset 启用:生成/体检只动工作区文件,写入仍受 fs 策略约束;
    # Go 插件不在生成范围内(见 docs/tool-plugin-helper.md)。
    $agentAllow = $agentBase + $shellAllow + @("plugin-helper", "skill-helper", "http-helper", "seam-secrets", "secrets-env")
    $workspaceAllow = $agentBase + @(
        "seam-workspace", "workspace-helper", "workspace-guard", "workspace-local"
    )
    $missing = @($agentAllow | Where-Object { $_ -notin $ids })
    if ($missing.Count -gt 0) {
        throw "allowlist 引用了不存在的插件: $($missing -join ', ')"
    }

    $dataDir = Join-Path $pkg "data"
    New-Item -ItemType Directory -Force -Path $dataDir, $plugins | Out-Null
    Copy-Item "examples/agent/replay.jsonl" (Join-Path $dataDir "agent-replay.jsonl") -Force
    Copy-Item "examples/workspace/replay.jsonl" (Join-Path $dataDir "workspace-replay.jsonl") -Force

    $workspaceDir = Join-Path $pkg "workspace"
    $docsDir = Join-Path $pkg "docs"
    New-Item -ItemType Directory -Force -Path $workspaceDir, $docsDir | Out-Null
    Copy-Item "examples/agent/workspace/a.txt" (Join-Path $workspaceDir "a.txt") -Force
    Copy-Item "examples/workspace/docs/a.txt" (Join-Path $docsDir "a.txt") -Force

    $presetsDir = Join-Path $pkg "presets"
    New-Item -ItemType Directory -Force -Path $presetsDir | Out-Null
    $utf8 = New-Object System.Text.UTF8Encoding($false)
    $replayModels = @([ordered]@{ id = "replay"; contextWindow = 8192; maxOutput = 1024 })
    $disableAgent = @($ids | Where-Object { $_ -notin $agentAllow })
    $disableWorkspace = @($ids | Where-Object { $_ -notin $workspaceAllow })

    if ($goosValue -eq "windows") {
        $shellProgram = "cmd.exe"
        $shellArgs = @("/C")
    } else {
        $shellProgram = "/bin/sh"
        $shellArgs = @("-c")
    }
    # shell 栈的公共配置:sandbox 只在策略层生效,命令仍由用户权限执行。
    $shellPatch = [ordered]@{
        "sandbox-none" = @{ mode = "workspace-write" }
        "shell-local" = @{ workspace = "workspace"; tmp = "workspace/.ada-tmp" }
        "shell-guard" = @{ backend = "sandbox-none"; allowShell = $false }
        "shell-helper" = @{ program = $shellProgram; args = $shellArgs; maxTimeoutSeconds = 120 }
    }

    $agentPatch = [ordered]@{
        "agent-loop" = @{ model = "replay" }
        "llm-replay" = [ordered]@{
            path = "data/agent-replay.jsonl"; models = $replayModels; defaultModel = "replay"
        }
        "fs-local" = @{ workspace = "workspace"; tmp = "workspace/.ada-tmp" }
        # http_search 的 DeepSeek key 走 seam/secrets:allow 只放这一个名字,其它 secret 仍读不到。
        "secrets-env" = @{ allow = @("DEEPSEEK_API_KEY") }
        # 记忆工具默认随包启用;模型需要知道什么时候写入/检索/忘记。
        "memory-helper" = @{ autoRecall = $true }
        "system-prompt" = @{
            tools = "长期记忆:`n- 用户要求记住事实/偏好时调用 memory_write;`n- 回答涉及既往信息时先 memory_search 再引用;`n- 用户要求忘记时调用 memory_forget。`n工具发现:`n- 工具较多或不确定用哪个时,先 tool_find 查询;`n- 已知工具名时用 tool_exec 按名字调用,参数放在 args 对象里。`n插件开发:`n- 用户要新建插件时用 plugin_scaffold(只生成 TS,生成即可被 preset 装配);`n- 查看/体检已有插件用 plugin_list / plugin_show;`n- 把插件接入 preset 用 plugin_preset。`n联网:`n- 抓取网页用 http_request(默认把 HTML 清洗成正文,需要原文时传 clean:false);`n- 搜索网页用 http_search(DeepSeek 需 key,无 key 自动用 DuckDuckGo)。`n技能:`n- 先 skill_list 查可用技能;`n- 需要步骤细节时 skill_show 读 SKILL.md(可带 file 读附带文件);`n- 新建技能用 skill_scaffold。"
        }
    }
    foreach ($k in $shellPatch.Keys) { $agentPatch[$k] = $shellPatch[$k] }

    $agentPreset = [ordered]@{
        name = "agent-release"
        plugins = @("../plugins")
        disable = $disableAgent
        patch = $agentPatch
    }
    $workspacePreset = [ordered]@{
        name = "workspace-release"
        plugins = @("../plugins")
        disable = $disableWorkspace
        patch = [ordered]@{
            "agent-loop" = @{ model = "replay" }
            "llm-replay" = [ordered]@{
                path = "data/workspace-replay.jsonl"; models = $replayModels; defaultModel = "replay"
            }
            "fs-local" = @{ workspace = ""; tmp = "workspace/.ada-tmp" }
            "memory-helper" = @{ autoRecall = $true }
            "system-prompt" = @{
                tools = "长期记忆:`n- 用户要求记住事实/偏好时调用 memory_write;`n- 回答涉及既往信息时先 memory_search 再引用;`n- 用户要求忘记时调用 memory_forget。`n工具发现:`n- 工具较多或不确定用哪个时,先 tool_find 查询;`n- 已知工具名时用 tool_exec 按名字调用,参数放在 args 对象里。"
            }
            "workspace-local" = [ordered]@{
                active = "repo"
                workspaces = @(
                    [ordered]@{ id = "repo"; name = "示例工作区"; root = "workspace"; ignore = @(".git", "bin", "dist") },
                    [ordered]@{ id = "docs"; name = "只读文档"; root = "docs"; readOnly = $true }
                )
            }
            "workspace-guard" = @{ denyWrite = @(".git", ".ada-tmp"); denyExec = @(".git") }
        }
    }
    [System.IO.File]::WriteAllText((Join-Path $presetsDir "agent-release.preset.json"),
        (($agentPreset | ConvertTo-Json -Depth 10) + "`n"), $utf8)
    # http-release:与 agent-release 同构,只把 http-helper 从 disable 里去掉。
    # 单独一份 preset 而不是改默认值:启用它等于允许模型对外发请求,是显式选择。
    $httpPreset = [ordered]@{
        name = "agent-release-http"
        plugins = @("../plugins")
        disable = @($disableAgent | Where-Object { $_ -ne "http-helper" })
        patch = $agentPatch
    }
    [System.IO.File]::WriteAllText((Join-Path $presetsDir "agent-release-http.preset.json"),
        (($httpPreset | ConvertTo-Json -Depth 10) + "`n"), $utf8)
    [System.IO.File]::WriteAllText((Join-Path $presetsDir "workspace-release.preset.json"),
        (($workspacePreset | ConvertTo-Json -Depth 10) + "`n"), $utf8)

    if ($LLMEndpoint -and $LLMModel) {
        $realAllow = @($agentAllow | Where-Object { $_ -ne "llm-replay" }) + @("llm-openai")
        $disableReal = @($ids | Where-Object { $_ -notin $realAllow })
        $realPatch = [ordered]@{
            "agent-loop" = @{ model = $LLMModel }
            "llm-openai" = @{ endpoint = $LLMEndpoint; model = $LLMModel; apiKeyEnv = "ADA_LLM_API_KEY"; maxTokens = 512 }
            "fs-local" = @{ workspace = "workspace"; tmp = "workspace/.ada-tmp" }
        }
        foreach ($k in $shellPatch.Keys) { $realPatch[$k] = $shellPatch[$k] }
        $realPreset = [ordered]@{
            name = "agent-real-release"
            plugins = @("../plugins")
            disable = $disableReal
            patch = $realPatch
        }
        [System.IO.File]::WriteAllText((Join-Path $presetsDir "agent-real-release.preset.json"),
            (($realPreset | ConvertTo-Json -Depth 10) + "`n"), $utf8)
    }

    $binNames = @(Get-ChildItem $binDir -File | Select-Object -ExpandProperty Name)
    $readme = @"
Ada 发布包 $pkgName

在包根目录运行(不要回退到源码目录):
  bin/ada$ext plan  --preset presets/agent-release.preset.json
  bin/ada$ext agent --preset presets/agent-release.preset.json -message "读取 a.txt 并总结"
  bin/ada$ext agent --preset presets/workspace-release.preset.json --workspace docs -message "查询工作区并读取 a.txt"

plugins/ 的 Go 清单由编译进二进制的 builtins 自动导出,与源码手写清单无关;
TS 插件(text-helper / shell-helper / 示例脚本插件)随包复制源码,启用时现场编译;
presets/ 只引用 ../plugins、workspace/ 与 data/,不包含开发目录路径。
真实端点可用 LLMEndpoint/LLMModel 参数生成 agent-real-release.preset.json。

环境变量:把 .env.example 复制为 .env 填写(API key 等),启动时补进环境;
真实环境变量优先,文件内容不写日志。ada 读当前目录的 .env,ada-rpc-agent 读它自己
可执行文件旁的 .env(不读工作区)。写法见源码仓库 docs/env-file.md。
"@
    [System.IO.File]::WriteAllText((Join-Path $pkg "README.txt"), ($readme.TrimEnd() + "`n"), $utf8)
    $envExample = @"
# 复制为 .env 后按需填写;真实环境变量优先于本文件。语法见源码仓库 docs/env-file.md。
# ——— 模型(OpenAI 兼容端点)———
# ADA_LLM_API_KEY=sk-...
# ADA_LLM_ENDPOINT=https://api.deepseek.com
# ADA_LLM_MODEL=deepseek-chat
# ——— http_search 的 DeepSeek 网页搜索(需在 preset 里启用 seam-secrets + secrets-env)———
# ADA_SECRET_DEEPSEEK_API_KEY=sk-...
"@
    [System.IO.File]::WriteAllText((Join-Path $pkg ".env.example"), ($envExample.TrimEnd() + "`n"), $utf8)

    $meta = [ordered]@{
        name = $pkgName; version = $ver; commit = $commit
        goos = $goosValue; goarch = $goarchValue
        plugins = $ids; binaries = $binNames
    }
    [System.IO.File]::WriteAllText((Join-Path $pkg "package.json"),
        (($meta | ConvertTo-Json -Depth 6) + "`n"), $utf8)

    # 同平台时用打包后的二进制自检,确保 preset 不依赖开发目录。
    if ($goosValue -eq $hostGOOS -and $goarchValue -eq $hostGOARCH) {
        $ada = Join-Path $binDir ("ada" + $ext)
        Push-Location $pkg
        try {
            foreach ($name in @("agent-release.preset.json", "agent-release-http.preset.json", "workspace-release.preset.json")) {
                & $ada plan --preset (Join-Path "presets" $name) | Out-Null
                if ($LASTEXITCODE -ne 0) {
                    throw "包内 plan 自检失败: $name"
                }
            }
            # 整目录自检:清单扫描 + 计划 + TS 现场编译。复制进包的示例脚本插件
            # 只有在这一步才会真正经过 esbuild,能发现漏拷源码或 entry 写错。
            & $ada check plugins | Out-Null
            if ($LASTEXITCODE -ne 0) {
                throw "包内 check 自检失败: plugins/"
            }
        } finally {
            Pop-Location
        }
        Write-Host "包内自检通过"
    } else {
        Write-Host "交叉编译目标,跳过包内运行自检"
    }

    $zip = Join-Path $outRoot ($pkgName + ".zip")
    if ($NoZip) {
        if (Test-Path $zip) { Remove-Item -LiteralPath $zip -Force }
    } else {
        if (Test-Path $zip) { Remove-Item -LiteralPath $zip -Force }
        Compress-Archive -Path $pkg -DestinationPath $zip -Force
        Write-Host "打包完成: $zip"
    }
    Write-Host "发布目录: $pkg"
}
finally {
    Pop-Location
}
