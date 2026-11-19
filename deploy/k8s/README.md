# Praxis-RAG Kubernetes 部署（Kustomize，第一版）

本目录负责将 Praxis-RAG 的 API/MCP/Console、worker 和可选的 PDF 高保真微服务
部署到已有集群。MySQL、Qdrant、Kafka、
分布式 Kafka Connect（装好匹配版本的 Debezium MySQL Connector）以及可选 Redis
都由使用方提供，**不在**本仓库的部署范围内。

第一版的取舍：

- 固定副本数，全部集群内访问（ClusterIP）；公网入口、HPA/KEDA、Helm、基础设施
  Operator 留给后续有明确需求的版本。
- 单实例 API（`strategy: Recreate`），文档 worker 起步 1 副本、可单独扩到 2 副本。
- 干净安装的顺序是「schema → CDC 注册 → 应用」，三段都是可复制命令，见 §6。

本地 Compose 开发入口不受影响，见 [`docs/USAGE.md`](../../docs/USAGE.md) §8；
两者的环境变量契约相同，差别只在「地址是容器名还是集群 DNS 名」。

> 命令按 bash 书写。PowerShell 把行尾的 `\` 换成反引号 `` ` ``，`$VAR` 换成 `$env:VAR`。

---

## 1. 目录结构

```
deploy/k8s/
├── common/                     # ConfigMap（普通参数）——先于一切 apply
├── base/                       # API、文档 worker、SAG worker、文档死信 worker
│                               #   + rbac-monitor-ops.yaml（监控页 Kubernetes 标签与扩缩容的 SA/ClusterRole/ClusterRoleBinding）
├── overlays/
│   ├── go/                     # 完整安装入口：Go 进程内 PDF 解析
│   ├── vision/                 # 完整安装入口：+ pdf-parser（高保真 PDF）
│   ├── go-cache/               # go + 展示缓存消费者
│   └── vision-cache/           # vision + 展示缓存消费者
├── components/
│   ├── cache/                  # 缓存消费者、缓存死信、Redis 连接（可选组件）
│   └── persistence/            # /app/data 的 PVC（可选，仅日志留存需要）
├── bootstrap/                  # 一次性 Job，各自独立 kustomization
│   ├── state-check/            # schema 只读检查
│   ├── state-migrate/          # schema --apply
│   ├── connect-precheck/       # Debezium 插件版本 + 连接器现状（只读）
│   └── connect-register/       # 注册两个连接器并等待 RUNNING
└── examples/secret.example.yaml
```

四个 overlay 都是**完整**入口（`resources: [../../base]` 或另一个完整 overlay），
`kubectl kustomize` 能直接渲染出全部对象。`base/` 单独渲染只用于调试。

`bootstrap/` 的 kustomization 刻意**不**声明 ConfigMap：否则升级时重跑 Job 会用
「基础配置」覆盖 overlay 写进去的键（例如 vision 的 `RAG_PDF_SERVICE_URL`）。

---

## 2. 先核对：代码里的事实

这一节的结论都来自当前 checkout 的代码，改动清单前请复核。

| 事实 | 依据 | 部署含义 |
| --- | --- | --- |
| 镜像以名字用户 `rag` 运行（`USER rag`） | `Dockerfile` | 不能设 `runAsNonRoot: true`：kubelet 无法用数字校验名字用户，Pod 会被拒。见 §10 |
| 运行时用户 UID/GID | 在 `alpine:3.21` 上按 Dockerfile 同样步骤实测 **uid=100 / gid=101** | 需要 `fsGroup` 时用 101；基础镜像换代后要重新核对 |
| `/app/config.toml` 把 `RAG_SERVICE_HOST` 设为 `0.0.0.0`，且**每个**进程都会加载它 | `deploy/config.container.toml`、`internal/config/startup.go` | 所有 worker 也会看到 `0.0.0.0`，进而要求 `RAG_SERVICE_TOKEN`。清单给 worker 显式设回 `127.0.0.1`，Token 只留在 API 副本（最小权限） |
| 上传 workspace 的根目录是 `os.TempDir()`，不是 `RAG_SERVICE_DATA_ROOT` | `internal/runtime/documents_publish.go`（`NewUploadCoordinator(stores, os.TempDir())`）、`documents_upload.go` | 解析暂存空间由 `TMPDIR` 决定；清单把它指向每个 Pod 自己的 emptyDir |
| workspace 归属按 `os.Hostname()` 判定 | `internal/state/upload_coordinator.go`（`Node`） | Pod 名就是 hostname；重建后新 Pod 不会认领旧 Pod 的目录 |
| 文档正文/切块/artifact 存在 MySQL（`artifact_blobs` 等），Qdrant 只存向量 | `internal/state/blobs.go` | `/app/data` 不是数据卷，默认无需 PVC（§10） |
| `RAG_LOG_FILE` 不会自建父目录 | `internal/logging/logs.go`（`openLogFile`） | 启用文件日志时路径只能落在已存在的挂载根下 |
| `/health` 固定返回 200，不做依赖探测 | `internal/httpapi/server.go`（`handleHealth`） | HTTP 探针只证明进程能响应；依赖就绪要靠 §7/§8 的检查 |
| `/healthz` 始终 200，配置错误体现在响应体 `status=degraded` | `python-workers/pdf-service/app/main.py` | 同上 |
| 文档任务租约 300s、每 20s 续租、扫描间隔 30s | `internal/state/document_jobs.go`、`cmd/rag-document-worker/main.go` | Pod 被强杀后约 5 分钟才会重投 |
| consumer 并发默认 1、上限 64 | `internal/mq/settings.go` | 首版每 Pod 1 个并发，避免内存放大 |
| MCP 会话保存在进程内，按 namespace 各一套 | `internal/mcp/endpoint.go` | 多副本需要会话粘性或共享存储，首版固定 1 副本 |
| Connect 失败任务重启由 `rag-service` 进程内 supervisor 执行 | `cmd/rag-service/main.go`、`deploy/kafka-connect/README.md` §5 | 多副本时每个副本都会尝试重启，需按 §14 先验证 |
| `rag-connect` 的 `--wait` 是 Go duration 标志 | `cmd/rag-connect/main.go` + 实测 | 必须写 `180s`；写成裸数字会退出 2（本目录已按 `180s` 书写） |

---

## 3. 镜像：构建、推送、本地导入

两个镜像互相独立，彼此不需要对方的构建产物。

```bash
# 1) Go 镜像：构建 Console + 全部 cmd/*，alpine 非 root 运行
docker build -t registry.example.com/praxis-rag/praxis-rag:1.0.0 .
#   受限网络：
#   docker build --build-arg NPM_REGISTRY=https://registry.npmmirror.com \
#                --build-arg GOPROXY=https://goproxy.cn,direct \
#                -t registry.example.com/praxis-rag/praxis-rag:1.0.0 .

# 2) PDF 微服务镜像（只有 vision 入口需要；构建上下文是子目录）
docker build -t registry.example.com/praxis-rag/praxis-rag-pdf-parser:1.0.0 python-workers/pdf-service

# 3) 推送
docker push registry.example.com/praxis-rag/praxis-rag:1.0.0
docker push registry.example.com/praxis-rag/praxis-rag-pdf-parser:1.0.0
```

