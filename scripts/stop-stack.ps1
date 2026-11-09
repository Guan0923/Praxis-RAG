# Stop the local MySQL/Qdrant/Redis/Kafka stack. Data volumes are preserved.
# Add -RemoveVolumes to delete all stack data as well.
# Kafka replaces the former RocketMQ services; the optional Debezium profile
# is stopped together with the rest of the compose project.

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
