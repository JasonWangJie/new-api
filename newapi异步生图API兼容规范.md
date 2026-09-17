# new-api 异步生图 API 兼容规范

> 来源代码基线：`defccfacba610450ac5f9fd06e0f54c2f25fbb7a`，VERSION `0.1.173.49`；提取日期 2026-09-16。本规范用于 new-api 重新实现当前协议。示例域名 `https://api.example.com` 必须替换为目标站点公开地址，不能带入原项目生产域名。

配套：[开发总纲](newapi图片功能迁移开发总纲.md)、[页面与图库广场规格](newapi图片页面与图库广场功能规格.md)、[Codex 任务书](newapi图片功能Codex开发任务书.md)。

## 1. 固定路径与兼容边界

BB/SC 是下游请求方言，不是上游供应商；平台由提交路径确定。`_gm/_sc` 调用 Gemini，`_oa` 调用 OpenAI。

| 方言 | 场景 | 方法与路径 | 成功 HTTP |
|---|---|---|---:|
| BB/OpenAI | 文生图，带有效参考图自动进入编辑 | `POST /v1/images/generations_oa` | 202 |
| BB/OpenAI | 明确图生图 | `POST /v1/images/edits_oa` | 202 |
| BB/Gemini | Chat Completions 风格文生图/图生图 | `POST /v1/chat/completions_gm` | 202 |
| SC/Gemini | 上传一张参考图 | `POST /v1/uploads/images_sc` | 200 |
| SC/Gemini | JSON 风格文生图/图生图 | `POST /v1/images/generations_sc` | 202 |
| 共用 | 查询任何上述任务 | `GET /v1/images/tasks_async/{task_id}` | 200 |
| 兼容别名 | 同一查询格式 | `GET /v1/tasks_sc/{task_id}` | 200 |

不提供省略 `/v1` 的别名，不让部署配置改协议路径。所有提交 `Location/query_url` 指向统一 `tasks_async`，SC 别名保留读取兼容。

原 `/v1/images/generations`、`/v1/images/edits`、`/v1/chat/completions` 和 Gemini 原生端点保持其原有实时格式；新开关不把它们改成任务响应。new-api 已有 `/v1/tasks/:key`、视频、Midjourney/插件任务也保持独立。来源另有旧 Redis `/images/generations/async` 等接口，不属于本开发包必迁范围，不能用它们替代新的持久协议。

## 2. 通用约定

### 2.1 启用与鉴权

```http
Authorization: Bearer <API_KEY>
Content-Type: application/json
Idempotency-Key: <CLIENT_OPERATION_ID>
```

必须为有效 Token，用户/模型/IP 等原有权限成立，可解析路径平台对应计费分组，该组图片与异步开关同时打开，持久化、加密配置、图片存储可用，资格/额度检查成立。新异步开关默认关闭。不开启返回 403，不回退实时。

公共查询必须用**提交任务的同一个 Token**；即便同用户的另一个 Token 也返回 404。站内登录任务中心另按用户所有权可看本人全部 Token，管理员可看全站。

一 Token 可以显式映射 openai/gemini 两个分组，映射优先于主分组回退；聊天和原同步请求保留目标已有分组逻辑。不能通过 body 的模型名猜平台或绕过映射与权限。

### 2.2 受理、轮询与 URL

提交返回 HTTP 202 表示 task/event/outbox 已持久化，不表示已经调用上游、结算或成功。

```http
Cache-Control: no-store
Location: https://api.example.com/v1/images/tasks_async/asyncimg_0123456789abcdef
Retry-After: 3
```

`query_url` 的根是网关站点/API 公开地址，**不是 OSS/CDN 根**。留空时可返回 `/v1/...` 相对查询地址。处理中查询仍 no-store，带 Retry-After 3；客户端优先遵守头部，采用 3～30 秒退避和业务总等待上限。

HTTP 200 仅表示查询成功。图片生成成功取决于 `status=succeeded`；处理期间可能已生成图片但仍在上传/结算。