把仓库地址写进 overlay 的 `images:`（`deploy/k8s/overlays/go/kustomization.yaml`
等四处），或临时用命令行覆盖：

```bash
kubectl kustomize deploy/k8s/overlays/go \
  | sed 's#praxis-rag:1.0.0#registry.example.com/praxis-rag/praxis-rag:1.0.0#g' \
  | kubectl apply -f -
```

### 3.1 私有仓库

```bash
kubectl -n praxis-rag create secret docker-registry registry-cred \
  --docker-server=registry.example.com \
  --docker-username='<user>' --docker-password='<token>'
```

在**自己的 overlay** 里给三个工作负载加上拉取凭据（不要直接改 `base/`）：

```yaml
# deploy/k8s/overlays/local/pull-secret.yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: rag-service
spec:
  template:
    spec:
      imagePullSecrets:
        - name: registry-cred
# rag-document-worker / rag-dead-letter-document / pdf-parser 各写一份同名文件，
# 然后在 overlay 的 kustomization.yaml 里 `patches: [{path: ...}]` 引用。
```

### 3.2 本地集群（kind / minikube / k3d）

清单统一用 `imagePullPolicy: IfNotPresent` + 固定 tag，所以本地导入即可：

```bash
# kind
kind load docker-image praxis-rag:1.0.0 --name <cluster>
kind load docker-image praxis-rag-pdf-parser:1.0.0 --name <cluster>

# minikube
minikube image load praxis-rag:1.0.0
minikube image load praxis-rag-pdf-parser:1.0.0

# k3d
k3d image import praxis-rag:1.0.0 praxis-rag-pdf-parser:1.0.0 -c <cluster>
```

不要用 `:latest`：会退化成 `Always` 拉取，本地导入的镜像被忽略。

---

## 4. Secret 与 ConfigMap 契约

### 4.1 Secret（必须已存在，键名即契约）

清单只按**键名**引用，Secret 的创建方式不限。默认名字 `praxis-rag-secrets`
（改名要同时改 9 处 `secretKeyRef`：`base/` 3 个、`components/cache/` 2 个、
`bootstrap/` 3 个、vision 的 `pdf-parser` 1 个）。

| 键 | 用于 | 必填 |
| --- | --- | --- |
| `RAG_SERVICE_TOKEN` | rag-service（`Bearer` 认证；非 loopback 绑定时为空会启动失败） | ✅ |
| `RAG_MYSQL_PASSWORD` | rag-service、文档 worker、文档死信、缓存 worker、缓存死信、state-migrate/state-check Job | ✅ |
| `RAG_STATE_ENCRYPTION_KEY` | API 与全部 worker（必须**完全一致**，否则读不出 `enc:v1:` 凭据） | ✅ |
| `RAG_MYSQL_DEBEZIUM_PASSWORD` | 仅 connect-register Job（document 连接器账号） | ✅ |
| `RAG_MYSQL_DEBEZIUM_CACHE_PASSWORD` | 仅 connect-register Job（cache 连接器账号） | ✅ |
| `RAG_REDIS_URL` | 仅 `components/cache`：rag-service + rag-cache-worker | 启用缓存时 ✅ |
| `RAG_VLM_API_KEY` | rag-service / 文档 worker / pdf-parser 的可选兜底 | 可选 |
| `RAG_MIGRATE_MYSQL_USER`、`RAG_MIGRATE_MYSQL_PASSWORD` | **仅** state-migrate Job（独立迁移账号） | 可选 |
| `RAG_FEISHU_APP_SECRET`、`RAG_KAFKA_SASL_PASSWORD` | 启用飞书导入 / Kafka SASL 时由你自己的 patch 引用 | 可选 |

创建示例（占位值换成真实凭据；`RAG_SERVICE_TOKEN` 建议随机 32 字节）：

```bash
kubectl -n praxis-rag create secret generic praxis-rag-secrets \
  --from-literal=RAG_SERVICE_TOKEN="$(openssl rand -hex 32)" \
  --from-literal=RAG_MYSQL_PASSWORD='<应用账号口令>' \
  --from-literal=RAG_STATE_ENCRYPTION_KEY='<32+ 字节随机串>' \
  --from-literal=RAG_MYSQL_DEBEZIUM_PASSWORD='<document CDC 口令>' \
  --from-literal=RAG_MYSQL_DEBEZIUM_CACHE_PASSWORD='<cache CDC 口令>'
```

可选键用同样的 `--from-literal=KEY=...` 追加，或 `kubectl -n praxis-rag edit secret praxis-rag-secrets`。
模板见 [`examples/secret.example.yaml`](examples/secret.example.yaml)（只有占位值）。

**凭据不要扩散**：服务 Token 只给 rag-service；CDC 账号只给注册 Job；
独立迁移账号只给迁移 Job。worker 之间共享的只有应用数据库口令与状态加密密钥。

### 4.2 ConfigMap（按环境替换的值）

`common/configmap.yaml` 里的 `*.infra.svc.cluster.local` 是**占位域名**，部署前必须改成
使用方真实地址：

| 键 | 含义 | 注意 |
| --- | --- | --- |
| `QDRANT_URL` | Qdrant HTTP（`:6333`） | 集群内可达即可 |
| `RAG_MYSQL_HOST` / `RAG_MYSQL_PORT` / `RAG_MYSQL_DATABASE` / `RAG_MYSQL_USER` | 状态库 | 同时是连接器的 `database.hostname`，必须对 **Connect worker** 可达 |
| `RAG_KAFKA_BOOTSTRAP_SERVERS` | 业务生产/消费 | 必须对**应用**可达 |
| `RAG_CONNECT_KAFKA_BOOTSTRAP_SERVERS` | 写进连接器配置的 Kafka 地址 | 必须对**Connect worker** 可达；与上一项可能不同 |
| `RAG_CONNECT_URLS` | Connect REST（逗号分隔每个 worker） | 列出全部 worker，单个不可达不会瞎掉监控 |

其余键（topic/group、并发、租约、探针相关、日志）一般不需要改。改完 ConfigMap 后
必须重启工作负载，环境变量不会热更新：

```bash
kubectl -n praxis-rag rollout restart deploy/rag-service deploy/rag-document-worker deploy/rag-dead-letter-document
```

启用 Kafka SASL/TLS 时补这些键（`RAG_KAFKA_SECURITY_PROTOCOL`=SASL_SSL、`RAG_KAFKA_SASL_MECHANISM`、
`RAG_KAFKA_SASL_USERNAME`、`RAG_KAFKA_TLS_CA` 进 ConfigMap，`RAG_KAFKA_SASL_PASSWORD` 进 Secret）。

---

## 5. 外部依赖核对清单

