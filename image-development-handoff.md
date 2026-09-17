# new-api 图片功能开发交接

日期：2026-09-17。代码基线：`69a50029819a26c53e6babd276d49cfe2f8880ad`。

本次实现 A–F 的后端和页面，保留 new-api / QuantumNous 标识、原同步接口及旧任务契约。当前为可审查的本机源码及构建产物；没有执行正式部署、生产重启、付费上游请求或生产数据清理。外部服务和发布版升级的未验证项见下文，不能据此宣称所有验收已完成。

## 运行环境与文件位置

- 实际运行验收使用独立 PostgreSQL 数据库和真实 Redis，明确关闭 `MEMORY_CACHE_ENABLED`。没有在运行验收中使用 SQLite 或内存任务队列；SQLite 仅用于保留项目原有数据库兼容性的测试。
- PostgreSQL：18.0；Redis：8.0.2；SQLite 测试引擎：3.50.4。Go：1.26.8，代码语言基线按模块中的 1.25.1；前端使用 Bun。两个模块指定修复漏洞的 Go 1.26.8 工具链，Docker 构建阶段也更新至该版本及官方镜像摘要。
- 隔离数据库为 `new_api_image_test_20260916_140455`。测试还在该库内创建随机 schema，并使用随机 Redis 键前缀。数据库密码、Token、连接字符串不写入本交接文档。
- 默认文件根目录以**实际可执行文件所在目录**确定，分别为 `images/temporary` 和 `images/durable`。结果及输入对象 key 包含 `results/YYYY/MM/DD` 或 `inputs/YYYY/MM/DD`，对象身份和校验和保存在数据库中。
- 临时存储用于任务结果及 SC 输入；持久存储用于手工归档和投稿原图。临时转持久时复制对象，避免临时生命周期删除长期资产。
- 日期目录统一使用 UTC，保持重启及节点切换时的对象 key 稳定；任务页面日期按用户本地时区显示和筛选。
- 前端生产文件：`web/dist`。Windows 后端产物：`build/new-api-images.exe`，已嵌入生产前端。
- 冒烟修复后后端文件大小 166,707,200 字节，SHA-256：`670194B8466F25643BC1883F4AB9CC039ECE29FCE983DA3741113631FAC6329A`。
- 本机预览使用 `build/image-preview` 下的独立测试程序和图片文件。它只调用本机模拟上游，不代表厂商接口验收。
- 隔离预览程序在验收后已停止，仅删除了 `new-api:image-preview:<随机 UUID>:` 下的 7 个测试 Redis 键；未清空 Redis、修改生产业务数据或删除生产文件。测试数据库和图片证据保留以便复核。

## 页面与接口

| 页面 | 路径 |
|---|---|
| 图片工作台 | `/image-workbench` |
| 本机个人图库 | `/image-library` |
| 图片广场 | `/image-plaza` |
| 用户异步任务 | `/async-image-tasks` |
| 管理员异步任务 | `/admin/async-image-tasks` |
| 审核、举报、资产和清理 | `/admin/image-moderation` |
| 中英文站内 API 指南 | `/guide/async-image-api` |
| 图片运行、平台、渠道池和存储设置 | `/system-settings/operations/images` |

七条公共路径已接入：`POST /v1/images/generations_oa`、`POST /v1/images/edits_oa`、`POST /v1/chat/completions_gm`、`POST /v1/uploads/images_sc`、`POST /v1/images/generations_sc`、`GET /v1/images/tasks_async/:task_id`、`GET /v1/tasks_sc/:task_id`。管理接口沿用 `/api`，没有新增 `/api/v1` 别名。

## 已实现行为

