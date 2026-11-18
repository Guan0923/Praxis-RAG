# 角色、知识库归属与权限管理

本文描述 Praxis-RAG 的账号角色、知识库归属隔离，以及基于邀请链接与操作位掩码的知识库共享权限。

## 1. 角色

| 角色 | `role` 值 | 能力 |
| --- | --- | --- |
| 管理员 | `admin` | 管理用户与角色、全部 API Key、知识库配置；查看并使用**所有**知识库；为任意知识库创建邀请与授权；管理任意账号的 API Key |
| VIP | `vip` | 创建自己的知识库；使用自己创建或被邀请的知识库；**仅对自己创建的知识库**创建邀请与授权（手工 VIP 保留此能力）；创建并管理自己的 API Key（只能绑定本人知识库） |
| 普通用户 | `user` | 创建自己的知识库；使用自己创建或被邀请的知识库；默认不能邀请，但当前 Plus/Pro 会员可对自己拥有的知识库发邀请；创建并管理自己的 API Key（只能绑定本人知识库） |

- 只有管理员可以在「用户管理」页面修改角色，接口为 `PATCH /v1/admin/users/{user_id}`，`role` 只接受 `admin`、`vip`、`user`。
- 现阶段没有「普通用户自助升级为 VIP」的入口；如需 VIP，请联系管理员调整。个人会员（Free/Plus/Pro）与角色相互独立：购买、续费和到期只改会员权益，不改 `role`。
- 系统始终保留至少一名启用状态的管理员，管理员不能被自己降级或删除为 0 人。

## 2. 知识库归属

- 每个知识库（namespace）记录创建者，**默认只有创建者本人和管理员可以使用**。
- 所有已登录用户都可以创建知识库：
  - 管理员使用「创建知识库」的完整表单，可自行配置 Embedding / Reranker / VLM 服务与切分参数。
  - VIP 和普通用户在自己的「模型配置」页维护模型连接，建库时从**本人**配置中选择 Embedding / Reranker / VLM；不能使用他人的模型配置。
  - 管理员在未提交模型连接时可回退到 `RAG_DEFAULT_KB_TEMPLATE`（默认 `default`）模板知识库；普通账号必须选择自己的模型连接，缺失时返回提示：`请先在「模型配置」页添加并选择 Embedding 模型后重试`。
- 历史数据（本功能上线前创建的知识库）没有记录创建者，只有管理员可以查看和授权；管理员可以通过邀请把权限授予其他用户。
- 知识库切换器（控制台左上角）只会列出当前账号有权使用的知识库：管理员看到全部，其他账号看到自己创建的以及被邀请授权的。

## 3. 权限模型

权限按**操作**授予与拒绝，共四项，每项对应一个二进制位：

| 操作 | 位值 | 二进制 | 覆盖的入口 |
| --- | --- | --- | --- |
| `read` | 1 | `0001` | 检索、文档列表/预览/原件、检索结果与附件 |
| `create` | 2 | `0010` | 新增文档：批量上传、MCP 新增、飞书导入 |
| `update` | 4 | `0100` | 覆盖/替换已有文档 |
| `delete` | 8 | `1000` | 删除文档 |

- **规则存储**：每条规则按「账号 × 知识库 × 文件名」存储，文件名为空表示全库（`all`）；明确列出的若干文档即多文件（`files`），单文件就是只选一个文档。每条规则同时保存 `allow_mask` 与 `deny_mask`（INT UNSIGNED，0..15 的四位掩码），重复授权会更新而不是叠加重复行。
- **三态语义**：对每一项操作，两个掩码都未置位 = 未设置；`allow_mask` 置位 = 允许；`deny_mask` 置位 = 拒绝。四项操作独立组合，`update` 不隐含 `read`（可以只授予替换而不授予阅读）。
- **合并公式**：`effective = (默认 allow | 全库 allow | 文件 allow) &^ (默认 deny | 全库 deny | 文件 deny)`；合并后没有获得允许的操作默认拒绝。
- **deny 优先**：任何适用规则的 deny 都优先于同一操作的 allow——全库拒绝可以压过单文件允许，单文件拒绝也可以限制全库允许。
- **角色默认值**：管理员默认 `allow=1111`、`deny=0000`；知识库所有者保留默认全库权限。两者同样参与 deny 计算：服务端不再有直接返回全权的短路分支，会读取显式规则，显式 deny 同样能约束管理员与所有者的会话。
- **create 的范围限制**：`create` 只使用角色默认值与知识库级（全库）规则；文件级规则不接受 create 位，写接口会校验未知位与越界值并拒绝。

