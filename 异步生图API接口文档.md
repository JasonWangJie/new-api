# new-api 异步图片与异步视频 API 接口文档

日期：2026-09-19。源码仓库：[JasonWangJie/new-api](https://github.com/JasonWangJie/new-api)。本文按当前工作区实现说明接口，不把设计稿或上游厂商能力当成已实现行为。保留 new-api / QuantumNous 项目信息。

本文覆盖全渠道异步图片、异步视频、本地视频结果链接，以及既有 OpenAI / Gemini 异步兼容入口。渠道、模型、尺寸、优先级和权重的后台配置参见根目录的 [异步图片与视频调度配置说明.md](./异步图片与视频调度配置说明.md)。

## 1. 调用流程

统一流程为：

1. 客户端提交图片或视频请求。
2. 网关完成鉴权、参数和能力检查，冻结供应商、协议、模型映射、渠道及计费快照。
3. 网关先持久化任务，再返回 HTTP 202。
4. 后台 Worker 调用上游，轮询需要异步完成的上游任务。
5. 图片保存到已配置的图片存储；视频下载到服务器本地并完成格式、大小和校验和验证。
6. 客户端通过统一任务接口轮询，完成后读取 `data[].url`。

HTTP 202 只表示任务已经持久受理，不代表生成成功。客户端断开后，已经受理的任务仍由后台继续处理。

原 `/v1/images/generations`、`/v1/images/edits`、`/v1/videos`、聊天接口和原生供应商接口不会自动变成异步接口。

## 2. 接口总表

### 2.1 新的统一异步媒体接口

| 方法 | 路径 | 用途 | 成功 HTTP |
|---|---|---|---:|
| POST | `/v1/images/generations_async` | 全渠道异步图片生成或带参考图生成 | 202 |
| POST | `/v1/images/edits_async` | 全渠道异步图片编辑，必须有参考图 | 202 |
| POST | `/v1/videos/generations_async` | 异步视频生成，完成后保存到服务器本地 | 202 |
| GET | `/v1/media/tasks_async/{task_id}` | 图片和视频统一任务查询 | 200 |
| GET | `/v1/media/objects/{object_id}` | 读取签名视频结果，支持 Range | 200 / 206 |
| HEAD | `/v1/media/objects/{object_id}` | 读取签名视频元数据 | 200 / 206 |

### 2.2 保留的图片兼容接口

| 方法 | 路径 | 用途 | 成功 HTTP |
|---|---|---|---:|
| POST | `/v1/images/generations_oa` | OpenAI 兼容异步生成，BB 与 SC 共用 | 202 |
| POST | `/v1/images/edits_oa` | OpenAI 兼容异步编辑 | 202 |
| POST | `/v1/chat/completions_gm` | Gemini BB，以 messages 提交图片任务 | 202 |
| POST | `/v1/uploads/images_sc` | Gemini SC 单图上传 | 200 |
| POST | `/v1/images/generations_sc` | Gemini SC 图片生成 | 202 |
| GET | `/v1/images/tasks_async/{task_id}` | 旧图片任务查询 | 200 |
| GET | `/v1/tasks_sc/{task_id}` | Gemini SC 查询别名 | 200 |

兼容入口继续返回原有查询格式。新业务优先使用统一的 `_async` 提交入口和 `/v1/media/tasks_async/{task_id}` 查询入口。

## 3. Base URL、鉴权和通用请求头

假设服务地址为 `https://api.your-domain.com`：

```bash
export BASE_URL='https://api.your-domain.com'
export NEW_API_TOKEN='替换为 new-api 签发的 API Token'
```

`BASE_URL` 不带 `/v1`，也不带末尾 `/`。

| 请求头 | 必须 | 说明 |
|---|---|---|
| `Authorization: Bearer <NEW_API_TOKEN>` | 提交和查询必须 | 使用 new-api Token，不是上游 API Key |
| `Content-Type: application/json` | JSON 必须 | 可以带 charset |
| `Content-Type: multipart/form-data; boundary=...` | 文件提交必须 | 使用 curl `-F` 时由 curl 自动生成 |
| `Idempotency-Key` | 可选，强烈建议 | 最多 255 个 UTF-8 字节 |

Token 和用户必须可用；Token 不能禁用、过期或撤销，并需满足 IP、分组、模型和媒体平台权限。客户端提交的 `group` 不能覆盖服务端分组策略。

任务查询必须使用提交时的同一个 Token。同一用户使用另一个 Token 查询也会得到 404，从而避免对象 ID 被用于跨 Token 读取。

生产环境应设置正确的外部 `ServerAddress`。`query_url` 和签名结果链接会据此生成；未设置时可能返回相对路径。

## 4. provider：网关专用供应商选择

三个新提交接口都支持可选的 `provider`：

```json
{
  "provider": "openai",
  "model": "gpt-image-1",
  "prompt": "一座雪山"
}
```

- 指定后，只会在该供应商、分组、模型能力和渠道池中选择渠道。
- 未指定时，系统根据 Token 分组、模型能力、平台策略、渠道池、优先级和权重自动选择。
- 选定后，供应商、协议、上游模型映射和渠道会冻结到任务。
- `provider` 不会发送给上游。

图片常见供应商标识包括 `openai`、`gemini`、`ali`、`volcengine`、`jimeng`、`minimax`、`xai`、`replicate`、`zhipu_v4`、`siliconflow`、`vertex`、`azure`、`openrouter`、`xinference`、`newapi`、`sub2api` 和已注册的高级自定义渠道。

视频供应商来自任务插件，常见标识包括 `sora`、`alibaba`、`doubao`、`jimeng`、`kling`、`hailuo`、`vidu`、`google` 和 `vertex-ai`。实际可用值以服务器能力响应和管理员配置为准。

## 5. 全渠道异步图片生成

### 5.1 POST /v1/images/generations_async

支持 `application/json` 和 `multipart/form-data`。没有参考图时是文生图；存在有效参考图时进入该渠道声明的参考图生成或编辑能力。

常用 JSON 字段：

| 字段 | 类型 | 必须/默认 | 说明 |
|---|---|---|---|
| `model` | string | 是 | 用户可见模型名；模型映射在选定渠道后应用 |
| `prompt` | string | 是 | 去除首尾空白后非空，最多 64 KiB |
| `provider` | string | 否 | 网关专用供应商筛选，不转发上游 |
| `n` | integer | 否，默认 1 | 范围 1–128，并受模型能力更小上限约束 |
| `size` | string | 否 | 原生 `宽x高`、`auto` 或适配器支持的值 |
| `resolution` | string | 否 | `1K`、`2K`、`4K`、`AUTO`；Gemini 能力可额外允许 `0.5K` |
| `aspect_ratio` | string | 否 | `auto` 或模型支持的比例 |
| `quality` | string | 否 | 由模型能力和适配器决定 |
| `output_format` | string | 否 | 常见为 `png`、`jpeg`、`webp`，以能力声明为准 |
| `background` | string | 否 | 以能力声明为准 |
| `response_format` | string | 否 | 上游请求格式提示；公共任务结果仍返回 URL |
| `image` | string 或 string[] | 否 | 新统一入口支持的参考图 URL/Data URI |
| `image_urls` | string[] | 否 | 参考图 URL/Data URI |
| `images` | object[] | 否 | OpenAI 形状的 `{ "image_url": "..." }` |
| `mask` | string 或 object | 否 | URL，或 `{ "image_url": "..." }`；必须同时有参考图 |
| 供应商扩展参数 | JSON | 否 | 保留给所选适配器转换和校验 |

`images[].file_id` 不支持。显式 `0`、`0.0` 和 `false` 会被保留；无效的 `n=0` 会在受理前拒绝。`batch_size`、`num_outputs`、`input.num_outputs`、`sequential_image_generation_options.max_images` 及 `parameters` 中的数量不能绕过 1–128 边界，也不能与 `n` 冲突。

请求示例：

```bash
curl -i -sS "$BASE_URL/v1/images/generations_async" \
  -H "Authorization: Bearer $NEW_API_TOKEN" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: image-generate-001' \
  --data-binary '{
    "provider":"openai",
    "model":"gpt-image-1",
    "prompt":"清晨自然光下的陶瓷花瓶，简洁构图",
    "n":1,
    "resolution":"1K",
    "size":"1024x1024",
    "quality":"high"
  }'
```

带远程参考图：

```json
{
  "provider": "replicate",
  "model": "black-forest-labs/flux-schnell",
  "prompt": "保留主体，将背景改成摄影棚",
  "n": 1,
  "resolution": "1K",
  "image_urls": [
    "https://your-public-image-host.example/reference.png"
  ]
}
```

multipart 文件参考图：

```bash
curl -i -sS "$BASE_URL/v1/images/generations_async" \
  -H "Authorization: Bearer $NEW_API_TOKEN" \
  -H 'Idempotency-Key: image-file-001' \
  -F 'provider=openai' \
  -F 'model=gpt-image-1' \
  -F 'prompt=保留主体，将背景改成摄影棚' \
  -F 'resolution=1K' \
  -F 'image=@/实际路径/reference.png;type=image/png'
```

文件字段支持重复的 `image` / `image[]` 以及可选 `mask`。文件必须通过实际格式和完整解码校验，不能只依靠扩展名或声明 MIME。

### 5.2 POST /v1/images/edits_async

请求结构与生成接口相同，但必须至少提供一张有效参考图。只有 `prompt`、只有 `mask`，或参考图字段为空都会返回 400。

```bash
curl -i -sS "$BASE_URL/v1/images/edits_async" \
  -H "Authorization: Bearer $NEW_API_TOKEN" \
  -H 'Idempotency-Key: image-edit-001' \
  -F 'provider=openai' \
  -F 'model=gpt-image-1' \
  -F 'prompt=保留主体轮廓，改成水彩插画' \
  -F 'image=@/实际路径/reference.png;type=image/png' \
  -F 'resolution=1K'
```

### 5.3 图片协议选择

异步资格来自渠道适配器声明的能力，不通过渠道类型白名单或模型名包含 `image` 来猜测。

- OpenAI 及兼容渠道使用图片接口协议。
- Imagen 使用图片接口，而不是 Gemini 多模态文本协议。
- Gemini 多模态图片模型使用 Gemini 原生生成协议。
- 模型映射决定实际上游模型名，权限、计费和任务展示仍保留客户端模型身份。
- Midjourney 等独立图片任务协议保持原流程，不进入统一图片入口。

## 6. 图片尺寸规则

### 6.1 OpenAI 及兼容图片协议

- 未指定显式档位的原生 `宽x高` 按长边归档：长边不超过 1024 为 1K，不超过 2048 为 2K，更大为 4K。
- 显式 `resolution` 与原生尺寸冲突时返回 400。
- 显式合法比例优先于 `size` 中的比例。

### 6.2 Gemini 图片协议

- 原生尺寸按短边归档。
- `0.5K` 只在模型目录明确允许时可用，并按最低 1K 账单档处理。

为了稳定匹配管理员设置的 `model_resolution` 渠道池，建议同时发送明确的 `resolution` 和实际 `size`。

## 7. 异步视频生成

### 7.1 POST /v1/videos/generations_async

请求采用现有 OpenAI Video 形状，支持 JSON 和 multipart。通用字段如下：

| 字段 | 类型 | 必须/默认 | 说明 |
|---|---|---|---|
| `model` | string | 是 | 用户可见视频模型名 |
| `prompt` | string | 依插件 | 文本提示词；部分图生视频插件允许由参考图提供主要输入 |
| `provider` | string | 否 | 网关专用供应商筛选，不转发上游 |
| `seconds` | number | 否 | 视频时长；兼容 `duration` 别名 |
| `duration` | number | 否 | `seconds` 的兼容字段 |
| `size` | string | 否 | 常见为 `1280x720`、`1920x1080` 等 |
| `input_reference` | string 或 file | 否 | JSON 中为插件支持的引用；multipart 中为参考图片文件 |
| 供应商扩展参数 | JSON / form field | 否 | 由选中的任务插件校验和转换 |

每个视频插件可以进一步限制时长、分辨率、参考图 MIME、模型组合或其他参数。客户端不能假设一个供应商接受的扩展字段会被另一个供应商接受。

JSON 文生视频示例：

```bash
curl -i -sS "$BASE_URL/v1/videos/generations_async" \
  -H "Authorization: Bearer $NEW_API_TOKEN" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: video-generate-001' \
  --data-binary '{
    "provider":"sora",
    "model":"sora-2",
    "prompt":"清晨薄雾中的山间河流，镜头缓慢前移",
    "seconds":4,
    "size":"1280x720"
  }'
```

multipart 图生视频示例：

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

整个视频提交请求最多读取 64 MiB；插件可对参考文件设置更低的限制。

### 7.2 视频尺寸对应的渠道池档位

视频按 `size` 的较短边选择渠道池：

| 请求尺寸 | 档位 |
|---|---|
| 短边不超过 1024，例如 `1280x720` | 1K |
| 短边为 1025–2048，例如 `1920x1080` | 2K |
| 短边超过 2048，例如 `3840x2160` | 4K |

当前渠道池按模型和尺寸调度，不按视频时长调度。

### 7.3 视频本地保存

上游任务成功后，网关通过任务插件提供的内容描述读取视频。内容可以来自：

- 普通下载 URL。
- 需要上游认证的内容接口。
- 插件返回的内联数据。
- 一个任务产生的多个视频结果。

视频以流方式写入临时文件。服务端在原子发布前会检查：

- 内容确实是支持的视频格式，而不是 HTML 错误页等伪装内容。
- 文件没有超过配置的最大字节数。
- 内容完整，能够通过媒体结构校验。
- 实际大小和 SHA-256 校验和已记录。

保存失败会进入 `storage_failed`，只重试保存过程，不会再次调用上游生成，也不会产生新的预扣或结算。

## 8. 受理响应和幂等

三个新接口成功后均返回 HTTP 202：

```json
{
  "task_id": "asyncimg_example_or_video_task",
  "query_url": "https://api.your-domain.com/v1/media/tasks_async/asyncimg_example_or_video_task"
}
```

响应头：

```http
HTTP/1.1 202 Accepted
Location: https://api.your-domain.com/v1/media/tasks_async/...
Retry-After: 3
Cache-Control: no-store
```

幂等重放成功时额外返回：

```http
X-Idempotency-Replayed: true
```

### 8.1 图片幂等规则

相同 Token 和 `Idempotency-Key` 必须使用：

- 同一个提交路径。
- 完全相同的原始请求字节。
- 相同 JSON 空白、字段顺序和值。
- multipart 时相同的完整 multipart 字节；普通 curl `-F` 重建 boundary 后可能被视为不同请求。

请求摘要不同返回 HTTP 409。

### 8.2 视频幂等规则

- JSON 按原始请求字节判断，包含网关 `provider` 字段。
- multipart 使用规范化 part 摘要，随机 boundary 变化不会创建新任务。
- multipart 的字段顺序、part 头、文件名、声明 MIME 或内容变化仍会产生冲突。
- 已存在的幂等任务可以在视频功能后来关闭、渠道后来删除的情况下继续重放原受理响应。

## 9. 统一任务查询

### 9.1 GET /v1/media/tasks_async/{task_id}

```bash
curl -sS "$BASE_URL/v1/media/tasks_async/YOUR_TASK_ID" \
  -H "Authorization: Bearer $NEW_API_TOKEN"
```

建议至少每 3 秒查询一次。网络错误、503 或其他 5xx 时，延长到 5–30 秒；单次查询失败不等于任务失败。

统一响应中的主要字段：

| 字段 | 说明 |
|---|---|
| `task_id` | 网关任务 ID |
| `media_type` | `image` 或 `video` |
| `provider` | 已冻结的供应商 |
| `protocol` | 图片实际协议或 `openai_video` |
| `model` | 用户请求的模型名；视频响应包含 |
| `status` | 详细任务状态 |
| `stage` | 稳定阶段：`queued`、`generating`、`saving`、`completed`、`failed` |
| `progress` | 0–100 的展示进度 |
| `billing_status` | 计费状态 |
| `storage_status` | 存储状态 |
| `quota` / `cost` | 已知费用；视频提供两者，图片至少提供 quota |
| `error_code` / `error_message` / `fail_reason` | 失败原因；图片兼容 601–613 |
| `data` | 任务完成后的全部结果；未完成时省略或为空数组 |

处理中图片示例：

```json
{
  "task_id": "asyncimg_example",
  "media_type": "image",
  "provider": "replicate",
  "protocol": "openai_images",
  "stage": "generating",
  "status": "processing",
  "progress": 30,
  "billing_status": "pending",
  "storage_status": "pending"
}
```

完成图片示例：

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
    {"url":"https://api.your-domain.com/api/image-objects/..."}
  ]
}
```

完成视频示例：

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
      "url": "https://api.your-domain.com/v1/media/objects/obj_example?expires=1800000000&access=SIGNED_VALUE",
      "view_url": "https://api.your-domain.com/v1/media/objects/obj_example?expires=1800000000&access=SIGNED_VALUE",
      "content_type": "video/mp4",
      "byte_size": 10485760,
      "checksum": "完整的 SHA-256 十六进制值",
      "expires_at": 1800000000
    }
  ]
}
```

