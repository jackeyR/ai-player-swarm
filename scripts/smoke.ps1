# 冒烟脚本（Windows PowerShell）：起 SUT → 跑 runner → 收尾 → 出报告。
#
# 用法：
#   .\scripts\smoke.ps1
#   .\scripts\smoke.ps1 -Plan plans/smoke.yaml -TimeoutSec 180
#
# 如果提示「未对文件进行数字签名」，用配套的 scripts\smoke.cmd：
#   .\scripts\smoke.cmd
# 或者执行一次：
#   powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\smoke.ps1
#
# 这个脚本存在的意义：把「三条命令 + 三个终端 + 别忘了关进程」压缩成一条，
# 让 CI 和新接手的人都能一条命令看到结果。

[CmdletBinding()]
param(
    [string]$Plan = "plans/smoke.yaml",
    [string]$SutConfig = "deploy/sut.smoke.yaml",
    [int]$TimeoutSec = 180,
    [switch]$KeepRunning
)

$ErrorActionPreference = "Stop"

# Go 与 Python 都输出 UTF-8；控制台默认是本地代码页，不设一下会看到乱码。
try { [Console]::OutputEncoding = [System.Text.Encoding]::UTF8 } catch { }
$env:PYTHONIOENCODING = "utf-8"
$env:PYTHONUTF8 = "1"

$root = Split-Path -Parent $PSScriptRoot
Set-Location $root

$exitCode = 0
$sut = $null

try {
    Write-Host "== 1/4 编译 ==" -ForegroundColor Cyan
    New-Item -ItemType Directory -Force -Path bin | Out-Null
    go build -o bin/sutd.exe ./cmd/sutd
    if ($LASTEXITCODE -ne 0) { throw "编译 sutd 失败" }
    go build -o bin/runner.exe ./cmd/runner
    if ($LASTEXITCODE -ne 0) { throw "编译 runner 失败" }

    Write-Host "== 2/4 启动被测系统（$SutConfig）==" -ForegroundColor Cyan
    # 上一轮残留的 sutd 会占用 8080/8090/9090，先清掉。
    Get-Process sutd -ErrorAction SilentlyContinue | Stop-Process -Force
    Start-Sleep -Milliseconds 300
    $sut = Start-Process -FilePath ".\bin\sutd.exe" -ArgumentList @("-config", $SutConfig) `
        -PassThru -WindowStyle Hidden

    $ready = $false
    for ($i = 0; $i -lt 40; $i++) {
        Start-Sleep -Milliseconds 200
        try {
            $r = Invoke-WebRequest -Uri "http://127.0.0.1:9090/healthz" -TimeoutSec 2 -UseBasicParsing
            if ($r.StatusCode -eq 200) { $ready = $true; break }
        } catch { }
    }
    if (-not $ready) { throw "被测系统 8 秒内没有就绪（端口被占用？）" }

    Write-Host "== 3/4 跑负载（$Plan）==" -ForegroundColor Cyan
    & .\bin\runner.exe -plan $Plan -metrics :9091 -timeout "${TimeoutSec}s"
    if ($LASTEXITCODE -ne 0) { throw "runner 退出码 $LASTEXITCODE" }

    $latest = Get-ChildItem -Path results -Directory |
        Sort-Object LastWriteTime -Descending | Select-Object -First 1
    if (-not $latest) { throw "没有产出结果目录" }

    Write-Host "== 4/4 生成分析报告 ==" -ForegroundColor Cyan
    $ref = "analysis/reference.example.json"
    if (Test-Path $ref) {
        & python analysis/analyze.py $latest.FullName --reference $ref
    } else {
        & python analysis/analyze.py $latest.FullName
    }

    Write-Host ""
    Write-Host ("结果目录： " + $latest.FullName) -ForegroundColor Green
    Write-Host ("报告：     " + (Join-Path $latest.FullName "analysis\report.md")) -ForegroundColor Green
    Write-Host ("曲线图：   " + (Join-Path $latest.FullName "analysis\cdf.svg")) -ForegroundColor Green
}
catch {
    Write-Host ("失败： " + $_.Exception.Message) -ForegroundColor Red
    $exitCode = 1
}
finally {
    if (-not $KeepRunning) {
        Write-Host "== 收尾：关闭被测系统 ==" -ForegroundColor Cyan
        if ($sut -and -not $sut.HasExited) { Stop-Process -Id $sut.Id -Force -ErrorAction SilentlyContinue }
        Get-Process sutd -ErrorAction SilentlyContinue | Stop-Process -Force -ErrorAction SilentlyContinue
    }
}

exit $exitCode