干净安装前逐项确认；已有一套生产环境时，第 2、4 项尤其重要，避免同一个 binlog 来源被重复采集。

### 5.1 MySQL 8

```sql
SELECT @@GLOBAL.binlog_format, @@GLOBAL.binlog_row_image, @@GLOBAL.log_bin, @@GLOBAL.server_id;
-- 期望：ROW / FULL / ON（非空路径）/ 非 0
SHOW MASTER STATUS;                     -- 需要 binlog 位点
```

两个 CDC 账号各需（与 `deploy/mysql/init/10-debezium-user.sh` 一致）：

```sql
GRANT SELECT, RELOAD, SHOW DATABASES, REPLICATION SLAVE, REPLICATION CLIENT ON *.* TO 'debezium'@'%';
```

- 应用账号只需自己库的 DML；迁移若用独立账号，额外需要 DDL（CREATE/ALTER/INDEX）。
- `binlog_row_image` 必须是 `FULL`：cache 连接器靠 before image 发删除事件。
- 每个连接器的 `database.server.id` 全局唯一（默认 184054 / 184055）。

### 5.2 Kafka

两个业务 topic、两个死信 topic 必须存在。分区数决定**并行上限**：

```bash
# 在使用方的 Kafka 客户端里执行（镜像内路径示例）
kafka-topics.sh --bootstrap-server <bootstrap> --describe --topic praxis_document_ingest
kafka-topics.sh --bootstrap-server <bootstrap> --create --if-not-exists \
  --topic praxis_document_ingest --partitions 4 --replication-factor 3
```

仓库的本地脚本 `scripts/start-stack.ps1` 默认建 **4 个分区**；沿用同一套 topic 时，
2 个文档 worker 会各分到 2 个分区。生产复制因子建议 3。

### 5.3 分布式 Kafka Connect

```bash
curl -s http://connect.infra.svc.cluster.local:8083/ | head -c 400      # 集群信息
curl -s http://connect.infra.svc.cluster.local:8083/connector-plugins \
  | grep -o '"class":"io.debezium.connector.mysql.MySqlConnector"'
```

- 每个 worker 必须装**同一版本**的 Debezium MySQL Connector（默认期望 `3.7.1.Final`），
  混合版本会被 `rag-connect register` 拒绝。
- REST 没有认证：只在内网暴露；`RAG_CONNECT_URLS` 只写内网地址。
- 内部 topic（`praxis_connect_configs/offsets/status`）副本数应 ≥ 2，否则单 broker
  故障会丢连接器状态（由 Connect 侧的 `CONFIG_STORAGE_REPLICATION_FACTOR` 等决定，
  不在本目录的清单里）。

### 5.4 Qdrant / Redis

- Qdrant：HTTP 可达即可；集合由应用按知识库自动创建，不需要预建。
- Redis（仅启用展示缓存时）：可达、允许 `GET/SET/DEL/EXPIRE`；
  `RAG_REDIS_PREFIX` 与同实例的其他部署区分开。

---

## 6. 首次安装（干净命名空间）

顺序不能调换：**ConfigMap → Secret → schema → CDC → 应用**。
`bootstrap/` 里的 Job 只引用已存在的 ConfigMap，不会创建应用工作负载，
所以可以安全地在应用之前执行。

```bash
# 0) 命名空间
kubectl create namespace praxis-rag

# 1) 普通参数（占位域名先改成真实地址，见 §4.2）
kubectl apply -k deploy/k8s/common

# 2) Secret（见 §4.1）
kubectl -n praxis-rag create secret generic praxis-rag-secrets \
  --from-literal=RAG_SERVICE_TOKEN="$(openssl rand -hex 32)" \
  --from-literal=RAG_MYSQL_PASSWORD='<应用账号口令>' \
  --from-literal=RAG_STATE_ENCRYPTION_KEY='<32+ 字节随机串>' \
  --from-literal=RAG_MYSQL_DEBEZIUM_PASSWORD='<document CDC 口令>' \
  --from-literal=RAG_MYSQL_DEBEZIUM_CACHE_PASSWORD='<cache CDC 口令>'
```

### 6.1 schema 检查（可选，但建议）

```bash
kubectl apply -k deploy/k8s/bootstrap/state-check
kubectl -n praxis-rag logs job/rag-state-check
```

干净库会打印 `schema=incomplete` 与若干 `MISSING <表>`，Job 状态为 `Failed` ——
这是预期结果，说明需要下一步。已有库应看到 `schema=ready`。

### 6.2 schema 初始化

```bash
kubectl apply -k deploy/k8s/bootstrap/state-migrate
kubectl -n praxis-rag wait --for=condition=complete job/rag-state-migrate --timeout=300s
kubectl -n praxis-rag logs job/rag-state-migrate
```

期望最后一行 `mode=apply ... schema=ready`。

- 这是**唯一**执行 DDL 的地方：应用的 `RAG_STATE_AUTO_SCHEMA=false`，副本不会建表。
- 独立迁移账号：把 `RAG_MIGRATE_MYSQL_USER` / `RAG_MIGRATE_MYSQL_PASSWORD` 加进 Secret
  即可，Job 会自动改用它们，凭据不会进入应用 Pod。
- 重跑必须先删 Job：`kubectl -n praxis-rag delete job rag-state-migrate --ignore-not-found`。

### 6.3 CDC 检查与注册

```bash
# 现状检查（只读）：插件版本 + 已有连接器
kubectl apply -k deploy/k8s/bootstrap/connect-precheck
kubectl -n praxis-rag logs job/rag-connect-precheck

# 注册 + 等待 RUNNING
kubectl apply -k deploy/k8s/bootstrap/connect-register
kubectl -n praxis-rag wait --for=condition=complete job/rag-connect-register --timeout=420s
kubectl -n praxis-rag logs job/rag-connect-register
```

`connect-precheck` 在干净环境里 `plugins` 通过、`status` 因连接器不存在而失败 —— 预期行为。
`connect-register` 成功时日志形如：

```
Kafka Connect worker 插件版本：http://connect...=3.7.1.Final
已创建连接器 praxis-document-cdc（document-cdc.json）
已创建连接器 praxis-cache-cdc（cache-cdc.json）
就绪：...
```

**已有生产连接器时**：先跑 `connect-precheck` 看两个固定名称连接器的状态与承载 worker。
`register` 是幂等的——配置相同不会更新；配置不同会更新（已保存 binlog 位点不受影响）。
不要在同一份 binlog 上再注册一套不同名字的连接器。

失败排查：

| 现象 | 先看 |
| --- | --- |
| `校验 Debezium MySQL Connector 版本失败` | worker 是否都装了同版本插件；`RAG_CONNECT_URLS` 是否包含全部 worker |
| `注册连接器 ... 失败：401/404` | Connect REST 地址、路径前缀、网络策略 |
| `register` 退出 2 | 标志格式（`--wait 180s` 带单位）、模板占位符缺失（口令没注入） |
| `wait` 超时 | `connect-precheck` 看 task 状态与 `trace`；MySQL 账号权限、binlog 配置 |
| Job `Failed` | `kubectl -n praxis-rag get pod -l app.kubernetes.io/component=bootstrap` 找到 Pod，`kubectl -n praxis-rag logs <pod> --previous` |