任务 ID 不透明、随机，前缀 `asyncimg_`，不能使用顺序数据库 ID。成功图片 URL 每次按当前存储生成：私有签名默认 **3600 秒**，公开 CDN 可以稳定；输入默认保留 24 小时，任务/结果默认保留 90 天。这三种时限不能混为“图片统一一天有效”。公共结果项当前只含 url，不强行新增必需 expires_at 字段。

### 2.3 提交幂等

`Idempotency-Key` 可选，最多 255 字节，作用域为 Token + key。指纹来源为：

```text
SHA256(trim(platform) + NUL + trim(dialect) + NUL + trim(source_path) + NUL + raw_body)
```

| 操作 | 行为 |
|---|---|
| 同 Token、同 key、同指纹 | 返回首次 task，仍是受理格式，不重复调用/计费 |
| 同 key、不同路径/方言/原始 body | 409 |
| 无 key | 每次新建 task |
| JSON 调整空白/顺序后原 key 重试 | 可能 409，必须重发原始字节 |
| multipart 更换 boundary 后原 key 重试 | 指纹不同，必须复用同一序列化 body/boundary |

提交幂等不等于上游幂等，不保证 unknown 任务可以重跑；不等于账务幂等，两层都必须实现。

## 3. OpenAI 文生图与图生图

### 3.1 generations 自动分流

`POST /v1/images/generations_oa` 支持 JSON 与 multipart。

| 输入 | 行为 |
|---|---|
| 不传 image_urls / null / [] / 无有效非空 URL | 文生图 |
| image_urls 存在有效值 | 图生图 |
| multipart 有 image 文件 | 图生图 |
| 明确 edits_oa 没有参考输入 | 参数错误 |

支持新版 `image_urls:string[]`，兼容原生 `images[].image_url` 对象数组；不支持 `images[].file_id`。前端推荐统一 image_urls 或重复 image 文件，不混合不明输入格式。mask URL 使用 `mask.image_url`；multipart mask 按原生适配能力支持。

### 3.2 参数表

| 字段 | 类型 | 推荐必填 | 规则 |
|---|---|---|---|
| model | string | 是 | 图片模型，如 gpt-image-2，仍受 Token/分组/渠道模型权限约束；来源解析器省略时默认 gpt-image-2，目标如实现默认兼容须保留权限检查 |
| prompt | string | 是 | 非空提示词/编辑指令 |
| image_urls | string[] | 图生图 JSON | 有效参考 URL；数量受全局/模型/上游约束 |
| images[].image_url | string | 否 | 原生 URL 对象数组兼容；file_id 不支持 |
| image | file，可重复 | 图生图 multipart | 字段 image，不是 SC 上传的 file |
| n | integer | 否 | 来源原生默认 1，遵守渠道与模型张数能力 |
| resolution | string | 否 | 推荐 1K/2K/4K；兼容 auto 按实际结果；非法别名报错 |
| aspect_ratio | string | 否 | auto、1:1、2:3、3:2、4:5、5:4、4:3、3:4、16:9、9:16、21:9、9:21、2:1、1:2 |
| size | string | 否 | 原生 WxH/auto，或档位/比例兼容别名，优先级见下 |
| quality | string | 否 | 由模型能力决定，不全局硬编码统一质量枚举 |
| background / output_format | string | 否 | 原生背景与 png/jpeg/webp 等格式，受模型支持约束 |
| output_compression / moderation / style / input_fidelity 等 | 原生类型 | 否 | 原链支持时保留，不在异步包装中丢失 |
| mask.image_url | string | 否 | 编辑遮罩 URL，仅有参考图的编辑有效 |
| response_format | string | 否 | 原执行链兼容；持久异步最终统一 URL，不原样返回上游 b64 |
| stream | boolean | 否 | false 或省略；true 拒绝 |

新模型目录不锁死单个 ID；来源允许 `gpt-image-*`，目录包含 `gpt-image-2.5-flare/sunburst`。这是来源目录快照，不表示任何目标渠道一定支持。模型级 qualities 由能力返回，新模型扩展质量不能发送给旧模型。

