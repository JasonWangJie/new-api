const RATIOS = ["16:9", "9:16", "1:1", "4:3", "3:4"];
// Capability snapshot from Wolong's 2026-11-03 API documentation. Prices stay
// administrator-owned; never substitute upstream credit amounts for local usage.
const MODELS = {
  "haya-mo-seedance-25": { min: 5, max: 30, images: 30, videos: 10, audios: 10, total: 50, ratios: [...RATIOS, "21:9"] },
  "mili-bd-seedance-25": { min: 4, max: 30, images: 30, videos: 10, audios: 10, total: 30, ratios: ["auto", ...RATIOS, "21:9"] },
  "ljvideo-seedance-25-deal-y": { min: 5, max: 30, images: 9, videos: 0, audios: 3, resolutions: ["1080p"], ratios: ["16:9", "9:16"] },
  "meaicc-sd-2-c5": { min: 4, max: 15, images: 9, videos: 3, audios: 3, ratios: ["1:1", "16:9", "9:16"] },
  "meaicc-sd-2-c7": { min: 4, max: 15, images: 9, videos: 3, audios: 3, ratios: ["1:1", "16:9", "9:16"] },
  "aigc-seedance-20-manju": { durations: [5, 10, 15], images: 9, videos: 0, audios: 0, frames: true },
  "czniu-seedance-20": { durations: [5, 15], images: 9, videos: 3, audios: 3 },
  "czniu-seedance-20-fast": { min: 4, max: 15, images: 9, videos: 3, audios: 3, requiresVisual: true },
  "czniu-seedance-20-mini": { min: 4, max: 15, images: 9, videos: 3, audios: 3, requiresVisual: true },
  "czniu-seedance-20-stable": { min: 4, max: 15, images: 9, videos: 0, audios: 3, resolutions: ["720p", "1080p"], perSecond: true },
};
const MEDIA_FIELDS = [
  ["reference_images", ["reference_images", "images", "image_urls"], "image", "images"],
  ["reference_videos", ["reference_videos", "videos", "video_urls"], "video", "videos"],
  ["reference_audios", ["reference_audios", "audios", "audio_urls"], "audio", "audios"],
];
const FIRST_FIELDS = ["image", "input_reference", "first_frame", "start_frame"];
const LAST_FIELDS = ["last_frame", "end_frame"];
const FIELDS = new Set([
  "model",
  "prompt",
  "seconds",
  "duration",
  "resolution",
  "quality",
  "size",
  "aspect_ratio",
  "ratio",
  "references",
  "mode",
  ...FIRST_FIELDS,
  ...LAST_FIELDS,
  ...MEDIA_FIELDS.flatMap((entry) => entry[1]),
]);
const COUNT_SCHEMA = {
  type: "number",
  unit: "count",
  unitLabel: { en: "video", zh: "条" },
  description: { en: "Video generation unit price", zh: "视频生成单价" },
};
const SECOND_SCHEMA = { type: "number", unit: "second", description: { en: "Video generation unit price", zh: "视频生成单价" } };

