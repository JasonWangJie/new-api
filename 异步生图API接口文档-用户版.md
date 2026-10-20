# 异步图片与视频 API 接口文档

本文面向 API 调用方，介绍异步图片生成、图片编辑、视频生成、视频编辑、视频延长、任务查询与当日统计。可用模型、供应商和参数范围以您已开通的服务为准。

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

**关键区别：**提交返回的 202 仅表示任务已持久受理；生成、保存和结算仍可能失败。原同步 `POST /v1/images/generations`、`POST /v1/images/edits`、`POST /v1/videos` 不会自动转为这些异步接口。旧版 OpenAI / Gemini 异步入口见[第 8 节](#8-兼容接口)。

## 2. 最短调用流程：文生图

以下示例使用 Bash 和 curl。先设置 API 服务地址和您的 API Key；`BASE_URL` 不含 `/v1`，也不带末尾斜杠。Windows PowerShell 请使用 `curl.exe`，并将 JSON 请求体另存为 UTF-8 文件后通过 `--data-binary '@request.json'` 提交；Bash 的续行写法不能直接用于 PowerShell。

```bash
export BASE_URL='https://api.your-domain.com'
export API_KEY='替换为您的 API Key'
```

**第一步：提交。**示例中的 `openai`、`gpt-image-1` 须为您的 API Key 可调用的供应商和模型；也可省略 `provider`，由服务自动选择。

```bash
curl -i -sS "$BASE_URL/v1/images/generations_async" \
  -H "Authorization: Bearer $API_KEY" \
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
  -H "Authorization: Bearer $API_KEY"
```

**第三步：读取结果。**当 `stage=completed`、`status=succeeded` 且 `data` 非空时，使用 `data[].url`。图片完整响应示例见[第 6 节](#6-统一任务查询与结果)。查询间隔建议不短于 3 秒；网络错误或 5xx 时逐步延长到 5–30 秒，不要把一次查询失败当成任务失败。

## 3. 所有新异步入口的共同规则

### 3.1 鉴权与请求头

| 请求头 | 适用范围 | 说明 |
|---|---|---|
| `Authorization: Bearer <API_KEY>` | 提交与任务查询必需 | 使用本服务提供的 API Key |
| `Content-Type: application/json` | JSON 提交必需 | 可附带 charset |
| `Content-Type: multipart/form-data` | 文件提交必需 | curl `-F` 会自动生成 boundary，勿手填 |
| `Idempotency-Key` | 提交可选，建议设置 | 最多 255 个 UTF-8 字节；重试同一业务操作时复用 |

API Key 须有效，账户状态、IP 访问限制、模型和供应商权限须满足服务要求。请求中的 `group` 不会改变已有账户权限。

服务先校验请求并受理任务，返回 202 后继续异步生成、保存结果和结算。客户端断开连接不会取消已受理的任务。

新提交接口均可选填 `provider`。填写后仅使用指定供应商，不填写则由服务自动选择。图片供应商示例为 `openai`、`gemini`、`replicate`，视频供应商示例为 `sora`、`xai`、`drama`。请仅使用您已开通的供应商和模型；`provider` 不会转发为生成模型的原生参数。

`query_url` 和结果链接可能是完整 URL，也可能是以 `/` 开头的相对路径；收到相对路径时，将其拼接到 `BASE_URL` 后请求，并保留全部查询参数。

### 3.2 幂等重试

同一 Token 和 `Idempotency-Key` 重放相同请求会返回原任务的 202 响应，并增加 `X-Idempotency-Replayed: true`。不同请求复用同一 Key 返回 409。一个业务操作使用一个 Key；新操作使用新 Key。

| 类型 | 判定“相同请求”的依据 |
|---|---|
| 图片 JSON / multipart | 同一提交路径和**完全相同的原始请求字节**。JSON 空白或字段顺序变化也会冲突；curl 重新构造 multipart boundary 后也可能冲突 |
| 视频 JSON | 同一提交路径和原始请求字节，包括 `provider` |
| 视频 multipart 生成 | 规范化 part 摘要；仅 boundary 随机变化不会创建新任务，字段顺序、part 头、文件名、声明 MIME 或内容变化仍会冲突 |

视频编辑、延长与生成是不同提交路径，不能跨路径复用同一个 Key。

## 4. 异步图片

### 4.1 生成与编辑怎么选

- `POST /v1/images/generations_async`：没有参考图时是文生图；有有效参考图时，模型须支持参考图生成或编辑。
- `POST /v1/images/edits_async`：明确要求编辑，**至少一张有效参考图**。只有 `prompt`、只有 `mask`，或参考图字段为空都会返回 400。

两个接口共用以下主要字段：

| 字段 | 类型 | 必需 / 默认 | 说明 |
|---|---|---|---|
| `model` | string | 必需 | 您已开通的图片模型名 |
| `prompt` | string | 必需 | 去除首尾空白后非空，最多 64 KiB |
| `provider` | string | 可选 | 指定供应商；省略时由服务自动选择 |
| `n` | integer | 默认 1 | 1–128，具体模型可能有更小上限 |
| `size` | string | 可选 | 如 `1024x1024`、`auto`，或模型支持的值 |
| `resolution` | string | 可选 | `1K`、`2K`、`4K`、`AUTO`；部分 Gemini 模型还支持 `0.5K` |
| `aspect_ratio` | string | 可选 | `auto` 或模型支持的比例 |
| `quality`、`output_format`、`background` | string | 可选 | 取值以所选模型支持的范围为准 |
| `response_format` | string | 可选 | 上游请求格式提示；公共任务结果仍使用 URL |
| `image` | string / string[] | 可选 | JSON 参考图 URL 或 Data URI |
| `image_urls` | string[] | 可选 | JSON 参考图 URL 或 Data URI |
| `images` | object[] | 可选 | OpenAI 形状的 `{"image_url":"..."}`；不支持 `file_id` |
| `mask` | string / object | 可选 | URL 或 `{"image_url":"..."}`；必须同时提供参考图 |
| 供应商扩展参数 | JSON | 可选 | 仅使用所选模型支持的参数 |

显式 `0`、`0.0`、`false` 会保留；具体值须符合所选模型要求，无效的 `n=0` 会在受理前拒绝。`batch_size`、`num_outputs`、`input.num_outputs`、`sequential_image_generation_options.max_images` 和 `parameters` 中的数量也受边界检查，不能与 `n` 冲突。

#### 示例 A：带远程参考图生成

参考图必须是服务可读取的有效地址。此请求仍提交到 `generations_async`；所选模型须支持参考图。

```bash
curl -i -sS "$BASE_URL/v1/images/generations_async" \
  -H "Authorization: Bearer $API_KEY" \
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
  -H "Authorization: Bearer $API_KEY" \
  -H 'Idempotency-Key: image-edit-001' \
  -F 'provider=openai' \
  -F 'model=gpt-image-1' \
  -F 'prompt=保留主体轮廓，改成水彩插画' \
  -F 'image=@/实际路径/reference.png;type=image/png'
```

文件字段可重复使用 `image` / `image[]`，也可加 `mask` 文件。生成接口也接受相同的 multipart 参考图。文件会按实际格式与完整解码结果校验，不能只依赖扩展名或声明的 MIME。**图片 multipart 幂等重试**须保持完整请求字节一致；重新执行 `curl -F` 可能产生不同 boundary。

### 4.2 尺寸与分辨率

| 图片协议 | 原生 `宽x高` 对应的分辨率档位 | 额外说明 |
|---|---|---|
| OpenAI 及兼容图片协议 | 按**长边**：≤1024 为 1K，≤2048 为 2K，更大为 4K | 显式 `resolution` 与原生尺寸冲突时返回 400 |
| Gemini 图片协议 | 按**短边**归档 | `0.5K` 仅在所选模型支持时可用，按最低 1K 账单档处理 |

显式合法 `aspect_ratio` 优先于 `size` 中的比例。同时传入 `resolution` 与实际 `size` 时，应使用相互匹配的值。

OpenAI 兼容图片模型、Imagen 和 Gemini 多模态图片模型可通过本节异步接口提交；具体支持范围以您的服务为准。Midjourney 等独立图片任务使用各自的接口。

## 5. 异步视频

视频调用按 **选择供应商与型号 → 提交 → 保存任务号 → 查询 → 下载结果** 执行。以下示例分别标明供应商和用途，不能只替换 `provider` 就把同一请求用于其他模型。

`BASE_URL` 是 API 服务根地址，不带 `/v1`；`API_KEY` 是您的 API Key。每个新请求使用新的幂等键；同一请求超时重试时保留原键和原请求内容。

### 5.1 视频生成：`POST /v1/videos/generations_async`

视频生成支持 JSON 和 multipart，具体格式由供应商和模型决定。例如 Sora / xAI 可接收本地参考图片文件；`doubao` Seedance 和 `drama` 使用 JSON 素材引用，不接收 multipart 文件上传。客户端提交后只需查询本服务返回的任务，无需直接请求供应商接口。

| 字段 | 类型 | 说明 |
|---|---|---|
| `model` | string，必需 | 用户可见视频模型名 |
| `prompt` | string | 文本提示词；部分图生视频模型允许参考图作为主要输入 |
| `provider` | string，可选 | 指定供应商，省略时自动选择 |
| `seconds` / `duration` | number，按模型要求 | 时长以秒为单位，建议只填一个字段；Drama G 生成必须显式填写。Drama A 系列与 `minimax-h3` 允许同时填写相同值，详见示例 D7；具体范围以模型要求为准 |
| `size`、`resolution`、`aspect_ratio` | string，可选 | 仅使用该模型声明支持的维度与取值 |
| `input_reference` / `image` | URL string / object 或 multipart 文件，按模型要求 | xAI / Drama G 等可表示首帧；E / F 仅作普通参考。使用哪一个别名和来源取决于模型，不要重复发送同一图片 |
| `last_frame`、`reference_images`、`reference_videos`、`reference_audios` | 供应商规定的素材类型，可选 | 仅在相应视频模式支持时使用 |
| `generate_audio` 及其他扩展参数 | 供应商规定的类型，可选 | 仅使用所选模型支持的参数和值 |

#### 5.1.1 参考图文件和 URL 到底怎么写

| 目的 | 写法 | 本节适用示例 |
| --- | --- | --- |
| 上传本机图片文件 | multipart 文件 part：`-F 'input_reference=@/实际路径/reference.png;type=image/png'` | 示例 D：Sora；此处的 `@` 是 curl 读取本机文件的语法 |
| 单张首帧 URL，JSON 字符串 | `"image": "https://cdn.example.com/first.png"` | xAI、Drama G；Drama E / F 的普通参考使用下方数组 |
| 单张首帧 URL，JSON 对象 | `"image": {"url": "https://cdn.example.com/first.png"}` | 示例 D1：xAI；Drama G 也接受这种 URL 对象 |
| 多张普通参考图，URL 字符串数组 | `"reference_images": ["https://cdn.example.com/a.png", "https://cdn.example.com/b.png"]` | 示例 D4：Drama E / F；对应普通参考模式 |
| 多张普通参考图，URL 对象数组 | `"reference_images": [{"url": "https://cdn.example.com/a.png"}, {"url": "https://cdn.example.com/b.png"}]` | 示例 D3：xAI 1.5 |
| 首尾帧 | `"image": "https://cdn.example.com/first.png", "last_frame": "https://cdn.example.com/last.png"` | 示例 D6：Drama G 2.5；同时给两帧，比例用 `adaptive` |
| xAI multipart 文本字段内放 URL | `-F 'image={"url":"https://cdn.example.com/first.png"}'` | 示例 D2：字段内容是 JSON 对象文本，不是上传文件；不能照搬到 Sora 或 JSON-only 模型 |

注意：

- **JSON 里传 URL 不加 `@`**；`"image": "@reference.png"`、`"image": "C:\\pics\\reference.png"` 都不会替你上传文件。裸 Base64 也不是 URL；完整 Data URI 仅在模型明确支持时使用，不能套用到所有 Seedance 型号。
- `reference_images` 是数组，不写成 `"https://a.png,https://b.png"`，也不把数组整体包成一个 JSON 字符串。不要把聊天接口的 `messages[].content` 或图片接口的字段格式直接套到所有视频模型。
- 图地址应直接返回图片内容，优先公网 HTTPS，可由上游服务器读取，无需浏览器登录、Cookie 或另加 Authorization。网盘分享页、需要登录的相册页、`localhost`、内网路径不能当作可访问的参考图地址；地址含签名查询参数时完整保留，并留足生成期间的有效期。
- 图像格式、分辨率、大小、比例、张数及内容要求继续由具体上游校验。“浏览器能看见图片”不能代替上游可读取性检查。下列 `cdn.example.com` 均为占位地址，必须替换。
- Drama E / F 普通参考使用公网 HTTPS；Drama G 和 `doubao` 还可使用服务有权访问的供应商素材引用 `asset://ID`。它不是本地文件路径或本站任务号。
- `/v1/uploads/images_sc` 是 Gemini SC 专用临时上传接口，不是通用视频素材上传服务。使用 URL-only 的视频模型时，先上传到你自己的可公开读取的对象存储/图床，再传所得 URL。

#### 5.1.2 各种视频生成示例

**示例 C：文生视频**

适用：`provider=sora`、`model=sora-2`。本例没有参考图，生成 4 秒、1280×720 的视频。`size` 是输出尺寸，`seconds` 是输出时长；不能将这组参数直接用于 Drama 模型。

```bash
curl -i -sS "$BASE_URL/v1/videos/generations_async" \
  -H "Authorization: Bearer $API_KEY" \
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

适用：Sora 的文件参考图请求。将 `/实际路径/reference.png` 换成**执行 curl 的那台电脑**上的文件；`input_reference` 是文件字段名，`@` 使 curl 读取文件，`;type=image/png` 声明 MIME。JPEG 文件相应改为 `type=image/jpeg`。图片还需满足当前上游的图生视频要求。

```bash
curl -i -sS "$BASE_URL/v1/videos/generations_async" \
  -H "Authorization: Bearer $API_KEY" \
  -H 'Idempotency-Key: video-reference-001' \
  -F 'provider=sora' \
  -F 'model=sora-2' \
  -F 'prompt=让画面中的河流缓慢流动' \
  -F 'seconds=4' \
  -F 'size=1280x720' \
  -F 'input_reference=@/实际路径/reference.png;type=image/png'
```

使用 `-F` 时由 curl 自动设置 `multipart/form-data` 与 boundary，**不要另加 `Content-Type: application/json`**。已有图片 URL 时，选择下面的 URL 示例；不要把本例的文件路径直接替换成 URL 并期待所有模型自动下载。

**示例 D1：单张参考图 URL 作为首帧（xAI，JSON）**

适用：`xai` 的 `grok-imagine-video-1.5`。本例 `image` 为 URL 对象，作为首帧；也可写成 `"image": "https://cdn.example.com/first.png"`，两种写法选一种。`720p` 和 `16:9` 是输出规格。

```bash
curl -i -sS "$BASE_URL/v1/videos/generations_async" \
  -H "Authorization: Bearer $API_KEY" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: video-image-url-001' \
  --data-binary '{
    "provider": "xai",
    "model": "grok-imagine-video-1.5",
    "prompt": "以参考图为首帧，镜头缓慢推进",
    "image": {"url": "https://cdn.example.com/first.png"},
    "seconds": 8,
    "resolution": "720p",
    "aspect_ratio": "16:9"
  }'
```

**示例 D2：multipart 中填写参考图 URL（仅此 xAI 写法）**

适用：同一 xAI 型号。这里 `image` 是**文本字段**，内容为 JSON 对象文本。URL 前没有 `@`，请求不会从本机读取图片。与示例 D 的 Sora 文件 part 是两种不同格式；Drama 不接收本例的 multipart 请求。

```bash
curl -i -sS "$BASE_URL/v1/videos/generations_async" \
  -H "Authorization: Bearer $API_KEY" \
  -H 'Idempotency-Key: video-multipart-url-001' \
  -F 'provider=xai' \
  -F 'model=grok-imagine-video-1.5' \
  -F 'prompt=以参考图为首帧，镜头缓慢推进' \
  -F 'seconds=8' -F 'resolution=720p' -F 'aspect_ratio=16:9' \
  -F 'image={"url":"https://cdn.example.com/first.png"}'
```

**示例 D3：多张普通参考图 URL（xAI 1.5，JSON）**

适用：`grok-imagine-video-1.5` 的普通参考模式；经典 `grok-imagine-video` 不支持此多参考模式。两张图放入 `reference_images` 数组，分别提供人物和场景；它们不是两张“首帧”，也不是首尾帧。本例使用此模式允许的 720p。

```bash
curl -i -sS "$BASE_URL/v1/videos/generations_async" \
  -H "Authorization: Bearer $API_KEY" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: video-reference-urls-001' \
  --data-binary '{
    "provider": "xai",
    "model": "grok-imagine-video-1.5",
    "prompt": "保持第一张图的人物，使用第二张图的场景",
    "reference_images": [
      {"url": "https://cdn.example.com/character.png"},
      {"url": "https://cdn.example.com/setting.png"}
    ],
    "seconds": 8,
    "resolution": "720p",
    "aspect_ratio": "16:9"
  }'
```

**示例 D4：Drama E / F 使用参考图 URL（JSON）**

适用：`drama` 的 E / F 系列普通参考模式。本例用 `seedance2.5-F`，`reference_images` 是 URL 字符串数组；增加图片时继续在数组中添加字符串。参考图用于内容引导，**不保证固定为首帧**。改用 E 时需使用该 E 型号支持的分辨率；`seedance2.0-F` 必须使用 `720px`。E / F 不使用 G 的首尾帧功能。

```bash
curl -i -sS "$BASE_URL/v1/videos/generations_async" \
  -H "Authorization: Bearer $API_KEY" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: drama-reference-urls-001' \
  --data-binary '{
    "provider": "drama",
    "model": "seedance2.5-F",
    "prompt": "让参考图中的人物自然挥手",
    "reference_images": ["https://cdn.example.com/character.png"],
    "seconds": 8,
    "resolution": "720p",
    "aspect_ratio": "16:9"
  }'
```

**示例 D5：Drama G 首帧 URL（JSON）**

适用：`seedance2.5-G`。这里 `image` 明确指定首帧，比例必须为 `adaptive`，生成时显式提供正整数秒数。它与示例 D4 的 `reference_images` 普通参考含义不同。

```bash
curl -i -sS "$BASE_URL/v1/videos/generations_async" \
  -H "Authorization: Bearer $API_KEY" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: drama-first-frame-001' \
  --data-binary '{
    "provider": "drama",
    "model": "seedance2.5-G",
    "prompt": "从首帧开始，镜头平稳向前移动",
    "image": "https://cdn.example.com/first.png",
    "seconds": 8,
    "resolution": "720p",
    "aspect_ratio": "adaptive"
  }'
```

**示例 D6：Drama G 首尾帧 URL（JSON）**

适用：`seedance2.5-G`。`image` 是首帧，`last_frame` 是尾帧，必须同时提供；比例仍为 `adaptive`。不要混入 `reference_images`、参考视频或参考音频，也不要只给尾帧。

```bash
curl -i -sS "$BASE_URL/v1/videos/generations_async" \
  -H "Authorization: Bearer $API_KEY" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: drama-first-last-frame-001' \
  --data-binary '{
    "provider": "drama",
    "model": "seedance2.5-G",
    "prompt": "从首帧平稳过渡到尾帧",
    "image": "https://cdn.example.com/first.png",
    "last_frame": "https://cdn.example.com/last.png",
    "seconds": 8,
    "resolution": "720p",
    "aspect_ratio": "adaptive"
  }'
```

<a id="drama-a-h3-examples"></a>

**示例 D7：Drama A 纯文本生成**

适用：`provider=drama`，型号 `seedance2.0-A` 或 `seedance2.5-A`。下面显式填写默认值：4 秒、480p、16:9，不发送参考素材。两款模型按秒计费。

| 型号 | 输出时长 | 输出分辨率 | 模式与比例 |
| --- | --- | --- | --- |
| `seedance2.0-A` | 4–15 秒 | 480p、720p、1080p、4k | 文生和普通参考可用六种固定比例、`Auto`；成对首尾帧仅 `Auto` |
| `seedance2.5-A` | 4–30 秒 | 480p、720p | 文生和普通参考；六种固定比例，不支持 `Auto`，不支持首尾帧 |
| `minimax-h3` | 4–15 秒 | 480p、720p、1080p | 文生和普通参考；仅 16:9、9:16，无首尾帧 |

三款省略参数均为 4 秒、480p、16:9；`duration` 可代替 `seconds`，同时填写时必须一致。六种固定比例为 16:9、4:3、1:1、3:4、9:16、21:9。此处不含 A fast、Mini 或 `minimax-h3-2k`。

```bash
curl -i -sS "$BASE_URL/v1/videos/generations_async" \
  -H "Authorization: Bearer $API_KEY" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: drama-a-text-001' \
  --data-binary '{
    "provider": "drama",
    "model": "seedance2.0-A",
    "prompt": "海面日出，镜头缓慢前移，电影感光影",
    "seconds": 4,
    "resolution": "480p",
    "aspect_ratio": "16:9"
  }'
```

**示例 D8：Drama A 按编号使用参考图片、视频和音频**

适用：两款 A 的普通参考模式。`reference_images`、`reference_videos`、`reference_audios` 都是 **URL 字符串数组**；一个素材也保留方括号。每类从 1 编号，`@图1` / `@图片1` 对应图片数组第一项，`@视频1` 与 `@音频1` 分别对应各自数组第一项。编号不是整个请求的总序号，也不是 URL。

每个普通参考素材都必须在 prompt 中引用。`@图01`、`@图0`、负数、小数、越界编号和未引用素材会被拒绝；同一素材可以多次引用。使用上游风格的 `references: [{"type":"image","role":"reference","source":"https://..."}]` 也可以，编号依每类普通素材的顺序计算；不要通过多个别名重复发送同一素材。

```bash
curl -i -sS "$BASE_URL/v1/videos/generations_async" \
  -H "Authorization: Bearer $API_KEY" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: drama-a-references-001' \
  --data-binary '{
    "provider": "drama",
    "model": "seedance2.5-A",
    "prompt": "@图1 中的人自然行走，运镜参考@视频1，声音参考@音频1",
    "reference_images": ["https://cdn.example.com/character.png"],
    "reference_videos": ["https://cdn.example.com/motion.mp4"],
    "reference_audios": ["https://cdn.example.com/voice.mp3"],
    "seconds": 8,
    "resolution": "720p",
    "aspect_ratio": "16:9"
  }'
```

将三个示例 URL 分别替换成真实媒体文件的公网 HTTPS 地址，不要在 URL 前写 `@`；提示词中的 `@图1` 是 A 系列的素材引用语法。A 2.0 上限为 9 图、3 视频、3 音频、合计 12 项；A 2.5 为 30 图、10 视频、10 音频、合计 50 项。所有 URL 唯一，不接收本地文件、Data URI 或 `asset://`。真实媒体的 MIME、像素、时长和文件大小仍由上游检查。

**示例 D9：Drama A 2.0 成对首尾帧**

仅适用 `seedance2.0-A`。`image` 是首帧，`last_frame` 是尾帧，必须同时提供且 URL 不同；比例使用 `Auto`，不是 G 的 `adaptive`。不混入普通参考素材，不需要使用 `@图N`。省略比例时该模式会使用 `Auto`，但建议明确填写。

```bash
curl -i -sS "$BASE_URL/v1/videos/generations_async" \
  -H "Authorization: Bearer $API_KEY" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: drama-a-frames-001' \
  --data-binary '{
    "provider": "drama",
    "model": "seedance2.0-A",
    "prompt": "从室内首帧平稳过渡到窗外城市尾帧",
    "image": "https://cdn.example.com/first.png",
    "last_frame": "https://cdn.example.com/last.png",
    "seconds": 8,
    "resolution": "720p",
    "aspect_ratio": "Auto"
  }'
```

`first_image` / `last_image` 是兼容别名，不能与对应的 `image` / `last_frame` 重复填写。只想用一张普通参考图时，改用示例 D8 的 `reference_images` 并在 prompt 写 `@图1`；不要只传一个 `image`。

**示例 D10：Drama minimax-h3 使用参考图并关闭音频**

适用：小写 `minimax-h3`，`provider=drama`。普通图片参考用 URL 字符串数组，**不固定为首帧**；提示词可直接描述动作，不强制 A 系列的编号规则。`generate_audio: false` 会保留并传给上游；A 系列不支持此字段，不能把本例只改模型名后调用 A。

```bash
curl -i -sS "$BASE_URL/v1/videos/generations_async" \
  -H "Authorization: Bearer $API_KEY" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: drama-h3-reference-001' \
  --data-binary '{
    "provider": "drama",
    "model": "minimax-h3",
    "prompt": "参考图中的人物自然点头挥手，镜头缓慢推进",
    "reference_images": ["https://cdn.example.com/character.png"],
    "seconds": 6,
    "resolution": "720p",
    "aspect_ratio": "16:9",
    "generate_audio": false
  }'
```

H3 图片最多 9 张，视频与音频合计最多 3 项，总数最多 12 项，URL 不重复。可用 `references` 的普通 `reference` 或匹配类型的 `reference_image/reference_video/reference_audio` 角色，也可省略该角色；不接收 `first_frame`、`last_frame`。去掉 `reference_images` 即为纯文本生成。海螺官方 `MiniMax-H3` 走另一套协议，名称、供应商、分辨率不能混用；使用上表精确拼写，其他混合大小写不自动归一化。

上述 D7–D10 均使用本站 `_async` 接口：HTTP 202 只确认受理，保存 `task_id/query_url`，用同一本站 Token 查询 `/v1/media/tasks_async/{task_id}`，等 `status=succeeded` 后下载 `data[].url`。失败时读取 `fail_reason`，不要重新生成来“查询”；标准 `/v1/videos` 则返回 `id` 并使用对应的标准查询路径。

视频请求体最大 64 MiB，具体模型可对单个参考文件设更小限制。输出分辨率必须使用模型支持的值；`minimax-h3` 不支持 `2k` 输出。

<a id="drama-efg-examples"></a>

#### 5.1.3 E / F / G 各五种完整生成示例

以下十五条命令分别覆盖 E、F、G 的纯文本、参考图、参考视频、参考音频和三者混合。先按第 2 节设置 `BASE_URL` 和 `API_KEY`，将 `cdn.example` 替换为可直接读取的公网 HTTPS 素材地址；这些占位 URL 不可直接生成。

每次新的生成使用新的 `Idempotency-Key`；重试同一个任务时保留原 Key 和完全相同的请求体。成功受理返回 HTTP 202 后，保存 `task_id` 并按第 6 节查询，最终从 `data` 获取结果链接。不要仅因生成时间长而重复提交。

| 系列与本节型号 | 输出参数 | 计费用量与差异 |
| --- | --- | --- |
| E：`seedance2.5-E` | 8 秒、720p、16:9，不发送 `generate_audio` | 按条 `requests`，素材编号可选 |
| F：`seedance2.5-F` | 8 秒、720p、16:9、`generate_audio=true` | 无参考视频时按 `seconds`；有参考视频时等待上游实际 `tokens`。`seedance2.0-F` 使用 `720px`，不能直接照搬此分辨率值 |
| G：`seedance2.5-G` | 8 秒、720p、16:9、`generate_audio=true` | 按实际 `tokens`；本型号允许只参考音频，其他 G 型号还需图片或视频。普通参考图不等于首帧；首尾帧另见 D5/D6 |

切换具体型号时，先核对该型号支持的分辨率、时长、素材类型和生成模式。

**E 系列：seedance2.5-E**

**E1：文本生成视频**

仅发送提示词和输出参数，不携带参考素材。

```bash
curl -i -sS "$BASE_URL/v1/videos/generations_async" \
  -H "Authorization: Bearer $API_KEY" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: drama-e-text-001' \
  --data-binary '{
  "provider": "drama",
  "model": "seedance2.5-E",
  "prompt": "Sunrise over the sea, slow forward camera movement",
  "seconds": 8,
  "resolution": "720p",
  "aspect_ratio": "16:9"
}'
```

**E2：参考图片 URL 生成**

reference_images 是 HTTPS 图片 URL 字符串数组，用作普通参考，不固定为首帧。

```bash
curl -i -sS "$BASE_URL/v1/videos/generations_async" \
  -H "Authorization: Bearer $API_KEY" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: drama-e-image-001' \
  --data-binary '{
  "provider": "drama",
  "model": "seedance2.5-E",
  "prompt": "Animate the character from the reference image",
  "seconds": 8,
  "resolution": "720p",
  "aspect_ratio": "16:9",
  "reference_images": [
    "https://cdn.example/character.png"
  ]
}'
```

**E3：参考视频 URL 生成**

reference_videos 是 HTTPS 视频 URL 字符串数组，用于引导动作与运镜；本接口不编辑或延长原视频。

```bash
curl -i -sS "$BASE_URL/v1/videos/generations_async" \
  -H "Authorization: Bearer $API_KEY" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: drama-e-video-001' \
  --data-binary '{
  "provider": "drama",
  "model": "seedance2.5-E",
  "prompt": "Follow the camera movement and action rhythm of the reference video",
  "seconds": 8,
  "resolution": "720p",
  "aspect_ratio": "16:9",
  "reference_videos": [
    "https://cdn.example/motion.mp4"
  ]
}'
```

**E4：参考音频 URL 生成**

reference_audios 是 HTTPS 音频 URL 字符串数组，用于引导场景或声音。

```bash
curl -i -sS "$BASE_URL/v1/videos/generations_async" \
  -H "Authorization: Bearer $API_KEY" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: drama-e-audio-001' \
  --data-binary '{
  "provider": "drama",
  "model": "seedance2.5-E",
  "prompt": "Let the reference audio guide the mood and movement of an ocean scene",
  "seconds": 8,
  "resolution": "720p",
  "aspect_ratio": "16:9",
  "reference_audios": [
    "https://cdn.example/voice.mp3"
  ]
}'
```

**E5：图片 + 视频 + 音频混合生成**

references 是对象数组，type 指定媒体类型，role=reference 表示普通参考，source 填独立 HTTPS URL。不要再把同一素材填入 reference_images / reference_videos / reference_audios。

```bash
curl -i -sS "$BASE_URL/v1/videos/generations_async" \
  -H "Authorization: Bearer $API_KEY" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: drama-e-mixed-001' \
  --data-binary '{
  "provider": "drama",
  "model": "seedance2.5-E",
  "prompt": "Keep the person from the image, follow the video movement and use the audio as voice guidance",
  "seconds": 8,
  "resolution": "720p",
  "aspect_ratio": "16:9",
  "references": [
    {
      "type": "image",
      "role": "reference",
      "source": "https://cdn.example/character.png"
    },
    {
      "type": "video",
      "role": "reference",
      "source": "https://cdn.example/motion.mp4"
    },
    {
      "type": "audio",
      "role": "reference",
      "source": "https://cdn.example/voice.mp3"
    }
  ]
}'
```

**F 系列：seedance2.5-F**

**F1：文本生成视频**

仅发送提示词和输出参数，不携带参考素材。

```bash
curl -i -sS "$BASE_URL/v1/videos/generations_async" \
  -H "Authorization: Bearer $API_KEY" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: drama-f-text-001' \
  --data-binary '{
  "provider": "drama",
  "model": "seedance2.5-F",
  "prompt": "Sunrise over the sea, slow forward camera movement",
  "seconds": 8,
  "resolution": "720p",
  "aspect_ratio": "16:9",
  "generate_audio": true
}'
```

**F2：参考图片 URL 生成**

reference_images 是 HTTPS 图片 URL 字符串数组，用作普通参考，不固定为首帧。

```bash
curl -i -sS "$BASE_URL/v1/videos/generations_async" \
  -H "Authorization: Bearer $API_KEY" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: drama-f-image-001' \
  --data-binary '{
  "provider": "drama",
  "model": "seedance2.5-F",
  "prompt": "Animate the character from the reference image",
  "seconds": 8,
  "resolution": "720p",
  "aspect_ratio": "16:9",
  "reference_images": [
    "https://cdn.example/character.png"
  ],
  "generate_audio": true
}'
```

**F3：参考视频 URL 生成**

reference_videos 是 HTTPS 视频 URL 字符串数组，用于引导动作与运镜；本接口不编辑或延长原视频。

```bash
curl -i -sS "$BASE_URL/v1/videos/generations_async" \
  -H "Authorization: Bearer $API_KEY" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: drama-f-video-001' \
  --data-binary '{
  "provider": "drama",
  "model": "seedance2.5-F",
  "prompt": "Follow the camera movement and action rhythm of the reference video",
  "seconds": 8,
  "resolution": "720p",
  "aspect_ratio": "16:9",
  "reference_videos": [
    "https://cdn.example/motion.mp4"
  ],
  "generate_audio": true
}'
```

**F4：参考音频 URL 生成**

reference_audios 是 HTTPS 音频 URL 字符串数组，用于引导场景或声音。

```bash
curl -i -sS "$BASE_URL/v1/videos/generations_async" \
  -H "Authorization: Bearer $API_KEY" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: drama-f-audio-001' \
  --data-binary '{
  "provider": "drama",
  "model": "seedance2.5-F",
  "prompt": "Let the reference audio guide the mood and movement of an ocean scene",
  "seconds": 8,
  "resolution": "720p",
  "aspect_ratio": "16:9",
  "reference_audios": [
    "https://cdn.example/voice.mp3"
  ],
  "generate_audio": true
}'
```

**F5：图片 + 视频 + 音频混合生成**

references 是对象数组，type 指定媒体类型，role=reference 表示普通参考，source 填独立 HTTPS URL。不要再把同一素材填入 reference_images / reference_videos / reference_audios。

```bash
curl -i -sS "$BASE_URL/v1/videos/generations_async" \
  -H "Authorization: Bearer $API_KEY" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: drama-f-mixed-001' \
  --data-binary '{
  "provider": "drama",
  "model": "seedance2.5-F",
  "prompt": "Keep the person from the image, follow the video movement and use the audio as voice guidance",
  "seconds": 8,
  "resolution": "720p",
  "aspect_ratio": "16:9",
  "references": [
    {
      "type": "image",
      "role": "reference",
      "source": "https://cdn.example/character.png"
    },
    {
      "type": "video",
      "role": "reference",
      "source": "https://cdn.example/motion.mp4"
    },
    {
      "type": "audio",
      "role": "reference",
      "source": "https://cdn.example/voice.mp3"
    }
  ],
  "generate_audio": true
}'
```

**G 系列：seedance2.5-G**

**G1：文本生成视频**

仅发送提示词和输出参数，不携带参考素材。

```bash
curl -i -sS "$BASE_URL/v1/videos/generations_async" \
  -H "Authorization: Bearer $API_KEY" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: drama-g-text-001' \
  --data-binary '{
  "provider": "drama",
  "model": "seedance2.5-G",
  "prompt": "Sunrise over the sea, slow forward camera movement",
  "seconds": 8,
  "resolution": "720p",
  "aspect_ratio": "16:9",
  "generate_audio": true
}'
```

**G2：参考图片 URL 生成**

reference_images 是 HTTPS 图片 URL 字符串数组，用作普通参考，不固定为首帧。

```bash
curl -i -sS "$BASE_URL/v1/videos/generations_async" \
  -H "Authorization: Bearer $API_KEY" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: drama-g-image-001' \
  --data-binary '{
  "provider": "drama",
  "model": "seedance2.5-G",
  "prompt": "Animate the character from the reference image",
  "seconds": 8,
  "resolution": "720p",
  "aspect_ratio": "16:9",
  "reference_images": [
    "https://cdn.example/character.png"
  ],
  "generate_audio": true
}'
```

**G3：参考视频 URL 生成**

reference_videos 是 HTTPS 视频 URL 字符串数组，用于引导动作与运镜；本接口不编辑或延长原视频。

```bash
curl -i -sS "$BASE_URL/v1/videos/generations_async" \
  -H "Authorization: Bearer $API_KEY" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: drama-g-video-001' \
  --data-binary '{
  "provider": "drama",
  "model": "seedance2.5-G",
  "prompt": "Follow the camera movement and action rhythm of the reference video",
  "seconds": 8,
  "resolution": "720p",
  "aspect_ratio": "16:9",
  "reference_videos": [
    "https://cdn.example/motion.mp4"
  ],
  "generate_audio": true
}'
```

**G4：参考音频 URL 生成**

reference_audios 是 HTTPS 音频 URL 字符串数组，用于引导场景或声音。

```bash
curl -i -sS "$BASE_URL/v1/videos/generations_async" \
  -H "Authorization: Bearer $API_KEY" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: drama-g-audio-001' \
  --data-binary '{
  "provider": "drama",
  "model": "seedance2.5-G",
  "prompt": "Let the reference audio guide the mood and movement of an ocean scene",
  "seconds": 8,
  "resolution": "720p",
  "aspect_ratio": "16:9",
  "reference_audios": [
    "https://cdn.example/voice.mp3"
  ],
  "generate_audio": true
}'
```

**G5：图片 + 视频 + 音频混合生成**

references 是对象数组，type 指定媒体类型，role 分别使用 reference_image、reference_video、reference_audio，source 填独立 HTTPS URL。不要再把同一素材填入 reference_images / reference_videos / reference_audios。

```bash
curl -i -sS "$BASE_URL/v1/videos/generations_async" \
  -H "Authorization: Bearer $API_KEY" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: drama-g-mixed-001' \
  --data-binary '{
  "provider": "drama",
  "model": "seedance2.5-G",
  "prompt": "Keep the person from the image, follow the video movement and use the audio as voice guidance",
  "seconds": 8,
  "resolution": "720p",
  "aspect_ratio": "16:9",
  "references": [
    {
      "type": "image",
      "role": "reference_image",
      "source": "https://cdn.example/character.png"
    },
    {
      "type": "video",
      "role": "reference_video",
      "source": "https://cdn.example/motion.mp4"
    },
    {
      "type": "audio",
      "role": "reference_audio",
      "source": "https://cdn.example/voice.mp3"
    }
  ],
  "generate_audio": true
}'
```

### 5.2 视频编辑与延长

编辑、延长均**只接受 JSON**，必须使用声明了对应操作能力的模型，且请求中提供 `model`、`prompt`、`video`。`video` 可为字符串、`{"url":"..."}` 或 `{"file_id":"..."}`；允许的来源由模型决定。不可上传视频 multipart，也不可用 `source_task_id` 代替 `video`。

| 操作 | 路径 | 额外规则 |
|---|---|---|
| 编辑 | `POST /v1/videos/edits_async` | 模型能力可能固定继承源视频的时长、分辨率或比例，不能强行提交被锁定的维度 |
| 延长 | `POST /v1/videos/extensions_async` | 可传 `seconds` **或** `duration`，不能同时传；`extension_direction` 的允许值和默认值由模型决定 |

下列两个示例使用 `provider=xai`、`model=grok-imagine-video`；源视频 URL 必须替换为实际可访问的 MP4 地址。此模型的编辑不接收时长或尺寸，延长示例选择其支持的向后延长方向。

**示例 E：编辑视频**

用途：对现有源视频做内容修改，创建持久化媒体任务。替换 `video` 中的公网 MP4 URL；本例使用 URL 字符串，也可使用 `{"url":"..."}`。该经典 xAI 编辑模式继承源视频维度，所以不填 `seconds`、`size` 或 `resolution`。素材本身必须可读取，不能填本站 `task_id`。

```bash
curl -i -sS "$BASE_URL/v1/videos/edits_async" \
  -H "Authorization: Bearer $API_KEY" \
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

用途：在经典 xAI 模型支持的方向上延长源视频。`seconds: 6` 是本次延长时长，`backward` 是该模型支持的方向值；不要按其他模型的示例改成 `forward`。同一请求不再添加 `duration`。

```bash
curl -i -sS "$BASE_URL/v1/videos/extensions_async" \
  -H "Authorization: Bearer $API_KEY" \
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

以上三个持久化视频提交接口都返回同样的 202 结构：`task_id` 和 `query_url`。它们使用本站 API Key 查询本站任务，不能混用上游任务号。提交成功后的下一步：

```json
{
  "task_id": "video_example_local_task",
  "query_url": "https://api.your-domain.com/v1/media/tasks_async/video_example_local_task"
}
```

```bash
TASK_ID='从受理回包复制真实的task_id'
curl -sS "$BASE_URL/v1/media/tasks_async/$TASK_ID" \
  -H "Authorization: Bearer $API_KEY"
```

间隔约 3 秒查询原任务。`stage=completed`、`status=succeeded` 且有 `data[].url` 时，再下载返回的 URL；不要将供应商的原始 `completed` / `done` 状态当作本服务已完成。G 与带参考视频的 F 还需等待实际用量和结算完成。不要因 HTTP 202、结算仍在进行或保存失败而再次创建生成任务。

兼容接口 `POST /v1/videos/edits` 和 `POST /v1/videos/extensions` 返回带 `id` 的标准视频任务对象，查询使用 `GET /v1/videos/{id}`，与本节 `_async` 接口的 `task_id/query_url` 响应格式不同。请勿混用两种协议的任务号和查询路径。

### 5.3 视频保存

一个任务可能生成多个视频结果。任务进入保存阶段后，服务会检查视频文件，处理完成后在 `data` 中提供结果链接。

若保存失败，任务可能进入 `storage_failed`；恢复只重试保存，不重新调用上游，也不再次预扣或结算。部分可公开读取的上游结果在本地下载失败时可能以 `source: "upstream"` 直链回退；该链接不代表本站已保存成功，详见[第 6.3 节](#63-结果链接)。

## 6. 统一任务查询与结果

### 6.1 `GET /v1/media/tasks_async/{task_id}`

图片和视频使用同一查询入口，但字段并非完全相同。查询必须使用提交时的 Token。任务查询返回 HTTP 200 时也可能表示**任务失败**，应读取响应中的 `stage`、`status`，不能只看 HTTP 状态。

| 字段 | 含义 |
|---|---|
| `task_id`、`media_type` | 服务任务 ID；`image` 或 `video` |
| `provider`、`protocol` | 本任务使用的供应商与执行协议 |
| `model`、`request_type` | 视频响应包含；图片公共查询不保证包含 |
| `stage` | 通用阶段：`queued`、`generating`、`saving`、`completed`、`failed` |
| `status` | 细分任务状态；图片处理中常返回 `processing` |
| `progress` | 0–100 的展示进度 |
| `billing_status`、`storage_status` | 计费与存储状态 |
| `quota`、`cost` | 已知费用；图片公共查询提供 `quota`，视频还提供 `cost` |
| `error_code`、`fail_reason` / `error_message` | 失败信息；图片可有 601–613 公共错误码 |
| `data` | 结果数组；图片处理中通常省略，视频处理中为空数组 |

通用 `stage` 与常见细分状态关系：

| `stage` | 常见细分状态 | 客户端处理 |
|---|---|---|
| `queued` | `queued` | 等待执行 |
| `generating` | `processing`、`invoking`、`submitting`、`submitted` | 等待上游生成或轮询 |
| `saving` | `upstream_succeeded`、`uploading`、`storage_failed`、`billing_pending`、`billing_failed` | 等待保存或结算；失败时保留任务号并联系技术支持 |
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

通常只在 `stage=completed`、`status=succeeded` 且 `data` 非空时把任务标记为成功。若 `stage=saving` 且 `storage_status=failed`，或 `stage=failed`，应展示错误并联系技术支持处理。

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
| 403 | IP、账户、模型或供应商权限不足 |
| 404 | 任务不属于当前 Token，或签名媒体对象不可访问 |
| 409 | 同一 `Idempotency-Key` 对应不同请求 |
| 413 | 图片或视频请求体超过服务限制 |
| 429 | Gemini SC 上传频率或容量限制，见兼容入口 |
| 503 | 服务暂不可用，可稍后重试或联系技术支持 |

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

- 图片成功后按实际产物计费；视频使用预扣与结果结算。已受理任务使用受理时的价格，不受后续价格变化影响。
- 已受理的任务会继续处理；客户端暂时断网或查询失败时，应使用原任务号继续查询，不要创建新的生成任务。
- `execution_unknown` 表示无法确认供应商是否已经受理。请保留任务号并联系技术支持，不要直接重新提交，以免重复生成或计费。
- 图片保存或结算失败、视频保存失败时，应恢复原任务的后续处理。视频已生成但保存失败时，已产生的费用仍会保留。
- `storage_failed` 或 `billing_failed` 时，请提供原 `task_id` 和失败信息联系技术支持。不要通过重新生成来修复保存或结算问题。

## 8. 兼容接口

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

## 9. 当日任务统计与账户余额

使用您的 API Key 查询所属账户在服务时区当天创建的异步任务汇总。图片接口统计异步图片任务；视频接口统计持久化异步视频任务；统一媒体接口汇总这两类任务。统计包含同一账户通过工作台及其他 API Key 提交的任务，不限于当前 API Key。正在处理的任务也计入请求数。

### 9.1 请求

```http
GET /v1/images/tasks_async/stats
Authorization: Bearer <API_KEY>
```

图片、视频与合并统计使用相同的字段和鉴权规则：

| 方法与路径 | 统计范围 | 响应 `object` |
|---|---|---|
| `GET /v1/images/tasks_async/stats` | 持久化异步图片 | `async_image.stats` |
| `GET /v1/videos/tasks_async/stats` | 持久化异步视频，包括生成、编辑和延长 | `async_video.stats` |
| `GET /v1/media/tasks_async/stats` | 上述图片和视频之和 | `async_media.stats` |

接口只支持 `Authorization: Bearer` 方式传入 API Key，不接受 URL 查询参数、其他密钥请求头或浏览器登录会话代替 Bearer 鉴权。鉴权通过但 Key 额度或用户余额已耗尽时，仍可读取自己的统计。Key 必须未过期、未被禁用或删除，所属用户必须可用；已有 IP 访问限制继续生效。

无需请求体，也不提供日期、时区、用户或 Key 筛选参数；传入此类查询参数不会改变统计范围。统计日期与时区以响应中的 `date`、`timezone` 为准，按当地零点起、次日零点前计算，以任务创建时间归属日期，不按完成日期或固定 24 小时计算。

视频统计仅包含持久化异步媒体任务，同一任务不会因关联供应商记录而重复计数；旧版普通视频、音乐等任务不在统计范围内。

### 9.2 成功响应

```json
{
  "object": "async_image.stats",
  "date": "2026-10-20",
  "timezone": "Asia/Shanghai",
  "balance": 12.5,
  "today_requests": 20,
  "success_count": 15,
  "failure_count": 3,
  "success_rate": 83.3333333333
}
```

| 字段 | 类型 | 说明 |
|---|---|---|
| `object` | string | 图片为 `async_image.stats`，视频为 `async_video.stats`，合并统计为 `async_media.stats` |
| `date` | string | 统计日期，格式 `YYYY-MM-DD` |
| `timezone` | string | 服务使用的统计时区，日期边界按此时区计算 |
| `balance` | number | 当前账户的完整钱包余额，使用美元基准钱包额度单位；不是当前 API Key 的剩余额度，不按媒体类型分摊，也不随界面显示币种汇率换算 |
| `today_requests` | integer | 当天创建的任务总数，包含处理中、成功和失败任务；一次任务生成多个结果仍只计一次 |
| `success_count` | integer | 当天任务中已成功的任务数 |
| `failure_count` | integer | 当天任务中处于失败状态的任务数，包括 `failed`、`execution_unknown`、`storage_failed`、`billing_failed` 和 `expired` |
| `success_rate` | number | 成功率百分比，计算为 `success_count / (success_count + failure_count) * 100`；没有成功或失败任务时为 `0` |

失败按查询时的任务状态统计。保存或计费失败即使仍允许恢复，也暂计入失败；恢复后按新状态统计。视频生成成功但任务尚未完成时仍按处理中计算；生成、保存或计费失败时计入失败，同一任务不会同时增加成功数和失败数。已通过供应商结果链接回退且状态为 `succeeded` 的视频可计为成功。

所有统计响应带有 `Cache-Control: no-store`。统计和余额无法可靠读取时，接口返回 `stats_unavailable`，请稍后重试；不要将查询失败视为余额为零。三个接口的 `balance` 均表示所属账户的完整钱包余额，不按媒体类型分摊。

### 9.3 错误

缺少或无效 API Key 返回 `401`，统一使用以下结构，不公开具体鉴权失败原因：

```json
{
  "error": {
    "type": "authentication_error",
    "code": "authentication_error",
    "message": "invalid API key"
  }
}
```

| HTTP | code | 说明 |
|---|---|---|
| `401` | `authentication_error` | 缺少、无效、过期、禁用或删除的 API Key，所属用户不可用，或 IP 不符合 Key 限制 |
| `429` | `rate_limit_exceeded` | 请求频率超限，按响应 `Retry-After` 等待后重试 |
| `500` | `stats_unavailable` | 暂时无法读取账户余额或任务统计，请稍后重试 |
| `503` | `stats_unavailable` | 统计服务暂不可用 |

`500`、`503` 使用 `error.type: "server_error"`，`429` 使用 `error.type: "rate_limit_error"`；错误响应同样使用上述 `error` 对象结构并带有 `Cache-Control: no-store`。客户端应按 HTTP 状态和 `error.code` 处理，不依赖可调整的服务端错误文案。
