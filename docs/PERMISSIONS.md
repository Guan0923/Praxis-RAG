# 角色、知识库归属与权限管理

本文描述 Praxis-RAG 的账号角色、知识库归属隔离、基于邀请链接的知识库共享权限，以及基于用户组的共享权限。

## 1. 角色

| 角色 | `role` 值 | 能力 |
| --- | --- | --- |
| 管理员 | `admin` | 管理用户与角色、全部 API Key、知识库配置；查看并使用**所有**知识库；为任意知识库创建邀请与授权；管理任意账号的 API Key |
| VIP | `vip` | 创建自己的知识库；使用自己创建或被邀请的知识库；**仅对自己创建的知识库**创建邀请与授权（手工 VIP 保留此能力）；创建并管理自己的 API Key（只能绑定本人知识库） |
| 普通用户 | `user` | 创建自己的知识库；使用自己创建或被邀请的知识库；默认不能邀请，但当前 Plus/Pro 会员可对自己拥有的知识库发邀请；创建并管理自己的 API Key（只能绑定本人知识库） |

- 只有管理员可以在「用户管理」页面修改角色，接口为 `PATCH /v1/admin/users/{user_id}`，`role` 只接受 `admin`、`vip`、`user`。
- 现阶段没有「普通用户自助升级为 VIP」的入口；如需 VIP，请联系管理员调整。个人会员（Free/Plus/Pro）与角色相互独立：购买、续费和到期只改会员权益，不改 `role`。
- 系统始终保留至少一名启用状态的管理员，管理员不能被自己降级或删除为 0 人。

## 2. 用户组

用户组是第一版的共享单位：**已启用且角色为 `vip` 或 `admin` 的账号可以创建组并邀请成员**；一个账号可以加入多个组，一个组可以被授权访问多个知识库（每个库可以设置不同权限）。

| 操作 | 谁可以执行 |
| --- | --- |
| 创建组 | 已启用且 `role IN ('vip','admin')` 的账号 |
| 邀请成员 | 仍为 `vip` 或 `admin` 的组创建者 |
| 接受或拒绝邀请 | 邀请绑定的已启用账号（`user`/`vip`/`admin` 均可） |
| 查看成员名单、移除成员、撤销邀请、解散组 | 组创建者 |
| 退出组 | 成员本人 |
| 给组设置知识库权限 | 仍为 `vip` 或 `admin` 的组创建者，可以为自己拥有的知识库设置（管理员可为任意库设置） |
| 撤销已有组授权 | 组创建者（或知识库所有者/管理员），可以清理自己拥有的知识库中的组授权 |

- 创建者不需要是组成员；创建者从 `vip`/`admin` 变为 `user` 后，已有成员和授权继续生效，但失去创建组、发邀请、新增或扩大组授权的权限，仍可以移除成员、撤销授权和解散组。接受邀请时会在事务内重新校验邀请人的当前角色。
- 组邀请与知识库邀请使用同一套链接规则：令牌只存 SHA-256 摘要、默认 168 小时有效（`RAG_INVITATION_TTL_HOURS`）、只能由目标账号接受、重新邀请会作废旧链接。接受与撤销并发时只有一个状态转换成功；重复接受已处理的邀请保持幂等，**通过旧链接重试不会把已退出或被移除的成员加回来**。
- 解散组让组立即失效并撤销待接受邀请，同时清除全部成员关系与组授权；删除账号、删除知识库也会级联清理相关组数据，避免残留记录重新生效。
- 第一版由创建者管理组，暂缓组长转让、嵌套组和跨所有者共享组。

## 3. 用户组权限位与合并

组授权使用文档权限位掩码：`read=0001`、`create=0010`、`update=0100`、`delete=1000`。组权限支持全库或指定文件；`create` 只能设置在全库授权上。

对当前知识库或目标文件，服务端读取个人规则和所有**当前有效成员关系**对应的组规则，先合并允许位，再清除拒绝位：

```go
allow := roleAllow | personalAllow | groupAllow
deny := roleDeny | personalDeny
effective := allow &^ deny
```

- `groupAllow` 是所有适用组规则的按位或。退出组、被移除、撤销组授权或解散组后，后续请求重新计算权限：该组提供的权限消失，其他有效来源继续参与计算。
- 第一版组规则只支持 `allow`。个人全库与文件级规则中的 `deny` 都参与最终计算。以后如需组级拒绝，再明确「加入组可能减少用户已有权限」的产品语义。

