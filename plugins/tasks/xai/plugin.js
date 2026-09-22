const MODELS_15 = ["grok-imagine-video-1.5", "grok-imagine-video-1.5-preview", "grok-imagine-video-1.5-2026-05-30"];
const ALL_RATIOS = ["1:1", "16:9", "9:16", "4:3", "3:4", "3:2", "2:3"];
const IMAGE_SOURCES = ["url", "data_uri", "file_id", "upload"];
const VIDEO_SOURCES = ["url", "data_uri", "file_id"];

const MODE_TEXT_15 = {
  name: "text_to_video",
  duration: { min: 1, max: 15, step: 1, default: 5 },
  resolutions: ["480p", "720p", "1080p"],
  defaultResolution: "480p",
  aspectRatios: ALL_RATIOS,
  defaultAspectRatio: "16:9",
  options: { generateAudio: true },
};
const MODE_IMAGE_15 = {
  name: "image_to_video",
  duration: { min: 1, max: 15, step: 1, default: 5 },
  resolutions: ["480p", "720p", "1080p"],
  defaultResolution: "480p",
  aspectRatios: ALL_RATIOS,
  defaultAspectRatio: "16:9",
  inputs: [{ name: "image", kind: "image", sources: IMAGE_SOURCES, maxItems: 1, required: true }],
  options: { generateAudio: true },
};
const MODE_REFERENCE_15 = {
  name: "reference_to_video",
  duration: { min: 1, max: 15, step: 1, default: 5 },
  resolutions: ["480p", "720p"],
  defaultResolution: "480p",
  aspectRatios: ALL_RATIOS,
  defaultAspectRatio: "16:9",
  inputs: [
    { name: "image", kind: "image", sources: IMAGE_SOURCES, maxItems: 1 },
    { name: "reference_images", kind: "image", sources: IMAGE_SOURCES, maxItems: 7 },
    { name: "reference_audios", kind: "audio", sources: ["voice_id"], maxItems: 3 },
    { name: "last_frame", kind: "image", sources: IMAGE_SOURCES, maxItems: 1 },
  ],
  options: { generateAudio: true },
};
const MODE_FRAMES_15 = {
  name: "first_last_frame",
  duration: { min: 1, max: 15, step: 1, default: 5 },
  resolutions: ["480p", "720p"],
  defaultResolution: "480p",
  aspectRatios: ALL_RATIOS,
  defaultAspectRatio: "16:9",
  inputs: [
    { name: "image", kind: "image", sources: IMAGE_SOURCES, maxItems: 1 },
    { name: "last_frame", kind: "image", sources: IMAGE_SOURCES, maxItems: 1, required: true },
  ],
  options: { generateAudio: true },
};

