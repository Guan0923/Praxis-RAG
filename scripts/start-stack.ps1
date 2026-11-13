# Start dependencies (MySQL, Qdrant, Redis, Kafka), optionally provision topics,
# migrate, then run rag-service.
# Also starts the Docling PDF parser microservice (pdf-parser, :8091) because the
# default PDF engine in .env.example is RAG_PDF_ENGINE=docling. The first run
# builds a large Python image (CPU torch + docling); use RAG_PDF_ENGINE=go, or
# `docker compose up -d mysql qdrant redis kafka`, to skip it.
# Kafka replaces the former RocketMQ stack; topics/consumer groups are created
# explicitly below (auto-creation is also enabled on the broker).
# rag-service is started below, and rag-document-worker runs as its children
# with the same database, Redis, Qdrant, encryption and Kafka settings (RAG_DOCUMENT_WORKERS, default 1).
# Run rag-search-worker separately: without a search consumer, MCP searches need a
# consumer before their task deadline.
#
# RAG_OUTBOX_BRIDGE selects how a committed document job reaches Kafka:
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
    throw "Created .env from .env.example. Fill in RAG_MYSQL_PASSWORD, MYSQL_ROOT_PASSWORD, RAG_STATE_ENCRYPTION_KEY and RAG_MYSQL_DEBEZIUM_PASSWORD, then rerun."
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
# Host processes connect through the published listener; containers use kafka:9092.
if (-not $env:RAG_KAFKA_BOOTSTRAP_SERVERS) { $env:RAG_KAFKA_BOOTSTRAP_SERVERS = "127.0.0.1:9092" }
if ($env:RAG_KAFKA_BOOTSTRAP_SERVERS -match '^kafka(:|$)') {
    Write-Warning "RAG_KAFKA_BOOTSTRAP_SERVERS=$($env:RAG_KAFKA_BOOTSTRAP_SERVERS) is container-internal; host processes must use 127.0.0.1:9092."
}
if (-not $env:RAG_KAFKA_CLIENT_ID) { $env:RAG_KAFKA_CLIENT_ID = "praxis-rag" }
if (-not $env:RAG_OUTBOX_BRIDGE) { $env:RAG_OUTBOX_BRIDGE = "relay" }
if ($env:RAG_OUTBOX_BRIDGE -notin @("relay", "debezium")) {
    throw "RAG_OUTBOX_BRIDGE must be relay or debezium."
}
if (-not $env:RAG_MYSQL_DATABASE) { $env:RAG_MYSQL_DATABASE = "praxis_rag" }
if (-not $env:RAG_CACHE_TOPIC) { $env:RAG_CACHE_TOPIC = "praxis_state_cache_refresh" }
if (-not $env:RAG_CACHE_GROUP) { $env:RAG_CACHE_GROUP = "praxis_state_cache_refresh_workers" }
if (-not $env:RAG_DOCUMENT_TOPIC) { $env:RAG_DOCUMENT_TOPIC = "praxis_document_ingest" }
if (-not $env:RAG_DOCUMENT_GROUP) { $env:RAG_DOCUMENT_GROUP = "praxis_document_ingest_workers" }
if (-not $env:RAG_SEARCH_TOPIC) { $env:RAG_SEARCH_TOPIC = "praxis_search" }
if (-not $env:RAG_SEARCH_GROUP) { $env:RAG_SEARCH_GROUP = "praxis_search_workers" }
if (-not $env:RAG_DOCUMENT_DLQ_TOPIC) { $env:RAG_DOCUMENT_DLQ_TOPIC = "praxis_document_ingest_dead_letters" }
if (-not $env:RAG_SEARCH_DLQ_TOPIC) { $env:RAG_SEARCH_DLQ_TOPIC = "praxis_search_dead_letters" }
if (-not $env:RAG_DOCUMENT_DLQ_GROUP) { $env:RAG_DOCUMENT_DLQ_GROUP = "praxis_document_dead_letters" }
if (-not $env:RAG_SEARCH_DLQ_GROUP) { $env:RAG_SEARCH_DLQ_GROUP = "praxis_search_dead_letters" }