export const meta = {
  apiVersion: 1,
  key: "wolong",
  name: "Wolong Video",
  icon: "text:卧龙",
  version: "1.0.0",
  author: { name: "new-api contributors" },
  baseUrl: "https://wolongvideo.com",
  website: "https://wolongvideo.com/docs.html",
  description: { en: "Wolong video generation with built-in and channel-configured models", zh: "卧龙内置与渠道自定义模型视频生成" },
  models: Object.keys(MODELS),
  fetchMode: "per_task",
  protocols: ["openai_video"],
  usageSchema: { video_count: COUNT_SCHEMA },
  usageProfiles: Object.entries(MODELS).map(([model, config]) => ({
    models: [model],
    schema: {
      ...(config.perSecond ? { seconds: SECOND_SCHEMA } : { video_count: COUNT_SCHEMA }),
      resolution: { enum: config.resolutions || ["720p"], description: { en: "Output video resolution", zh: "输出视频分辨率" } },
    },
  })),
  videoRetryInputs: [
    ...FIRST_FIELDS.map((field) => ({ path: "/" + field, kind: "image" })),
    ...LAST_FIELDS.map((field) => ({ path: "/" + field, kind: "image" })),
    ...MEDIA_FIELDS.flatMap(([, fields, kind]) =>
      fields.flatMap((field) => [
        { path: "/" + field, kind },
        { path: "/" + field + "/*", kind },
      ])
    ),
    { path: "/references/*/source", kind: "media" },
  ],
  videoProfiles: Object.entries(MODELS).map(([model, config]) => {
    const settings = {
      duration: config.durations ? { values: config.durations, default: 5 } : { min: config.min, max: config.max, step: 1, default: 5 },
      resolutions: config.resolutions || ["720p"],
      defaultResolution: (config.resolutions || ["720p"])[0],
      aspectRatios: config.ratios || RATIOS,
      defaultAspectRatio: "16:9",
    };
    const inputs = MEDIA_FIELDS.filter(([, , , limit]) => config[limit] > 0).map(([name, , kind, limit]) => ({
      name,
      kind,
      sources: ["url"],
      maxItems: config[limit],
    }));
    const modes = [{ ...settings, name: "reference_to_video", inputs }];
    if (!config.requiresVisual) modes.unshift({ ...settings, name: "text_to_video" });
    if (config.frames) {
      modes.push({ ...settings, name: "image_to_video", inputs: [{ name: "image", kind: "image", sources: ["url"], maxItems: 1, required: true }] });
      modes.push({
        ...settings,
        name: "first_last_frame",
        inputs: [
          { name: "image", kind: "image", sources: ["url"], maxItems: 1, required: true },
          { name: "last_frame", kind: "image", sources: ["url"], maxItems: 1, required: true },
        ],
      });
    }
    return { models: [model], modes };
  }),
};