### 3.3 尺寸处理的精确规则

1. resolution/aspect_ratio 严格校验；显式 aspect_ratio 优先于 size 中的比例别名。
2. size=比例时，转 aspect_ratio；size=档位时在 resolution 为空才填入。
3. 单独 aspect_ratio 且没有原生 size/resolution，来源报 resolution required，不可随意补 1K。
4. 原生 size=WxH 是上游尺寸、调度和计费权威。附带非 auto resolution 必须与该 WxH 的原生档位相容，否则 400。
5. 原生 size=auto 保留 auto，最终按实际产物档位；不能用附带 resolution 降档。
6. 仅别名 resolution + aspect_ratio 才进行下表映射；省略比例默认为 1:1，auto 比例映射 auto。
7. 原生未知 size 且未附带别名时，来源允许上游处理；附带别名造成歧义则拒绝。兼容实现不能用别名白名单封死原生未来尺寸。
8. 转发时移除包装层 resolution/aspect_ratio，保留转换后的原生 size 与其他参数。保留别名来源标志供计费，不能仅看改写后的长边抬档。

| 比例 | 1K size | 2K size | 4K size |
|---|---|---|---|
| 1:1 | 1024x1024 | 2048x2048 | 4096x4096 |
| 3:2、16:9 | 1536x1024 | 2048x1152 | 4096x2304 |
| 2:3、9:16 | 1024x1536 | 1152x2048 | 2304x4096 |
| 5:4 | 1280x1024 | 2048x1632 | 4096x3272 |
| 4:5 | 1024x1280 | 1632x2048 | 3272x4096 |
| 4:3 | 1360x1024 | 2048x1536 | 4096x3072 |
| 3:4 | 1024x1360 | 1536x2048 | 3072x4096 |
| 21:9 | 2384x1024 | 2048x880 | 4096x1752 |
| 9:21 | 1024x2384 | 880x2048 | 1752x4096 |
| 2:1 | 2048x1024 | 2048x1024 | 4096x2048 |
| 1:2 | 1024x2048 | 1024x2048 | 2048x4096 |
| auto | auto | auto | auto |

这是来源兼容转换表，不声称等同上游原生比例。比如 1K 的 16:9 与 3:2 共用 1536x1024；迁移时不要改为自算数学比例而破坏旧客户端规格。

### 3.4 可复制示例

文生图 body：

```json
{
  "model": "gpt-image-2",
  "prompt": "一只在沙滩上的猫，写实风格",
  "n": 1,
  "resolution": "1K",
  "aspect_ratio": "3:2",
  "quality": "high",
  "stream": false
}
```

图生图 JSON body，generations_oa 自动分流或 edits_oa 明确编辑：

```json
{
  "model": "gpt-image-2",
  "prompt": "保留主体，把背景换成夜景",
  "image_urls": ["https://cdn.example.com/reference.png"],
  "resolution": "1K",
  "aspect_ratio": "1:1",
  "output_format": "webp"
}
```

一次 multipart 编辑示例；自动化重发需要缓存本次实际 body 和 boundary，不能直接再次运行此命令假定幂等命中：

```bash
curl 'https://api.example.com/v1/images/edits_oa' \
  -H 'Authorization: Bearer YOUR_API_KEY' \
  -H 'Idempotency-Key: edit-001' \
  -F 'model=gpt-image-2' \
  -F 'prompt=把背景换成夜景' \
  -F 'resolution=1K' \
  -F 'aspect_ratio=1:1' \
  -F 'image=@reference.png'
```

OpenAI 受理响应：

```json
{
  "task_id": "asyncimg_0123456789abcdef",
  "query_url": "https://api.example.com/v1/images/tasks_async/asyncimg_0123456789abcdef"
}
```

## 4. Gemini BB

路径 `POST /v1/chat/completions_gm`，JSON 请求。

