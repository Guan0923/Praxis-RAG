# Stop the local MySQL + Qdrant stack. Data volumes are preserved.
# Add -RemoveVolumes to delete MySQL/Qdrant data as well.

param([switch]$RemoveVolumes)

$ErrorActionPreference = "Stop"
$root = Split-Path -Parent $PSScriptRoot
Set-Location $root

function Invoke-Compose {
    param([string[]]$Arguments)
    if (Get-Command docker-compose -ErrorAction SilentlyContinue) {
        & docker-compose @Arguments
    }
    else {
        & docker compose @Arguments
    }
}

if ($RemoveVolumes) {
    Invoke-Compose @("down", "-v")
}
else {
    Invoke-Compose @("down")
}