### 6.4 启动应用

```bash
# 只要纯文本解析（无 python 依赖）：
kubectl apply -k deploy/k8s/overlays/go

# 或者高保真 PDF（额外拉起 pdf-parser）：
# kubectl apply -k deploy/k8s/overlays/vision

# 需要展示缓存时用带 -cache 的入口（Secret 里要有 RAG_REDIS_URL）：
# kubectl apply -k deploy/k8s/overlays/go-cache
# kubectl apply -k deploy/k8s/overlays/vision-cache

kubectl -n praxis-rag rollout status deploy/rag-service --timeout=180s
kubectl -n praxis-rag rollout status deploy/rag-document-worker --timeout=180s
kubectl -n praxis-rag rollout status deploy/rag-sag-worker --timeout=180s
kubectl -n praxis-rag get pods -o wide
```

四个 overlay 都部署一个 `rag-sag-worker` 副本。启用 SAG 抽取前，在 Console「模型配置」添加 `kind=extraction` 模型。升级时用包含 `rag-sag-worker` 的 Go 镜像先运行 §6.2 的 `state-migrate` Job，创建 `sag_extraction_jobs`，再更新应用。启用 Kafka ACL 的环境需允许消费组 `praxis_sag_extraction_workers` 读取 `RAG_CACHE_TOPIC`。可用 `kubectl -n praxis-rag logs deploy/rag-sag-worker` 查看订阅与抽取日志。

切换入口（go ↔ vision）用同一条 `apply` 覆盖即可；从 vision 切回 go 时
`pdf-parser` 不会被自动删除，需要显式清理：

```bash
kubectl -n praxis-rag delete deploy/pdf-parser svc/pdf-parser --ignore-not-found
```

（`kubectl apply` 不会删除已不在清单里的对象。）

---

## 7. 访问、冒烟与首次上传检索

### 7.1 端口转发

```bash
kubectl -n praxis-rag port-forward svc/rag-service 8011:8011
```

- `port-forward` 在 Pod 网络命名空间内转发：服务看到的客户端地址与 `Host` 都是
  `127.0.0.1`，正好满足 `/v1/admin/*` 的「本机直连」条件（loopback 来源 +
  loopback Host + 无代理头）。所以管理接口在本机转发下可用，**换成 Ingress 就不可用**。
- ConfigMap 里的 `RAG_MCP_ALLOWED_HOSTS/ORIGINS` 就是按这个场景写的白名单。
  以后加正式域名/集群内 MCP 客户端，必须把对应 host 与 origin 加进去，否则 MCP 返回 403。

### 7.2 冒烟

```bash
curl -s http://127.0.0.1:8011/health        # 200 + 状态摘要（不探测依赖）
curl -s http://127.0.0.1:8011/openapi.json | head -c 200
# 浏览器打开 Console：
#   http://127.0.0.1:8011/console
```

依赖是否就绪用监控接口（只读、仅本机直连/管理员会话）：

```bash
curl -s http://127.0.0.1:8011/v1/admin/monitor/overview \
  | python -m json.tool | head -80
```

关注 `status`、`dependencies[].status`、`pipeline.debezium.tasks/cache`、
`pipeline.kafka.groups`、`pipeline.worker`。数字 `null` 表示未知，不是 0。

### 7.3 首次上传 → 检索（带出处）

```bash
TOKEN=$(kubectl -n praxis-rag get secret praxis-rag-secrets -o jsonpath='{.data.RAG_SERVICE_TOKEN}' | base64 -d)
NS=default                         # 需要先在 Console 建好同名知识库

printf 'kyoto-evidence-7731 是本次验收的唯一短语。\n' > /tmp/evidence-kyoto-7731.txt

curl -s -X POST http://127.0.0.1:8011/v1/document-batches \
  -H "Authorization: Bearer $TOKEN" -H "X-RAG-Namespace: $NS" \
  -F "files=@/tmp/evidence-kyoto-7731.txt"

# 记下响应里的 artifact_id，然后轮询到 succeeded
curl -s "http://127.0.0.1:8011/v1/document-jobs/<artifact_id>" \
  -H "Authorization: Bearer $TOKEN" -H "X-RAG-Namespace: $NS"

# succeeded 之后做一次同步搜索，检查片段与出处
curl -s -X POST http://127.0.0.1:8011/v1/search \
  -H "Authorization: Bearer $TOKEN" -H "X-RAG-Namespace: $NS" \
  -H "Content-Type: application/json" \
  -d '{"query":"kyoto-evidence-7731","retrieval":"hybrid","rerank":"cross_encoder","candidate_k":10,"top_k":5,"filters":{}}'
```

通过标准：任务状态到 `succeeded`；搜索结果里能取到该文档的片段与出处
（文档名、片段定位，有 PDF 时还有页码）。`artifact_id` 不代表已索引——
只有 `succeeded` 才可以检索。

MCP 侧同样的验收（端点 `/mcp`，需要 `Authorization: Bearer $TOKEN` 与 `X-RAG-Namespace`）。
MCP 是**有状态**会话：先用 `initialize` 拿到 `Mcp-Session-Id`，后续请求都要带上它。

```bash
# 1) 握手：响应头里的 Mcp-Session-Id 就是会话标识
curl -si -X POST http://127.0.0.1:8011/mcp \
  -H "Authorization: Bearer $TOKEN" -H "X-RAG-Namespace: $NS" \
  -H "Content-Type: application/json" -H "Accept: application/json, text/event-stream" \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"acceptance","version":"1"}}}'

# 2) 完成初始化通知（同上请求头 + Mcp-Session-Id）
curl -s -X POST http://127.0.0.1:8011/mcp \
  -H "Authorization: Bearer $TOKEN" -H "X-RAG-Namespace: $NS" \
  -H "Mcp-Session-Id: <上一步的会话 id>" \
  -H "Content-Type: application/json" -H "Accept: application/json, text/event-stream" \
  -d '{"jsonrpc":"2.0","method":"notifications/initialized"}'

# 3) 调用 search 工具，检查返回的证据里带出处
curl -s -X POST http://127.0.0.1:8011/mcp \
  -H "Authorization: Bearer $TOKEN" -H "X-RAG-Namespace: $NS" \
  -H "Mcp-Session-Id: <上一步的会话 id>" \
  -H "Content-Type: application/json" -H "Accept: application/json, text/event-stream" \
  -d '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"search","arguments":{"query":"kyoto-evidence-7731"}}}'
```

响应可能是 `text/event-stream`（每条 data 一行 JSON），curl 直接打印即可。
`protocolVersion` 由服务端协商，服务端会回自己能支持的版本；工具清单固定为
`search` / `add_document` / `update_document` / `delete_document` / `get_document_status`。
会话只存在于当前 Pod（首版单副本），换 Pod 后旧 `Mcp-Session-Id` 失效，重新握手即可。

