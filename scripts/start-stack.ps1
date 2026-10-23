# Start the local MySQL + Qdrant stack, migrate legacy state, then run rag-service.
#
#   .\scripts\start-stack.ps1                 # full start
#   .\scripts\start-stack.ps1 -MigrateOnly    # only start dependencies + migrate
#   .\scripts\start-stack.ps1 -SkipMigrate    # start without importing legacy data
#
# Run this from a normal PowerShell window (not the sandboxed agent shell), because
# Docker Desktop exposes a named pipe that the agent sandbox cannot open.

param(
    [switch]$MigrateOnly,
    [switch]$SkipMigrate,
    [string]$LegacyRoot = (Join-Path $env:USERPROFILE ".praxis-rag\data")
)

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

if (-not (Test-Path ".env")) {
    Copy-Item ".env.example" ".env"
    throw "Created .env from .env.example. Fill in RAG_MYSQL_PASSWORD, MYSQL_ROOT_PASSWORD and RAG_STATE_ENCRYPTION_KEY, then rerun."
}

# Load .env into this process without printing secrets.
Get-Content ".env" | ForEach-Object {
    $line = $_.Trim()
    if (-not $line -or $line.StartsWith("#")) { return }
    if ($line -match '^([A-Za-z_][A-Za-z0-9_]*)\s*=\s*(.*)$') {
        $name = $Matches[1]
        $value = $Matches[2].Trim()
        if ($value.Length -ge 2 -and (
            ($value.StartsWith('"') -and $value.EndsWith('"')) -or
            ($value.StartsWith("'") -and $value.EndsWith("'")))) {
            $value = $value.Substring(1, $value.Length - 2)
        }
        Set-Item -Path "Env:$name" -Value $value
    }
}

if (-not $env:RAG_REDIS_URL) { $env:RAG_REDIS_URL = "redis://127.0.0.1:6379/0" }
if (-not $env:RAG_ROCKETMQ_ENDPOINT) { $env:RAG_ROCKETMQ_ENDPOINT = "127.0.0.1:8081" }
if (-not $env:RAG_ROCKETMQ_TOPIC) { $env:RAG_ROCKETMQ_TOPIC = "praxis_state_cache_refresh" }
if (-not $env:RAG_ROCKETMQ_GROUP) { $env:RAG_ROCKETMQ_GROUP = "praxis_state_cache_refresh_workers" }

$python = Join-Path $root ".venv\Scripts\python.exe"
if (-not (Test-Path $python)) {
    $python = "python"
}
$env:PYTHONPATH = (Join-Path $root "src") + [IO.Path]::PathSeparator + $env:PYTHONPATH

& $python -c "import redis; import rocketmq"
if ($LASTEXITCODE -ne 0) { throw "Cache dependencies missing. Install: pip install -e '.[cache]'" }

Write-Host "Starting MySQL, Qdrant, Redis and RocketMQ..."
Invoke-Compose @("up", "-d")
if ($LASTEXITCODE -ne 0) {
    throw "docker compose up failed. Is Docker Desktop running and this shell allowed to access it?"
}

Write-Host "Waiting for MySQL to become healthy..."
$healthy = $false
for ($attempt = 0; $attempt -lt 90; $attempt++) {
    $status = docker inspect --format "{{.State.Health.Status}}" praxis-rag-mysql 2>$null
    if ($status -eq "healthy") {
        $healthy = $true
        break
    }
    Start-Sleep -Seconds 2
}
if (-not $healthy) {
    throw "MySQL did not become healthy. Run: docker compose logs mysql"
}

Write-Host "Waiting for RocketMQ and provisioning cache retry resources..."
$mqReady = $false
for ($attempt = 0; $attempt -lt 45; $attempt++) {
    & docker exec praxis-rag-rocketmq sh mqadmin updatetopic -n rocketmq-namesrv:9876 -c DefaultCluster -t $env:RAG_ROCKETMQ_TOPIC *> $null
    if ($LASTEXITCODE -eq 0) {
        & docker exec praxis-rag-rocketmq sh mqadmin updateSubGroup -n rocketmq-namesrv:9876 -c DefaultCluster -g $env:RAG_ROCKETMQ_GROUP -r 16 *> $null
        if ($LASTEXITCODE -eq 0) { $mqReady = $true; break }
    }
    Start-Sleep -Seconds 2
}
if (-not $mqReady) { throw "RocketMQ provisioning failed. Check docker-compose logs rocketmq." }

if (-not $SkipMigrate) {
    if (Test-Path $LegacyRoot) {
        Write-Host "Migrating legacy state from $LegacyRoot (repeatable)..."
        & $python -m rag_mcp.state.migrate --legacy-root $LegacyRoot --apply
        if ($LASTEXITCODE -ne 0) {
            Write-Warning "Migration reported warnings; review them before serving traffic."
        }
    }
    else {
        Write-Host "Legacy data root not found ($LegacyRoot); skipping migration."
    }
}

if ($MigrateOnly) {
    Write-Host "Dependencies started and migration finished."
    exit 0
}

Write-Host "Starting rag-service on http://$($env:RAG_SERVICE_HOST):$($env:RAG_SERVICE_PORT)/console"
& $python -m rag_mcp.http_service