export const meta = {
  apiVersion: 1,
  key: "xai",
  name: "xAI Video",
  icon: "XAI",
  description: {
    en: "xAI Grok video generation with text, image, references, and pinned frames",
    zh: "xAI Grok 视频生成，支持文本、图片、参考素材和首尾帧",
  },
  version: "1.1.0",
  author: { name: "QuantumNous", url: "https://x.ai" },
  baseUrl: "https://api.x.ai",
  channelTypes: [48],
  models: [...MODELS_15, "grok-imagine-video"],
  fetchMode: "per_task",
  usageSchema: {
    seconds: {
      type: "number",
      unit: "second",
      description: { en: "Video generation unit price", zh: "视频生成单价" },
    },
    resolution: {
      enum: ["480p", "720p", "1080p"],
      enumLabels: {
        "480p": { en: "480p", zh: "480p" },
        "720p": { en: "720p", zh: "720p" },
        "1080p": { en: "1080p", zh: "1080p" },
      },
      description: { en: "Output video resolution", zh: "输出视频分辨率" },
    },
    input_images: {
      type: "number",
      unit: "count",
      unitLabel: { en: "image", zh: "张", "zh-TW": "張" },
      description: { en: "Input image unit price", zh: "输入图片单价" },
    },
    input_video_seconds: {
      type: "number",
      unit: "second",
      description: { en: "Input video unit price", zh: "输入视频单价" },
    },
    billing_source: {
      enum: ["estimate", "upstream"],
      enumLabels: {
        estimate: { en: "Safe estimate", zh: "安全估值" },
        upstream: { en: "Upstream reported cost", zh: "上游实际费用" },
      },
      description: { en: "Video billing source", zh: "视频计费来源" },
    },
    upstream_cost_credit: {
      type: "number",
      unit: "credit",
      description: { en: "Upstream cost credit unit price", zh: "上游费用 Credit 单价" },
    },
  },
  videoProfiles: [
    { models: MODELS_15, modes: [MODE_TEXT_15, MODE_IMAGE_15, MODE_REFERENCE_15, MODE_FRAMES_15] },
    {
      models: ["grok-imagine-video"],
      modes: [
        {
          name: "text_to_video",
          duration: { min: 1, max: 15, step: 1, default: 5 },
          resolutions: ["480p", "720p"],
          defaultResolution: "480p",
          aspectRatios: ALL_RATIOS,
          defaultAspectRatio: "16:9",
          options: { generateAudio: true },
        },
        {
          name: "image_to_video",
          duration: { min: 1, max: 15, step: 1, default: 5 },
          resolutions: ["480p", "720p"],
          defaultResolution: "480p",
          aspectRatios: ALL_RATIOS,
          defaultAspectRatio: "16:9",
          inputs: [{ name: "image", kind: "image", sources: IMAGE_SOURCES, maxItems: 1, required: true }],
          options: { generateAudio: true },
        },
        {
          name: "edit_video",
          inputs: [{ name: "video", kind: "video", sources: VIDEO_SOURCES, maxItems: 1, required: true }],
        },
        {
          name: "extend_video",
          duration: { min: 2, max: 10, step: 1, default: 6 },
          extensionDirections: ["backward"],
          defaultExtensionDirection: "backward",
          inputs: [{ name: "video", kind: "video", sources: VIDEO_SOURCES, maxItems: 1, required: true }],
        },
      ],
    },
  ],
  protocols: ["openai_video"],
};

function trimmed(value) {
  return String(value === undefined || value === null ? "" : value).trim();
}

function isModel15(model) {
  return MODELS_15.includes(model);
}

function normalizeResolution(value) {
  const raw = trimmed(value).toLowerCase();
  if (!raw) return "480p";
  if (["480p", "720p", "1080p"].includes(raw)) return raw;
  const parts = raw.replace("*", "x").split("x");
  if (parts.length !== 2) return "";
  const width = Number(parts[0]);
  const height = Number(parts[1]);
  if (!Number.isFinite(width) || !Number.isFinite(height) || width <= 0 || height <= 0) return "";
  const edge = Math.max(width, height);
  if (edge >= 1920) return "1080p";
  if (edge >= 1280) return "720p";
  return "480p";
}

