---
iteration_id: "ITER-20261001-001-renewal-image-quota"
date: "2026-10-01"
status: "已实现但验证不完整"
task_level: "中大型跨层"
version_change: "NONE"
release_performed: false
---

# 迭代记录：账号自动续期、定时唤醒与生图额度隔离

## 1. 目标与范围

- 按 `c-users-admin-claude-plans-1-auth-7-clau-bright-llama.md` 实现第二轮计划 A/B/C/D，保留第一轮已有修改。
- 项目为 Go 后端与 Vue 前端共仓库；代码基线为 `42bc7f6cf`。遵循用户提供的治理规则、现有 `DEV_GUIDE.md` 和指定计划。本次只增加此迭代记录，没有初始化治理目录入口或创建另一套规则。
- 开始时已有 4 个已修改文件：`account_repo.go`、`scheduler_cache.go`、`account.go`、`account_test_service.go`；还有 4 个未跟踪的续期实现/测试文件。写入前已备份到 `%TEMP%/sub2api-plan-baseline-20261001`，后续基线复核使用 Go overlay，没有恢复、暂存或丢弃工作区代码。
- 完成状态：计划功能已实现，专项验证通过；后端全量回归存在失败项，Docker 集成测试、golangci-lint 和真实上游端到端验证尚未完成，所以不声明全部验证通过。
- 同日第二轮复核：逐项对照计划核实代码，补修两处联动缺陷（见 2 节末），并重新执行全部可在本机运行的验证（见 5 节末）。
- 非目标：不新增数据库迁移，不创建生图影子账号，不根据 `/wham/usage` 主动安装图片冷却，不改变普通 `/responses` 仅凭生图意图的选号规则，不安装系统级工具，不提交、推送、部署或发布。

## 2. 变更摘要

- 自动续期：配置规范化，服务端状态键保护；续期前按服务器本地日历校验锚点与当前到期日，改期、改周期及导入后不再盲信旧轮数。月末钳位、年周期和补齐周期继续复用原算法。
- 续期调度：每 5 分钟扫描，单次总超时 10 秒，循环检查停止信号；Wire 显式接线并纳入关闭流程。宽限期同时覆盖 ent 查询、原生 SQL、分组计数、自动暂停、调度快照及固定 Codex 清单账号。
- 宽限 SQL：限定列名，使用 PostgreSQL 参数构造器；天数字段先判定 1–9 位数字再转 int，超长纯数字封顶 365，其他值回落 7，消除整数溢出导致整组查询失败的路径。
- 账号表单：新建默认一个月有效期、按月自动续期；编辑回填现有配置，未修改到期日时不提交 `expires_at`，避免后台续期后被旧表单回滚。账号行显示续期及剩余宽限天数。
- 定时唤醒：每天、每周、每 N 小时、自定义 cron；无法准确表达的历史 cron 保持原文。固定间隔提供 1/2/3/4/6/8/12 小时，避免 `*/5` 之类跨午夜时并非固定间隔的表达被误称为“每 5 小时”；其他规则仍可自定义。
- 批量计划：复用现有单账号 API，最多并发 5 个账号；支持跳过、覆盖第一条、追加；模型留空采用各平台默认，失败逐项展示并继续处理其余账号。明确提示真实请求消耗配额，执行中固定本次目标列表。
- 紧凑模式：浏览器保存 `account-compact-mode`；行高估算在 92/156 切换，压缩单元格留白，平台类型详情折叠到悬停提示。
- 品牌图标：Gemini 复用已有官方路径；Antigravity 采用指定 5.21.0 Mono 源码；Yi/Ai360/Dify/Coze 采用已安装 4.0.2 Mono，Yi 的椭圆转换为等价弧线以维持 path 契约。OpenCode 当前路径与 5.21.0 源码相同，保留图形并标明来源。没有升级图标依赖。
- 生图额度：识别专用 OpenAI OAuth 生图请求的 429，只写 `model_rate_limits["openai:image_generation"]`；保留旧 gpt-image 标记识别。Grok 的专用请求标记和普通 Responses 的生图意图不会触发新增判定。
- 图片冷却依次采用 body reset、try-again、Retry-After、已耗尽 Codex 头、分类默认值；quota 默认 30 分钟，瞬时 rate limit 默认 1 分钟。已有更晚的快照冷却不被缩短，超过 8 秒时直接使用正常换号预算，避免创建通用 OAuth 同号重试窗口。
- 流内额度错误安装生图冷却，保留 429 与 502 原有响应、重试语义；原生 `/codex/images/*` 头写入独立诊断快照，Luna `/codex/responses` 回退仍写通用快照。图片诊断单独节流，不触发自动重置或调度 outbox。
- 状态列显示“生图”作用域；查询次数/积分后，按上游返回名称展示 `additional_rate_limits` 的窗口使用率与重置时间。

### 实施中确认的计划补充

现有定时计划 Update 把 `model_id: ""` 当成“不更新”，使“覆盖第一条 + 模型留空”无法恢复平台默认。将内部请求字段改为 `*string`：省略/null 保留原值，显式空字符串清空。JSON 字段和路由不变，新增 handler 回归测试覆盖三种情况。这是完成批量计划所需的最小契约语义补充。