### 9.2 stage 与详细 status

| `stage` | 常见 `status` | 说明 |
|---|---|---|
| `queued` | `queued` | 等待后台执行 |
| `generating` | `processing`、`invoking`、`submitting`、`submitted` | 调用或轮询上游 |
| `saving` | `upstream_succeeded`、`uploading`、`storage_failed`、`billing_pending`、`billing_failed` | 上游已完成，正在存储或确认账务 |
| `completed` | `succeeded` | 结果已经完整保存并可以读取 |
| `failed` | `failed`、`execution_unknown`、`expired` | 生成失败、执行状态不可确认或结果已过期 |

只根据 `stage` 构建通用进度界面；需要恢复或排障时再查看详细 `status`、`billing_status` 和 `storage_status`。

## 10. 视频结果链接

视频 `data[].url` 是带 `expires` 和 `access` 的本站签名链接。客户端必须完整保留查询参数。

### 10.1 完整下载

```bash
curl -L -o result.mp4 \
  'https://api.your-domain.com/v1/media/objects/YOUR_OBJECT_ID?expires=1800000000&access=SIGNED_VALUE'
```

### 10.2 HEAD

```bash
curl -I \
  'https://api.your-domain.com/v1/media/objects/YOUR_OBJECT_ID?expires=1800000000&access=SIGNED_VALUE'
```