旧接口的「可读 / 可修改」两级权限继续接受，转换规则见 §7.3 与 §10。

## 4. 邀请流程

1. 知识库创建者（VIP、管理员或当前 Plus/Pro 会员；管理员可对任意知识库操作）在「权限管理」页选择知识库，点击「邀请用户」。Plus/Pro 只能对自己拥有的知识库发邀请。
2. 填写被邀请人（只能选择已存在且启用的账号）、允许的操作（§3 的四项，`create` 只能随全库范围授予）、权限范围（全库或选择若干文件）。
3. 系统生成一条邀请：
   - 被邀请人会收到一条站内邀请消息（控制台顶部铃铛中的待处理邀请数量）；
   - 邀请同时生成一个邀请链接（形如 `<origin>/console#/invite?token=...`），可以复制发给对方。
   - 数据库只保存链接 token 的 SHA-256 摘要，链接本身只在创建时展示一次。
4. 被邀请人打开链接，**使用被邀请的账号登录**，确认知识库、邀请人、允许的操作与范围后点击「接受」：
   - 接受后写入 `allow` 掩码并立即生效；同一范围重复授权会覆盖原 allow，但保留已有 deny；
   - 状态变更与授权写入在 `AcceptPending` 的同一事务内完成；拒绝/撤销/过期只允许从 `pending` 状态转换，并发时只有赢得状态转换的一方生效；
   - 拒绝或忽略过期链接都不会获得任何权限；
   - 邀请链接默认 168 小时内有效（`RAG_INVITATION_TTL_HOURS`），过期后需要重新邀请。
5. 邀请人/知识库创建者/管理员可以撤销尚未接受的邀请；已接受的允许可以通过「权限管理」页撤销——旧撤销授权接口只清除 allow、不移除拒绝规则，清除拒绝需用 §7.3 的明确操作；也可以不经邀请，直接按「账号 + 全库或单文件」设置规则，设置 deny 立即生效、无需接收人接受邀请。

> 邀请只能由被邀请的账号接受：链接被转发给其他账号时无法接受。同一账号在同一知识库重新被邀请时，旧的未处理邀请会自动作废，只保留最新一条。
>
> 会员到期后已有共享保留、可撤销，但停止新发付费邀请；接受过期会员先前发出的未使用邀请时，服务端会重新校验邀请人的当前资格，资格失效则拒绝。手工 `vip` 角色保留原有邀请能力。

知识库所有者取 `knowledge_bases.owner_user_id`。共享给个人不限数量，不占共享者的自有名额；同一库的协作者共用该库所有者的文件上限和 Embedding 余额。共享者可看到该库文件用量、上限及能否继续上传和原因，详细余额与账单仅所有者可见。详见 [个人会员与额度](PERSONAL_MEMBERSHIP.md)。

## 5. 受限账号在 Console 看到什么

