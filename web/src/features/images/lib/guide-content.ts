/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For commercial licensing, please contact support@quantumnous.com
*/
export const ASYNC_IMAGE_ENDPOINTS = [
  ['POST', '/v1/images/generations_async', 'Image · capability routed'],
  ['POST', '/v1/images/edits_async', 'Image edit · capability routed'],
  ['POST', '/v1/videos/generations_async', 'Video · OpenAI Video shape'],
  ['POST', '/v1/videos/edits', 'Video edit · task plugin'],
  ['POST', '/v1/videos/extensions', 'Video extension · task plugin'],
  ['POST', '/v1/videos/edits_async', 'Video edit · durable media task'],
  [
    'POST',
    '/v1/videos/extensions_async',
    'Video extension · durable media task',
  ],
  ['GET', '/v1/media/tasks_async/{task_id}', 'Unified media task query'],
  ['GET', '/v1/media/objects/{object_id}', 'Signed local video content'],
  ['HEAD', '/v1/media/objects/{object_id}', 'Signed video metadata'],
  ['POST', '/v1/images/generations_oa', 'OpenAI image compatibility'],
  ['POST', '/v1/images/edits_oa', 'OpenAI edit compatibility'],
  ['POST', '/v1/chat/completions_gm', 'Gemini BB compatibility'],
  ['POST', '/v1/uploads/images_sc', 'Gemini SC input upload'],
  ['POST', '/v1/images/generations_sc', 'Gemini SC compatibility'],
  ['GET', '/v1/images/tasks_async/{task_id}', 'Legacy image task query'],
  ['GET', '/v1/tasks_sc/{task_id}', 'Gemini SC query alias'],
] as const

export const IMAGE_ERROR_CODES = [
  [601, 'Content or safety policy', '内容或安全策略拒绝'],
  [602, 'Reference image fetch failed', '参考图拉取失败'],
  [603, 'Image capacity unavailable', '图片容量暂不可用'],
  [604, 'Invalid request or dimensions', '请求或图片尺寸无效'],
  [605, 'Upstream rate limit', '上游频率限制'],
  [606, 'Temporary upstream failure', '上游临时故障'],
  [607, 'Image result missing or invalid', '图片产物缺失或无效'],
  [608, 'Execution unknown, expired or ended', '执行未知、超时或已结束'],
  [609, 'Storage or billing confirmation failed', '存储或账务确认失败'],
  [610, 'Unclassified upstream failure', '未分类上游错误'],
  [611, 'Too many reference images upstream', '上游参考图数量超限'],
  [612, 'Required reference image missing', '缺少上游要求的参考图'],
  [613, 'Prompt or input image cannot be processed', '提示词或输入图无法处理'],
] as const

export type MediaGuideExample = {
  code: string
  title?: string
  description?: string
}

