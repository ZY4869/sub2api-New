---
iteration_id: "ITER-20261005-001-image-quota-reverse-isolation"
date: "2026-10-05"
status: "已实现但验证不完整"
task_level: "中大型跨层"
version_change: "NONE"
release_performed: false
---

# 迭代记录：生图额度反向隔离、主动暂停与按套餐生图统计/限额

## 1. 目标与范围

- 背景：OpenAI OAuth 账号此前只做到单向隔离。生图 429 只冷却 `openai:image_generation`，文本不受影响；反过来，普通额度（Codex 主池）耗尽会写账号级 `rate_limit_reset_at` 并在内存中封停整号，生图也随之停用。
- 用户决策：
  - 反向隔离默认开启，可配置关闭。
  - 5h/7d 普通额度阈值（账号/全局自动暂停与平台调度阈值）对原生生图放行。
  - 同时实现：后台状态提示；生图额度主动暂停；按套餐（Go=`go`、Plus=`plus`、Pro5x=`prolite`、Pro10x=`pro`、Pro25x=`promax`）的生图统计和手动限额。
  - 生图额度窗口很可能不是 5h/7d，一律不写死窗口。
- 计划：`C:\Users\admin\.claude\plans\floofy-greeting-snail.md`（用户已批准）。
- 基线：
  - 代码与环境：`main@34fcccea4`，工作区干净；Go 1.27.0（`GOTOOLCHAIN=auto` 使用缓存工具链）；pnpm 10.31.0；Node 24.9.0。
  - 依赖文件 SHA256：`go.mod` 002aea87…、`go.sum` 990626cf…、`package.json` 6f1fffab…、`pnpm-lock.yaml` 9382acbd…。
- 非目标：
  - 不改 API Key、SetupToken、Grok、Spark 影子账号的限流语义。
  - 不做平台对下游用户的独立生图额度。
  - 不新增数据库迁移，不改计费。
  - 不提交、推送、部署或发布。

## 2. 变更摘要

### Phase 1 反向隔离（后端核心）

- **主池封停识别**，两种来源都自校验，不依赖清理：
  - `handle429` 判定主池耗尽（5h/7d 窗口用满，或 body `usage_limit_reached`）时，用一条 UPDATE 同时写限流列与 extra 标记 `openai_main_pool_rate_limit{reset_at, reason, limited_at}`；标记与 `rate_limit_reset_at` 相差 ≤1s 才有效。
  - 平台调度阈值写入的临时不可调度，原因 JSON 为 `source=account_scheduling_threshold, platform=openai, window∈{5h,7d}, scope=""` 时视为主池暂停。
- **原生生图请求判定**：
  - handler 在 ctx 写入渠道映射后的转发模型。
  - 调度时用与转发共用的 `resolveOpenAIImagesOAuthRequestModel` 加 `GetMappedModel`，预测是否命中 `usesCodexDirectImages`。
- **调度关卡**：
  - 候选列表：生图请求补查 `ListModelAvailabilityCandidates`（5s 缓存 + singleflight，合并时深拷贝）。
  - 资格判定、IsSchedulable、运行时封停、粘性会话：均按请求放行。运行时封停对原生生图既不读也不清除账号级内存封停。
  - 自动暂停与调度阈值：对原生生图跳过。
  - 生图冷却（模型级）仍然生效。
- **Luna 路径 429 归属**：`forwardOpenAIImagesOAuth` 在上游 ctx 记录 direct/Luna。走 `/codex/responses` 且带主池耗尽信号的 429 改走账号级路径，并写主池标记。
- **测试恢复保护**：原生生图测试成功（管理端测试、定时测试 AutoRecover）时，账号若仅主池封停，跳过恢复。
- **配置开关**：`gateway.disable_openai_image_main_pool_isolation`，默认 false 即开启；在服务构造时发布为进程级开关。关闭后不写标记、不放行、不改归属。

### Phase 2 后台状态提示

- 账号状态：标记与限流截止时间一致时，显示“普通额度”，并提示原生生图仍可调度。
- 生图冷却提示显示原因：额度用尽、限速、主动暂停、套餐上限等。
- 已知原因清单抽为共享模块 `imageCooldownReasons.ts`。

### Phase 3 生图额度设置、主动暂停、观测

- **设置**：新键 `openai_image_quota_settings`（主动暂停阈值 0–100，默认 100；按套餐的滚动窗口规则）。
  - 管理接口：`GET/PUT /admin/settings/openai-image-quota`。
  - 热路径读取：进程内 30s 缓存，按 SettingService 实例区分，加 singleflight；保存后立即刷新。
