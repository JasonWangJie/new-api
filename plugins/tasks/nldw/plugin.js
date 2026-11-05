const ASPECT_RATIOS = ["16:9", "9:16", "1:1", "4:3", "3:4", "21:9"];
const MODELS = {
  "sd2.5pp": {
    min: 5,
    max: 15,
    resolutions: ["720p", "1080p"],
    defaultResolution: "720p",
    text: true,
    images: 30,
    videos: 10,
    audios: 10,
    total: 50,
    sizes: { image: 10, video: 100, audio: 20 },
  },
  "sd2.5": {
    min: 5,
    max: 30,
    resolutions: ["480p", "720p"],
    defaultResolution: "720p",
    images: 30,
    videos: 10,
    audios: 10,
    total: 50,
    sizes: { image: 30, video: 200, audio: 15 },
  },
  "sd2.5de": {
    min: 4,
    max: 30,
    resolutions: ["480p", "720p"],
    defaultResolution: "480p",
    images: 30,
    videos: 0,
    audios: 10,
    total: 40,
    sizes: { image: 30, audio: 15 },
  },
};
const REQUEST_FIELDS = new Set([
  "model",
  "prompt",
  "seconds",
  "duration",
  "resolution",
  "size",
  "aspect_ratio",
  "ratio",
  "mode",
  "references",
  "image",
  "input_reference",
  "reference_images",
  "images",
  "image_urls",
  "reference_videos",
  "videos",
  "video_urls",
  "reference_audios",
  "audios",
  "audio_urls",
]);
const RESOLUTION_SCHEMA = { enum: ["480p", "720p", "1080p"], description: { en: "Output video resolution", zh: "输出视频分辨率" } };
const SECONDS_SCHEMA = { type: "number", unit: "second", description: { en: "Video generation unit price", zh: "视频生成单价" } };
const REQUESTS_SCHEMA = {
  type: "number",
  unit: "count",
  unitLabel: { en: "video", zh: "条" },
  description: { en: "Video generation unit price", zh: "视频生成单价" },
};

export const meta = {
  apiVersion: 1,
  key: "nldw",
  name: "NLDW Video",
  icon: "text:NL",
  version: "1.0.2",
  author: { name: "new-api contributors" },
  baseUrl: "https://api.nldw.online",
  website: "https://api.nldw.online/video-api-docs",
  description: { en: "NLDW SD2.5 video generation with image, video and audio references", zh: "NLDW SD2.5 图片、视频与音频参考视频生成" },
  models: Object.keys(MODELS),
  fetchMode: "per_task",
  protocols: ["openai_video"],
  usageSchema: { seconds: SECONDS_SCHEMA, resolution: RESOLUTION_SCHEMA },
  usageProfiles: Object.entries(MODELS).map(([model, config]) => ({
    models: [model],
    schema: { ...(config.text ? { requests: REQUESTS_SCHEMA } : { seconds: SECONDS_SCHEMA }), resolution: { ...RESOLUTION_SCHEMA, enum: config.resolutions } },
  })),
  videoProfiles: Object.entries(MODELS).map(([model, config]) => {
    const settings = {
      duration: { min: config.min, max: config.max, step: 1, default: config.min },
      resolutions: config.resolutions,
      defaultResolution: config.defaultResolution,
      aspectRatios: config.text ? ASPECT_RATIOS : [...ASPECT_RATIOS, "adaptive"],
      defaultAspectRatio: "16:9",
    };
    const inputs = [{ name: "reference_images", kind: "image", sources: ["url", "data_uri"], maxItems: config.images }];
    if (config.videos) inputs.push({ name: "reference_videos", kind: "video", sources: ["url", "data_uri"], maxItems: config.videos });
    inputs.push({ name: "reference_audios", kind: "audio", sources: ["url", "data_uri"], maxItems: config.audios });
    const modes = [{ ...settings, name: "reference_to_video", inputs }];
    if (config.text) modes.unshift({ ...settings, name: "text_to_video" });
    return { models: [model], modes };
  }),
};