### 10.3 Range 播放或分段下载

```bash
curl -H 'Range: bytes=0-1048575' \
  'https://api.your-domain.com/v1/media/objects/YOUR_OBJECT_ID?expires=1800000000&access=SIGNED_VALUE'
```

合法 Range 返回 HTTP 206，并带 `Content-Range`。签名链接默认有效 1 小时；重新查询任务即可签发新链接。

链接不要求 Bearer 请求头，但服务端仍会验证：

- 签名和过期时间。
- 对象仍属于已成功保存的任务。
- 原始 Token 仍属于原用户。
- 原始 Token 没有禁用、过期或撤销。
- 当前客户端 IP 仍满足 Token 的 IP 限制。
- 对象和任务没有被清理或过期。

失败统一返回 404，避免泄露对象是否存在。视频保存完成后，读取本站链接不再依赖上游继续在线。

## 11. 错误响应

受理和查询失败使用：

```json
{
  "error": {
    "type": "invalid_request_error",
    "code": "invalid_request",
    "message": "具体错误说明"
  }
}
```

常见 HTTP 状态：

| HTTP | 场景 |
|---:|---|
| 400 | JSON、multipart、参数、模型、能力或 provider 无效 |
| 401 | Token、用户或鉴权不可用 |
| 403 | 分组、模型、供应商或渠道没有权限或不可用 |
| 404 | 任务不属于当前 Token，或签名媒体对象不可访问 |
| 409 | 同一 Idempotency-Key 对应不同请求，或任务阶段不允许操作 |
| 413 | 图片或视频提交体超过网关限制 |
| 429 | Gemini SC 上传频率或容量限制 |
| 503 | 数据库、Redis、稳定密钥、存储或后台能力不可用 |

