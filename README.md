# 有限域访问策略模拟器（accesssim）

**这只是一个策略推演/教学模拟器，不是任何真实系统的鉴权入口、SDK 或可接入的
授权组件。** 所有判定仅发生在页面枚举的、显式受限的有限集合内（最多
8 角色 × 8 资源类别 × 6 操作 = 384 个元组），数据保存在进程内存中，
重启即还原为内置演示策略。

## 功能

- **角色继承 DAG**：至多 8 个角色，可有向继承、必须无环（后端 DFS 着色校验，
  含自环/重复边/悬空父节点检查）。规则选择某祖先角色时，其全部后代角色命中。
- **规则匹配**：按 `角色 / 资源类别 / 操作` 三元组匹配，三者均支持 `*` 通配；
  整数优先级；`allow` / `deny` 效果。
- **裁决语义**：
  1. 收集命中的全部规则；
  2. 仅最高命中优先级的规则组参与裁决；
  3. 该组只有一种效果 → 取该效果；allow 与 deny 同优先级并存 → **deny 胜出**
     （证据标记 `tie-deny`，两条规则都列入 winners）；
  4. 无任何规则命中 → **默认 deny**（`no-match-default-deny`）。
- **穷举预览**：保存草稿后可生成预览，后端枚举全部元组，与已发布版本逐元组
  对比，列出「新增允许」「新增拒绝」，每条都带发布版/草稿两侧的命中规则证据
  （winners 与完整 considered 链）。
- **发布闸门（防混合版本）**：发布请求必须同时携带
  `draftRevision`、`publishedRevision` 与完整预览 `summary`。服务端在互斥锁内：
  1. 校验摘要 SHA-256 指纹（修订号也在指纹内，篡改任何字段都失效）；
  2. 比对当前草稿修订——草稿在预览后被改过 → `409 stale_draft_preview`；
  3. 比对当前已发布修订——别的客户端抢先发布 → `409 published_moved`；
  4. 用当前两份文档重新穷举，与提交摘要比对（哈希），杜绝「修订号对得上但
     内容不是预览过的内容」。任一条件不满足即拒绝，无法发布未经预览的混合版本。
- **模拟应急例外（break-glass，仅已发布）**：演练中可对一个「当前确为
  published deny」的**精确**（角色—资源—操作）元组（不接受通配、草稿域或
  已 allow 元组）建立带**明确理由**、**1–60 分钟**期限的临时放行：
  - 创建时在存储层互斥锁内对**当前已发布修订**重新裁决并把例外**钉住**该
    修订号；生效期间已发布决策返回临时 `allow`，证据 `reason` 为
    `emergency-exception-allow`，同时保留**原规则 deny 证据**（`considered`/
    `winners` 原样、`baseEvidence` 为完整原裁决）和**例外身份**（id、绑定
    修订、理由、到期时间）。页面因此不可能出现「矩阵已放行而证据仍是纯规则
    deny」的混合状态。
  - **草稿决策与草稿×已发布对比预览永远按纯策略计算**，例外完全不参与，
    学员不会把临时放行误读为草稿规则已经发布。
  - 到期判断为**惰性**：无后台任务，每次创建/列举/读取都在同一把锁内按注入
    时钟裁剪（到期时刻为排他边界，`now == expiresAt` 即失效），到期后读取
    立即恢复原裁决。
  - **发布新策略**（锁内换版同时清空全部例外）或**重置演示数据**（原子替换
    整个存储）会让旧例外**立即失效**；迟到的创建请求只能针对**新发布修订**
    重新裁决——新修订仍 deny 才会建立（并绑定新修订、取得新身份），新修订
    已 allow 则整次拒绝，无法附着到旧修订。
- **决策矩阵**：可点击查看草稿/已发布版本每个元组的完整证据链；被应急例外
  临时放行的单元格带 `!` 标记，证据框同时展示例外身份与原发布 deny 裁决。

## 目录结构

```
policy/   模型校验、无环检查、继承闭包、裁决引擎、有限域穷举与版本对比
store/    草稿/已发布双修订存储、预览、带 CAS 的发布、注入时钟的应急例外与锁内共享版本裁决
api/      JSON HTTP API 与内置菱形继承演示数据
web/      Vue 3 + Vite 单页（构建产物嵌入 Go 二进制，含 vitest 页面组件测试）
main.go   HTTP 服务（:8080），嵌入 web/dist 并做 SPA fallback
```

