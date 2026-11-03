param([string]$Binary = '')
$ErrorActionPreference = 'Stop'
$projectRoot = Split-Path -Parent $PSScriptRoot
if (!$Binary) { $Binary = Join-Path $projectRoot 'LinkWeaver.exe' }
$versionSource = Get-Content -LiteralPath (Join-Path $projectRoot 'update_windows.go') -Raw
$version = [regex]::Match($versionSource,'const appVersion = "([0-9.]+)"').Groups[1].Value
if (!$version) { throw 'Version not found' }
$stage = Join-Path $projectRoot '.tools\release-payload'
$dist = Join-Path $projectRoot 'dist'
New-Item -ItemType Directory -Force -Path $dist,$stage,(Join-Path $stage 'bin') | Out-Null
try {
    Copy-Item -LiteralPath $Binary -Destination (Join-Path $stage 'LinkWeaver.exe') -Force
    Copy-Item -LiteralPath (Join-Path $projectRoot 'bin\mihomo.exe') -Destination (Join-Path $stage 'bin\mihomo.exe') -Force
    $files = @{}
    foreach ($name in @('LinkWeaver.exe','bin/mihomo.exe')) {
        $files[$name] = (Get-FileHash -LiteralPath (Join-Path $stage $name) -Algorithm SHA256).Hash.ToLowerInvariant()
    }
    @{product='LinkWeaver';version=$version;files=$files} | ConvertTo-Json -Depth 4 | Set-Content -LiteralPath (Join-Path $stage 'release.json') -Encoding utf8NoBOM
    $zip = Join-Path $dist ('LinkWeaver-Update-'+$version+'-Windows-x64.zip')
    Compress-Archive -LiteralPath (Join-Path $stage 'LinkWeaver.exe'),(Join-Path $stage 'bin'),(Join-Path $stage 'release.json') -DestinationPath $zip -Force
    & (Join-Path $PSScriptRoot 'build-installer.ps1') -Binary $Binary
    foreach ($file in @($zip,(Join-Path $dist ('LinkWeaver-Setup-'+$version+'-Windows-x64.exe')))) {
        $hash = (Get-FileHash -LiteralPath $file -Algorithm SHA256).Hash.ToLowerInvariant()
        ($hash+'  '+(Split-Path -Leaf $file)) | Set-Content -LiteralPath ($file+'.sha256') -Encoding ascii
    }
    Write-Output ('Release ready: '+$version)
} finally {
    $resolvedStage = [IO.Path]::GetFullPath($stage)
    if ($resolvedStage -ne [IO.Path]::GetFullPath((Join-Path $projectRoot '.tools\release-payload'))) { throw 'Unexpected release staging path' }
    if (Test-Path -LiteralPath $resolvedStage) {
        Add-Type -AssemblyName Microsoft.VisualBasic
        [Microsoft.VisualBasic.FileIO.FileSystem]::DeleteDirectory($resolvedStage,[Microsoft.VisualBasic.FileIO.UIOption]::OnlyErrorDialogs,[Microsoft.VisualBasic.FileIO.RecycleOption]::SendToRecycleBin)
    }
}