图片已受理任务继续使用公共错误码：

| 错误码 | 含义 |
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

视频不使用 601–613 分类，需结合 `status`、`stage`、`billing_status`、`storage_status` 和 `error_message` 判断生成失败还是保存失败。

## 12. 计费、重试和恢复

### 12.1 图片

- 任务成功后按实际产物固定账单。
- 账单和日志写入有幂等保护。
- 已冻结账单不受后续价格修改影响。
- 阿里、Replicate 等返回上游任务 ID 的适配器会持久化 ID，重启后只恢复轮询。
- 如果无法确认是否已调用上游，任务进入 `execution_unknown`，不会自动重新生图。
- 存储或结算失败只重试后处理，不重新调用上游。

### 12.2 视频

- 视频沿用现有任务预扣和上游结果结算。
- 受理阶段冻结价格及计费表达式快照。
- 网关任务入库后，后台才提交上游。
- 已经存在上游 `model.Task` 时，后台只继续轮询或保存。
- 上游成功而本地保存失败时保留已产生的费用。
- 保存重试不会生成第二个上游任务，也不会重复收费。
- 上游提交边界无法确认时进入 `execution_unknown`，禁止自动重试生成。

如果任务是 `storage_failed`，应从用户或管理员任务中心执行恢复。不要重新提交生成请求来修复下载或磁盘问题。

