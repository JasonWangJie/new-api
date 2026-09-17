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
  ['POST', '/v1/images/generations_oa', 'OpenAI BB / SC'],
  ['POST', '/v1/images/edits_oa', 'OpenAI edits'],
  ['POST', '/v1/chat/completions_gm', 'Gemini BB'],
  ['POST', '/v1/uploads/images_sc', 'Gemini SC upload'],
  ['POST', '/v1/images/generations_sc', 'Gemini SC'],
  ['GET', '/v1/images/tasks_async/{task_id}', 'Task query'],
  ['GET', '/v1/tasks_sc/{task_id}', 'SC query alias'],
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

export function asyncImageGuide(chinese: boolean, base: string) {
  const sections = chinese
    ? [
        {
          id: 'overview',
          title: '调用流程',
          paragraphs: [
            '新接口先持久受理，立即返回任务 ID，再通过统一查询接口读取进度和结果。提交成功不是图片生成成功。原同步图片、聊天、视频和插件接口保持原行为。',
            'OpenAI BB / SC 共用 _oa 路径。Gemini BB 使用 _gm，SC 使用 _sc。不得将不支持的链路自动切到另一平台或同步模式。',
          ],
        },
        {
          id: 'authentication',
          title: '鉴权与幂等',
          paragraphs: [
            '所有公共路径使用 Authorization: Bearer <NEW_API_TOKEN>。请在 API Keys 中创建允许图片平台、分组和模型的 Key。查询必须使用提交时的同一 Token，其他 Token 或用户查询返回 404。',
            'Idempotency-Key 可选，最大 255 个 UTF-8 字节。重试必须复用同一路径、同一 Token、同一 Key 和完全相同的原始请求字节；空白、字段顺序或字段值变化都会产生 409。不同任务使用新 Key。未提供 Key 的每次提交视为新请求。',
          ],
        },
        {
          id: 'openai',
          title: 'OpenAI 生成与编辑',
          paragraphs: [
            'POST /v1/images/generations_oa 支持 JSON 和 multipart。有效 image_urls 或 images[].image_url 使请求进入图生图；POST /v1/images/edits_oa 必须提供有效参考图。JSON 不支持 images[].file_id。',
            '支持 model、prompt、n、size、resolution、aspect_ratio、quality，以及原链支持的 output_format、background、output_compression、moderation、style、input_fidelity 等原生参数。显式 0 / false 不会被包装层丢弃；非法 n=0 会在受理前拒绝。n 为 1–128，实际模型可能有更小上限。',
            'mask.image_url 只可随有效参考图使用。multipart 使用重复 image 文件字段及按模型能力支持的 mask 文件字段。无效空参考值不能掩盖混合请求中的非法参考图。',
          ],
        },
        {
          id: 'dimensions',
          title: '尺寸与计费规格',
          paragraphs: [
            'resolution 接受 1K、2K、4K、AUTO；aspect_ratio 接受 auto 和下表比例。默认 auto。显式比例优先于 size 内的比例；原生 WxH 与显式分辨率冲突时返回 400。',
            'OpenAI 未带显式别名的原生 WxH 按长边确定 1K（≤1024）、2K（≤2048）或 4K。Gemini 按短边判定。合法的显式别名按其请求档位计费，例如 1K / 21:9 的映射长边仍可能超过 2048。0.5K 使用最低 1K 账单档，且只对允许该能力的 Gemini 模型开放。',
          ],
        },
        {
          id: 'gemini',
          title: 'Gemini BB 与 SC',
          paragraphs: [
            'BB 使用 POST /v1/chat/completions_gm，messages 中的 text 与 image_url 保持原始顺序，stream 必须为 false 或省略。extra_body.google.image_config.image_size 接受 0.5K、1K、2K、4K，aspect_ratio 可选。',
            'SC 使用 POST /v1/images/generations_sc，JSON 包含 model、prompt、image_urls、size 或 resolution、aspect_ratio 或兼容 ratio。Gemini 每次请求数量固定为 1，但服务端会保存上游实际返回的全部图片。',
            '全局参考图上限默认 8，已知 Flash Image 模型本地上限 3，Pro Image 能力 14，最终取模型和全局限制中较小值。提交阶段超限返回 400；601–613 是已受理任务的查询错误分类。',
          ],
        },
        {
          id: 'uploads',
          title: 'SC 单图上传',
          paragraphs: [
            'POST /v1/uploads/images_sc 仅接受 multipart/form-data 的一个非空 file 字段，格式限 PNG、JPEG、WebP。默认单图 32 MiB、每 Key 每分钟 20 次、临时输入总量 1 GiB，保留 24 小时。无效请求也消耗上传次数，超限 429。',
            '上传使用独立 Idempotency-Key，同 Key 的字节、声明 MIME、文件名或长度变化返回 409。上传进行中返回 409；重试不会释放未知上传的预留字节。成功响应是 {url, created_at}，重放带 Idempotency-Replayed。',
            '上传 URL 绑定该 Token。过期、已删除、其他 Token 的已知上传 URL 均拒绝使用，不作为普通远程 URL 重试。重复签名别名最多 128 个。迟到上传通过两次删除、至少间隔 10 分钟确认后回收。',
          ],
        },
        {
          id: 'query',
          title: '轮询与响应',
          paragraphs: [
            '受理返回 HTTP 202、Location 和 Retry-After: 3。OpenAI / SC 包含 task_id 与 query_url；Gemini BB 包含 id、object: image.task、status: queued。统一查询及 SC 别名返回 200。',
            'status 为 queued、processing、succeeded 或 failed。queued / processing 不含可用图片；succeeded 的 data 数组包含全部结果 URL；failed 的 error_code 为 601–613。图片、账务和日志整批确认后才公布成功 URL。',
            '建议至少每 3 秒查询，网络、503 或 5xx 时延长到 5–30 秒再查；一次查询故障不代表生成失败。签名默认 3600 秒，任务与结果默认保留 90 天。重新查询或稳定 view 接口获取新签名，不持久保存签名为对象身份。',
          ],
        },
        {
          id: 'billing',
          title: '执行、重试与计费',
          paragraphs: [
            '提交只鉴权、校验并持久受理，不调用上游、不占渠道执行并发、不预扣图片费用。Worker 执行前检查当前资格，成功后按实际产物固定账单。后台改价不会改变已固定账单。',
            '参考拉取、容量及临时上游错误按独立预算重试。可能已调用上游但结果未持久化的任务进入 execution_unknown，禁止自动重新生图。上传或结算失败只重试后处理；管理员恢复不会重新调用上游。',
            '已结算的任务即使之后被管理员结束，也不会隐式退款；详情记录账务与对账状态。不要通过重新提交生成来修复下载、本机保存、存储或账务问题，以免产生新费用。',
          ],
        },
        {
          id: 'errors',
          title: '错误分类与排查',
          paragraphs: [
            '受理前错误使用非 2xx HTTP 和 error 对象，包括鉴权失败、平台禁用、参数错误、409 幂等冲突、429 上传限额、503 数据库 / Redis / 存储或密钥不可用。不能把请求阶段的错误当作任务查询错误码。',
            '先保存任务 ID 并检查任务中心事件。601、604、607、608、610–613 通常需要修正输入或人工处理；602、603、605、606 会按分类预算重试；609 检查存储或账务，恢复只处理已有产物。',
          ],
        },
        {
          id: 'workbench',
          title: '站内工作台、图库与投稿',
          paragraphs: [
            '工作台由所选 Key 的服务端能力决定实时或异步，提交前复核版本。异步成功只预览并保留在任务中心，不自动写本机图库。实时图片保存在当前设备的 IndexedDB Blob 图库，默认每用户 30 天、100 张、200 MiB。',
            '本机延期投稿审核前只发送元数据，默认不分享提示词。批准进入 approved_pending_sync；用户携原 Blob 同步并通过 SHA-256、大小、MIME 和完整图片校验后才公开。等待同步的原图有独立本机额度。',
            '手工从任务归档及服务器资产投稿使用持久存储。已隐藏、撤回、过期或未发布的作品不能通过广场公开接口读取。清理先预览，再创建任务，并在删除前重新检查引用。',
          ],
        },
      ]
    : [
        {
          id: 'overview',
          title: 'Workflow',
          paragraphs: [
            'New endpoints durably accept a request and return a task ID immediately. Poll the unified query endpoint for progress and results. Acceptance does not mean generation succeeded. Existing synchronous, video and plugin endpoints retain their behavior.',
            'OpenAI BB and SC share the _oa routes. Gemini BB uses _gm; Gemini SC uses _sc. Do not silently switch platforms or fall back to synchronous generation.',
          ],
        },
        {
          id: 'authentication',
          title: 'Authentication and idempotency',
          paragraphs: [
            'Send Authorization: Bearer <NEW_API_TOKEN> on every public endpoint. Create an API key allowed to use the image platform, group and model. Poll with the exact submitting token; another token or user receives 404.',
            'Idempotency-Key is optional and limited to 255 UTF-8 bytes. A retry must use the same path, token, key and exact raw request bytes. Changed whitespace, field order or values produce 409. Use a fresh key for a new task. Without a key, each submission is a new request.',
          ],
        },
        {
          id: 'openai',
          title: 'OpenAI generation and editing',
          paragraphs: [
            'POST /v1/images/generations_oa accepts JSON and multipart. Valid image_urls or images[].image_url trigger image editing. POST /v1/images/edits_oa requires valid reference images. JSON images[].file_id is unsupported.',
            'Supported fields include model, prompt, n, size, resolution, aspect_ratio, quality and native fields supported by the existing adaptor, such as output_format, background, output_compression, moderation, style and input_fidelity. Explicit zero and false values survive wrapping. n=0 is rejected; n is 1–128, subject to a smaller model limit.',
            'mask.image_url requires reference images. Multipart accepts repeated image fields and a mask field where the selected model supports it. Empty values must not hide malformed references in a mixed request.',
          ],
        },
        {
          id: 'dimensions',
          title: 'Dimensions and billing tiers',
          paragraphs: [
            'resolution supports 1K, 2K, 4K and AUTO. aspect_ratio supports auto and the ratios below. The default is auto. An explicit ratio takes precedence over a ratio in size. A native WxH that conflicts with the explicit resolution is rejected with 400.',
            'For native dimensions without an explicit alias, OpenAI uses the longer edge and Gemini the shorter edge: up to 1024 is 1K, up to 2048 is 2K, and larger is 4K. A valid explicit alias retains its requested tier even when the mapped edge is larger, such as 1K / 21:9. Gemini 0.5K is available only for enabled models and uses the minimum 1K billing tier.',
          ],
        },
        {
          id: 'gemini',
          title: 'Gemini BB and SC',
          paragraphs: [
            'BB uses POST /v1/chat/completions_gm. text and image_url parts in messages preserve order. Omit stream or use false. extra_body.google.image_config.image_size accepts 0.5K, 1K, 2K and 4K, with optional aspect_ratio.',
            'SC uses POST /v1/images/generations_sc with model, prompt, image_urls, size or resolution, and aspect_ratio or the ratio alias. Gemini accepts one generation request at a time; every image actually returned upstream is persisted.',
            'The global reference limit defaults to 8. Known Flash Image models allow 3 and Pro Image models allow 14, bounded by the global limit. Admission violations return HTTP 400. Codes 601–613 describe failures of accepted tasks.',
          ],
        },
        {
          id: 'uploads',
          title: 'SC image uploads',
          paragraphs: [
            'POST /v1/uploads/images_sc accepts exactly one nonempty multipart file field containing PNG, JPEG or WebP. Defaults are 32 MiB per image, 20 attempts per key per minute, 1 GiB of reserved input bytes per key and 24-hour retention. Invalid attempts count; rate-limit violations return 429.',
            'Uploads have a separate Idempotency-Key scope. Changed bytes, declared MIME, filename or length produce 409. An upload in progress returns 409. Unknown uploads retain their byte reservations. Successful responses contain {url, created_at}; replays include Idempotency-Replayed.',
            'Uploaded URLs belong to the uploading token. Known foreign, expired or deleted input aliases never fall back to ordinary remote URLs. At most 128 signed aliases are retained. Late uploads are reclaimed only after two confirmed deletes at least ten minutes apart.',
          ],
        },
        {
          id: 'query',
          title: 'Polling and responses',
          paragraphs: [
            'Acceptance returns HTTP 202, Location and Retry-After: 3. OpenAI / SC return task_id and query_url. Gemini BB returns id, object: image.task and status: queued. Both task-query routes return HTTP 200.',
            'Public status is queued, processing, succeeded or failed. Queued and processing responses expose no usable result images. Succeeded data contains every result URL. Failed error_code is 601–613. Success is visible only after the whole image manifest, billing and log confirmation complete.',
            'Poll no faster than every three seconds. Back off to five–thirty seconds for network errors, 503 or 5xx. A query failure is not a generation failure. Signatures default to 3600 seconds; tasks and results to 90 days. Query again or use the stable view endpoint for a fresh signature.',
          ],
        },
        {
          id: 'billing',
          title: 'Execution, retries and billing',
          paragraphs: [
            'Submission authenticates, validates and durably accepts the task. It does not invoke upstream, occupy an execution slot or precharge image costs. Workers recheck eligibility before execution and freeze a bill from actual output. Later price edits do not alter that bill.',
            'Reference fetch, capacity and temporary upstream failures have separate retry budgets. A task that may have invoked upstream without persisting output becomes execution_unknown and is never automatically regenerated. Upload and settlement retries, including administrator recovery, process existing output only.',
            'Ending an already settled task does not implicitly refund it; reconciliation is recorded in task details. Do not submit another generation to repair downloads, local saving, storage or settlement, because that creates a new billable request.',
          ],
        },
        {
          id: 'errors',
          title: 'Errors and troubleshooting',
          paragraphs: [
            'Admission failures use non-2xx HTTP and an error object: authentication, disabled platform, invalid input, 409 idempotency conflicts, 429 upload limits and 503 database, Redis, storage or encryption-key availability. Admission errors are separate from accepted-task error codes.',
            'Retain the task ID and inspect task-center events. Codes 601, 604, 607, 608 and 610–613 generally require corrected input or manual investigation. Codes 602, 603, 605 and 606 use classified retry budgets. For 609, inspect storage or billing and recover existing output only.',
          ],
        },
        {
          id: 'workbench',
          title: 'Workbench, local library and publishing',
          paragraphs: [
            'The selected key’s server capabilities determine realtime or async mode and are checked again before submission. Async results are previewed in the task center and never automatically added to the local library. Realtime images are stored as IndexedDB Blobs on this device: 30 days, 100 images and 200 MiB per user by default.',
            'Local deferred submissions send metadata only before review, with prompt sharing off by default. Approval means approved_pending_sync. The exact original Blob must pass SHA-256, byte-size, MIME and complete-image validation before it is uploaded and published. Pending originals use a separate local quota.',
            'Explicit task archiving and server-asset publications use durable storage. Hidden, withdrawn, expired and unpublished works cannot be read through public plaza content routes. Cleanup requires a preview and a persistent job, then rechecks references before deletion.',
          ],
        },
      ]
  const oa = JSON.stringify(
    {
      model: 'gpt-image-2',
      prompt: chinese
        ? '清晨自然光下的陶瓷花瓶，简洁构图'
        : 'A ceramic vase in morning light, restrained composition',
      n: 1,
      resolution: '1K',
      aspect_ratio: '1:1',
      quality: 'high',
    },
    null,
    2
  )
  const gm = JSON.stringify(
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
  const sc = JSON.stringify(
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
  const examples: Record<string, string[]> = {
    authentication: [
      `export NEW_API_TOKEN='YOUR_NEW_API_TOKEN'\nexport NEW_API_BASE='${base}'`,
    ],
    openai: [
      `curl -X POST '${base}/v1/images/generations_oa' \\\n  -H "Authorization: Bearer $NEW_API_TOKEN" \\\n  -H 'Content-Type: application/json' \\\n  -H 'Idempotency-Key: oa-example-001' \\\n  --data '${oa}'`,
      `curl -X POST '${base}/v1/images/edits_oa' \\\n  -H "Authorization: Bearer $NEW_API_TOKEN" \\\n  -H 'Idempotency-Key: edit-example-001' \\\n  -F 'model=gpt-image-2' -F 'prompt=Use softer morning light' \\\n  -F 'resolution=1K' -F 'aspect_ratio=1:1' -F 'image=@reference.png'`,
    ],
    gemini: [
      `curl -X POST '${base}/v1/chat/completions_gm' \\\n  -H "Authorization: Bearer $NEW_API_TOKEN" \\\n  -H 'Content-Type: application/json' \\\n  -H 'Idempotency-Key: gm-example-001' --data '${gm}'`,
      `curl -X POST '${base}/v1/images/generations_sc' \\\n  -H "Authorization: Bearer $NEW_API_TOKEN" \\\n  -H 'Content-Type: application/json' \\\n  -H 'Idempotency-Key: sc-example-001' --data '${sc}'`,
    ],
    uploads: [
      `curl -X POST '${base}/v1/uploads/images_sc' \\\n  -H "Authorization: Bearer $NEW_API_TOKEN" \\\n  -H 'Idempotency-Key: upload-example-001' -F 'file=@reference.png'`,
    ],
    query: [
      `curl '${base}/v1/images/tasks_async/YOUR_TASK_ID' \\\n  -H "Authorization: Bearer $NEW_API_TOKEN"`,
      `curl '${base}/v1/tasks_sc/YOUR_TASK_ID' \\\n  -H "Authorization: Bearer $NEW_API_TOKEN"`,
      JSON.stringify(
        {
          task_id: 'asyncimg_example',
          query_url: `${base}/v1/images/tasks_async/asyncimg_example`,
        },
        null,
        2
      ),
      JSON.stringify(
        { id: 'asyncimg_example', object: 'image.task', status: 'queued' },
        null,
        2
      ),
      JSON.stringify(
        {
          task_id: 'asyncimg_example',
          status: 'succeeded',
          data: [{ url: 'https://example.com/image.png' }],
        },
        null,
        2
      ),
      JSON.stringify(
        {
          task_id: 'asyncimg_example',
          status: 'failed',
          error_code: 601,
          fail_reason: 'Content policy rejection',
        },
        null,
        2
      ),
    ],
  }
  return sections.map((section) => ({
    ...section,
    examples: examples[section.id] || [],
  }))
}