1. 独立持久任务、事件、请求幂等、Outbox、上传意图、全量图片暂存、固定账单和日志收据。提交只鉴权、验证和持久受理；Worker 调度后调用上游，整批存储、结算及日志确认后才公布成功 URL。
2. CAS 版本、租约令牌、心跳、数据库恢复扫描和可重建 Redis 调度。上游可能已执行而结果未落库时进入 `execution_unknown`，不能自动重新生成。上传及结算恢复只处理已有产物。Worker 执行异常不会直接耗尽工作线程。
3. 固定保存 quota、资金来源、真实用量、定价事实和快照。零 Token 用量不会补占位 Token，按张表达式及旧单价仍按全部实际图片计算。主库事务幂等应用钱包/订阅、Token、用户/渠道计数和看板投影；独立关系型日志库使用唯一事件收据。ClickHouse 实现独立去重投影及 `FINAL` 查询，但未完成真实实例验收。
4. 本机存储及 AWS SDK v2 的 S3 兼容实现和厂商预设。配置版本不可变，旧对象按原配置解析。上传重试复用确定对象 key 并校验身份，访问时生成签名。Superbed 未实现。
5. SC 上传验证完整容器、声明 MIME、字节和像素；无效请求计次，先预留字节，签名别名有上限并保留墓碑。迟到上传及未确认孤立上传意图使用两次删除确认。
6. 分组/平台策略、Token 双平台映射及分辨率、模型、模型与分辨率三种渠道池。账号尝试保存不可用于登录的指纹及索引，支持持久 A/A/A/B 历史和 Gemini 切换预算。图片同步与异步使用不同熔断状态；原生 Gemini 图片请求也接入同步熔断。
7. 两个任务中心具有日期时区、分页、筛选、排序、完整筛选集合统计、事件、结果及管理员恢复/结束操作。批量操作逐项返回；终态不能被晚到 Worker 覆盖。
8. 工作台按服务端能力显示实时/异步及模型参数，提交前复核能力版本；网络重试复用请求快照和幂等 key。实时返回的全部图片保存在 IndexedDB Blob；异步仅预览和留在任务中心。
9. 本机图库每用户默认 30 天、100 张、200 MiB。用户及浏览器存储隔离；等待同步投稿原图有独立保留额度。服务器归档额度与本机图库额度是两套配置。
10. 延期投稿审核前仅提交元数据；批准后为 `approved_pending_sync`，原图 SHA-256、MIME、大小及容器验证并同步完成才公开。提示词默认私有。支持服务器已有资产投稿、举报、隐藏、恢复、作者撤回及引用保护清理。
11. 中英文完整 API 指南、动态 Base URL、可复制 curl 和响应示例、601–613 分类。新增 UI 文案覆盖 en、zh、zh-TW、fr、ja、ru、vi。

前端组合了现有 DataTablePage/useDataTable、StaticDataTable、CopyButton、ConfirmDialog、Dialog、SettingsCard 和移动筛选组件。多图预览兼容扩展现有 ImageDialog；IndexedDB Blob 模块补足现有文本存储无法保存图片原始字节和管理容量的能力。

## 启用方式

正式部署时继续使用现有 `.env` 中的 PostgreSQL 和 Redis 配置，并明确设置 `MEMORY_CACHE_ENABLED=false`。Redis 故障时异步受理和调度失败关闭，不回退到 SQLite 或内存队列。可通过 `REDIS_KEY_PREFIX` 隔离整套应用键，以及 `ASYNC_IMAGE_REDIS_PREFIX` 隔离图片调度键；已经上线的实例切换前缀须作为有计划的部署操作。

为新图片任务和本机签名访问配置稳定的 32 字节随机密钥：

```dotenv
ASYNC_IMAGE_ACTIVE_KEY_ID=images-1
ASYNC_IMAGE_PAYLOAD_KEYS=images-1:<32-byte-random-key-in-base64>
ASYNC_IMAGE_REDIS_PREFIX=new-api:images
MEMORY_CACHE_ENABLED=false
```

不要使用占位符或每次启动重新生成密钥。轮换时保留旧 key，直到使用它的请求、存储凭据及签名不再需要解析；多实例必须共享密钥。本机存储不需要 S3 连接。

在图片设置中检查两类本机存储根目录，按平台设置可用分组、模型能力及渠道池；在 Token 管理中配置 OpenAI / Gemini 图片分组映射，最后启用全局异步和对应平台异步。能力快照同时检查 Token、用户、平台、模型和分组资格。缺少密钥或 Redis 时拒绝新异步提交。

默认异步和自动归档关闭；Worker 4，租约 120 秒，总执行时限 1200 秒，输入保留 24 小时，任务/结果保留 90 天，签名 3600 秒。过期任务不可再通过公共接口查询，敏感请求和提示词按终态/保留规则清除；账务和幂等墓碑继续保留，避免旧请求重新执行。

本次首版按单实例验收。数据库租约和 CAS 支持多实例安全恢复，但多个节点直接使用各自本机目录不能视为共享文件存储；扩展实例时需共享存储或完成 S3 验收。

