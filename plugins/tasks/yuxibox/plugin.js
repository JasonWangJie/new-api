const G_RATIOS = ["16:9", "9:16", "4:3", "3:4", "1:1", "21:9", "adaptive"];
const MODELS = {
  "seedance-2.0-mini-yx-g": { min: 4, max: 15, defaultSeconds: 15, resolutions: ["480p", "720p"], images: 9, videos: 3, audios: 3, frames: true },
  "seedance-2.0-mini-yx-g-no-face": { min: 4, max: 15, defaultSeconds: 15, resolutions: ["480p", "720p"], images: 9, videos: 3, audios: 3, frames: true },
  "seedance-2.0-fast-yx-g": { min: 4, max: 15, defaultSeconds: 15, resolutions: ["480p", "720p"], images: 9, videos: 3, audios: 3, frames: true },
  "seedance-2.0-fast-yx-g-no-face": { min: 4, max: 15, defaultSeconds: 15, resolutions: ["480p", "720p"], images: 9, videos: 3, audios: 3, frames: true },
  "seedance-2.0-yx-g": { min: 4, max: 15, defaultSeconds: 15, resolutions: ["480p", "720p", "1080p", "4k"], images: 9, videos: 3, audios: 3, frames: true },
  "seedance-2.0-yx-g-no-face": {
    min: 4,
    max: 15,
    defaultSeconds: 15,
    resolutions: ["480p", "720p", "1080p", "4k"],
    images: 9,
    videos: 3,
    audios: 3,
    frames: true,
  },
  "seedance-2.5-yx-g": { min: 4, max: 30, defaultSeconds: 30, resolutions: ["480p", "720p", "1080p"], images: 30, videos: 10, audios: 10, frames: true },
  "seedance-2.5-yx-g-no-face": {
    min: 4,
    max: 30,
    defaultSeconds: 30,
    resolutions: ["480p", "720p", "1080p"],
    images: 30,
    videos: 10,
    audios: 10,
    frames: true,
  },
  "seedance-2.5-yx-sz": {
    min: 5,
    max: 30,
    defaultSeconds: 5,
    resolutions: ["480p", "720p", "1080p"],
    images: 30,
    videos: 10,
    audios: 10,
    frames: true,
    seconds: true,
  },
  "seedance2.5-yx-count": { min: 5, max: 30, defaultSeconds: 30, resolutions: ["720p"], images: 30, videos: 10, audios: 10, total: 30, count: true },
  "minimax-h3-quality": { min: 1, max: 15, defaultSeconds: 5, resolutions: ["480p", "768p"], images: 9, videos: 0, audios: 3, h3: true, requiresImage: true },
  "minimax-h3-fast": { min: 1, max: 15, defaultSeconds: 5, resolutions: ["480p", "768p"], images: 9, videos: 0, audios: 3, h3: true, requiresImage: true },
  "minimax-h3-15s": { min: 1, max: 15, defaultSeconds: 5, resolutions: ["480p", "768p"], images: 9, videos: 0, audios: 3, h3: true, portrait: true },
};
const REQUEST_FIELDS = new Set([
  "model",
  "mode",
  "prompt",
  "seconds",
  "duration",
  "resolution",
  "size",
  "aspect_ratio",
  "ratio",
  "aspectRatio",
  "image",
  "input_reference",
  "first_frame",
  "start_frame",
  "last_frame",
  "end_frame",
  "reference_images",
  "images",
  "image_urls",
  "reference_videos",
  "videos",
  "video_urls",
  "reference_audios",
  "audios",
  "audio_urls",
  "references",
  "seed",
  "generate_audio",
  "enable_audio",
  "watermark",
  "prompt_extend",
]);
const RESOLUTION_SCHEMA = { enum: ["480p", "720p", "768p", "1080p", "4k"], description: { en: "Output video resolution", zh: "输出视频分辨率" } };
const SECOND_SCHEMA = { type: "number", unit: "second", description: { en: "Video generation unit price", zh: "视频生成单价" } };
const COUNT_SCHEMA = {
  type: "number",
  unit: "count",
  unitLabel: { en: "video", zh: "条" },
  description: { en: "Video generation unit price", zh: "视频生成单价" },
};
const PERIOD_SCHEMA = {
  enum: ["peak", "off_peak"],
  description: { en: "Video generation billing period", zh: "视频生成计费时段" },
  enumLabels: { peak: { en: "Peak period", zh: "高峰时段" }, off_peak: { en: "Off-peak period", zh: "低峰时段" } },
};