| 字段 | 规则 |
|---|---|
| model | 必填，权限/映射检查仍执行 |
| stream | false 或省略；true 拒绝 |
| messages | 至少一条，来源只接受 role=user |
| content | 非空字符串，或非空 text/image_url 数组；整个请求至少一个非空文本提示 |
| extra_body.google.image_config.image_size | 可省略、1K/2K/4K；0.5K 需模型精确/末尾 * 前缀规则显式允许 |
| extra_body.google.image_config.aspect_ratio | 可省略、auto/自动、1:1、2:3、3:2、4:5、5:4、4:3、3:4、16:9、9:16、21:9、9:21 |

保持多条 user 消息和数组中的文本/图片顺序，文本按原规范汇总，不把图 1/图 2 重排。兼容外层字段不代表 max_tokens 等会进入当前规范化图片调用，应以实际适配器支持为准。

文生图：

```json
{
  "model": "gemini-3-pro-image-preview",
  "stream": false,
  "messages": [{"role": "user", "content": "现代客厅，北欧风，自然光"}],
  "extra_body": {"google": {"image_config": {"image_size": "2K", "aspect_ratio": "16:9"}}}
}
```

图生图：

```json
{
  "model": "gemini-3-pro-image-preview",
  "stream": false,
  "messages": [{
    "role": "user",
    "content": [
      {"type": "image_url", "image_url": {"url": "https://cdn.example.com/reference.png"}},
      {"type": "text", "text": "保留构图，把场景改成夜景"}
    ]
  }],
  "extra_body": {"google": {"image_config": {"image_size": "4K", "aspect_ratio": "auto"}}}
}
```

受理响应与 OpenAI/SC 有兼容性差别，保留以下额外字段：

```json
{
  "id": "asyncimg_0123456789abcdef",
  "task_id": "asyncimg_0123456789abcdef",
  "object": "image.task",
  "status": "queued",
  "query_url": "https://api.example.com/v1/images/tasks_async/asyncimg_0123456789abcdef"
}
```

Gemini 强制非流式，`responseModalities=[TEXT,IMAGE]`，image_size/aspect_ratio 转 generationConfig.imageConfig。HTTPS 转 fileData.fileUri，内联 data URI 完整校验后转 inlineData。passthrough/local/混合由后端策略决定。返回全部 candidates/parts 中图片进入同一个任务，不能只保留首图。

## 5. Gemini SC

### 5.1 提交

`POST /v1/images/generations_sc` 是 JSON。SC 仅 Gemini，不能当 OpenAI 原生尺寸转换入口。

| 字段 | 类型 | 规则 |
|---|---|---|
| model / prompt | string | 均为非空必填 |
| image_urls | string[] | 省略/null/[]为文生图，非空为图生图；**元素不能空字符串**，不同于 OpenAI 无效值分流 |
| resolution | string | 可省略、1K/2K/4K，0.5K 显式允许模型例外 |
| aspect_ratio | string | 显式时优先于 size 的比例别名 |
| size | string | 比例/auto/自动，或 WxH 推导支持比例；未传 resolution 时档位别名 2K 可填入 |

SC 当前解析结构不定义 stream/n/quality 等字段；未知 JSON 字段不因此成为已支持参数。不能声称 SC stream=true 已有独立 400 校验，也不能提供 SC 流式或批量张数功能。工作台不发送这些参数，执行链固定非流式。

支持比例与 BB Gemini 相同。WxH 只选比例、不保证精确像素：1080x1350 → 4:5；2520x1080 → 21:9；反向 → 9:21。比例可约分，非法比例/档位 400。auto/自动 在文生图和图生图均通过省略上游比例实现。

```json
{
  "model": "gemini-3-pro-image-preview",
  "prompt": "现代客厅，北欧风，自然光",
  "resolution": "2K",
  "size": "16:9"
}
```

```json
{
  "model": "gemini-3-pro-image-preview",
  "prompt": "保留图1主体，参考图2的自然光和色调",
  "image_urls": ["https://cdn.example.com/ref1.png", "https://cdn.example.com/ref2.jpg"],
  "resolution": "4K",
  "size": "1080x1350"
}
```