## 测试证据

所有日志、测试连接、预览凭据和截图在被忽略的 `build/` 中。以下是有效结果；较早的失败或被中止运行仍保留在其他日志中，未当作成功证据。

| 验证 | 命令/证据 | 结果 |
|---|---|---|
| 图片涉及的完整 Go 包 | `go test ./relay ./relay/channel/gemini ./model ./router ./service -count=1`；`build/image-go-feature-packages.log` | PASS |
| 零 Token 真实用量计费及最终安全用例 | `go test ./service -run TestAsyncImage -count=1`；`build/image-billing-and-security-final.log` | PASS |
| 独立 relaykit | 在 `relaykit/` 执行 `GOWORK=off go build ./...`；`build/image-relaykit-build-final.log` | PASS |
| PostgreSQL 钱包/日志故障重试、订阅、CAS | 设置仅指向测试库的 `ASYNC_IMAGE_TEST_PG_DSN` 后执行对应 `TestAsyncImage*`；`build/image-pg-tests.log` | PASS |
| 最新 PostgreSQL 图库/投稿/引用/孤立意图 | `go test ./model -run TestAsyncImageLibraryPublicationAndReferenceCleanup -v`；`build/image-pg-redis-verified-final.log` 中 model 结果 | PASS，248.30 秒 |
| 最新 PostgreSQL + Redis 七接口和恢复 | 设置测试 PG/Redis 环境后 `go test ./router -run TestAsyncImagePublicAdmissionRecoveryAndIsolation -v -count=1 -timeout 20m`；`build/image-api-pg-redis-verified.log` | PASS，469.59 秒 |
| 前端完整用例 | `bun run test --maxWorkers=2`；`build/image-web-all-tests-bounded.log` | 138 文件、1707 用例 PASS |
| 最新类型检查 | `bun run typecheck`；`build/image-web-typecheck-verified.log` | PASS |
| 新增/改动 UI lint | `bun run oxlint -c .oxlintrc.json` 对图片功能、Token 表单、设置注册、多图预览、导航和静态翻译键执行；`build/image-web-new-lint-verified.log` | 无诊断 |
| 全量 lint 对照 | `build/image-web-lint-final.log` 与原始快照 `build/image-web-lint-baseline.log` | 同样 261 条诊断，无新增差异；全量 lint 未通过 |
| 翻译同步 | 临时脚本补齐七语言后执行 `bun run i18n:sync`；检查新功能键，补齐浏览器发现的三个日期/搜索动态键 | PASS；临时脚本已删除 |
| 最新前端生产构建 | `bun run build`；`build/image-web-build-verified.log` | PASS |
| 后端构建 | `go build -o build/new-api-images.exe .` | PASS |
| 全量 Go | `go test ./... -count=1`；`build/image-go-all-verified.log` | FAIL，仅下述既有问题 |

真实 SQLite 与 PostgreSQL 测试使用新 schema、代表性用户/Token/渠道数据以及连续两次图片模型迁移，检查原数据、唯一约束和账务结果。该结果不等同于“最新发布版本完整数据库升级验收”。

全量 Go 的既有失败：`TestSecurityAccountDeletionConcurrentRequestsHaveOneWinner` 的 SQLite 并发锁冲突，以及 `TestUpstreamGetBody_HTTP2RetryAfterGracefulGoAway_PassThrough` / `TestUpstreamGetBody_HTTP2CannotRetryWithoutGetBody` 的 Windows TCP 连接中断语义。均在原始基线归档上复现，见 `build/image-baseline-platform-tests.log`。已修复两处既有测试运行环境问题：审计测试未关闭 SQLite 句柄，以及低精度 Windows 时钟生成的缓存测试 ID 碰撞；未改变原账号删除或 HTTP/2 业务逻辑。

## 2026-09-17 冒烟复查与修复

本轮对实际异步执行器、两种原生 HTTP 协议、固定账单、授权和依赖漏洞复查。上游是本机 HTTP 测试服务器，实际经过请求转换、上游执行、响应解析、图片校验、存储和真实主库结算；不是直接注入模拟账单。没有请求收费厂商。本轮没有更换 GORM、数据库驱动或修改生产连接。

修复六处问题：