export const meta = {
  apiVersion: 1,
  key: "yuxibox",
  name: "Yuxibox Video",
  icon: "text:YX",
  version: "1.0.2",
  author: { name: "new-api contributors" },
  baseUrl: "https://yuxibox.xyz",
  website: "https://yuxibox.xyz/docs",
  description: { en: "Yuxibox Seedance and MiniMax H3 video generation with media references", zh: "Yuxibox Seedance 与 MiniMax H3 媒体参考视频生成" },
  models: Object.keys(MODELS),
  fetchMode: "per_task",
  protocols: ["openai_video"],
  usageSchema: { video_count: COUNT_SCHEMA, resolution: RESOLUTION_SCHEMA },
  usageProfiles: Object.entries(MODELS).map(([model, config]) => ({
    models: [model],
    schema: config.count
      ? { video_count: COUNT_SCHEMA }
      : {
          ...(config.seconds || config.h3 ? { seconds: SECOND_SCHEMA } : { video_count: COUNT_SCHEMA }),
          resolution: { ...RESOLUTION_SCHEMA, enum: config.resolutions },
          ...(config.h3 ? { billing_period: PERIOD_SCHEMA } : {}),
        },
  })),
  videoProfiles: Object.entries(MODELS).map(([model, config]) => {
    const ratios = config.h3
      ? config.portrait
        ? ["16:9", "9:16"]
        : ["16:9", "9:16", "1:1"]
      : config.seconds
        ? G_RATIOS.filter((ratio) => ratio !== "adaptive")
        : G_RATIOS;
    const settings = {
      duration: { min: config.min, max: config.max, step: 1, default: config.defaultSeconds },
      resolutions: config.resolutions,
      defaultResolution: config.h3 ? "768p" : "720p",
      aspectRatios: ratios,
      defaultAspectRatio: config.h3 ? "9:16" : "16:9",
    };
    const inputs = [
      { name: "reference_images", kind: "image", sources: ["url"], maxItems: config.images, ...(config.requiresImage ? { required: true } : {}) },
    ];
    if (config.videos) inputs.push({ name: "reference_videos", kind: "video", sources: ["url"], maxItems: config.videos });
    inputs.push({ name: "reference_audios", kind: "audio", sources: ["url"], maxItems: config.audios });
    const modes = [{ ...settings, name: "reference_to_video", inputs }];
    if (!config.requiresImage) modes.unshift({ ...settings, name: "text_to_video" });
    if (config.frames)
      modes.push({
        ...settings,
        name: "first_last_frame",
        inputs: [
          { name: "image", kind: "image", sources: ["url"], maxItems: 1, required: true },
          { name: "last_frame", kind: "image", sources: ["url"], maxItems: 1, required: true },
        ],
      });
    return { models: [model], modes };
  }),
};

function mediaURL(value) {
  // References stay byte-for-byte intact, including signed query parameters.
  // oxlint-disable no-control-regex -- Reject controls and URL parser ambiguities
  if (typeof value !== "string" || !value || value.length > 8192 || /[\s\\\x00-\x1f\x7f]/.test(value) || value.includes("#"))
    throw new Error("Media references must be public HTTPS URLs");
  // oxlint-enable no-control-regex
  const match = /^https:\/\/([a-z0-9](?:[a-z0-9.-]*[a-z0-9])?)(?::([0-9]{1,5}))?(?:[/?].*)?$/i.exec(value);
  if (!match || (match[2] && (Number(match[2]) < 1 || Number(match[2]) > 65535)))
    throw new Error("Media references must use HTTPS URLs without credentials or IPv6 literals");
  const host = match[1].toLowerCase();
  const labels = host.split(".");
  if (
    host.length > 253 ||
    labels.length < 2 ||
    [".local", ".localhost", ".internal", ".localdomain", ".lan"].some((suffix) => host.endsWith(suffix)) ||
    labels.some((label) => !label || label.length > 63 || label.startsWith("-") || label.endsWith("-"))
  )
    throw new Error("Media references must use public hosts");
  if (/^[0-9.]+$/.test(host)) {
    const ip = labels.map(Number);
    if (ip.length !== 4 || labels.some((label, index) => String(ip[index]) !== label || ip[index] > 255))
      throw new Error("Media reference IP address is invalid");
    if (
      [0, 10, 127].includes(ip[0]) ||
      ip[0] >= 224 ||
      (ip[0] === 169 && ip[1] === 254) ||
      (ip[0] === 172 && ip[1] >= 16 && ip[1] <= 31) ||
      (ip[0] === 192 && (ip[1] === 168 || (ip[1] === 0 && [0, 2].includes(ip[2])))) ||
      (ip[0] === 100 && ip[1] >= 64 && ip[1] <= 127) ||
      (ip[0] === 198 && [18, 19].includes(ip[1])) ||
      (ip[0] === 198 && ip[1] === 51 && ip[2] === 100) ||
      (ip[0] === 203 && ip[1] === 0 && ip[2] === 113)
    )
      throw new Error("Media references must use public hosts");
  } else if (/^(?:[0-9]+|0x[0-9a-f]+)$/i.test(labels[labels.length - 1])) throw new Error("Media reference IP address is invalid");
  return value;
}