受理 HTTP 202，body 与 OpenAI 完全相同，仅 task_id/query_url。查询也与 BB 共用格式。

### 5.2 SC 上传参考图

上传是独立步骤，图生图已有安全 HTTPS URL 时可以直接提交，不必重复上传。

```bash
curl 'https://api.example.com/v1/uploads/images_sc' \
  -H 'Authorization: Bearer YOUR_API_KEY' \
  -H 'Idempotency-Key: upload-reference-001' \
  -F 'file=@reference.png'
```

成功 HTTP 200：

```json
{
  "url": "https://storage.example.com/inputs/2026/09/16/reference.png",
  "filename": "reference.png",
  "content_type": "image/png",
  "bytes": 204800,
  "created_at": 1789552800
}
```

created_at 是实际 Unix 秒；上面的对象路径仅示例，真实 key 服务端生成。返回的 url 可用于后续 image_urls，当前 Token 绑定有效输入，任务正在引用时不能提前清理。

### 5.3 上传 admission、幂等与恢复

1. 鉴权/分组/配置成功后，**body 解析前**事务锁定 Token，检查准确 60 秒滚动尝试数并写 attempt，失败/非法 body 也计频率。
2. 有界读取单个 file；完整图片解码/写存储前，第二阶段消费 admission，检查 Token 级幂等与总字节 reservation。
3. 额度统计有效输入、活跃 reservation、未清理失败/stale intent，未完成删除不能释放字节；主库不可用返回 503，不能改单实例内存限额。
4. 完整校验后先保存 deterministic intent，实际 MIME 扩展名，客户端文件名不入 key；写入超时受控，返回存储身份与 intent 一致才完成输入登记/URL hash。
5. 同 Token/key/同上传指纹且输入仍有效：重签同对象、不重新 PUT，`X-Idempotency-Replayed:true`，登记新 URL SHA-256 alias。
6. 同 key 不同文件/声明 MIME/净化文件名/字节等指纹：409 conflict；正在处理 409 in_progress，Retry-After 60；已过期/清理墓碑 409 result_unavailable，新上传必须换 key。
7. 每对象最多 128 alias，行锁定后去重，相同 hash 可延长有效期，第129个新 hash 429 alias_limit；过期 alias 仍留所有权墓碑至输入删除。
8. 已知其他 Token、过期或清理中的 alias 拒绝，不能当普通公网图片继续透传。
9. 失败/stale intent 由维护 claim 精确 Delete：首次成功保留 intent和删除时间，至少十分钟后第二次 Delete 成功再释放 reservation，收敛迟到 PUT。
10. 文件名统一 slash 后取 basename，去控制字符/首尾空白，限255字节，空名按真实 MIME 默认 image.png/.jpg/.webp。

| 配置 | 默认 | 来源强制最大 |
|---|---:|---:|
| upload_per_minute | 20 | 1000 |
| max_input_bytes_per_key | 1 GiB | 100 GiB |
| download_max_bytes | 32 MiB | 64 MiB |
| upload_timeout_seconds | 300 | 600，短于 reservation lease |
| input_retention_hours | 24 | 720 |

超大 body/图片 413；字节额度不足 409 `async_image_upload_byte_quota`；限频429 `async_image_upload_rate_limited`，Retry-After60；幂等冲突/处理中/墓碑分别 `async_image_upload_idempotency_conflict`、`async_image_upload_in_progress`、`async_image_upload_result_unavailable`；alias上限 `async_image_upload_alias_limit`。

## 6. 共用任务查询

```http
GET /v1/images/tasks_async/asyncimg_0123456789abcdef
Authorization: Bearer <提交任务的同一个 API_KEY>
```

SC 查询别名响应相同，不能因提交方言不同返回假404。所有成功查询含 no-store。

排队（HTTP200，Retry-After3）：

```json
{"status":"queued","task_id":"asyncimg_0123456789abcdef"}
```

处理（HTTP200，Retry-After3）：

```json
{"status":"processing","task_id":"asyncimg_0123456789abcdef"}
```

