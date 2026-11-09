# 角色、知识库归属与权限管理

本文描述 Praxis-RAG 的账号角色、知识库归属隔离，以及基于邀请链接的知识库共享权限。

## 1. 角色

| 角色 | `role` 值 | 能力 |
| --- | --- | --- |
| 管理员 | `admin` | 管理用户与角色、API Key、知识库配置；查看并使用**所有**知识库；为任意知识库创建邀请与授权 |
| VIP | `vip` | 创建自己的知识库；使用自己创建或被邀请的知识库；**仅对自己创建的知识库**创建邀请与授权 |
| 普通用户 | `user` | 创建自己的知识库；使用自己创建或被邀请的知识库；不能邀请或授权他人 |

- 只有管理员可以在「用户管理」页面修改角色，接口为 `PATCH /v1/admin/users/{user_id}`，`role` 只接受 `admin`、`vip`、`user`。
- 现阶段没有「普通用户自助升级为 VIP」的入口；如需 VIP，请联系管理员调整。
- 系统始终保留至少一名启用状态的管理员，管理员不能被自己降级或删除为 0 人。

## 2. 知识库归属

- 每个知识库（namespace）记录创建者，**默认只有创建者本人和管理员可以使用**。
- 所有已登录用户都可以创建知识库：
  - 管理员使用「创建知识库」的完整表单，自行配置 Embedding / Reranker / VLM 服务与切分参数。
  - VIP 和普通用户使用简化表单，只需要填写知识库标识与切分参数；模型服务配置自动沿用管理员配置的模板知识库。
  - 模板知识库由环境变量 `RAG_DEFAULT_KB_TEMPLATE` 指定，默认是 `default`。若管理员还没有创建该模板，普通用户创建知识库会收到提示：`管理员尚未配置默认知识库模板，请联系管理员创建知识库`。
- 历史数据（本功能上线前创建的知识库）没有记录创建者，只有管理员可以查看和授权；管理员可以通过邀请把权限授予其他用户。
- 知识库切换器（控制台左上角）只会列出当前账号有权使用的知识库：管理员看到全部，其他账号看到自己创建的以及被邀请授权的。

## 3. 权限模型

权限由两个维度组成：

- **权限级别**
  - `read` 可读：检索、查看文档列表、预览/下载文档内容、读取检索结果与附件。
  - `write` 可修改：在可读基础上，还可以上传/替换、删除文档。
- **权限范围**
  - `all` 全库：知识库中的全部文档，包括之后新增的文档。
  - `files` 多文件：明确列出的若干文档。
  - 单文件就是 `files` 范围下只选一个文档。

一个账号可以在同一个知识库上拥有多条不同范围的授权，实际权限取并集；`write` 隐含 `read`。授权记录存储为「(知识库, 被授权账号, 文档名或全库) → 权限」，重复授权会更新而不是叠加重复行。

管理员或知识库创建者天然拥有全库读写权限，无需授权。

## 4. 邀请流程

1. 知识库创建者（VIP 或管理员；管理员可对任意知识库操作）在「权限管理」页选择知识库，点击「邀请用户」。
2. 填写被邀请人（只能选择已存在且启用的账号）、权限级别、权限范围（全库或选择若干文件）。
3. 系统生成一条邀请：
   - 被邀请人会收到一条站内邀请消息（控制台顶部铃铛中的待处理邀请数量）；
   - 邀请同时生成一个邀请链接（形如 `<origin>/console#/invite?token=...`），可以复制发给对方。
   - 数据库只保存链接 token 的 SHA-256 摘要，链接本身只在创建时展示一次。
4. 被邀请人打开链接，**使用被邀请的账号登录**，确认知识库、邀请人、权限与范围后点击「接受」：
   - 接受后权限立即生效，且不可撤销地写入授权记录；
   - 拒绝或忽略过期链接都不会获得任何权限；
   - 邀请链接默认 168 小时内有效（`RAG_INVITATION_TTL_HOURS`），过期后需要重新邀请。
5. 邀请人/知识库创建者/管理员可以撤销尚未接受的邀请；已接受的权限可以通过「权限管理」页的授权列表撤销。

> 邀请只能由被邀请的账号接受：链接被转发给其他账号时无法接受。同一账号在同一知识库重新被邀请时，旧的未处理邀请会自动作废，只保留最新一条。