function aliasedValue(request, names) {
  let value;
  for (const name of names) {
    if (!Object.hasOwn(request, name)) continue;
    const candidate = request[name];
    if (value !== undefined && JSON.stringify(value) !== JSON.stringify(candidate)) throw new Error(names.join(" and ") + " must match");
    if (candidate === undefined || candidate === null) throw new Error(name + " must not be null");
    value = candidate;
  }
  return value;
}

function videoSize(value, config) {
  if (value === undefined) return {};
  if (typeof value !== "string") throw new Error("size must be a resolution or pixel dimensions");
  // Custom specifications are opaque upstream values; do not infer dimensions
  // or remap their resolution using a known model's pixel table.
  if (config.custom) return { resolution: value };
  const size = value.toLowerCase();
  if (RESOLUTION_SCHEMA.enum.includes(size)) return { resolution: size };
  if (config.count && !["1280x720", "720x1280", "720x720"].includes(size)) throw new Error("Unsupported Yuxibox count video size");
  const dimensions = /^([1-9][0-9]{0,4})x([1-9][0-9]{0,4})$/.exec(size);
  if (!dimensions) throw new Error("size must be a resolution or pixel dimensions");
  const width = Number(dimensions[1]),
    height = Number(dimensions[2]);
  const resolution = { 480: "480p", 720: "720p", 768: "768p", 1080: "1080p", 2160: "4k" }[Math.min(width, height)];
  if (!resolution) throw new Error("Unsupported Yuxibox video dimensions");
  let a = width,
    b = height;
  while (b) {
    const remainder = a % b;
    a = b;
    b = remainder;
  }
  let ratio = width / a + ":" + height / a;
  if (["854x480", "1366x768"].includes(size)) ratio = "16:9";
  if (["480x854", "768x1366"].includes(size)) ratio = "9:16";
  return { resolution, ratio };
}

function modelConfig(model) {
  // oxlint-disable no-control-regex -- Match the host's model identity rules
  if (typeof model !== "string" || !model || model !== model.trim() || /[\x00-\x1f\x7f-\x9f,*]/.test(model)) throw new Error("Invalid Yuxibox video model");
  // oxlint-enable no-control-regex
  let modelBytes = 0;
  for (const character of model) {
    const codePoint = character.codePointAt(0);
    modelBytes += codePoint <= 0x7f ? 1 : codePoint <= 0x7ff ? 2 : codePoint <= 0xffff ? 3 : 4;
    if (modelBytes > 255) throw new Error("Yuxibox video model must not exceed 255 UTF-8 bytes");
  }
  if (Object.hasOwn(MODELS, model)) return MODELS[model];
  // Saved channel capabilities provide model limits within this host envelope.
  return { custom: true, min: 1, max: 3600, resolutions: RESOLUTION_SCHEMA.enum, images: 64, videos: 64, audios: 64, total: 64, frames: true };
}

