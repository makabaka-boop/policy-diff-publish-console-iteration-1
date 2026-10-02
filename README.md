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
- **决策矩阵**：可点击查看草稿/已发布版本每个元组的完整证据链。

## 目录结构

```
policy/   模型校验、无环检查、继承闭包、裁决引擎、有限域穷举与版本对比
store/    草稿/已发布双修订存储、预览、带 CAS 的发布
api/      JSON HTTP API 与内置菱形继承演示数据
web/      Vue 3 + Vite 单页（构建产物嵌入 Go 二进制）
main.go   HTTP 服务（:8080），嵌入 web/dist 并做 SPA fallback
```

## HTTP API

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/api/state` | 草稿/已发布文档与修订号（含模拟用途声明） |
| PUT | `/api/draft` | 保存草稿（校验 + 草稿修订 +1，旧预览立即作废） |
| POST | `/api/preview` | 穷举草稿×已发布，返回带指纹的对比摘要 |
| POST | `/api/publish` | 携带 `{draftRevision, publishedRevision, summary}` 发布 |
| GET | `/api/decisions/{draft\|published}` | 某版本全量元组裁决与证据 |
| POST | `/api/demo/reset` | 重置为内置演示策略 |

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
