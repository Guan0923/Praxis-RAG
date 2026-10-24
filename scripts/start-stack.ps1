# Start dependencies, provision normal queues and readable DLQs, migrate, then run rag-service.
# Normal DLQ receive/ACK verified with SDK 5.1.2 / Apache Broker 5.3.3; revalidate other deployments.
# Run rag-document-worker and rag-search-worker separately with the same database,
# Redis, Qdrant, encryption and RocketMQ settings. Without a search consumer,
# MCP searches remain queued until their task deadline; --dispatch-only is not a consumer.
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
if (-not $env:RAG_DOCUMENT_TOPIC) { $env:RAG_DOCUMENT_TOPIC = "praxis_document_ingest" }
if (-not $env:RAG_DOCUMENT_GROUP) { $env:RAG_DOCUMENT_GROUP = "praxis_document_ingest_workers" }
if (-not $env:RAG_SEARCH_TOPIC) { $env:RAG_SEARCH_TOPIC = "praxis_search" }
if (-not $env:RAG_SEARCH_GROUP) { $env:RAG_SEARCH_GROUP = "praxis_search_workers" }
if (-not $env:RAG_DOCUMENT_DLQ_GROUP) { $env:RAG_DOCUMENT_DLQ_GROUP = "praxis_document_dead_letters" }
if (-not $env:RAG_SEARCH_DLQ_GROUP) { $env:RAG_SEARCH_DLQ_GROUP = "praxis_search_dead_letters" }

function Get-BrokerMaxRetries {
    param([string]$Name)
    $value = [Environment]::GetEnvironmentVariable($Name)
    if ([string]::IsNullOrWhiteSpace($value)) { return 16 }
    $parsed = 0
    if (-not [int]::TryParse($value, [ref]$parsed) -or $parsed -lt 0) {
        throw "$Name must be a nonnegative integer for Broker provisioning."
    }
    return $parsed
}

# These overrides configure Broker resources only; Python workers do not parse them.
$documentMaxRetries = Get-BrokerMaxRetries "RAG_DOCUMENT_BROKER_MAX_RETRIES"
$searchMaxRetries = Get-BrokerMaxRetries "RAG_SEARCH_BROKER_MAX_RETRIES"
$normalGroups = @($env:RAG_ROCKETMQ_GROUP, $env:RAG_DOCUMENT_GROUP, $env:RAG_SEARCH_GROUP)
$dlqGroups = @($env:RAG_DOCUMENT_DLQ_GROUP, $env:RAG_SEARCH_DLQ_GROUP)
if ($dlqGroups[0] -eq $dlqGroups[1] -or @($dlqGroups | Where-Object { $_ -in $normalGroups }).Count) {
    throw "DLQ handler groups must be distinct from each other and all normal consumer groups."
}
# Use the actual source group name, including any explicit physical namespace prefix.
# Resource namespace metadata alone is not verified to map to namespaced Broker resources.
$dlqTopics = @("%DLQ%$($env:RAG_DOCUMENT_GROUP)", "%DLQ%$($env:RAG_SEARCH_GROUP)")
$consumerGroups = @(
    @{ Name = $env:RAG_ROCKETMQ_GROUP; MaxRetries = 16 },
    @{ Name = $env:RAG_DOCUMENT_GROUP; MaxRetries = $documentMaxRetries },
    @{ Name = $env:RAG_SEARCH_GROUP; MaxRetries = $searchMaxRetries },
    @{ Name = $env:RAG_DOCUMENT_DLQ_GROUP; MaxRetries = 16 },
    @{ Name = $env:RAG_SEARCH_DLQ_GROUP; MaxRetries = 16 }
)

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

Write-Host "Waiting for RocketMQ and provisioning normal queues, readable DLQ topics and independent handler groups..."
$mqReady = $false
for ($attempt = 0; $attempt -lt 45; $attempt++) {
    $mqReady = $true
    foreach ($topic in @($env:RAG_ROCKETMQ_TOPIC, $env:RAG_DOCUMENT_TOPIC, $env:RAG_SEARCH_TOPIC)) {
        & docker exec praxis-rag-rocketmq sh mqadmin updatetopic -n rocketmq-namesrv:9876 -c DefaultCluster -t $topic *> $null
        if ($LASTEXITCODE -ne 0) { $mqReady = $false; break }
    }
    if ($mqReady) {
        foreach ($topic in $dlqTopics) {
            # Broker-created DLQs can be write-only; retain writes and explicitly enable reads.
            & docker exec praxis-rag-rocketmq sh mqadmin updatetopic -n rocketmq-namesrv:9876 -c DefaultCluster -t $topic -p 6 -r 1 -w 1 *> $null
            if ($LASTEXITCODE -ne 0) { $mqReady = $false; break }
        }
    }
    if ($mqReady) {
        foreach ($group in $consumerGroups) {
            & docker exec praxis-rag-rocketmq sh mqadmin updateSubGroup -n rocketmq-namesrv:9876 -c DefaultCluster -g $group.Name -r $group.MaxRetries *> $null
            if ($LASTEXITCODE -ne 0) { $mqReady = $false; break }
        }
    }
    if ($mqReady) { break }
    Start-Sleep -Seconds 2
}
if (-not $mqReady) { throw "RocketMQ provisioning failed. Check docker-compose logs rocketmq." }
Write-Host "Consumers are not started: run rag-search-worker for MCP search and rag-document-worker for MCP add_document separately."
Write-Host "DLQ resources are provisioned; planned rag-dead-letter-worker --kind document|search needs reader permissions. Revalidate SDK/Broker versions other than tested 5.1.2/5.3.3."
Write-Host "Without a search consumer, MCP searches remain queued until their task deadline; --dispatch-only is not a consumer."

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
