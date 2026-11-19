# Start dependencies (MySQL, Qdrant, Redis, Kafka), optionally provision topics,
# migrate, then run rag-service.
# Also starts the PDF vision parser microservice (pdf-parser, :8091) because the
# default PDF engine in .env.example is RAG_PDF_ENGINE=vision. It runs a small
# Python image (PyMuPDF + httpx) with no local models; each request uses the
# knowledge base's remote VLM. Use RAG_PDF_ENGINE=go, or
# `docker compose up -d mysql qdrant redis kafka`, to skip it.
# Kafka replaces the former RocketMQ stack; topics/consumer groups are created
# explicitly below (auto-creation is also enabled on the broker).
# rag-service is started below; rag-document-worker (RAG_DOCUMENT_WORKERS, default 1)
# and rag-cache-worker run as sibling processes with the same database, Redis,
# encryption and Kafka settings.
# MCP/REST search is synchronous (one call returns evidence): there is no search
# worker, search topic/group or queued task to consume or drain.
#
# Two Debezium MySQL connectors run as tasks inside a distributed Kafka Connect
# cluster (two workers on this machine; production spreads workers across
# machines). Connector configs, source offsets and tasks status live in Kafka
# internal topics, so a dead worker is replaced by its peer and resumes from the
# saved binlog position.
#   * praxis-document-cdc (RAG_CONNECT_DOCUMENT_CONNECTOR) document_jobs -> RAG_DOCUMENT_TOPIC
#   * praxis-cache-cdc    (RAG_CONNECT_CACHE_CONNECTOR)    business tables -> RAG_CACHE_TOPIC
# The application never publishes task/cache notifications itself. Connectors
# are registered by fixed name below; an unchanged configuration is not updated,
# so repeating the start never bounces a healthy capture task.
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
# The document lock is independent of the display cache but defaults to the same
# local Redis. Redis is the required lease backend: a missing lock address is a
# startup configuration error for every document-writing process.
if (-not $env:RAG_REDIS_LOCK_URL) { $env:RAG_REDIS_LOCK_URL = "redis://127.0.0.1:6379/0" }
# Host processes connect through the published listener; containers use kafka:9092.
if (-not $env:RAG_KAFKA_BOOTSTRAP_SERVERS) { $env:RAG_KAFKA_BOOTSTRAP_SERVERS = "127.0.0.1:9092" }
if ($env:RAG_KAFKA_BOOTSTRAP_SERVERS -match '^kafka(:|$)') {
    Write-Warning "RAG_KAFKA_BOOTSTRAP_SERVERS=$($env:RAG_KAFKA_BOOTSTRAP_SERVERS) is container-internal; host processes must use 127.0.0.1:9092."
}
if (-not $env:RAG_KAFKA_CLIENT_ID) { $env:RAG_KAFKA_CLIENT_ID = "praxis-rag" }
if (-not $env:RAG_MYSQL_DATABASE) { $env:RAG_MYSQL_DATABASE = "praxis_rag" }
if (-not $env:RAG_CACHE_TOPIC) { $env:RAG_CACHE_TOPIC = "praxis_state_cache_refresh" }
if (-not $env:RAG_CACHE_GROUP) { $env:RAG_CACHE_GROUP = "praxis_state_cache_refresh_workers" }
if (-not $env:RAG_DOCUMENT_TOPIC) { $env:RAG_DOCUMENT_TOPIC = "praxis_document_ingest" }
if (-not $env:RAG_DOCUMENT_GROUP) { $env:RAG_DOCUMENT_GROUP = "praxis_document_ingest_workers" }
if (-not $env:RAG_REEMBED_TOPIC) { $env:RAG_REEMBED_TOPIC = "praxis_reembed" }
if (-not $env:RAG_REEMBED_GROUP) { $env:RAG_REEMBED_GROUP = "praxis_reembed_workers" }
if (-not $env:RAG_DOCUMENT_DLQ_TOPIC) { $env:RAG_DOCUMENT_DLQ_TOPIC = "praxis_document_ingest_dead_letters" }
if (-not $env:RAG_CACHE_DLQ_TOPIC) { $env:RAG_CACHE_DLQ_TOPIC = "praxis_state_cache_refresh_dead_letters" }
if (-not $env:RAG_DOCUMENT_DLQ_GROUP) { $env:RAG_DOCUMENT_DLQ_GROUP = "praxis_document_dead_letters" }
if (-not $env:RAG_CACHE_DLQ_GROUP) { $env:RAG_CACHE_DLQ_GROUP = "praxis_state_cache_refresh_dead_letters" }
# Distributed Kafka Connect REST endpoints (the two workers' loopback ports).
# Both are listed so the monitor and the restart supervisor fail over when one
# worker is down. RAG_CONNECT_URLS may be set explicitly to the same list.
if (-not $env:RAG_CONNECT_PORT_1) { $env:RAG_CONNECT_PORT_1 = "8083" }
if (-not $env:RAG_CONNECT_PORT_2) { $env:RAG_CONNECT_PORT_2 = "8084" }
if (-not $env:RAG_CONNECT_URLS) {
    $env:RAG_CONNECT_URLS = "http://127.0.0.1:$($env:RAG_CONNECT_PORT_1),http://127.0.0.1:$($env:RAG_CONNECT_PORT_2)"
}
if (-not $env:RAG_CONNECT_DOCUMENT_CONNECTOR) { $env:RAG_CONNECT_DOCUMENT_CONNECTOR = "praxis-document-cdc" }
if (-not $env:RAG_CONNECT_CACHE_CONNECTOR) { $env:RAG_CONNECT_CACHE_CONNECTOR = "praxis-cache-cdc" }
if (-not $env:DEBEZIUM_CONNECT_VERSION) { $env:DEBEZIUM_CONNECT_VERSION = "3.7.1.Final" }
if (-not $env:RAG_MYSQL_DEBEZIUM_CACHE_USER) { $env:RAG_MYSQL_DEBEZIUM_CACHE_USER = "debezium_cache" }
if (-not $env:RAG_MYSQL_DEBEZIUM_CACHE_PASSWORD) {
    # One password for two least-privilege accounts is acceptable in development;
    # production should set a separate value per connector.
    $env:RAG_MYSQL_DEBEZIUM_CACHE_PASSWORD = $env:RAG_MYSQL_DEBEZIUM_PASSWORD
}