### 7.4 vision 模式验收（一页 PDF）

前提：知识库已配置可用的远端 VLM（Console「模型设置」），或 pdf-parser 配置了
服务级 `RAG_VLM_BASE_URL`/`RAG_VLM_MODEL`。

```bash
# pdf-parser 启动日志应显示 converter=vision、无 config_error
kubectl -n praxis-rag logs deploy/pdf-parser --tail=50
# 应用侧能否解析到服务名（Go 镜像自带 wget）
kubectl -n praxis-rag exec deploy/rag-service -- \
  wget -qO- http://pdf-parser:8091/healthz | head -c 300
```

上传一页 PDF（走同一条 `/v1/document-batches`），`succeeded` 后做搜索，
检查证据里带页码与对应图/表 artifact。

若用 `RAG_PDF_SERVICE_CONVERTER=fake`（只返回固定清单，不调用模型），
**只能记为协议冒烟**，真实 VLM 验收仍待执行。

---

## 8. 文档 worker 扩容（1 → 2）

扩容有两条等价路径：

```bash
# A. kubectl（README 原有方式）
kubectl -n praxis-rag scale deploy/rag-document-worker --replicas=2
kubectl -n praxis-rag rollout status deploy/rag-document-worker --timeout=180s
kubectl -n praxis-rag get pods -l app.kubernetes.io/component=document-worker -o wide
```

```bash
# B. Console 监控页（「文档 Worker」标签页的「副本数」卡片）
#    前提：rag-service 已解析到 Kubernetes 凭据——集群内使用
#    rbac-monitor-ops.yaml 的 ServiceAccount（base/ 已接线，正常 apply 即
#    生效），集群外设置 RAG_K8S_KUBECONFIG（可选 RAG_K8S_CONTEXT）。
#    ClusterRole 的 get/list/watch 覆盖读取 Deployment 副本数，
#    patch 覆盖 spec.replicas 的 merge patch（与滚动重启共用同一凭据）。
#    卡片会实时显示期望/就绪副本数，输入目标副本数点「应用副本数」即可。
#    实现 = 对 Deployment 的 spec.replicas 发 merge patch（与 kubectl scale
#    等价，admission/quota 同样生效），进程内 k8sops 客户端每次调用重读
#    kubelet 投影轮换的 token（kubeconfig 的 tokenFile 同理）。
```

确认两件事：

```bash
# 1) 两个实例都在同一消费组、各自分到分区（在 Kafka 客户端侧）
kafka-consumer-groups.sh --bootstrap-server <bootstrap> \
  --describe --group praxis_document_ingest_workers

# 2) 心跳表里出现两个不同实例（Pod 名-pid）
#    走监控接口最省事：
curl -s http://127.0.0.1:8011/v1/admin/monitor/overview | python -m json.tool \
  | grep -A6 '"worker"'
```

手头没有 Kafka 客户端时，用等价的 SQL：

```sql
SELECT role, instance, updated_at FROM runtime_heartbeats
WHERE role = 'document-worker' ORDER BY updated_at DESC;
```

调参依据：

- 并行度上限 = **分区数**。仓库脚本默认 4 分区；`RAG_DOCUMENT_WORKER_CONCURRENCY=1`
  时，2 个 Pod 各占 2 个分区。要提高单文档吞吐再动并发（每并发会多占一份解析内存）。
- 扩容前后用同一批文档比较：排队时长（`pipeline.document_jobs` 的最老排队时长）、
  单位时间完成数、下游限流（远端 Embedding/VLM 429）。PDF 高峰期 Concurrent
  解析数增加会同比放大远端 API 调用量。

缩回 1 副本：

```bash
kubectl -n praxis-rag scale deploy/rag-document-worker --replicas=1
```

缩容时正在处理的分区会 rebalance 给存活 Pod；未提交的消息重新投递，
由 `schedule_version` + 租约保证幂等。

### 8.1 监控页的运维操作（立即重投 / 重启连接器 / worker 扩缩容 / 滚动重启）

Console 监控页固定每秒自动刷新（页头只显示「自动刷新 · 每 1 秒」和最近一次成功更新时间，没有自动刷新开关、手动刷新或失败重试按钮；失败保留上次数据并在下一秒重试，每个数据类同一时间最多一个请求）。除原有的三类操作外，管理员的 `Kubernetes` 标签还提供工作负载/Pod 查看与滚动重启：

| 操作 | 位置 | 后端端点 | 前提 |
| --- | --- | --- | --- |
| 立即重投 | MySQL / 文档 Worker 标签页「卡死处理中」行 | `POST /v1/admin/monitor/actions/requeue-jobs` | 无（只依赖 MySQL） |
| 重启连接器 | Debezium 标签页失败采集行的按钮 | `POST /v1/admin/monitor/actions/connectors/{name}/restart` | `RAG_CONNECT_URLS` 已配置 |
| worker 扩缩容 | 文档 Worker 标签页「副本数」卡片 | `GET/POST /v1/admin/monitor/actions/worker-deployment(/scale)` | 已解析 Kubernetes 凭据（ServiceAccount 或 kubeconfig）+ RBAC |
| 滚动重启 | Kubernetes 标签页工作负载行 | `POST /v1/admin/k8s/workloads/{resource}/{namespace}/{name}/restart` | 已挂载 ServiceAccount + ClusterRole RBAC（或集群外 kubeconfig） |

要点：

- **立即重投**复用 Sweep 的全部语义（递增 `schedule_version`、尊重 3 次
  执行预算、终态判定共享），只是跳过周期扫描的静默窗口；用于「不想等
  30s 扫描」的人工补救，不会产生第四次执行或重复可检索版本。
- **重启连接器**面向 supervisor 预算耗尽（`restart_exhausted`）或 FAILED
  任务：重启连接器及其任务的同时清空该连接器的重启预算，自动重启循环
  拿到新预算。只接受两个固定连接器名（`praxis-document-cdc` /
  `praxis-cache-cdc`），不会转发任意名字。
- **扩缩容与滚动重启**的 RBAC 在 `base/rbac-monitor-ops.yaml`：ServiceAccount
  `rag-monitor-ops` + ClusterRole/ClusterRoleBinding（全部命名空间的
  `pods`/`replicationcontrollers`、`apps` 的 `deployments`/`statefulsets`/
  `daemonsets`/`replicasets`、`batch` 的 `jobs`/`cronjobs` 为
  `get`/`list`/`watch`；另对 `deployments`/`statefulsets`/`daemonsets`
  授予 `patch`）。扩缩容与 Kubernetes 标签共用同一份已解析凭据（集群内
  ServiceAccount 或集群外 `RAG_K8S_KUBECONFIG`）：`get` 已包含在 list/watch
  规则中，`patch` 授权覆盖 `rag-document-worker` 的 `spec.replicas` 和滚动
  重启；原 `resourceNames` 限定单 Deployment 的 Role 已不再使用。两者都未
  配置时，rag-service 正常启动，扩缩容接口返回 503「Kubernetes 操作未启用」、
  卡片显示「未启用」，Kubernetes 标签显示配置提示；RBAC 缺少权限时页面显示
  502 错误提示。Compose/宿主机部署不受影响。
