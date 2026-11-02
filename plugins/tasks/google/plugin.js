const MODELS_30 = ["veo-3.0-generate-001", "veo-3.0-fast-generate-001"];
const MODELS_31 = ["veo-3.1-generate-preview", "veo-3.1-fast-generate-preview"];

function videoModes(duration, resolutions) {
  const dimensions = {
    duration,
    resolutions,
    defaultResolution: "720p",
    aspectRatios: ["16:9", "9:16"],
    defaultAspectRatio: "16:9",
  };
  return [
    { name: "text_to_video", ...dimensions },
    { name: "image_to_video", ...dimensions, inputs: [{ name: "image", kind: "image", sources: ["data_uri", "upload"], maxItems: 1, required: true }] },
  ];
}

export const meta = {
  videoRetryInputs: [
    {"path": "/image", "kind": "image"},
    {"path": "/input_reference", "kind": "image"},
    {"path": "/images/*", "kind": "image"},
  ],
  apiVersion: 1,
  key: "google",
  name: "Google Veo (Gemini API)",
  icon: "Gemini.Color",
  description: {
    en: "Google Veo video generation on the Gemini API (text-to-video and image-to-video)",
    zh: "Google Veo 视频生成（文生视频、图生视频），Gemini API 版本",
  },
  version: "1.0.5",
  author: { name: "QuantumNous" },
  channelTypes: [24],
  models: [...MODELS_30, ...MODELS_31],
  videoProfiles: [
    { models: MODELS_30, modes: videoModes({ values: [8], default: 8 }, ["720p", "1080p"]) },
    { models: MODELS_31, modes: videoModes({ values: [4, 6, 8], default: 8 }, ["720p", "1080p", "4k"]) },
  ],
  fetchMode: "per_task",
  usageSchema: {
    // Veo 3.0 uses eight seconds; Veo 3.1 supports four, six, or eight.
    seconds: {
      type: "number",
      unit: "second",
      description: { en: "Video generation unit price", zh: "视频生成单价" },
    },
    // Requested video output resolution. Veo prices differ per resolution tier.
    resolution: {
      enum: ["720p", "1080p", "4k"],
      enumLabels: { "720p": { en: "720p", zh: "720p" }, "1080p": { en: "1080p", zh: "1080p" }, "4k": { en: "4k", zh: "4k" } },
      description: { en: "Output video resolution", zh: "输出视频分辨率" },
    },
  },
  usageExamples: [
    { label: "8s × 720p", facts: { seconds: 8, resolution: "720p" } },
    { label: "8s × 1080p", facts: { seconds: 8, resolution: "1080p" } },
    { label: "8s × 4k", facts: { seconds: 8, resolution: "4k" } },
    { label: "4s × 720p", facts: { seconds: 4, resolution: "720p" } },
  ],
  protocols: [{ name: "openai_responses", supports: ["stream", "sync", "background"] }, "openai_video"],
};

function trimmed(value) {
  return String(value || "").trim();
}

function responsesInput(req) {
  const texts = [],
    images = [];
  const input = req.input;
  if (typeof input === "string") texts.push(input);
  else if (Array.isArray(input)) {
    for (const item of input) {
      if (typeof item === "string") {
        texts.push(item);
        continue;
      }
      if (!item || typeof item !== "object" || Array.isArray(item)) continue;
      const content = item.content === undefined ? [item] : Array.isArray(item.content) ? item.content : [item.content];
      for (const part of content) {
        if (typeof part === "string") {
          texts.push(part);
          continue;
        }
        if (!part || typeof part !== "object" || Array.isArray(part)) continue;
        if (["input_text", "text"].includes(part.type) && typeof part.text === "string") texts.push(part.text);
        if (["input_image", "image_url"].includes(part.type)) {
          let image = part.image_url;
          if (image && typeof image === "object") image = image.url;
          if (trimmed(image)) images.push(trimmed(image));
        }
      }
    }
  }
  return {
    prompt: texts
      .filter(function (text) {
        return trimmed(text);
      })
      .join("\n"),
    images: images,
  };
}

