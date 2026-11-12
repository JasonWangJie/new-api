# new-api 异步图片与视频 API 接口文档

更新：2026-11-12。本文面向 API 调用方，介绍统一异步图片与视频接口。项目署名：new-api / QuantumNous。

本文按调用场景介绍统一异步图片与视频接口。调用方选择已开通、支持目标操作的模型，由服务自动选路。不同模型的素材类型、尺寸、时长和扩展能力仍有差异；统一路径不代表所有模型接受同一组参数。

## 1. 先选对接口

| 场景 | 方法与路径 | 请求格式 |
| --- | --- | --- |
| 文生图；带参考图生成 | `POST /v1/images/generations_async` | JSON 或 multipart |
| 图生图／图片编辑 | `POST /v1/images/edits_async` | JSON 或 multipart，至少一张参考图 |
| 文生视频、参考素材生视频、首尾帧生成 | `POST /v1/videos/generations_async` | JSON；所选模型支持时也可用 multipart |
| 视频编辑 | `POST /v1/videos/edits_async` | JSON |
| 视频延长 | `POST /v1/videos/extensions_async` | JSON |
| 查询图片或视频任务 | `GET /v1/media/tasks_async/{task_id}` | 无请求体 |
| 旧版视频对象读取 | `GET` / `HEAD /v1/media/objects/{object_id}` | 保留既有签名 URL 访问协议 |
| 读取按文件名提供的已保存产物 | `GET` / `HEAD /v1/media/files/{filename}` | 使用服务返回的完整结果 URL |

五个 `_async` 提交接口成功受理后均返回 **HTTP 202** 与 `task_id`、`query_url`。202 表示已受理任务，不表示已经生成成功。图片和视频共用任务查询入口，客户端无需直接查询上游任务。