- **Kubernetes 标签**（仅管理员）默认展示「工作负载」，旁边可切「全部 Pod」：
  覆盖已连接集群全部命名空间（含系统命名空间与 Praxis-RAG 以外的应用），
  支持命名空间/名称筛选，展开行按 ownerReferences 显示归属 Pod。滚动重启只
  对可滚动更新的 Deployment/StatefulSet/DaemonSet 可用，paused/Recreate/
  OnDelete/`partition>0` 会在行内说明不可用原因。
- 三个监控操作端点都在 `/v1/admin/*` 边界内（管理员会话 + CSRF，或本机直连
  破窗），与只读监控接口同一鉴权。Kubernetes 查询/重启使用更严格的
  `authAdminSession`：只接受已登录管理员会话，**没有本机直连兜底**，
  无会话 401、普通用户 403。
- 操作按钮按 overview 返回的 `actions` 能力块显示：能力为假（后端未装配/
  老版本）时按钮直接不渲染，而不是调一个会 503 的端点。

---

## 9. 重建与重投演练（隔离环境执行）

目的：证明「Pod 在解析过程中消失」不会产生重复可检索版本，也不会丢数据。

### 9.1 准备

- 用独立的测试库/知识库/topic/group/collection（不要把生产数据当靶子）。
- 上传一份**耗时足够长**的文档（大 PDF 或多页扫描件），确认
  `GET /v1/document-jobs/<artifact_id>` 处于 `processing`。
- 记下处理它的 Pod：`kubectl -n praxis-rag get pods -l app.kubernetes.io/component=document-worker -o wide`，
  以及日志里的 `artifact_id`。

### 9.2 两种终止方式都要做，并分别记录

```bash
# A. 正常终止（SIGTERM，走宽限期）
kubectl -n praxis-rag delete pod <worker-pod>

# B. 强制失联（SIGKILL，无善后）
kubectl -n praxis-rag delete pod <worker-pod> --grace-period=0 --force
```

预期时间线：A/B 都会立刻取消消费循环与进行中的解析；`document_jobs.lease_until`
停在最后一次续租处（≤ 终止时刻 + 300s），`RAG_DOCUMENT_WORKER_RECOVER_SECONDS=30`
的扫描在其后把任务重新投递（递增 `schedule_version`，Debezium 发新通知），
新副本领取并完成。

> 演练想更快看到重投，可临时把 `RAG_DOCUMENT_JOB_LEASE_SECONDS` 调到 60 并重启
> worker；完成后改回。

### 9.3 判定与观测

| 观察点 | 命令 | 通过标准 |
| --- | --- | --- |
| MySQL 任务终态 | `GET /v1/document-jobs/<artifact_id>` | 最终 `succeeded`（不是永久 `processing`/`failed`） |
| Qdrant 发布结果 | 同一文档再次搜索 | 只出现**一个**可检索版本，片段内容与页码正常；无重复证据 |
| 旧 workspace 登记 | `SELECT id, workspace, workspace_node FROM upload_operations WHERE workspace IS NOT NULL;` | 只可能残留**旧 Pod** 的登记行 |
| 实例身份 | `runtime_heartbeats` 或监控 `pipeline.worker` | 新 Pod 名-pid，旧实例在心跳窗口外消失 |

关于旧 workspace 登记的**实际影响**（这是设计使然，不是缺陷）：

- `internal/state/upload_coordinator.go` 只清理 `workspace_node == 本进程 hostname` 的目录。
  Pod 重建后 hostname 变了，新 Pod 永远不会认领旧登记行。
- 因为本部署把暂存目录放在**每个 Pod 自己的 emptyDir**，旧目录随 Pod 一起消失，
  磁盘上没有残留；MySQL 里那一行只是历史字符串，不影响上传/检索/清理流程，
  也不会触发 `cleanup_pending` 重试风暴。
- **反例**：如果把 `TMPDIR` 换成跨 Pod 共享的 PVC，这些目录会永久残留且无人认领。
  这是「解析暂存不要共享卷」的第二个理由（第一个是并发解析互相看不到对方的临时文件）。

若观察到**数据丢失**（任务再也到不了 succeeded）或**重复可见**（同一片段出现两次、
同一文档出现两个版本），先按 §9.4 定位，修好并用针对性回归验证后，再声称支持该故障场景。

### 9.4 排查方向

```bash
# 任务为什么没终态：租约是否被反复续期（说明还有进程活着）、阶段停在哪
kubectl -n praxis-rag logs deploy/rag-document-worker --tail=200 | grep <artifact_id>
curl -s "http://127.0.0.1:8011/v1/uploads" -H "Authorization: Bearer $TOKEN" -H "X-RAG-Namespace: $NS"

# 是否有卡死任务（租约过期）与失败数
curl -s http://127.0.0.1:8011/v1/admin/monitor/overview | python -m json.tool | grep -i stuck_processing
```

---

## 10. 存储、临时目录与文件权限

### 10.1 两个目录，别混为一谈

| 目录 | 由谁决定 | 内容 | 生命周期 |
| --- | --- | --- | --- |
| `RAG_SERVICE_DATA_ROOT=/app/data` | ConfigMap | 仅「确保目录存在」+ 可选的 `RAG_LOG_FILE` | 容器可写层，Pod 删除即丢 |
| `os.TempDir()`（本部署 `TMPDIR=/var/tmp/rag`） | Deployment env | 上传 workspace `rag-txn-<opid>`、`rag-ingest-*`、`rag-word-*`、`rag-pdf-service-*` 解析暂存 | emptyDir，Pod 删除即丢 |

**持久数据不在这两个目录里**：文档/切块/artifact/任务在 MySQL，向量在 Qdrant，
消费位点在 Kafka，连接器位点在 Connect 的 offset topic。所以默认部署**不需要 PVC**。

### 10.2 什么时候才需要 PVC

只有一种情况：希望 Console 日志页读的 JSON Lines 文件跨 Pod 重建保留。

在仓库里加一个自己的 overlay（同根目录下的相对路径最稳，kustomize 默认会拒绝
引用 kustomization 根目录之外的文件）：

```bash
mkdir -p deploy/k8s/overlays/local
cat > deploy/k8s/overlays/local/kustomization.yaml <<'EOF'
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
namespace: praxis-rag
resources:
  - ../vision            # 或者 ../go / ../go-cache / ../vision-cache
components:
  - ../../components/persistence
EOF
kubectl apply -k deploy/k8s/overlays/local
```

`components/persistence` 只给 **rag-service** 挂 `ReadWriteOnce` 卷到 `/app/data`，
并把 `RAG_LOG_FILE` 指到 `/app/data/praxis-rag.jsonl`（不能写子目录：`openLogFile`
不会 `MkdirAll`）。`fsGroup: 101` 对应镜像里 `rag` 组。