function normalizeRequest(request, model) {
  const config = modelConfig(model);
  if (!request || typeof request !== "object" || Array.isArray(request)) throw new Error("JSON object required");
  for (const name of Object.keys(request)) if (!REQUEST_FIELDS.has(name)) throw new Error(name + " is not supported by Yuxibox video");
  if (request.mode !== undefined && (!config.custom || !["text_to_video", "reference_to_video", "image_to_video", "first_last_frame"].includes(request.mode)))
    throw new Error("Unsupported Yuxibox video mode");
  if (typeof request.prompt !== "string" || !request.prompt.trim()) throw new Error("prompt is required");
  if (Array.from(request.prompt).length > (config.h3 ? 20000 : 16000)) throw new Error("prompt exceeds the Yuxibox video length limit");
  let seconds;
  for (const name of ["seconds", "duration"]) {
    if (!Object.hasOwn(request, name)) continue;
    const raw = request[name];
    if (!["number", "string"].includes(typeof raw) || (typeof raw === "string" && !/^[0-9]+$/.test(raw))) throw new Error("seconds must be an integer");
    const value = Number(raw);
    if (!Number.isInteger(value) || value < config.min || value > config.max) throw new Error("seconds must be between " + config.min + " and " + config.max);
    if (seconds !== undefined && seconds !== value) throw new Error("seconds and duration must match");
    seconds = value;
  }
  if (seconds === undefined) {
    if (config.custom) throw new Error("seconds is required for a channel-configured Yuxibox model");
    seconds = config.defaultSeconds;
  }
  const size = videoSize(request.size, config);
  let resolution =
    request.resolution === undefined
      ? size.resolution
      : typeof request.resolution === "string"
        ? config.custom
          ? request.resolution
          : request.resolution.toLowerCase()
        : "";
  if (config.h3 && resolution === "720p") resolution = "768p";
  const sizeResolution = config.h3 && size.resolution === "720p" ? "768p" : size.resolution;
  if (request.resolution !== undefined && sizeResolution && resolution !== sizeResolution) throw new Error("resolution and size must match");
  if (resolution === undefined) {
    if (config.custom) throw new Error("resolution is required for a channel-configured Yuxibox model");
    resolution = config.h3 ? "768p" : "720p";
  }
  if (config.custom ? typeof resolution !== "string" || !/^[A-Za-z0-9][A-Za-z0-9_.-]{0,31}$/.test(resolution) : !config.resolutions.includes(resolution))
    throw new Error("Unsupported Yuxibox video resolution");
  const rawRatio = aliasedValue(request, ["aspect_ratio", "ratio", "aspectRatio"]);
  if (config.custom && rawRatio === undefined && !size.ratio) throw new Error("aspect_ratio is required for a channel-configured Yuxibox model");
  const ratio = rawRatio === undefined ? size.ratio || (config.h3 ? "9:16" : "16:9") : rawRatio;
  if (size.ratio && ratio !== size.ratio) throw new Error("size and aspect_ratio must match");
  const ratios = config.h3
    ? config.portrait
      ? ["16:9", "9:16"]
      : ["16:9", "9:16", "1:1"]
    : config.seconds
      ? G_RATIOS.filter((value) => value !== "adaptive")
      : G_RATIOS;
  if (
    config.custom
      ? typeof ratio !== "string" || (!/^[1-9][0-9]{0,2}:[1-9][0-9]{0,2}$/.test(ratio) && !["auto", "adaptive"].includes(ratio))
      : !ratios.includes(ratio)
  )
    throw new Error("Unsupported Yuxibox video aspect ratio");
  const body = { model, prompt: request.prompt.trim(), seconds, resolution, aspect_ratio: ratio };
  for (const [target, names, limit] of [
    ["reference_images", ["reference_images", "images", "image_urls"], config.images],
    ["reference_videos", ["reference_videos", "videos", "video_urls"], config.videos],
    ["reference_audios", ["reference_audios", "audios", "audio_urls"], config.audios],
  ]) {
    let values;
    for (const name of names) {
      if (!Object.hasOwn(request, name)) continue;
      // Single-item workbench controls emit a URL string. Normalize before
      // comparing aliases so equivalent string and array inputs stay equal.
      const items = typeof request[name] === "string" ? [request[name]] : request[name];
      if (!Array.isArray(items) || items.length > limit) throw new Error(target + " exceeds the Yuxibox media limit");
      const references = items.map(mediaURL);
      if (values !== undefined && JSON.stringify(values) !== JSON.stringify(references)) throw new Error(names.join(" and ") + " must match");
      values = references;
    }
    if (values !== undefined) body[target] = values;
  }
  const image = aliasedValue(request, ["image", "input_reference", "first_frame", "start_frame"]);
  const lastFrame = aliasedValue(request, ["last_frame", "end_frame"]);
  let pinnedFirst = request.first_frame !== undefined || request.start_frame !== undefined;
  if (!config.frames && (request.first_frame !== undefined || request.start_frame !== undefined || lastFrame !== undefined))
    throw new Error("This Yuxibox model does not support first and last frames");
  if (image !== undefined) {
    if (config.frames) body.image = mediaURL(image);
    else body.reference_images = [mediaURL(image), ...(body.reference_images || [])];
  }
  if (lastFrame !== undefined) body.last_frame = mediaURL(lastFrame);
  if (request.references !== undefined) {
    if (!Array.isArray(request.references) || request.references.length > (config.total || config.images + config.videos + config.audios))
      throw new Error("references exceeds the Yuxibox media limit");
    for (const reference of request.references) {
      if (!reference || typeof reference !== "object" || Array.isArray(reference)) throw new Error("Invalid Yuxibox media reference");
      for (const name of Object.keys(reference))
        if (!["type", "role", "source"].includes(name)) throw new Error("Unsupported Yuxibox reference field: " + name);
      const kind = reference.type,
        role = reference.role;
      if (!["image", "video", "audio"].includes(kind)) throw new Error("Unsupported Yuxibox reference media kind");
      const source = mediaURL(reference.source);
      if (kind === "image" && ["first_frame", "last_frame"].includes(role)) {
        if (!config.frames) throw new Error("This Yuxibox model does not support first and last frames");
        const field = role === "first_frame" ? "image" : "last_frame";
        if (role === "first_frame") pinnedFirst = true;
        if (body[field] !== undefined && body[field] !== source) throw new Error("Conflicting Yuxibox frame references");
        body[field] = source;
      } else {
        if (role !== undefined && role !== "" && !["reference", "reference_" + kind].includes(role)) throw new Error("Unsupported Yuxibox reference role");
        const field = "reference_" + kind + "s";
        if (!body[field]) body[field] = [];
        body[field].push(source);
      }
    }
  }
  if (
    config.custom &&
    body.image &&
    !body.last_frame &&
    !pinnedFirst &&
    ((body.reference_images || []).length || (body.reference_videos || []).length || (body.reference_audios || []).length)
  ) {
    body.reference_images = [body.image, ...(body.reference_images || [])];
    delete body.image;
  }
  const imageCount = (body.reference_images || []).length,
    videoCount = (body.reference_videos || []).length,
    audioCount = (body.reference_audios || []).length;
  const total = imageCount + videoCount + audioCount;
  if (imageCount > config.images || videoCount > config.videos || audioCount > config.audios || (config.total && total > config.total))
    throw new Error("References exceed the Yuxibox media limits");
  if (config.requiresImage && !imageCount) throw new Error("This Yuxibox H3 model requires at least one reference image");
  if (Boolean(body.image) !== Boolean(body.last_frame) && !(config.custom && body.image && !pinnedFirst))
    throw new Error("Yuxibox first and last frames must be provided together");
  if (body.image && total) throw new Error("First and last frames cannot be combined with ordinary references");
  if (config.h3) {
    if (["generate_audio", "enable_audio", "watermark", "prompt_extend"].some((name) => Object.hasOwn(request, name)))
      throw new Error("Audio and watermark options are not supported by Yuxibox H3");
    if (Object.hasOwn(request, "seed")) {
      if (!Number.isSafeInteger(request.seed)) throw new Error("seed must be a safe integer");
      body.seed = request.seed;
    }
  } else {
    if (Object.hasOwn(request, "seed")) throw new Error("seed is not supported by Yuxibox Seedance");
    const generateAudio = aliasedValue(request, ["generate_audio", "enable_audio"]);
    if (generateAudio !== undefined) {
      if (generateAudio !== true) throw new Error("Yuxibox Seedance only supports generate_audio=true");
    }
    // Audio is always enabled upstream, not a toggle in the workbench.
    // Accept an explicit true for compatibility, then use the provider default.
    for (const name of ["watermark", "prompt_extend"]) {
      if (!Object.hasOwn(request, name)) continue;
      if (request[name] !== false) throw new Error("Yuxibox Seedance only supports " + name + "=false");
      body[name] = false;
    }
  }
  for (const name of ["reference_images", "reference_videos", "reference_audios"]) if (body[name] && !body[name].length) delete body[name];
  const action = body.last_frame ? "first_last_frame" : body.image ? "image_to_video" : total ? "reference_to_video" : "text_to_video";
  if (request.mode !== undefined && request.mode !== action) throw new Error("mode conflicts with the supplied media");
  return { body, action };
}