$normalTopics = @($env:RAG_CACHE_TOPIC, $env:RAG_DOCUMENT_TOPIC, $env:RAG_REEMBED_TOPIC)
$dlqTopics = @($env:RAG_DOCUMENT_DLQ_TOPIC, $env:RAG_CACHE_DLQ_TOPIC)
$normalGroups = @($env:RAG_CACHE_GROUP, $env:RAG_DOCUMENT_GROUP, $env:RAG_REEMBED_GROUP)
$dlqGroups = @($env:RAG_DOCUMENT_DLQ_GROUP, $env:RAG_CACHE_DLQ_GROUP)
if (@($normalTopics | Sort-Object -Unique).Count -ne $normalTopics.Count) {
    throw "Cache, document and re-embedding topics must be distinct."
}
if (@($dlqTopics | Sort-Object -Unique).Count -ne $dlqTopics.Count) {
    throw "RAG_CACHE_DLQ_TOPIC and RAG_DOCUMENT_DLQ_TOPIC must be distinct."
}
if (@($normalGroups | Sort-Object -Unique).Count -ne $normalGroups.Count) {
    throw "Cache, document and re-embedding consumer groups must be distinct."
}
if (@($dlqGroups | Sort-Object -Unique).Count -ne $dlqGroups.Count) {
    throw "DLQ handler groups must be distinct from each other."
}
if (@($dlqGroups | Where-Object { $_ -in $normalGroups }).Count) {
    throw "DLQ handler groups must be distinct from all normal consumer groups."
}
if (@($dlqTopics | Where-Object { $_ -in $normalTopics }).Count) {
    throw "DLQ topics must be separate from the normal cache/document topics."
}

