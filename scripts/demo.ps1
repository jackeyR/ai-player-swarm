# 一条命令开玩：起被测系统 → 起陪练 bot → 你自己进去打。
#
# 用法：
#   .\scripts\demo.cmd                        # 1v1，保证能马上排上（推荐先用这个）
#   .\scripts\demo.cmd -Account me2           # 换个账号（同名重登会让旧 token 失效）
#   .\scripts\demo.cmd -Five                  # 5v5：需要等 10 个人凑齐，等人时间更长
#
# 这个脚本存在的意义：把「起 SUT、起陪练、连客户端、收尾」四件事压成一条，
# 并且保证退出时把后台进程清干净——手动开三个终端最容易忘掉的是收尾。

[CmdletBinding()]
param(
    [string]$Account = "me",
    [switch]$Five,
    [switch]$Plain,
    [switch]$NoBots,
    [int]$TimeoutSec = 300
)

$ErrorActionPreference = "Stop"
try { [Console]::OutputEncoding = [System.Text.Encoding]::UTF8 } catch { }

$root = Split-Path -Parent $PSScriptRoot
Set-Location $root

$sutConfig = if ($Five) { "deploy/sut.smoke.yaml" } else { "deploy/sut.duel.yaml" }
$plan      = if ($Five) { "plans/smoke.yaml" }    else { "plans/duel.yaml" }

Write-Host "== 1/4 编译 ==" -ForegroundColor Cyan
New-Item -ItemType Directory -Force -Path bin | Out-Null
go build -o bin/sutd.exe ./cmd/sutd
if ($LASTEXITCODE -ne 0) { throw "编译 sutd 失败" }
go build -o bin/client.exe ./cmd/client
if ($LASTEXITCODE -ne 0) { throw "编译 client 失败" }
if (-not $NoBots) {
    go build -o bin/runner.exe ./cmd/runner
    if ($LASTEXITCODE -ne 0) { throw "编译 runner 失败" }
}

Write-Host "== 2/4 清理上一轮残留进程 ==" -ForegroundColor Cyan
Get-Process sutd, runner -ErrorAction SilentlyContinue | Stop-Process -Force -ErrorAction SilentlyContinue
Start-Sleep -Milliseconds 400

$sut = $null
$runner = $null
try {
    Write-Host "== 3/4 启动被测系统（$sutConfig）==" -ForegroundColor Cyan
    $sut = Start-Process -FilePath ".\bin\sutd.exe" -ArgumentList @("-config", $sutConfig) `
        -PassThru -WindowStyle Hidden
    $ready = $false
    for ($i = 0; $i -lt 40; $i++) {
        Start-Sleep -Milliseconds 200
        try {
            $r = Invoke-WebRequest -Uri "http://127.0.0.1:9090/healthz" -TimeoutSec 2 -UseBasicParsing
            if ($r.StatusCode -eq 200) { $ready = $true; break }
        } catch { }
    }
    if (-not $ready) { throw "被测系统 8 秒内没有就绪（8080/8090/9090 被占用？）" }

    if (-not $NoBots) {
        Write-Host "启动陪练队列（$plan）…" -ForegroundColor Cyan
        $runner = Start-Process -FilePath ".\bin\runner.exe" `
            -ArgumentList @("-plan", $plan, "-metrics", ":9091", "-timeout", "600s") `
            -PassThru -WindowStyle Hidden
        Start-Sleep -Seconds 3
    }

    Write-Host ""
    Write-Host "== 4/4 进入对局 ==" -ForegroundColor Green
    if ($Five) {
        Write-Host "5v5：需要凑齐 10 人，可能要等十几秒（对局期间也有对手在动）。" -ForegroundColor Yellow
    } else {
        Write-Host "1v1：通常几秒内就能排上。" -ForegroundColor Yellow
    }
    Write-Host "操作：WASD 移动 · 空格攻击 · Q 退出（排队中按 Q 取消排队）" -ForegroundColor Yellow
    Write-Host ""

    $clientArgs = @("-account", $Account, "-out", "results/human-$Account")
    if ($Plain) { $clientArgs += "-plain" }

    & .\bin\client.exe @clientArgs
}
finally {
    Write-Host ""
    Write-Host "== 收尾：关闭陪练与被测系统 ==" -ForegroundColor Cyan
    if ($runner -and -not $runner.HasExited) { Stop-Process -Id $runner.Id -Force -ErrorAction SilentlyContinue }
    if ($sut -and -not $sut.HasExited) { Stop-Process -Id $sut.Id -Force -ErrorAction SilentlyContinue }
    Get-Process runner, sutd -ErrorAction SilentlyContinue | Stop-Process -Force -ErrorAction SilentlyContinue
    Write-Host "真人记录：results\human-$Account\（bots.jsonl / sessions.json / trace.jsonl）" -ForegroundColor Green
    Write-Host "用它做保真度对比：" -ForegroundColor Green
    Write-Host "  python analysis\analyze.py results\<bot-run> --reference results\human-$Account\sessions.json" -ForegroundColor Green
}