1. 含 `image_count` 的表达式实际命中 Token 定价分支时，多图重复计算整批 Token 用量。现在整批计算一次；混合规格无法拆分聚合 Token 用量时拒绝虚构分摊。
2. 按张表达式先逐张取整导致整批费用偏高。现在累计实际规格的未取整金额，应用分组倍率后统一取整一次。
3. 实际 OpenAI 成功响应刷新时，异步响应收集器缺少 `http.Flusher` 而 panic。补齐刷新接口，原生双图响应可以正常解析并持久化。
4. 上游 HTTP 200 返回损坏 JSON 原先可能作为临时错误重试并再次生图。OpenAI / Gemini 均改为 `execution_unknown`、608；清除请求密文，不自动重新调用，不生成未经确认的账单。终态清除下一次尝试时间。
5. 捕获的预计价格非零且实际按 Token 收费时，缺失真实用量或 Gemini 本地估算用量原先可能产生零费用或估算账单。现在拒绝该账单并保留异常任务。明确按张收费及免费模型仍允许无 Token 用量，不补造 Token。
6. 管理员结束任务原先在事务外读取账单，与结算并发时可能把已扣费任务标成未扣费。现在与结算采用相同的“任务 → 账单”锁顺序，账单读取、终态 CAS 和事件同事务；已应用的账单保留 `settled` 及实际计费状态。

两处计费问题修复前已由精确断言复现：应扣 200 却扣 400、应扣 1 却扣 2，见 `build/image-smoke-billing-reproduced.log`。实际执行器刷新 panic 见 `build/image-smoke-native-reproduced.log`。这些失败日志保留，不列为通过证据。

| 本轮验证 | 证据 | 结果 |
|---|---|---|
| 最新图片 Go 包及终止并发回归 | `go test ./model ./router ./service ./relay ./relay/channel/openai ./relay/channel/gemini -count=1`；`build/image-smoke-go-feature-verified.log` | PASS |
| 按张 0.1 × 两张 = 0.2 | `go test ./service -run TestAsyncImageFixedBillMixedSpecificationsAndUsageBoundaries -v -count=1`；`build/image-billing-two-images-unit-price.log` | PASS；旧单价和按张表达式均扣 100,000 quota，倍率 1 |
| 图片涉及包静态检查 | `go vet ./model ./router ./service ./relay`；`build/image-smoke-vet-verified.log` | PASS |
| 独立 relaykit | `GOWORK=off go build ./...`；`build/image-smoke-relaykit-build.log` | PASS |
| PostgreSQL 钱包、日志故障重试、订阅、终态 CAS | `build/image-smoke-accounting-pg.log` | PASS |
| PostgreSQL 并发重复结算 | `build/image-smoke-concurrent-ledger-pg-final.log` | PASS，重复请求只扣一次 |
| PostgreSQL＋Redis 两协议真实执行器及损坏响应 | `build/image-smoke-native-pg-redis-patched-final.log` | PASS，754.12 秒 |
| PostgreSQL 管理员结束与结算并发 | `go test ./model -run TestAsyncImageBillAtomicSettlementAndLogRetry -v -count=1 -timeout 20m`；`build/image-smoke-termination-pg-final.log` | PASS，259.23 秒 |
| PostgreSQL＋Redis 两协议聚合 Token / 缺失用量 | 设置隔离 PG/Redis 连接后 `go test ./router -run TestAsyncImagePublicAdmissionRecoveryAndIsolation -v -count=1 -timeout 20m`；`build/image-smoke-native-token-pg-redis-final.log` | PASS，740.91 秒；两协议共八次本机原生 HTTP 调用 |
| 安全更新后前端完整测试 | `bun run test --maxWorkers=2`；`build/image-smoke-web-all-tests.log` | 138 文件、1707 用例 PASS |
| 类型检查、生产构建 | `build/image-smoke-web-types-final.log`、`build/image-smoke-web-build-final.log` | PASS；生产构建 39.6 秒 |
| 最新后端嵌入前端构建 | `go build -o build/new-api-images.exe .`；`build/image-smoke-backend-build-final.log` | PASS |
| 全量 lint | `build/image-smoke-web-lint-final.log` 对比 `build/image-web-lint-baseline.log` | 同样 261 条诊断，逐条比较无新增差异；未通过 |
| 全量 Go | `go test -p 4 ./... -count=1`；`build/image-smoke-go-all-final.log` | 仍仅上述三项基线失败；终止事务修复后相关包另行全部通过 |