function taskIdentity(value) {
  // oxlint-disable no-control-regex -- Task identifiers become URL path components
  if (typeof value !== "string" || !value || value === "." || value === ".." || value.length > 191 || /[\s\\/\x00-\x1f\x7f]/.test(value))
    throw new Error("Yuxibox video task id is missing or invalid");
  // oxlint-enable no-control-regex
  return value;
}

function videoAPIError(value) {
  if (value === undefined || value === null || value === false || value === 0 || value === "") return "";
  if (typeof value === "string") return value.trim();
  if (typeof value !== "object" || Array.isArray(value)) return "Yuxibox returned an invalid video error";
  if (typeof value.message === "string" && value.message.trim()) return value.message.trim();
  if (Object.hasOwn(value, "code") && value.code !== 0 && value.code !== "0" && value.code !== "" && value.code !== null)
    return "Yuxibox video error: " + String(value.code);
  return Object.keys(value).some((name) => name !== "code" && value[name] !== null && value[name] !== "" && value[name] !== false && value[name] !== 0)
    ? "Yuxibox returned an invalid video error"
    : "";
}

export function buildSubmitRequest(ctx) {
  const prepared = normalizeRequest(ctx.requestBody, ctx.upstreamModel || ctx.model);
  const request = prepared.body;
  const body = { model: request.model, prompt: request.prompt, seconds: request.seconds, ratio: request.aspect_ratio };
  if (!modelConfig(request.model).count) body.resolution = request.resolution;
  if (request.image) {
    if (prepared.action === "image_to_video") body.reference_images = [request.image];
    else body.first_frame = request.image;
  }
  if (request.last_frame) body.last_frame = request.last_frame;
  for (const [source, target] of [
    ["reference_images", "reference_images"],
    ["reference_videos", "videos"],
    ["reference_audios", "audios"],
    ["watermark", "watermark"],
    ["prompt_extend", "prompt_extend"],
    ["seed", "seed"],
  ])
    if (Object.hasOwn(request, source)) body[target] = request[source];
  return {
    method: "POST",
    url: ctx.baseUrl.replace(/\/+$/, "") + "/v1/videos",
    headers: { "Content-Type": "application/json", Authorization: "Bearer " + ctx.apiKey },
    body,
  };
}