| 场景 | 个人允许 | 组允许 | 个人拒绝 | 有效权限 |
| --- | ---: | ---: | ---: | ---: |
| 个人可修改，组允许新增和修改 | `0100` | `0110` | `0000` | `0110` |
| 同上，但个人被明确禁止新增 | `0100` | `0110` | `0010` | `0100` |
| 两个组分别允许阅读、修改 | `0000` | `0001 \| 0100` | `0000` | `0101` |

权限详情页展示每个权限位的来源（「阅读来自用户组「行业研究组」」「修改来自个人授权」「删除被个人规则拒绝」），帮助管理员排查为什么移除某个组后成员仍有其他访问权限。

## 4. 知识库归属

- 每个知识库（namespace）记录创建者，**默认只有创建者本人和管理员可以使用**。
- 所有已登录用户都可以创建知识库：
  - 管理员使用「创建知识库」的完整表单，可自行配置 Embedding / Reranker / VLM 服务与切分参数。
  - VIP 和普通用户在自己的「模型配置」页维护模型连接，建库时从**本人**配置中选择 Embedding / Reranker / VLM；不能使用他人的模型配置。
  - 管理员在未提交模型连接时可回退到 `RAG_DEFAULT_KB_TEMPLATE`（默认 `default`）模板知识库；普通账号必须选择自己的模型连接，缺失时返回提示：`请先在「模型配置」页添加并选择 Embedding 模型后重试`。
- 历史数据（本功能上线前创建的知识库）没有记录创建者，只有管理员可以查看和授权；管理员可以通过邀请把权限授予其他用户。
- 知识库切换器（控制台左上角）只会列出当前账号有权使用的知识库：管理员看到全部，其他账号看到自己创建的以及被邀请授权的。

## 5. 权限模型（个人授权）

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

## 6. 邀请流程（知识库共享）

1. 知识库创建者（VIP、管理员或当前 Plus/Pro 会员；管理员可对任意知识库操作）在「权限管理」页选择知识库，点击「邀请用户」。Plus/Pro 只能对自己拥有的知识库发邀请。
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
>
> 会员到期后已有共享保留、可撤销，但停止新发付费邀请；接受过期会员先前发出的未使用邀请时，服务端会重新校验邀请人的当前资格，资格失效则拒绝。手工 `vip` 角色保留原有邀请能力。

知识库所有者取 `knowledge_bases.owner_user_id`。共享给个人不限数量，不占共享者的自有名额；同一库的协作者共用该库所有者的文件上限和 Embedding 余额。共享者可看到该库文件用量、上限及能否继续上传和原因，详细余额与账单仅所有者可见。详见 [个人会员与额度](PERSONAL_MEMBERSHIP.md)。

## 7. 只读用户看到什么

- 知识库列表、文档列表只显示被授权的文档；
- 检索只会在被授权的文档范围内进行；
- 「知识文档」页面隐藏上传、替换与删除入口，并显示「只读」标记。
- 仅有部分文件 `write` 权限的账号会看到「文件级写入」标记：只对被授权文档显示「替换文件」，仍不显示上传、删除与飞书导入（这些需要全库写权限）。
- 「从飞书导入」只有对该知识库具备整体写权限（`whole_write`）时才显示；导入只读取当前账号在飞书本来就能访问的文档。导入后的飞书文档副本就是知识库普通文档，检索可见性按本节规则计算，不继承飞书原文档的分享范围。

## 8. 环境变量

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `RAG_INVITATION_TTL_HOURS` | `168` | 邀请链接有效期（小时） |
| `RAG_DEFAULT_KB_TEMPLATE` | `default` | 管理员自建知识库未提交模型连接时使用的模板知识库；普通账号必须使用本人模型配置 |

## 9. HTTP 接口

所有错误使用 `{"detail": "..."}`；带 `*` 的接口返回 `Cache-Control: no-store`。

### 9.1 我可以用哪些知识库

```text
GET /v1/namespaces                       # 当前账号可用的知识库列表
GET /v1/knowledge-bases/{namespace}/access
```

`GET /v1/namespaces` 返回 `{"namespaces": [...], "owned_namespaces": [...]}`，响应带 `Cache-Control: no-store`：