- 知识库列表、文档列表只显示具备有效 `read` 的文档；
- 检索只会在有效 `read` 的文档范围内进行（各检索模式的应用方式与 SAG 的明确拒绝见 §11）；
- 「权限管理」页（PermissionsPage）按操作三态设置：每项操作可在「未设置 / 允许 / 拒绝」之间切换；选择具体文件时只显示阅读/修改/删除三项（`create` 不适用于文件级规则）；
- 「邀请中心」（InvitationsPage）展示每条邀请具体允许的操作；
- 「知识文档」页（DocumentsPage）按有效 create/update/delete 控制按钮：具备 `create` 才显示上传与「从飞书导入」；对授权文件具备 `update` 才显示「替换文件」；具备 `delete` 才显示删除；没有任何写操作的账号显示「只读」标记；
- 「从飞书导入」只读取当前账号在飞书本来就能访问的文档。导入后的飞书文档副本就是知识库普通文档，检索可见性按本节规则计算，不继承飞书原文档的分享范围。

## 6. 环境变量

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `RAG_INVITATION_TTL_HOURS` | `168` | 邀请链接有效期（小时） |
| `RAG_DEFAULT_KB_TEMPLATE` | `default` | 管理员自建知识库未提交模型连接时使用的模板知识库；普通账号必须使用本人模型配置 |

## 7. HTTP 接口

所有错误使用 `{"detail": "..."}`；带 `*` 的接口返回 `Cache-Control: no-store`。

### 7.1 我可以用哪些知识库

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

旧版权限摘要（`can_read`/`can_write` 与 `scope`）按**保守口径**返回：带文件例外的权限不会被显示成全库可读写；精确掩码通过新增字段表达，字段级 schema 以服务端契约为准。

### 7.2 创建知识库（VIP / 普通用户）

```text
POST /v1/knowledge-bases
{"namespace": "my-kb", "strategy": "character", "chunk_size": 800, "chunk_overlap": 100}
```

请求可携带从**本人模型配置**解析出的 `embedding` / `reranker` / `vlm` 连接字段；服务端校验连接可用性并保存副本，不会使用他人的模型配置。管理员未提交连接时可从默认模板克隆。

### 7.3 权限管理（创建者 / 管理员）

```text
GET    /v1/users                                      # 邀请候选人（管理员/VIP/Plus/Pro）
GET    /v1/knowledge-bases/{namespace}/permissions    # 授权列表
DELETE /v1/knowledge-bases/{namespace}/permissions/{user_id}   # 旧撤销授权：只清除 allow，不移除拒绝规则
GET    /v1/knowledge-bases/{namespace}/invitations    # 该知识库的邀请
POST   /v1/knowledge-bases/{namespace}/invitations    # 创建邀请
DELETE /v1/invitations/{invitation_id}                # 撤销未接受的邀请
```

新增**直接规则管理**：按「账号 + 全库或单文件」用 PUT 语义在该范围上设置 allow 与 deny——设置 deny **立即生效，无需接收人接受邀请**；「撤销允许」与「清除拒绝」是两个明确操作，分别只动对应掩码。接口路径与请求字段以服务端契约为准，本文只描述行为。

旧 REST 的 `permission: "read"|"write"` 继续接受并按 §10 的表转换（`read→1`，全库 `write→15`，文件 `write→13`）；精确掩码通过新增字段表达。

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

## 8. 访问控制规则（操作入口表）

会话账号访问租户 API 时，服务端按 §3 的合并公式计算四项操作的有效掩码，再按入口逐一检查：

| 入口 | 检查 |
| --- | --- |
| 文档列表 / 预览 / 原件、REST 与 MCP 附件 | 按服务端解析出的目标文件检查 `read`；附件找不到可信所属文件时，对受账号 ACL 约束的请求直接拒绝 |
| 批量上传、MCP 新增、飞书导入 | 检查目标库 `create`；凡是允许覆盖已有文件的路径，对覆盖目标再检查 `update`（防止只有 create 的账号覆盖现有文件） |
| REST / MCP 替换 | 检查 `update` |
| REST / MCP 删除 | 检查 `delete` |
| 批量写入 | 在受理前完成各项检查 |
| 查重 / 上传历史 / 任务状态 | 沿用提交者可查本人任务；返回的文件元数据按目标文件 `read` 过滤 |
| 检索与 MCP 资源通知 | 在候选生成前应用有效 `read`；重排与 JEV 只能收到可读候选；显式空白名单直接返回空结果 |