function mediaObject(value, name) {
  if (typeof value === "string") {
    const text = trimmed(value);
    if (!text) throw new Error(`${name} must not be empty`);
    if (!/^https:\/\//i.test(text) && !/^data:/i.test(text)) throw new Error(`${name} must be an HTTPS URL, data URI, or file_id object`);
    return { url: text };
  }
  if (!value || typeof value !== "object" || Array.isArray(value)) throw new Error(`${name} must be an object or URL string`);
  const url = trimmed(value.url);
  const fileID = trimmed(value.file_id);
  if ((url ? 1 : 0) + (fileID ? 1 : 0) !== 1) throw new Error(`${name} must contain exactly one of url or file_id`);
  if (url && !/^https:\/\//i.test(url) && !/^data:/i.test(url)) throw new Error(`${name}.url must be an HTTPS URL or data URI`);
  return url ? { url } : { file_id: fileID };
}

function hasOwn(object, name) {
  return Object.prototype.hasOwnProperty.call(object || {}, name);
}

function videoObject(value) {
  const video = mediaObject(value, "video");
  if (video.file_id) return video;
  const url = trimmed(video.url);
  if (/^data:/i.test(url)) {
    if (!/^data:video\/mp4(?:;|,)/i.test(url)) throw new Error("video data URI must use video/mp4");
    return video;
  }
  const path = url.split(/[?#]/, 1)[0].toLowerCase();
  if (!path.endsWith(".mp4")) throw new Error("video URL must point to an MP4 file");
  return video;
}

function rejectVideoOperationFields(req, names) {
  const metadata = req.metadata && typeof req.metadata === "object" && !Array.isArray(req.metadata) ? req.metadata : {};
  for (const name of names) {
    if (hasOwn(req, name) || hasOwn(metadata, name)) throw new Error(`${name} is not supported for this xAI video operation`);
  }
}

function videoOperationRequest(req, model, operation) {
  if (model !== "grok-imagine-video") throw new Error("xAI video editing and extension require grok-imagine-video");
  const prompt = trimmed(req.prompt);
  if (!prompt) throw new Error("prompt is required");
  if (!hasOwn(req, "video")) throw new Error("video is required");
  rejectVideoOperationFields(req, ["action", "operation", "mode", "source_task_id"]);
  const metadata = req.metadata && typeof req.metadata === "object" && !Array.isArray(req.metadata) ? req.metadata : {};
  for (const name of ["model", "provider", "prompt", "video", "extension_direction"]) {
    if (hasOwn(metadata, name)) throw new Error(`metadata.${name} cannot override a structured xAI video field`);
  }
  const body = { ...req, model, prompt, video: videoObject(req.video) };
  delete body.provider;
  delete body.extension_direction;
  if (operation === "edit") {
    rejectVideoOperationFields(req, [
      "duration",
      "seconds",
      "resolution",
      "size",
      "aspect_ratio",
      "ratio",
      "image",
      "images",
      "input_reference",
      "last_frame",
      "reference_images",
      "reference_videos",
      "reference_audios",
      "generate_audio",
      "watermark",
      "return_last_frame",
      "extension_direction",
    ]);
    return { action: "edit_video", body };
  }
  rejectVideoOperationFields(req, [
    "resolution",
    "size",
    "aspect_ratio",
    "ratio",
    "image",
    "images",
    "input_reference",
    "last_frame",
    "reference_images",
    "reference_videos",
    "reference_audios",
    "generate_audio",
    "watermark",
    "return_last_frame",
  ]);
  if (hasOwn(req, "duration") && hasOwn(req, "seconds")) throw new Error("duration and seconds cannot both be provided");
  const seconds = Number(hasOwn(req, "duration") ? req.duration : hasOwn(req, "seconds") ? req.seconds : 6);
  if (!Number.isInteger(seconds) || seconds < 2 || seconds > 10) throw new Error("extension duration must be an integer between 2 and 10");
  const direction = trimmed(req.extension_direction) || "backward";
  if (direction !== "backward") throw new Error("xAI video extension supports only backward");
  delete body.seconds;
  body.duration = seconds;
  return { action: "extend_video", body };
}

function mediaList(value, name, maxItems) {
  if (value === undefined || value === null || value === "") return [];
  const values = Array.isArray(value) ? value : [value];
  if (values.length > maxItems) throw new Error(`${name} supports at most ${maxItems} items`);
  return values.map(function (item, index) {
    return mediaObject(item, `${name}[${index}]`);
  });
}

function audioList(value) {
  if (value === undefined || value === null || value === "") return [];
  const values = Array.isArray(value) ? value : [value];
  if (values.length > 3) throw new Error("reference_audios supports at most 3 preset voices");
  return values.map(function (item, index) {
    const voiceID = typeof item === "string" ? trimmed(item) : trimmed(item && item.voice_id);
    if (!voiceID) throw new Error(`reference_audios[${index}] must contain voice_id`);
    return { voice_id: voiceID };
  });
}

function parseJSONField(value, name) {
  if (value === undefined) return undefined;
  try {
    return JSON.parse(value);
  } catch {
    throw new Error(`${name} must be valid JSON`);
  }
}

function validateRequest(req, model, fileCounts) {
  rejectVideoOperationFields(req, ["video", "source_task_id", "action", "operation", "mode"]);
  const modern = isModel15(model);
  let duration = req.duration;
  if (duration === undefined) duration = req.seconds;
  if (duration === undefined) duration = 5;
  const seconds = Number(duration);
  if (!Number.isInteger(seconds) || seconds < 1 || seconds > 15) throw new Error("duration must be an integer between 1 and 15");
  const resolution = normalizeResolution(req.resolution || req.size);
  if (!resolution) throw new Error("resolution must be 480p, 720p, 1080p, or a valid pixel size");
  const aspectRatio = trimmed(req.aspect_ratio) || "16:9";
  if (!ALL_RATIOS.includes(aspectRatio)) throw new Error("aspect_ratio is not supported");
  if (!modern && resolution === "1080p") throw new Error("grok-imagine-video supports only 480p or 720p");
  const image = req.image === undefined ? req.input_reference : req.image;
  const hasImage = (image !== undefined && image !== null && image !== "") || fileCounts.image > 0;
  const references = mediaList(req.reference_images, "reference_images", 7);
  const audios = audioList(req.reference_audios);
  const lastFrame = req.last_frame;
  const hasLastFrame = (lastFrame !== undefined && lastFrame !== null && lastFrame !== "") || fileCounts.lastFrame > 0;
  const referenceCount = references.length + fileCounts.references;
  if ((image !== undefined && image !== null && image !== "" ? 1 : 0) + fileCounts.image > 1) {
    throw new Error("image or input_reference must be provided at most once");
  }
  if ((lastFrame !== undefined && lastFrame !== null && lastFrame !== "" ? 1 : 0) + fileCounts.lastFrame > 1) {
    throw new Error("last_frame must be provided at most once");
  }
  if (referenceCount > 7) throw new Error("reference_images supports at most 7 items");
  if (req.reference_videos !== undefined && (!Array.isArray(req.reference_videos) || req.reference_videos.length > 0)) {
    throw new Error("xAI video generation does not support reference_videos");
  }
  if (!modern && (referenceCount > 0 || audios.length > 0 || hasLastFrame)) throw new Error("reference inputs and last_frame require grok-imagine-video-1.5");
  const referenceMode = referenceCount > 0 || audios.length > 0 || hasLastFrame;
  if (referenceMode && resolution === "1080p") throw new Error("reference and first/last-frame modes support at most 720p");
  const prompt = trimmed(req.prompt);
  if (!prompt && !hasImage && !hasLastFrame && referenceCount === 0 && audios.length === 0) throw new Error("prompt is required for text-to-video");
  if (req.generate_audio !== undefined && typeof req.generate_audio !== "boolean") throw new Error("generate_audio must be a boolean");
  let action = "text_to_video";
  if (referenceCount > 0 || audios.length > 0) action = "reference_to_video";
  else if (hasLastFrame) action = "first_last_frame";
  else if (hasImage) action = "image_to_video";
  return { action, seconds, resolution, aspectRatio, references, audios };
}

function normalizedRequest(req, model, validation) {
  const body = { ...req };
  delete body.seconds;
  delete body.size;
  delete body.input_reference;
  delete body.reference_videos;
  delete body.provider;
  body.model = model;
  body.duration = validation.seconds;
  body.resolution = validation.resolution;
  body.aspect_ratio = validation.aspectRatio;
  if (req.image !== undefined || req.input_reference !== undefined) {
    body.image = mediaObject(req.image === undefined ? req.input_reference : req.image, "image");
  }
  if (req.last_frame !== undefined) body.last_frame = mediaObject(req.last_frame, "last_frame");
  if (validation.references.length) body.reference_images = validation.references;
  else delete body.reference_images;
  if (validation.audios.length) body.reference_audios = validation.audios;
  else delete body.reference_audios;
  return body;
}

function multipartRequest(ctx) {
  const fields = ctx.body.fields || {};
  const req = {};
  const scalarFields = ["model", "prompt", "duration", "seconds", "resolution", "size", "aspect_ratio", "generate_audio"];
  for (const name of scalarFields) {
    const values = fields[name] || [];
    if (values.length > 1) throw new Error(`${name} must be provided once`);
    if (values.length) req[name] = values[0];
  }
  if (req.generate_audio !== undefined) {
    if (req.generate_audio !== "true" && req.generate_audio !== "false") throw new Error("generate_audio must be true or false");
    req.generate_audio = req.generate_audio === "true";
  }
  for (const name of ["image", "input_reference", "last_frame"]) {
    const values = fields[name] || [];
    if (values.length > 1) throw new Error(`${name} must be provided once`);
    if (values.length) req[name] = parseJSONField(values[0], name);
  }
  for (const name of ["reference_images", "reference_audios", "reference_videos"]) {
    const values = fields[name] || [];
    if (values.length === 1) {
      const parsed = parseJSONField(values[0], name);
      req[name] = Array.isArray(parsed) ? parsed : [parsed];
    } else if (values.length > 1) {
      req[name] = values.map(function (value) {
        return parseJSONField(value, name);
      });
    }
  }
  const known = new Set([...scalarFields, "image", "input_reference", "last_frame", "reference_images", "reference_audios", "reference_videos"]);
  for (const name of Object.keys(fields)) {
    if (!known.has(name)) throw new Error(`unexpected multipart field: ${name}`);
  }
  return req;
}

function multipartFileCounts(files) {
  const counts = { image: 0, lastFrame: 0, references: 0 };
  for (const file of files || []) {
    if (file.field === "image" || file.field === "input_reference") counts.image++;
    else if (file.field === "last_frame") counts.lastFrame++;
    else if (file.field === "reference_images") counts.references++;
    else throw new Error(`unexpected file field: ${file.field}`);
  }
  if (counts.image > 1) throw new Error("image or input_reference must be provided at most once");
  if (counts.lastFrame > 1) throw new Error("last_frame must be provided at most once");
  return counts;
}

function addMultipartFiles(body, files) {
  const references = Array.isArray(body.reference_images) ? [...body.reference_images] : [];
  for (const file of files || []) {
    const placeholder = { __fileRef: file.ref, encoding: "dataUrl", mimeType: file.mimeType || "image/png", maxBytes: 20971520 };
    const media = { url: placeholder };
    if (file.field === "image" || file.field === "input_reference") body.image = media;
    else if (file.field === "last_frame") body.last_frame = media;
    else if (file.field === "reference_images") references.push(media);
  }
  if (references.length) body.reference_images = references;
}

function artifactData(ctx) {
  const data = ctx && ctx.data && typeof ctx.data === "object" ? ctx.data : {};
  if (data.data && typeof data.data === "object" && data.data.task_id && Object.hasOwn(data.data, "data")) return data.data.data || {};
  return data;
}

export function buildSubmitRequest(ctx) {
  const req = ctx.requestBody || {};
  const body = { ...req };
  body.model = ctx.upstreamModel || ctx.model;
  addMultipartFiles(body, ctx.files || []);
  const path = ctx.action === "edit_video" ? "edits" : ctx.action === "extend_video" ? "extensions" : "generations";
  return {
    url: `${ctx.baseUrl}/v1/videos/${path}`,
    method: "POST",
    headers: { "Content-Type": "application/json", Accept: "application/json", Authorization: `Bearer ${ctx.apiKey}` },
    body,
    action: ctx.action,
    rewriteModel: body.model,
  };
}

export function parseSubmitResponse(_ctx, response) {
  const body = response.body || {};
  const taskID = trimmed(body.request_id);
  if (!taskID) throw new Error("request_id is empty");
  return { taskId: taskID, taskData: body };
}

export function buildQueryRequest(ctx) {
  return {
    url: `${ctx.baseUrl}/v1/videos/${encodeURIComponent(ctx.taskId)}`,
    method: "GET",
    headers: { Accept: "application/json", Authorization: `Bearer ${ctx.apiKey}` },
  };
}

export function parseTaskResult(_ctx, body) {
  const status = trimmed(body && body.status).toLowerCase();
  if (status === "pending") return { status: "QUEUED", progress: trimmed(body.progress) ? `${String(body.progress)}%` : "10%" };
  if (status === "done") return { status: "SUCCESS", progress: "100%", url: body.video && body.video.url ? body.video.url : "" };
  if (status === "failed" || status === "expired") {
    const reason = body && body.error && body.error.message ? body.error.message : status;
    return { status: "FAILURE", progress: "100%", reason };
  }
  return { status: "UNKNOWN", reason: `unrecognized status: ${status}` };
}

export function extractUsage(ctx) {
  const req = ctx.requestBody || {};
  if (ctx.action === "edit_video") {
    return {
      seconds: 8.7,
      resolution: "720p",
      input_images: 0,
      input_video_seconds: 8.7,
      billing_source: "estimate",
      upstream_cost_credit: 0,
    };
  }
  if (ctx.action === "extend_video") {
    return {
      seconds: Number(req.duration || 6),
      resolution: "720p",
      input_images: 0,
      input_video_seconds: 15,
      billing_source: "estimate",
      upstream_cost_credit: 0,
    };
  }
  const files = multipartFileCounts(ctx.files || []);
  const image = req.image === undefined ? req.input_reference : req.image;
  let references = 0;
  if (Array.isArray(req.reference_images)) references = req.reference_images.length;
  else if (req.reference_images) references = 1;
  return {
    seconds: Number(req.duration || req.seconds || 5),
    resolution: normalizeResolution(req.resolution || req.size),
    input_images: (image ? 1 : 0) + (req.last_frame ? 1 : 0) + references + files.image + files.lastFrame + files.references,
    input_video_seconds: 0,
    billing_source: "estimate",
    upstream_cost_credit: 0,
  };
}

export function extractUsageOnComplete(task, _result, body) {
  const facts = {};
  const action = trimmed(task && task.action);
  const seconds = Number(body && body.video && body.video.duration);
  const maximumSeconds = action === "edit_video" ? 8.7 : 15;
  if (action !== "extend_video" && Number.isFinite(seconds) && seconds >= 1 && seconds <= maximumSeconds) facts.seconds = seconds;
  const resolution = normalizeResolution(body && (body.resolution || (body.video && body.video.resolution)));
  if (body && (body.resolution || (body.video && body.video.resolution))) facts.resolution = resolution;
  const ticks = Number(body && body.usage && body.usage.cost_in_usd_ticks);
  if (Number.isFinite(ticks) && ticks >= 0) {
    facts.billing_source = "upstream";
    facts.upstream_cost_credit = ticks / 10000000000;
  }
  return facts;
}

export function listArtifacts(task) {
  if (!task || task.status !== "SUCCESS") return [];
  const data = artifactData(task);
  return data.video && trimmed(data.video.url) ? [{ key: "video", type: "video", mimeType: "video/mp4" }] : [];
}

export function buildContentRequest(ctx) {
  if (ctx.artifactKey !== "video") throw new Error("artifact_not_found");
  const data = artifactData(ctx);
  const url = trimmed(data.video && data.video.url);
  if (!url) throw new Error("artifact_not_found");
  return { url, method: ctx.clientRequest.method, credentialless: true };
}

export const protocols = {
  openai_video: {
    decodeRequest(ctx) {
      if (!ctx.body || (ctx.body.kind !== "json" && ctx.body.kind !== "multipart")) throw new Error("JSON or multipart body required");
      const req = ctx.body.kind === "json" ? ctx.body.value : multipartRequest(ctx);
      if (!req || typeof req !== "object" || Array.isArray(req)) throw new Error("request body must be an object");
      const model = trimmed(ctx.model || req.model);
      if (!meta.models.includes(model)) throw new Error("unsupported xAI video model");
      if (ctx.operation === "edit" || ctx.operation === "extend") {
        if (ctx.body.kind !== "json") throw new Error("xAI video editing and extension require a JSON body");
        const prepared = videoOperationRequest(req, model, ctx.operation);
        return { kind: "submit", model, action: prepared.action, requestBody: prepared.body };
      }
      const fileCounts = ctx.body.kind === "multipart" ? multipartFileCounts(ctx.body.files || []) : { image: 0, lastFrame: 0, references: 0 };
      const validation = validateRequest(req, model, fileCounts);
      return { kind: "submit", model: model, action: validation.action, requestBody: normalizedRequest(req, model, validation) };
    },
    render(_ctx, task) {
      const statuses = { NOT_START: "pending", SUBMITTED: "pending", QUEUED: "pending", IN_PROGRESS: "pending", SUCCESS: "done", FAILURE: "failed" };
      const data = task.data && typeof task.data === "object" && !Array.isArray(task.data) ? Object.assign({}, task.data) : {};
      data.status = statuses[task.status] || "pending";
      if (task.fail_reason) data.error = { message: task.fail_reason };
      return data;
    },
  },
};