$binDir = Join-Path $root "dist\bin"
$ragService = Join-Path $binDir "rag-service.exe"
$connectExe = Join-Path $binDir "rag-connect.exe"
$reembedWorker = Join-Path $binDir "rag-reembed-worker.exe"
if (-not (Test-Path $ragService) -or -not (Test-Path $connectExe) -or -not (Test-Path $reembedWorker)) {
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

if (-not $env:RAG_MYSQL_DEBEZIUM_PASSWORD) {
    throw "RAG_MYSQL_DEBEZIUM_PASSWORD must be set in .env for the Debezium document connector."
}
if (-not $env:RAG_MYSQL_DEBEZIUM_CACHE_PASSWORD) {
    throw "RAG_MYSQL_DEBEZIUM_CACHE_PASSWORD (or RAG_MYSQL_DEBEZIUM_PASSWORD as fallback) must be set in .env for the Debezium cache connector."
}
if ($env:RAG_MYSQL_DEBEZIUM_CACHE_USER -eq $env:RAG_MYSQL_DEBEZIUM_USER) {
    throw "RAG_MYSQL_DEBEZIUM_CACHE_USER must differ from RAG_MYSQL_DEBEZIUM_USER so one capture path cannot lock out the other."
}

Write-Host "Starting MySQL, Qdrant, Redis, Kafka, the Kafka Connect cluster and the PDF vision parser (pdf-parser)..."
Invoke-Compose -Arguments @("up", "-d")
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

# Existing data volumes skip docker-entrypoint-initdb.d, so converge both CDC
# accounts here. This is idempotent and also re-aligns passwords after an
# operator rotates them in .env. Passwords never go on the docker CLI argv: the
# SQL goes over stdin and the root password through MYSQL_PWD.
function ConvertTo-SqlLiteral {
    param([string]$Value)
    return $Value.Replace("'", "''")
}
$documentUser = ConvertTo-SqlLiteral $env:RAG_MYSQL_DEBEZIUM_USER
$documentPassword = ConvertTo-SqlLiteral $env:RAG_MYSQL_DEBEZIUM_PASSWORD
$cacheUser = ConvertTo-SqlLiteral $env:RAG_MYSQL_DEBEZIUM_CACHE_USER
$cachePassword = ConvertTo-SqlLiteral $env:RAG_MYSQL_DEBEZIUM_CACHE_PASSWORD
$cdcSql = @'
CREATE USER IF NOT EXISTS '{0}'@'%' IDENTIFIED BY '{1}';
ALTER USER '{0}'@'%' IDENTIFIED BY '{1}';
CREATE USER IF NOT EXISTS '{2}'@'%' IDENTIFIED BY '{3}';
ALTER USER '{2}'@'%' IDENTIFIED BY '{3}';
GRANT SELECT, RELOAD, SHOW DATABASES, REPLICATION SLAVE, REPLICATION CLIENT ON *.* TO '{0}'@'%';
GRANT SELECT, RELOAD, SHOW DATABASES, REPLICATION SLAVE, REPLICATION CLIENT ON *.* TO '{2}'@'%';
FLUSH PRIVILEGES;
'@ -f $documentUser, $documentPassword, $cacheUser, $cachePassword
Write-Host "Ensuring the two CDC accounts ($($env:RAG_MYSQL_DEBEZIUM_USER), $($env:RAG_MYSQL_DEBEZIUM_CACHE_USER)) have binlog privileges..."
$savedMysqlPwd = $env:MYSQL_PWD
$env:MYSQL_PWD = $env:MYSQL_ROOT_PASSWORD
try {
    # `docker exec -e MYSQL_PWD` (no value) copies the variable from this
    # process, so the root password never appears on the docker CLI argv.
    $cdcSql | docker exec -i -e MYSQL_PWD praxis-rag-mysql mysql -uroot
}
finally {
    $env:MYSQL_PWD = $savedMysqlPwd
}
if ($LASTEXITCODE -ne 0) {
    throw "Could not create the CDC users. Check MYSQL_ROOT_PASSWORD and: docker compose logs mysql"
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

# The default PDF engine (vision) needs the pdf-parser container. Wait softly:
# RAG_PDF_ENGINE=go deployments do not need it, and the service itself stays up
# even when the remote VLM is unreachable (parse requests then fail).
$pdfEngine = $env:RAG_PDF_ENGINE
if (-not $pdfEngine) {
    if ($env:RAG_PDF_SERVICE_URL) { $pdfEngine = "vision" } else { $pdfEngine = "go" }
}
if ($pdfEngine -eq "vision" -or $pdfEngine -eq "docling") {
    Write-Host "Waiting for pdf-parser to become healthy (first build/start can take a minute)..."
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
        Write-Warning "pdf-parser is not healthy yet; PDF uploads with RAG_PDF_ENGINE=vision fail until it is. Check: docker compose logs pdf-parser"
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
Write-Host "Consumers: rag-document-worker, rag-cache-worker and rag-reembed-worker are started automatically with rag-service below; MCP/REST search is synchronous and needs no worker."
Write-Host "REST/Console batch uploads return after durable admission; rag-document-worker processes them afterwards."
Write-Host "Document workers retry in-process and publish exhausted messages to RAG_DOCUMENT_DLQ_TOPIC; run rag-dead-letter-worker --kind document|cache for them."
Write-Host "Task/cache notifications come from the Kafka Connect Debezium connectors instead of an outbox: the document connector carries only document_jobs schema-4 locator ids plus schedule_version; the cache connector publishes locator-only c/u/d events to RAG_CACHE_TOPIC. Deploy API and workers together."

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

# Wait until both Connect workers answer their REST API, then register both
# connectors by fixed name and wait for their connector AND task states to be
# RUNNING. This replaces the old /q/health probe: it asserts the actual capture
# task state that delivers notifications, not just a live process.
function Wait-ConnectWorker {
    param([string]$Url, [string]$Label)
    for ($attempt = 0; $attempt -lt 120; $attempt++) {
        try {
            $response = Invoke-WebRequest -UseBasicParsing -TimeoutSec 3 -Uri "$Url/connector-plugins"
            if ($response.StatusCode -eq 200 -and $response.Content -match "io.debezium.connector.mysql.MySqlConnector") {
                Write-Host "$Label REST API is ready."
                return
            }
        }
        catch {
            $response = $null
        }
        Start-Sleep -Seconds 2
    }
    throw "$Label REST API did not become ready; check docker compose logs $Label."
}

$connectUrls = @($env:RAG_CONNECT_URLS -split ',' | ForEach-Object { $_.Trim() } | Where-Object { $_ })
if ($connectUrls.Count -eq 0) {
    throw "RAG_CONNECT_URLS must list at least one Kafka Connect REST endpoint."
}
Write-Host "Waiting for the Kafka Connect worker REST APIs: $($connectUrls -join ', ')"
for ($index = 0; $index -lt $connectUrls.Count; $index++) {
    Wait-ConnectWorker -Url $connectUrls[$index] -Label "connect-$($index + 1)"
}

# Registration expands the connector templates with the *container-internal*
# database/broker addresses: connectors run inside Docker even though this
# script runs on the host. Host values are restored afterwards.
function Invoke-ConnectRegistration {
    param([string]$Executable, [string]$Urls, [string]$ExpectedVersion)
    $overrides = [ordered]@{
        RAG_MYSQL_HOST                      = "mysql"
        RAG_MYSQL_PORT                      = "3306"
        RAG_CONNECT_KAFKA_BOOTSTRAP_SERVERS = "kafka:9092"
    }
    $saved = @{}
    foreach ($name in $overrides.Keys) {
        $saved[$name] = [Environment]::GetEnvironmentVariable($name, "Process")
        Set-Item -Path "Env:$name" -Value $overrides[$name]
    }
    try {
        # --wait 是 Go duration 标志：必须带单位（180s）。裸数字会让
        # rag-connect 以 `invalid value "180" for flag -wait` 退出 2。
        & $Executable register --urls $Urls --expected-version $ExpectedVersion --wait 180s
        if ($LASTEXITCODE -ne 0) {
            throw "Connector registration failed. Check docker compose logs connect-1 connect-2."
        }
        & $Executable wait --urls $Urls --wait 180s
        if ($LASTEXITCODE -ne 0) {
            throw "Kafka Connect connectors/tasks did not become RUNNING. Run: dist\bin\rag-connect.exe status"
        }
    }
    finally {
        foreach ($name in $overrides.Keys) {
            if ($null -eq $saved[$name]) { Remove-Item -Path "Env:$name" -ErrorAction SilentlyContinue }
            else { Set-Item -Path "Env:$name" -Value $saved[$name] }
        }
    }
}

Write-Host "Registering the two Debezium connectors (idempotent, by fixed name)..."
Invoke-ConnectRegistration -Executable $connectExe -Urls ($connectUrls -join ',') -ExpectedVersion $env:DEBEZIUM_CONNECT_VERSION
Write-Host "Kafka Connect is capturing MySQL binlog -> topics $($env:RAG_DOCUMENT_TOPIC), $($env:RAG_CACHE_TOPIC), $($env:RAG_REEMBED_TOPIC)."

if ($MigrateOnly) {
    Write-Host "Dependencies started and migration finished."
    exit 0
}
$docWorker = Join-Path $binDir "rag-document-worker.exe"
if (-not (Test-Path $docWorker)) {
    throw "Document worker executable not found: $docWorker. Run scripts\build-go.ps1 without -SkipFrontend to build dist\bin, then retry."
}
# RAG_DOCUMENT_WORKERS fans out document workers: Kafka shares the
# RAG_DOCUMENT_TOPIC partitions between consumers of RAG_DOCUMENT_GROUP. Each
# process runs RAG_DOCUMENT_WORKER_CONCURRENCY handler goroutines (default 1).
# The partition count limits active consumer processes, not handler goroutines;
# one process can handle multiple documents concurrently. PDF parsing is also
# bounded by pdf-parser's RAG_PDF_SERVICE_CONCURRENCY.
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
$cacheWorkerProcess = $null
$cacheWorker = Join-Path $binDir "rag-cache-worker.exe"
if (-not (Test-Path $cacheWorker)) {
    Write-Warning "rag-cache-worker executable not found ($cacheWorker); the Redis read model only converges through the periodic fallback refresh until it is started. Run scripts\build-go.ps1 to build it."
}
else {
    Write-Host "Starting rag-cache-worker (consumes $($env:RAG_CACHE_TOPIC) as group $($env:RAG_CACHE_GROUP))..."
    $cacheWorkerProcess = Start-Process -FilePath $cacheWorker -NoNewWindow -PassThru
}
$reembedWorkerProcess = $null
try {
    Write-Host "Starting rag-reembed-worker (consumes $($env:RAG_REEMBED_TOPIC) as group $($env:RAG_REEMBED_GROUP))..."
    $reembedWorkerProcess = Start-Process -FilePath $reembedWorker -WindowStyle Hidden -PassThru
    & $ragService
}
finally {
    if ($reembedWorkerProcess) {
        Stop-Process -Id $reembedWorkerProcess.Id -Force -ErrorAction SilentlyContinue
    }
    foreach ($docWorkerProcess in $docWorkerProcesses) {
        Write-Host "Stopping rag-document-worker (pid $($docWorkerProcess.Id))..."
        Stop-Process -Id $docWorkerProcess.Id -Force -ErrorAction SilentlyContinue
    }
    if ($cacheWorkerProcess) {
        Write-Host "Stopping rag-cache-worker (pid $($cacheWorkerProcess.Id))..."
        Stop-Process -Id $cacheWorkerProcess.Id -Force -ErrorAction SilentlyContinue
    }
}