- **主动暂停（原生响应头）**：
  - 在诊断快照节流之前解析 `x-codex-*`，任一窗口用量达到阈值即冷却生图（reason `openai_image_quota_pause`）。
  - 镜像保护：响应头窗口时长与用量都和主池快照一致时，视为疑似回显，不暂停，并在快照中记 `mirror_suspected`。
- **`/wham/usage` 生图池**：
  - 只在会写状态的 POST 刷新和自动重置中处理，只读 GET 查询不写。
  - 名称含 image 的附加额度池写入 `codex_image_usage_snapshot`；上游明确用满时冷却生图并记录观测。
- **用满观测**：
  - 存在 `codex_image_quota_observations`，只保留最近 20 条，同一窗口（重置时间相差 ±60s）去重。
  - 字段：套餐、窗口、重置时间、用量与 `counted_from`。
  - 后台异步写入，同一账号每分钟最多写一次，失败只记日志。
- 两个诊断键都加入调度中立键与管理员编辑保留键。

### Phase 4 按套餐统计与限额

- **限额执行**：`RecordUsage` 入账后异步核对，按滚动窗口计数。
  - 计数方式：库内计数排除本次 request_id，再加上本次张数。
  - 达到上限时冷却生图到“窗口内最早一张 + 窗口”（reason `openai_image_plan_limit`）。
  - 冷却复用 `openai:image_generation`，调度热路径不加查询。
- **统计接口**：`GET /admin/accounts/openai-image-quota-stats`。
  - 按套餐：用满时张数的最小/中位/最大值。
  - 按账号：当前窗口（张数统计到快照时刻）、估算上限、冷却原因、最近一次用满。
- **前端**：“更多操作 → 生图额度（按套餐）”弹窗。
  - 两个标签页：按套餐、按账号。
  - Go/Plus/Pro 100/Pro 200/Pro 500 固定显示，每个套餐最多 4 条规则。
  - 弹窗内可设置主动暂停阈值。

### 实施中相对计划的调整

1. **观测张数改为读时计算**：观测记录只保存 `counted_from`，张数在统计接口中按保留日志计算。这样写入路径不依赖用量仓储，RateLimitService 与 OpenAIQuotaService 也不必改 Wire 注入；并且计算时异步批量写入的日志已落库，结果更准确。代价是日志清理会让历史样本变小，统计说明中已注明。
2. **套餐限额后台任务只携带账号 ID 与套餐类型**，避免请求结束后账号对象被复用造成数据竞争。
3. **`isOpenAIImageScopedRateLimit` 增加 headers 参数**；为保持现有测试意图，只做签名适配。
4. **`openai_images_failover_test.go` 的仓储替身补充 `ListModelAvailabilityCandidates`**（语义与生产一致：忽略瞬时状态）。原因是新的生图候选补查会调用该方法；原断言未变。
5. **`TestImagesSnapshotSeparateFromCodexUsageSnapshot` 保留 100% 输入**，改用包装桩记录冷却，并新增断言：用满只写生图冷却、不写主池。
6. **`account_repo.go` 的中立键**放在 map 末尾并用空行分段，避免 gofmt 重排上游已有的对齐行。

## 3. 文件与 Diff

| 文件或模块 | 变更 | 原因 |
|---|---|---|
| `service/openai_image_main_pool_isolation.go`（新） | 主池封停识别、原生生图判定、调度放行 helper、候选补查、主池标记写入、测试恢复保护 | Phase 1 核心 |
| `service/openai_gateway_scheduling.go`、`openai_account_scheduler.go` | 关卡改为请求感知；`listSchedulableAccounts` 合并候选；自动暂停/调度阈值对原生生图跳过 | 调度放行 |
| `service/ratelimit_service.go`、`account_test_service.go` | 主池 429 改调 `persistOpenAIRateLimit` | 写主池标记 |
| `service/openai_image_quota_scope.go`、`openai_account_runtime_block_fastpath.go` | 429 归属增加 headers 与 Luna 判定；响应头主动暂停与 429 观测接入 | 归属修正、主动暂停 |
| `service/openai_images_direct.go`、`openai_images_responses.go`、`handler/openai_images.go` | 共享模型解析；上游 direct 标记；ctx 转发模型 | 预测与实际路由一致 |
| `service/openai_gateway_service.go`、`config/config.go`、`deploy/config.example.yaml` | 开关字段、默认值、进程级发布、候选缓存字段 | 配置开关 |
| `service/openai_image_quota_settings.go`、`openai_image_quota_observation.go`、`openai_image_plan_limit.go`、`openai_image_quota_stats.go`（新） | 设置、主动暂停/观测、套餐限额、统计 | Phase 3/4 |
| `service/account_image_stats.go`、`repository/account_image_stats.go` | 窗口计数接口与 SQL | 限额与统计 |
| `repository/account_main_pool_rate_limit.go`（新）、`account_repo.go`、`account_image_cooldown.go`、`scheduler_cache.go` | 原子写入；行锁合并；中立键；调度投影白名单 | 标记持久化 |
| `service/admin_account.go`、`domain_constants.go`、`scheduled_test_runner_service.go`、`openai_gateway_usage.go`、`openai_quota_auto_reset.go` | 保留键、设置键、测试恢复、限额入口、usage 入口 | 联动 |
| `handler/admin/*`、`handler/dto/settings.go`、`server/routes/admin.go` | 设置与统计接口；POST 刷新接入生图池 | 管理端 |
| 前端 `AccountStatusIndicator.vue`、`imageCooldownReasons.ts`、`OpenAIImageQuotaModal.vue`、`AccountsView.vue`、`api/admin/{accounts,settings}.ts`、i18n zh/en | 状态提示、额度弹窗与入口 | Phase 2/4 |
| 测试（新增与适配） | 见第 5 节 | 验证 |
| `.gitignore`、本记录 | 精确放行本记录 | 留痕 |