### 第二轮复核补充

1. 单账号定时测试面板：批量下发的留空模型计划，标题原来显示空白；编辑时又因“必须选模型”而无法保存，只改 cron 也不行。改为空模型显示“平台默认模型”，编辑只要求 cron，未改动时以空串提交，后端保持平台默认。新建计划仍按上游要求必须选模型。
2. 批量定时计划：API 客户端 reject 的是普通对象 `{status, message}`，不是 `Error`。原逻辑因此只显示通用“失败”，看不到服务端原因（如非法 cron）。改为复用项目已有的 `extractApiErrorMessage`。
3. 两条新测试都先对旧逻辑执行过，确认会失败，然后才恢复修复。另外把中文 `scheduledTests` 本地块整理为每行一个键，与该文件及英文版的写法一致。

## 3. 文件与 Diff

| 文件或模块 | 变更 | 原因 | 是否计划内 |
|---|---|---|---|
| `backend/internal/service/account_renewal_*` | 补齐解析、锚点、后台扫描及测试 | 完整续期生命周期 | 是 |
| `backend/internal/repository/account_renewal_*` | SQL 片段、谓词、窄接口、CAS 与 SQL 测试 | 宽限期查询一致性及溢出保护 | 是 |
| `account_repo.go` / `group_repo.go` / `scheduler_cache.go` | 宽限查询、快照字段、诊断字段中立化 | 调度与存储联动 | 是 |
| `account.go` / `admin_account.go` / `openai_codex_models_pinned.go` | 调度宽限与管理入口规范化 | 各入口行为一致 | 是 |
| `backend/cmd/server/wire*` | 启停接线、生成文件、关闭测试参数 | 服务实际运行 | 是 |
| `openai_image_quota_scope*` / `openai_account_runtime_block_fastpath.go` / `openai_images_responses.go` / `handler/openai_images.go` | 图片作用域限流、快照、流内冷却及测试 | 图片额度隔离 | 是 |
| `handler/admin/scheduled_test_handler.go` / `scheduled_test_renewal_test.go` | 显式空模型恢复默认 | 批量覆盖旧计划的必要补充 | 计划内功能的补充接点 |
| `frontend/src/components/account` | 续期配置、创建/编辑、状态及额度池展示、回归测试 | 账号管理入口 | 是 |
| `frontend/src/components/admin/account` / `utils/cronSchedule*` | cron 选择、批量计划、现有面板与批量栏接入、测试 | 定时唤醒配置 | 是 |
| `AccountsView.vue` / `useAccountsDensity.ts` / `PlatformTypeBadge.vue` | 紧凑模式、续期徽章及弹窗 | 列表操作与信息展示 | 是 |
| `ModelIcon.vue` / `PlatformIcon.vue` / `ProviderIcon.vue` / zh/en 语言文件 | 图标及配套文案 | 保持组件契约和语言键一致 | 是 |
| `account_test_service.go` | 保留用户已有 `Output exactly: OK` 改动 | 第一轮已完成需求 | 是，原有修改 |
| `ScheduledTestsPanel.vue` / `__tests__/ScheduledTestsPanel.spec.ts` / zh/en `resources.ts` | 空模型计划显示与编辑、`platformDefaultModel` 文案 | 第二轮复核：批量计划与现有面板联动 | 计划内功能的补充接点 |
| `BulkScheduledTestModal.vue` 及其测试 | 失败原因按 API 错误形状提取 | 第二轮复核：逐项失败原因可见 | 是 |

上游改动接点带 `[local]` 标记；`wire_gen.go` 由生成器产生，没有手工修改。源码和测试之外仅增加本记录，构建输出按项目既有 Vite 配置生成。

## 4. 联动影响（Integration Map）

| 检查项 | 实际结论 |
|---|---|
| 自动续期链路 | 管理表单 → extra 配置 → 仓储扫描业务用量/定时测试成功 → 锚点计算 → CAS 更新 → outbox/快照 → 调度热路径；过期暂停与分组计数共享宽限 SQL |
| 生图链路 | Images handler → 专用上下文 → 429 早退 → 图片模型冷却 → 有预算的换号；成功头按实际上游端点选择诊断或通用快照 |
| 定时计划链路 | 批量栏 → 表单固定目标列表 → 5 个 worker → 查询冲突 → Create/Update；后端仍用原有 runner 发真实测试请求 |
| API、CLI、事件或配置 | 没有新路由或公开接口字段；补充定时计划 `model_id` 的显式清空语义；extra 使用 `auto_renewal_*`、`codex_image_headers_snapshot`，紧凑模式仅浏览器存储 |
| 数据、迁移和状态 | 无 schema/migration。续期更新带到期日 CAS，受管状态由后台写；生图冷却复用 `SetModelRateLimit`，诊断快照不刷新调度 |
| 权限、归属、租户和配额 | 沿用现有管理员权限与计费；定时唤醒实际消耗上游配额；只有图片额度信号进入新增隔离路径 |
| 错误、日志、审计和指标 | 复用 `[AccountRenewal]`、`openai_image_rate_limited`；保留原生错误状态码；新增 reason 区分 quota 和 rate |
| 外部服务、超时、重试和幂等 | 续期扫描 5 分钟，10 秒总超时；图片冷却 >8 秒直接切号；批量冲突策略显式选择，完成后禁止重复提交同一轮 |
| 旧版本和旧功能兼容 | 存量账号缺少续期配置时默认关闭；无法解析的 cron 保留；紧凑模式默认关闭；普通文本 429 的账号级处理保留；残留 extra 键不会要求数据库迁移 |

