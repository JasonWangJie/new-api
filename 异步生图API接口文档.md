# new-api 异步生图 API 接口文档

日期：2026-09-17。源码仓库：[JasonWangJie/new-api](https://github.com/JasonWangJie/new-api)。核对代码：`c211e365027e79e0a195125227af87d7e9015642`。

本文按当前 Fork 实现说明接口，不把设计稿或上游厂商能力当成已实现行为。保留 new-api / QuantumNous 项目信息。重点覆盖全部七个公共接口，附任务中心管理接口说明；广场、投稿和通用图库不属于本文的生图调用范围。

## 1. 调用流程与接口总表

接口采用 **提交 → 返回任务 ID → 轮询 → 下载全部结果** 的流程。HTTP 202 只表示任务已持久受理，不代表生图完成。客户端连接断开后，已受理任务仍由后台处理。

| 方法 | 路径 | 用途 | 成功 HTTP |
|---|---|---|---|
| POST | `/v1/images/generations_oa` | OpenAI 兼容文生图/图生图，BB 与 SC 共用 | 202 |
| POST | `/v1/images/edits_oa` | OpenAI 兼容图片编辑，必须有参考图 | 202 |
| POST | `/v1/chat/completions_gm` | Gemini BB，以 messages 提交图片任务 | 202 |
| POST | `/v1/uploads/images_sc` | Gemini SC 单图上传，获得可引用的输入 URL | 200 |
| POST | `/v1/images/generations_sc` | Gemini SC，以 prompt/image_urls 提交 | 202 |
| GET | `/v1/images/tasks_async/{task_id}` | 全平台统一任务查询 | 200 |
| GET | `/v1/tasks_sc/{task_id}` | 上一个查询接口的别名，行为完全一致 | 200 |

这些是本 Fork 的扩展接口。原 `/v1/images/generations`、`/v1/images/edits`、`/v1/chat/completions` 和原生 Gemini 接口不会因为使用本指南而自动变成异步接口；异步没有 SSE、流式增量图片或回调通知接口。

## 2. Base URL、鉴权与通用请求头

假设服务外部地址为 `https://api.your-domain.com`，完整提交地址为 `https://api.your-domain.com/v1/images/generations_oa`。本文 `BASE_URL` 不带 `/v1`，也不带末尾 `/`。

```bash
# Linux/macOS Bash 示例。填写站点实际地址和 new-api API Token。
export BASE_URL='https://api.your-domain.com'
export NEW_API_TOKEN='替换为你的实际 API Token'
```

| 请求头 | 必须 | 说明 |
|---|---|---|
| `Authorization: Bearer <NEW_API_TOKEN>` | 是 | new-api 签发的 relay API Token，不是上游 API Key |
| `Content-Type: application/json` | JSON 提交必须 | 可带 charset |
| `Content-Type: multipart/form-data; boundary=...` | 文件提交必须 | 使用 curl `-F` 时让 curl 生成，不要手工写 boundary |
| `Idempotency-Key` | 否，建议提交/上传使用 | 最多 255 个 UTF-8 字节，细则见第 9 节 |

Token、用户需可用，Token 不能过期，需满足 IP 限制、模型权限、图片平台分组权限与可用额度。平台分组可按 Token 的 OpenAI/Gemini 映射解析；没有映射时使用 Token 分组，Token 分组为空时使用用户分组。客户端不能通过提交 `group` 绕过服务端分组策略。

公共任务查询必须使用 **提交时同一个 Token**；同一用户换另一个 Token 也返回 404。只读查询允许额度耗尽状态的 Token，但不允许禁用、过期、撤销或 IP 权限不符的 Token。额度耗尽不等于撤销查询权限。

管理员必须先启用全局异步和对应平台分组，配置模型目录、渠道池、价格、Redis、稳定图片加密密钥及存储。示例模型名来自当前实现/示例配置，不保证你账号已开通；实际只可使用管理员配置并有可用渠道的模型。

后台 ServerAddress 应为真实外部 HTTPS 地址，公共 `query_url` 和本地图片签名链接按它生成。如果未设置，可能得到相对路径；客户端应基于自己的服务 Base URL 解析，生产环境应优先修正站点配置。

## 3. OpenAI 兼容生成：POST /v1/images/generations_oa

支持 JSON 和 multipart/form-data。没有参考图时是文生图；提供有效参考图时进入图生图链路。输出始终通过任务查询获取。

### 3.1 JSON 入参

| 字段 | 类型 | 必须/默认 | 说明 |
|---|---|---|---|
| `model` | string | 否，默认 `gpt-image-2` | 去除首尾空白后须为有效模型名，最多 255 UTF-8 字节，不得含 `*` 或控制字符；仍需位于平台模型目录 |
| `prompt` | string | 是 | 去除首尾空白后非空，最多 64 KiB UTF-8 字节 |
| `n` | integer | 否，默认 1 | 范围 1–128，并受模型 `max_output_images` 更小上限约束；0、负数、小数和超大值拒绝 |
| `size` | string | 否 | 原生 `宽x高`、`auto`，或 `1K/2K/4K`、比例别名；规则见第 8 节 |
| `resolution` | string | 否 | `1K`、`2K`、`4K`、`AUTO`，大小写会规范化 |
| `aspect_ratio` | string | 否 | 第 8 节比例或 `auto`/`自动` |
| `image_urls` | string[] | 否 | 有效参考图 URL/Data URI；适用于图生图 |
| `images` | object[] | 否 | 每项 `{ "image_url": "..." }`；与 image_urls 同时提供时会合并计数 |
| `mask` | object | 否 | `{ "image_url": "..." }`；必须同时提供至少一张参考图，最终支持取决于模型 |
| `quality` | string | 否 | 须满足管理员模型能力的 qualities，例如配置允许的 `high`；不统一承诺所有模型都支持 |
| `output_format` | string | 否 | 受模型 formats 约束；本地可验证产物限 PNG/JPEG/WebP |
| `background` | string | 否 | 受模型 backgrounds 约束，如配置允许的 `transparent` |
| `output_compression` | number | 否 | 沿既有 relay/模型语义处理，合法范围取决于实际模型；显式 0 不因包装层丢失 |
| `response_format` | string | 否 | 作用于已有上游 relay 返回格式，例如适配器支持的 `b64_json`；公共任务查询始终返回 URL |
| `moderation`、`style`、`input_fidelity`、`user` 等 | 依既有协议 | 否 | 保留给已有图片 relay 处理；是否支持、类型和枚举由实际模型/适配器决定 |
| `stream` | boolean | 否 | 省略或 false；true 返回 400，异步不支持流式 |

`images[].file_id` 不支持；请改用 `image_url`。JSON 参考图请使用上表的 `image_urls` 或 `images`，不要把 multipart 的 `image` 文件字段当成 JSON 输入格式。包装层保留 OpenAI 原生字段，但不代表任意扩展字段会被对应适配器发给上游或被模型支持。

### 3.2 示例

```bash
curl -i -sS "$BASE_URL/v1/images/generations_oa" \
  -H "Authorization: Bearer $NEW_API_TOKEN" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: oa-new-task-001' \
  --data-binary '{"model":"gpt-image-2","prompt":"清晨自然光下的陶瓷花瓶，简洁构图","n":1,"resolution":"1K","aspect_ratio":"1:1","stream":false}'
```

参考图请求示例：

```json
{
  "model": "gpt-image-2",
  "prompt": "保留主体形状，将背景改成简洁的摄影棚",
  "n": 1,
  "resolution": "1K",
  "aspect_ratio": "1:1",
  "images": [
    {"image_url": "https://your-public-image-host.com/reference.png"}
  ]
}
```

成功返回见第 7 节，任务失败分类见第 11 节。

## 4. OpenAI 编辑：POST /v1/images/edits_oa

字段和受理响应与第 3 节相同，但 **必须有至少一张有效参考图**；只有 prompt 或只有 mask 会被拒绝。可用 JSON 的 image_urls/images，也可用文件 multipart。

### 4.1 multipart 入参

| 字段 | 类型 | 说明 |
|---|---|---|
| `image` 或 `image[]` | file，可重复 | 每张图片一个文件 part；支持多图，受全局/模型参考图限制 |
| `mask` | file，可选 | 有参考图时使用；须为可验证图片，最终语义取决于模型 |
| `model`、`prompt`、`size`、`resolution`、`aspect_ratio`、`quality` 等 | 普通文本 part | 按对应字段处理；prompt 仍必填 |
| `n` | 数字文本 | 如 `1`，按整数解析，不能带 JSON 字符串引号 |
| `stream` | 布尔文本 | 省略或 `false` |
| `output_compression`、`partial_images` | 数字文本 | 原生参数，按已有模型语义处理；partial_images 不会启用公共接口流式结果 |

文件字段仅接受 `image`、`image[]`、`mask`；其他带文件名的字段返回 400。默认每个文件最多 32 MiB；普通 part 最多 64 KiB；整个请求还受总体字节限制。PNG/JPEG/WebP 必须能完整解码，不能只靠文件后缀通过验证。

multipart 文件直接进入参考图 parts。若要用远程 image_urls/images 形式，建议使用 JSON；不要在 multipart 普通字段里塞一个 JSON 数组并假定会被按数组解析。

```bash
curl -i -sS "$BASE_URL/v1/images/edits_oa" \
  -H "Authorization: Bearer $NEW_API_TOKEN" \
  -H 'Idempotency-Key: oa-edit-task-001' \
  -F 'model=gpt-image-2' \
  -F 'prompt=保留人物，改成水彩插画风格' \
  -F 'image=@/实际路径/reference.png;type=image/png' \
  -F 'n=1' \
  -F 'resolution=1K' \
  -F 'aspect_ratio=1:1' \
  -F 'stream=false'
```

第 3 节 generations_oa 也支持同类 multipart 图生图提交。生成/编辑接口的幂等摘要包含完整原始 multipart 字节，重复执行 curl `-F` 通常改变 boundary，见第 9 节。

## 5. Gemini BB：POST /v1/chat/completions_gm

仅支持 application/json。这是以聊天 messages 形状提交的图片任务，并非任意聊天请求异步化，也不返回 ChatCompletion 文本。

### 5.1 入参

| 字段 | 类型 | 必须/默认 | 说明 |
|---|---|---|---|
| `model` | string | 是 | Gemini 图片模型名，无默认；需通过名称、目录及 Token 权限校验 |
| `messages` | object[] | 是 | 至少一项，当前所有项 `role` 必须为 `user` |
| `messages[].role` | string | 是 | 只支持 `user`；system/assistant/tool 等拒绝 |
| `messages[].content` | string 或 object[] | 是 | 非空文本，或以下 text/image_url parts 数组 |
| `content[].type` | string | 是 | `text` 或 `image_url` |
| `content[].text` | string | text part 必须 | 去除首尾空白后非空 |
| `content[].image_url.url` | string | image_url part 必须 | 非空参考图 HTTPS URL、Data URI 或有效绑定输入 URL |
| `extra_body.google.image_config.image_size` | string | 否 | `0.5K`、`1K`、`2K`、`4K`；0.5K 还须模型 allow_half_k |
| `extra_body.google.image_config.aspect_ratio` | string | 否 | 第 8 节 Gemini 比例；auto/自动表示不指定比例 |
| `stream` | boolean | 否 | 省略或 false；true 返回 400 |

所有文本拼接后必须非空，且最多 64 KiB；参考图不能代替提示词。图片与文字 parts 的相对次序被保留；多个 user messages 会汇集到上游的一条 user contents 中，不保留多轮聊天边界。

请求数量固定为 1，**不读取 `n`**；上游实际返回多图时会保存全部图片。这里只转换 model/messages/上述 image_config，不能假定 temperature、max_tokens、tools、任意 extra_body 或其他聊天字段会原样透传。

```bash
curl -i -sS "$BASE_URL/v1/chat/completions_gm" \
  -H "Authorization: Bearer $NEW_API_TOKEN" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: gemini-bb-task-001' \
  --data-binary '{"model":"gemini-3-pro-image-preview","messages":[{"role":"user","content":"画一张清晨薄雾中的山间小屋"}],"extra_body":{"google":{"image_config":{"image_size":"1K","aspect_ratio":"16:9"}}},"stream":false}'
```

带参考图的 content 示例：

```json
{
  "model": "gemini-3-pro-image-preview",
  "messages": [
    {
      "role": "user",
      "content": [
        {"type": "text", "text": "参考下面的主体，改为水彩风格"},
        {"type": "image_url", "image_url": {"url": "https://your-public-image-host.com/reference.png"}}
      ]
    }
  ],
  "extra_body": {"google": {"image_config": {"image_size": "1K", "aspect_ratio": "1:1"}}},
  "stream": false
}
```

受理响应除通用字段外还有 `id`、`object`、`status`，见第 7 节。

## 6. Gemini SC：上传与提交

### 6.1 POST /v1/uploads/images_sc

只接受 multipart/form-data 的 **一个非空 `file` part**。不接受多图、额外文本字段，或其他名称的 part。上传不创建生图任务，不返回 task_id；它创建绑定上传 Token 的临时输入。

| 项目 | 说明 |
|---|---|
| `file` | 唯一必填文件字段；PNG、JPEG、WebP |
| MIME | 声明 MIME 非空时必须与真实图片格式一致；curl 建议显式 type |
| 文件名 | 去除路径、控制字符及首尾空白后最多 255 UTF-8 字节；缺失时根据格式生成名称 |
| 单图大小 | 默认 32 MiB，由 max_upload_bytes 配置；整个 multipart 上限额外预留 64 KiB |
| 像素 | 默认不超过 80,000,000，由 download_max_pixels 配置 |
| 次数限制 | 每 Token 滚动一分钟默认 20 次，重放和进入计次阶段的无效请求也计次 |
| 临时输入总量 | 每 Token 默认 1 GiB，包含仍保留的预留/活跃等占用；不能通过失败重试自动释放不确定上传 |
| 输入保留 | 默认 24 小时，由 input_retention_hours 配置 |
| 幂等 | 可选；规则与生成提交的原始字节规则不同，见第 9 节 |

该接口要求 Gemini 平台分组可用且启用异步，用户有分组权限，全局异步/Redis/密钥/临时存储可用。上传额度与图片生成数量是不同限制。

```bash
curl -i -sS "$BASE_URL/v1/uploads/images_sc" \
  -H "Authorization: Bearer $NEW_API_TOKEN" \
  -H 'Idempotency-Key: sc-input-001' \
  -F 'file=@/实际路径/reference.png;type=image/png'
```

成功 HTTP 200：

```json
{
  "url": "https://api.your-domain.com/api/image-objects/imo_example/content?expires=1800000000&key=images-1&signature=example",
  "created_at": 1790000000
}
```

| 返回字段 | 类型 | 说明 |
|---|---|---|
| `url` | string | 可访问的签名 URL；也用作同一 Token 的输入引用。local 为网关签名地址，S3 为对象存储预签名地址 |
| `created_at` | integer | 原始上传记录创建时间，Unix 秒；幂等重放保留原时间 |

使用返回的 URL **原样**放入后续 image_urls，不重排查询参数、不删除签名。上传引用身份与 Token 绑定；输入过期、删除、不可用或属于其他 Token 会拒绝，不能当普通远程 URL 绕过。签名下载有效期与输入记录保留期不同：下载链接可能先过期，绑定输入仍按服务端记录判断；重放同一有效上传可获得当前签名，但累计签名别名最多 128 个。

### 6.2 POST /v1/images/generations_sc

仅支持 JSON，既可直接文生图，也可提供远程图/Data URI/上述上传 URL 做图生图，并非每次都必须先上传。

| 字段 | 类型 | 必须/默认 | 说明 |
|---|---|---|---|
| `model` | string | 是 | Gemini 图片模型名，无默认 |
| `prompt` | string | 是 | 去除首尾空白后非空，最多 64 KiB |
| `image_urls` | string[] | 否 | 省略/空数组为文生图；非空为图生图。数组不能含空字符串 |
| `resolution` | string | 否 | `0.5K`、`1K`、`2K`、`4K`，须满足模型能力 |
| `size` | string | 否 | 支持分辨率别名或比例；数字 WxH 可被归约成比例，仍必须是允许的比例 |
| `aspect_ratio` | string | 否 | 第 8 节 Gemini 比例；auto/自动表示不指定 |
| `ratio` | string | 否 | aspect_ratio 为空时使用的兼容别名 |

Gemini 不读取 `n`，数量固定 1；仍保存实际返回的全部图片。`stream` 不是 SC 协议参数，当前 SC 解析器不读取它；不要用它请求流式输出。OpenAI 的 quality、background、output_format 等不能当成 SC 图片配置原样透传。

```bash
curl -i -sS "$BASE_URL/v1/images/generations_sc" \
  -H "Authorization: Bearer $NEW_API_TOKEN" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: gemini-sc-task-001' \
  --data-binary '{"model":"gemini-3-pro-image-preview","prompt":"画一张清晨薄雾中的山间小屋","size":"1K","ratio":"16:9","image_urls":[]}'
```

使用上传图片时把返回 URL 放入 image_urls，并为这个新的生成请求使用另一个幂等 key。成功为 HTTP 202 通用响应。

## 7. 受理与查询响应

### 7.1 生图提交受理响应

OpenAI 生成、编辑及 Gemini SC：

```json
{
  "task_id": "asyncimg_example",
  "query_url": "https://api.your-domain.com/v1/images/tasks_async/asyncimg_example"
}
```

Gemini BB：

```json
{
  "task_id": "asyncimg_example",
  "query_url": "https://api.your-domain.com/v1/images/tasks_async/asyncimg_example",
  "id": "asyncimg_example",
  "object": "image.task",
  "status": "queued"
}
```

| 返回项 | 说明 |
|---|---|
| HTTP 202 | 已受理，包括复用已有任务的幂等重放 |
| `task_id` | string，唯一任务身份，保存后用于查询 |
| `query_url` | string，统一查询地址 |
| BB `id` | string，与 task_id 相同 |
| BB `object` | 固定 `image.task` |
| BB `status` | 受理响应固定 queued；重放旧任务也不表示旧任务当前仍在排队，必须查询 |
| `Location` 响应头 | 与 query_url 相同 |
| `Retry-After: 3` | 建议至少 3 秒后查询 |
| `X-Idempotency-Replayed: true` | 只在复用已有受理任务时出现 |
| `Cache-Control: no-store` | 不缓存公共响应 |

### 7.2 GET /v1/images/tasks_async/{task_id}

无请求体。路径参数 task_id 使用受理返回的原值，请做好 URL 编码。

```bash
curl -i -sS "$BASE_URL/v1/images/tasks_async/asyncimg_example" \
  -H "Authorization: Bearer $NEW_API_TOKEN"
```

排队/处理中返回 HTTP 200，并带 `Retry-After: 3`：

```json
{"task_id":"asyncimg_example","status":"queued"}
```

```json
{"task_id":"asyncimg_example","status":"processing"}
```

成功返回 HTTP 200：

```json
{
  "task_id": "asyncimg_example",
  "status": "succeeded",
  "data": [
    {"url": "https://api.your-domain.com/api/image-objects/imo_example/content?expires=1800000000&key=images-1&signature=example"},
    {"url": "https://your-object-storage-host.com/another-signed-result.png"}
  ]
}
```

失败也返回 HTTP 200：

```json
{
  "task_id": "asyncimg_example",
  "status": "failed",
  "error_code": 601,
  "fail_reason": "Upstream content or safety policy rejected the request"
}
```

| 字段 | 类型 | 出现条件/含义 |
|---|---|---|
| `task_id` | string | 所有正常查询响应 |
| `status` | string | queued、processing、succeeded、failed 四种公共状态 |
| `data` | object[] | 仅 succeeded，包含全部图片，按服务端 image_index 排序 |
| `data[].url` | string | 当前签名访问 URL；不返回原始 Base64、文件路径或对象凭据 |
| `error_code` | integer | 仅 failed，601–613，见第 11 节 |
| `fail_reason` | string | 仅 failed；面向调用者的说明，不应当作机器判断的稳定枚举 |

queued/processing 不包含可用 data；只有整批图片持久化、账务及消费日志确认满足条件才公布成功。公共响应没有 progress、用量、价格、账号详情或部分结果字段；这些属于任务中心。

**HTTP 非 2xx 查询错误是查询本身失败**，例如数据库/存储暂不可用，并非任务 status=failed。服务端成功任务缺少对象记录、签名失败或结果清单不完整时会返回 503 storage_unavailable，不会把残缺列表当作成功返回。

终态任务超过保留期、任务不存在或不属于当前 Token，返回 HTTP 404 task_not_found。过期终态不再通过公共接口暴露 608；已经过保留期的记录直接是 404。

### 7.3 GET /v1/tasks_sc/{task_id}

入参、鉴权、返回、错误、过期及缓存规则均与第 7.2 节完全相同。**它不是另一套 SC 返回格式，也不返回 Base64**；也可以查询当前 Token 提交的 OpenAI/BB 任务。

```bash
curl -i -sS "$BASE_URL/v1/tasks_sc/asyncimg_example" \
  -H "Authorization: Bearer $NEW_API_TOKEN"
```

## 8. 尺寸、参考图与默认限制

### 8.1 OpenAI 尺寸映射

| aspect_ratio | 1K | 2K | 4K |
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

这是 Fork 当前的兼容映射，不保证所有上游模型接受所有映射尺寸；模型目录与实际上游支持仍有约束。

OpenAI 规则：

- 显式 resolution + 比例按上表映射；显式 resolution 没有比例时按 `1:1` 映射。两者均未提供时，不在解析器里补一个默认原生 size，交给既有 relay/模型处理。
- size=1K/2K/4K 可作为 resolution 别名；size=16:9 等可作为比例别名。已有显式 resolution/aspect_ratio 时，以显式对应值优先。
- 指定比例却没有可解析的 resolution 时返回 400，例如只传 aspect_ratio=16:9。
- size=auto 会规范化为 size=auto、resolution=AUTO；aspect_ratio=auto 或 resolution=AUTO 也可产生 auto 上游尺寸。自动尺寸是否被上游接受仍取决于模型。
- 原生 WxH 会规范化 X、*、× 为 x；同时传显式 resolution 时须与原生尺寸按长边计算的档位一致，否则 400。不要混传原生 size 与比例期望服务端再次裁切。
- 原生 size 的其他字符串可以交给既有 relay 处理；同时再传 resolution/比例造成歧义时拒绝。

### 8.2 Gemini 尺寸规则

Gemini 分辨率为 0.5K/1K/2K/4K，其中 0.5K 必须有 allow_half_k 能力。比例支持 `1:1`、`2:3`、`3:2`、`4:5`、`5:4`、`4:3`、`3:4`、`16:9`、`9:16`、`21:9`、`9:21`，**不支持 OpenAI 表里的 2:1/1:2**。

BB 从 extra_body.google.image_config 读取尺寸；SC 支持 resolution/size 和 aspect_ratio/ratio。未指定时不补默认分辨率/比例。size 为 WxH 时只能归约成支持的比例，不能向 Gemini 要求精确像素尺寸；auto/自动比例表示省略比例。

计费规格与请求别名/实际图片尺寸一起确定：OpenAI 原生尺寸按长边、Gemini 按短边判档；合法显式分辨率别名保留请求档位，0.5K 使用最低 1K 账单档。具体金额由管理员价格及分组倍率决定，不是此表给出的固定价格。

### 8.3 参考图与默认预算

| 限制 | 当前默认 | 说明 |
|---|---|---|
| 全局参考图数量 | 8 | 还需受模型 max_reference_images 限制 |
| Gemini Flash Image 本地上限 | 3 | 当前实现对模型名同时含 flash 和 image 的模型额外限 3 |
| 单图本地下载/验证大小 | 32 MiB | download_max_bytes |
| 单图像素 | 80,000,000 | download_max_pixels |
| 参考图累计大小 | 64 MiB | max_reference_total_bytes，包括本地解析/执行处理的参考图与 mask |
| 参考图累计像素 | 80,000,000 | max_reference_total_pixels |
| 参考图下载时间 | 30 秒 | download_timeout_seconds |
| 重定向 | 最多 3 次 | download_max_redirects |
| 整个生成提交请求体 | `(max_reference_total_bytes / 3) * 4 + 1 MiB` | 整数除法，预留 Base64 与 JSON/multipart 开销；代理可设置更低限制 |
| SC 上传单图 | 32 MiB | max_upload_bytes，与 download_max_bytes 是不同配置 |
| 输入/任务/结果保留 | 24 小时 / 90 天 / 90 天 | 管理员可调整 |
| 签名 URL | 默认 3600 秒 | 不超过对象剩余有效期 |

最终参考图数量取全局、模型能力与已知模型限制共同允许的范围，不能仅凭某厂商支持更多图就绕过平台配置。输入格式支持 `data:image/png;base64,...`、JPEG/WebP Data URI，或 **公共 HTTPS URL**；远程 URL 不允许凭据、fragment、内网/回环/保留地址。本地下载会再次校验 DNS、重定向、实际连接地址、MIME 和完整图片。

参考图输送可配置 passthrough/local/passthrough_fallback_local，是否由网关下载、内联或由上游读取取决于平台和该设置；网关成功下载不能保证上游也能读取远程 URL。已绑定 SC 输入按服务端对象读取，不通过公网绕回下载。URL 故障有时在受理前验证拒绝，有时在 Worker 下载/上游阶段才成为任务失败。

## 9. 幂等：避免重复生图与重复上传

### 9.1 生成提交

四条生成/编辑提交路径共享 **Token + Idempotency-Key** 的受理幂等范围，摘要还包含平台、方言、请求路径和**完整原始请求体字节**。

- 同一 Token、同 key、同路径、同请求字节：复用同一 task_id，HTTP 202，带 `X-Idempotency-Replayed: true`，不会因为重放再生成。
- JSON 空白、字段顺序、字符转义、字段值变化，或改用另一条提交路径：同 key 返回 HTTP 409 async_image_idempotency_conflict；语义相同的 JSON 也不保证字节相同。
- 同一路径带 Gemini BB 的重放仍固定返回 status=queued；真实终态需要查询。
- 没有 key：每次成功受理都创建新任务，可能新增费用。
- 需要修改输入开始新任务时使用新 key；单纯网络重试保留旧 key 和原始请求快照。
- 旧任务/结果不可用时不应把旧 key 当成再次生成开关；会返回结果不可用/查询不可见等错误，幂等记录不会自动让旧请求重新执行。

可把 JSON 请求快照保存到文件，并用 `--data-binary @request.json` 重试；修改该文件就必须重新评估是否是新任务。**multipart 生成请求必须保存完整编码体及对应 Content-Type boundary**；反复运行 curl `-F` 即使图片不变，也常因新 boundary 而返回 409，不应为绕过冲突随意换 key。

### 9.2 SC 上传

上传有独立 Token + key 范围，摘要由 **文件字节摘要 + 声明 MIME + 规范化文件名 + 文件长度**组成，不包含 multipart boundary。所以 curl `-F` 重试同一文件、同 MIME、同文件名可重放，即使 boundary 不同。

同 key 但文件内容/上述元数据变化会 409；上传处理中会 409，按 Retry-After 等待。有效上传重放返回原 created_at 和当前签名，带 **`X-Idempotency-Replayed: true`**。不是 `Idempotency-Replayed` 或没有 X- 前缀的响应头。

## 10. HTTP 请求阶段错误：error.code

普通控制器错误格式为：

```json
{
  "error": {
    "type": "invalid_request_error",
    "code": "invalid_request",
    "message": "reference images are required for edits"
  }
}
```

控制器当前即使返回 503，也可能使用 invalid_request_error 类型；鉴权中间件规范化错误可能使用 authentication_error、permission_error、server_error。**以 HTTP 状态 + error.code 为机器判断依据，不只根据 error.type。**

| error.code | HTTP | 含义与处理 |
|---|---|---|
| `authentication_failed` | 通常 401 | 缺少/无效 Token，用户/Token 不可用、过期或状态不允许；修正 Token，不能无条件重试 |
| `access_denied` | 通常 403，鉴权层保留实际状态 | 权限/IP 等限制；检查用户、Token 与代理地址配置 |
| `service_unavailable` | 鉴权层 5xx | 既有鉴权依赖不可用；等待恢复并保留原请求快照 |
| `invalid_request` | 400 | JSON、Content-Type、messages、prompt、n、尺寸、模型参数或 multipart 字段不合法；修正输入 |
| `invalid_idempotency_key` | 400 | key 非有效 UTF-8 或超过 255 UTF-8 字节 |
| `request_too_large` | 413 | 生成请求体、SC 上传文件/整体 multipart 超出字节限制 |
| `invalid_reference_image` | 400 | 已知 SC 上传引用不属于本 Token、过期/不可用，或引用解析失败 |
| `invalid_image` | 400 | SC 上传不能完整解码、格式/MIME 不匹配、像素/容器不合法；其他提交的图像验证错误可能归 invalid_request |
| `async_image_unavailable` | 403 或 503 | 403：全局/平台禁用、分组/模型/渠道/价格/额度/存储等受理资格不足；503：运行配置读取失败。需结合 message 与后台状态定位 |
| `async_image_queue_unavailable` | 503 | Redis 调度 Ping 失败；服务恢复后用同 key/同原始字节重试 |
| `async_image_encryption_unavailable` | 503 | 未配置有效稳定图片加密密钥；修复服务端密钥，勿随意替换已有密钥 |
| `database_unavailable` | 503 | 权限、Token、任务、幂等、受理或上传记录数据库操作失败 |
| `storage_unavailable` | 503 | 输入上传、对象读取/清单/签名不可用；查询返回此错误时不代表任务必须重新生成 |
| `task_not_found` | 404 | 任务不存在、不属于提交 Token/用户，或终态保留已过期 |
| `async_image_idempotency_conflict` | 409 | 生成 key 复用但请求字节/路径等不同；比对保存的请求快照 |
| `async_image_result_unavailable` | 409 | 生成 key 已绑定，但原任务不可读取；不要自动换 key 再生成 |
| `async_image_upload_idempotency_conflict` | 409 | 上传 key 的文件字节或元数据改变，或上传确认状态冲突 |
| `async_image_upload_rate_limited` | 429 | 上传滚动一分钟计次超过配置限制，Retry-After: 60；失败尝试/重放也可能计次 |
| `async_image_upload_byte_quota` | 409 | 本 Token 临时输入占用已达上限；等待清理/检查配额，不靠改 key 绕过 |
| `async_image_upload_in_progress` | 409 | 同 key 上传仍在处理中；Retry-After: 60，保留文件和 key 重试 |
| `async_image_upload_result_unavailable` | 409 | 该上传记录失败、过期、删除或结果不可用；确认后如确需新的输入可使用新上传 key |
| `async_image_upload_alias_limit` | 429 | 同一上传签名别名上限 128，Retry-After: 60；上限不是一分钟计次，等待 60 秒不保证自动解除 |

例如非法 n、模型参考图超限通常直接是 400 invalid_request，**不会先返回 task_id 再返回数字 604/611**。错误发生前若已持久受理而响应丢失，客户端用同一快照和幂等 key 重试才能消除不确定性。

反向代理、WAF、全局管理 API 限流或未匹配路由可能返回自己的格式，不能假定所有基础设施错误都有上表 error 对象。不要根据 message 的中英文文案写固定分支。

## 11. 已受理任务失败：error_code 601–613

这些是查询 HTTP 200、status=failed 时的 **数字** error_code，不是 HTTP 状态码，也不是上表的字符串 error.code。

| error_code | 分类 | 常见原因 | 客户端建议 |
|---|---|---|---|
| 601 | 内容或安全策略拒绝 | 内容政策、安全过滤、政策相关拒绝 | 根据实际政策修改输入；不盲目换账号重试 |
| 602 | 参考图拉取失败 | 网关/上游无法取图，DNS、URL 有效期、远程下载故障 | 排队期间由服务端按预算重试；最终失败检查 URL 与输入可用性 |
| 603 | 图片执行容量不可用 | 执行/渠道并发繁忙，容量或账号资源耗尽等 | 服务端按容量预算等待/重试；最终失败查看后台资源与渠道 |
| 604 | 参数、尺寸或资格无效 | 上游明确拒绝参数，执行前分组/模型权限变化、输入预算等 | 修正参数或服务端资格；新任务使用新 key |
| 605 | 上游频率限制 | 上游 HTTP 429，且未被更具体语义分类覆盖 | 服务端按临时错误预算及 Retry-After 重试 |
| 606 | 上游临时故障 | 上游 5xx，且未被更具体语义分类覆盖 | 服务端按临时错误预算重试；最终失败检查上游 |
| 607 | 图片产物缺失/无效 | 完整图片列表解析或下载/验证失败，数量不合法，用量缺失 | 检查上游返回与适配器；不承诺上游没有执行/计费 |
| 608 | 执行未知、期限到达或任务结束 | 已发上游但响应/结果未持久化、租约恢复发现不确定执行、总期限到达、管理员终止 | **先人工核对任务事件与账务，不自动重新生成** |
| 609 | 存储或账务/日志确认失败 | 已有图片后处理失败，自动恢复预算耗尽 | 管理员修复存储/账务/日志后 resume，只恢复已有产物 |
| 610 | 未分类上游/任务失败 | 不能由已知语义归类的错误；非法保存的公开错误码也回退到此值 | 检查任务事件和服务端日志，不推测可安全重试 |
| 611 | 上游参考图数量超限 | 本地通过但上游更低上限拒绝，或上游对应错误文案 | 减少参考图并核对模型目录能力 |
| 612 | 上游要求参考图 | 上游模型/场景要求输入图，但请求未提供 | 按模型要求添加有效参考图 |
| 613 | 提示词或输入图无法处理 | 上游提示输入处理失败，图片/提示词本身不适合处理 | 检查输入、模型和上游限制后修改 |

当前实现按错误语义优先分类，策略/参考图等信息可能优先于 HTTP 429/5xx；未知上游 400 不自动全部当 604。分类依赖当前适配器与错误信息，不能把分类当作上游原始错误码的一一映射。

602、603、605、606 自动重试期间，公共查询仍是 queued/processing，通常不暴露中间数字错误。默认参考重试预算 2、容量 5、临时上游错误 3、总重试预算 16，并有账号切换、参考图失败与截止时间等额外约束；这些不是无条件保证的实际尝试次数。

609 自动后处理重试尚有 next_attempt_at 时公共状态仍 processing；预算耗尽才公开 failed/609。管理员可恢复特定 storage_failed/billing_failed 记录，因此公共 failed 在这类人工恢复后可重新变 processing；无需再次调用生成接口。

execution_unknown 是内部状态，公共接口显示 failed/608。系统不会把“不知道上游是否已经执行”当成“肯定没执行”；换 key 重新提交会创建新的生图任务，存在重复成本。已结算任务被结束也不能据此推断自动退款。

## 12. 轮询、下载与客户端重试

建议流程：

1. 创建请求快照与唯一幂等 key；提交后立即保存 task_id/query_url。网络超时重试原快照和原 key，不重新编排 JSON。
2. 至少间隔 3 秒查询。queued/processing 继续查询；HTTP 503/5xx 或网络故障采用 5–30 秒退避，仍查询原任务。
3. succeeded 时遍历整个 data 数组并下载；不要只保存第一张。签名 URL 不宜当长期图片身份，保留 task_id 和结果序号。
4. 下载签名过期时重新查询获取新 URL；任务/结果保留期已过则不能保证重新签名。下载失败不调用生图接口修复。
5. failed 时保存数字 error_code 和 fail_reason；按上表决定修正输入或联系管理员。608/609 先核对现有产物和账务。
6. 客户端停止等待/进程退出不会取消已受理任务；公共接口没有取消/删除任务 API。

提交受理前会检查当前额度与价格，但不调用上游、不占渠道执行并发、不在受理阶段扣图片费；Worker 执行前再次校验资格。成功后账单按实际产物/实际用量固定并幂等结算。具体资金来源、订阅、管理员配置和上游计费以实际运行结果为准，不能从 HTTP 202/失败数字码直接推断账单或退款。

本地存储 URL 通常指向以下签名资源接口：

```text
GET /api/image-objects/{object_id}/content?expires=...&key=...&signature=...
```

这是下载入口，不是新的任务查询接口：使用返回 URL 原样访问，不需额外 Bearer Token；URL 本身具备访问能力，应按敏感链接保管。成功直接返回图片字节及真实 Content-Type；签名无效/到期或对象不可用通常 404，存储不可用可能 503，此接口不保证 error.code JSON 格式。S3 预签名链接则直接由相应存储服务处理。

## 13. 附录：站内任务中心接口

以下路径走 **管理 API 鉴权**，不是公共 relay Token 接口。使用站内有效会话/系统支持的管理凭据，并遵守既有管理 API 要求；不要直接把第 2 节 relay API Token 当成管理权限。用户路径要求 UserAuth；管理员路径要求 AdminAuth，并按接口要求资源 `async_image_task` 的 `read` / `manage` 权限。

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/user/async-image-tasks` | 当前登录用户的任务列表，跨其 Token 查看 |
| GET | `/api/user/async-image-tasks/{task_id}` | 用户任务详情、事件、可用结果 |
| GET | `/api/user/async-image-tasks/{task_id}/results/{image_index}/view` | 用户任务结果最新签名/重定向 |
| GET | `/api/admin/async-image-tasks` | 管理员任务列表 |
| GET | `/api/admin/async-image-tasks/{task_id}` | 管理员任务详情与审计字段 |
| GET | `/api/admin/async-image-tasks/{task_id}/results/{image_index}/view` | 管理员结果签名/重定向 |
| POST | `/api/admin/async-image-tasks/{task_id}/resume` | 仅恢复 storage_failed/billing_failed 的后处理 |
| POST | `/api/admin/async-image-tasks/{task_id}/terminate` | 结束任务；不能假定撤销已发生的上游请求或自动退款 |
| POST | `/api/admin/async-image-tasks/batch-terminate` | 批量结束，逐项反馈 |

管理成功/控制器错误分别采用如下形状，与公共接口不同：

```json
{"success":true,"message":"","data":{"task_id":"asyncimg_example"}}
```

```json
{"success":false,"message":"image operation conflict"}
```

既有管理鉴权/限流层可能有自己的错误状态与格式。以下说明针对图片控制器，通常参数错 400、找不到 404、状态冲突 409、数据库/存储故障 503。

### 13.1 列表入参与返回

用户/管理员列表共用：

| 查询参数 | 类型/默认 | 说明 |
|---|---|---|
| `page` | 正整数，默认 1 | 页码 |
| `page_size` | 1–100，默认 20 | 每页数量 |
| `timezone` | string，默认 UTC | IANA 时区，如 Asia/Shanghai |
| `start_date`、`end_date` | YYYY-MM-DD | 创建日期筛选，开始含、结束日期整日含；两者都省略默认该时区今天 |
| `task_id`、`model`、`group` | string | 精确匹配 |
| `platform` | string | openai/gemini |
| `protocol` | string | 内部 dialect：bb/sc；OpenAI 公共 _oa 任务当前也记 bb，不是输入 oa |
| `request_type` | string | text_to_image/image_to_image |
| `status` | string | 内部任务状态，见下文；不是公共 processing 的同义枚举 |
| `billing_status` | string | 数据库保存的账务状态，如 pending/succeeded/not_billable |
| `api_key_id` | 正整数 | Token 记录 ID |
| `q` | string | task_id/model/prompt_summary 的文字搜索 |
| `storage_provider` | string | 按结果所用存储厂商筛选，如 local/aws/r2 |
| `sort_by` | 默认 created_at | created_at/submitted_at/finished_at/updated_at/status/quota/image_count/duration_ms |
| `sort_order` | asc/desc，默认 desc | 排序方向 |
| `user_id`、`channel_id`、`account_id` | 正整数，仅管理员 | 当前 account_id 实际按 channel_id 筛选，不能当上游单个 Key 指纹 ID；用户路径传这些参数会 400 |

响应 data 有 `items`、`total`、`page`、`page_size`、`pages`、`stats`。stats 是整个筛选集合的 total/queued/processing/succeeded/failed/image_count/quota/average_duration_ms，不只是当前页；没有成功任务时平均耗时可以为 null。

items/task 通用字段：

| 字段组 | 类型与说明 |
|---|---|
| id/task_id、protocol、platform、request_type、model、group | string；id 与 task_id 相同 |
| api_key_id | integer，提交 Token 的数据库 ID |
| status/billing_status | string，内部任务/账务状态 |
| progress | integer，处理进度 |
| requested_size/requested_resolution/actual_size/aspect_ratio | string，请求与实际尺寸事实 |
| image_count/result_count | integer，产物数量与已登记结果数量 |
| quota/cost/currency | quota 为整数；cost 按全局 QuotaPerUnit 换算，currency 当前标为 USD；不是实时外部汇率报价 |
| prompt_summary | string，受提示词预览开关/长度及清理规则影响 |
| retry_count、error_code、error_message | 重试次数、**字符串内部错误码**与说明；这里的 error_code 不是公共 601–613 |
| created_at/updated_at/started_at/upstream_succeeded_at/finished_at/expires_at/next_attempt_at | Unix 秒，未发生时可能为 0 |
| duration_ms | integer 或 null，已结束时为从创建到结束的毫秒数 |
| can_resume/can_terminate | boolean，用户视图为 false，管理员按当前状态计算 |

管理员额外字段为 user_id、channel_id、attempts、reference_urls、reconciliation_status。attempts/reference_urls 当前为保存的字符串内容，不保证是已解析数组；用户接口不包含这些字段。

内部 status 可为 queued、invoking、upstream_succeeded、uploading、billing_pending、storage_failed、billing_failed、succeeded、failed、expired、execution_unknown。未选定渠道的 invoking 在列表展示/queued 筛选中按 queued 处理。公共查询会将这些状态转换成第 7 节的四种状态；尤其后处理失败等待自动重试时，公共查询仍可能是 processing。

### 13.2 详情与结果 view

详情成功 data 为 `{task, results, events}`。results 仅在结果符合可用条件时提供，每项字段为 `id`、`image_index`（从 0 开始）、`content_type`、`byte_size`、`checksum`、`width`、`height`、`view_url`、`created_at`、`expires_at`。events 每项为 id、event_type、status、message、created_at，按事件顺序排列。

view 路径的 image_index 必须为非负整数，属于该任务。请求头 Accept 包含 application/json 时，成功为：

```json
{"success":true,"message":"","data":{"url":"https://your-signed-result-url","expires_at":1800000000}}
```

否则返回 HTTP 307 跳转到签名链接。失败可能为 400/404/503；view 获取新签名不重新生成图片。

### 13.3 resume、terminate 与 batch-terminate

resume/terminate 无必需 JSON 入参；task_id 在路径中。成功 HTTP 200，data={task_id}。resume 只支持 storage_failed/billing_failed，其他状态会冲突；从已有对象/固定账单继续恢复，不再次调用上游。terminate 按任务状态处理，事件与 reconciliation_status 用来记录结算事实。

batch-terminate 请求体：

```json
{"task_ids":["asyncimg_example_a","asyncimg_example_b"]}
```

task_ids 为 string[]，原数组长度 1–100；去除首尾空白、忽略空值及重复 ID。成功响应 data.items 的每项含 task_id/status，status 为 terminated/skipped/failed；跳过/失败项可含 message。总体 HTTP 200/success=true **不代表每项都结束成功**。

### 13.4 配置入口索引

生图部署管理员可在 `/system-settings/operations/images` 设置。相关接口是 `/api/option/images`（读取）、`PUT /api/option/images/runtime`、`PUT /api/option/images/policy`、`PUT /api/option/images/pool`、`POST /api/option/images/storage` 和 `POST /api/option/images/storage/{profile_id}/test`，受 RootAuth 和图片配置权限约束。

Token 图片分组映射：`GET/PATCH /api/token/{id}/image-platform-groups`。工作台能力：`GET /api/user/image-workbench/capabilities/{token_id}`，返回可用平台/模型、capability_version、gateway_base_url、默认轮询间隔等；这些是站内管理能力接口，不是第 1 节七条公共 Token 路径。

## 14. 文档依据与验证边界

主要实现依据：`router/async-image-router.go`、`controller/async_image.go`、`service/image_protocol.go`、`service/image_validation.go`、`service/image_references.go`、`service/image_routing.go`、`service/image_failure.go`、`service/image_worker.go`、`service/image_storage.go`、`relay/async_image.go`、`model/image_inputs.go`、`model/image_models.go`、`model/image_tasks.go`、`controller/image_tasks.go` 及 `router/api-router.go`。

本文纠正了旧说明中可能混淆的响应头、默认尺寸和 Gemini 参数透传表述；发生差异时以这里标明的源码版本和实际返回为准。示例 ID、时间戳、域名、签名是说明用占位值，不能直接访问；示例模型必须与实际平台目录相符。

本次为源码契约核对和文档示例检查，没有新运行数据库矩阵、真实上游调用、服务器部署或生产账务测试。文档不据此宣称生产端到端验收通过。
