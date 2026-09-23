# new-api 异步图片与视频 API 接口文档

更新：2026-09-23。源码仓库：[JasonWangJie/new-api](https://github.com/JasonWangJie/new-api)。本文描述当前工作区已实现的接口；具体可用模型和参数取决于管理员配置的渠道、模型能力与任务插件。保留 new-api / QuantumNous 项目信息。渠道池配置另见 [异步图片与视频调度配置说明](./异步图片与视频调度配置说明.md)。

本文按**图片提交 → 视频提交 → 统一查询 → 错误与兼容**组织。下面的域名、Token、任务 ID、文件路径和结果链接均为示例，调用时替换为实际值。

## 1. 先选对接口

| 需求 | 方法与路径 | 请求格式 | 成功响应 |
|---|---|---|---|
| 文生图、带参考图生图 | `POST /v1/images/generations_async` | JSON 或 multipart | 202，返回任务 ID |
| 图片编辑，必须提供参考图 | `POST /v1/images/edits_async` | JSON 或 multipart | 202，返回任务 ID |
| 视频生成 | `POST /v1/videos/generations_async` | JSON 或 multipart | 202，返回任务 ID |
| 视频编辑 | `POST /v1/videos/edits_async` | JSON | 202，返回任务 ID |
| 视频延长 | `POST /v1/videos/extensions_async` | JSON | 202，返回任务 ID |
| 查询上述图片或视频任务 | `GET /v1/media/tasks_async/{task_id}` | 无请求体 | 200，返回状态与结果 |
| 读取**本站已保存**的视频 | `GET /v1/media/objects/{object_id}` | 使用完整签名 URL | 200 或 206 |
| 获取**本站已保存**的视频元数据 | `HEAD /v1/media/objects/{object_id}` | 使用完整签名 URL | 200 或 206 |

**关键区别：**提交返回的 202 仅表示任务已持久受理；后台生成、存储和结算仍可能失败。原同步 `POST /v1/images/generations`、`POST /v1/images/edits`、`POST /v1/videos` 不会自动转为这些异步接口。旧版 OpenAI / Gemini 异步入口见[第 9 节](#9-旧版兼容入口)。

## 2. 最短调用流程：文生图

以下示例使用 Bash 和 curl。先设置网关地址及 **new-api 签发的 API Token**；`BASE_URL` 不含 `/v1`，也不带末尾斜杠。

```bash
export BASE_URL='https://api.your-domain.com'
export NEW_API_TOKEN='替换为 new-api 签发的 API Token'
```

**第一步：提交。**示例中的 `openai`、`gpt-image-1` 必须已经在网关中配置并启用；不想指定供应商时可以删掉 `provider`，由网关选路。

```bash
curl -i -sS "$BASE_URL/v1/images/generations_async" \
  -H "Authorization: Bearer $NEW_API_TOKEN" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: image-generate-001' \
  --data-binary '{
    "provider": "openai",
    "model": "gpt-image-1",
    "prompt": "清晨自然光下的陶瓷花瓶，简洁构图",
    "n": 1,
    "size": "1024x1024",
    "resolution": "1K"
  }'
```

受理响应示例：

```http
HTTP/1.1 202 Accepted
Location: https://api.your-domain.com/v1/media/tasks_async/asyncimg_example
Retry-After: 3
Cache-Control: no-store
Content-Type: application/json

{"task_id":"asyncimg_example","query_url":"https://api.your-domain.com/v1/media/tasks_async/asyncimg_example"}
```

**第二步：查询。**保存实际返回的 `task_id` 和 `query_url`，用**提交时的同一个 Token**查询；不同 Token 即使属于同一用户也会得到 404。优先直接请求响应中的 `query_url`。

```bash
curl -sS "$BASE_URL/v1/media/tasks_async/asyncimg_example" \
  -H "Authorization: Bearer $NEW_API_TOKEN"
```

**第三步：读取结果。**当 `stage=completed`、`status=succeeded` 且 `data` 非空时，使用 `data[].url`。图片完整响应示例见[第 6 节](#6-统一任务查询与结果)。查询间隔建议不短于 3 秒；网络错误或 5xx 时逐步延长到 5–30 秒，不要把一次查询失败当成任务失败。

## 3. 所有新异步入口的共同规则

### 3.1 鉴权、请求头和处理流程

| 请求头 | 适用范围 | 说明 |
|---|---|---|
| `Authorization: Bearer <NEW_API_TOKEN>` | 提交与任务查询必需 | 使用网关 Token，不使用上游 API Key |
| `Content-Type: application/json` | JSON 提交必需 | 可附带 charset |
| `Content-Type: multipart/form-data` | 文件提交必需 | curl `-F` 会自动生成 boundary，勿手填 |
| `Idempotency-Key` | 提交可选，建议设置 | 最多 255 个 UTF-8 字节；重试同一业务操作时复用 |

Token、用户、IP、分组、模型和媒体平台权限都要满足服务端规则。客户端提交的 `group` 不会覆盖服务端分组策略。

网关会先检查参数与渠道能力，冻结供应商、协议、模型映射、渠道和计费快照，然后持久化任务并返回 202。后台 Worker 才调用上游、轮询结果、保存产物并结算。客户端断开连接不会取消已受理的任务。

新提交接口均可选填 `provider`。填写后只在对应供应商、分组、模型能力及渠道池中选路；不填写则由网关自动选择。`provider` 是网关参数，不会转发给上游。图片可用值取决于图片渠道配置（如 `openai`、`gemini`、`replicate`）；视频使用任务插件标识（如 `sora`、`xai`）。不要把供应商示例理解为所有部署都可用。

生产环境应设置正确的外部 `ServerAddress`。`query_url` 和结果链接据此生成；未设置时可能返回相对路径。

### 3.2 幂等重试

同一 Token 和 `Idempotency-Key` 重放相同请求会返回原任务的 202 响应，并增加 `X-Idempotency-Replayed: true`。不同请求复用同一 Key 返回 409。一个业务操作使用一个 Key；新操作使用新 Key。

| 类型 | 判定“相同请求”的依据 |
|---|---|
| 图片 JSON / multipart | 同一提交路径和**完全相同的原始请求字节**。JSON 空白或字段顺序变化也会冲突；curl 重新构造 multipart boundary 后也可能冲突 |
| 视频 JSON | 同一提交路径和原始请求字节，包括 `provider` |
| 视频 multipart 生成 | 规范化 part 摘要；仅 boundary 随机变化不会创建新任务，字段顺序、part 头、文件名、声明 MIME 或内容变化仍会冲突 |

视频编辑、延长与生成是不同提交路径，不能跨路径复用同一个 Key。视频既有任务即使之后停用功能或删除渠道，仍可重放原受理响应。

## 4. 异步图片

### 4.1 生成与编辑怎么选

- `POST /v1/images/generations_async`：没有参考图时是文生图；有有效参考图时使用所选渠道声明的参考图生成或编辑能力。
- `POST /v1/images/edits_async`：明确要求编辑，**至少一张有效参考图**。只有 `prompt`、只有 `mask`，或参考图字段为空都会返回 400。

两个接口共用以下主要字段：

| 字段 | 类型 | 必需 / 默认 | 说明 |
|---|---|---|---|
| `model` | string | 必需 | 用户可见模型名；选定渠道后应用上游模型映射 |
| `prompt` | string | 必需 | 去除首尾空白后非空，最多 64 KiB |
| `provider` | string | 可选 | 仅用于网关选路 |
| `n` | integer | 默认 1 | 1–128，模型能力可设更小上限 |
| `size` | string | 可选 | 如 `1024x1024`、`auto`，或适配器支持的值 |
| `resolution` | string | 可选 | `1K`、`2K`、`4K`、`AUTO`；明确声明的 Gemini 模型还可允许 `0.5K` |
| `aspect_ratio` | string | 可选 | `auto` 或模型支持的比例 |
| `quality`、`output_format`、`background` | string | 可选 | 取值由模型能力与适配器决定 |
| `response_format` | string | 可选 | 上游请求格式提示；公共任务结果仍使用 URL |
| `image` | string / string[] | 可选 | JSON 参考图 URL 或 Data URI |
| `image_urls` | string[] | 可选 | JSON 参考图 URL 或 Data URI |
| `images` | object[] | 可选 | OpenAI 形状的 `{"image_url":"..."}`；不支持 `file_id` |
| `mask` | string / object | 可选 | URL 或 `{"image_url":"..."}`；必须同时提供参考图 |
| 供应商扩展参数 | JSON | 可选 | 由所选适配器转换、校验 |

显式 `0`、`0.0`、`false` 会保留并交给适配器；无效的 `n=0` 会在受理前拒绝。`batch_size`、`num_outputs`、`input.num_outputs`、`sequential_image_generation_options.max_images` 和 `parameters` 中的数量也受边界检查，不能与 `n` 冲突。

#### 示例 A：带远程参考图生成

参考图必须是网关或上游可读取的有效地址。此请求仍提交到 `generations_async`；渠道需要支持参考图能力。

```bash
curl -i -sS "$BASE_URL/v1/images/generations_async" \
  -H "Authorization: Bearer $NEW_API_TOKEN" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: image-reference-001' \
  --data-binary '{
    "provider": "openai",
    "model": "gpt-image-1",
    "prompt": "保留主体，将背景改为明亮摄影棚",
    "n": 1,
    "image_urls": ["https://images.example.com/reference.png"]
  }'
```

#### 示例 B：上传本地参考图进行编辑

```bash
curl -i -sS "$BASE_URL/v1/images/edits_async" \
  -H "Authorization: Bearer $NEW_API_TOKEN" \
  -H 'Idempotency-Key: image-edit-001' \
  -F 'provider=openai' \
  -F 'model=gpt-image-1' \
  -F 'prompt=保留主体轮廓，改成水彩插画' \
  -F 'image=@/实际路径/reference.png;type=image/png'
```

文件字段可重复使用 `image` / `image[]`，也可加 `mask` 文件。生成接口也接受相同的 multipart 参考图。文件会按实际格式与完整解码结果校验，不能只依赖扩展名或声明的 MIME。**图片 multipart 幂等重试**须保持完整请求字节一致；重新执行 `curl -F` 可能产生不同 boundary。

### 4.2 尺寸与渠道协议

| 图片协议 | 原生 `宽x高` 如何归档 | 额外说明 |
|---|---|---|
| OpenAI 及兼容图片协议 | 按**长边**：≤1024 为 1K，≤2048 为 2K，更大为 4K | 显式 `resolution` 与原生尺寸冲突时返回 400 |
| Gemini 图片协议 | 按**短边**归档 | `0.5K` 仅在模型能力声明后可用，按最低 1K 账单档处理 |

显式合法 `aspect_ratio` 优先于 `size` 中的比例。要精确匹配管理员设置的 `model_resolution` 渠道池，可以同时传匹配的 `resolution` 与实际 `size`。

异步资格来自渠道适配器声明的能力：OpenAI 及兼容渠道使用图片接口，Imagen 使用图片接口，Gemini 多模态图片模型使用 Gemini 原生生成协议。模型映射只改变实际上游模型名，权限、计费和任务展示仍保留客户端模型身份。Midjourney 等独立图片任务协议沿用原流程。

## 5. 异步视频

### 5.1 视频生成：`POST /v1/videos/generations_async`

支持 JSON 和 multipart。视频由任务插件执行；时长、尺寸、参考素材、模型组合和扩展参数以所选插件及模型能力为准，不同供应商的参数不能互换。

| 字段 | 类型 | 说明 |
|---|---|---|
| `model` | string，必需 | 用户可见视频模型名 |
| `prompt` | string | 文本提示词；部分图生视频插件允许参考图作为主要输入 |
| `provider` | string，可选 | 网关供应商筛选，不转发上游 |
| `seconds` / `duration` | number，可选 | 时长字段及兼容别名；具体边界由模型能力与插件校验 |
| `size`、`resolution`、`aspect_ratio` | string，可选 | 仅使用该模型声明支持的维度与取值 |
| `input_reference` / `image` | string 或文件，可选 | 首帧参考图；文件通过 multipart 提交 |
| `last_frame`、`reference_images`、`reference_videos`、`reference_audios` | 供应商规定的素材类型，可选 | 仅在相应视频模式支持时使用 |
| `generate_audio` 及其他扩展参数 | 供应商规定的类型，可选 | 由模型能力、插件与服务端校验 |

**示例 C：文生视频**

```bash
curl -i -sS "$BASE_URL/v1/videos/generations_async" \
  -H "Authorization: Bearer $NEW_API_TOKEN" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: video-generate-001' \
  --data-binary '{
    "provider": "sora",
    "model": "sora-2",
    "prompt": "清晨薄雾中的山间河流，镜头缓慢前移",
    "seconds": 4,
    "size": "1280x720"
  }'
```

**示例 D：上传参考图生成视频**

```bash
curl -i -sS "$BASE_URL/v1/videos/generations_async" \
  -H "Authorization: Bearer $NEW_API_TOKEN" \
  -H 'Idempotency-Key: video-reference-001' \
  -F 'provider=sora' \
  -F 'model=sora-2' \
  -F 'prompt=让画面中的河流缓慢流动' \
  -F 'seconds=4' \
  -F 'size=1280x720' \
  -F 'input_reference=@/实际路径/reference.png;type=image/png'
```

视频提交体最多读取 64 MiB；插件可对单个参考文件设更低限制。视频渠道池按 `size` 的**短边**归档：≤1024 为 1K，1025–2048 为 2K，>2048 为 4K；当前渠道池按模型和尺寸选路，不按时长选路。

### 5.2 视频编辑与延长

编辑、延长均**只接受 JSON**，必须使用声明了对应操作能力的模型，且请求中提供 `model`、`prompt`、`video`。`video` 可为字符串、`{"url":"..."}` 或 `{"file_id":"..."}`；允许的来源由插件决定。不可上传视频 multipart，也不可用 `source_task_id` 代替 `video`。

| 操作 | 路径 | 额外规则 |
|---|---|---|
| 编辑 | `POST /v1/videos/edits_async` | 模型能力可能固定继承源视频的时长、分辨率或比例，不能强行提交被锁定的维度 |
| 延长 | `POST /v1/videos/extensions_async` | 可传 `seconds` **或** `duration`，不能同时传；`extension_direction` 的允许值和默认值由插件决定 |

下列两个示例使用已配置 `xai` 插件的 `grok-imagine-video` 模型；示例源视频 URL 必须替换为实际可访问的 MP4 地址。此模型的编辑不接收时长或尺寸，延长示例选择其支持的向后延长方向。

**示例 E：编辑视频**

```bash
curl -i -sS "$BASE_URL/v1/videos/edits_async" \
  -H "Authorization: Bearer $NEW_API_TOKEN" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: video-edit-001' \
  --data-binary '{
    "provider": "xai",
    "model": "grok-imagine-video",
    "prompt": "把天空改成晚霞，保持镜头运动",
    "video": "https://media.example.com/source.mp4"
  }'
```

**示例 F：延长视频**

```bash
curl -i -sS "$BASE_URL/v1/videos/extensions_async" \
  -H "Authorization: Bearer $NEW_API_TOKEN" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: video-extend-001' \
  --data-binary '{
    "provider": "xai",
    "model": "grok-imagine-video",
    "prompt": "镜头继续向前推进",
    "video": "https://media.example.com/source.mp4",
    "seconds": 6,
    "extension_direction": "backward"
  }'
```

以上三个视频提交接口都返回同样的 202 结构：`task_id` 和 `query_url`，并通过统一任务接口查询。

### 5.3 视频保存

上游成功后，网关读取插件给出的普通下载 URL、需要上游认证的内容接口或内联数据；一个任务也可能有多个视频结果。视频先流式写入临时文件，再检查格式、最大字节数、媒体结构完整性、实际大小与 SHA-256，最后发布本站对象。

若保存失败，任务可能进入 `storage_failed`；恢复只重试保存，不重新调用上游，也不再次预扣或结算。部分可公开读取的上游结果在本地下载失败时可能以 `source: "upstream"` 直链回退；该链接不代表本站已保存成功，详见[第 6.3 节](#63-结果链接)。

## 6. 统一任务查询与结果

### 6.1 `GET /v1/media/tasks_async/{task_id}`

图片和视频使用同一查询入口，但字段并非完全相同。查询必须使用提交时的 Token。任务查询返回 HTTP 200 时也可能表示**任务失败**，应读取响应中的 `stage`、`status`，不能只看 HTTP 状态。

| 字段 | 含义 |
|---|---|
| `task_id`、`media_type` | 网关任务 ID；`image` 或 `video` |
| `provider`、`protocol` | 已冻结的供应商与执行协议 |
| `model`、`request_type` | 视频响应包含；图片公共查询不保证包含 |
| `stage` | 通用阶段：`queued`、`generating`、`saving`、`completed`、`failed` |
| `status` | 更细的任务状态；图片处理中常返回 `processing` |
| `progress` | 0–100 的展示进度 |
| `billing_status`、`storage_status` | 计费与存储状态 |
| `quota`、`cost` | 已知费用；图片公共查询提供 `quota`，视频还提供 `cost` |
| `error_code`、`fail_reason` / `error_message` | 失败信息；图片可有 601–613 公共错误码 |
| `data` | 结果数组；图片处理中通常省略，视频处理中为空数组 |

通用 `stage` 与常见内部状态关系：

| `stage` | 常见细分状态 | 客户端处理 |
|---|---|---|
| `queued` | `queued` | 等待后台执行 |
| `generating` | `processing`、`invoking`、`submitting`、`submitted` | 等待上游生成或轮询 |
| `saving` | `upstream_succeeded`、`uploading`、`storage_failed`、`billing_pending`、`billing_failed` | 等待存储或账务确认；必要时在任务中心恢复 |
| `completed` | `succeeded` | 结果已可用 |
| `failed` | `failed`、`execution_unknown`、`expired` | 展示失败说明，勿盲目重提 |

**处理中图片示例：**

```json
{
  "task_id": "asyncimg_example",
  "media_type": "image",
  "provider": "openai",
  "protocol": "openai_images",
  "stage": "generating",
  "status": "processing",
  "progress": 30,
  "billing_status": "pending",
  "storage_status": "pending",
  "quota": 0
}
```

**完成图片示例：**链接只是格式示意，实际查询会签发可用链接；`quota` 为示例值，不代表固定价格。

```json
{
  "task_id": "asyncimg_example",
  "media_type": "image",
  "provider": "openai",
  "protocol": "openai_images",
  "stage": "completed",
  "status": "succeeded",
  "progress": 100,
  "billing_status": "succeeded",
  "storage_status": "succeeded",
  "quota": 500,
  "data": [
    {
      "url": "https://api.your-domain.com/api/image-objects/obj_example/content?expires=EXPIRES&key=KEY&signature=SIGNATURE"
    }
  ]
}
```

**完成视频示例：**`quota`、`cost` 同样仅为示例值；实际响应还可能有更多元数据字段。

```json
{
  "id": "video_task_example",
  "task_id": "video_task_example",
  "media_type": "video",
  "provider": "sora",
  "platform": "sora",
  "protocol": "openai_video",
  "model": "sora-2",
  "request_type": "text_to_video",
  "stage": "completed",
  "status": "succeeded",
  "progress": 100,
  "billing_status": "settled",
  "storage_status": "succeeded",
  "quota": 500000,
  "cost": 1,
  "result_count": 1,
  "data": [
    {
      "key": "video",
      "url": "https://api.your-domain.com/v1/media/objects/obj_example?expires=EXPIRES&access=SIGNED_VALUE",
      "view_url": "https://api.your-domain.com/v1/media/objects/obj_example?expires=EXPIRES&access=SIGNED_VALUE",
      "content_type": "video/mp4",
      "byte_size": 10485760,
      "checksum": "SHA256_HEX",
      "expires_at": 1800000000
    }
  ]
}
```

**图片任务失败示例：**此时 HTTP 查询仍为 200。错误原因用于展示或排障；错误码表见[第 7 节](#7-错误和恢复)。

```json
{
  "task_id": "asyncimg_example",
  "media_type": "image",
  "stage": "failed",
  "status": "failed",
  "error_code": 602,
  "fail_reason": "Reference image could not be fetched"
}
```

### 6.2 哪些结果算完成

通常只在 `stage=completed`、`status=succeeded` 且 `data` 非空时把任务标记为成功。若 `stage=saving` 且 `storage_status=failed`，或 `stage=failed`，应展示错误并走任务恢复/排障流程。

图片动态上游任务在结果下载失败、且**全部结果均为可匿名访问的公网 HTTP(S) URL**时，查询可能仍携带 `data`，同时标记 `result_source: "upstream"`。此时任务仍失败或待恢复，链接未经本站图片校验，不能视为本站已保存结果或归档到图库。视频也可能在本地下载失败时给出 `data[].source: "upstream"`；同样应按 `status` 和 `storage_status` 判断任务状态。

### 6.3 结果链接

本站保存的**视频**结果 URL 带 `expires` 和 `access`，客户端必须原样保留全部查询参数。可直接使用查询响应中的 `data[0].url`：

```bash
export RESULT_URL='把实际响应中的 data[0].url 完整粘贴到这里'

curl -L "$RESULT_URL" -o result.mp4
curl -I "$RESULT_URL"
curl -H 'Range: bytes=0-1048575' "$RESULT_URL" -o first-part.bin
```

合法 Range 返回 206 和 `Content-Range`。本站视频签名链接默认有效 1 小时；过期后重新查询任务即可取得新链接。访问本站签名视频链接无需 Bearer 请求头，但服务端仍检查签名、过期时间、对象与任务、原 Token 状态及 IP 限制；无权或已过期时返回 404。视频落盘后读取本站链接不依赖上游继续在线。

本站保存的**图片**结果使用查询返回的签名 `data[].url`；链接过期时也应重新查询。若 `data` 标记为上游直链，则由上游提供访问与过期规则，不能假设它支持本站的 HEAD、Range 或签名刷新语义。

## 7. 错误和恢复

### 7.1 提交或查询接口报错

提交、查询失败的 HTTP 错误结构：

```json
{
  "error": {
    "type": "invalid_request_error",
    "code": "invalid_request",
    "message": "具体错误说明"
  }
}
```

| HTTP | 常见原因 |
|---:|---|
| 400 | JSON、multipart、参数、模型、能力或 `provider` 无效 |
| 401 | Token、用户或鉴权不可用 |
| 403 | IP、分组、模型、供应商或渠道权限不足 |
| 404 | 任务不属于当前 Token，或签名媒体对象不可访问 |
| 409 | 同一 `Idempotency-Key` 对应不同请求 |
| 413 | 图片或视频请求体超过网关限制 |
| 429 | Gemini SC 上传频率或容量限制，见兼容入口 |
| 503 | 数据库、Redis、稳定密钥、存储或后台能力不可用 |

**任务失败与 HTTP 错误是两回事。**一个已受理任务生成失败时，查询接口仍可能返回 HTTP 200，并在响应中给出 `stage=failed`、`status=failed` 和失败信息。

图片任务可返回以下公共错误码：

| `error_code` | 含义 |
|---:|---|
| 601 | 内容或安全策略拒绝 |
| 602 | 参考图拉取失败 |
| 603 | 图片容量暂不可用 |
| 604 | 请求或尺寸无效 |
| 605 | 上游频率限制 |
| 606 | 上游临时故障 |
| 607 | 图片结果缺失或无效 |
| 608 | 执行未知、超时或已结束 |
| 609 | 存储或账务确认失败 |
| 610 | 未分类上游错误 |
| 611 | 上游参考图数量超限 |
| 612 | 缺少上游要求的参考图 |
| 613 | 提示词或输入图无法处理 |

视频不使用 601–613 分类；结合 `status`、`stage`、`billing_status`、`storage_status` 和 `error_message` 判断。

### 7.2 重试、计费与恢复

- 图片成功后按实际产物固定账单；账单与日志写入有幂等保护。视频沿用任务预扣和上游结果结算。受理时冻结的价格快照不受后续价格修改影响。
- 阿里、Replicate 等带上游任务 ID 的图片适配器，以及已有上游 `Task` 的视频任务，重启后只继续轮询或保存，不重新生成。
- 无法确认上游是否已经接单时，任务进入 `execution_unknown`，不会自动重新调用上游。
- 图片存储或结算失败、视频本地保存失败，只重试后处理。视频上游已成功而本地保存失败时保留已产生的费用。
- `storage_failed` 应从用户或管理员任务中心执行恢复；不要重新提交生成请求来修复下载或磁盘问题。

## 8. 管理员启用条件

客户端能否使用接口取决于部署配置；以下仅列与异步任务直接相关的条件。更完整的渠道池、模型、权重设置见[调度配置说明](./异步图片与视频调度配置说明.md)。

| 异步图片 | 异步视频 |
|---|---|
| 打开异步图片功能；Redis 可用 | 打开“异步视频和本地存储”（默认关闭）；Redis 可用 |
| 设置稳定的任务加密密钥 | 使用同一套稳定的任务加密密钥，并设置稳定的 `CRYPTO_SECRET` 供视频链接签名 |
| 配置并启用 `temporary` 图片存储 | 配置服务器可写的本地视频存储目录 |
| 配置平台策略、模型能力、渠道池与价格 | 启用对应任务插件、渠道、模型能力和平台策略 |

任务加密密钥配置形状示例（占位值不可直接使用）：

```env
ASYNC_IMAGE_ACTIVE_KEY_ID=images-1
ASYNC_IMAGE_PAYLOAD_KEYS=images-1:<32字节随机密钥的Base64值>
ASYNC_IMAGE_REDIS_PREFIX=new-api:images
```

视频默认保留 90 天，本站签名链接默认有效 1 小时，单文件上限 1 GiB，单次下载超时 15 分钟，下载并发 2，存储重试 5 次；均以实际运行配置为准。

## 9. 旧版兼容入口

下列接口继续保留，**提交响应和查询格式沿用各自的旧协议**；新业务优先使用第 1 节的新提交入口和统一查询入口。

| 方法与路径 | 用途 |
|---|---|
| `POST /v1/images/generations_oa` | OpenAI 兼容异步图片生成，BB / SC 共用 |
| `POST /v1/images/edits_oa` | OpenAI 兼容异步图片编辑 |
| `POST /v1/chat/completions_gm` | Gemini BB，以 messages 提交图片任务 |
| `POST /v1/uploads/images_sc` | Gemini SC 单图上传，成功返回 200 |
| `POST /v1/images/generations_sc` | Gemini SC 图片生成 |
| `GET /v1/images/tasks_async/{task_id}` | 旧图片任务查询格式 |
| `GET /v1/tasks_sc/{task_id}` | Gemini SC 查询别名 |

旧 OpenAI / Gemini 异步提交与查询路径继续工作；原同步图片接口及原 `/v1/videos` 任务协议保持原语义。新统一查询可以读取新 `_async` 图片任务与异步视频任务；旧图片查询路径不会自动增加完整的统一媒体字段。