$normalTopics = @($env:RAG_CACHE_TOPIC, $env:RAG_DOCUMENT_TOPIC, $env:RAG_SEARCH_TOPIC)
$dlqTopics = @($env:RAG_DOCUMENT_DLQ_TOPIC, $env:RAG_SEARCH_DLQ_TOPIC)
$normalGroups = @($env:RAG_CACHE_GROUP, $env:RAG_DOCUMENT_GROUP, $env:RAG_SEARCH_GROUP)
$dlqGroups = @($env:RAG_DOCUMENT_DLQ_GROUP, $env:RAG_SEARCH_DLQ_GROUP)
if (@($normalTopics | Sort-Object -Unique).Count -ne $normalTopics.Count) {
    throw "RAG_CACHE_TOPIC, RAG_DOCUMENT_TOPIC and RAG_SEARCH_TOPIC must be distinct."
}
if (@($dlqTopics | Sort-Object -Unique).Count -ne $dlqTopics.Count) {
    throw "RAG_DOCUMENT_DLQ_TOPIC and RAG_SEARCH_DLQ_TOPIC must be distinct."
}
if ($dlqGroups[0] -eq $dlqGroups[1] -or @($dlqGroups | Where-Object { $_ -in $normalGroups }).Count) {
    throw "DLQ handler groups must be distinct from each other and all normal consumer groups."
}
if (@($dlqTopics | Where-Object { $_ -in $normalTopics }).Count) {
    throw "DLQ topics must be separate from the normal cache/document/search topics."
}

$binDir = Join-Path $root "dist\bin"
$ragService = Join-Path $binDir "rag-service.exe"
if (-not (Test-Path $ragService)) {
    Write-Host "Building the Go backend (dist/bin) before starting..."
    & (Join-Path $PSScriptRoot "build-go.ps1") -SkipFrontend
    if ($LASTEXITCODE -ne 0) { throw "Go build failed; run scripts/build-go.ps1 and retry." }
}
$stateMigrate = Join-Path $binDir "rag-state-migrate.exe"
$serviceMigrate = Join-Path $binDir "rag-service-migrate.exe"

# Legacy RocketMQ containers from the pre-Kafka stack block the new profile
# (Debezium previously shared the broker network namespace). Remove the
# containers only; data volumes are preserved.
foreach ($legacy in @("praxis-rag-rocketmq", "praxis-rag-rocketmq-namesrv", "praxis-rag-rocketmq-storage-init-1")) {
    $existing = & docker ps -a --filter "name=^/${legacy}$" --format "{{.Names}}"
    if ($existing) {
        Write-Host "Removing legacy RocketMQ container $legacy (data volumes are preserved)..."
        & docker rm -f $legacy *> $null
    }
}

$composeProfiles = @()
if ($env:RAG_OUTBOX_BRIDGE -eq "debezium") {
    $composeProfiles = @("--profile", "debezium")
    if (-not $env:RAG_MYSQL_DEBEZIUM_PASSWORD) {
        throw "RAG_MYSQL_DEBEZIUM_PASSWORD must be set in .env for the Debezium profile."
    }
    Write-Host "Debezium bridge enabled: tailing MySQL binlog for $($env:RAG_DOCUMENT_TOPIC)"
}

Write-Host "Starting MySQL, Qdrant, Redis, Kafka and the Docling PDF parser (pdf-parser)..."
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

Write-Host "Waiting for Kafka to become healthy..."
$kafkaReady = $false
for ($attempt = 0; $attempt -lt 60; $attempt++) {
    $health = docker inspect --format "{{.State.Health.Status}}" praxis-rag-kafka 2>$null
    if ($health -eq "healthy") {
        & docker exec praxis-rag-kafka /opt/kafka/bin/kafka-broker-api-versions.sh --bootstrap-server kafka:9092 *> $null
        if ($LASTEXITCODE -eq 0) { $kafkaReady = $true; break }
    }
    Start-Sleep -Seconds 2
}
if (-not $kafkaReady) {
    throw "Kafka did not become healthy. Run: docker compose logs kafka"
}

# The default PDF engine (docling) needs the pdf-parser container. Wait softly:
# a cold model start is slow, and RAG_PDF_ENGINE=go deployments do not need it.
$pdfEngine = $env:RAG_PDF_ENGINE
if (-not $pdfEngine) {
    if ($env:RAG_PDF_SERVICE_URL) { $pdfEngine = "docling" } else { $pdfEngine = "go" }
}
if ($pdfEngine -eq "docling") {
    Write-Host "Waiting for pdf-parser to become healthy (first build/start can take minutes)..."
    $pdfReady = $false
    for ($attempt = 0; $attempt -lt 150; $attempt++) {
        $pdfHealth = docker inspect --format "{{.State.Health.Status}}" praxis-rag-pdf-parser 2>$null
        if ($pdfHealth -eq "healthy") {
            $pdfReady = $true
            break
        }
        Start-Sleep -Seconds 2
    }
    if ($pdfReady) {
        Write-Host "pdf-parser is healthy."
    }
    else {
        Write-Warning "pdf-parser is not healthy yet; PDF uploads with RAG_PDF_ENGINE=docling fail until it is. Check: docker compose logs pdf-parser"
    }
}