成功（HTTP200，只有存储清单和账务/日志均确认后）：

```json
{
  "status": "succeeded",
  "task_id": "asyncimg_0123456789abcdef",
  "data": [{"url":"https://cdn.example.com/results/output-1.png"}]
}
```

失败（仍HTTP200）：

```json
{
  "status": "failed",
  "task_id": "asyncimg_0123456789abcdef",
  "error_code": 611,
  "fail_reason": "上游生图失败（HTTP 400）：image: at most 8 images are allowed"
}
```

公共响应不包含最终渠道、上游账号、内部 provider/bucket/key、请求体、账单命令、管理员参考 URL 列表。fail_reason 保留**已有脱敏和长度限制内**的上游提示，不能泄露凭证或用固定中文概述替换全部原文。

内部 invoking 尚未落库最终渠道显示 queued；重试 queued 清当前渠道，保留尝试审计。上传/账务失败未耗尽预算继续 processing，耗尽failed；execution_unknown/expired 对外failed。成功后临时签名解析不可用，查询本身可503 storage_unavailable，不能改变已成功任务或重新生图。

## 7. 失败应用码 601–613

这些整数是任务查询 body 的应用层分类，**不是 HTTP 6xx**。站内任务 API 的 error_code 仍可为内部字符串，前端不能把两个层级混为同一枚举。

| code | 含义 | 客户端建议 |
|---:|---|---|
| 601 | 内容安全/政策/第三方相似性拦截 | 展示原提示，修改输入后新提交 |
| 602 | 参考图 URL 网络拉取失败，DNS/TLS/超时/重置等 | 检查 HTTPS/CDN，必要时上传，避免无限重复提交 |
| 603 | 账号/渠道或上游容量耗尽 | 稍后重试并排查可用容量 |
| 604 | 参数/提示/尺寸/格式/参考图输入无效、资格变更等 | 按原提示修正 |
| 605 | 上游429限流 | 退避 |
| 606 | 上游临时不可用/网关5xx | 保留 task ID，稍后操作 |
| 607 | 图片输出无法解析 | 排查模型/响应格式 |
| 608 | 执行超时或未知结果 | 不自动再提交，先人工对账 |
| 609 | 存储/结算后处理失败 | 只恢复后处理，不重新生成 |
| 610 | 未分类兜底 | 展示原文和 task ID，不能因此自动重提 |
| 611 | 参考图超过上游数量限制，当前已见上游最多8张 | 减少到允许数量后新提交 |
| 612 | 缺少/未检测到/无法使用所需参考图 | 重新上传清晰完整参考图 |
| 613 | 提示词或输入图片无法被上游处理 | 简化提示，检查图片格式和内容 |

分类实现需区分内部确定性代码与上游正文样本，优先保留内容政策与网络拉取语义，避免“请上传”等短语抢占已有政策/网络分类。新增样本至少包括 `image: at most 8 images are allowed`、中文“请上传/未检测到参考图”、`prompt or input images could not be processed`。真正未知保留610，不把所有HTTP400都强行归604。

参考数量还有本地准入限制：来源全局默认8；已知 Gemini Flash Image 模型能力3、Pro Image能力14，最终取模型上限与全局较小值，未知模型用全局。超过已知本地上限可在提交阶段400 `too_many_reference_images_for_model`；**提交时的400不是任务查询611**，611用于已经受理任务的上游失败分类。

## 8. 请求本身的错误

BB/OpenAI/SC 当前统一：

```json
{
  "error": {
    "type": "async_image_generation_disabled",
    "code": "async_image_generation_disabled",
    "message": "asynchronous image generation is not enabled for this group"
  }
}
```

| HTTP | 场景 |
|---:|---|
| 400 | 无效JSON/multipart、缺必要输入、BB/OpenAI stream=true、别名尺寸/比例非法、已知模型参考数超限 |
| 401 | Token鉴权失败 |
| 403 | 平台/分组权限不满足、生图/异步关闭 |
| 404 | task不存在或公共查询Token不属于提交者 |
| 409 | 提交/上传幂等冲突、上传处理中/墓碑、输入字节额度不足 |
| 413 | body/图片资源超限 |
| 429 | SC本地上传限频/alias上限或原资格限流 |
| 503 | 主库 admission、存储、加密或运行配置不可用 |

