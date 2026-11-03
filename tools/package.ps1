param([string]$Binary = '')
$ErrorActionPreference = 'Stop'
& (Join-Path $PSScriptRoot 'build-installer.ps1') -Binary $Binary