function modelConfig(model) {
  // oxlint-disable no-control-regex -- Match the host's model identity rules
  if (typeof model !== "string" || !model || model !== model.trim() || /[\x00-\x1f\x7f-\x9f,*]/.test(model)) throw new Error("Invalid NLDW video model");
  // oxlint-enable no-control-regex
  let modelBytes = 0;
  for (const character of model) {
    const codePoint = character.codePointAt(0);
    modelBytes += codePoint <= 0x7f ? 1 : codePoint <= 0x7ff ? 2 : codePoint <= 0xffff ? 3 : 4;
    if (modelBytes > 255) throw new Error("NLDW video model must not exceed 255 UTF-8 bytes");
  }
  if (Object.hasOwn(MODELS, model)) return MODELS[model];
  // Channel capabilities own unknown-model support. These are the host safety
  // ceilings, not vendor promises; custom references use public URLs only.
  return {
    custom: true,
    min: 1,
    max: 3600,
    resolutions: RESOLUTION_SCHEMA.enum,
    images: 64,
    videos: 64,
    audios: 64,
    total: 64,
  };
}

function aliasedValue(request, names) {
  let value;
  for (const name of names) {
    if (!Object.hasOwn(request, name)) continue;
    if (value !== undefined && String(value) !== String(request[name])) throw new Error(names.join(" and ") + " must match");
    value = request[name];
  }
  return value;
}

function videoSeconds(value, min, max) {
  if (!["number", "string"].includes(typeof value) || (typeof value === "string" && !value.trim())) throw new Error("seconds must be an integer");
  const seconds = Number(value);
  if (!Number.isInteger(seconds) || seconds < min || seconds > max) throw new Error("seconds must be between " + min + " and " + max);
  return seconds;
}

