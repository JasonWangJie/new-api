const RATIOS = ["auto", "21:9", "16:9", "4:3", "1:1", "3:4", "9:16"];
const MODELS = {
  "seedance-2.0": {
    min: 4,
    max: 15,
    defaultSeconds: 5,
    resolutions: ["480p", "720p", "1080p", "4k"],
    defaultResolution: "720p",
    ratios: RATIOS,
    images: 9,
    videos: 3,
    audios: 3,
    total: 12,
    frames: true,
  },
  "seedance-2.5": {
    min: 4,
    max: 30,
    defaultSeconds: 5,
    resolutions: ["480p", "720p", "1080p"],
    defaultResolution: "720p",
    ratios: RATIOS,
    images: 30,
    videos: 10,
    audios: 10,
    total: 50,
    frames: true,
    audioOnly: true,
  },
  "kling-4.0": {
    min: 3,
    max: 30,
    defaultSeconds: 5,
    resolutions: ["720p", "1080p"],
    defaultResolution: "1080p",
    ratios: ["auto", "16:9", "9:16", "1:1", "21:9"],
    images: 10,
    videos: 5,
    audios: 0,
    total: 15,
    frames: true,
  },
  "gemini-omni-1.1-flash": {
    durations: [4, 6, 8, 10],
    defaultSeconds: 6,
    resolutions: ["360p", "720p", "1080p"],
    defaultResolution: "720p",
    ratios: ["16:9", "9:16"],
    images: 10,
    videos: 1,
    audios: 0,
    total: 11,
    frames: false,
    singleVideo: true,
  },
  // Automatic duration and reference-video billing require durations that the
  // published Task API does not report. Only explicit output durations are billed.
  "wan3.0-video": {
    min: 2,
    max: 30,
    defaultSeconds: 5,
    resolutions: ["480p", "720p", "1080p"],
    defaultResolution: "720p",
    ratios: RATIOS,
    images: 10,
    videos: 0,
    audios: 5,
    total: 15,
    frames: true,
    audioOnly: true,
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
  "image",
  "input_reference",
  "last_frame",
  "start_frame",
  "end_frame",
  "reference_images",
  "images",
  "image_urls",
  "reference_videos",
  "videos",
  "video_urls",
  "video",
  "reference_audios",
  "audios",
  "audio_urls",
  "references",
]);

function videoProfile(model, config) {
  const settings = {
    duration: config.durations
      ? { values: config.durations, default: config.defaultSeconds }
      : { min: config.min, max: config.max, step: 1, default: config.defaultSeconds },
    resolutions: config.resolutions,
    defaultResolution: config.defaultResolution,
    aspectRatios: config.ratios,
    defaultAspectRatio: "16:9",
  };
  const image = { name: "image", kind: "image", sources: ["url"], maxItems: 1, required: true };
  const referenceInputs = [{ name: "reference_images", kind: "image", sources: ["url"], maxItems: config.images }];
  if (config.videos) referenceInputs.push({ name: "reference_videos", kind: "video", sources: ["url"], maxItems: config.videos });
  if (config.audios) referenceInputs.push({ name: "reference_audios", kind: "audio", sources: ["url"], maxItems: config.audios });
  const adaptive = model === "kling-4.0" || model === "wan3.0-video";
  const referenceSettings = adaptive ? { ...settings, defaultAspectRatio: "auto" } : settings;
  const frameSettings = adaptive ? { ...settings, aspectRatios: ["auto"], defaultAspectRatio: "auto" } : settings;
  const modes = [
    { ...settings, name: "text_to_video" },
    { ...(config.frames ? frameSettings : settings), name: "image_to_video", inputs: [image] },
    { ...referenceSettings, name: "reference_to_video", inputs: referenceInputs },
  ];
  if (config.frames) modes.push({ ...frameSettings, name: "first_last_frame", inputs: [image, { ...image, name: "last_frame", required: false }] });
  return { models: [model], modes };
}