function mediaURL(value) {
  // Preserve signed URLs exactly while rejecting credentials and parser ambiguities.
  // oxlint-disable no-control-regex -- Media URLs must not contain controls
  if (typeof value !== "string" || !value || value.length > 8192 || /[\s\\\x00-\x1f\x7f]/.test(value) || value.includes("#"))
    throw new Error("Wolong media references must be public HTTP or HTTPS URLs");
  // oxlint-enable no-control-regex
  const match = /^https?:\/\/([a-z0-9](?:[a-z0-9.-]*[a-z0-9])?)(?::([0-9]{1,5}))?(?:[/?].*)?$/i.exec(value);
  if (!match || (match[2] && (Number(match[2]) < 1 || Number(match[2]) > 65535))) throw new Error("Invalid Wolong media URL");
  const host = match[1].toLowerCase();
  const labels = host.split(".");
  if (
    host.length > 253 ||
    labels.length < 2 ||
    [".local", ".localhost", ".internal", ".localdomain", ".lan"].some((suffix) => host.endsWith(suffix)) ||
    labels.some((label) => !label || label.length > 63 || label.startsWith("-") || label.endsWith("-"))
  )
    throw new Error("Wolong media references must use public hosts");
  if (/^[0-9.]+$/.test(host)) {
    const ip = labels.map(Number);
    if (
      ip.length !== 4 ||
      labels.some((label, index) => String(ip[index]) !== label || ip[index] > 255) ||
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
      throw new Error("Wolong media references must use public hosts");
  } else if (/^(?:[0-9]+|0x[0-9a-f]+)$/i.test(labels[labels.length - 1])) throw new Error("Invalid Wolong media host");
  return value;
}

function alias(request, names) {
  let value;
  for (const name of names) {
    if (!Object.hasOwn(request, name)) continue;
    const candidate = request[name];
    if (candidate === null || candidate === undefined) throw new Error(name + " must not be null");
    if (value !== undefined && value !== candidate) throw new Error(names.join(" and ") + " must match");
    value = candidate;
  }
  return value;
}

function normalizeRequest(request, model) {
  // oxlint-disable no-control-regex -- Match the host's model identity rules
  if (typeof model !== "string" || !model || model !== model.trim() || /[\x00-\x1f\x7f-\x9f,*]/.test(model)) throw new Error("Invalid Wolong video model");
  // oxlint-enable no-control-regex
  let modelBytes = 0;
  for (const character of model) {
    const codePoint = character.codePointAt(0);
    modelBytes += codePoint <= 0x7f ? 1 : codePoint <= 0x7ff ? 2 : codePoint <= 0xffff ? 3 : 4;
    if (modelBytes > 255) throw new Error("Wolong video model must not exceed 255 UTF-8 bytes");
  }
  const builtIn = Object.hasOwn(MODELS, model);
  // Unknown models require saved channel capabilities. This envelope only
  // bounds protocol values; host admission validates the selected model profile.
  const config = builtIn ? MODELS[model] : { min: 1, max: 3600, images: 64, videos: 64, audios: 64, total: 64, frames: true };
  if (!request || typeof request !== "object" || Array.isArray(request)) throw new Error("JSON object required");
  for (const name of Object.keys(request)) if (!FIELDS.has(name)) throw new Error(name + " is not supported by Wolong video");
  if (request.mode !== undefined && !["text_to_video", "image_to_video", "reference_to_video", "first_last_frame"].includes(request.mode))
    throw new Error("Unsupported Wolong video mode");
  if (request.prompt !== undefined && typeof request.prompt !== "string") throw new Error("prompt must be a string");
  const prompt = (request.prompt || "").trim();
  if (Array.from(prompt).length > 10000) throw new Error("Wolong prompt must not exceed 10000 characters");
  let seconds;
  for (const name of ["seconds", "duration"]) {
    if (!Object.hasOwn(request, name)) continue;
    const raw = request[name];
    if (!["number", "string"].includes(typeof raw) || (typeof raw === "string" && !/^[0-9]+$/.test(raw)))
      throw new Error("Wolong duration must be a whole number");
    const value = Number(raw);
    if (!Number.isSafeInteger(value) || (config.durations ? !config.durations.includes(value) : value < config.min || value > config.max))
      throw new Error("Unsupported Wolong video duration");
    if (seconds !== undefined && seconds !== value) throw new Error("seconds and duration must match");
    seconds = value;
  }
  if (seconds === undefined) {
    if (!builtIn) throw new Error("seconds is required for a channel-configured Wolong model");
    seconds = 5;
  }
  let sizeResolution, sizeRatio;
  if (request.size !== undefined) {
    if (typeof request.size !== "string") throw new Error("size must be a string");
    const size = request.size.toLowerCase();
    if (["720p", "1080p"].includes(size)) sizeResolution = size;
    else if (builtIn ? (config.ratios || RATIOS).includes(size) : /^(?:[1-9][0-9]{0,2}:[1-9][0-9]{0,2}|auto)$/.test(size)) sizeRatio = size;
    else if (!builtIn && /^[a-z0-9][a-z0-9_.-]{0,31}$/.test(size) && !size.includes("x") && size !== "adaptive") sizeResolution = size;
    else {
      const dimensions = {
        "1280x720": ["720p", "16:9"],
        "720x1280": ["720p", "9:16"],
        "720x720": ["720p", "1:1"],
        "1920x1080": ["1080p", "16:9"],
        "1080x1920": ["1080p", "9:16"],
        "1080x1080": ["1080p", "1:1"],
      };
      if (!Object.hasOwn(dimensions, size)) throw new Error("Unsupported Wolong video size");
      [sizeResolution, sizeRatio] = dimensions[size];
    }
  }
  const explicitResolution = alias(request, ["resolution", "quality"]);
  const resolution = explicitResolution === undefined ? sizeResolution || (builtIn ? (config.resolutions || ["720p"])[0] : undefined) : explicitResolution;
  if (sizeResolution && resolution !== sizeResolution) throw new Error("resolution and size must match");
  if (
    builtIn ? !(config.resolutions || ["720p"]).includes(resolution) : typeof resolution !== "string" || !/^[A-Za-z0-9][A-Za-z0-9_.-]{0,31}$/.test(resolution)
  )
    throw new Error("A supported Wolong video resolution is required");
  const explicitRatio = alias(request, ["aspect_ratio", "ratio"]);
  const ratio = explicitRatio === undefined ? sizeRatio || (builtIn ? "16:9" : undefined) : explicitRatio;
  if (sizeRatio && ratio !== sizeRatio) throw new Error("aspect_ratio and size must match");
  if (builtIn ? !(config.ratios || RATIOS).includes(ratio) : typeof ratio !== "string" || !/^(?:[1-9][0-9]{0,2}:[1-9][0-9]{0,2}|auto)$/.test(ratio))
    throw new Error("A supported Wolong video aspect ratio is required");
  const body = { model, prompt, seconds, resolution: resolution.toLowerCase(), aspect_ratio: ratio };
  for (const [target, names, , limit] of MEDIA_FIELDS) {
    let values;
    for (const name of names) {
      if (!Object.hasOwn(request, name)) continue;
      const items = typeof request[name] === "string" ? [request[name]] : request[name];
      if (!Array.isArray(items) || items.length > config[limit]) throw new Error("Too many Wolong " + target);
      const urls = items.map(mediaURL);
      if (values !== undefined && JSON.stringify(values) !== JSON.stringify(urls)) throw new Error(names.join(" and ") + " must match");
      values = urls;
    }
    body[target] = values || [];
  }
  const first = alias(request, FIRST_FIELDS),
    last = alias(request, LAST_FIELDS);
  if (first !== undefined) body.image = mediaURL(first);
  if (last !== undefined) body.last_frame = mediaURL(last);
  if (request.references !== undefined) {
    if (!Array.isArray(request.references) || request.references.length > (config.total || config.images + config.videos + config.audios))
      throw new Error("Too many Wolong media references");
    for (const reference of request.references) {
      if (
        !reference ||
        typeof reference !== "object" ||
        Array.isArray(reference) ||
        Object.keys(reference).some((name) => !["type", "role", "source"].includes(name))
      )
        throw new Error("Invalid Wolong media reference");
      const kind = reference.type,
        role = reference.role,
        source = mediaURL(reference.source);
      if (!["image", "video", "audio"].includes(kind)) throw new Error("Unsupported Wolong media kind");
      if (kind === "image" && ["first_frame", "last_frame"].includes(role)) {
        const field = role === "first_frame" ? "image" : "last_frame";
        if (body[field] !== undefined && body[field] !== source) throw new Error("Conflicting Wolong frame references");
        body[field] = source;
      } else {
        if (role !== undefined && role !== "" && !["reference", "reference_" + kind].includes(role)) throw new Error("Unsupported Wolong media role");
        body["reference_" + kind + "s"].push(source);
      }
    }
  }
  if (
    !config.frames &&
    (last !== undefined ||
      body.last_frame ||
      request.first_frame !== undefined ||
      request.start_frame !== undefined ||
      (request.references || []).some((reference) => ["first_frame", "last_frame"].includes(reference.role)))
  )
    throw new Error("This Wolong model does not support explicit frame roles");
  if (body.last_frame && !body.image) throw new Error("Wolong last frame requires a first frame");
  const primaryImage = Boolean(body.image),
    lastFrame = Boolean(body.last_frame);
  if (body.image) {
    if (body.reference_images.length && (builtIn ? config.frames : lastFrame || ["image_to_video", "first_last_frame"].includes(request.mode)))
      throw new Error("Wolong explicit frames cannot be combined with image references");
    body.reference_images = [body.image, ...(body.last_frame ? [body.last_frame] : []), ...body.reference_images];
    delete body.image;
    delete body.last_frame;
  }
  const images = body.reference_images.length,
    videos = body.reference_videos.length,
    audios = body.reference_audios.length;
  const total = images + videos + audios;
  if (images > config.images || videos > config.videos || audios > config.audios || (config.total && total > config.total))
    throw new Error("Wolong references exceed the model limits");
  if (config.requiresVisual && images + videos === 0) throw new Error("This Wolong model requires a reference image or video");
  if (model === "mili-bd-seedance-25" && (images > 9 || videos > 0 || audios > 0) && seconds < 5)
    throw new Error("Wolong Mili requires at least 5 seconds with these references");
  if (!total && !prompt) throw new Error("prompt is required without reference media");
  let action = total ? "reference_to_video" : "text_to_video";
  if (builtIn && config.frames && (images === 1 || images === 2)) {
    action = images === 1 ? "image_to_video" : "first_last_frame";
  } else if (!builtIn) {
    if (lastFrame) action = "first_last_frame";
    else if (primaryImage && images === 1 && videos + audios === 0) action = "image_to_video";
    if (request.mode !== undefined) action = request.mode;
    if (
      (action === "text_to_video" && total !== 0) ||
      (action === "reference_to_video" && (!total || lastFrame)) ||
      (action === "image_to_video" && (images !== 1 || videos + audios !== 0 || lastFrame)) ||
      (action === "first_last_frame" && (images !== 2 || videos + audios !== 0))
    )
      throw new Error("Wolong mode conflicts with its reference media");
  }
  if (builtIn && request.mode !== undefined && request.mode !== action) throw new Error("Wolong mode conflicts with this model's reference behavior");
  if (action === "image_to_video" || action === "first_last_frame") {
    body.image = body.reference_images[0];
    if (action === "first_last_frame") body.last_frame = body.reference_images[1];
    delete body.reference_images;
  }
  for (const [field] of MEDIA_FIELDS) if (body[field] && !body[field].length) delete body[field];
  return { body, action };
}

function taskIdentity(value) {
  if (typeof value !== "string" || !/^[A-Za-z0-9][A-Za-z0-9_.:-]{0,190}$/.test(value) || value === "." || value === "..")
    throw new Error("Invalid Wolong video task id");
  return value;
}

function videoResult(task, baseUrl) {
  if (!Array.isArray(task.data)) throw new Error("Wolong video output is missing");
  const item = task.data.find((file) => file && typeof file.mime === "string" && file.mime.startsWith("video/"));
  if (!item || typeof item.url !== "string" || !item.url) throw new Error("Wolong video output is missing");
  let url = item.url;
  if (url.startsWith("/") && !url.startsWith("//")) {
    const origin = /^https?:\/\/[^/?#]+/i.exec(baseUrl || "");
    if (!origin) throw new Error("Invalid Wolong base URL");
    url = origin[0] + url;
  }
  return mediaURL(url);
}

export function buildSubmitRequest(ctx) {
  const request = normalizeRequest(ctx.requestBody, ctx.upstreamModel || ctx.model).body;
  if (typeof ctx.publicTaskId !== "string" || !/^[A-Za-z0-9._:-]{8,128}$/.test(ctx.publicTaskId))
    throw new Error("Wolong requires a valid public task id for idempotency");
  const body = { model: request.model, prompt: request.prompt, duration: request.seconds, quality: request.resolution, ratio: request.aspect_ratio };
  for (const [source, target] of [
    ["reference_images", "image_urls"],
    ["reference_videos", "video_urls"],
    ["reference_audios", "audio_urls"],
  ])
    if (request[source]) body[target] = request[source];
  if (request.image) body.image_urls = [request.image, ...(request.last_frame ? [request.last_frame] : [])];
  return {
    method: "POST",
    url: ctx.baseUrl.replace(/\/+$/, "") + "/v1/videos/generations",
    headers: { "Content-Type": "application/json", Authorization: "Bearer " + ctx.apiKey, "Idempotency-Key": ctx.publicTaskId },
    body,
  };
}

export function extractUsage(ctx) {
  if (ctx.usagePurpose === "billing_ratios") return null;
  const model = ctx.upstreamModel || ctx.model,
    request = normalizeRequest(ctx.requestBody, model).body;
  if (!Object.hasOwn(MODELS, model)) return { seconds: request.seconds, video_count: 1, resolution: request.resolution };
  return { ...(MODELS[model].perSecond ? { seconds: request.seconds } : { video_count: 1 }), resolution: request.resolution };
}

export function parseSubmitResponse(_ctx, response) {
  const body = response.body;
  if (!body || typeof body !== "object" || Array.isArray(body) || !body.task || typeof body.task !== "object" || Array.isArray(body.task))
    throw new Error("Wolong returned an invalid video receipt");
  // An accepted task id must survive uncertain/failed receipts. Polling validates
  // terminal evidence before any settlement, refund, or retry is permitted.
  return { taskId: taskIdentity(body.task.id), taskData: body };
}

export function buildQueryRequest(ctx) {
  return {
    method: "GET",
    url: ctx.baseUrl.replace(/\/+$/, "") + "/v1/tasks/" + encodeURIComponent(taskIdentity(ctx.taskId)),
    headers: { Authorization: "Bearer " + ctx.apiKey },
  };
}

export function parseTaskResult(ctx, body) {
  if (!body || body.ok !== true || !body.task || typeof body.task !== "object" || Array.isArray(body.task))
    return { status: "UNKNOWN", reason: "Invalid Wolong task response" };
  const task = body.task;
  if (typeof task.id !== "string" || task.id !== ctx.taskId) return { status: "UNKNOWN", reason: "Wolong task id mismatch" };
  const statuses = { queued: "QUEUED", running: "IN_PROGRESS", needs_review: "IN_PROGRESS", succeeded: "SUCCESS", failed: "FAILURE", canceled: "FAILURE" };
  if (typeof task.status !== "string" || !Object.hasOwn(statuses, task.status)) return { status: "UNKNOWN", reason: "Unrecognized Wolong task status" };
  const result = { status: statuses[task.status] };
  if (result.status === "SUCCESS") {
    if (task.error !== undefined && task.error !== null && task.error !== "") return { status: "UNKNOWN", reason: "Wolong success contradicts its error" };
    try {
      result.url = videoResult(task, ctx.baseUrl);
    } catch {
      return { status: "UNKNOWN", reason: "Wolong success has no valid video output" };
    }
    result.progress = "100%";
  } else if (result.status === "FAILURE") {
    if (task.data !== undefined && task.data !== null && (!Array.isArray(task.data) || task.data.length > 0))
      return { status: "UNKNOWN", reason: "Wolong failure contains an output or invalid output data" };
    const charged = task.charged_amount,
      refunded = task.refunded_amount;
    const validCharged = typeof charged === "number" && Number.isFinite(charged) && charged >= 0;
    const validRefunded = typeof refunded === "number" && Number.isFinite(refunded) && refunded >= 0;
    if (
      (charged !== undefined && !validCharged) ||
      (refunded !== undefined && !validRefunded) ||
      (task.refunded !== undefined && typeof task.refunded !== "boolean") ||
      (task.refunded === false && validRefunded && refunded > 0) ||
      (task.refunded === true && validCharged && charged > 0 && validRefunded && refunded < charged)
    )
      return { status: "UNKNOWN", reason: "Wolong failure has contradictory refund evidence" };
    const confirmed = task.refunded === true || (validCharged && charged === 0) || (validCharged && validRefunded && refunded >= charged);
    if (!confirmed) return { status: "UNKNOWN", reason: "Wolong failure refund is unconfirmed" };
    result.reason = typeof task.error === "string" && task.error.trim() ? task.error.trim() : "Wolong video generation " + task.status;
  }
  if (result.status !== "SUCCESS" && typeof task.progress === "number" && Number.isFinite(task.progress) && task.progress >= 0 && task.progress <= 100)
    result.progress = Math.min(result.status === "FAILURE" ? 100 : 99, Math.floor(task.progress)) + "%";
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
  const task = ctx.data && ctx.data.task;
  if (!task || task.id !== ctx.upstreamTaskId || task.status !== "succeeded") throw new Error("Wolong video output is unavailable");
  return { method: ctx.clientRequest.method, url: videoResult(task, ctx.baseUrl), credentialless: true };
}

export const protocols = {
  openai_video: {
    decodeRequest(ctx) {
      if (!ctx.body || ctx.body.kind !== "json" || (ctx.operation && ctx.operation !== "create")) throw new Error("Wolong supports JSON video generation only");
      // Public aliases are not upstream model identities. Preserve the complete
      // input until channel selection supplies an upstream model, so first-pass
      // normalization cannot change a known model's default or media semantics.
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