## 5. 只读用户看到什么

- 知识库列表、文档列表只显示被授权的文档；
- 检索只会在被授权的文档范围内进行；
- 「知识文档」页面隐藏上传、替换与删除入口，并显示「只读」标记。

## 6. 环境变量

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `RAG_INVITATION_TTL_HOURS` | `168` | 邀请链接有效期（小时） |
| `RAG_DEFAULT_KB_TEMPLATE` | `default` | 普通用户/VIP 创建知识库时克隆模型配置的模板知识库 |

## 7. HTTP 接口

所有错误使用 `{"detail": "..."}`；带 `*` 的接口返回 `Cache-Control: no-store`。

### 7.1 我可以用哪些知识库

```text
GET /v1/namespaces                       # 当前账号可用的知识库列表
GET /v1/knowledge-bases/{namespace}/access
```

`access` 返回：

```json
{
  "namespace": "team-kb",
  "owner": false,
  "can_manage": false,
  "can_share": false,
  "role": "user",
  "can_read": true,
  "can_write": true,
  "scope": {
    "whole_read": false,
    "whole_write": false,
    "read_documents": ["spec.md"],
    "write_documents": ["spec.md"]
  }
}
```

### 7.2 创建知识库（VIP / 普通用户）

```text
POST /v1/knowledge-bases
{"namespace": "my-kb", "strategy": "character", "chunk_size": 800, "chunk_overlap": 100}
```

非管理员提交的模型服务字段会被忽略，统一从模板知识库克隆。

### 7.3 权限管理（创建者 / 管理员）

```text
GET    /v1/users                                      # 邀请候选人（仅 VIP/管理员）
GET    /v1/knowledge-bases/{namespace}/permissions    # 授权列表
DELETE /v1/knowledge-bases/{namespace}/permissions/{user_id}
GET    /v1/knowledge-bases/{namespace}/invitations    # 该知识库的邀请
POST   /v1/knowledge-bases/{namespace}/invitations    # 创建邀请
DELETE /v1/invitations/{invitation_id}                # 撤销未接受的邀请
```

创建邀请请求：

```json
{"username": "dave", "permission": "read", "scope": "files", "document_names": ["spec.md", "guide.pdf"]}
```

响应（`201`）：

```json
{
  "invitation": {"id": "...", "namespace": "team-kb", "inviter_username": "bob",
    "invitee_username": "dave", "permission": "read", "scope": "files",
    "document_names": ["spec.md", "guide.pdf"], "status": "pending"},
  "invite_path": "/console#/invite?token=<只在本次响应中出现>"
}
```

### 7.4 被邀请人

```text
GET  /v1/invitations                    # 我的邀请消息（pending 优先）
POST /v1/invitations/preview            # {"token": "..."}
POST /v1/invitations/accept             # {"token": "..."}
POST /v1/invitations/decline            # {"token": "..."}
```

状态值：`pending`、`accepted`、`declined`、`revoked`、`expired`（过期由有效期计算，不落库）。

## 8. 访问控制规则

- 会话账号访问租户 API 时，服务端会按知识库归属与授权计算可见文档集合：
  - 只读接口（检索、文档列表/预览、检索结果、附件、上传状态）需要至少一个可读范围；
  - 上传、替换、删除文档需要对应文件的 `write` 权限（全库写可以上传新文件；文件级写只能替换已授权的文件）；
  - `GET /v1/documents` 只返回可见文档；受限账号的检索结果也会在生成检索快照前按可见文档过滤。
- 管理凭证（管理员签发的 API Key、`RAG_SERVICE_TOKEN`、本机无会话直连）沿用原有语义：不做知识库归属校验，API Key 自身的 namespace 白名单仍然生效。
- MCP（`/mcp`）使用同一套可见性：工具与资源只暴露调用方可见的文档；写工具要求对应文档的写权限；受限账号不支持异步（queued）检索。

## 9. 已知限制

- 邀请消息是**站内消息**（控制台通知铃铛 + 待处理邀请列表），当前不发送邮件或第三方 IM 消息；邀请链接需要邀请人复制后通过其它渠道转达，或由被邀请人登录后在通知中心直接处理。
- 文件级授权在文档被删除后自然失效；重新上传同名文件会继续沿用原授权。
- 历史知识库（无创建者）不会自动分配给任何普通用户，需要管理员显式授权。