卷的边界：

- **ReadWriteOnce** = 单节点读写。它不是跨节点共享方案；同一节点上多 Pod 可挂，
  跨节点调度时第二个 Pod 会卡在 `Multi-Attach`/`Pending`。
- 不能缩容（PVC 的 `storage` 只增不减；多数 CSI 支持在线扩容）。
- 不要把它挂给文档 worker 当暂存目录（§9.3 的原因）。

### 10.3 权限与非 root

- Go 镜像的非 root 用户是**名字**用户 `rag`（实测 uid=100/gid=101）。
  kubelet 对名字用户无法校验 `runAsNonRoot`，设置它会以
  `container has runAsNonRoot and image has non-numeric user (rag)` 拒绝启动；
  写死 `runAsUser: 100` 又会在基础镜像改 UID 时让 `/app` 失去写权限。
  因此清单**不设置**这两个字段，改用 `allowPrivilegeEscalation: false` +
  `capabilities.drop: [ALL]` + `seccompProfile: RuntimeDefault` 收敛权限。
- `emptyDir` 由 kubelet 以可写模式创建，非 root 进程可直接写，不需要 `fsGroup`。
- pdf-service 的 UID 在构建期固定（`useradd --uid 10001`），所以 vision 入口可以
  给出 `runAsUser: 10001` + `runAsNonRoot: true` + `fsGroup: 10001`。
- 复核实际用户：

```bash
kubectl -n praxis-rag exec deploy/rag-service -- id
kubectl -n praxis-rag exec deploy/pdf-parser -- id       # 仅 vision 入口
```

`readOnlyRootFilesystem` 首版保持 false：Go 镜像的 `/app/data` 落在可写层，
PDF 侧 `TMPDIR` 已挂 emptyDir。确认无其他写入后可自行打开。

### 10.4 容量

`emptyDir.sizeLimit` 按「单份文件大小 × 并发 × 2~3」给：文档 worker 20Gi、
pdf-parser 10Gi、API 2Gi、死信/缓存 256Mi~512Mi。超过 `sizeLimit` 会被驱逐重来
（大文件解析会因此失败），所以它必须明显大于最大单文档的渲染放大倍数。

---

## 11. 探针、健康语义与观测

| 工作负载 | 探针 | 真实含义 |
| --- | --- | --- |
| rag-service | `/health`（readiness + liveness） | 进程能响应 HTTP；**不代表** MySQL/Qdrant/Kafka/Connect 就绪 |
| pdf-parser | `/healthz`（readiness + liveness） | 进程能响应；配置错误只体现在响应体 `status=degraded` |
| 文档 worker / 死信 / 缓存 worker | 无探针 | 存活 = 进程退出后 kubelet 重启；状态看心跳 + 消费进度 |

依赖就绪的**独立验收**（替代「探针通过」）：

```bash
curl -s http://127.0.0.1:8011/v1/admin/monitor/overview | python -m json.tool
```

看 `dependencies[]`（MySQL/Redis/Qdrant/Kafka/PDF）、`pipeline.debezium.tasks|.cache`、
`pipeline.kafka.groups.document|.cache`、`pipeline.worker.instances`。

常用观测命令：

```bash
kubectl -n praxis-rag get pods,deploy,svc
kubectl -n praxis-rag logs deploy/rag-service --tail=100
kubectl -n praxis-rag logs deploy/rag-document-worker -f
kubectl -n praxis-rag logs deploy/rag-document-worker --previous     # 重启前的日志
kubectl -n praxis-rag describe pod <pod>                             # 调度/挂载/探针失败
kubectl -n praxis-rag get events --sort-by=.lastTimestamp | tail -30
```

worker 的「活着」证据（三选一即可）：`runtime_heartbeats` 有新行、
`pipeline.worker.instances` 与副本数一致、Kafka 消费组有活跃成员。

---

## 12. 资源与调参依据

首版给的是保守**估算**（不是压测结论），按 PDF 体量和远端配额在测试环境校准：

| 工作负载 | requests | limits | 依据 |
| --- | --- | --- | --- |
| rag-service | 200m / 512Mi | 1 CPU / 1Gi | 同步检索 + Console 静态资源 + 监控聚合；检索批量大小与 rerank 候选数决定峰值 |
| rag-document-worker | 250m / 512Mi | 2 CPU / 2Gi | 一个 PDF 解析在内存里要放下页面位图与切块文本；`RAG_DOCUMENT_WORKER_CONCURRENCY` 每加 1 大致再要一份 |
| rag-sag-worker | 50m / 128Mi | 500m / 512Mi | 每个进程最多并行抽取 4 个片段，模型在远端执行 |
| pdf-parser | 500m / 1Gi | 2 CPU / 2Gi | PyMuPDF 渲染单页约几十 MB 峰值；`WORKERS × CONCURRENCY` 决定并发 |
| 死信 / 缓存 worker | 50~100m / 128~256Mi | 300~500m / 256~512Mi | 只做状态/水位更新 |

关键旋钮与联动关系：

- `RAG_PDF_SERVICE_WORKERS` × `RAG_PDF_SERVICE_CONCURRENCY` = 单个 pdf-parser Pod 的并发上限，
  首版 **1×1**。扩 pdf-parser 副本前先确认远端 VLM 的 QPS 配额，否则多个 Pod 一起把
  配额打满，表现为大面积 429/超时。
- `RAG_PDF_SERVICE_PARSE_TIMEOUT`(120s) 必须**小于** `RAG_PDF_SERVICE_TIMEOUT`(900s)，
  否则启动即报配置错误；临时提高总预算时两个一起调。
- `terminationGracePeriodSeconds`：API 30s（HTTP 优雅关闭 5s + 连接池关闭）、
  worker 30s、pdf-parser 60s。注意 worker 收到 SIGTERM 会立刻取消进行中的解析，
  宽限期再长也救不回长解析——重投由租约保证（§9）。
- `RAG_DOCUMENT_JOB_LEASE_SECONDS`(300s) 与 `RAG_DOCUMENT_JOB_HEARTBEAT_SECONDS`(20s) 的关系：
  进程活着就每 20s 续租，所以**单次解析再长也不会被误判**超时；只有进程消失（被强杀、
  节点故障）时才等满约 300s 才重投。要缩短故障恢复时间就调小租约（并保持续租间隔约为
  租约的 1/15），代价是节点短暂抖动时更容易被别的副本接管。

---

## 13. 升级与回退

### 13.1 版本关系

一次发布涉及四类对象，必须**同版本**推进：

| 对象 | 变更方式 | 备注 |
| --- | --- | --- |
| 镜像 | overlay 的 `images:`（4 个入口 + 4 个 bootstrap 都要改） | Job 用的也是同一个 Go 镜像 tag |
| ConfigMap | `common/configmap.yaml`（或 overlay patch） | 改完要 `rollout restart` |
| Secret | `kubectl edit secret` / 重新 create | 轮换加密密钥会导致旧 `enc:v1:` 凭据读不出，见下 |
| schema | `bootstrap/state-migrate` | 只允许向前；旧版本可能读不懂新列 |