function responsesVideoText(ctx) {
  const artifact = ctx && ctx.artifacts && ctx.artifacts.video;
  const url = trimmed(artifact && artifact.url);
  if (!url) throw new Error("video artifact is unavailable");
  const escaped = url.replace(/&/g, "&amp;").replace(/"/g, "&quot;").replace(/</g, "&lt;").replace(/>/g, "&gt;");
  return '<video controls src="' + escaped + '"></video>';
}

function sizeParts(size) {
  const parts = String(size || "")
    .toLowerCase()
    .split("x");
  if (parts.length !== 2) return null;
  return [Number(parts[0]), Number(parts[1])];
}

function resolutionForSize(size) {
  const resolution = String(size || "").toLowerCase();
  if (["720p", "1080p", "4k"].includes(resolution)) return resolution;
  const parts = sizeParts(size);
  if (!parts) return "720p";
  const max = Math.max(parts[0], parts[1]);
  if (max >= 3840) return "4k";
  if (max >= 1920) return "1080p";
  return "720p";
}

function aspectForSize(size) {
  const parts = sizeParts(size);
  if (!parts || parts[0] <= 0 || parts[1] <= 0) return "16:9";
  return parts[1] > parts[0] ? "9:16" : "16:9";
}

function imageInput(value, files) {
  if (value && typeof value === "object" && !Array.isArray(value) && value.__fileRef) {
    let mime = value.mimeType || "";
    if (!mime && files) {
      for (const file of files) {
        if (file.ref === value.__fileRef || file.field === "input_reference") {
          mime = file.mimeType || "";
          break;
        }
      }
    }
    return { inlineData: { mimeType: mime || "application/octet-stream", data: value } };
  }
  value = String(value || "").trim();
  if (!value) return null;
  if (value.startsWith("data:")) {
    const comma = value.indexOf(",");
    if (comma < 0 || !value.slice(comma + 1)) return null;
    const mediaType = value.slice(5, comma).split(";")[0];
    return { inlineData: { mimeType: mediaType || "application/octet-stream", data: value.slice(comma + 1) } };
  }
  // Raw base64 input is accepted by the Go adaptor. The common fixtures use
  // PNG data; browser-free plugins cannot invoke net/http DetectContentType.
  if (!/^[A-Za-z0-9+/]+={0,2}$/.test(value)) return null;
  let mime = "application/octet-stream";
  if (value.startsWith("iVBORw0KGgo")) mime = "image/png";
  else if (value.startsWith("/9j/")) mime = "image/jpeg";
  else if (value.startsWith("R0lGOD")) mime = "image/gif";
  else if (value.startsWith("UklGR")) mime = "image/webp";
  return { inlineData: { mimeType: mime, data: value } };
}

function videoParameters(req, model) {
  if (req.metadata !== undefined && (!req.metadata || typeof req.metadata !== "object" || Array.isArray(req.metadata)))
    throw new Error("metadata must be an object");
  const params = Object.assign({}, req.metadata || {});
  if (req.seconds !== undefined && req.duration !== undefined && Number(req.seconds) !== Number(req.duration))
    throw new Error("seconds and duration must match");
  const durations = [req.seconds, req.duration, req.durationSeconds, params.seconds, params.duration, params.durationSeconds].filter((value) => value !== undefined);
  const seconds = durations.length ? Number(durations[0]) : 8;
  if (durations.some((value) => Number(value) !== seconds)) throw new Error("video duration aliases must match");
  const classic = MODELS_30.includes(model);
  if (!(classic ? [8] : [4, 6, 8]).includes(seconds))
    throw new Error(classic ? "seconds must be 8 for Veo 3.0" : "seconds must be one of 4, 6, or 8");
  const resolution = String(req.resolution || params.resolution || resolutionForSize(req.size)).toLowerCase();
  if (!(classic ? ["720p", "1080p"] : ["720p", "1080p", "4k"]).includes(resolution))
    throw new Error(classic ? "resolution must be 720p or 1080p for Veo 3.0" : "resolution must be one of 720p, 1080p, or 4k");
  const aspectRatio = trimmed(req.aspect_ratio || req.aspectRatio || req.ratio || params.aspectRatio || aspectForSize(req.size));
  if (aspectRatio !== "16:9" && aspectRatio !== "9:16") throw new Error("aspect_ratio must be 16:9 or 9:16");
  // Gemini's higher-resolution generation requires the full eight-second clip.
  if (resolution !== "720p" && seconds !== 8) throw new Error("1080p and 4k resolution require 8 seconds");
  if (classic && resolution === "1080p" && aspectRatio !== "16:9") throw new Error("Veo 3.0 1080p requires aspect_ratio 16:9");
  params.durationSeconds = seconds;
  delete params.seconds;
  delete params.duration;
  params.resolution = resolution;
  params.aspectRatio = aspectRatio;
  params.numberOfVideos = 1;
  return params;
}

function converted(ctx) {
  const req = ctx.requestBody || {};
  const params = videoParameters(req, ctx.upstreamModel || ctx.model);
  const instance = { prompt: req.prompt };
  const image = imageInput((req.images || [])[0] || req.image || req.input_reference, ctx.files);
  if (image) instance.image = image;
  return { body: { instances: [instance], parameters: params }, action: image ? "image_to_video" : "text_to_video" };
}

function version(ctx) {
  const settings = ctx.userSetting || {};
  return settings.geminiVersion || "v1beta";
}

export function buildSubmitRequest(ctx) {
  const result = converted(ctx);
  return {
    url: ctx.baseUrl + "/" + version(ctx) + "/models/" + (ctx.upstreamModel || ctx.model) + ":predictLongRunning",
    method: "POST",
    headers: { "Content-Type": "application/json", Accept: "application/json", "x-goog-api-key": ctx.apiKey },
    body: result.body,
    action: result.action,
  };
}

export function parseSubmitResponse(ctx, resp) {
  const body = resp.body || {};
  if (!String(body.name || "").trim()) throw new Error("missing operation name");
  const result = { taskId: utils.base64URL(body.name), taskData: body };
  if (body.done && !(body.error && body.error.message)) {
    const videos = ((body.response || {}).generateVideoResponse || {}).generatedVideos || [];
    const uri = videos.length && videos[0].video ? videos[0].video.uri || "" : "";
    result.immediate = { taskId: result.taskId, status: "SUCCESS", progress: "100%", remoteUrl: uri };
  }
  return result;
}

export function extractUsage(ctx) {
  const params = videoParameters(ctx.requestBody || {}, ctx.upstreamModel || ctx.model);
  return { seconds: params.durationSeconds, resolution: params.resolution };
}

export function extractUsageOnComplete() {
  return null;
}

export function buildQueryRequest(ctx) {
  return {
    url: ctx.baseUrl + "/v1beta/" + utils.base64URLDecode(ctx.taskId),
    method: "GET",
    headers: { Accept: "application/json", "x-goog-api-key": ctx.apiKey },
  };
}

export function parseTaskResult(ctx, body) {
  if (body.error && body.error.message) return { status: "FAILURE", progress: "100%", reason: body.error.message };
  // Google long-running operations omit `done` (proto3 default) while still
  // running, so a missing key means in-progress; only a non-operation shape is
  // unrecognized.
  if (!body || typeof body !== "object" || !String(body.name || "").trim()) return { status: "UNKNOWN", reason: "unrecognized operation state" };
  if (body.done !== true) return { status: "IN_PROGRESS", progress: "50%" };
  const videos = ((body.response || {}).generateVideoResponse || {}).generatedVideos || [];
  const uri = videos.length && videos[0].video ? videos[0].video.uri || "" : "";
  return { taskId: utils.base64URL(body.name || ""), status: "SUCCESS", progress: "100%", remoteUrl: uri };
}

function artifactData(ctx) {
  const data = (ctx && ctx.data) || {};
  if (data.data && typeof data.data === "object" && data.data.task_id && Object.prototype.hasOwnProperty.call(data.data, "data")) return data.data.data || {};
  return data;
}

function artifactVideos(ctx) {
  const videos = ((artifactData(ctx).response || {}).generateVideoResponse || {}).generatedVideos || [];
  return videos.filter((item) => item.video && item.video.uri);
}

export function listArtifacts(task) {
  if (task.status !== "SUCCESS") return [];
  return artifactVideos(task).map((item, index) => ({ key: index === 0 ? "video" : "video-" + index, type: "video", mimeType: "video/mp4" }));
}

export function buildContentRequest(ctx) {
  const videos = artifactVideos(ctx);
  const index = videos.findIndex((item, position) => (position === 0 ? "video" : "video-" + position) === ctx.artifactKey);
  if (index < 0) throw new Error("artifact_not_found");
  const url = String(videos[index].video.uri).trim();
  if (!url) throw new Error("artifact_not_found");
  return { url: url, method: ctx.clientRequest.method, headers: { "x-goog-api-key": ctx.apiKey } };
}

export const protocols = {
  openai_responses: {
    decodeRequest: function (ctx) {
      if (!ctx.body || ctx.body.kind !== "json") throw new Error("JSON body required");
      const req = ctx.body.value;
      if (!req || typeof req !== "object" || Array.isArray(req)) throw new Error("request body must be an object");
      const model = trimmed(ctx.model || req.model);
      if (!model) throw new Error("model is required");
      if (req.input !== undefined && typeof req.input !== "string" && !Array.isArray(req.input)) throw new Error("input must be a string or array");
      if (req.images !== undefined && !Array.isArray(req.images)) throw new Error("images must be an array");
      if (req.metadata !== undefined && (!req.metadata || typeof req.metadata !== "object" || Array.isArray(req.metadata)))
        throw new Error("metadata must be an object");
      const input = responsesInput(req);
      const prompt = input.prompt || trimmed(req.prompt);
      const images = [];
      for (const image of [req.image, req.input_reference].concat(req.images || [], input.images)) {
        if (trimmed(image) && !images.includes(trimmed(image))) images.push(trimmed(image));
      }
      if (!prompt && images.length === 0) throw new Error("input is required");
      if (images.length && !imageInput(images[0])) throw new Error("input image must be a data URL or base64 value");
      const metadata = Object.assign({}, req.metadata || {});
      if (Object.prototype.hasOwnProperty.call(req, "resolution")) metadata.resolution = req.resolution;
      const requestBody = { model: model, prompt: prompt, metadata: metadata };
      if (images.length) requestBody.images = images;
      if (Object.prototype.hasOwnProperty.call(req, "seconds")) requestBody.duration = req.seconds;
      else if (Object.prototype.hasOwnProperty.call(req, "duration")) requestBody.duration = req.duration;
      if (Object.prototype.hasOwnProperty.call(req, "size")) requestBody.size = req.size;
      for (const name of ["aspect_ratio", "aspectRatio", "ratio"]) {
        if (Object.prototype.hasOwnProperty.call(req, name)) requestBody[name] = req[name];
      }
      videoParameters(requestBody, ctx.upstreamModel || ctx.model || model);
      return { kind: "submit", model: model, action: images.length ? "image_to_video" : "text_to_video", requestBody: requestBody };
    },
    renderEvents: function (ctx, task, previousState) {
      const status = String(task.status || "UNKNOWN").toUpperCase();
      const value = Number(String(task.progress || "").replace("%", ""));
      const progress = Number.isFinite(value) && value >= 0 && value <= 100 ? value : null;
      const state = { status: status, progress: progress };
      if (status === "SUCCESS") {
        const text = responsesVideoText(ctx);
        const events = previousState && previousState.status === status ? [] : [{ type: "output", data: text }];
        return { events: events, state: state, done: true };
      }
      if (status === "FAILURE")
        return { events: [{ type: "error", code: "task_failed", message: task.fail_reason || "task failed" }], state: state, done: true };
      if (previousState && previousState.status === status && previousState.progress === progress) return { events: [], state: state, done: false };
      const event = { type: "progress", message: status.toLowerCase() };
      if (progress !== null) event.progress = progress;
      return { events: [event], state: state, done: false };
    },
    renderFinal: function (ctx, _task) {
      return {
        output: [
          {
            type: "message",
            status: "completed",
            role: "assistant",
            content: [{ type: "output_text", text: responsesVideoText(ctx), annotations: [], logprobs: [] }],
          },
        ],
        metadata: { vendor: "gemini" },
      };
    },
  },
};

const legacyRenderers = {
  openai_video: function (task) {
    const model = (task.properties || {}).origin_model_name || "veo-3.0-generate-001";
    const statuses = { SUBMITTED: "queued", QUEUED: "queued", IN_PROGRESS: "in_progress", SUCCESS: "completed", FAILURE: "failed" };
    const output = {
      id: task.task_id,
      object: "video",
      model: model,
      status: statuses[task.status] || "unknown",
      progress: Number(String(task.progress || "0").replace("%", "")),
      created_at: task.created_at,
    };
    if (Number(task.finish_time) > 0) output.completed_at = Number(task.finish_time);
    else if (Number(task.updated_at) > 0) output.completed_at = Number(task.updated_at);
    return output;
  },
};

protocols.openai_video = {
  decodeRequest: function (ctx) {
    if (!ctx.body || (ctx.body.kind !== "json" && ctx.body.kind !== "multipart")) throw new Error("JSON or multipart body required");
    let req;
    let inputReferenceFile = null;
    if (ctx.body.kind === "json") {
      if (!ctx.body.value || Array.isArray(ctx.body.value)) throw new Error("JSON object required");
      req = Object.assign({}, ctx.body.value);
    } else {
      const first = function (name) {
        const values = (ctx.body.fields || {})[name] || [];
        if (values.length > 1) throw new Error(name + " must be provided once");
        return values[0];
      };
      req = {};
      const fields = ctx.body.fields || {};
      for (const name of Object.keys(fields)) {
        req[name] = first(name);
      }
      for (const file of ctx.body.files || []) {
        if (file.field !== "input_reference" && file.field !== "image") throw new Error("unexpected file field: " + file.field);
        if (inputReferenceFile) throw new Error("image or input_reference must be provided once");
        if (!trimmed(file.ref)) throw new Error("uploaded image reference is missing");
        inputReferenceFile = file;
      }
      if (req.metadata !== undefined) {
        let parsed;
        try {
          parsed = JSON.parse(req.metadata);
        } catch {
          throw new Error("metadata must be a JSON object string");
        }
        if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) throw new Error("metadata must be a JSON object string");
        req.metadata = parsed;
      }
      if (req.seconds !== undefined) req.seconds = Number(req.seconds);
      else if (req.duration !== undefined) req.seconds = Number(req.duration);
    }
    const params = videoParameters(req, ctx.upstreamModel || ctx.model);
    req.duration = params.durationSeconds;
    req.resolution = params.resolution;
    req.aspect_ratio = params.aspectRatio;
    if (inputReferenceFile) {
      req.images = [{ __fileRef: inputReferenceFile.ref, encoding: "base64", maxBytes: 20971520 }];
    }
    return {
      kind: "submit",
      model: ctx.model,
      action: inputReferenceFile || req.input_reference || req.image || (req.images || []).length ? "image_to_video" : "text_to_video",
      requestBody: Object.assign({}, req, { model: ctx.model }),
    };
  },
  render: function (ctx, task) {
    return legacyRenderers.openai_video(task);
  },
};
