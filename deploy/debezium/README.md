# Debezium Server（旧部署，仅回滚保留）

> **已下线 / 仅回滚使用。** 当前生产采集层是分布式 Kafka Connect +
> 两个 Debezium MySQL Connector，配置与操作见
> [`deploy/kafka-connect/README.md`](../kafka-connect/README.md)。
>
> 本目录的 `application.properties`、`application-cache.properties` 不再被
> `docker-compose.yml` 或 `scripts/start-stack.ps1` 引用，只在需要回退到旧
> Debezium Server 部署时使用。新部署不要从这里复制配置。

## 旧部署是什么

两个独立的 Debezium Server 3.7.1 实例，各自使用本地文件保存 binlog offset 与
schema history：

| 实例 | 容器 | 采集表 | 目标 topic | 状态卷 |
| --- | --- | --- | --- | --- |
| 任务连接器 | `praxis-rag-debezium` | `document_jobs` | `RAG_DOCUMENT_TOPIC` | `debezium_data` |
| 缓存连接器 | `praxis-rag-debezium-cache` | 8 张读模型表 | `RAG_CACHE_TOPIC` | `debezium_cache_data` |

旧实例通过 Quarkus `/q/health` 暴露健康状态；新部署已改为 Kafka Connect
`GET /connectors/{name}/status` 与 task 状态，不再使用 `/q/health`。

## 回滚要点

1. 停止并移除 `connect-init`、`connect-1`、`connect-2`。
2. 恢复旧的两个 Debezium Server 服务定义（旧版 `docker-compose.yml` 或发布分支）
   与 `debezium_data` / `debezium_cache_data` 卷。**不要删除这两个卷**，它们保存
   旧位点与 schema history；旧实例会从保存位点继续。
3. 回退 `rag-service` 的环境变量：移除/忽略 `RAG_CONNECT_*`，恢复
   `RAG_DEBEZIUM_HEALTH_URL`、`RAG_DEBEZIUM_CACHE_HEALTH_URL` 与旧版监控代码。
4. 业务消费者必须容忍重复通知：旧位点可能早于切换时已投递的位置，按
   `schedule_version` / 定位标识幂等吸收。
5. 新 Kafka Connect 的 `praxis_connect_*` 内部 topic 不会被旧部署使用；确认无其他
   连接器后再清理。

旧版文件保留仅用于审计和回滚，不再作为契约来源；新契约以
[`docs/cdc-migration.md`](../../docs/cdc-migration.md) 与
[`deploy/kafka-connect/`](../kafka-connect/) 为准。