## 5. 验证证据

以下日志均保存在本机 `%TEMP%`，未复制含真实账号数据的响应、密钥或 Token 到仓库。

| 命令或操作 | 结果 | 证据摘要 | 未通过或未执行原因 |
|---|---|---|---|
| Wire v0.7.0 `gen ./cmd/server` | ✅ 通过 | 实际生成 `wire_gen.go`，服务被启动且接入 cleanup | 原预装 Wire 用 Go 1.26 构建，报项目要求 Go 1.27；在临时 GOBIN 用缓存的 Go 1.27 重建同版本后成功 |
| `gofmt -l ./internal ./cmd` | ✅ 通过 | 最终无输出 | 只对本次目标文件运行写入格式化 |
| `go vet ./...`、`go build ./...` | ✅ 通过 | handler 补充后也重新执行通过 | 无 |
| 计划后端专项回归及新增 handler 测试 | ✅ 通过 | repository / service / handler/admin / cmd/server 全部通过；`sub2api-backend-targeted-20261001.log` | 无 |
| 图片快照测试 `-count=3` | ✅ 通过 | 验证独立节流测试可重复运行 | 无 |
| `go test -tags=unit ./...` | ❌ 失败 | `sub2api-backend-unit-20261001.log`；失败项见下文 | 全量回归不能声明通过 |
| 修改前基线 overlay 复核 | ❌ 失败 | 基线 service 全量也复现 Ollama 同名失败；聚焦及全量日志见下文 | 确认既有失败；不覆盖工作区 |
| 临时 PostgreSQL 18.3 合成校验 | ✅ 通过 | 复现旧 cast 的 22003；12 组布尔/天数期望全部吻合；两个实际 SELECT 与 AutoPause UPDATE 的 `EXPLAIN` 均成功 | 校验在 `BEGIN READ ONLY` 中执行；临时实例只含合成空表，已关闭 |
| 现有本地数据库宽限账号查询 | 🚫 环境不允许 | 本机实际为 PostgreSQL 18，文档中的应用与管理员凭据均认证失败 | 未调整认证、密码、权限或既有数据 |
| `go test -tags=integration ./...` | 🚫 环境不允许 | 本机未发现 Docker 命令 | testcontainers 无法运行；临时 PostgreSQL 校验不能等价替代全部集成测试 |
| `golangci-lint run ./...` | 🚫 环境不允许 | PATH 和 Go bin 未找到该工具 | 按计划未安装系统工具 |
| pnpm 冻结锁文件安装 | ✅ 通过 | `sub2api-pnpm-install-shortpath-20261001.log`，973 个包 | 原目录安装遇 Windows rename 重试，改临时 virtual store；没有升级依赖或修改锁文件 |
| `pnpm run check:i18n` | ✅ 通过 | 3 项检查通过，build 前置也通过 | 临时依赖目录使用进程级 NODE_PATH 补 peer 回溯 |
| `pnpm typecheck`、`pnpm lint:check` | ✅ 通过 | 类型检查退出码 0；`sub2api-frontend-lint-verified-20261001.log` 退出码 0 | 无源码规则降级 |
| 前端专项回归 | ✅ 通过 | 7 文件、142 用例；含批量并发/冲突、旧 cron、表单日期及续期、附加额度池；`sub2api-frontend-targeted-20261001.log` | 无 |
| 前端全量与环境补跑 | ✅ 通过（分批） | 全量 334 文件、2518 用例通过；另 4 文件因外部 virtual store 的 ESM Vue peer 加载失败，修正本次验证的模块解析路径后补跑 59 用例通过；合计 338 文件、2577 用例 | `sub2api-frontend-tests-verified-20261001.log`、`sub2api-frontend-remaining-20261001.log`；未修改测试断言或依赖源码 |
| `pnpm build` | ✅ 通过 | `sub2api-frontend-build-20261001.log`；Vite 构建完成 | 有既有体积/浏览器数据过期提示，未顺手升级依赖或重构拆包 |
| `git diff --check` 与依赖文件 SHA256 | ✅ 通过 | 无空白错误；`go.mod`、`go.sum`、`pnpm-lock.yaml` 与开始时哈希一致 | 无 |
| 真实上游、真实账号端到端及窗口截图 | ⚪ 未执行 | 没有触发真实配额消耗或修改业务库来制造过期/限流 | 需要在实际运行环境验收；本轮采用本地模拟上游和合成数据 |

### 后端全量失败项及基线复核

首次全量回归出现以下三个失败，不删除测试、不降低断言：