- `GET /v1/documents` 只返回有效 `read` 的文档。
- 管理凭证：API Key 沿用自身 namespace 白名单，不套用账号 ACL（见 §9）；`RAG_SERVICE_TOKEN` 与本机无会话直连保持现有凭据语义不变，不做知识库归属校验。
- MCP（`/mcp`）使用同一套可见性：工具与资源只暴露调用方可见的文档；写工具按上表检查 `create`/`update`/`delete`；**MCP 工具名与输入 schema 不变**。SAG 模式对受限账号的明确拒绝见 §11。

## 9. API Key 归属隔离

**凭据语义**：账号上的 deny（乃至整套账号 ACL）约束**该账号的登录会话**；独立 API Key 仍按自身绑定的知识库范围授权，不套用账号 ACL；服务令牌（`RAG_SERVICE_TOKEN`）与本机直连保持现有凭据语义不变。

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

## 10. 历史数据迁移

升级把旧权限值转换为位掩码，转换**幂等**：重复升级不覆盖新掩码——迁移以「未迁移状态（NULL）」为回填标记，不以掩码 0 判断：

| 历史权限 | 新 allow_mask | 新 deny_mask |
| --- | --- | --- |
| `read` | `1`（`0001`） | `0` |
| 全库 `write` | `15`（`1111`） | `0` |
| 单文件 `write` | `13`（`1101`：read+update+delete） | `0` |

`kb_invitations` 增加 `allow_mask` 列，待接受邀请同样迁移：`read→1`、全库 `write→15`、文件 `write→13`。

## 11. 检索模式下的权限限制（已知限制）

- **Dense / Sparse / Hybrid / SAG2**：召回与扩展在 Qdrant 过滤 / 词法索引阶段就限定可读文档，重排与 JEV 只收到这些候选。
- **graph（LightRAG）**：通过 `filters.document_ids` 白名单限制，可用。
- **SAG（事件-实体模式）**：受限账号（任何非全库可读账号）调用会被**明确拒绝**——该模式的动态超边扩展无法在召回前可靠限定文档范围。这是明确的拒绝而非静默泄露。
- 全库 `read` + 文件级 deny 仍视为受限检索。

## 12. 验收场景（概述）

1. 管理员默认四项全开；给 `A.pdf` 显式拒绝删除后，仍能读、改 `A.pdf`，仍能删除 `B.pdf`。
2. 全库允许 + 文件拒绝、全库拒绝 + 文件允许都以拒绝为准；单独撤销 allow 后已有 deny 保持生效。
3. 只有 `create` 的账号能上传新文件，但不能覆盖同名已有文件；只有 `update` 的账号能替换授权文件，但不能删除。
4. 文件被拒绝阅读后，REST 和 MCP 都拿不到原件 / 附件 / 检索结果；被拒绝的片段不进入重排。
5. 旧库与待接受邀请正确迁移；重复升级不覆盖新掩码；接受邀请与撤销/拒绝并发时，只有赢得状态转换的一方生效。

## 13. 已知限制

- 邀请消息是**站内消息**（控制台通知铃铛 + 待处理邀请列表），当前不发送邮件或第三方 IM 消息；邀请链接需要邀请人复制后通过其它渠道转达，或由被邀请人登录后在通知中心直接处理。
- 文件级授权在文档被删除后自然失效；重新上传同名文件会继续沿用原授权。
- API Key 的 namespace 绑定保存的是知识库名称，租户请求不再复核知识库归属：删除某个知识库后若有人重建同名知识库，此前为该 namespace 签发的密钥仍会对其生效。删除/重建知识库时应同步删除相关 API Key（后续可考虑在租户认证时校验密钥归属与知识库归属一致）。
- 历史知识库（无创建者）不会自动分配给任何普通用户，需要管理员显式授权。
