param([switch]$RunTests)
$ErrorActionPreference = 'Stop'
$projectRoot = $PSScriptRoot
$runtimeRoot = $projectRoot
if ((Split-Path -Leaf $projectRoot) -eq 'source' -and (Test-Path -LiteralPath (Join-Path (Split-Path -Parent $projectRoot) 'bin\mihomo.exe'))) { $runtimeRoot = Split-Path -Parent $projectRoot }
$env:GOCACHE = Join-Path $runtimeRoot '.tools\go-cache'
$env:GOPATH = Join-Path $runtimeRoot '.tools\go-path'
$env:GOTMPDIR = Join-Path $runtimeRoot '.tools\tmp'
$env:TEMP = $env:GOTMPDIR
$env:TMP = $env:GOTMPDIR
$env:GOTOOLCHAIN = 'local'
New-Item -ItemType Directory -Force -Path $env:GOCACHE,$env:GOPATH,$env:GOTMPDIR | Out-Null
$goExe = Join-Path $runtimeRoot '.tools\go\bin\go.exe'
if (!(Test-Path -LiteralPath $goExe)) { $goExe = (Get-Command go -ErrorAction Stop).Source }
Push-Location -LiteralPath $projectRoot
try {
    & $goExe mod download all
    if ($LASTEXITCODE -ne 0) { throw 'Dependency download failed' }
    if ($RunTests) {
        & $goExe test ./...
        if ($LASTEXITCODE -ne 0) { throw 'Tests failed' }
    }
    & $goExe build -trimpath -ldflags '-s -w -H=windowsgui' -o (Join-Path $runtimeRoot 'LinkWeaver.exe') .
    if ($LASTEXITCODE -ne 0) { throw 'Build failed' }
    Write-Output 'Built LinkWeaver.exe'
} finally { Pop-Location }