function referenceSource(value, kind, model, config) {
  if (typeof value !== "string" || !value) throw new Error("Media references must be public HTTPS URLs or base64 data URLs");
  if (config.custom && value.startsWith("data:")) throw new Error("Channel-configured NLDW models require public HTTPS media URLs");
  if (value.startsWith("data:")) {
    const separator = value.indexOf(",");
    const header = value.slice(0, separator);
    const match = /^data:([a-z0-9.+-]+\/[a-z0-9.+-]+);base64$/i.exec(header);
    if (!match || !match[1].toLowerCase().startsWith(kind + "/")) throw new Error("Reference data URL does not match its media kind");
    const mimeType = match[1].toLowerCase();
    if (
      model === "sd2.5de" &&
      !(kind === "image" ? ["image/jpeg", "image/png", "image/webp"] : ["audio/mpeg", "audio/mp3", "audio/wav", "audio/x-wav"]).includes(mimeType)
    )
      throw new Error("Unsupported SD2.5DE reference media format");
    const encoded = value.slice(separator + 1);
    const maxBytes = config.sizes[kind] * 1024 * 1024;
    if (encoded.length > Math.ceil(maxBytes / 3) * 4) throw new Error("Reference exceeds the NLDW media size limit");
    if (!encoded || encoded.length % 4 !== 0 || !/^[A-Za-z0-9+/]*={0,2}$/.test(encoded)) throw new Error("Reference data URL must contain valid base64");
    const padding = encoded.endsWith("==") ? 2 : encoded.endsWith("=") ? 1 : 0;
    if ((encoded.length / 4) * 3 - padding > maxBytes) throw new Error("Reference exceeds the NLDW media size limit");
    return value;
  }
  // URL media bytes, MIME signatures, dimensions and durations are checked by
  // NLDW when it downloads the reference; this plugin never fetches URLs itself.
  // oxlint-disable no-control-regex -- Reject controls and ambiguous authorities
  if (value.length > 8192 || /[\s\\\x00-\x1f\x7f]/.test(value)) throw new Error("Media references must use public HTTPS URLs");
  // oxlint-enable no-control-regex
  const match = /^https:\/\/([a-z0-9](?:[a-z0-9.-]*[a-z0-9])?)(?::([0-9]{1,5}))?(?:[/?#].*)?$/i.exec(value);
  if (!match || (match[2] && (Number(match[2]) < 1 || Number(match[2]) > 65535)))
    throw new Error("Media references must use public HTTPS URLs without credentials");
  const host = match[1].toLowerCase();
  if (
    !host.includes(".") ||
    [".local", ".localhost", ".internal"].some((suffix) => host.endsWith(suffix)) ||
    host.split(".").some((part) => !part || part.startsWith("-") || part.endsWith("-"))
  )
    throw new Error("Media references must use public hosts");
  if (/^[0-9.]+$/.test(host)) {
    const parts = host.split(".");
    const ip = parts.map(Number);
    if (ip.length !== 4 || parts.some((part, index) => String(ip[index]) !== part || ip[index] > 255)) throw new Error("Media reference IP address is invalid");
    if (
      [0, 10, 127].includes(ip[0]) ||
      ip[0] >= 224 ||
      (ip[0] === 169 && ip[1] === 254) ||
      (ip[0] === 172 && ip[1] >= 16 && ip[1] <= 31) ||
      (ip[0] === 192 && ip[1] === 168) ||
      (ip[0] === 100 && ip[1] >= 64 && ip[1] <= 127)
    )
      throw new Error("Media references must use public hosts");
  }
  return value;
}

function normalizeRequest(request, model) {
  const config = modelConfig(model);
  if (!request || typeof request !== "object" || Array.isArray(request)) throw new Error("JSON object required");
  for (const name of Object.keys(request)) {
    if (!REQUEST_FIELDS.has(name)) throw new Error(name + " is not supported by NLDW video");
  }
  if (typeof request.prompt !== "string" || !request.prompt.trim()) throw new Error("prompt is required");
  if (
    request.mode !== undefined &&
    request.mode !== "omni_reference" &&
    !(config.custom && ["text_to_video", "reference_to_video", "image_to_video"].includes(request.mode))
  )
    throw new Error("Unsupported NLDW video mode");
  const rawSeconds = aliasedValue(request, ["seconds", "duration"]);
  if (config.custom && rawSeconds === undefined) throw new Error("seconds is required for a channel-configured NLDW model");
  const seconds = videoSeconds(rawSeconds === undefined ? config.min : rawSeconds, config.min, config.max);
  const rawResolution = aliasedValue(request, ["resolution", "size"]);
  if (config.custom && rawResolution === undefined) throw new Error("resolution is required for a channel-configured NLDW model");
  const resolution = rawResolution === undefined ? config.defaultResolution : rawResolution;
  if (config.custom ? typeof resolution !== "string" || !/^[A-Za-z0-9][A-Za-z0-9_.-]{0,31}$/.test(resolution) : !config.resolutions.includes(resolution))
    throw new Error("Unsupported NLDW video resolution");
  const rawRatio = aliasedValue(request, ["aspect_ratio", "ratio"]);
  if (config.custom && rawRatio === undefined) throw new Error("aspect_ratio is required for a channel-configured NLDW model");
  const ratio = rawRatio === undefined ? "16:9" : rawRatio;
  if (
    config.custom
      ? typeof ratio !== "string" || (!/^[1-9][0-9]{0,2}:[1-9][0-9]{0,2}$/.test(ratio) && !["auto", "adaptive"].includes(ratio))
      : !ASPECT_RATIOS.includes(ratio) && !(ratio === "adaptive" && !config.text)
  )
    throw new Error("Unsupported NLDW video aspect ratio");
  const references = [];
  if (request.references !== undefined) {
    if (!Array.isArray(request.references) || request.references.length > config.total) throw new Error("references exceeds the NLDW media limit");
    for (const reference of request.references) {
      if (!reference || typeof reference !== "object" || Array.isArray(reference)) throw new Error("Invalid NLDW media reference");
      const native = Object.hasOwn(reference, "kind");
      for (const name of Object.keys(reference)) {
        if (!(native ? ["kind", "data"] : ["type", "source", "role"]).includes(name)) throw new Error("Unsupported NLDW reference field: " + name);
      }
      const kind = native ? reference.kind : reference.type;
      if (!["image", "video", "audio"].includes(kind) || !config[kind + "s"]) throw new Error("Unsupported NLDW reference media kind");
      if (!native && reference.role !== undefined && !["", "reference", "reference_" + kind].includes(reference.role))
        throw new Error("NLDW supports ordinary media references only");
      references.push({ type: kind, source: referenceSource(native ? reference.data : reference.source, kind, model, config) });
    }
  }
  const image = aliasedValue(request, ["image", "input_reference"]);
  if (image !== undefined) references.push({ type: "image", source: referenceSource(image, "image", model, config) });
  for (const [kind, names] of [
    ["image", ["reference_images", "images", "image_urls"]],
    ["video", ["reference_videos", "videos", "video_urls"]],
    ["audio", ["reference_audios", "audios", "audio_urls"]],
  ]) {
    for (const name of names) {
      if (request[name] === undefined) continue;
      // Capability overrides may narrow an input to one item. The shared
      // workbench emits a scalar for that case, and an array for larger limits.
      const sources = typeof request[name] === "string" ? [request[name]] : request[name];
      if (!Array.isArray(sources) || sources.length > config[kind + "s"]) throw new Error(name + " exceeds the NLDW media limit");
      for (const source of sources) references.push({ type: kind, source: referenceSource(source, kind, model, config) });
    }
  }
  const counts = { image: 0, video: 0, audio: 0 };
  for (const reference of references) counts[reference.type]++;
  if (references.length > config.total || counts.image > config.images || counts.video > config.videos || counts.audio > config.audios)
    throw new Error("References exceed the NLDW media limits");
  if (model === "sd2.5" && !counts.image && !counts.video) throw new Error("SD2.5 requires at least one image or video reference");
  if (model === "sd2.5de" && !counts.image && !counts.audio) throw new Error("SD2.5DE requires at least one image or audio reference");
  const action =
    config.custom && image !== undefined && references.length === 1 ? "image_to_video" : references.length ? "reference_to_video" : "text_to_video";
  if (config.custom && request.mode !== undefined && request.mode !== "omni_reference" && request.mode !== action)
    throw new Error("mode conflicts with the supplied media");
  const body = { model, prompt: request.prompt.trim(), seconds, resolution, aspect_ratio: ratio, mode: "omni_reference" };
  if (action === "image_to_video") body.image = references[0].source;
  else if (references.length) body.references = references;
  return { body, action };
}

function taskIdentity(value) {
  // oxlint-disable no-control-regex -- IDs become URL components and HTTP headers
  if (typeof value !== "string" || !value || value === "." || value === ".." || value.length > 191 || /[\s\x00-\x1f\x7f]/.test(value))
    throw new Error("NLDW video task id is missing or invalid");
  // oxlint-enable no-control-regex
  return value;
}

function videoAPIError(value) {
  if (value === undefined || value === null || value === false || value === 0) return "";
  if (typeof value === "string") return value.trim();
  if (typeof value !== "object" || Array.isArray(value)) return "NLDW returned an invalid video error";
  if (typeof value.message === "string" && value.message.trim()) return value.message.trim();
  if ((typeof value.code === "string" && value.code.trim() && value.code !== "0") || (typeof value.code === "number" && value.code !== 0))
    return "NLDW video error: " + String(value.code);
  for (const field of Object.keys(value)) {
    const detail = value[field];
    if (detail === undefined || detail === null || detail === "" || detail === false || detail === 0 || (field === "code" && detail === "0")) continue;
    return "NLDW returned an invalid video error";
  }
  return "";
}

function completionSeconds(ctx, body) {
  const raw = aliasedValue(body, ["seconds", "duration"]);
  if (raw === undefined) return undefined;
  const config = modelConfig(ctx.upstreamModel || ctx.model);
  const seconds = videoSeconds(raw, config.min, config.max);
  const requested = ctx.state && ctx.state.requestedSeconds;
  if (!Number.isInteger(requested) || requested < config.min || requested > config.max) throw new Error("NLDW reserved video duration is unavailable");
  if (seconds > requested) throw new Error("NLDW completed video duration exceeds the requested duration");
  return seconds;
}

function hasVideoResult(body) {
  const pending = [{ value: body, depth: 0 }];
  let visited = 0;
  while (pending.length) {
    const { value, depth } = pending.pop();
    if (depth > 16 || ++visited > 4096) return true;
    if (!value || typeof value !== "object") continue;
    if (Array.isArray(value)) {
      for (const child of value) pending.push({ value: child, depth: depth + 1 });
      continue;
    }
    for (const field of Object.keys(value)) {
      const name = field.toLowerCase().replace(/[_-]/g, "");
      // Echoed inputs and diagnostics can contain media URLs without proving
      // that the supposedly failed task produced an output.
      if (["prompt", "input", "request", "references", "error", "errors", "reason", "message"].includes(name)) continue;
      const child = value[field];
      if (["success", "succeeded", "completed"].includes(name) && child === true) return true;
      if (name === "status" && ["success", "succeeded", "completed", "done"].includes(child)) return true;
      if (["url", "remoteurl", "videourl", "lastframeurl", "outputurl"].includes(name) && typeof child === "string" && child.trim()) return true;
      if (
        [
          "result",
          "results",
          "output",
          "outputs",
          "artifacts",
          "video",
          "videos",
          "image",
          "images",
          "audio",
          "audios",
          "file",
          "files",
          "b64json",
          "b64video",
          "bytesbase64encoded",
          "base64",
          "lastframe",
        ].includes(name)
      ) {
        if (typeof child === "string" && child.trim()) return true;
        if (child && typeof child === "object" && Object.keys(child).length) return true;
      }
      if (child && typeof child === "object") pending.push({ value: child, depth: depth + 1 });
    }
  }
  return false;
}

export function buildSubmitRequest(ctx) {
  const prepared = normalizeRequest(ctx.requestBody, ctx.upstreamModel || ctx.model);
  const body = { ...prepared.body };
  if (body.image) {
    body.references = [{ type: "image", source: body.image }];
    delete body.image;
  }
  if (body.references) body.references = body.references.map((reference) => ({ kind: reference.type, data: reference.source }));
  return {
    method: "POST",
    url: ctx.baseUrl + "/v1/videos",
    headers: { "Content-Type": "application/json", Authorization: "Bearer " + ctx.apiKey, "Idempotency-Key": taskIdentity(ctx.publicTaskId) },
    body,
  };
}

export function extractUsage(ctx) {
  const model = ctx.upstreamModel || ctx.model;
  const prepared = normalizeRequest(ctx.requestBody, model).body;
  if (!Object.hasOwn(MODELS, model))
    return ctx.usagePurpose === "billing_ratios" ? null : { seconds: prepared.seconds, requests: 1, resolution: prepared.resolution.toLowerCase() };
  // The host uses legacy ratios only when there is no usage expression.
  // Per-video prices must never be multiplied by their requested duration.
  if (ctx.usagePurpose === "billing_ratios") return model === "sd2.5pp" ? null : { seconds: prepared.seconds };
  return model === "sd2.5pp" ? { requests: 1, resolution: prepared.resolution } : { seconds: prepared.seconds, resolution: prepared.resolution };
}

export function parseSubmitResponse(ctx, response) {
  const body = response.body;
  if (!body || typeof body !== "object" || Array.isArray(body)) throw new Error("NLDW returned an invalid video response");
  const id = taskIdentity(body.id || body.task_id);
  if (body.id !== undefined && body.task_id !== undefined && body.id !== body.task_id) throw new Error("NLDW returned conflicting video task ids");
  const prepared = normalizeRequest(ctx.requestBody, ctx.upstreamModel || ctx.model).body;
  // Keep every accepted ID, including failed or uncertain receipts. Polling
  // validates the full terminal response before the host settles or refunds.
  return { taskId: id, taskData: body, state: { requestedSeconds: prepared.seconds } };
}

export function buildQueryRequest(ctx) {
  return { method: "GET", url: ctx.baseUrl + "/v1/videos/" + encodeURIComponent(taskIdentity(ctx.taskId)), headers: { Authorization: "Bearer " + ctx.apiKey } };
}

export function parseTaskResult(ctx, body) {
  if (!body || typeof body !== "object" || Array.isArray(body)) return { status: "UNKNOWN", reason: "NLDW returned an invalid video response" };
  for (const id of [body.id, body.task_id]) {
    if (id !== undefined && taskIdentity(id) !== ctx.taskId) throw new Error("NLDW returned a different video task id");
  }
  const statuses = { queued: "QUEUED", in_progress: "IN_PROGRESS", submission_unknown: "IN_PROGRESS", completed: "SUCCESS", failed: "FAILURE" };
  if (typeof body.status !== "string" || !Object.hasOwn(statuses, body.status)) return { status: "UNKNOWN", reason: "Unrecognized NLDW video status" };
  const result = { status: statuses[body.status] };
  if (result.status === "SUCCESS") {
    if (videoAPIError(body.error)) throw new Error("NLDW returned a completed task with an error");
    completionSeconds(ctx, body);
  }
  if (result.status === "FAILURE") {
    if (hasVideoResult(body)) throw new Error("NLDW returned a failed task with a video result");
    result.reason = videoAPIError(body.error) || "NLDW video generation failed";
  }
  if (typeof body.progress === "number" && Number.isFinite(body.progress) && body.progress >= 0 && body.progress <= 100)
    result.progress = (result.status === "IN_PROGRESS" ? Math.min(99, body.progress) : body.progress) + "%";
  return result;
}

export function extractUsageOnComplete(ctx, result, body) {
  if (!body || body.status !== "completed" || result.status !== "SUCCESS") return {};
  if (videoAPIError(body.error)) throw new Error("NLDW returned a completed task with an error");
  // Unknown models retain the host-selected submission usage and billing mode.
  if (!Object.hasOwn(MODELS, ctx.upstreamModel || ctx.model)) return {};
  const seconds = completionSeconds(ctx, body);
  if ((ctx.upstreamModel || ctx.model) === "sd2.5pp") return { requests: 1 };
  return seconds === undefined ? {} : { seconds };
}

export function listArtifacts(task) {
  return task.status === "SUCCESS" ? [{ key: "video", type: "video", mimeType: "video/mp4" }] : [];
}

export function buildContentRequest(ctx) {
  if (ctx.artifactKey !== "video") throw new Error("artifact_not_found");
  return {
    method: ctx.clientRequest.method,
    url: ctx.baseUrl + "/v1/videos/" + encodeURIComponent(taskIdentity(ctx.upstreamTaskId)) + "/content",
    headers: { Authorization: "Bearer " + ctx.apiKey },
  };
}

export const protocols = {
  openai_video: {
    decodeRequest(ctx) {
      if (!ctx.body || ctx.body.kind !== "json") throw new Error("NLDW video requires JSON with public HTTPS URLs or base64 data URLs");
      if (ctx.operation && ctx.operation !== "create") throw new Error("NLDW video supports generation only");
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