上游执行期间的400/429/5xx/超时通常落为受理任务失败，由查询HTTP200 + 应用码返回，不能把 Worker上游HTTP直接当提交响应。原目标鉴权中间件的既有错误格式如果有差异，要在新固定协议边界适配；不全局改旧接口错误。

## 9. 安全与客户端恢复规则

- HTTPS公网URL及受限data URI；SC上传/本地下载/import严格PNG/JPEG/WebP校验，声明MIME、魔数、完整容器、字节、像素一致，拒SVG/HTML/脚本/polyglot/尾随载荷。
- 参考图全局默认8、累计64MiB/80MP，单图下载默认32MiB/80MP；计数先于下载/解码。
- 请求跨持久层加密，不保存原始下游API Key；终态清完整request_payload，保存指纹、脱敏摘要、必要审计。
- 新任务 remote reference_image_urls 按请求顺序保存，trim过滤HTTP(S)，重复保留，内联不保存，历史不回填；仅管理员**单任务详情**返回，公共/用户/管理员列表不返回。存审计HTTP(S)并不意味着执行准入允许普通HTTP。
- 网络提交结果未知只能用原body+原key重发确认task；改body要新操作key。不能自动切到实时/另一平台。
- 用户停止等待/页面卸载只停止客户端请求，已受理task在服务端继续。
- storage_failed/billing_failed允许管理员resume后处理；execution_unknown禁止resume；管理员结束不等于上游取消/退款。

## 10. 站内异步 API 文档页面规范

必须交付与本规范匹配的完整站内指南，而不是只放下载链接或截图。

### 10.1 信息结构

目录：概览 → Base URL/鉴权/幂等 → OpenAI文生图 → OpenAI图生图 → Gemini BB文生图/图生图 → Gemini SC文生图/图生图 → SC上传 → 共用查询 → 参数限制/重试 → HTTP错误/601–613 → 对象存储/链接时限。

桌面目录固定侧栏和正文滚动，当前锚点高亮；手机目录折叠抽屉。章节和端点标题可直接跳转。使用目标站点统一布局、暗色/亮色；代码块可横向滚动，不能撑破手机。

每个端点卡片展示方法、完整路径、平台、content-type、场景说明、参数表（字段/类型/必填/说明）、合法JSON或curl、HTTP202/200受理、相关注意事项。图生图展示JSON/multipart差异，SC上传字段file与OpenAI字段image明确区分。

共用查询展示四种status的独立示例；注明查询200≠成功。错误区分请求HTTP错误与任务应用码，保留上游脱敏原文展示说明。

### 10.2 复制、语言与地址

- Base URL从目标配置取得，移除末尾slash，避免重复`/v1/v1`；不硬编码原生产域名。
- 代码示例全部是可复制合法JSON，不带`//`注释；curl续行符后不跟中文行注释。
- 复制按钮支持端点、鉴权、body、受理/查询响应，显示短暂成功反馈及aria-label，使用目标已有复制组件。
- 中文/英文内容共用数据结构，目标i18next按其已有English-key规则组织；语言切换不改变端点协议。英文不得残留另一份旧SC响应或旧URL时限。
- 代码块中的API Key只用占位符，文档页不自动嵌入用户真实Token。
- 页面与可下载开发规范同步维护，契约测试校验路径/字段/状态与复制示例；不能仅更新后端而留下旧指南。

### 10.3 来源结构参考

来源 `GuideAsyncImageApiView.vue` 负责布局、目录与复制，`guideAsyncImageApiContent.ts` 生成中英文结构化端点内容，`AsyncImageApiEndpointCard.vue` 渲染参数/示例。目标以React业务组件重写，保持这种“内容与渲染分离”，复用目标CodeMirror/代码展示/复制能力，不带入Vue依赖。