本地改动均带 `[local]` 标记；没有修改生成代码、Wire、依赖声明或锁文件，也没有新增迁移。

## 4. Integration Map

| 检查项 | 实际结论 |
|---|---|
| 入口与边界 | `/v1/images/*`（含 `/async`）走同一 handler。批量生图仅 Gemini，不受影响。`/v1/responses` 生图意图与 Luna 模型仍按主池限流。 |
| 主流程 | 文本主池 429 → `handle429` 一条 UPDATE 写限流列与标记 → outbox 与快照同步 → 文本被 SQL/缓存与内存封停拦截 → 原生生图从候选补查进入 → 各关卡请求感知放行 → 原生端点转发；限流列、标记与内存封停保持不变。 |
| 失败流程 | 主池耗尽后原生端点仍被拒时，原生 429 归为生图冷却（每窗口每账号浪费一次请求），必要时关闭开关。候选查询失败时缓存空结果 5s，等同现状。观测或限额异步写入失败只记日志。 |
| 契约 | 新增管理接口：`GET/PUT /admin/settings/openai-image-quota`、`GET /admin/accounts/openai-image-quota-stats`。新增配置 `gateway.disable_openai_image_main_pool_isolation`。现有接口字段语义不变。 |
| 数据 | 新增 extra 键：`openai_main_pool_rate_limit`（进调度投影、行锁合并、编辑保留）、`codex_image_usage_snapshot`、`codex_image_quota_observations`（调度中立、编辑保留）。新增设置键 `openai_image_quota_settings`。无迁移；残留键可自校验或作为诊断数据保留。 |
| 事务与并发 | 标记与限流列同一条 UPDATE。生图冷却沿用行锁“只延长”。候选缓存合并时深拷贝。异步任务只带值类型参数并 recover。 |
| 权限与审计 | 新接口均在 admin 组；写状态只走 PUT/POST（会进入审计）；只读 GET QueryQuota 不写入。 |
| 日志 | `openai_main_pool_rate_limited`、`openai_image_main_pool_bypass`（Debug）、`openai_image_rate_limited`（reason 区分 quota_pause/plan_limit）、`openai_image_plan_limit_reached`、`openai_image_test_recovery_skipped`、`openai_image_headers_mirror_suspected`（Debug）。 |
| 兼容与回滚 | 开关关闭即恢复整号限流；阈值置 0 关闭主动暂停；清空规则即关闭套餐限额。 |

## 5. 验证证据

日志位于 `%TEMP%/sub2api-image-isolation-20261005/`。测试只用合成账号与假凭据，未调用真实上游。