export function extractUsage(ctx) {
  if (ctx.usagePurpose === "billing_ratios") return null;
  const model = ctx.upstreamModel || ctx.model;
  const request = normalizeRequest(ctx.requestBody, model).body;
  const config = modelConfig(model);
  if (config.custom) return { seconds: request.seconds, requests: 1, resolution: request.resolution.toLowerCase() };
  if (config.count) return { video_count: 1 };
  if (!config.h3) return config.seconds ? { seconds: request.seconds, resolution: request.resolution } : { video_count: 1, resolution: request.resolution };
  // These facts are frozen in the host's admission billing snapshot. Completion
  // must not re-evaluate the Shanghai time band or replace requested seconds.
  const hour = (Math.floor(utils.unixNow() / 3600) + 8) % 24;
  return { seconds: request.seconds, resolution: request.resolution, billing_period: hour < 8 ? "off_peak" : "peak" };
}

export function parseSubmitResponse(_ctx, response) {
  const body = response.body;
  if (!body || typeof body !== "object" || Array.isArray(body)) throw new Error("Yuxibox returned an invalid video response");
  const id = taskIdentity(body.id || body.task_id);
  if (body.id !== undefined && body.task_id !== undefined && body.id !== body.task_id) throw new Error("Yuxibox returned conflicting video task ids");
  // An accepted identifier survives error statuses and uncertain receipts.
  // Polling confirms the terminal state before the host settles or refunds.
  return { taskId: id, taskData: body };
}

export function buildQueryRequest(ctx) {
  return {
    method: "GET",
    url: ctx.baseUrl.replace(/\/+$/, "") + "/v1/videos/" + encodeURIComponent(taskIdentity(ctx.taskId)),
    headers: { Authorization: "Bearer " + ctx.apiKey },
  };
}