## 13. 管理员启用条件

### 13.1 异步图片

- 打开异步图片功能。
- Redis 可用。
- 设置稳定的 `ASYNC_IMAGE_ACTIVE_KEY_ID` 和 `ASYNC_IMAGE_PAYLOAD_KEYS`。
- 配置并启用 `temporary` 图片存储。
- 配置平台策略、模型能力、渠道池和价格。

示例：

```env
ASYNC_IMAGE_ACTIVE_KEY_ID=images-1
ASYNC_IMAGE_PAYLOAD_KEYS=images-1:<32字节随机密钥的Base64值>
ASYNC_IMAGE_REDIS_PREFIX=new-api:images
```

### 13.2 异步视频

- 打开“异步视频和本地存储”；默认关闭。
- Redis 可用。
- 使用与图片任务相同的稳定任务加密密钥。
- 设置稳定的 `CRYPTO_SECRET`，用于视频签名链接。
- 配置服务器可写的本地存储目录。
- 启用对应视频任务插件、渠道、模型能力和平台策略。

默认值：

| 设置 | 默认值 |
|---|---:|
| 视频保留期 | 90 天 |
| 签名链接有效期 | 1 小时 |
| 单文件上限 | 1 GiB |
| 单次下载超时 | 15 分钟 |
| 下载并发 | 2 |
| 存储重试次数 | 5 |

