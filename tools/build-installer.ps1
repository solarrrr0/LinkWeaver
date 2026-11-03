param([string]$Binary = '')
$ErrorActionPreference = 'Stop'
$projectRoot = Split-Path -Parent $PSScriptRoot
$version = [regex]::Match((Get-Content -LiteralPath (Join-Path $projectRoot 'update_windows.go') -Raw),'const appVersion = "([0-9.]+)"').Groups[1].Value
if (!$version) { throw 'Version not found' }
if (!$Binary) { $Binary = Join-Path $projectRoot 'LinkWeaver.exe' }
$payload = Join-Path $projectRoot '.tools\setup-payload'
$compiler = Join-Path $projectRoot '.tools\inno\ISCC.exe'
if (!(Test-Path -LiteralPath $compiler)) { $compiler = (Get-Command ISCC.exe -ErrorAction Stop).Source }
New-Item -ItemType Directory -Force -Path $payload,(Join-Path $payload 'bin'),(Join-Path $payload 'licenses') | Out-Null
try {
    Copy-Item -LiteralPath $Binary -Destination (Join-Path $payload 'LinkWeaver.exe') -Force
    Copy-Item -LiteralPath (Join-Path $projectRoot 'bin\mihomo.exe') -Destination (Join-Path $payload 'bin\mihomo.exe') -Force
    Copy-Item -LiteralPath (Join-Path $projectRoot 'installer\LinkWeaver.ico') -Destination $payload -Force
    Copy-Item -LiteralPath (Join-Path $projectRoot 'README.md') -Destination $payload -Force
    Get-ChildItem -LiteralPath (Join-Path $projectRoot 'licenses') -File | Copy-Item -Destination (Join-Path $payload 'licenses') -Force
    Copy-Item -LiteralPath (Join-Path $projectRoot 'LICENSE') -Destination (Join-Path $payload 'licenses\LinkWeaver-LICENSE.txt') -Force
    & $compiler ('/DBuildRoot=' + $payload) (Join-Path $projectRoot 'installer\LinkWeaver.iss')
    if ($LASTEXITCODE -ne 0) { throw 'Installer compilation failed' }
    Get-Item -LiteralPath (Join-Path $projectRoot ('dist\LinkWeaver-Setup-'+$version+'-Windows-x64.exe')) | Select-Object FullName,Length
} finally {
    $resolvedPayload = [IO.Path]::GetFullPath($payload)
    $expectedPayload = [IO.Path]::GetFullPath((Join-Path $projectRoot '.tools\setup-payload'))
    if ($resolvedPayload -ne $expectedPayload) { throw 'Unexpected installer staging path' }
    if (Test-Path -LiteralPath $resolvedPayload) {
        Add-Type -AssemblyName Microsoft.VisualBasic
        [Microsoft.VisualBasic.FileIO.FileSystem]::DeleteDirectory($resolvedPayload,[Microsoft.VisualBasic.FileIO.UIOption]::OnlyErrorDialogs,[Microsoft.VisualBasic.FileIO.RecycleOption]::SendToRecycleBin)
    }
}