export const meta = {
  apiVersion: 1,
  key: "tabtab",
  name: "TabTab Video",
  icon: "text:TT",
  version: "1.0.0",
  author: { name: "new-api contributors" },
  baseUrl: "https://api.tabtabapi.com",
  website: "https://tabtabapi.com/docs",
  description: { en: "TabTab asynchronous video generation with media references", zh: "TabTab 媒体参考异步视频生成" },
  models: Object.keys(MODELS),
  fetchMode: "per_task",
  protocols: ["openai_video"],
  usageSchema: {
    seconds: { type: "number", unit: "second", description: { en: "Video generation unit price", zh: "视频生成单价" } },
    resolution: { enum: ["360p", "480p", "720p", "1080p", "4k"], description: { en: "Output video resolution", zh: "输出视频分辨率" } },
    reference_video: { type: "boolean", description: { en: "Reference video present", zh: "存在参考视频" } },
  },
  videoProfiles: Object.entries(MODELS).map(([model, config]) => videoProfile(model, config)),
};

function aliasedValue(request, names, media = false) {
  let value;
  for (const name of names) {
    if (!Object.hasOwn(request, name)) continue;
    const candidate = media ? publicMediaURL(request[name]) : request[name];
    if (value !== undefined && String(value) !== String(candidate)) throw new Error(names.join(" and ") + " must match");
    value = candidate;
  }
  return value;
}