### 13.2 升级顺序

```bash
# 1) 改 overlay 的 images: 与需要变更的 ConfigMap 键
kubectl apply -k deploy/k8s/common
# 2) schema（幂等、增量；失败就停在这里，别继续）
kubectl -n praxis-rag delete job rag-state-migrate --ignore-not-found
kubectl apply -k deploy/k8s/bootstrap/state-migrate
kubectl -n praxis-rag wait --for=condition=complete job/rag-state-migrate --timeout=300s
kubectl -n praxis-rag logs job/rag-state-migrate
# 3) 业务应用（先 worker 后 API，或反过来都行；API 会短暂中断）
kubectl apply -k deploy/k8s/overlays/vision
kubectl -n praxis-rag rollout status deploy/rag-document-worker --timeout=180s
kubectl -n praxis-rag rollout status deploy/rag-service --timeout=180s
```

### 13.3 回退

```bash
# 1) 把 overlay 的 images.tag 改回上一版，然后重新 apply
kubectl apply -k deploy/k8s/overlays/vision
kubectl -n praxis-rag rollout status deploy/rag-service --timeout=180s
```

- **先查 schema 兼容性**：旧镜像能不能读当前 schema。只向前、不可逆的迁移
  （删列/改语义）不能靠回退镜像解决，必须用备份恢复。
- **保留**：MySQL 数据、Qdrant collection、Kafka consumer offsets、PVC（若启用）。
  回退时不要删除 topic/group/collection，否则会重放或丢位点。
- **ConfigMap 也要回退**：新版本引入的键对旧版本无害，但被改过语义的键必须一起退。
- **Job 不用回退**：一次性 Job 的记录保留即可；需要重跑先 `delete job`。
- **镜像回退不要复用 `:latest`**，否则节点缓存会掩盖版本变化。

### 13.4 从 Compose / 宿主机切流（同一套 Kafka/MySQL 时）

1. 先让旧部署的**文档 worker** 停止领取新任务：Compose 里没有内置的文档 worker
   服务（见 [`docs/USAGE.md`](../../docs/USAGE.md) §8），通常是宿主机进程或
   systemd 单元，停掉它并等其进行中的任务结束或租约过期：
   `systemctl stop praxis-rag-document-worker`（或你实际使用的进程管理器）。
2. 再在集群里启动 worker（同名 `RAG_DOCUMENT_GROUP`），它会按已提交位点继续。
3. 观察 `pipeline.kafka.groups.document` 的 lag 归零，再切 API 流量。
4. 切回：反向执行——集群 worker 缩到 0，旧 worker 起回来；两种 worker
   都按 `schedule_version` + 租约幂等，过渡期的重复投递不会产生重复版本。
5. 记录切流窗口内未完成的任务 id，切换后确认它们最终 `succeeded`。

---

## 14. 多副本 API：先验证，再改副本数

首版固定 1 副本。要扩到 2+ 之前必须逐项验证（本仓库没有集群，以下均未实测）：

1. **MCP 会话路由**：`internal/mcp/endpoint.go` 的 SDK server / Streamable HTTP
   handler 是进程内状态，`Mcp-Session-Id` 只对创建它的 Pod 有效。必须先有会话粘性
   （Service 的 `sessionAffinity` 不足以覆盖 SSE 重连，通常要做 Ingress 侧一致性哈希）
   或把会话外置。
2. **断线重连**：客户端在重连时若被路由到另一个 Pod，会收到 404/session not found，
   需要确认客户端的重试语义，或让网关按会话粘。
3. **Connect supervisor 并发**：`cmd/rag-service/main.go` 在每个副本里启动失败任务
   重启循环；`deploy/kafka-connect/README.md` §5 已提示多个副本会各自尝试重启
   （幂等但会重复调用）。多副本时只保留一个副本启用
   （其余设 `RAG_CONNECT_SUPERVISOR_ENABLED=false`）。
4. **更新策略**：确认可以零中断后再从 `Recreate` 换成 `RollingUpdate`。
5. Console 的登录会话（MySQL `sessions` 表）本身是共享的，不受副本数影响。

---

## 15. 验证范围

可复跑的离线检查（需要 kubectl 与 python3 + PyYAML）：

```bash
python3 deploy/k8s/tests/verify_manifests.py      # 渲染全部入口并断言契约，exit 0 = 通过
```

它做的事：用 `kubectl kustomize` 渲染 `common/`、四个 overlay、四个 bootstrap 入口，
然后断言命令、镜像、Service selector、Secret 键、DNS 名、探针、资源、安全上下文、
以及「bootstrap 只产出 Job」和「`--wait` 带单位」这类约定。

本轮交付的验证（无集群，全部离线）：

| 项目 | 结果 |
| --- | --- |
| `kubectl kustomize` 渲染 `overlays/go`、`overlays/vision`、`overlays/go-cache`、`overlays/vision-cache` | 4/4 成功（kubectl v1.36.1 / kustomize v5.8.1） |
| 渲染 `bootstrap/*`（4 个）与 `common/` | 5/5 成功，bootstrap 只产出 Job |
| 清单契约检查（命令、镜像、Service selector、Secret 键、DNS、探针、资源、安全上下文） | 534 项断言全部通过 |
| `components/cache` 叠加渲染（`overlays/go-cache`、`overlays/vision-cache`） | 通过：两个缓存消费者 + `RAG_REDIS_URL` + `RAG_REDIS_ENABLED` |
| `components/persistence` 叠加渲染（临时 overlay，见 §10.2） | 通过：PVC + `/app/data` 挂载 + `RAG_LOG_FILE` + `fsGroup=101` |
| 连接器模板用清单实际注入的变量展开 | `rag-connect register --offline` 成功，两个连接器各 31 项配置 |
| `--wait` 参数格式 | 实测 `--wait 180` 报 `parse error`；已统一改为 `180s` |
| 镜像运行时 UID/GID | 在 alpine:3.21 上按 Dockerfile 步骤实测 uid=100/gid=101 |
| `go build` / `go vet ./cmd/rag-connect` | 通过（本轮只改了该包的一行注释） |
| 服务端校验（server-side dry-run） | **未执行**：无 API Server 可达 |

未验证（需要真实集群与依赖，按 §6/§8/§9 逐项补）：

- server-side dry-run / apply 后的实际调度（资源、亲和、存储类、网络策略）。
- 真实 MySQL 的 schema 迁移、CDC 注册与 binlog 位点推进。
- 上传 → `succeeded` → 同步搜索带出处的端到端链路。
- vision：真实 VLM 的解析、页码/图/表证据；`fake` 只能算协议冒烟。
- 删除 worker Pod 后的重投与新副本接管（§9 的判定标准已写明）。
- 文档 worker 1→2 的分区分配与下游限流实测。