## HTTP API

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/api/state` | 草稿/已发布文档与修订号（含模拟用途声明） |
| PUT | `/api/draft` | 保存草稿（校验 + 草稿修订 +1，旧预览立即作废） |
| POST | `/api/preview` | 穷举草稿×已发布，返回带指纹的对比摘要（纯策略对比，不叠加应急例外） |
| POST | `/api/publish` | 携带 `{draftRevision, publishedRevision, summary}` 发布；成功后旧应急例外立即全部失效 |
| GET | `/api/decisions/{draft\|published}` | 某版本全量元组裁决与证据；published 行会叠加生效中的例外，原证据保留在 `baseEvidence` |
| GET | `/api/exceptions` | 当前生效的应急例外及其绑定的已发布修订号（惰性清理过期项） |
| POST | `/api/exceptions` | `{role,resource,action,reason,ttlMinutes}` 建立临时放行；400 请求非法 / 422 元组不在已发布有限域或当前非 deny / 409 已有生效例外 |
| POST | `/api/demo/reset` | 重置为内置演示策略（原子替换存储，全部例外立即失效） |

## 构建与运行

```bash
make build      # 构建前端并编译嵌入静态资源的单一二进制
make test       # go test -race ./... 与前端 vitest 页面测试
./accesssim     # 打开 http://localhost:8080

# 仅后端 / 仅前端测试
go test -race ./...
(cd web && npm install && npm test)

# 开发模式（两个终端）：
(cd web && npm install && npm run dev)   # Vite :5173，/api 代理到 :8080
go run .                                  # 后端 :8080
```

## 测试覆盖

`go test -race ./...` 与 `cd web && npm test` 覆盖：

**原有四类场景：**

1. **角色继承菱形**（`policy/engine_test.go::TestDiamondInheritance`）：
   菱形两条路径都能继承到祖先规则、规则不重复计数、侧向角色不串权。
2. **同优先级冲突**（`TestSamePriorityConflictDenyWins`）：
   同优先级 allow/deny → deny，两条规则都在 winners 证据里；另含高优先级
   双向覆盖、通配符、默认 deny、环与上限校验。
3. **规则编辑后的过期预览**（`store/store_test.go::TestStalePreviewAfterRuleEdit`）：
   预览后再改草稿 → `ErrDraftConflict`；篡改摘要修订号 → 指纹失效；
   重新预览后发布成功。HTTP 层端到端用例见 `api/server_test.go`。
4. **两个客户端竞争发布**（`TestConcurrentPublishers`，含真实 HTTP 并发版
   `TestHTTPConcurrentPublishers`）：同一修订对的两个预览并发发布，恰好一个
   成功、另一个 `409 published_moved`，败者重新预览后可见新基线。

另有：移除 allow 产生新增拒绝及双方证据、跨版本域并集、非法文档 422、
摘要内容篡改 422 等用例。

**应急例外场景（`store/exception_test.go`、`api/exception_test.go`，
均使用注入式假时钟，不依赖真实睡眠）：**

5. **建立与证据一致性**（`TestExceptionCreateAndPublishedOverride`）：
   published 行翻为 allow 且同时携带例外身份与完整原 deny 证据
   （`baseEvidence` + 原 winners）；草稿行无任何例外字段、预览对比不出现
   该元组的新增允许。
6. **期限边界**（`TestExceptionTTLBoundary`，HTTP 版
   `TestHTTPExceptionExpiryBoundary`）：TTL 仅接受 1–60 分钟；到期时刻为
   排他边界（`expires-1ns` 仍生效、`now == expiresAt` 立即恢复原 deny），
   无后台任务；到期后同一元组可再次建立。
7. **无效元组整次拒绝**：空理由/超长理由、通配或空缺字段 → 400；元组不在
   已发布有限域 → 422 `tuple_outside_domain`；当前已 allow → 422
   `tuple_not_denied`；重复元组 → 409。所有拒绝都不留任何残留（
   `TestExceptionInvalidRequestValidation`、
   `TestExceptionTupleMustBeConcretePublishedDomainAndDenied`、
   `TestExceptionDuplicateTuple` 及对应 HTTP 用例）。
8. **发布竞争与迟到请求**
   （`TestExceptionPublishInvalidatesAndLateCreateCannotAttach`、
   `TestHTTPExceptionPublishRace`）：发布在锁内换版同时清空例外；迟到的
   创建请求对新修订重新裁决——新修订仍 deny 才建立并绑定新修订/新身份，
   新修订已 allow 则拒绝，无法附着旧修订。
9. **交错请求与失效清理**（`TestExceptionConcurrentCreateAndPublish`）：
   16 个创建协程与 4 个发布协程、列举协程在 `-race` 下交错；结束后不变量
   为「每个存活例外都绑定当前已发布修订、且该修订下元组纯裁决确为 deny、
   行证据 allow 与 `baseEvidence`/身份三者齐全」，id 不重复。
10. **重置立即失效**（`TestExceptionResetClearsEverything`、
    `TestHTTPExceptionResetInvalidates`，后者还验证重置后的新存储仍沿用
    注入时钟）。
11. **页面组件测试**（`web/src/components/__tests__/`，vitest + happy-dom）：
    矩阵单元格在临时放行时同时渲染例外标记与证据框（例外身份 + 原 deny
    裁决 + 原规则链），杜绝「放行单元格配纯 deny 证据」的混合展示；到期后
    bump 重取原子翻回 deny；面板校验空理由、提交载荷、422 整次错误展示、
    以及发布后旧修订例外不在面板出现。