- 登录会话：`namespaces` 是本人创建的与被邀请授权的知识库（管理员为全部）；`owned_namespaces` 是其中本人创建的子集（管理员同样只包含本人创建的）。
- API Key（`Authorization: Bearer <key>`；`X-RAG-Namespace` 可选且在此端点被忽略）：只返回该凭据绑定的知识库范围，`namespaces` 与 `owned_namespaces` 都是这一范围，不会返回凭据之外的知识库。
- `RAG_SERVICE_TOKEN` 与本机无会话直连：沿用管理凭证语义，返回全部知识库。
- 权限服务不可用或权限查询失败时返回 HTTP 500 与明确的 `detail`（`知识库权限服务异常` / `知识库权限服务未配置`），不会回退成全部知识库。
- 该端点不要求 `X-RAG-Namespace`：浏览器记住的 namespace 已失效、或 API Key 被改绑到其它知识库时，仍能拿到该凭据当前的授权范围，Console 据此自动纠正选择；其他 `/v1/*` 租户接口仍要求 header 属于凭据绑定范围（否则返回 HTTP 403 `API Key 未获授权访问此 Namespace`）。

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

### 9.2 创建知识库（VIP / 普通用户）

```text
POST /v1/knowledge-bases
{"namespace": "my-kb", "strategy": "character", "chunk_size": 800, "chunk_overlap": 100}
```

请求可携带从**本人模型配置**解析出的 `embedding` / `reranker` / `vlm` 连接字段；服务端校验连接可用性并保存副本，不会使用他人的模型配置。管理员未提交连接时可从默认模板克隆。

### 9.3 权限管理（创建者 / 管理员）

```text
GET    /v1/users                                      # 邀请候选人（管理员/VIP/Plus/Pro）
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

### 9.4 被邀请人

```text
GET  /v1/invitations                    # 我的邀请消息（pending 优先）
POST /v1/invitations/preview            # {"token": "..."}
POST /v1/invitations/accept             # {"token": "..."}
POST /v1/invitations/decline            # {"token": "..."}
```

状态值：`pending`、`accepted`、`declined`、`revoked`、`expired`（过期由有效期计算，不落库）。

### 9.5 用户组管理

所有用户组接口要求登录会话；角色、组归属和知识库归属在服务端校验。

```text
GET    /v1/groups                                            # 自己创建与加入的组
POST   /v1/groups                                           # {"name": "行业研究组"}（vip/admin）
GET    /v1/groups/{group_id}                                # 组详情 + 成员 + 邀请（创建者/成员）
DELETE /v1/groups/{group_id}                                # 解散组（创建者）
POST   /v1/groups/{group_id}/invitations                    # {"username": "dave"}（创建者且仍为 vip/admin）
DELETE /v1/groups/{group_id}/members/{user_id}             # 移除成员（创建者）
DELETE /v1/groups/{group_id}/membership                     # 退出组（成员本人）
GET    /v1/group-invitations                                # 我的组邀请（pending 优先）
POST   /v1/group-invitations/preview                        # {"token": "..."}
POST   /v1/group-invitations/accept                         # {"token": "..."}
POST   /v1/group-invitations/decline                        # {"token": "..."}
DELETE /v1/group-invitations/{invitation_id}                # 撤销未接受的组邀请（创建者）
```

组邀请响应（`201`）携带 `invite_path: "/console#/group-invite?token=<只在本次响应中出现>"`。

### 9.6 组授权（知识库权限）

```text
GET    /v1/knowledge-bases/{namespace}/group-permissions             # 组授权列表（所有者/管理员）
PUT    /v1/knowledge-bases/{namespace}/group-permissions/{group_id}   # 写入/覆盖一条组授权
DELETE /v1/knowledge-bases/{namespace}/group-permissions/{group_id}  # 撤销该组的全部授权
GET    /v1/knowledge-bases/{namespace}/permission-sources            # 权限来源说明（可带 ?document_name=）
```

写入请求：

```json
{"document_names": ["spec.md"], "allow_mask": 5}
```

- `document_names` 为空数组表示全库；`allow_mask` 是 `read=1`、`create=2`、`update=4`、`delete=8` 的非空组合（1–15）。
- `create` 只能设置在全库授权上；指定文件时写入会拒绝 `create` 位。
- 写入方必须仍为 `vip`/`admin`、是组创建者、且为该知识库所有者（管理员可为任意库）；服务端同时校验目标知识库存在、指定文件在库内。
- `permission-sources` 返回 `{"sources": [{"bit": 1, "bit_name": "read", "kind": "group", "origin": "来自用户组「行业研究组」", "document_name": ""}]}`，`kind` 为 `role` / `personal` / `group`。

## 10. 访问控制规则

- 会话账号访问租户 API 时，服务端会按知识库归属与授权计算可见文档集合（个人授权 + 当前有效用户组合并）：
  - 只读接口（检索、文档列表/预览、检索结果、附件、上传状态）需要至少一个可读范围；
  - 附件（`GET /v1/artifacts/{artifact_id}` 与 `/content`）按其所属文档校验权限：附件元数据中的 `document_id` 解析出父文档名，在**同一个数据库事务**内完成「读取附件字节 + 按父文档名校验读权限 + 提交」——受限（文件级）授权只能读取自己可读文档的附件；父文档不存在或已删除则一律拒绝（防闭式失败，fail closed）。管理凭证与全库读权限沿用原行为不挂校验门。附件自身的 `name`（如 `document.md`、图片/表格名）不是权限主体；
  - 上传、替换、删除文档需要对应文件的 `write` 权限（全库写可以上传新文件；文件级写只能替换已授权的文件）；
  - `GET /v1/documents` 只返回可见文档；受限账号的检索结果也会在生成检索快照前按可见文档过滤。
- 管理凭证（API Key、`RAG_SERVICE_TOKEN`、本机无会话直连）沿用原有语义：不做知识库归属校验，API Key 自身的 namespace 白名单仍然生效。
- MCP（`/mcp`）使用同一套可见性：工具与资源只暴露调用方可见的文档；写工具要求对应文档的写权限；附件资源（`rag://artifacts/...`）与 REST 附件接口走同一事务的文档级校验；受限账号不支持异步（queued）检索。组权限在 `internal/access` 的统一鉴权入口加载，REST、MCP、知识库列表和检索复用合并后的权限；文件级组授权在召回前生效；长连接上的后续 MCP 调用与资源通知也按当前成员关系检查（会话重评估在每次请求时完成）。
- `GET /v1/namespaces` 的可见列表包含「至少存在一条组授权且本人当前是该组成员」的知识库。