| 命令或操作 | 结果 | 证据与说明 |
|---|---|---|
| `go build ./...` | ✅ 通过 | 退出码 0 |
| `go vet ./...`、`go vet -tags=unit ./...` | ✅ 通过 | 退出码 0 |
| gofmt（39 个改动/新增 Go 文件） | ✅ 通过 | `gofmt -l` 无输出；`account_repo.go` 仅新增 5 行，不重排上游行 |
| Phase 1 新增测试 | ✅ 通过 | service 9 个（主池封停矩阵、放行判定、阈值跳过、两种调度器选号、429 写标记、Luna 归属、转发层归属、端到端、测试恢复）；repository 3 个（单 UPDATE + outbox、投影保留、行锁合并）；均用 `-v` 确认已执行 |
| Phase 3/4 新增测试 | ✅ 通过 | service 8 个（设置校验/缓存、响应头暂停 6 种情形、异步观测、观测去重与上限、usage 匹配、套餐限额、统计汇总）；repository 2 个（计数 SQL）；handler 1 个（设置往返） |
| 适配的既有测试 | ✅ 通过 | `TestImageScopedRateLimitMatrix` 适配签名；`TestImagesSnapshotSeparateFromCodexUsageSnapshot` 新增断言；handler 换号测试替身补方法，原断言不变 |
| 首轮全量 `go test -tags=unit ./...` | ❌ 已处理 | handler 换号测试因替身缺少 `ListModelAvailabilityCandidates` 而 panic，已补替身；service 包因该轮运行时正在重构、编译中途失败。两包重跑通过（`backend-unit-service-handler.log`：service 208.9s、handler 40.8s） |
| 最终全量 `go test -count=1 -tags=unit ./...` | ✅ 通过 | `backend-unit-full-final.log`：58 个包 ok，`UNIT_EXIT=0` |
| 前端 `check:i18n`、`typecheck`、`lint:check` | ✅ 通过 | 退出码均为 0 |
| 前端专项 | ✅ 通过 | `AccountStatusIndicator` 9 个、`OpenAIImageQuotaModal` 4 个用例 |
| 前端全量 vitest | ✅ 通过（分批） | `frontend-full.log`：339 个文件、2556 个用例通过。另 4 个文件（3 个 GroupsView、SettingsView）在外置 pnpm 虚拟仓库下无法解析 `vue` peer，与上一轮相同；用同一临时解析钩子补跑，61 个用例通过（`frontend-remaining.log`）。合计 343 个文件、2617 个用例 |
| `pnpm build` | ✅ 通过 | `frontend-build.log`；产物位于已忽略的 `backend/internal/web/dist/`；`stores/app.ts` 动态导入提示为既有警告 |
| `git diff --check`、依赖哈希、换行符、调试残留 | ✅ 通过 | 依赖文件哈希与基线一致；未引入 CRLF、行尾空白或调试输出 |
| `go test -tags=integration` | 🚫 环境不允许 | 本机无 Docker |
| golangci-lint v2.13 | 🚫 环境不允许 | 本机未安装，按范围不安装系统工具 |
| 真实账号端到端 | ⚪ 未执行 | 需专用测试账号，未消耗真实额度 |

## 5.1 验收步骤（需真实环境）

1. 选一个专用 OpenAI OAuth 测试账号。在普通额度耗尽后：`/v1/responses` 返回无可用账号；`/v1/images/generations` 使用 gpt-image-2 成功；账号状态显示“普通额度”。
2. 同一账号用 gpt-image-1（Luna 路径）发起生图，应被拦截。
3. 在“更多操作 → 生图额度（按套餐）”中，给该账号的套餐设置“60 分钟 2 张”：第 3 张请求换号，或返回无可用账号；冷却原因显示“达到套餐生图上限”。
4. 对生图额度用满的账号执行“刷新额度”后，查看弹窗中的观测记录和用满时张数。
5. 在 config.yaml 中设置 `gateway.disable_openai_image_main_pool_isolation: true` 并重启，确认回到整号限流。

## 6. 审查结论

- **白盒**：逐项核对了 4 层拦截（SQL/缓存候选、IsSchedulable、持久化与内存封停、自动暂停/调度阈值）、粘性会话、DB 复查、预测与实际路由一致性、Luna 归属、标记写入与保护、测试恢复、上游 diff 范围，以及 gofmt 对上游行的影响。
- **灰盒**：使用模拟 SQL 过滤的仓储替身、sqlmock、模拟 HTTP 上游和 Vue 组件测试，断言实际选中的账号、冷却原因与截止时间、写入的 extra 和 API payload，而不只是替身调用次数。
- **黑盒（未执行）**：真实账号在主池耗尽时原生生图是否被上游放行、原生响应头与 `/wham/usage` 的生图池语义、各套餐的真实窗口与上限，都需要专用测试账号验证。

## 7. 风险、限制与回滚

- **上游假设未实测**：主池耗尽后原生生图接口是否仍放行。若不放行，每个窗口每个账号浪费一次请求，之后转为生图冷却；可用开关回退。
- **生图池语义未实测**：原生响应头是否反映生图池，以及 `/wham/usage` 中生图池的命名，均未实测。已有镜像保护、宽松匹配和日志兜底；默认阈值 100%，只在用满时暂停。
- **套餐限额是软限制**：同时进行中的请求可能略超上限；站外使用不计入；日志清理会让统计样本变小。
- **候选补查有开销**：每个分组每个进程每 5 秒最多查询一次分组内账号，生图候选最多延迟 5 秒生效。
- **粘性会话键与文本共用**，生图与文本在同一会话上可能来回切换账号。
- **回滚方式**：
  - 先用开关和设置止损：`disable_openai_image_main_pool_isolation: true`（需重启）、阈值置 0、清空规则。
  - 再按本记录的文件清单逐项撤销代码。
  - 不需要回滚数据；extra 中的残留键可以保留。