1. `TestInflightEstimate_AccountMappingNoDBAndBoundedMemory`：分配量 8,482,872 字节超过 8,388,608 字节；本轮没有修改相关计费实现。聚焦复跑与修改前基线聚焦复跑通过，尚不能据此宣称全量运行中的内存波动已解决。
2. `TestFilterGrokFreeQuotaAccountsOnlyBlocksExplicitFreeOAuth`：第二次筛选仍保留免费账号；本轮没有修改 Grok 配额过滤实现。当前聚焦复跑曾失败；修改前基线单项重复 20 次通过，异步时序问题仍需单独排查。
3. `TestOllamaProbeCallback_StaleLongDoesNotOverrideNewShort`：旧回调仍通过 CAS。修改前基线聚焦复跑 3 次均复现同样失败，确认并非本次生图/续期接入才出现。

最终又对修改前基线完整执行 `go test -overlay=<baseline.json> -tags=unit ./internal/service`，耗时 216.792 秒，仅上述 Ollama 用例失败；其他两项在该次基线全量中通过。对应日志为 `sub2api-baseline-service-full-20261001.log`。这些结果区分了稳定既有失败与不稳定失败，没有把未复现等同于已修复。

### 本机前端验证环境说明

依赖安装最终使用 `pnpm install --frozen-lockfile --offline --package-import-method=copy --virtual-store-dir=<TEMP>/sub2api-frontend-virtual-store`。Windows 原目录重命名重试通过只读检查 pnpm 已安装源码和本次安装进程确认，未修改 pnpm 源码或系统权限。

外部 virtual store 的少数 peer 依赖无法回溯到本项目 `node_modules`。CJS 验证使用进程级 `NODE_PATH=<frontend>/node_modules`；仅余 4 个 ESM 测试使用临时 `sub2api-vue-peer-resolution.mjs`，只在原解析报 `ERR_MODULE_NOT_FOUND` 且包名是 `vue` 时，从项目 package.json 位置按原 exports 重新解析同一已安装版本。没有 Mock Vue、跳过测试或写入全局环境变量。正常环境在项目内安装依赖时不需要此临时处理。

临时 PostgreSQL 证据位于 `%TEMP%/sub2api-renewal-pg-m6mszrmq/{overflow.log,readonly.sql,verification.log}`；监听仅为 `127.0.0.1:55437`，完成后已用 pg_ctl 停止并核对无服务运行。现有 PostgreSQL 服务未重启或修改。

### 第二轮复核验证（在含两处补充修复的最终代码上执行）

| 命令或操作 | 结果 | 证据摘要 |
|---|---|---|
| `gofmt -l ./internal ./cmd`、`go build ./...`、`go vet ./...` | ✅ 通过 | 无输出，退出码均为 0 |
| `go vet -tags=unit`（service / repository / handler / cmd/server） | ✅ 通过 | 覆盖带 `unit` 标签的新测试文件 |
| 计划后端专项 `-run` 与 handler/admin `ScheduledTest` | ✅ 通过 | `-v` 核对 32 个新增测试均实际执行并通过 |
| `wire diff ./cmd/server`（v0.7.0，Go 1.27 构建） | ✅ 通过 | 无差异，`wire_gen.go` 与生成器输出一致；`go.mod`/`go.sum` 未变 |
| `go test -count=1 -tags=unit ./...` | ❌ 失败 | 57 包通过，仅 `TestOllamaProbeCallback_StaleLongDoesNotOverrideNewShort` 失败。该用例两次 `time.Now()+5s` 在 Windows 同一毫秒内会相等，CAS 被误判通过；文件未被本轮触及，基线也复现。上一轮另两项不稳定用例本次通过。`sub2api-backend-unit-recheck2-20261001.log` |
| 前端专项（admin/account 全部、cron、续期、创建/编辑弹窗、额度卡、i18n） | ✅ 通过 | 14 文件、178 用例 |
| `pnpm typecheck`、`pnpm lint:check` | ✅ 通过 | 退出码 0；`sub2api-frontend-*-recheck-20261001.log` |
| 前端全量 `vitest run` | ✅ 通过（分批） | 335 文件、2521 用例通过；4 个 GroupsView/SettingsView 文件因外置 virtual store 的 `vue` peer 解析而加载失败，用同一临时解析钩子补跑 59 用例通过；合计 339 文件、2580 用例 |
| `pnpm build` | ✅ 通过 | i18n 门禁、`vue-tsc -b`、Vite 构建完成；产物位于已忽略的 `backend/internal/web/dist/`，`git status` 无新增条目 |
| `git diff --check`、新文件换行与行尾空白、依赖文件状态 | ✅ 通过 | 编辑文件保持原 CRLF；`pnpm-lock.yaml`、`package.json`、`go.mod`、`go.sum` 无变化 |
| `go test -tags=integration`、`golangci-lint`、真实上游端到端 | 🚫 / ⚪ | 仍无 Docker；本机只缓存 golangci-lint v2.9.0 源码，与 CI v2.13 不一致，按计划不安装；未触发真实配额 |

## 6. 审查结论

