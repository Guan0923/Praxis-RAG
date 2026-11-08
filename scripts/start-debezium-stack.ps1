# 启动「MySQL + Qdrant + Redis + RocketMQ + Debezium」开发栈并自检。
#
# 用途：验证 Debezium CDC 桥接（MySQL binlog -> RocketMQ 通知）。
# 必须在普通 PowerShell 窗口运行：Docker Desktop 的 named pipe 在 agent 沙箱里不可见。
#
#   .\scripts\start-debezium-stack.ps1
#   .\scripts\start-debezium-stack.ps1 -FreshVolumes   # 清空数据卷重建（会删数据）
#
# 顺序刻意分两段：先起依赖、建表、建 CDC 账号、建 topic，最后才起 Debezium，
# 否则连接器会因为表或账号不存在而反复重启。

param(
    [switch]$FreshVolumes
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
    if (Get-Command docker-compose -ErrorAction SilentlyContinue) { & docker-compose @Arguments }
    else { & docker compose @Arguments }
    if ($LASTEXITCODE -ne 0) { throw "docker compose $($Arguments -join ' ') failed" }
}

function Invoke-Mysql {
    param([string]$Sql)
    $Sql | docker exec -i praxis-rag-mysql sh -c 'mysql -uroot -p"$MYSQL_ROOT_PASSWORD"'
}

if (-not (Test-Path ".env")) {
    throw "missing .env: Copy-Item .env.example .env and fill in passwords + RAG_STATE_ENCRYPTION_KEY"
}

Get-Content ".env" | ForEach-Object {
    $line = $_.Trim()
    if (-not $line -or $line.StartsWith("#")) { return }
    if ($line -match '^([A-Za-z_][A-Za-z0-9_]*)\s*=\s*(.*)$') {
        $name = $Matches[1]; $value = $Matches[2].Trim()
        if ($value.Length -ge 2 -and (
            ($value.StartsWith('"') -and $value.EndsWith('"')) -or
            ($value.StartsWith("'") -and $value.EndsWith("'")))) {
            $value = $value.Substring(1, $value.Length - 2)
        }
        Set-Item -Path "Env:$name" -Value $value
    }
}

$db = if ($env:RAG_MYSQL_DATABASE) { $env:RAG_MYSQL_DATABASE } else { "praxis_rag" }
if (-not $env:RAG_ROCKETMQ_BROKER_IP) { $env:RAG_ROCKETMQ_BROKER_IP = "rocketmq" }
if (-not $env:DEBEZIUM_TOPIC_PREFIX) { $env:DEBEZIUM_TOPIC_PREFIX = "praxis" }
if (-not $env:RAG_DOCUMENT_GROUP) { $env:RAG_DOCUMENT_GROUP = "praxis_document_ingest_workers" }
if (-not $env:RAG_ROCKETMQ_GROUP) { $env:RAG_ROCKETMQ_GROUP = "praxis_state_cache_refresh_workers" }
if (-not $env:RAG_SEARCH_GROUP) { $env:RAG_SEARCH_GROUP = "praxis_search_workers" }
if (-not $env:RAG_ROCKETMQ_TOPIC) { $env:RAG_ROCKETMQ_TOPIC = "praxis_state_cache_refresh" }
if (-not $env:RAG_SEARCH_TOPIC) { $env:RAG_SEARCH_TOPIC = "praxis_search" }

# RocketMQ topic names only allow ^[%a-zA-Z0-9_-]+$: the connector default
# destination <prefix>.<db>.<table> contains dots and is rejected, so the
# Debezium SMT routes every record to this single legal topic name.
if (-not $env:RAG_DOCUMENT_TOPIC) { $env:RAG_DOCUMENT_TOPIC = "praxis_document_ingest" }
if (-not $env:DEBEZIUM_TOPIC_PREFIX) { $env:DEBEZIUM_TOPIC_PREFIX = $env:RAG_DOCUMENT_TOPIC }
$documentTopic = $env:RAG_DOCUMENT_TOPIC
$env:RAG_DOCUMENT_TOPIC = $documentTopic
$env:RAG_OUTBOX_BRIDGE = "debezium"
$cdcUser = if ($env:RAG_MYSQL_DEBEZIUM_USER) { $env:RAG_MYSQL_DEBEZIUM_USER } else { "debezium" }
$cdcPassword = $env:RAG_MYSQL_DEBEZIUM_PASSWORD
if (-not $cdcPassword) { throw "RAG_MYSQL_DEBEZIUM_PASSWORD is not configured" }

if ($FreshVolumes) {
    Write-Host "== resetting volumes (deletes MySQL data) ==" -ForegroundColor Yellow
    Invoke-Compose(@("down", "-v"))
}

Write-Host "== step 1/5: starting MySQL / Qdrant / Redis / RocketMQ ==" -ForegroundColor Cyan
Invoke-Compose(@("up", "-d", "mysql", "qdrant", "redis", "rocketmq-namesrv", "rocketmq-storage-init", "rocketmq"))

Write-Host "== step 2/5: waiting for MySQL health ==" -ForegroundColor Cyan
$healthy = $false
for ($i = 0; $i -lt 90; $i++) {
    if ((docker inspect --format "{{.State.Health.Status}}" praxis-rag-mysql 2>$null) -eq "healthy") {
        $healthy = $true; break
    }
    Start-Sleep -Seconds 2
}
if (-not $healthy) { throw "MySQL not healthy; run: docker compose logs mysql" }