原生协议测试同时核对实际图片和钱包/Token 余额：按张模型在存储失败后改价并恢复，仍仅结算原先固定的双图 10,000 quota；重复调度不再次调用上游。Token 表达式 `p * 2 + c * 8` 接收实际输入 10、输出 20 时，两张图片整批扣 90 quota，OpenAI 和 Gemini 各一次，共 180。缺少上游真实用量和损坏 JSON 的四个异常任务不扣费，不产生可执行的重试；免费/按张模型没有被这项保护误拒绝。并发账本测试还覆盖 Token 不足时钱包事务回滚、软删除 Token 的旧任务结算、订阅幂等、独立日志库故障及唯一日志收据。

“只结算一次”指同一任务账单幂等应用一次，金额包含全部实际图片，不是只收费一张。例如按张单价 0.1、两张、分组倍率 1，总费用为 0.2；默认每单位 500,000 quota 对应扣 100,000 quota。旧按张单价与 `fixed(0.1) * image_count` 表达式均有这一精确回归用例。上面的双图 10,000 quota 使用的是 0.01/张，即 0.02 总价；存储重试只防止同一笔 0.02 被重复扣款。

依赖安全扫描与更新：

- `go run golang.org/x/vuln/cmd/govulncheck@latest .` 初次有 12 项依赖调用路径命中。升级 Go 1.26.8、`x/image` 0.45.0、`x/text` 0.41.0、gorilla/websocket 1.5.3、AWS S3 1.97.3 及必要的传递依赖后，该源码扫描的依赖调用路径命中为 **0**，导入包命中为 **0**。`build/image-smoke-vulnerabilities-go-final.log` 仍记录 **7 项未被本项目调用的所需模块告警**，不能称所有依赖零漏洞，也不能用源码扫描代替项目自身漏洞核对。
- 补充 `govulncheck -mode=binary build/new-api-images.exe` **未通过**，见 `build/image-smoke-vulnerabilities-binary-final.log`。它按未发布工作区的 `v0.0.0-…+dirty` 版本和包级范围匹配出 13 项 new-api 历史告警，并把本机 `./relaykit` 替换路径也列入同一批告警。逐项下载官方漏洞数据库记录：其中 8 项的全部参考修复提交已经是当前 HEAD 的祖先，见 `build/image-smoke-own-advisory-ancestry.json`；其余 5 项核对当前源码已有 Stripe 空密钥拒绝/已付款检查、未指定 IP 拒绝、Token 搜索模式限制、Markdown 的 DOMPurify 清洗、下载实际拨号及重定向保护。没有修改模块身份、伪造发布版本或屏蔽扫描结果。历史告警的源码核对不等于全站漏洞审计通过。
- 官方参考：[Go 漏洞检测](https://go.dev/doc/security/vuln/)、[图片解码资源耗尽](https://pkg.go.dev/vuln/GO-2026-6222)、[AWS SDK](https://pkg.go.dev/vuln/GO-2026-5764)、[WebSocket](https://pkg.go.dev/vuln/GO-2026-6278)、[文本处理](https://pkg.go.dev/vuln/GO-2026-5970)。
- 前端与原始基线均有 53 条相同的依赖告警，没有新增告警。`bun audit fix` 在依赖允许范围内修复 16 条，升级九个包并完成全套前端测试和生产构建。最终仍有 **37 条既有告警（12 high / 23 moderate / 2 low）**，涉及 brace-expansion、decode-uri-component、fast-uri、hono、ip-address、mermaid、postcss、qs；上游精确版本或版本范围阻止直接修复，未强制覆盖这些依赖契约。见 `build/image-smoke-vulnerabilities-web-final.json` 及 `build/image-smoke-security-upgrades-web.log`。告警不全是图片运行路径，但仍是待处理安全缺口。
- 本机没有 C 编译器，未执行 Go race detector；真实 PostgreSQL 并发账本测试不能替代完整 race 检测。未构建 Docker 镜像。

历史告警的额外回归见 `build/image-smoke-historical-controls.log`：真实拨号/重定向保护、Stripe 配置拒绝及 Token 查询密钥屏蔽的已有用例通过；扩展现有 `common/ssrf_protection_test.go` 验证 `0.0.0.0`、`0.1.2.3`、`::` 均被拒绝。该日志明确显示 middleware/model 无匹配用例，未把这些包的空测试计为安全控制验证。Token 搜索及 Markdown 清洗的其余部分为源码核对，没有宣称新做了它们的完整攻击链验收。

## 浏览器证据

使用真实测试 PostgreSQL / Redis、同源生产前端和本机模拟上游完成：异步两图成功及轮询、左右键多图预览；实时两图自动进入本机图库而异步不自动保存；延期投稿批准后广场仍空；原 Blob 同步后公开；提示词私有；举报及管理员处理；隐藏后不可公开、恢复后可见、作者撤回后不可见；清理预览返回 0 且未创建删除任务；两个任务中心、日期键盘筛选及完整集合统计；中文/英文指南及复制出的动态 Base URL 和合法多行 curl；390×844 指南/图库/任务筛选与折叠。

截图位于 `build/image-evidence/`：`workbench-async.png`、`workbench-async-final.png`、`lightbox-keyboard.png`、`plaza-private-prompt.png`、`cleanup-preview.png`、`user-tasks.png`、`admin-tasks.png`、`guide-zh.png`、`guide-en.png`、`guide-mobile.png`、`library-mobile.png`、`tasks-mobile.png`。最后一轮两图任务为 `asyncimg_a4fa0ade374846df93c492db0a0504fa`，已完成 100% 并显示两图。浏览器临时尺寸已恢复，语言已恢复中文。

## 授权与图片安全边界

按 [ASVS 5.0.0 V8](https://github.com/OWASP/ASVS/blob/v5.0.0/5.0/en/0x17-V8-Authorization.md) 的 8.2.1（功能权限）、8.2.2（对象权限）、8.2.3（字段权限）、8.3.1（服务端执行）、8.3.2（权限变更及时生效）实施和验证相关边界：公共任务同 Token 查询；站内本人全部 Token；管理员资源权限；SC 输入归属；私有提示词；最新 Token/用户状态、过期和 IP 限制，即使旧用户缓存仍存在也拒绝禁用用户。查询可用额度耗尽 Token 的既有任务；零价任务不会因为余额为零被误拒绝，付费任务仍检查额度。

参照 [Authentication Cheat Sheet](https://cheatsheetseries.owasp.org/cheatsheets/Authentication_Cheat_Sheet.html)、[Session Management Cheat Sheet](https://cheatsheetseries.owasp.org/cheatsheets/Session_Management_Cheat_Sheet.html)、[SSRF 指南](https://cheatsheetseries.owasp.org/cheatsheets/Server_Side_Request_Forgery_Prevention_Cheat_Sheet.html)及[上传指南](https://cheatsheetseries.owasp.org/cheatsheets/File_Upload_Cheat_Sheet.html)。安全用例覆盖同 Token 幂等/跨 Token 拒绝、禁用/过期/IP/陈旧缓存、密文关联/轮换/缺密钥、清理预览的操作者/范围/期限/篡改、混合公网私网 DNS 拒绝、固定实际连接 IP 和对端地址核对、重定向规则、MIME/完整容器/字节/像素限制。

DNS/连接与重定向回调使用确定性模拟依赖测试，不冒充真实互联网 TLS 重定向全链验证。这些是相关控制的证据，不是全站 ASVS 合规认证。

## 尚未完成的外部验收

- **PENDING：真实厂商上游**。尚未指定允许实际生图的测试 Token，未调用收费厂商，厂商费用为 0；本机模拟器只证明本项目流程。
- **PENDING：S3 与 ClickHouse**。实现已构建及代码路径测试，未有指定实例的真实读写/删除/投影验证。本次实际图片验收采用本机目录和关系型日志库。
- **PENDING：最新发布版完整升级及最低支持版本矩阵**。已验证代表性基线数据和图片迁移重复执行；尚未完成真实发布版完整 schema/data 的升级与启动两次。当前 PG 18.0 不替代 PG 9.6 等最低版本验收。
- **MySQL：按用户最新指示不作为本次运行环境，不索取连接**。保留可移植 GORM 实现，但没有实际 MySQL 验证，不能宣称完整三数据库兼容性矩阵通过。
- **既有全量检查问题**：上述全量 Go 失败和 261 条基线 lint 诊断仍存在，不用单独功能测试通过掩盖。

正式部署、重启、生产存储清理、真实数据搬迁，以及视频、社交互动、个人图库跨设备同步、新支付/认证体系均不在本次执行范围。交接材料不包含生产秘密。