- 白盒：核对普通文本与生图分支、Grok/API Key 判定、冷却优先级、流内错误、快照归属、管理状态键、改期重锚、日期边界、Wire 启停、实际生成 SQL 和最终 diff。
- 灰盒：使用既有仓储 stub、sqlmock、模拟 HTTP 上游及 Vue 组件测试，断言实际冷却作用域、最终日期、API payload、批量结果和额度卡内容；不是仅以 Mock 调用次数替代业务结果。
- 黑盒：真实账号 429 后文本仍可用、生产时区下实际续期、真实定时测试启动 5h 窗口、四行可视高度尚未在运行环境验证。
- 压力与滥用：验证批量最大并发为 5、单账号失败不中断、已完成批次不重复提交、超长天数不会 SQL 溢出。没有对真实上游或业务库做压测。

## 7. 版本、风险与回滚

- 当前代码基线：`42bc7f6cf`。未修改版本号，未暂存、提交或发布。
- 已知限制：已被 AutoPause 暂停的账号再开启续期，不会自动恢复调度；需要管理员显式恢复。每轮扫描最多 200 个候选，极大积压时处理可能超过一个扫描周期。紧凑模式能否一屏显示四行取决于窗口高度和可见列。
- 未验证风险：后端全量失败仍存在；缺少 Docker 集成测试与 golangci-lint；实际 PostgreSQL 16 和真实账号链路未验证。本机已用 PostgreSQL 18.3 验证 SQL 解析/绑定与合成配置。
- 回滚：按本轮 diff 撤销新增接点和本地文件，Wire 源定义回退后重新生成。必须保留任务开始前的四个已修改文件及四个续期文件原内容，可对照 `%TEMP%/sub2api-plan-baseline-20261001`，不要对整个工作区 reset/clean。未做数据迁移；以后运行产生的 extra 配置键可以保留，模型冷却按时间自然失效。
- 验收：新建账号确认默认一个月与按月续期；编辑无关字段确认不会回写旧到期日；勾选多个账号配置定时计划，分别验证跳过/覆盖/追加；切换紧凑模式并刷新；查询额度卡查看附加池。使用专门测试账号验证图片 429 仅出现“生图”冷却、文本仍可调用，以及到期后成功请求或定时测试可在扫描后顺延到期日。
- 建议 Conventional Commit（未执行）：`feat(account): add automatic renewal and grace scheduling`、`fix(openai): isolate image generation quota cooldowns`、`feat(admin-ui): add bulk schedules and compact account view`。