## 10a. MySQL 表结构（用户组）

四张表随仓库现有的 schema 初始化自动创建；老库升级时由启动迁移补建，幂等且容忍并发启动。

| 表 | 主要字段和约束 |
| --- | --- |
| `user_groups` | `id`、`name`、`owner_user_id`、`status`（`active`/`dissolved`）、`created_at`、`updated_at`、`dissolved_at`；按 `owner_user_id` 建索引 |
| `user_group_members` | `id`、`group_id`、`user_id`、`joined_at`、`created_at`；唯一键 `(group_id, user_id)`，按 `user_id` 建索引 |
| `user_group_invitations` | `id`、`group_id`、`inviter_user_id`、`invitee_user_id`、`token_digest`（唯一）、`status`、`created_at`、`expires_at`、`accepted_at`、`updated_at`；按 `invitee_user_id`、`group_id` 建索引 |
| `kb_group_permissions` | `id`、`namespace`、`group_id`、`document_name`（`''` 表示全库）、`allow_mask`、`granted_by`、`created_at`、`updated_at`；唯一键 `(namespace, group_id, document_name)`，按 `group_id` 建索引 |

- 邀请令牌绑定目标账号，只存摘要（与 `kb_invitations`、`user_sessions` 一致）。接受时在一个 MySQL 事务内检查组仍有效、邀请仍为 `pending` 且未过期、邀请人仍有资格，再写入成员并更新邀请状态。
- 解散组在一个事务内让组失效、撤销待接受邀请、删除成员与全部组授权。删除账号级联清理其拥有的组（含成员/邀请/授权）、其成员关系与其收发的邀请；删除知识库的清库流程同步删除相关组授权。

## 11. API Key 归属隔离

管理凭证（API Key）与知识库归属对齐，按创建者账号隔离：