export function asyncImageGuide(
  chinese: boolean,
  base: string,
  t: (key: string) => string = (key) => key
) {
  const videoSection = {
    id: 'video',
    title: t(
      'Asynchronous video generation, editing, extension and persistence'
    ),
    paragraphs: [
      t(
        'Choose an enabled provider and model first. Model names, duration, resolution and media inputs are provider-specific; changing only provider does not convert a request.'
      ),
      t(
        'Durable video workflow: submit to a *_async endpoint, keep task_id and query_url, query with the same API token, then download data[].url after status=succeeded and billing has settled. HTTP 202 only confirms admission.'
      ),
      t(
        'Reference image URLs use JSON. A single image may be a URL string or a {"url":"https://..."} object; reference_images must be an array, not a comma-separated string. The model determines whether an image is a first frame or an ordinary reference.'
      ),
      t(
        'Local image uploads use multipart file parts. @reference.png is curl file syntax, not a JSON URL. Let curl set Content-Type and boundary when using -F.'
      ),
      t(
        'Drama E/F use reference_images for ordinary references; Drama G also supports image and last_frame. Current Drama and official Doubao Seedance plugins accept JSON media references, not multipart uploads.'
      ),
      t(
        'Drama A ordinary references must each be mentioned in prompt with @图N or @图片N, @视频N and @音频N. Number each media type separately from 1; do not use leading zeros. A 2.0 paired frames use image plus last_frame with Auto; A 2.5 does not expose frame control here.'
      ),
      t(
        'Drama minimax-h3 uses lowercase model spelling, public HTTPS references and seconds-based pricing. It does not require numbered prompt references or support pinned frames. The official Hailuo MiniMax-H3 uses a different protocol; do not interchange their configurations.'
      ),
      t(
        'Use public HTTPS image URLs that the upstream can read without browser cookies or authorization headers. Replace every example URL; a page URL, local path or bare Base64 string is not an image URL.'
      ),
      t(
        'The Gemini SC upload endpoint is not a general video upload service. For a URL-only video model, upload the image to your own publicly readable storage first.'
      ),
      t(
        'Each new generation is billable. Reuse the original Idempotency-Key only when retrying the same request; storage recovery reuses the existing result and does not generate again.'
      ),
      t(
        'Shell examples use Bash. In Windows PowerShell, use curl.exe and put the JSON body in a UTF-8 file with --data-binary @request.json.'
      ),
    ],
  }
  const sections = chinese
    ? [
        {
          id: 'overview',
          title: '统一异步媒体流程',
          paragraphs: [
            '图片和视频请求先持久受理并返回任务 ID，再由后台调用上游。客户端断开不会取消已经受理的任务；HTTP 202 只表示任务已入库，不表示生成成功。',
            '新图片入口会根据渠道声明的生成、编辑能力以及模型映射选择协议和供应商。新视频入口复用现有 OpenAI Video 任务插件，生成成功后先下载到服务器本地，再公布本站签名链接。原同步图片、原视频和旧 OpenAI / Gemini 异步入口保持原行为。',
          ],
        },
        {
          id: 'authentication',
          title: '鉴权、供应商与幂等',
          paragraphs: [
            '提交和任务查询都使用 Authorization: Bearer <NEW_API_TOKEN>。查询必须使用提交时的同一 Token；同一用户的其他 Token 也会得到 404。签名视频链接不要求 Bearer，但服务端仍会检查原 Token 的状态、有效期和 IP 限制。',
            'provider 是可选的网关调度字段。指定后只在该供应商范围内选渠道，未指定时由分组、模型能力、平台策略和渠道池自动选择；该字段不会发送给上游。',
            'Idempotency-Key 最多 255 个 UTF-8 字节。图片重放要求同一路径、Token、Key 和完全相同的原始请求字节。视频 JSON 也按请求字节判断；生成接口的 multipart 会忽略随机 boundary，但字段顺序、part 头或内容变化仍会冲突。生成、编辑和延长属于不同路径，同一个 Key 跨路径复用会返回 409。',
          ],
        },
        {
          id: 'unified-images',
          title: '全渠道异步图片生成与编辑',
          paragraphs: [
            'POST /v1/images/generations_async 和 POST /v1/images/edits_async 支持 JSON 与 multipart/form-data。请求结构沿用 OpenAI 图片结构，并保留供应商适配器可以识别的扩展参数。编辑接口必须提供参考图。',
            '常用字段包括 model、prompt、n、size、resolution、aspect_ratio、quality、output_format、background、response_format、image、image_urls、images 和 mask。n 的网关上限为 128，模型能力可以设置更小上限；供应商扩展中的数量字段也必须与 n 一致并满足同一边界。',
            '系统在受理时冻结供应商、请求协议、模型映射、渠道和账单快照。OpenAI 兼容渠道使用图片接口；Imagen 使用图片协议；Gemini 多模态生图使用原生生成协议。Midjourney 等独立任务协议不进入这两个入口。',
          ],
        },
        {
          id: 'dimensions',
          title: '图片尺寸与计费档位',
          paragraphs: [
            'resolution 接受 1K、2K、4K 和 AUTO；aspect_ratio 接受 auto 及下表比例。显式比例优先于 size 中的比例；原生 WxH 与显式 resolution 冲突时返回 400。',
            'OpenAI 图片原生 WxH 按长边归入 1K、2K 或 4K，Gemini 图片按短边判定。合法的显式档位按请求档位计费。Gemini 0.5K 仅在模型能力允许时可用，并按最低 1K 账单档处理。',
          ],
        },
        {
          id: 'compatibility',
          title: 'OpenAI / Gemini 兼容入口',
          paragraphs: [
            '既有 POST /v1/images/generations_oa、POST /v1/images/edits_oa、POST /v1/chat/completions_gm 和 POST /v1/images/generations_sc 继续可用。它们保留各自的请求及查询格式，方便现有客户端平滑迁移。',
            'OpenAI 兼容入口支持 JSON 和 multipart；Gemini BB 使用 messages 和 extra_body.google.image_config；Gemini SC 使用 model、prompt、image_urls、resolution 和 aspect_ratio。兼容入口不会自动切换到其他平台。',
          ],
        },
        {
          id: 'uploads',
          title: 'Gemini SC 单图上传',
          paragraphs: [
            'POST /v1/uploads/images_sc 只接受 multipart/form-data 的一个非空 file 字段，格式限 PNG、JPEG、WebP。默认单图 32 MiB、每 Token 每分钟 20 次、临时输入总量 1 GiB，保留 24 小时。',
            '上传使用独立 Idempotency-Key。返回的 URL 与上传 Token 绑定，必须原样放入后续 image_urls；不能删除或重排签名参数，也不能由其他 Token 使用。',
          ],
        },
        videoSection,
        {
          id: 'query',
          title: '统一查询、状态与本地链接',
          paragraphs: [
            '新入口受理成功返回 HTTP 202、task_id、query_url，并带 Location、Retry-After: 3。使用 GET /v1/media/tasks_async/{task_id} 查询图片或视频；响应包含 media_type、provider、protocol、stage、progress、billing_status 和 storage_status。',
            '稳定阶段为 queued、generating、saving、completed 和 failed。status 提供更细状态，例如 queued、processing、invoking、uploading、storage_failed、execution_unknown、succeeded、failed 或 expired。保存成功后 data 返回本站链接；动态上游任务下载失败时，符合公开访问条件的上游结果也会在 data 中返回，并标记 source 或 result_source 为 upstream。图片仍保留当前下载及计费状态。',
            '视频结果包含 content_type、byte_size、checksum 和 expires_at。签名链接默认有效 1 小时，重新查询会签发新链接；GET、HEAD 和 Range 均受支持。链接过期、任务清理、签名错误，或原 Token 被禁用、过期、撤销、IP 不匹配时返回 404。',
          ],
        },
        {
          id: 'billing',
          title: '执行恢复与计费',
          paragraphs: [
            '图片沿用固定账单和幂等结算；视频沿用现有任务预扣及上游结果结算。受理时冻结价格快照，之后修改价格不会影响该任务。存储重试不会新增费用。',
            '带上游任务 ID 的图片供应商会持久化任务 ID 并恢复轮询。视频先持久化网关任务，再由后台提交；已经跨过上游提交边界但无法确认结果的任务进入 execution_unknown，禁止自动重新生成。',
            '视频上游成功但本地保存失败时会显示 saving 或 storage_failed，并保留已经发生的费用。动态上游任务耗尽本地保存重试后，如有可匿名访问的公网结果链接，会改用上游链接完成任务并标记 storage_status=upstream。管理员或用户恢复只处理现有产物；重新提交生成会创建新的计费任务。',
          ],
        },
        {
          id: 'errors',
          title: '错误与排查',
          paragraphs: [
            '受理前错误使用非 2xx HTTP 和 error 对象，包括 400 参数或能力错误、401/403 鉴权和权限错误、404 查询隔离、409 幂等冲突、413 请求过大，以及 503 数据库、Redis、密钥或存储不可用。',
            '图片任务失败仍使用 601–613 公共错误码。视频任务通过 status、stage、billing_status、storage_status 和 error_message 表示生成或保存失败。先保存 task_id，再到任务中心检查当前阶段；存储失败应执行恢复，不能再次提交生成。',
          ],
        },
        {
          id: 'configuration',
          title: '启用条件与工作台',
          paragraphs: [
            '异步图片需要 Redis、稳定的 ASYNC_IMAGE_PAYLOAD_KEYS 和启用的 temporary 图片存储。异步视频默认关闭，还需要可写的本地目录、稳定的 CRYPTO_SECRET、文件上限、下载超时和下载并发配置。',
            '平台策略和渠道池可按 resolution、model 或 model_resolution 绑定。显式渠道池按池内权重分流，10、9、8 对应约 10:9:8 的调用比例；全部为 0 时平均随机分配，有正权重时排除零权重渠道。未配置渠道池时沿用渠道全局优先级和权重。工作台、用户任务中心和管理员任务中心会统一展示图片与视频阶段，视频支持播放、下载和复制本站链接。',
          ],
        },
      ]
    : [
        {
          id: 'overview',
          title: 'Unified asynchronous media flow',
          paragraphs: [
            'Image and video requests are durably accepted before a task ID is returned, then executed by background workers. Disconnecting the client does not cancel an accepted task. HTTP 202 confirms persistence, not successful generation.',
            'The new image routes select a protocol and provider from declared generation/edit capabilities and model mappings. The new video route uses the existing OpenAI Video task plugins, downloads successful output to local server storage, and publishes a gateway-signed URL. Existing synchronous image, video, and OpenAI/Gemini compatibility routes retain their behavior.',
          ],
        },
        {
          id: 'authentication',
          title: 'Authentication, providers and idempotency',
          paragraphs: [
            'Send Authorization: Bearer <NEW_API_TOKEN> for submission and task queries. Poll with the exact submitting token; another token owned by the same user still receives 404. Signed video URLs need no Bearer header, but the server rechecks the original token status, expiry and IP restrictions.',
            'provider is an optional gateway routing field. When present, only that provider is considered. Otherwise group access, model capability, platform policy and channel pools select it. The field is never forwarded upstream.',
            'Idempotency-Key is limited to 255 UTF-8 bytes. Image replay requires the same path, token, key and exact raw request bytes. Video JSON follows the same byte rule. Generation multipart ignores a random boundary, while part order, headers and content remain significant. Reusing one key across generation, edit, or extension paths returns 409.',
          ],
        },
        {
          id: 'unified-images',
          title: 'Capability-routed asynchronous images',
          paragraphs: [
            'POST /v1/images/generations_async and POST /v1/images/edits_async accept JSON and multipart/form-data. They use the OpenAI image request shape and preserve provider extension parameters recognized by the selected adaptor. The edit route requires at least one reference image.',
            'Common fields are model, prompt, n, size, resolution, aspect_ratio, quality, output_format, background, response_format, image, image_urls, images and mask. The gateway maximum for n is 128 and a model may declare a smaller limit. Provider-specific quantity fields must match n and obey the same bound.',
            'Admission freezes the provider, request protocol, model mapping, channel and billing snapshot. OpenAI-compatible channels use the image endpoint, Imagen uses an image protocol, and Gemini multimodal image generation uses the native generation protocol. Independent Midjourney-style task protocols are excluded.',
          ],
        },
        {
          id: 'dimensions',
          title: 'Image dimensions and billing tiers',
          paragraphs: [
            'resolution accepts 1K, 2K, 4K and AUTO. aspect_ratio accepts auto and the ratios below. An explicit ratio takes precedence over one embedded in size. A native WxH that conflicts with an explicit resolution is rejected with 400.',
            'OpenAI native image dimensions use the longer edge to derive 1K, 2K or 4K; Gemini uses the shorter edge. A valid explicit tier keeps its requested billing tier. Gemini 0.5K is available only when the model capability allows it and uses the minimum 1K billing tier.',
          ],
        },
        {
          id: 'compatibility',
          title: 'OpenAI and Gemini compatibility routes',
          paragraphs: [
            'Existing POST /v1/images/generations_oa, POST /v1/images/edits_oa, POST /v1/chat/completions_gm and POST /v1/images/generations_sc remain available with their original request and query envelopes.',
            'The OpenAI compatibility routes accept JSON and multipart. Gemini BB uses messages and extra_body.google.image_config. Gemini SC uses model, prompt, image_urls, resolution and aspect_ratio. Compatibility routes never silently switch to another platform.',
          ],
        },
        {
          id: 'uploads',
          title: 'Gemini SC image uploads',
          paragraphs: [
            'POST /v1/uploads/images_sc accepts exactly one nonempty multipart file field containing PNG, JPEG or WebP. Defaults are 32 MiB per image, 20 attempts per token per minute, 1 GiB of reserved input per token and 24-hour retention.',
            'Uploads have their own Idempotency-Key scope. The returned URL belongs to the uploading token and must be copied unchanged into image_urls. Do not remove or reorder its signature query and do not reuse it with another token.',
          ],
        },
        videoSection,
        {
          id: 'query',
          title: 'Unified queries, states and local links',
          paragraphs: [
            'New routes return HTTP 202 with task_id and query_url plus Location and Retry-After: 3. Query either image or video with GET /v1/media/tasks_async/{task_id}. The response includes media_type, provider, protocol, stage, progress, billing_status and storage_status.',
            'Stable stages are queued, generating, saving, completed and failed. status supplies more detail, including queued, processing, invoking, uploading, storage_failed, execution_unknown, succeeded, failed and expired. Saved artifacts return local links in data. Dynamic upstream tasks can also return a public upstream result link after a download failure, marked by source or result_source as upstream. Images retain their current download and billing status.',
            'Video results include content_type, byte_size, checksum and expires_at. Signed links default to one hour and a fresh task query issues new links. GET, HEAD and Range are supported. Expired or invalid signatures, cleaned tasks, and an original token that is disabled, expired, revoked or outside its IP policy return 404.',
          ],
        },
        {
          id: 'billing',
          title: 'Recovery and billing',
          paragraphs: [
            'Images retain fixed, idempotent settlement. Videos retain existing task pre-consumption and upstream-result settlement. Admission freezes the price snapshot, so later price changes do not alter the task. Storage retries add no charge.',
            'Image providers returning an upstream task ID persist it and resume polling. Video jobs are persisted before a worker submits upstream. If submission may have crossed the upstream boundary but cannot be confirmed, the task becomes execution_unknown and is never automatically regenerated.',
            'An upstream-successful video whose local save fails remains in saving or storage_failed and keeps the charge already incurred. After local storage retries are exhausted, a dynamic upstream task can complete with a public anonymous upstream link and storage_status=upstream. User or administrator recovery handles the existing artifact only; submitting generation again creates a separate billable task.',
          ],
        },
        {
          id: 'errors',
          title: 'Errors and troubleshooting',
          paragraphs: [
            'Admission failures use non-2xx HTTP and an error object: 400 input or capability errors, 401/403 authentication and authorization, 404 query isolation, 409 idempotency conflicts, 413 oversized requests, and 503 database, Redis, encryption-key or storage availability.',
            'Image task failures retain public codes 601–613. Video failures use status, stage, billing_status, storage_status and error_message. Keep the task_id and inspect its task-center stage. Resume storage failures instead of submitting another generation.',
          ],
        },
        {
          id: 'configuration',
          title: 'Enablement and workbench behavior',
          paragraphs: [
            'Asynchronous images require Redis, stable ASYNC_IMAGE_PAYLOAD_KEYS and an enabled temporary image store. Asynchronous video is disabled by default and also requires a writable local directory, stable CRYPTO_SECRET, file-size limit, download timeout and download-concurrency settings.',
            'Platform policies and channel pools bind by resolution, model or model_resolution. Explicit pools share traffic by pool weights: 10, 9 and 8 give an approximate 10:9:8 ratio. All-zero weights share traffic equally; positive weights exclude zero-weight channels. Without a pool, global channel priorities and weights still apply. The workbench and both task centers display image and video phases together, with video playback, download and gateway-link copying.',
          ],
        },
      ]

  const asyncImage = JSON.stringify(
    {
      provider: 'openai',
      model: 'gpt-image-1',
      prompt: chinese
        ? '清晨自然光下的陶瓷花瓶，简洁构图'
        : 'A ceramic vase in morning light, restrained composition',
      n: 1,
      resolution: '1K',
      size: '1024x1024',
      quality: 'high',
    },
    null,
    2
  )
  const compatibilityImage = JSON.stringify(
    {
      model: 'gpt-image-2',
      prompt: 'A ceramic vase in morning light',
      n: 1,
      resolution: '1K',
      aspect_ratio: '1:1',
    },
    null,
    2
  )
  const geminiBB = JSON.stringify(
    {
      model: 'gemini-3-pro-image-preview',
      stream: false,
      messages: [
        {
          role: 'user',
          content: [{ type: 'text', text: 'A ceramic vase in morning light' }],
        },
      ],
      extra_body: {
        google: { image_config: { image_size: '1K', aspect_ratio: '1:1' } },
      },
    },
    null,
    2
  )
  const geminiSC = JSON.stringify(
    {
      model: 'gemini-3-pro-image-preview',
      prompt: 'A ceramic vase in morning light',
      resolution: '1K',
      aspect_ratio: '1:1',
      image_urls: [],
    },
    null,
    2
  )
  const video = JSON.stringify(
    {
      provider: 'sora',
      model: 'sora-2',
      prompt: chinese
        ? '清晨薄雾中的山间河流，镜头缓慢前移'
        : 'A mountain river at dawn, slow forward camera movement',
      seconds: 4,
      size: '1280x720',
    },
    null,
    2
  )
  const videoImage = JSON.stringify(
    {
      provider: 'xai',
      model: 'grok-imagine-video-1.5',
      prompt: 'Animate the scene with a slow camera push',
      image: { url: 'https://cdn.example/reference.png' },
      seconds: 8,
      resolution: '720p',
      aspect_ratio: '16:9',
    },
    null,
    2
  )
  const videoReferences = JSON.stringify(
    {
      provider: 'xai',
      model: 'grok-imagine-video-1.5',
      prompt: 'Keep the character and use the setting of the second image',
      reference_images: [
        { url: 'https://cdn.example/character.png' },
        { url: 'https://cdn.example/setting.png' },
      ],
      seconds: 8,
      resolution: '720p',
      aspect_ratio: '16:9',
    },
    null,
    2
  )
  const dramaAText = JSON.stringify(
    {
      provider: 'drama',
      model: 'seedance2.0-A',
      prompt: 'Sunrise over the sea, slow forward camera movement',
      seconds: 4,
      resolution: '480p',
      aspect_ratio: '16:9',
    },
    null,
    2
  )
  const dramaAMedia = JSON.stringify(
    {
      provider: 'drama',
      model: 'seedance2.5-A',
      prompt:
        '@图1 walks naturally, follow the movement of @视频1 and the voice of @音频1',
      reference_images: ['https://cdn.example/character.png'],
      reference_videos: ['https://cdn.example/motion.mp4'],
      reference_audios: ['https://cdn.example/voice.mp3'],
      seconds: 8,
      resolution: '720p',
      aspect_ratio: '16:9',
    },
    null,
    2
  )
  const dramaAFrames = JSON.stringify(
    {
      provider: 'drama',
      model: 'seedance2.0-A',
      prompt: 'Make a smooth transition from the first frame to the last frame',
      image: 'https://cdn.example/first.png',
      last_frame: 'https://cdn.example/last.png',
      seconds: 8,
      resolution: '720p',
      aspect_ratio: 'Auto',
    },
    null,
    2
  )
  const dramaH3Reference = JSON.stringify(
    {
      provider: 'drama',
      model: 'minimax-h3',
      prompt: 'Animate the person from the reference image with a gentle wave',
      reference_images: ['https://cdn.example/character.png'],
      seconds: 6,
      resolution: '720p',
      aspect_ratio: '16:9',
      generate_audio: false,
    },
    null,
    2
  )
  const dramaScenarios = [
    {
      id: 'text',
      prompt: 'Sunrise over the sea, slow forward camera movement',
      media: {},
      description: t(
        'Send only prompt and output parameters. No reference media is attached.'
      ),
    },
    {
      id: 'image',
      prompt: 'Animate the character from the reference image',
      media: { reference_images: ['https://cdn.example/character.png'] },
      description: t(
        'reference_images is an array of public HTTPS image URL strings. These are ordinary references, not pinned first frames.'
      ),
    },
    {
      id: 'video',
      prompt:
        'Follow the camera movement and action rhythm of the reference video',
      media: { reference_videos: ['https://cdn.example/motion.mp4'] },
      description: t(
        'reference_videos is an array of public HTTPS video URL strings. It guides motion and camera movement; it does not edit or extend the source clip.'
      ),
    },
    {
      id: 'audio',
      prompt:
        'Let the reference audio guide the mood and movement of an ocean scene',
      media: { reference_audios: ['https://cdn.example/voice.mp3'] },
      description: t(
        'reference_audios is an array of public HTTPS audio URL strings. The audio guides the generated scene or voice.'
      ),
    },
    {
      id: 'mixed',
      prompt:
        'Keep the person from the image, follow the video movement and use the audio as voice guidance',
      media: {
        references: [
          {
            type: 'image',
            role: 'reference',
            source: 'https://cdn.example/character.png',
          },
          {
            type: 'video',
            role: 'reference',
            source: 'https://cdn.example/motion.mp4',
          },
          {
            type: 'audio',
            role: 'reference',
            source: 'https://cdn.example/voice.mp3',
          },
        ],
      },
      description: t(
        'references combines image, video and audio objects. Each entry has type, role and a distinct public HTTPS source. Do not repeat these sources in alias arrays.'
      ),
    },
  ]
  const dramaSeriesExamples: MediaGuideExample[] = [
    {
      id: 'e',
      model: 'seedance2.5-E',
      options: {},
      titles: [
        t('Drama E: generate a video from text'),
        t('Drama E: use reference image URLs'),
        t('Drama E: use a reference video URL'),
        t('Drama E: use a reference audio URL'),
        t('Drama E: combine image, video and audio references'),
      ],
      description: t(
        'This seedance2.5-E example is billed per video and does not send generate_audio. Numbered prompt references are optional.'
      ),
    },
    {
      id: 'f',
      model: 'seedance2.5-F',
      options: { generate_audio: true },
      titles: [
        t('Drama F: generate a video from text'),
        t('Drama F: use reference image URLs'),
        t('Drama F: use a reference video URL'),
        t('Drama F: use a reference audio URL'),
        t('Drama F: combine image, video and audio references'),
      ],
      description: t(
        'This seedance2.5-F example uses 720p and generate_audio=true. Without a reference video it uses seconds; with a video it waits for actual token usage. seedance2.0-F uses 720px.'
      ),
    },
    {
      id: 'g',
      model: 'seedance2.5-G',
      options: { generate_audio: true },
      titles: [
        t('Drama G: generate a video from text'),
        t('Drama G: use reference image URLs'),
        t('Drama G: use a reference video URL'),
        t('Drama G: use a reference audio URL'),
        t('Drama G: combine image, video and audio references'),
      ],
      description: t(
        'This seedance2.5-G example uses 720p and generate_audio=true and is billed by actual token usage. Audio-only references are supported by this model; other G models require an image or video too.'
      ),
    },
  ].flatMap((series) =>
    dramaScenarios.map((scenario, index) => ({
      title: series.titles[index],
      description: `${scenario.description} ${series.description}`,
      code: [
        `curl -X POST "${base}/v1/videos/generations_async" \\`,
        '  -H "Authorization: Bearer $NEW_API_TOKEN" \\',
        "  -H 'Content-Type: application/json' \\",
        `  -H 'Idempotency-Key: drama-${series.id}-${scenario.id}-001' \\`,
        `  --data '${JSON.stringify(
          {
            provider: 'drama',
            model: series.model,
            prompt: scenario.prompt,
            seconds: 8,
            resolution: '720p',
            aspect_ratio: '16:9',
            ...(scenario.media.references && series.id === 'g'
              ? {
                  references: scenario.media.references.map((reference) => ({
                    ...reference,
                    role: `reference_${reference.type}`,
                  })),
                }
              : scenario.media),
            ...series.options,
          },
          null,
          2
        )}'`,
      ].join('\n'),
    }))
  )
  const dramaFirstFrame = JSON.stringify(
    {
      provider: 'drama',
      model: 'seedance2.5-G',
      prompt: 'Start from this image and move the camera forward',
      image: 'https://cdn.example/first.png',
      seconds: 8,
      resolution: '720p',
      aspect_ratio: 'adaptive',
    },
    null,
    2
  )
  const dramaFirstLastFrame = JSON.stringify(
    {
      provider: 'drama',
      model: 'seedance2.5-G',
      prompt: 'Make a smooth transition between the first and last frames',
      image: 'https://cdn.example/first.png',
      last_frame: 'https://cdn.example/last.png',
      seconds: 8,
      resolution: '720p',
      aspect_ratio: 'adaptive',
    },
    null,
    2
  )
  const videoEdit = JSON.stringify(
    {
      provider: 'xai',
      model: 'grok-imagine-video',
      prompt: chinese ? '将天空替换为日落' : 'Replace the sky with a sunset',
      video: { url: 'https://cdn.example/source.mp4' },
    },
    null,
    2
  )
  const videoExtension = JSON.stringify(
    {
      provider: 'doubao',
      model: 'doubao-seedance-2-5-260628',
      prompt: chinese
        ? '向前延长并展示城门后的城市'
        : 'Extend forward and reveal the city beyond the gate',
      video: 'asset://source-video',
      seconds: 8,
      extension_direction: 'forward',
      resolution: '720p',
    },
    null,
    2
  )
  const examples: Record<string, string[]> = {
    authentication: [
      `export NEW_API_TOKEN='YOUR_NEW_API_TOKEN'\nexport NEW_API_BASE='${base}'`,
    ],
    'unified-images': [
      `curl -X POST '${base}/v1/images/generations_async' \\\n  -H "Authorization: Bearer $NEW_API_TOKEN" \\\n  -H 'Content-Type: application/json' \\\n  -H 'Idempotency-Key: image-example-001' \\\n  --data '${asyncImage}'`,
      `curl -X POST '${base}/v1/images/edits_async' \\\n  -H "Authorization: Bearer $NEW_API_TOKEN" \\\n  -H 'Idempotency-Key: image-edit-example-001' \\\n  -F 'provider=openai' -F 'model=gpt-image-1' \\\n  -F 'prompt=Keep the subject and replace the background' \\\n  -F 'resolution=1K' -F 'image=@reference.png;type=image/png'`,
    ],
    compatibility: [
      `curl -X POST '${base}/v1/images/generations_oa' \\\n  -H "Authorization: Bearer $NEW_API_TOKEN" \\\n  -H 'Content-Type: application/json' \\\n  -H 'Idempotency-Key: oa-example-001' --data '${compatibilityImage}'`,
      `curl -X POST '${base}/v1/images/edits_oa' \\\n  -H "Authorization: Bearer $NEW_API_TOKEN" \\\n  -H 'Idempotency-Key: edit-example-001' \\\n  -F 'model=gpt-image-2' -F 'prompt=Use softer morning light' \\\n  -F 'resolution=1K' -F 'aspect_ratio=1:1' -F 'image=@reference.png'`,
      `curl -X POST '${base}/v1/chat/completions_gm' \\\n  -H "Authorization: Bearer $NEW_API_TOKEN" \\\n  -H 'Content-Type: application/json' \\\n  -H 'Idempotency-Key: gm-example-001' --data '${geminiBB}'`,
      `curl -X POST '${base}/v1/images/generations_sc' \\\n  -H "Authorization: Bearer $NEW_API_TOKEN" \\\n  -H 'Content-Type: application/json' \\\n  -H 'Idempotency-Key: sc-example-001' --data '${geminiSC}'`,
    ],
    uploads: [
      `curl -X POST '${base}/v1/uploads/images_sc' \\\n  -H "Authorization: Bearer $NEW_API_TOKEN" \\\n  -H 'Idempotency-Key: upload-example-001' \\\n  -F 'file=@reference.png;type=image/png'`,
    ],
    query: [
      `curl '${base}/v1/media/tasks_async/YOUR_TASK_ID' \\\n  -H "Authorization: Bearer $NEW_API_TOKEN"`,
      `curl '${base}/v1/images/tasks_async/YOUR_TASK_ID' \\\n  -H "Authorization: Bearer $NEW_API_TOKEN"`,
      `curl '${base}/v1/tasks_sc/YOUR_TASK_ID' \\\n  -H "Authorization: Bearer $NEW_API_TOKEN"`,
      `curl -H 'Range: bytes=0-1048575' \\\n  '${base}/v1/media/objects/YOUR_OBJECT_ID?expires=1800000000&access=SIGNED_VALUE'`,
      `curl -I '${base}/v1/media/objects/YOUR_OBJECT_ID?expires=1800000000&access=SIGNED_VALUE'`,
      JSON.stringify(
        {
          task_id: 'asyncimg_example',
          query_url: `${base}/v1/media/tasks_async/asyncimg_example`,
        },
        null,
        2
      ),
      JSON.stringify(
        {
          task_id: 'video_example',
          query_url: `${base}/v1/media/tasks_async/video_example`,
        },
        null,
        2
      ),
      JSON.stringify(
        {
          task_id: 'asyncimg_example',
          media_type: 'image',
          provider: 'openai',
          protocol: 'openai_images',
          stage: 'completed',
          status: 'succeeded',
          progress: 100,
          storage_status: 'succeeded',
          billing_status: 'succeeded',
          data: [{ url: 'https://gateway.example/api/image-objects/example' }],
        },
        null,
        2
      ),
      JSON.stringify(
        {
          task_id: 'video_example',
          media_type: 'video',
          provider: 'sora',
          protocol: 'openai_video',
          stage: 'completed',
          status: 'succeeded',
          progress: 100,
          storage_status: 'succeeded',
          billing_status: 'settled',
          data: [
            {
              key: 'video',
              url: `${base}/v1/media/objects/YOUR_OBJECT_ID?expires=1800000000&access=SIGNED_VALUE`,
              content_type: 'video/mp4',
              byte_size: 10485760,
              checksum: 'sha256-hex',
              expires_at: 1800000000,
            },
          ],
        },
        null,
        2
      ),
      JSON.stringify(
        {
          task_id: 'asyncimg_example',
          media_type: 'image',
          stage: 'failed',
          status: 'failed',
          error_code: 601,
          fail_reason: 'Content policy rejection',
        },
        null,
        2
      ),
    ],
  }
  const videoExamples: MediaGuideExample[] = [
    {
      title: t('Sora: generate a video from text'),
      description: t(
        'No reference image is sent. provider=sora selects a Sora channel; sora-2, seconds=4 and size=1280x720 are Sora example values. A successful submission returns a durable task_id and query_url.'
      ),
      code: `curl -X POST '${base}/v1/videos/generations_async' \\\n  -H "Authorization: Bearer $NEW_API_TOKEN" \\\n  -H 'Content-Type: application/json' \\\n  -H 'Idempotency-Key: video-example-001' \\\n  --data '${video}'`,
    },
    {
      title: t('Sora: upload a local reference image'),
      description: t(
        'Replace reference.png with a file on the machine running curl. input_reference is the file field; @ reads the file and type=image/png declares its MIME type. This Sora upload form is not supported by Drama or official Doubao Seedance.'
      ),
      code: `curl -X POST '${base}/v1/videos/generations_async' \\\n  -H "Authorization: Bearer $NEW_API_TOKEN" \\\n  -H 'Idempotency-Key: video-reference-001' \\\n  -F 'provider=sora' -F 'model=sora-2' \\\n  -F 'prompt=Animate this scene' -F 'seconds=4' -F 'size=1280x720' \\\n  -F 'input_reference=@reference.png;type=image/png'`,
    },
    {
      title: t('xAI: generate from one image URL'),
      description: t(
        'image is one first-frame image. This xAI model accepts either "image":"https://..." or "image":{"url":"https://..."}; the example uses the object form. seconds, resolution and aspect_ratio control the output, not the input image.'
      ),
      code: `curl -X POST '${base}/v1/videos/generations_async' \\\n  -H "Authorization: Bearer $NEW_API_TOKEN" \\\n  -H 'Content-Type: application/json' \\\n  -H 'Idempotency-Key: video-image-url-001' \\\n  --data '${videoImage}'`,
    },
    {
      title: t('xAI: send an image URL in a multipart field'),
      description: t(
        'For this xAI form, image contains JSON object text. There is no @ before the URL and no file is uploaded. Do not replace a Sora file part with this field or use it with a JSON-only model.'
      ),
      code: `curl -X POST '${base}/v1/videos/generations_async' \\\n  -H "Authorization: Bearer $NEW_API_TOKEN" \\\n  -H 'Idempotency-Key: video-multipart-url-001' \\\n  -F 'provider=xai' -F 'model=grok-imagine-video-1.5' \\\n  -F 'prompt=Animate this scene' -F 'seconds=8' \\\n  -F 'resolution=720p' -F 'aspect_ratio=16:9' \\\n  -F 'image={"url":"https://cdn.example/reference.png"}'`,
    },
    {
      title: t('xAI 1.5: use multiple reference image URLs'),
      description: t(
        'reference_images is a JSON array of ordinary references, not a single first frame. This xAI 1.5 example uses two URL objects at 720p; the classic model does not support this mode. Do not join URLs with commas inside one string.'
      ),
      code: `curl -X POST '${base}/v1/videos/generations_async' \\\n  -H "Authorization: Bearer $NEW_API_TOKEN" \\\n  -H 'Content-Type: application/json' \\\n  -H 'Idempotency-Key: video-reference-urls-001' \\\n  --data '${videoReferences}'`,
    },
    {
      title: t('Drama A: generate a video from text'),
      description: t(
        'Use provider=drama and seedance2.0-A. The example explicitly sets the defaults: 4 seconds, 480p and 16:9. A 2.5 allows up to 30 seconds at 480p or 720p; do not copy E/F/G token pricing to these seconds-billed models.'
      ),
      code: `curl -X POST '${base}/v1/videos/generations_async' \\\n  -H "Authorization: Bearer $NEW_API_TOKEN" \\\n  -H 'Content-Type: application/json' \\\n  -H 'Idempotency-Key: drama-a-text-001' \\\n  --data '${dramaAText}'`,
    },
    {
      title: t('Drama A: reference images, video and audio by number'),
      description: t(
        'Each reference field is an array of public HTTPS URL strings. @图1, @视频1 and @音频1 refer to the first item of their respective arrays; @图片1 is equivalent to @图1. Every ordinary reference must be mentioned, and unused or out-of-range references are rejected.'
      ),
      code: `curl -X POST '${base}/v1/videos/generations_async' \\\n  -H "Authorization: Bearer $NEW_API_TOKEN" \\\n  -H 'Content-Type: application/json' \\\n  -H 'Idempotency-Key: drama-a-references-001' \\\n  --data '${dramaAMedia}'`,
    },
    {
      title: t('Drama A 2.0: provide paired first and last frames'),
      description: t(
        'This mode requires both image and last_frame with aspect_ratio=Auto. Both URLs must be different and publicly readable; do not add ordinary references or numbered prompt references. A 2.5 and Drama minimax-h3 do not expose this mode.'
      ),
      code: `curl -X POST '${base}/v1/videos/generations_async' \\\n  -H "Authorization: Bearer $NEW_API_TOKEN" \\\n  -H 'Content-Type: application/json' \\\n  -H 'Idempotency-Key: drama-a-frames-001' \\\n  --data '${dramaAFrames}'`,
    },
    {
      title: t('Drama minimax-h3: use a reference image without audio'),
      description: t(
        'Use lowercase minimax-h3 with provider=drama. reference_images contains ordinary references, not pinned frames; numbered prompt references are optional. generate_audio=false is sent upstream. Supported output is 4–15 seconds, 480p/720p/1080p and 16:9/9:16; 2k is a separate model.'
      ),
      code: `curl -X POST '${base}/v1/videos/generations_async' \\\n  -H "Authorization: Bearer $NEW_API_TOKEN" \\\n  -H 'Content-Type: application/json' \\\n  -H 'Idempotency-Key: drama-h3-reference-001' \\\n  --data '${dramaH3Reference}'`,
    },
    ...dramaSeriesExamples,
    {
      title: t('Drama G: use a first-frame image URL'),
      description: t(
        'Here image is the first frame, not an ordinary reference_images entry. seedance2.5-G requires aspect_ratio=adaptive in this mode and an explicit positive seconds value. Replace the image URL with your own publicly readable image.'
      ),
      code: `curl -X POST '${base}/v1/videos/generations_async' \\\n  -H "Authorization: Bearer $NEW_API_TOKEN" \\\n  -H 'Content-Type: application/json' \\\n  -H 'Idempotency-Key: drama-first-frame-001' \\\n  --data '${dramaFirstFrame}'`,
    },
    {
      title: t('Drama G: specify both first and last image URLs'),
      description: t(
        'image supplies the first frame and last_frame supplies the last frame. For seedance2.5-G use adaptive, provide both frames, and do not mix this mode with ordinary image, video or audio references.'
      ),
      code: `curl -X POST '${base}/v1/videos/generations_async' \\\n  -H "Authorization: Bearer $NEW_API_TOKEN" \\\n  -H 'Content-Type: application/json' \\\n  -H 'Idempotency-Key: drama-first-last-frame-001' \\\n  --data '${dramaFirstLastFrame}'`,
    },
    {
      title: t('xAI: edit a video as a durable task'),
      description: t(
        'Edits require JSON and a direct source video, not source_task_id. Classic grok-imagine-video inherits source duration and dimensions, so this example omits seconds, size and resolution; 1.5 does not support editing. Query the returned durable task through /v1/media/tasks_async/{task_id}.'
      ),
      code: `curl -X POST '${base}/v1/videos/edits_async' \\\n  -H "Authorization: Bearer $NEW_API_TOKEN" \\\n  -H 'Content-Type: application/json' \\\n  -H 'Idempotency-Key: video-edit-example-001' \\\n  --data '${videoEdit}'`,
    },
    {
      title: t('Doubao Seedance: extend a video as a durable task'),
      description: t(
        'This example uses official doubao-seedance-2-5-260628, not a Drama model. Replace asset://source-video with an asset owned by the selected upstream account or a public source-video URL. seconds is the extension duration; forward is this example direction. Do not add duration as well.'
      ),
      code: `curl -X POST '${base}/v1/videos/extensions_async' \\\n  -H "Authorization: Bearer $NEW_API_TOKEN" \\\n  -H 'Content-Type: application/json' \\\n  -H 'Idempotency-Key: video-extension-example-001' \\\n  --data '${videoExtension}'`,
    },
    {
      title: t('Compatibility route: edit a standard video task'),
      description: t(
        'This standard task-plugin route returns a video task object with id. Query GET /v1/videos/{id}; it does not use the durable task_id/query_url receipt or the durable media query route. Use the *_async examples above for local persistence and recovery.'
      ),
      code: `curl -X POST '${base}/v1/videos/edits' -H "Authorization: Bearer $NEW_API_TOKEN" -H 'Content-Type: application/json' --data '${videoEdit}'`,
    },
    {
      title: t('Compatibility route: extend a standard video task'),
      description: t(
        'This standard task-plugin route returns a video task object with id. Query GET /v1/videos/{id}; it does not use the durable task_id/query_url receipt or the durable media query route. Use the *_async examples above for local persistence and recovery.'
      ),
      code: `curl -X POST '${base}/v1/videos/extensions' -H "Authorization: Bearer $NEW_API_TOKEN" -H 'Content-Type: application/json' --data '${videoExtension}'`,
    },
  ]
  return sections.map((section) => ({
    ...section,
    examples:
      section.id === 'video'
        ? videoExamples
        : (examples[section.id] || []).map(
            (code): MediaGuideExample => ({ code })
          ),
  }))
}
