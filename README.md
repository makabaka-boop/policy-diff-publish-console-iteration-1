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
  （winners 与完整 considered 链）。应急例外不进入预览：草稿对比始终按纯规则计算。
- **模拟应急例外（临时放行，不是发布）**：管理员可对**当前已发布策略确实
  deny 的一个精确元组**建立 1–60 分钟、必须附理由的应急例外。
  - 创建时在存储层互斥锁内对**当前已发布有限域重新裁决**：元组必须属于
    已发布有限域且当前决策为 deny，否则整次请求拒绝、不留任何半状态；
    请求必须携带所见的 `publishedRevision`，迟到请求（修订已被发布/重置推进）
    返回 `409 published_moved`，无法附着到新修订。
  - 生效期间，已发布矩阵该元组返回临时 allow，证据同时携带**原规则 deny
    证据链**（considered/winners/原 reason）与**例外身份 id**（reason 为
    `emergency-exception-allow`）；草稿决策、穷举预览完全不受影响。
  - 到期判断为**惰性共享裁决**：创建、列举、矩阵枚举在同一把锁内先清理过期
    例外再读取，无需任何后台任务，到期瞬间下次读取即恢复原裁决。
  - **发布新策略或重置演示数据时，在同一临界区立即清空全部例外**；例外 id
    全局单调不复用。矩阵颜色、证据、例外身份来自同一次裁决的同一行响应，
    页面不可能出现「矩阵已放行、证据仍显示纯规则 deny」的混合状态。
- **发布闸门（防混合版本）**：发布请求必须同时携带
  `draftRevision`、`publishedRevision` 与完整预览 `summary`。服务端在互斥锁内：
  1. 校验摘要 SHA-256 指纹（修订号也在指纹内，篡改任何字段都失效）；
  2. 比对当前草稿修订——草稿在预览后被改过 → `409 stale_draft_preview`；
  3. 比对当前已发布修订——别的客户端抢先发布 → `409 published_moved`；
  4. 用当前两份文档重新穷举，与提交摘要比对（哈希），杜绝「修订号对得上但
     内容不是预览过的内容」。任一条件不满足即拒绝，无法发布未经预览的混合版本。
- **决策矩阵**：可点击查看草稿/已发布版本每个元组的完整证据链。

## 目录结构

```
policy/   模型校验、无环检查、继承闭包、裁决引擎、有限域穷举与版本对比
store/    草稿/已发布双修订存储、预览、带 CAS 的发布、应急例外（注入时钟、惰性到期）
api/      JSON HTTP API 与内置菱形继承演示数据
web/      Vue 3 + Vite 单页（构建产物嵌入 Go 二进制；vitest 页面测试）
main.go   HTTP 服务（:8080），嵌入 web/dist 并做 SPA fallback
```

## HTTP API

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/api/state` | 草稿/已发布文档与修订号（含模拟用途声明） |
| PUT | `/api/draft` | 保存草稿（校验 + 草稿修订 +1，旧预览立即作废） |
| POST | `/api/preview` | 穷举草稿×已发布，返回带指纹的对比摘要 |
| POST | `/api/publish` | 携带 `{draftRevision, publishedRevision, summary}` 发布 |
| GET | `/api/decisions/{draft\|published}` | 某版本全量元组裁决与证据（published 叠加生效例外） |
| GET | `/api/exceptions` | 当前生效例外列表 + 服务端裁决时钟 |
| POST | `/api/exceptions` | 携带 `{tuple, reason, ttlMinutes(1–60), publishedRevision}` 建立应急例外 |
| POST | `/api/demo/reset` | 重置为内置演示策略（立即清空全部例外） |

## 构建与运行

```bash
make build      # 构建前端并编译嵌入静态资源的单一二进制
make test       # go test -race ./...
./accesssim     # 打开 http://localhost:8080

# 开发模式（两个终端）：
(cd web && npm install && npm run dev)   # Vite :5173，/api 代理到 :8080
go run .                                  # 后端 :8080
```

## 测试覆盖

`go test -race ./...` 覆盖需求中点名的四类场景：

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

应急例外（`store/exception_test.go` 与 `api/exception_test.go`）使用
**注入时钟**（`store.NewWithClock`）覆盖：

1. **期限边界**：TTL 只接受 1–60 分钟；到期前 1ns 仍放行，到达期瞬间
   惰性清理、恢复原 deny 证据，无后台任务；到期后可重新建立且 id 不复用。
2. **发布竞争/失效清理**：发布后旧例外在同一临界区消失（即使时间未到）；
   钉在旧 `publishedRevision` 的迟到创建返回 `409 published_moved`，
   不能附着到新修订；对新修订重新裁决才能建立；重置同样立即清空。
3. **无效元组整次拒绝**：域外元组、缺角色/资源/操作、空理由、缺修订号、
   当前其实 allow 的元组，全部拒绝且不产生任何部分状态；重复建立被拒。
4. **交错请求无混合状态**：存储层与真实 HTTP 层都用并发创建 × 发布 ×
   矩阵读取压测（`-race`），断言「有 exceptionId ⇔ 有 exception 对象 ⇔
   当前结论为临时 allow 且绑定当前已发布修订」恒成立。
5. **草稿/预览不被污染**：例外生效期间草稿矩阵仍按规则 deny，预览 diff
   不出现该元组。

页面测试（`web/test/`，`cd web && npm test`，vitest + happy-dom）覆盖矩阵
单元放行与原 deny 证据同框展示、创建表单仅出现在已发布 deny 格、TTL 边界
本地拦截、`published_moved`/`tuple_not_denied` 错误不乐观放行、发布/重置后
例外即时清空、倒计时到期后刷新恢复原裁决。