- **归属**：每个 API Key 记录创建它的账号（`api_keys.owner_user_id`）。密钥列表条目新增可选字段 `owner_user_id` 与 `owner_username`；`owner_username` 仅用于展示。历史密钥（本功能上线前创建，`owner_user_id` 为 `NULL`）不带这两个字段。
- **可见性**：
  - 管理员：看到全部密钥（含历史无主密钥），「所有者」列显示账号名，历史密钥显示「—（未绑定）」；可以管理任意密钥、为任意知识库创建密钥。
  - VIP / 普通用户：只看到自己创建的密钥；查看/修改/删除/导出其他账号的密钥返回 HTTP 404 `API Key 不存在`。
- **创建与管理**：任意登录角色都可以打开 Console 的「API Key」页管理自己的密钥。非管理员只能把密钥绑定到**本人拥有**的知识库（`knowledge_bases.owner_user_id` 等于本人）；绑定他人的知识库返回 HTTP 403，detail 为 `只能为本人创建的知识库创建 API Key`。管理员不受此限制。
- **可用知识库列表**：`GET /v1/namespaces` 增加附加字段 `owned_namespaces`：会话账号返回本人拥有的知识库（管理员返回的是管理员自己拥有的，而不是全部）；本机无会话模式等于全部。控制台 API Key 页用它限制普通账号可选的 Namespace。会话的 `namespaces` 始终只包含自有与获授权知识库；API Key 调用该接口时 `namespaces`/`owned_namespaces` 都收敛为密钥自身的授权范围。
- **本机 break-glass**：没有登录会话时，本机 loopback 直连仍可管理凭证（保留旧行为）：可以看到全部密钥，并创建无主密钥（`owner_user_id` 为空，之后只有管理员或本机管理能看到）。
- **账号删除级联**：删除账号时一并删除该账号拥有的 API Key，正在使用这些密钥的应用立即失效。
- **账号禁用与角色降级**：禁用账号会吊销登录会话，但不会自动停用其已签发的 API Key；如需立即吊销请删除账号（级联删除），或由管理员/本机删除、停用相关密钥。管理员被降级为普通账号后，其在管理员任期内签发的密钥仍然有效（含绑定他人知识库的密钥）；这类密钥可以改名、停用或删除，但更新时只校验**新增**的 Namespace，不能再把密钥扩大到非本人知识库。
- **兼容性**：五条管理路由路径保持不变——`GET/POST /v1/admin/api-keys`、`PUT/DELETE /v1/admin/api-keys/{key_id}`、`GET /v1/admin/api-keys/{key_id}/mcp-config`；但鉴权不再是"仅本机或管理员"：任意角色的登录会话都可管理自己的密钥（Cookie 写操作需 `X-CSRF-Token`），无会话时仅接受可信本机管理。这些响应均为 `Cache-Control: no-store`。
- **租户认证不变（唯一例外：知识库列表）**：API Key 用于 `/v1/*` 与 `/mcp` 时仍按 `Authorization: Bearer <key>` + `X-RAG-Namespace` 校验，密钥自身的 namespace 白名单照旧生效，请求时不再做额外的知识库归属校验。`GET /v1/namespaces` 例外：它只校验凭据本身，不要求也不校验 `X-RAG-Namespace`，这样改绑或轮换后的密钥仍能读出自己的授权范围（详见第 7.1 节）。

## 12. 已知限制

- 邀请消息是**站内消息**（控制台通知铃铛 + 待处理邀请列表），当前不发送邮件或第三方 IM 消息；邀请链接需要邀请人复制后通过其它渠道转达，或由被邀请人登录后在通知中心直接处理。组邀请同样只做站内通知与链接转达。
- 文件级授权在文档被删除后自然失效；重新上传同名文件会继续沿用原授权。组授权的文件级规则同理：删除后重新上传同名文件会继续沿用原组授权。
- API Key 的 namespace 绑定保存的是知识库名称，租户请求不再复核知识库归属：删除某个知识库后若有人重建同名知识库，此前为该 namespace 签发的密钥仍会对其生效。删除/重建知识库时应同步删除相关 API Key（后续可考虑在租户认证时校验密钥归属与知识库归属一致）。
- 历史知识库（无创建者）不会自动分配给任何普通用户，需要管理员显式授权。
- 用户组第一版没有组长转让、嵌套组、跨所有者共享组和组级 `deny`；组规则只有允许位，与个人授权取并集。组邀请的消息铃铛与知识库邀请共用同一入口（「用户组」页与粘贴链接处理均可用）。