Write-Host "Ensuring Kafka topics exist (partitions=4, replication-factor=1): $($normalTopics -join ', ')"
$topicsReady = $false
for ($attempt = 0; $attempt -lt 30; $attempt++) {
    $topicsReady = $true
    foreach ($topic in ($normalTopics + $dlqTopics)) {
        & docker exec praxis-rag-kafka /opt/kafka/bin/kafka-topics.sh `
            --bootstrap-server kafka:9092 --create --if-not-exists `
            --topic $topic --partitions 4 --replication-factor 1 *> $null
        if ($LASTEXITCODE -ne 0) { $topicsReady = $false; break }
    }
    if ($topicsReady) { break }
    Start-Sleep -Seconds 2
}
if (-not $topicsReady) { throw "Kafka topic creation failed. Check docker compose logs kafka." }
Write-Host "Kafka topics ready: $((& docker exec praxis-rag-kafka /opt/kafka/bin/kafka-topics.sh --bootstrap-server kafka:9092 --list) -join ', ')"
Write-Host "Consumers: rag-document-worker is started automatically with rag-service below; run rag-search-worker for MCP search separately."
Write-Host "REST/Console batch uploads return after durable admission; rag-document-worker parses them afterwards."
Write-Host "Workers retry in-process and publish exhausted messages to RAG_*_DLQ_TOPIC; run rag-dead-letter-worker --kind document|search for them."
Write-Host "Document uploads are accepted by a MySQL commit; Kafka only carries the identifier notification. Deploy API and workers together; drain schema-2 chunked messages before upgrading."

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
        Write-Host "Debezium is streaming MySQL binlog -> Kafka topic $($env:RAG_DOCUMENT_TOPIC)."
        Write-Host "The Go service will NOT start its in-process relay (RAG_OUTBOX_BRIDGE=debezium)."
    }
    else {
        Write-Warning "Debezium did not report 'Starting streaming' yet; check docker logs praxis-rag-debezium before relying on async uploads."
    }
}

if ($MigrateOnly) {
    Write-Host "Dependencies started and migration finished."
    exit 0
}
$docWorker = Join-Path $binDir "rag-document-worker.exe"
if (-not (Test-Path $docWorker)) {
    throw "Document worker executable not found: $docWorker. Run scripts\build-go.ps1 without -SkipFrontend to build dist\bin, then retry."
}
# RAG_DOCUMENT_WORKERS fans out document workers: Kafka shares the
# RAG_DOCUMENT_TOPIC partitions between consumers of RAG_DOCUMENT_GROUP and each
# process parses one document at a time, so N workers = N concurrent parses. The
# effective ceiling is the topic partition count (topics are provisioned with 4
# below); extra processes stay idle. Keep the default 1 for a single-worker setup.
$documentWorkers = 1
if ($env:RAG_DOCUMENT_WORKERS) {
    $parsedWorkers = 0
    if (-not [int]::TryParse($env:RAG_DOCUMENT_WORKERS, [ref]$parsedWorkers) -or $parsedWorkers -lt 1 -or $parsedWorkers -gt 32) {
        throw "RAG_DOCUMENT_WORKERS must be an integer between 1 and 32."
    }
    $documentWorkers = $parsedWorkers
}
$docWorkerProcesses = @()
Write-Host "Starting $documentWorkers rag-document-worker process(es) (inherits the loaded .env settings)..."
for ($workerIndex = 1; $workerIndex -le $documentWorkers; $workerIndex++) {
    $docWorkerProcesses += Start-Process -FilePath $docWorker -NoNewWindow -PassThru
}

Write-Host "Starting rag-service on http://$($env:RAG_SERVICE_HOST):$($env:RAG_SERVICE_PORT)/console"
try {
    & $ragService
}
finally {
    foreach ($docWorkerProcess in $docWorkerProcesses) {
        Write-Host "Stopping rag-document-worker (pid $($docWorkerProcess.Id))..."
        Stop-Process -Id $docWorkerProcess.Id -Force -ErrorAction SilentlyContinue
    }
}