function publicMediaURL(value) {
  if (value && typeof value === "object" && !Array.isArray(value)) value = value.url || value.source;
  // oxlint-disable no-control-regex -- Reject ambiguous URLs before passing media to the provider
  if (typeof value !== "string" || !value || value.length > 8192 || /[\s\\\x00-\x1f\x7f]/.test(value))
    throw new Error("Media references must be public HTTP(S) URLs");
  // oxlint-enable no-control-regex
  const match = /^https?:\/\/([a-z0-9](?:[a-z0-9.-]*[a-z0-9])?)(?::([0-9]{1,5}))?(?:[/?#].*)?$/i.exec(value);
  if (!match || (match[2] && (Number(match[2]) < 1 || Number(match[2]) > 65535)))
    throw new Error("Media references must be public HTTP(S) URLs without credentials");
  const host = match[1].toLowerCase();
  if (/^(?:0x[0-9a-f]+|[0-9]+)(?:\.(?:0x[0-9a-f]+|[0-9]+)){0,3}$/i.test(host) && !/^[0-9.]+$/.test(host))
    throw new Error("Media references must not use alternate IP address spellings");
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
  if (!Object.hasOwn(MODELS, model)) throw new Error("Unsupported TabTab video model");
  const config = MODELS[model];
  if (!request || typeof request !== "object" || Array.isArray(request)) throw new Error("JSON object required");
  for (const name of Object.keys(request)) {
    if (!REQUEST_FIELDS.has(name)) throw new Error(name + " is not supported by TabTab video");
  }
  if (!config.frames && (request.start_frame !== undefined || request.end_frame !== undefined || request.last_frame !== undefined))
    throw new Error("This TabTab model does not support first and last frames");
  if (typeof request.prompt !== "string" || !request.prompt.trim()) throw new Error("prompt is required");
  const rawSeconds = aliasedValue(request, ["seconds", "duration"]);
  if (rawSeconds !== undefined && (!["string", "number"].includes(typeof rawSeconds) || (typeof rawSeconds === "string" && !rawSeconds.trim())))
    throw new Error("seconds must be an integer");
  const seconds = rawSeconds === undefined ? config.defaultSeconds : Number(rawSeconds);
  if (model === "wan3.0-video" && seconds === -1)
    throw new Error("Wan automatic duration is unavailable because the upstream does not report its billable duration");
  if (!Number.isInteger(seconds) || (config.durations ? !config.durations.includes(seconds) : seconds < config.min || seconds > config.max))
    throw new Error("seconds is outside this TabTab model's duration limits");
  const rawResolution = aliasedValue(request, ["resolution", "size"]);
  const resolution = rawResolution === undefined ? config.defaultResolution : typeof rawResolution === "string" ? rawResolution.toLowerCase() : "";
  if (!config.resolutions.includes(resolution)) throw new Error("resolution is not supported by this TabTab model");
  const images = [];
  const videos = [];
  const audios = [];
  for (const [names, destination, limit] of [
    [["reference_images", "images", "image_urls"], images, config.images],
    [["reference_videos", "videos", "video_urls"], videos, config.videos],
    [["reference_audios", "audios", "audio_urls"], audios, config.audios],
  ]) {
    for (const name of names) {
      if (request[name] === undefined) continue;
      if (model === "wan3.0-video" && destination === videos)
        throw new Error("Wan reference videos require billable reference durations that the upstream does not report");
      if (!Array.isArray(request[name]) || request[name].length > limit) throw new Error(name + " exceeds this TabTab model's reference limit");
      for (const value of request[name]) destination.push(publicMediaURL(value));
    }
  }
  if (request.video !== undefined) {
    if (model === "wan3.0-video") throw new Error("Wan reference videos require billable reference durations that the upstream does not report");
    videos.push(publicMediaURL(request.video));
  }
  let image = aliasedValue(request, ["image", "input_reference", "start_frame"], true);
  let lastFrame = aliasedValue(request, ["last_frame", "end_frame"], true);
  if (request.references !== undefined) {
    if (!Array.isArray(request.references) || request.references.length > config.total)
      throw new Error("references exceeds this TabTab model's reference limit");
    for (const reference of request.references) {
      if (!reference || typeof reference !== "object" || Array.isArray(reference) || !["image", "video", "audio"].includes(reference.type))
        throw new Error("Invalid media reference");
      if (model === "wan3.0-video" && reference.type === "video")
        throw new Error("Wan reference videos require billable reference durations that the upstream does not report");
      for (const name of Object.keys(reference)) {
        if (!["type", "role", "source"].includes(name)) throw new Error("Unsupported reference field: " + name);
      }
      if (reference.type === "image" && ["first_frame", "last_frame"].includes(reference.role)) {
        if (!config.frames) throw new Error("This TabTab model does not support first and last frames");
        if (reference.role === "first_frame") {
          if (image !== undefined) throw new Error("The first frame is specified more than once");
          image = reference.source;
        } else {
          if (lastFrame !== undefined) throw new Error("The last frame is specified more than once");
          lastFrame = reference.source;
        }
        continue;
      }
      if (reference.role !== undefined && !["reference", "reference_" + reference.type].includes(reference.role))
        throw new Error("Unsupported media reference role");
      (reference.type === "image" ? images : reference.type === "video" ? videos : audios).push(publicMediaURL(reference.source));
    }
  }
  if (!config.frames && image !== undefined && (images.length || videos.length || audios.length)) {
    images.unshift(publicMediaURL(image));
    image = undefined;
  }
  if (
    images.length > config.images ||
    videos.length > config.videos ||
    audios.length > config.audios ||
    images.length + videos.length + audios.length > config.total
  )
    throw new Error("Media references exceed this TabTab model's limits");
  if (lastFrame !== undefined && image === undefined) throw new Error("end_frame requires start_frame");
  if (lastFrame !== undefined && !config.frames) throw new Error("This TabTab model does not support first and last frames");
  if (image !== undefined && config.frames && (images.length || videos.length || audios.length))
    throw new Error("First and last frames cannot be combined with media references");
  if (image !== undefined && !config.frames && images.length >= config.images) throw new Error("Media references exceed this TabTab model's limits");
  if (audios.length && !images.length && !videos.length && !config.audioOnly) throw new Error("Audio references require an image or video reference");
  if (image !== undefined) image = publicMediaURL(image);
  if (lastFrame !== undefined) lastFrame = publicMediaURL(lastFrame);
  let ratio = aliasedValue(request, ["aspect_ratio", "ratio"]);
  const adaptive = model === "kling-4.0" || model === "wan3.0-video";
  if (ratio === undefined) ratio = adaptive && (image !== undefined || images.length || videos.length || audios.length) ? "auto" : "16:9";
  if (!config.ratios.includes(ratio)) throw new Error("aspect_ratio is not supported by this TabTab model");
  if (adaptive && image !== undefined && ratio !== "auto") throw new Error("This TabTab model's start_frame requires aspect_ratio=auto");
  const body = { model, prompt: request.prompt.trim(), seconds, resolution, aspect_ratio: ratio };
  if (image !== undefined) body.image = image;
  if (lastFrame !== undefined) body.last_frame = lastFrame;
  if (images.length) body.reference_images = images;
  if (videos.length) body.reference_videos = videos;
  if (audios.length) body.reference_audios = audios;
  let action = "text_to_video";
  if (lastFrame !== undefined) action = "first_last_frame";
  else if (image !== undefined && !images.length && !videos.length && !audios.length) action = "image_to_video";
  else if (image !== undefined || images.length || videos.length || audios.length) action = "reference_to_video";
  return { body, action };
}

export function buildSubmitRequest(ctx) {
  const model = ctx.upstreamModel || ctx.model;
  const request = normalizeRequest(ctx.requestBody, model).body;
  const args = { prompt: request.prompt, duration: request.seconds, resolution: request.resolution, ratio: request.aspect_ratio };
  if (request.reference_images) args.images = request.reference_images;
  if (request.reference_videos) {
    if (MODELS[model].singleVideo) args.video = request.reference_videos[0];
    else args.videos = request.reference_videos;
  }
  if (request.reference_audios) args.audios = request.reference_audios;
  if (request.image) {
    if (MODELS[model].frames) args.start_frame = request.image;
    else args.images = [request.image, ...(args.images || [])];
  }
  if (request.last_frame) args.end_frame = request.last_frame;
  return {
    method: "POST",
    url: ctx.baseUrl.replace(/\/+$/, "") + "/api/task/create",
    headers: { "Content-Type": "application/json", Authorization: "Bearer " + ctx.apiKey },
    body: { model, args },
  };
}

export function extractUsage(ctx) {
  const body = normalizeRequest(ctx.requestBody, ctx.upstreamModel || ctx.model).body;
  if (ctx.usagePurpose === "billing_ratios") return { seconds: body.seconds };
  return { seconds: body.seconds, resolution: body.resolution, reference_video: !!body.reference_videos };
}

function taskFailureReason(data, ctx, fallback) {
  let reason = "";
  if (data && typeof data === "object" && !Array.isArray(data)) {
    const code = data.error_code || data.code;
    if (typeof code === "string" && code !== "OK" && /^[A-Za-z0-9_.:-]{1,128}$/.test(code)) reason = code;
    const message = data.error_message || data.message;
    if (typeof message === "string" && message.trim() && !["{", "["].includes(message.trim()[0])) {
      reason += (reason ? ": " : "") + message.trim();
    }
  }
  if (!reason) return fallback;
  for (const secret of [ctx && ctx.apiKey, ctx && ctx.authHeader]) {
    if (typeof secret === "string" && secret) reason = reason.split(secret).join("[redacted]");
  }
  return reason
    .replace(/Bearer\s+[^\s,;]+/gi, "Bearer [redacted]")
    .replace(/\bsk[_-][A-Za-z0-9_-]+/g, "[redacted]")
    .replace(/\b(api[_ -]?key|authorization|access[_ -]?token|secret|password)\s*[:=]\s*["']?[^\s,;"'}]+/gi, "$1: [redacted]")
    .replace(/https?:\/\/[^\s<>"']+/gi, "[redacted URL]")
    .replace(/[\r\n\t]/g, " ")
    .slice(0, 512);
}

function taskData(body, ctx) {
  if (!body || typeof body !== "object" || Array.isArray(body) || body.code !== "OK" || !body.data || typeof body.data !== "object" || Array.isArray(body.data))
    throw new Error(taskFailureReason(body, ctx, "TabTab returned an unsuccessful task response"));
  return body.data;
}

function isClosedTaskFailure(body) {
  for (const name of Object.keys(body)) {
    if (!["code", "data", "message", "request_id"].includes(name)) return false;
  }
  const data = body.data;
  for (const name of Object.keys(data)) {
    if (!["id", "model", "status", "charged_points", "results", "error_code", "error_message", "generation_time_ms"].includes(name)) return false;
  }
  return data.results === undefined || data.results === null || (Array.isArray(data.results) && data.results.length === 0);
}

export function parseSubmitResponse(ctx, response) {
  const body = response.body;
  // A contradictory envelope must not erase an accepted operation identifier.
  // Retain that barrier and let polling resolve the authoritative task state.
  const accepted =
    body &&
    typeof body === "object" &&
    !Array.isArray(body) &&
    body.data &&
    typeof body.data === "object" &&
    !Array.isArray(body.data) &&
    typeof body.data.id === "string" &&
    body.data.id;
  const data = accepted ? body.data : taskData(body, ctx);
  // oxlint-disable no-control-regex -- Task identifiers cannot contain control characters
  if (typeof data.id !== "string" || !data.id || data.id !== data.id.trim() || data.id.length > 191 || /[\x00-\x1f\x7f]/.test(data.id))
    throw new Error("TabTab task id is missing or invalid");
  // oxlint-enable no-control-regex
  const request = normalizeRequest(ctx.requestBody, ctx.upstreamModel || ctx.model).body;
  const receipt = {
    taskId: data.id,
    taskData: response.body,
    state: { seconds: request.seconds, resolution: request.resolution, reference_video: !!request.reference_videos },
  };
  if (body.code === "OK" && data.status === "failed" && isClosedTaskFailure(body)) {
    receipt.immediate = { status: "FAILURE", reason: taskFailureReason(data, ctx, "TabTab video generation failed") };
  }
  // The create contract returns processing. Keep a valid accepted task id for
  // polling even when a future response adds or omits its initial status.
  return receipt;
}

export function buildQueryRequest(ctx) {
  return {
    method: "GET",
    url: ctx.baseUrl.replace(/\/+$/, "") + "/api/task/status?id=" + encodeURIComponent(ctx.taskId),
    headers: { Authorization: "Bearer " + ctx.apiKey },
  };
}

function resultURL(data, baseURL) {
  if (!Array.isArray(data.results) || data.results.length !== 1) throw new Error("TabTab completed video result is missing");
  const result = data.results[0];
  const value = result && result.url;
  // oxlint-disable no-control-regex -- The host performs download URL and DNS validation
  if (typeof value !== "string" || !value || value.length > 8192 || /[\s\\\x00-\x1f\x7f#]/.test(value)) throw new Error("TabTab video result URL is invalid");
  // oxlint-enable no-control-regex
  if (result.mime_type !== undefined && result.mime_type !== "video/mp4") throw new Error("TabTab returned an unsupported video result type");
  if (/^\/files\//.test(value)) {
    const origin = /^https?:\/\/[^/?#@]+/i.exec(baseURL || "");
    if (!origin) throw new Error("TabTab channel origin is invalid");
    return origin[0] + value;
  }
  return publicMediaURL(value);
}

export function parseTaskResult(ctx, body) {
  const data = taskData(body, ctx);
  if (ctx.taskId && data.id !== ctx.taskId) throw new Error("TabTab returned a different or missing task id");
  const statuses = { processing: "IN_PROGRESS", succeeded: "SUCCESS", failed: "FAILURE" };
  if (typeof data.status !== "string" || !Object.hasOwn(statuses, data.status)) return { status: "UNKNOWN", reason: "Unrecognized TabTab task status" };
  if (data.status !== "succeeded" && data.results !== undefined && data.results !== null && (!Array.isArray(data.results) || data.results.length > 0))
    throw new Error("TabTab non-success task contains contradictory output");
  if (data.status === "failed" && !isClosedTaskFailure(body)) throw new Error("TabTab failed task contains unrecognized execution evidence");
  const result = { status: statuses[data.status] };
  if (result.status === "FAILURE") result.reason = taskFailureReason(data, ctx, "TabTab video generation failed");
  if (result.status === "SUCCESS") result.url = resultURL(data, ctx.baseUrl);
  return result;
}

export function extractUsageOnComplete() {
  // TabTab's charged_points are a separate provider ledger. Settlement keeps
  // the validated submission facts and never converts those points to quota.
  return {};
}

export function listArtifacts(task) {
  return task.status === "SUCCESS" ? [{ key: "video", type: "video", mimeType: "video/mp4" }] : [];
}

export function buildContentRequest(ctx) {
  if (ctx.artifactKey !== "video") throw new Error("artifact_not_found");
  return { method: ctx.clientRequest.method, url: resultURL(taskData(ctx.data, ctx), ctx.baseUrl), credentialless: true };
}

export const protocols = {
  openai_video: {
    decodeRequest(ctx) {
      if (!ctx.body || ctx.body.kind !== "json") throw new Error("TabTab video requires JSON with public media URLs");
      if (ctx.operation && ctx.operation !== "create") throw new Error("TabTab video supports generation only");
      const normalized = normalizeRequest(ctx.body.value, ctx.upstreamModel || ctx.model);
      normalized.body.model = ctx.model;
      return { kind: "submit", model: ctx.model, action: normalized.action, requestBody: normalized.body };
    },
    render(_ctx, task) {
      return task.fail_reason ? { error: { message: task.fail_reason } } : {};
    },
  },
};
