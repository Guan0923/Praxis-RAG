# Start dependencies, provision normal queues and readable DLQs, migrate, then run rag-service.
# Normal DLQ receive/ACK verified with SDK 5.1.2 / Apache Broker 5.3.3; revalidate other deployments.
# Run rag-document-worker and rag-search-worker separately with the same database,
# Redis, Qdrant, encryption and RocketMQ settings. Without a search consumer,
# MCP searches need a consumer before their task deadline.
#
# RAG_OUTBOX_BRIDGE selects how a committed document job reaches RocketMQ:
#   relay    (default) in-process outbox publisher inside rag-service
#   debezium           the optional Debezium Server profile tails the MySQL binlog
# Both deliver the same identifier-only notification; never enable both.
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

# Docker Desktop 把 CLI 放在自己的目录里，很多终端会话不会继承这一项 PATH，
# 于是出现 "术语 'docker' 不会被识别"。这里显式解析路径再继续。
function Resolve-DockerCli {
    if (Get-Command docker -ErrorAction SilentlyContinue) { return }
    $roots = @($env:ProgramFiles, ${env:ProgramFiles(x86)}, $env:LOCALAPPDATA) | Where-Object { $_ }
    foreach ($base in $roots) {
        $bin = Join-Path $base "Docker\Docker\resources\bin"
        if (Test-Path (Join-Path $bin "docker.exe")) {
            $env:PATH = $bin + [IO.Path]::PathSeparator + $env:PATH
            Write-Host "   已把 Docker CLI 目录加入 PATH: $bin" -ForegroundColor DarkGray
            return
        }
    }
    throw "找不到 docker 命令。请启动 Docker Desktop，或把 '%ProgramFiles%\Docker\Docker\resources\bin' 加入 PATH 后重开终端。"
}
Resolve-DockerCli

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
if (-not $env:RAG_OUTBOX_BRIDGE) { $env:RAG_OUTBOX_BRIDGE = "relay" }
if ($env:RAG_OUTBOX_BRIDGE -notin @("relay", "debezium")) {
    throw "RAG_OUTBOX_BRIDGE must be relay or debezium."
}
if (-not $env:RAG_MYSQL_DATABASE) { $env:RAG_MYSQL_DATABASE = "praxis_rag" }
# Debezium derives the RocketMQ topic from <topic.prefix>.<database>.<table>.
if ($env:RAG_OUTBOX_BRIDGE -eq "debezium") {
    if (-not $env:DEBEZIUM_TOPIC_PREFIX) { $env:DEBEZIUM_TOPIC_PREFIX = "praxis" }
    if (-not $env:RAG_DOCUMENT_TOPIC) {
        $env:RAG_DOCUMENT_TOPIC = "$($env:DEBEZIUM_TOPIC_PREFIX).$($env:RAG_MYSQL_DATABASE).document_job_outbox"
    }
}
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

# These overrides configure Broker resources only; the worker binaries read them at startup.
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

$binDir = Join-Path $root "dist\bin"
$ragService = Join-Path $binDir "rag-service.exe"
if (-not (Test-Path $ragService)) {
    Write-Host "Building the Go backend (dist/bin) before starting..."
    & (Join-Path $PSScriptRoot "build-go.ps1") -SkipFrontend
    if ($LASTEXITCODE -ne 0) { throw "Go build failed; run scripts/build-go.ps1 and retry." }
}
$stateMigrate = Join-Path $binDir "rag-state-migrate.exe"
$serviceMigrate = Join-Path $binDir "rag-service-migrate.exe"

$composeProfiles = @()
if ($env:RAG_OUTBOX_BRIDGE -eq "debezium") {
    $composeProfiles = @("--profile", "debezium")
    if (-not $env:RAG_ROCKETMQ_BROKER_IP) { $env:RAG_ROCKETMQ_BROKER_IP = "rocketmq" }
    Write-Host "Debezium bridge enabled: tailing MySQL binlog for $($env:RAG_DOCUMENT_TOPIC)"
}

Write-Host "Starting MySQL, Qdrant, Redis and RocketMQ..."
Invoke-Compose (@($composeProfiles) + @("up", "-d"))
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
Write-Host "DLQ resources are provisioned; run rag-dead-letter-worker --kind document|search for them. Revalidate SDK/Broker versions other than tested 5.1.2/5.3.3."
Write-Host "Document uploads are accepted by a MySQL commit; MQ only carries the notification. Deploy API and workers together; drain schema-2 chunked messages before upgrading."

if (-not $SkipMigrate) {
    # Go state schema migration is idempotent; run it before the service touches the database.
    Write-Host "Ensuring the shared state schema is up to date..."
    & $stateMigrate --apply
    if ($LASTEXITCODE -ne 0) { throw "State schema migration failed; review the rag-state-migrate output." }
    if (Test-Path $LegacyRoot) {
        Write-Host "Importing legacy knowledge sources from $LegacyRoot (preview was applied with --apply)..."
        & $serviceMigrate --legacy-root $LegacyRoot --apply
        if ($LASTEXITCODE -ne 0) {
            Write-Warning "Legacy import reported failures; review them before serving traffic (vectors are not reindexed by this import)."
        }
    }
    else {
        Write-Host "Legacy data root not found ($LegacyRoot); skipping legacy knowledge import."
    }
}

if ($env:RAG_OUTBOX_BRIDGE -eq "debezium") {
    Write-Host "Waiting for Debezium to start streaming the document_job_outbox binlog..."
    $streaming = $false
    for ($attempt = 0; $attempt -lt 90; $attempt++) {
        $logs = docker logs praxis-rag-debezium --since 5m 2>&1
        if ($logs -match "Starting streaming") { $streaming = $true; break }
        Start-Sleep -Seconds 2
    }
    if ($streaming) {
        Write-Host "Debezium is streaming MySQL binlog -> topic $($env:RAG_DOCUMENT_TOPIC)."
        Write-Host "The Go service will NOT start its in-process relay (RAG_OUTBOX_BRIDGE=debezium)."
    }
    else {
        Write-Warning "Debezium did not report 'Starting streaming' yet; check docker logs praxis-rag-debezium before relying on async uploads."
    }
}
if () {
    Write-Host "Dependencies started and migration finished."
    exit 0
}
Write-Host "Starting rag-service on http://$($env:RAG_SERVICE_HOST):$($env:RAG_SERVICE_PORT)/console"
& $ragService