图标源码参考：[Antigravity Mono 5.21.0](https://unpkg.com/@lobehub/icons@5.21.0/es/Antigravity/components/Mono.js)、[OpenCode Mono 5.21.0](https://unpkg.com/@lobehub/icons@5.21.0/es/OpenCode/components/Mono.js)；其余 Mono 源码直接读取已安装的 `@lobehub/icons@4.0.2`。


## 8. 2026-10-02 缺口修复与验收追加

本节追加记录，不覆盖上文 2026-10-01 的历史结果。当前状态仍为 **已实现但验证不完整**：本轮代码与本机自动化验收通过，Docker 全套集成测试、golangci-lint v2.13、真实账号端到端验收尚未完成。

### 8.1 基线、授权与实际范围

- 用户明确要求实施“生图额度与自动续期缺口修复计划”，并确认一并处理 Ollama 既有失败用例。
- 实施前工作区已有 33 个跟踪文件修改及 19 个未跟踪文件。已将已有修改、目标文件、迭代记录和依赖文件共 65 个文件保存到 `%TEMP%/sub2api-gap-fix-20261002-iq6h4pzo/`；后续涉及的原有 OpenCode 测试文件也在修改前加入备份。
- 备份含原始字节副本、SHA256 清单、Git HEAD、完整工作区 Diff 和状态。回退基线是本轮开始时的工作区，而非 Git HEAD。
- 本轮没有修改前端源码、Dockerfile、依赖声明、锁文件或 Wire 生成文件；没有新增业务数据库迁移，没有操作现有应用数据库，没有暂存、提交、推送、部署、安装系统工具。

### 8.2 实际修复与 Integration Map

| 链路 | 修复后的行为 | 联动与兼容 |
|---|---|---|
| 所有生图冷却入口 → SetModelRateLimit → PostgreSQL | `openai:image_generation` 统一进入独立文件中的事务行锁分支；锁定后读取最新冷却，用 Go 解析时间，仅在新截止时间更晚时写入 | 覆盖额度耗尽、余额不足、能力不可用及流内错误；其他 scope 保持原路径；相等或更早时保留旧原因和限流时间 |
| 生图冷却 → outbox → 调度快照 | 冷却和 outbox 同事务提交；本方法拥有的事务提交后刷新快照；未延长时无新增 outbox，但刷新当前快照；外层事务依赖提交后的 outbox，不提前发布未提交状态 | 数据库不存在的账号返回原有错误；坏时间按无有效截止时间处理；保留其他模型冷却 |
| 生图 429 → 长冷却切号 | 服务层不再用旧快照省略数据库保护；切号判定补读最新持久化冷却，与请求已知时间取最大值，超过 8 秒直接切号 | 不写账号级限流，不建立通用 OAuth 同号重试窗口；日志使用 requested_reset_at，避免把较短的写入意图误说成最终截止时间 |
| 续期扫描 → 候选 → 写回 | 候选保存三个续期配置键的原始 JSON；续期写入先锁行，再核对 active 状态、未删除、到期日、原始配置、启用状态及写入时宽限期 | 配置变动返回 ErrAccountRenewalConflict，交下一轮重算；非续期 extra 更新不造成无意义冲突；数字使用原始 JSON/UseNumber 防止精度丢失 |
| 管理端编辑 → 现有仓储更新事务 | 内部 UpdateWithAccountBillingSettings 增加 expiresAt 指针，省略时不写到期日，正数明确设置，非正数清除；锁定合并最新 anchor/cycles/last_at 和生图冷却子项 | HTTP 契约不变；普通 Update 仍支持显式日期更新并保留微秒精度；不复活已主动清除的生图冷却；返回对象携带实际保存状态 |
| Ollama 旧回调测试 | 用捕获的旧截止时间加一秒构造确定的新一代短冷却 | 保留全部 CAS/通知断言，没有 sleep、降低断言或修改生产 Ollama 逻辑 |
| 迭代归档 | .gitignore 精确放行本记录及必要父目录 | 其他治理内容仍遵循原忽略规则，不执行 git add |

主要新增文件为 `account_image_cooldown.go`、`account_image_cooldown_test.go` 和 `account_renewal_image_concurrency_integration_test.go`。其余变动集中在既有续期、生图和管理更新接点及其接口调用、测试替身；OpenCode/计费探测测试的调整仅用于匹配原有锁定查询新增的受管状态列。

### 8.3 失败复现与额外边界发现

1. 修复前新增图片仓储测试确认原 SetModelRateLimit 没有锁定事务；生图切号测试确认旧账号快照会忽略已持久化的一小时冷却。失败输出：`red-image-tests.log`。
2. 首轮专项测试发现 sqlmock 以字符串返回 JSON 时不能直接 Scan 到 json.RawMessage。改为先 Scan 到 []byte，再保存为 RawMessage；真实 PostgreSQL 和 sqlmock 均可使用同一实现。
3. 真实 PostgreSQL 测试额外复现：UPDATE 的 clock_timestamp() 条件可能在等待行锁前求值，即使拿锁时宽限期已结束，仍会更新成功。`grace-lock-boundary.log` 保留失败；改为先取得行锁，再执行带全部保护条件的 UPDATE，最终同一测试通过。
4. 最终复核进一步约束事务归属：由外层管理的事务不提前刷新调度缓存；本方法自己提交的事务才同步刷新。补跑相关仓储、服务接入编译、vet、build 和 PostgreSQL 测试。

### 8.4 本轮验证证据

以下日志、截图和临时脚本均在 `%TEMP%/sub2api-gap-fix-20261002-iq6h4pzo/`；测试仅使用合成账号与假凭据。

| 验证 | 结果 | 实际证据与范围 |
|---|---|---|
| 修复专项 Go 测试 | ✅ 通过 | `targeted-final.log`；续期、图片 429、额度快照、管理日期意图、原有锁定合并等用例 |
| 生图 429 后同账号文本请求 | ✅ 通过 | `TestImageScoped429ThenTextResponseSucceeds`：模拟上游先返回图片 usage_limit 429，再经实际 Forward 文本链路返回 OK；核对图片冷却、账号可调度、无账号级限流 |
| Ollama 目标用例 `-count=100` | ✅ 通过 | `ollama-100.log`；连续 100 次通过 |
| `go test -mod=readonly -count=1 -tags=unit ./...` | ✅ 通过 | `backend-full.log`；58 个包通过，service 包 212.636 秒；此前 Ollama、内存分配量和 Grok 失败本次均未出现 |
| 最终仓储相关补跑 | ✅ 通过 | `repository-final.log`；repository、server、cmd/server 整包通过，覆盖事务归属和 nil 兼容的最后调整 |
| `go vet ./...`、`go build ./...`、gofmt 检查 | ✅ 通过 | `backend-vet.log`、`backend-build.log`、`backend-gofmt.log` 均无输出；最后调整后 `vet-final.log` 覆盖 unit 标签，`build-final.log` 再次通过 |
| PostgreSQL 18.3 真实仓储测试 | ✅ 通过 | `postgres-final-verified.log`；13 个顶层测试通过，含原有仓储回归。复用现有 integration 测试和 ApplyMigrations，以临时 Go overlay 仅替换测试环境启动段，连接隔离的本地 PostgreSQL；仓库测试框架未被修改 |
| 双连接并发与状态完整性 | ✅ 通过 | 测试实际观察 pg_stat_activity 的行锁等待；短冷却等待长冷却提交后不会覆盖它；相等截止时间不改原因/首次时间、不新增 outbox，并刷新缓存；外部事务回滚后冷却及 outbox 一同回滚 |
| 续期配置、日期和宽限边界 | ✅ 通过 | 关闭续期、改周期/天数、改期、停用、删除、过了宽限期均拒绝旧候选；实际行锁等待跨截止时间测试通过；9007199254740993 原始数值精度保留；无关 extra 更新不阻止合法续期 |
| 旧表单、显式改期/清除 | ✅ 通过 | 后台续期和生图冷却后保存旧对象的名称，数据库与返回对象保留最新到期日和受管状态；显式正数、零、负数按原约定执行；主动清除冷却后再次编辑不复活旧值 |
| 前端 typecheck / lint / i18n | ✅ 通过 | `frontend-typecheck.log`、`frontend-lint.log`、`frontend-i18n.log`；退出码均为 0 |
| 前端全量与构建 | ✅ 通过 | `frontend-full.log` 一次运行 339 文件、2580 用例全部通过；`frontend-build.log` 构建通过。本轮无前端源码变更，沿用已记录的进程级 NODE_PATH 和临时 Vue peer 解析钩子解决外置 pnpm virtual store 的模块解析 |
| 浏览器合成账号验收 | ✅ 通过（有尺寸限制） | 隔离的 Chromium、当前实际构建、8 个合成账号；API 全部拦截为合成数据，外部访问被阻止；`ui-verification.json` 与四张截图。OAuth 信息保留在 title，续期信息保留，刷新后紧凑设置有效，pageerror 为零 |
| 依赖、生成文件与 Diff | ✅ 通过 | go.mod、go.sum、package.json、pnpm-lock.yaml、wire_gen.go 的 SHA256 与本轮基线一致；原有前端文件逐字节未变；git diff --check 通过；本记录出现在未跟踪文件列表中 |
| Docker 全套 integration / PostgreSQL 16 | 🚫 环境不允许 | 本机没有 Docker；本轮已验证真实 PostgreSQL 18.3，但不能将其称为 Docker 全套测试或 PostgreSQL 16 验证 |
| golangci-lint v2.13 | 🚫 环境不允许 | 本机无对应可执行工具；遵照范围未安装系统工具 |
| 指定真实账号端到端 | ⚪ 未执行 | 尚未指定测试账号与环境，没有消耗真实上游配额，也没有修改业务数据 |

临时 PostgreSQL 仅监听 `127.0.0.1:12844`，使用本轮新建的数据目录。收尾查询测试账号数为 0，已通过 pg_ctl 停止并确认端口关闭；`postgres/verification-summary.json` 和 `postgres/stop.log` 保存证据。现有 PostgreSQL 服务未修改。

### 8.5 界面实测限制

| 视口 | 普通模式完整可见行数 | 紧凑模式完整可见行数 | 合成样本行高 |
|---|---:|---:|---|
| 1440×900 | 4 | 5 | 101px → 81px |
| 1280×720 | 2 | 2 | 101px → 81px |

1280×720 的筛选条件换行，压缩了表格可用高度，因此不能声明所有窗口均“一屏四行”。截图为默认侧栏和默认可见列；合成账号没有加载真实上游的复杂用量卡，实际行高仍受内容影响。本轮按计划记录结果，没有扩展重构筛选栏或表格布局。

### 8.6 后续验收与回退

- 在具备 Docker、golangci-lint v2.13 的现有开发/CI 环境执行剩余门禁；运行 PostgreSQL 16 时使用已有 `SUB2API_TEST_POSTGRES_IMAGE` 覆写能力，无需改测试框架。
- 指定专用测试账号后，验收生图 429 仅冷却图片、同账号文本实际成功，以及真实请求/定时测试成功触发续期。未完成前保持“已实现但验证不完整”。
- 原有限制继续存在：已暂停账号开启续期不会自动恢复；大规模积压仍按每轮 200 个处理；本轮修复没有调整这些业务策略。
- 回退只撤销本轮增量：按备份 manifest 和 `task-existing.diff` 逐文件恢复到实施前内容，并移除本轮新增的三个文件；备份中的原有未提交功能不得丢失，不使用 reset/clean。未新增业务 migration，无需数据回迁。
- 建议提交（均未执行）：`fix(openai): make image cooldown extension atomic`、`fix(account): preserve renewal state during concurrent updates`、`test(ratelimit): make stale callback regression deterministic`。


## 9. 2026-10-02 每账号生图数量统计

### 9.1 目标与统计口径

用户要求为每个账号增加生图数量统计。本轮状态：**完成且已验证（本地代码、合成数据及浏览器验收）**。上文真实上游额度与续期的未完成验收不因此改变。

- 账号列表新增默认可见的“生图数量”列，显示“今日 N 张 / 累计 N 张”；通过“更多操作 → 显示列 → 生图数量”可隐藏，列选择沿用已有浏览器持久化。
- 按当前站点保留的 `usage_logs.image_count` 汇总，按图片张数计算，一次请求生成多张分别计数；零费用图片、按 token 计费的图片和历史无 billing_mode 的图片均计入。
- 今日按服务器配置时区，累计覆盖保留日志截至查询时的记录。排除未出图记录、视频记录和未来时间记录；继承现有用量日志幂等性，不新建计数器。
- 不包含站外生成和未进入用量日志的管理员测试图片；日志清理会改变累计值。该字段不代表上游额度或剩余生图次数，悬停说明已注明口径。
- 加载中显示占位，查询失败或缺少数据时显示“—”，真实零张显示 0。

### 9.2 变更与 Integration Map

| 链路 | 实际变更 | 兼容及边界 |
|---|---|---|
| UsageLog 仓储 | 新增 account_image_stats.go，按账号数组一次查询今日与累计 SUM(image_count)，补齐无记录账号零值 | 复用已有 account_id 索引和日志，无 schema/migration，不改写入与计费链路 |
| AccountUsageService | 通过窄批量读取接口查询图片统计，过滤无效/重复 ID，设置 10 秒查询超时 | 数据库失败向上传递，不伪装为零；不调用外部模型 |
| 管理 API 与缓存 | 复用 POST /admin/accounts/today-stats/batch，增加可选 include_image_stats，启用时附带 image_stats 映射 | 旧请求与原 stats 字段兼容；缓存键区分含图统计的响应及服务器日期，沿用 30 秒缓存；仅管理员路由 |
| 账号列表 → API → 单元格 | 当前页批量请求，显示默认可见的新列；隐藏该列后省略图片查询选项，所有统计相关列隐藏时跳过请求 | 无逐账号请求、无新请求层；复用刷新、翻页、自动刷新和列持久化逻辑；zh/en 同步 |
| 审计 | 在本记录追加本节，保留全部前序修改和验证历史 | 未提交、推送、部署；未修改依赖、锁文件或生成代码 |

实施前基线保存在 `%TEMP%/sub2api-image-stats-20261002-prartoec/`，包含 73 个已有修改/目标文件的原始副本、SHA256 清单和工作区 Diff。本轮按这个基线增量实现，未覆盖前序续期与冷却修复。

### 9.3 验证证据

| 验证 | 结果 | 证据 |
|---|---|---|
| 后端专项 | ✅ 通过 | backend-targeted.log；仓储聚合、服务 ID 规范化/日期/超时、可选响应缓存隔离及既有今日统计检查 |
| 后端相关整包回归 | ✅ 通过 | backend-regression.log；repository 14.155 秒、service 218.231 秒、handler/admin 0.783 秒，均退出码 0 |
| Go vet / build / gofmt | ✅ 通过 | backend-vet.log、backend-build.log；格式检查无输出 |
| PostgreSQL 18.3 合成集成测试 | ✅ 通过 | postgres-final.log；真实日志写入后核对账号隔离、+08 时区午夜边界、一请求多图、零费用/按 token 计费/历史计费模式、零张、视频排除、未来记录排除及重复请求不重复计数 |
| PostgreSQL 首次失败处理 | 已解决 | 首次合成记录缺少现有数据库要求的 image_size，违反 usage_logs_image_billing_size_check；补充合法测试尺寸 1K 后通过，未改约束或生产 Schema |
| 前端专项 | ✅ 通过 | frontend-targeted.log；9 文件、49 用例，覆盖账号列表现有交互、新列默认请求与按账号展示、隐藏时不查询累计、零值/加载/失败显示和语言键完整性 |
| 前端 typecheck / lint / build | ✅ 通过 | frontend-typecheck.log、frontend-lint.log、frontend-build.log；退出码均为 0，构建前置 i18n 检查通过 |
| 真实浏览器渲染 | ✅ 通过 | ui-verification.json、image-counts-1440x900.png、image-counts-1280x720.png；当前构建、隔离 Chromium、8 个合成账号，验证 0/0 与 3/17 张、普通/紧凑显示、刷新保留、隐藏列持久化、隐藏后请求省略 include_image_stats；pageerror 为 0 |
| Diff 与范围核对 | ✅ 通过 | git diff --check、依赖和前序修复文件 SHA256 对比；本轮未变更上游配额或续期行为 |
| 本轮全仓 Go / 全量前端测试 | ⚪ 未执行 | 本轮运行受影响的后端三个整包、账号列表全部测试、新组件及 i18n；上节全仓结果仅作为历史证据，不冒称本轮重跑 |
| Docker 全套集成 / golangci-lint | 🚫 环境不允许 | 本机缺少对应工具；真实 PostgreSQL 定向验证使用临时 overlay 复用原集成框架，未修改仓库测试环境入口 |
| 真实业务库与大数据量性能 | ⚪ 未执行 | 使用合成数据库和被拦截的浏览器 API，未接触真实业务数据或消耗上游配额 |

上述日志、临时脚本与截图均位于本轮临时目录。测试 PostgreSQL 仅监听 127.0.0.1:21509；测试用量记录已全部回滚，确认剩余记录为 0，实例已停止且端口关闭，见 postgres/summary.json。浏览器使用独立配置，所有业务 API 被合成数据替代，外部请求被阻止。

### 9.4 验收、限制与回退

- 验收：启动包含本次修改的前后端，进入账号管理，查看“生图数量”的今日/累计张数；在“更多操作”隐藏并刷新，再重新显示。统计受现有 30 秒缓存影响，出图入账后在后续刷新更新。
- 累计值基于保留日志，日志被清理会减少；图片生成能力是否存在、上游剩余额度和站外历史不由该数字表示。历史用量很大时累计聚合可能较慢，本轮复用批量查询、可见列按需查询、缓存和超时控制，没有引入永久计数或索引迁移。
- 紧凑布局高度与前轮相同：合成样本 1440×900 完整显示 5 行，1280×720 完整显示 2 行；新增两行数字没有抬高样本行高。
- 回退仅撤销本轮相对备份的修改与新增文件，不撤销前序工作，不使用 reset/clean；没有数据迁移或业务数据写入。
- 建议 Conventional Commit：`feat(accounts): show per-account image generation counts`（未执行）。