原同步图片接口和不带 `_async` 的视频接口保留原语义，不能与本篇任务回包混用。兼容入口见[第 9 节](#9-旧版兼容入口)。

## 2. 调用约定与任务流程

示例使用 Bash 与 curl。`BASE_URL` 是服务根地址，不带 `/v1` 或末尾斜杠；`API_KEY` 是本站 API Key。Windows PowerShell 可使用 `curl.exe`，把 JSON 保存为 UTF-8 文件后用 `--data-binary '@request.json'` 提交。

```bash
export BASE_URL='https://api.your-domain.com'
export API_KEY='替换为本站 API Key'
```

示例中的 `YOUR_IMAGE_MODEL`、`YOUR_VIDEO_MODEL` 等是**待替换的模型名占位符**，不是实际型号。请替换为已开通且支持示例操作、素材与参数的模型。素材地址也必须替换为有效地址。

| 请求头 | 说明 |
| --- | --- |
| `Authorization: Bearer <API_KEY>` | 提交和任务查询必需；查询使用提交时的同一个 Key |
| `Content-Type: application/json` | JSON 提交使用 |
| `Content-Type: multipart/form-data` | 文件提交使用；curl `-F` 会生成 boundary，不手填此头 |
| `Idempotency-Key` | 可选，建议填写；最多 255 个 UTF-8 字节，规则见第 3 节 |

**受理回包示例：**

```http
HTTP/1.1 202 Accepted
Location: https://api.your-domain.com/v1/media/tasks_async/asyncimg_example
Retry-After: 3
Cache-Control: no-store
Content-Type: application/json

{"task_id":"asyncimg_example","query_url":"https://api.your-domain.com/v1/media/tasks_async/asyncimg_example"}
```

保存实际返回的任务号，优先请求 `query_url`；若返回相对路径，将其拼接到本站根地址。

```bash
curl -sS "$BASE_URL/v1/media/tasks_async/asyncimg_example" \
  -H "Authorization: Bearer $API_KEY"
```

建议按 `Retry-After`，或间隔至少 3 秒查询；网络错误或 5xx 时逐步延长间隔。HTTP 查询失败不等于任务失败。查询返回 `stage=completed`、`status=succeeded` 且 `data` 非空时读取结果，其他状态见[第 6 节](#6-统一任务查询与结果)。

客户端断开连接不会取消已受理任务。同一账户的其他 API Key 不能代替提交 Key 查询该任务，通常会得到 404。账户、Key、IP、分组与模型权限继续由服务端检查。

## 3. 幂等重试

一个新的业务操作使用一个新的 `Idempotency-Key`；同一操作超时重试时，保留原 Key 和请求内容。同一 Key 的有效重放返回原任务，并带 `X-Idempotency-Replayed: true`；同一 Key 对应不同请求返回 409。

| 请求类型 | 判定相同请求的依据 |
| --- | --- |
| 图片 JSON | 同一提交路径、完全相同的原始请求字节；空白或字段顺序变化也可能冲突 |
| 图片 multipart | 同一提交路径、完全相同的原始请求字节；重新执行 `curl -F` 可能因 boundary 变化而冲突 |
| 视频 JSON | 同一提交路径与原始请求字节 |
| 视频 multipart 生成 | 规范化 part 摘要；仅 boundary 变化不创建新任务，字段顺序、part 头、文件名、MIME 或内容变化可能冲突 |

幂等身份绑定当前 API Key。生成、编辑、延长是不同操作，不跨路径复用同一个 Key。已拿到 `task_id` 后继续查询原任务，不通过新 Key 重提来解决下载、保存或结算问题。

内部状态或失败原因中的 `execution_unknown` 表示无法确认上游是否已经执行；公共查询可能将它投影为 `status=failed`。请保留原任务号并联系技术支持；重新提交可能造成重复生成或费用。

## 4. 异步图片

### 4.1 参数与参考图

`generations_async` 没有参考图时执行文生图；带有效参考图时，模型须支持参考图生成或编辑。`edits_async` 明确要求至少一张有效参考图；只传提示词或 mask 不成立。

| 字段 | 类型 | 必需／默认 | 说明 |
| --- | --- | --- | --- |
| `model` | string | 必需 | 已开通且支持本次操作的模型名 |
| `prompt` | string | 必需 | 去除首尾空白后非空，最多 64 KiB |
| `n` | integer | 默认 1 | 1–128；所选模型可能限制得更小 |
| `size` | string | 可选 | 如 `1024x1024`、`auto`，须符合模型能力 |
| `resolution` | string | 可选 | `1K`、`2K`、`4K`、`AUTO`；明确支持的模型可用 `0.5K` |
| `aspect_ratio` | string | 可选 | `auto` 或模型支持的比例，如 `1:1`、`16:9` |
| `quality`、`background` | string | 可选 | 使用所选模型支持的值 |
| `output_format` | string | 可选 | 格式能力依模型；OpenAI 2.6 图片格式扩展见第 4.3 节 |
| `frame_count` | integer | 可选 | GIF 帧数；适用模型、默认值与边界见第 4.3 节 |
| `frame_duration_ms` | integer | 可选 | GIF 单帧间隔毫秒数；适用范围见第 4.3 节 |
| `response_format` | string | 可选 | 上游输出形式提示；公共任务结果仍通过 URL 提供 |
| `image` | string / string[] | 有参考图时使用 | JSON 图片 URL 或完整 Data URI |
| `image_urls` | string[] | 有参考图时使用 | JSON 图片 URL 或完整 Data URI 数组 |
| `images` | object[] | 有参考图时使用 | 如 `[{"image_url":"https://..."}]`；不支持 `file_id` |
| `mask` | string / object | 可选 | URL 或 `{"image_url":"..."}`，须同时提供参考图且模型支持蒙版 |

参考图采用一种支持的字段写法，避免重复传入同一图片。JSON 中的裸 Base64、本机路径和 `@file.png` 不会上传文件；使用完整 Data URI 或可访问的 URL。multipart 上传使用 `image`／`image[]` 文件 part，可重复添加，并可按模型能力附加 `mask` 文件。图片按实际格式与解码结果校验，不仅检查扩展名。

同时填写 `size`、`resolution`、`aspect_ratio` 时应彼此一致。原生像素尺寸用于分辨率路由，具体生成尺寸还由模型与执行协议决定；参数不能强迫模型输出其不支持的尺寸。显式 0 或 false 不代表省略，`n=0` 等非法值在受理前拒绝。模型特有的数量别名也受数量边界与冲突检查。

使用 OpenAI 官方图像接口时，服务将请求的尺寸规格转换为 `size`，实际可用的像素尺寸取决于所选模型。

### 4.2 两个调用场景

**文生图：**替换 `YOUR_IMAGE_MODEL`。不需要参考图；其他可选参数可按模型能力添加。

```bash
curl -i -sS "$BASE_URL/v1/images/generations_async" \
  -H "Authorization: Bearer $API_KEY" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: image-text-001' \
  --data-binary '{
    "model": "YOUR_IMAGE_MODEL",
    "prompt": "清晨自然光下的陶瓷花瓶，简洁构图",
    "n": 1,
    "size": "1024x1024"
  }'
```

**图生图／图片编辑：**替换 `YOUR_IMAGE_EDIT_MODEL` 为支持参考图的模型，并替换素材 URL。此例使用 JSON 单张参考图 URL。

```bash
curl -i -sS "$BASE_URL/v1/images/edits_async" \
  -H "Authorization: Bearer $API_KEY" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: image-edit-001' \
  --data-binary '{
    "model": "YOUR_IMAGE_EDIT_MODEL",
    "prompt": "保留主体轮廓，将背景改为明亮摄影棚",
    "n": 1,
    "image": "https://cdn.example/source.png"
  }'
```

上传本机参考图时，使用同一编辑路径，去掉 JSON 请求头与 `--data-binary`，改用 `-F 'model=YOUR_IMAGE_EDIT_MODEL'`、`-F 'prompt=保留主体并修改背景'`、`-F 'image=@/实际路径/reference.png;type=image/png'`。生成路径也接受相同参考图形式，但同样要求模型支持。

### 4.3 GIF、SVG 与帧参数

本次新增的 GIF、SVG 输出和 `frame_count`、`frame_duration_ms` **仅适用于 OpenAI 2.6+ 生图系列**；当前实现明确支持的型号是 `gpt-image-2.6-flare`、`gpt-image-2.6-sunburst`。其他型号是否支持要按实际适配与模型能力确认，不能仅凭版本号或类似名称推断。

| 参数 | 类型 | 非必填时默认 | 允许值／范围 |
| --- | --- | --- | --- |
| `output_format` | string | `png` | `png`、`webp`、`jpeg`、`gif`、`svg`，使用小写 |
| `frame_count` | integer | GIF 为 `9` | 2–16，仅 `output_format=gif` 时可填写 |
| `frame_duration_ms` | integer | GIF 为 `200` | 20–2000 毫秒，仅 `output_format=gif` 时可填写 |

在上面两个图片场景中，将模型替换为已开通的适用型号，并在同一请求添加以下 GIF 参数片段即可；无需更换提交或查询接口：

```json
{
  "output_format": "gif",
  "frame_count": 15,
  "frame_duration_ms": 100
}
```

输出 SVG 时只添加 `"output_format": "svg"`；PNG、WebP、JPEG 同理，均不填写帧参数。GIF 的两项帧参数可分别省略，由服务补齐 9 和 200。multipart 使用 `-F 'output_format=gif'`、`-F 'frame_count=15'`、`-F 'frame_duration_ms=100'`。

JSON 帧参数必须是整数，不能是数字字符串、`null` 或小数。类型错误、超界、非法格式、非 GIF 携带帧参数均返回 400；显式 0 不使用默认值。`n` 是产物数量，`frame_count` 是单个 GIF 的帧数。

服务校验并转发请求值；实际帧数和间隔由上游生成结果决定，现有实际调用已出现与请求不一致的情况，因此不能把参数受理成功视为精确帧控制保证。GIF 保存保留动画，仍受总像素与文件大小限制；SVG 须通过静态内容校验，脚本和外部资源引用会被拒绝。输出格式扩展不扩大参考图输入格式范围。

## 5. 异步视频

### 5.1 参数与素材格式

三个视频提交路径统一受理任务，但**生成、编辑、延长及各种素材组合均须由所选模型明确支持**。以下字段是现有接口中可使用的形式，不是所有视频模型共同承诺的能力。

| 字段 | 类型 | 用途与约束 |
| --- | --- | --- |
| `model` | string | 必需；选择支持目标操作与素材类型的模型 |
| `prompt` | string | 文生视频必填；其他生成模式按模型要求，编辑／延长必填 |
| `seconds` / `duration` | integer | 时长（整数秒）；建议只填一个，必需性与范围按模型要求；延长时不能同时填写两者 |
| `size`、`resolution`、`aspect_ratio` | string | 模型支持的尺寸、分辨率与比例；不要混用相互冲突的值 |
| `image` / `input_reference` | URL string / object 或文件 | 生成参考图或首帧，允许的字段与来源按模型能力 |
| `last_frame` | URL string / object | 仅支持首尾帧生成的模型使用，并按模型要求提供首帧 |
| `reference_images` | array | 参考图片；数组元素形状、数量上限由模型决定 |
| `reference_videos`、`reference_audios` | array | 参考视频／音频；仅在模型支持该类素材或混合参考时使用 |
| `video` | string / object | 编辑、延长的源视频；支持的 URL、Data URI 或文件标识形式由模型决定 |
| `extension_direction` | string | 仅延长使用；可用方向及省略时行为按模型要求 |
| 其他扩展字段 | 模型规定的类型 | 如音频生成开关；只发送所选模型已支持的字段 |

视频生成允许 JSON，部分模型也接受 multipart；视频编辑和延长**只接受 JSON**。视频请求体上限为 64 MiB，素材本身还受模型与下载校验限制。

#### 5.1.1 参考图文件和 URL 到底怎么写

| 目的 | 写法 | 使用条件 |
| --- | --- | --- |
| JSON 单张参考图／首帧 | `"image": "https://cdn.example.com/first.png"` | 模型支持该字段与 URL 来源 |
| JSON URL 对象 | `"image": {"url":"https://cdn.example.com/first.png"}` | 模型支持该对象形状 |
| JSON 多张参考图 | `"reference_images": ["https://cdn.example.com/a.png","https://cdn.example.com/b.png"]` | 模型支持图片数组；部分模型要求 URL 对象数组 |
| multipart 本机文件 | `-F 'input_reference=@/实际路径/reference.png;type=image/png'` | 模型支持该文件字段与上传方式 |
| 首尾帧 | 同时提供 `image` 与 `last_frame` | 模型声明支持对应别名与首尾帧操作 |
| 源视频 | `"video": "https://media.example.com/source.mp4"` | 编辑或延长模型支持该源格式 |

URL 应直接返回素材内容，可由服务读取，无需浏览器登录或额外授权。JSON URL 前不加 `@`；不要用网盘分享页、本机路径或本站任务号代替素材。数组不要写成逗号拼接的字符串。Data URI、文件标识及专属素材引用并非通用能力，只有已明确支持的模型才能使用。

以下视频示例各展示一个场景。生成示例的 8 秒、720p、16:9 及延长示例的 6 秒，均是待按模型能力调整的示例值；模型不支持某个字段时应删除该字段，要求其他值或额外参数时按已开通能力补齐。

### 5.2 视频生成场景

**文生视频：**将 `YOUR_VIDEO_MODEL` 替换为支持文生视频及所填时长、分辨率的模型。

```bash
curl -i -sS "$BASE_URL/v1/videos/generations_async" \
  -H "Authorization: Bearer $API_KEY" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: video-text-001' \
  --data-binary '{
    "model": "YOUR_VIDEO_MODEL",
    "prompt": "镜头缓慢掠过清晨湖面，柔和自然光",
    "seconds": 8,
    "resolution": "720p",
    "aspect_ratio": "16:9"
  }'
```

**参考素材生视频：**将 `YOUR_VIDEO_REFERENCE_MODEL` 替换为支持所用素材及混合参考的模型。下面展示图片、视频、音频数组的组合；只用参考图时删除后两项，不要保留空占位素材。

```bash
curl -i -sS "$BASE_URL/v1/videos/generations_async" \
  -H "Authorization: Bearer $API_KEY" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: video-reference-001' \
  --data-binary '{
    "model": "YOUR_VIDEO_REFERENCE_MODEL",
    "prompt": "保持参考人物与运动风格，生成连贯镜头",
    "reference_images": ["https://cdn.example.com/reference.png"],
    "reference_videos": ["https://cdn.example.com/motion.mp4"],
    "reference_audios": ["https://cdn.example.com/sound.mp3"],
    "seconds": 8,
    "resolution": "720p",
    "aspect_ratio": "16:9"
  }'
```

若模型的单图模式要求 `image` 或 multipart 文件，应按上表替换参考字段；不能把多素材模型的数组直接复制给所有图生视频模型。

**首尾帧生成：**将 `YOUR_VIDEO_KEYFRAME_MODEL` 替换为支持 `image`／`last_frame` URL 输入的首尾帧模型。两张图分别定义开始与结束画面，其他比例与时长要求按模型设置。

```bash
curl -i -sS "$BASE_URL/v1/videos/generations_async" \
  -H "Authorization: Bearer $API_KEY" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: video-keyframes-001' \
  --data-binary '{
    "model": "YOUR_VIDEO_KEYFRAME_MODEL",
    "prompt": "从首帧自然过渡到尾帧，保持主体一致",
    "image": "https://cdn.example.com/first.png",
    "last_frame": "https://cdn.example.com/last.png",
    "seconds": 8,
    "resolution": "720p"
  }'
```

### 5.3 视频编辑与延长

两种操作均要求 `model`、`prompt`、`video`。`video` 可按模型能力使用 URL 字符串、`{"url":"..."}` 或 `{"file_id":"..."}`，不能用 `source_task_id` 或本站 `task_id` 代替源视频。

**视频编辑：**替换为支持编辑及公网 MP4 输入的模型。部分模型继承源视频尺寸与时长，因此本例不指定这些维度。

```bash
curl -i -sS "$BASE_URL/v1/videos/edits_async" \
  -H "Authorization: Bearer $API_KEY" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: video-edit-001' \
  --data-binary '{
    "model": "YOUR_VIDEO_EDIT_MODEL",
    "prompt": "把天空改成晚霞，保持镜头运动",
    "video": "https://media.example.com/source.mp4"
  }'
```

**视频延长：**替换为支持延长、公网 MP4 输入和所填时长的模型。本例明确使用 `backward`，因此还要求模型支持该延长方向；其他方向的可用值与语义按模型能力调整。

```bash
curl -i -sS "$BASE_URL/v1/videos/extensions_async" \
  -H "Authorization: Bearer $API_KEY" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: video-extend-001' \
  --data-binary '{
    "model": "YOUR_VIDEO_EXTEND_MODEL",
    "prompt": "延续原有镜头运动与场景",
    "video": "https://media.example.com/source.mp4",
    "seconds": 6,
    "extension_direction": "backward"
  }'
```

生成、编辑与延长都按第 2 节保存受理回包并查询，不需要不同的轮询代码。结果保存或结算尚未完成时不要重新生成。

## 6. 统一任务查询与结果

### 6.1 `GET /v1/media/tasks_async/{task_id}`

图片和视频共用查询入口，返回字段可随媒体类型有所不同。HTTP 200 表示查询成功，任务本身可能失败。

| 字段 | 说明 |
| --- | --- |
| `task_id`、`media_type` | 本站任务号；媒体类型为 `image` 或 `video` |
| `provider`、`protocol` | 服务返回的只读执行元数据；客户端无需据此选择提交或查询接口 |
| `model`、`request_type` | 视频响应包含；图片公共查询不保证包含 |
| `stage` | `queued`、`generating`、`saving`、`completed`、`failed` |
| `status` | 细分状态；图片生成中常为 `processing`，完成为 `succeeded` |
| `progress` | 0–100 展示进度，不等于完成承诺 |
| `billing_status`、`storage_status` | 计费与存储状态；图片／视频可能使用不同细分值 |
| `quota`、`cost` | 已知费用；图片提供 `quota`，视频还提供 `cost`；示例数值不是固定售价 |
| `error_code`、`fail_reason` / `error_message` | 任务失败原因；图片公共错误码见第 7 节 |
| `data` | 结果数组；图片未有结果时通常省略，视频可为空数组 |
| `result_expired` | 成功任务的产物已过期时可返回 `true`，不能再把空结果当作可下载产物 |

| `stage` | 阶段含义 | 客户端处理 |
| --- | --- | --- |
| `queued` | 已受理，等待执行 | 继续查询 |
| `generating` | 正在生成或轮询 | 继续查询 |
| `saving` | 正在保存产物或确认账务 | 等待后处理；失败时联系技术支持恢复原任务 |
| `completed` | 处理已完成 | 检查结果是否可用或已过期 |
| `failed` | 任务失败或执行结果无法确认 | 展示错误；不要自动创建新任务 |

**完成图片响应示例（摘取公共字段）：**

```json
{
  "task_id": "asyncimg_example",
  "media_type": "image",
  "stage": "completed",
  "status": "succeeded",
  "progress": 100,
  "billing_status": "succeeded",
  "storage_status": "succeeded",
  "quota": 500,
  "data": [{"url":"https://api.your-domain.com/v1/media/files/img_0123456789abcdef0123456789abcdef.png"}]
}
```

**完成视频响应示例（摘取公共字段）：**

```json
{
  "task_id": "video_example",
  "media_type": "video",
  "model": "YOUR_VIDEO_MODEL",
  "stage": "completed",
  "status": "succeeded",
  "progress": 100,
  "billing_status": "settled",
  "storage_status": "succeeded",
  "quota": 500000,
  "cost": 1,
  "data": [{
    "url":"https://api.your-domain.com/v1/media/files/vid_0123456789abcdef0123456789abcdef.mp4",
    "content_type":"video/mp4",
    "byte_size":10485760
  }]
}
```

### 6.2 哪些结果算完成

以 `stage=completed`、`status=succeeded` 且 `data` 非空为结果可用条件。产物过期不会把曾经成功的任务改成生成失败：可保留 `status=succeeded`，同时返回 `result_expired=true`、空 `data` 或 `storage_status=expired`。`stage=saving` 且保存／计费失败时，应恢复原任务；不能只看到上游已完成或结果数组非空就认为本站流程成功。

图片下载失败后，符合上游直链回退条件且已完成结算的任务可返回 `status: "succeeded"`、`result_source: "upstream"` 与 `storage_status: "upstream"`，`data[].url` 为原上游地址。这属于结果可用的回退完成状态，不能当作本站保存成功；文件未经本站图片校验。视频也可能通过 `data[].source: "upstream"` 返回结果，结合 `status`、`storage_status` 判断。

### 6.3 结果链接与文件读取

普通已保存产物可使用本站 `/v1/media/files/{filename}`：图片文件名为 `img_` 开头，扩展名按实际编码为 `.png`、`.jpg`、`.webp`、`.gif` 或 `.svg`；视频为 `vid_` 开头的 `.mp4`。站点启用外部图片交付后，`data[].url` 也可直接使用外部图床地址，或在上传失败时按站点策略使用原上游地址、本站已保存文件；同一任务的不同图片可以使用不同来源。异步视频继续使用现有本地保存与交付流程。

客户端应逐项使用响应中的完整 `data[].url`，保留查询参数并允许 HTTP 跳转，不要假定所有图片同域、一定使用本站文件路径，或自行生成文件名和拼接对象 ID。图床上传重试与回退属于原任务后处理，不要求重新提交生成请求；已有有效回退结果时，任务仍可正常完成。

```bash
export RESULT_URL='把实际响应中的完整结果 URL 粘贴到这里'
curl -L "$RESULT_URL" -o result-file
```

本站 `/v1/media/files/` 文件 URL 不带签名查询参数，读取时不要求额外 Bearer 请求头，是可分享的结果地址。服务仍检查对应任务和产物是否可读、是否过期；视频读取还检查原 Key、所属用户及 IP 限制。任务或产物保留期结束后会返回 404，重新查询不能恢复已经过期的文件。

文件入口支持 `GET`、`HEAD`。本站视频还支持单范围 `Range` 读取；合法范围返回 206 和 `Content-Range`，可用 `curl -I "$RESULT_URL"` 或 `curl -H 'Range: bytes=0-1048575' "$RESULT_URL" -o first-part.bin`。

浏览器中直接用 `<img>` 显示外部图片，与通过 `fetch` 读取图片字节是不同能力。下载、放大预览或保存浏览器本地图库需要图床及其 CDN 为网页实际 Origin 配置 CORS；跨域读取不应携带本站 Cookie 或 API Key。HTTP 200 或 curl 下载成功不能代替浏览器跨域验收。

旧版 `/v1/media/objects/{object_id}` 签名视频链接继续按其签名协议读取；收到带查询参数的链接时须原样保留全部参数。外部图床与上游直链的有效期、访问限制及 HEAD、Range 支持由对应服务决定，不套用本站文件访问语义；本站任务或产物过期也不代表外部副本已被删除。受管画布继续使用原有会话鉴权内容入口。

## 7. 错误和恢复

### 7.1 HTTP 错误与任务失败

接口错误使用以下对象结构；客户端按 HTTP 状态与 `error.code` 处理，不依赖可能调整的文字：

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
| --- | --- |
| 400 | 请求格式、必填字段、模型操作、素材或参数范围无效；包括不适用的 GIF 帧参数 |
| 401 | API Key 或账户不可用 |
| 403 | IP、分组、模型或其他访问权限不满足，或异步能力不可用 |
| 404 | 任务不属于当前 Key，或结果对象不可访问 |
| 409 | 幂等 Key 对应不同请求，或原幂等任务不可恢复读取 |
| 413 | 请求体超过上限 |
| 429 | 请求频率、上传容量或异步队列容量限制；按 `Retry-After` 等待 |
| 503 | 数据库、队列、密钥、存储或执行能力暂不可用 |

已受理任务失败时，查询可以仍返回 HTTP 200，此时读取 `stage`、`status` 与失败信息。图片公共 `error_code`：

| 错误码 | 含义 | 错误码 | 含义 |
| --- | --- | --- | --- |
| 601 | 内容／安全策略拒绝 | 608 | 执行未知、超时或已结束 |
| 602 | 参考图拉取失败 | 609 | 存储或账务确认失败 |
| 603 | 图片容量暂不可用 | 610 | 未分类上游错误 |
| 604 | 请求或尺寸无效 | 611 | 上游参考图数量超限 |
| 605 | 上游频率限制 | 612 | 缺少上游要求的参考图 |
| 606 | 上游临时故障 | 613 | 提示词或输入图无法处理 |
| 607 | 图片结果缺失或无效 | | |

视频不使用这组 601–613 分类，应读取其 `error_message` 及任务、计费、存储状态。

### 7.2 任务恢复

已受理任务使用受理时冻结的计价，后续价格变化不会重定价。生成成功但保存或结算失败时，恢复原任务的后处理；不要重新生成来修复磁盘或下载问题。视频上游已生成但本站保存失败时，已产生费用仍可能保留。

保留 `task_id`、提交时间、HTTP 错误码和任务失败信息联系技术支持。对 `execution_unknown`、`storage_failed`、`billing_failed` 不进行无条件重新提交。

## 8. 模型能力与服务可用性

使用前确认账户已开通模型、目标操作、素材组合、尺寸与时长。提交成功还取决于账户额度、访问权限、异步运行和存储服务状态；示例没有承诺所有部署均开放所有场景。

模型名占位符只解决调用写法，不能代替实际能力配置。未知模型、未开放操作或不支持的素材组合可能在受理前拒绝；受理之后仍可能发生上游生成、保存与结算失败。

## 9. 旧版兼容入口

以下入口继续保留既有协议。新增接入优先使用本文 `_async` 路径；旧提交回包与查询格式不自动变成统一媒体协议。

| 方法与路径 | 用途 |
| --- | --- |
| `POST /v1/images/generations_oa`、`POST /v1/images/edits_oa` | 旧版图片生成／编辑；适用型号同样支持第 4.3 节格式参数 |
| `POST /v1/chat/completions_gm` | 旧 messages 形状的图片提交 |
| `POST /v1/uploads/images_sc` | 旧 SC 单图上传，成功返回 200 |
| `POST /v1/images/generations_sc` | 旧 SC 图片提交 |
| `GET /v1/images/tasks_async/{task_id}`、`GET /v1/tasks_sc/{task_id}` | 旧图片任务查询及别名 |
| `POST /v1/videos`、`POST /v1/videos/edits`、`POST /v1/videos/extensions` | 既有视频任务协议，返回 `id`，按原 `GET /v1/videos/{id}` 查询 |

旧 messages、SC 上传与素材引用须保持各自协议，不能直接替换成本篇 JSON。同步 `/v1/images/generations`、`/v1/images/edits` 不会因使用相同模型而自动转为异步任务。

## 10. 当日异步生图统计与异步视频扩展

### 10.1 请求与统计范围

| 方法与路径 | 范围 | 响应 `object` |
| --- | --- | --- |
| `GET /v1/images/tasks_async/stats` | 持久化异步图片 | `async_image.stats` |
| `GET /v1/videos/tasks_async/stats` | 持久化异步视频，含生成／编辑／延长 | `async_video.stats` |
| `GET /v1/media/tasks_async/stats` | 上述两类合计 | `async_media.stats` |

```bash
curl -sS "$BASE_URL/v1/media/tasks_async/stats" \
  -H "Authorization: Bearer $API_KEY"
```

统计使用 API Key 所属账户身份，包含该账户所有 Key 与工作台任务，**不限于当前 Key**；这与单任务查询必须使用原提交 Key 的规则不同。只接受 Bearer API Key，额度耗尽仍可读取；Key、账户和 IP 限制必须有效。

无需请求体，不提供日期、时区、用户或 Key 筛选参数。按响应 `timezone` 的自然日及任务创建时间归属 `date`，不是按完成时间或固定 24 小时统计。旧版只保存在普通任务表的视频、音乐等任务不在此范围，同一持久化视频不会因关联上游记录重复计数。受理前被拒绝的请求不计入任务数，幂等重放不会重复增加请求数。

### 10.2 成功响应

```json
{
  "object": "async_media.stats",
  "date": "2026-11-06",
  "timezone": "Asia/Shanghai",
  "balance": 12.5,
  "today_requests": 20,
  "success_count": 15,
  "failure_count": 3,
  "success_rate": 83.3333333333
}
```

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `object` | string | 对应所查询的媒体统计类型 |
| `date`、`timezone` | string | 当前服务统计日期与时区 |
| `balance` | number | 账户完整钱包余额，按服务的 `QuotaPerUnit` 折算；不是 Token 数量或当前 Key 剩余额度，不按媒体类型分摊，也不应用界面币种汇率 |
| `today_requests` | integer | 当天创建的任务数，包含处理中、成功和失败；多产物任务计一次 |
| `success_count` | integer | 当前已成功的当天任务数 |
| `failure_count` | integer | 当前失败的当天任务数，包含执行未知、保存失败、计费失败和过期任务 |
| `success_rate` | number | `success_count / (success_count + failure_count) * 100`；分母为 0 时返回 0 |

失败按查询时的持久化状态统计，恢复后按新状态计数。所有统计响应带 `Cache-Control: no-store`。无法可靠读取统计或余额时返回错误，不把错误解释为余额 0。

### 10.3 统计错误

| HTTP | `error.code` | 说明 |
| --- | --- | --- |
| 401 | `authentication_error` | Key／账户／IP 无效；统一消息为 `invalid API key` |
| 429 | `rate_limit_exceeded` | 频率限制；按 `Retry-After` 等待 |
| 500 | `stats_unavailable` | 余额、统计或时区数据暂不可用 |
| 503 | `stats_unavailable` | 当前实例未提供所需统计能力 |

仍使用第 7 节的 `error` 对象结构；401 的 `error.type` 为 `authentication_error`，429 为 `rate_limit_error`，500／503 为 `server_error`。错误响应同样禁止缓存。