## 14. 客户端实现建议

1. 每个业务操作生成唯一 `Idempotency-Key`，网络重试复用该 Key 和完全相同的请求体。
2. 收到 202 后立即保存 `task_id` 和 `query_url`。
3. 优先使用服务器返回的 `query_url`，不要自行拼接路径。
4. 至少每 3 秒查询一次，5xx 和网络错误使用指数退避。
5. 根据 `stage` 展示排队、生成、保存、完成和失败。
6. 只有 `status=succeeded` 且 `data` 非空时才展示结果。
7. 视频链接过期后重新查询任务，不要长期保存签名 URL。
8. 播放器应支持 Range；下载前可先发 HEAD。
9. `storage_failed` 应走任务恢复，不要重新生成。
10. 记录 `provider`、`protocol`、`billing_status` 和 `storage_status`，便于排查供应商、计费与磁盘问题。

## 15. 与旧客户端的兼容

- 旧 OpenAI / Gemini 异步提交和查询路径继续工作。
- 原同步图片接口保持同步语义。
- 原 `/v1/videos` 任务协议保持原行为。
- 新统一查询可以读取新 `_async` 图片任务和异步视频任务。
- `/v1/images/tasks_async/{task_id}` 和 `/v1/tasks_sc/{task_id}` 继续返回旧图片格式，不会自动增加完整统一媒体字段。
- 新代码应逐步迁移到 `/v1/media/tasks_async/{task_id}`，以统一处理图片和视频。