export function parseTaskResult(ctx, body) {
  if (!body || typeof body !== "object" || Array.isArray(body)) return { status: "UNKNOWN", reason: "Yuxibox returned an invalid video response" };
  if (body.id === undefined && body.task_id === undefined) return { status: "UNKNOWN", reason: "Yuxibox video task id is missing" };
  for (const id of [body.id, body.task_id])
    if (id !== undefined && taskIdentity(id) !== ctx.taskId) return { status: "UNKNOWN", reason: "Yuxibox returned a different video task id" };
  const statuses = { queued: "QUEUED", in_progress: "IN_PROGRESS", completed: "SUCCESS", failed: "FAILURE" };
  if (typeof body.status !== "string" || !Object.hasOwn(statuses, body.status)) return { status: "UNKNOWN", reason: "Unrecognized Yuxibox video status" };
  if (
    (body.stage !== undefined && typeof body.stage !== "string") ||
    (["submission_pending", "artifact_importing"].includes(body.stage) && ["completed", "failed"].includes(body.status)) ||
    (body.stage === "artifact_importing" && body.status === "queued") ||
    (body.stage === "completed" && body.status !== "completed") ||
    (body.stage === "failed" && body.status !== "failed")
  )
    return { status: "UNKNOWN", reason: "Yuxibox returned conflicting video status and stage" };
  const error = videoAPIError(body.error);
  if (error && body.status !== "failed") return { status: "UNKNOWN", reason: "Yuxibox returned a non-failed task with an error" };
  if (body.status === "failed") {
    const outputURLs = [body.url, body.video_url, body.download_url, body.result_url, body.volcesVideoUrl, body.metadata && body.metadata.url];
    if (body.data && typeof body.data === "object") {
      outputURLs.push(body.data.download_url, body.data.result_url, body.data.volcesVideoUrl);
      if (Array.isArray(body.data.creations))
        for (const creation of body.data.creations) if (creation && typeof creation === "object") outputURLs.push(creation.url, creation.volcesVideoUrl);
    }
    if (outputURLs.some((value) => typeof value === "string" && /^(?:https?:\/\/|data:video\/)/i.test(value)))
      return { status: "UNKNOWN", reason: "Yuxibox returned a failed task with a video result" };
  }
  const result = { status: statuses[body.status] };
  if (result.status === "FAILURE") result.reason = error || "Yuxibox video generation failed";
  if (typeof body.progress === "number" && Number.isFinite(body.progress) && body.progress >= 0 && body.progress <= 100)
    result.progress = (result.status === "IN_PROGRESS" ? Math.min(99, body.progress) : body.progress) + "%";
  return result;
}

export function extractUsageOnComplete() {
  return {};
}

export function listArtifacts(task) {
  return task.status === "SUCCESS" ? [{ key: "video", type: "video", mimeType: "video/mp4" }] : [];
}

export function buildContentRequest(ctx) {
  if (ctx.artifactKey !== "video") throw new Error("artifact_not_found");
  return {
    method: ctx.clientRequest.method,
    url: ctx.baseUrl.replace(/\/+$/, "") + "/v1/videos/" + encodeURIComponent(taskIdentity(ctx.upstreamTaskId)) + "/content",
    headers: { Authorization: "Bearer " + ctx.apiKey },
  };
}

export const protocols = {
  openai_video: {
    decodeRequest(ctx) {
      if (!ctx.body || ctx.body.kind !== "json") throw new Error("Yuxibox video requires JSON with public HTTPS media URLs");
      if (ctx.operation && ctx.operation !== "create") throw new Error("Yuxibox video supports generation only");
      if (!ctx.upstreamModel) {
        if (!ctx.body.value || typeof ctx.body.value !== "object" || Array.isArray(ctx.body.value)) throw new Error("JSON object required");
        return { kind: "submit", model: ctx.model, requestBody: ctx.body.value };
      }
      const prepared = normalizeRequest(ctx.body.value, ctx.upstreamModel || ctx.model);
      prepared.body.model = ctx.model;
      return { kind: "submit", model: ctx.model, action: prepared.action, requestBody: prepared.body };
    },
    render(_ctx, task) {
      return task.fail_reason ? { error: { message: task.fail_reason } } : {};
    },
  },
};