Write-Host "== step 3/5: creating CDC user and schema ==" -ForegroundColor Cyan
$grantSql = "CREATE USER IF NOT EXISTS '$cdcUser'@'%' IDENTIFIED BY '$cdcPassword'; " +
            "ALTER USER '$cdcUser'@'%' IDENTIFIED BY '$cdcPassword'; " +
            "GRANT SELECT, RELOAD, SHOW DATABASES, REPLICATION SLAVE, REPLICATION CLIENT ON *.* TO '$cdcUser'@'%'; " +
            "FLUSH PRIVILEGES;"
Invoke-Mysql $grantSql | Out-Null
if ($LASTEXITCODE -ne 0) { throw "CDC user creation failed (stale volume password? rerun with -FreshVolumes)" }
Write-Host "   CDC user ready: $cdcUser" -ForegroundColor Green

$python = Join-Path $root ".venv\Scripts\python.exe"
if (-not (Test-Path $python)) { throw "missing .venv: python -m venv .venv; .\.venv\Scripts\pip install -e ." }
$env:PYTHONPATH = (Join-Path $root "src")
& $python -c "from rag_mcp.state.store import StateStores; s = StateStores.create(); print('schema ready'); s.close()"
if ($LASTEXITCODE -ne 0) { throw "schema creation failed" }

Write-Host "== step 4/5: provisioning RocketMQ topics / groups ==" -ForegroundColor Cyan
$ready = $false
for ($i = 0; $i -lt 45; $i++) {
    $ready = $true
    foreach ($topic in @($env:RAG_ROCKETMQ_TOPIC, $documentTopic, $env:RAG_SEARCH_TOPIC)) {
        & docker exec praxis-rag-rocketmq sh mqadmin updatetopic -n rocketmq-namesrv:9876 -c DefaultCluster -t $topic *> $null
        if ($LASTEXITCODE -ne 0) { $ready = $false; break }
    }
    if ($ready) { break }
    Start-Sleep -Seconds 2
}
if (-not $ready) { throw "topic provisioning failed; run: docker compose logs rocketmq" }
foreach ($topic in @("%DLQ%$($env:RAG_DOCUMENT_GROUP)", "%DLQ%$($env:RAG_SEARCH_GROUP)")) {
    & docker exec praxis-rag-rocketmq sh mqadmin updatetopic -n rocketmq-namesrv:9876 -c DefaultCluster -t $topic -p 6 -r 1 -w 1 *> $null
}
foreach ($group in @($env:RAG_ROCKETMQ_GROUP, $env:RAG_DOCUMENT_GROUP, $env:RAG_SEARCH_GROUP)) {
    & docker exec praxis-rag-rocketmq sh mqadmin updateSubGroup -n rocketmq-namesrv:9876 -c DefaultCluster -g $group -r 16 *> $null
}
Write-Host "   document notification topic: $documentTopic" -ForegroundColor Green

Write-Host "== step 5/5: starting Debezium ==" -ForegroundColor Cyan
Invoke-Compose(@("--profile", "debezium", "up", "-d", "debezium"))

$streaming = $false
$failure = $null
for ($i = 0; $i -lt 60; $i++) {
    $state = (docker inspect --format "{{.State.Status}}" praxis-rag-debezium 2>$null)
    $log = (docker logs praxis-rag-debezium --tail 400 2>&1 | Out-String)
    if ($log -match "Starting streaming|streaming phase|Snapshot completed") { $streaming = $true; break }
    if ($log -match "SRCFG00014|Failed to load mandatory config|Failed to start application|Failed to start quarkus") {
        $failure = "Debezium 配置加载失败（sink/source 属性未生效）"
        break
    }
    if ($state -eq "restarting" -and $log -match "\\bERROR\\b") { $failure = "Debezium 反复重启"; break }
    Start-Sleep -Seconds 3
}
if ($streaming) { Write-Host "   Debezium is streaming the binlog" -ForegroundColor Green }
else {
    Write-Host "   Debezium 未进入 streaming：" -ForegroundColor Red
    if ($failure) { Write-Host "   $failure" -ForegroundColor Red }
    docker logs praxis-rag-debezium --tail 30 2>&1 | Write-Host
    throw "Debezium 启动失败，栈未就绪"
}

Write-Host ""
Write-Host "== containers ==" -ForegroundColor Cyan
docker compose --profile debezium ps --format "table {{.Service}}`t{{.Status}}"

Write-Host ""
Write-Host "== binlog settings ==" -ForegroundColor Cyan
Invoke-Mysql 'SHOW VARIABLES WHERE Variable_name IN ("log_bin","binlog_format","binlog_row_image","server_id");'

Write-Host ""
Write-Host "== document tables ==" -ForegroundColor Cyan
Invoke-Mysql "SHOW TABLES FROM $db LIKE '%document%';"

Write-Host ""
Write-Host "== debezium log tail ==" -ForegroundColor Cyan
docker logs praxis-rag-debezium --tail 50 2>&1

Write-Host ""
Write-Host "DEBEZIUM-STACK-READY document_topic=$documentTopic" -ForegroundColor Green
