const MODEL = "L-sd2-video";
const RESOLUTIONS = ["480p", "720p", "1080p"];
const ASPECT_RATIOS = ["16:9", "9:16", "4:3", "3:4", "1:1"];
// These are the host safety ceilings, not verified vendor model capabilities.
// Administrators must narrow the profile to the model's published limits.
const MAX_VIDEO_SECONDS = 3600;
const MAX_REFERENCES = 64;
const RESOLUTION_SCHEMA = { enum: RESOLUTIONS, description: { en: "Output video resolution", zh: "输出视频分辨率" } };
const SECOND_USAGE_SCHEMA = {
  seconds: { type: "number", unit: "second", description: { en: "Video generation unit price", zh: "视频生成单价" } },
  resolution: RESOLUTION_SCHEMA,
};
const REQUEST_USAGE_SCHEMA = {
  requests: { type: "number", unit: "count", unitLabel: { en: "video", zh: "条" }, description: { en: "Video generation unit price", zh: "视频生成单价" } },
  resolution: RESOLUTION_SCHEMA,
};
const VIDEO_DIMENSIONS = {
  duration: { min: 1, max: MAX_VIDEO_SECONDS, step: 1, default: 5 },
  resolutions: RESOLUTIONS,
  defaultResolution: "720p",
  aspectRatios: ASPECT_RATIOS,
  defaultAspectRatio: "16:9",
};
const REQUEST_FIELDS = new Set([
  "model",
  "mode",
  "prompt",
  "seconds",
  "duration",
  "video_duration",
  "resolution",
  "size",
  "aspect_ratio",
  "ratio",
  "image",
  "input_reference",
  "images",
  "image_urls",
  "reference_images",
  "audio_urls",
  "reference_audios",
  "video_urls",
  "reference_videos",
]);

export const meta = {
  videoRetryInputs: [
    { path: "/image", kind: "image" },
    { path: "/input_reference", kind: "image" },
    { path: "/images/*", kind: "image" },
    { path: "/reference_images/*", kind: "image" },
    { path: "/image_urls/*", kind: "image" },
    { path: "/reference_videos/*", kind: "video" },
    { path: "/video_urls/*", kind: "video" },
    { path: "/reference_audios/*", kind: "audio" },
    { path: "/audio_urls/*", kind: "audio" },
  ],
  apiVersion: 1,
  key: "tianyue",
  name: "Tianyue Video",
  icon: "text:TY",
  version: "1.0.2",
  author: { name: "new-api contributors" },
  baseUrl: "https://api.tianyue.xyz",
  website: "https://ai.tianyue.xyz/docs/tianyue-api.html#video-create",
  description: { en: "Tianyue asynchronous video generation with public media references", zh: "天悦公网媒体参考异步视频生成" },
  // Only the concrete model shown in the upstream documentation is registered.
  // A real per-second model must be confirmed before adding it to this list.
  models: [MODEL],
  fetchMode: "per_task",
  protocols: ["openai_video"],
  usageSchema: SECOND_USAGE_SCHEMA,
  usageProfiles: [{ models: [MODEL], schema: REQUEST_USAGE_SCHEMA }],
  videoProfiles: [
    {
      models: [MODEL],
      modes: [
        { ...VIDEO_DIMENSIONS, name: "text_to_video" },
        { ...VIDEO_DIMENSIONS, name: "image_to_video", inputs: [{ name: "image", kind: "image", sources: ["url"], maxItems: 1, required: true }] },
        {
          ...VIDEO_DIMENSIONS,
          name: "reference_to_video",
          inputs: [
            { name: "reference_images", kind: "image", sources: ["url"], maxItems: MAX_REFERENCES },
            { name: "reference_audios", kind: "audio", sources: ["url"], maxItems: MAX_REFERENCES },
            { name: "reference_videos", kind: "video", sources: ["url"], maxItems: MAX_REFERENCES },
          ],
        },
      ],
    },
  ],
};

function aliasedValue(request, names) {
  let value;
  for (const name of names) {
    if (!Object.hasOwn(request, name)) continue;
    if (value !== undefined && String(value) !== String(request[name])) throw new Error(names.join(" and ") + " must match");
    value = request[name];
  }
  return value;
}

function videoSeconds(value) {
  if (!["number", "string"].includes(typeof value) || (typeof value === "string" && !value.trim())) throw new Error("seconds must be a positive integer");
  const seconds = Number(value);
  if (!Number.isInteger(seconds) || seconds < 1 || seconds > MAX_VIDEO_SECONDS) throw new Error("seconds must be between 1 and " + MAX_VIDEO_SECONDS);
  return seconds;
}

function publicMediaURL(value) {
  if (typeof value !== "string" || !value || value.length > 8192) throw new Error("Media references must be public HTTP(S) URLs");
  // oxlint-disable no-control-regex -- Reject controls and URL parser ambiguities
  if (/[\s\\\x00-\x1f\x7f]/.test(value)) throw new Error("Media references must be public HTTP(S) URLs");
  // oxlint-enable no-control-regex
  const match = /^https?:\/\/([a-z0-9](?:[a-z0-9.-]*[a-z0-9])?)(?::([0-9]{1,5}))?(?:[/?#].*)?$/i.exec(value);
  if (!match || (match[2] && (Number(match[2]) < 1 || Number(match[2]) > 65535)))
    throw new Error("Media references must be public HTTP(S) URLs without credentials");
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
  // oxlint-disable no-control-regex -- Match the host's model identity rules
  if (typeof model !== "string" || !model || model !== model.trim() || /[\x00-\x1f\x7f-\x9f,*]/.test(model)) throw new Error("Invalid Tianyue video model");
  // oxlint-enable no-control-regex
  let modelBytes = 0;
  for (const character of model) {
    const codePoint = character.codePointAt(0);
    modelBytes += codePoint <= 0x7f ? 1 : codePoint <= 0x7ff ? 2 : codePoint <= 0xffff ? 3 : 4;
    if (modelBytes > 255) throw new Error("Tianyue video model must not exceed 255 UTF-8 bytes");
  }
  if (!request || typeof request !== "object" || Array.isArray(request)) throw new Error("JSON object required");
  for (const name of Object.keys(request)) {
    if (!REQUEST_FIELDS.has(name)) throw new Error(name + " is not supported by Tianyue video");
  }
  if (request.mode !== undefined && (model === MODEL || !["text_to_video", "image_to_video", "reference_to_video"].includes(request.mode)))
    throw new Error("Unsupported Tianyue video mode");
  if (typeof request.prompt !== "string" || !request.prompt.trim()) throw new Error("prompt is required");
  let rawSeconds = aliasedValue(request, ["seconds", "video_duration"]);
  if (request.video_duration === undefined) rawSeconds = aliasedValue(request, ["seconds", "duration"]);
  const seconds = videoSeconds(rawSeconds);
  if (request.video_duration !== undefined && request.duration !== undefined && videoSeconds(request.duration) !== (model.endsWith("-se") ? seconds : 1))
    throw new Error("duration conflicts with the Tianyue billing mode");
  const rawResolution = aliasedValue(request, ["resolution", "size"]);
  const resolution = typeof rawResolution === "string" ? (model === MODEL ? rawResolution.toLowerCase() : rawResolution) : "";
  if (model !== MODEL ? !/^[A-Za-z0-9][A-Za-z0-9_.-]{0,31}$/.test(resolution) : !RESOLUTIONS.includes(resolution))
    throw new Error("Unsupported Tianyue video resolution");
  const rawRatio = aliasedValue(request, ["aspect_ratio", "ratio"]);
  if (model !== MODEL && rawRatio === undefined) throw new Error("aspect_ratio is required for a channel-configured Tianyue model");
  const aspectRatio = rawRatio === undefined ? "16:9" : rawRatio;
  if (
    model !== MODEL
      ? typeof aspectRatio !== "string" || (!/^[1-9][0-9]{0,2}:[1-9][0-9]{0,2}$/.test(aspectRatio) && !["auto", "adaptive"].includes(aspectRatio))
      : !ASPECT_RATIOS.includes(aspectRatio)
  )
    throw new Error("aspect_ratio is not supported by Tianyue video");
  const body = { model, prompt: request.prompt.trim(), seconds, resolution, aspect_ratio: aspectRatio };
  for (const [names, target] of [
    [["reference_images", "images", "image_urls"], "reference_images"],
    [["reference_audios", "audio_urls"], "reference_audios"],
    [["reference_videos", "video_urls"], "reference_videos"],
  ]) {
    const values = [];
    for (const name of names) {
      if (request[name] === undefined) continue;
      const items = model !== MODEL && typeof request[name] === "string" ? [request[name]] : request[name];
      if (!Array.isArray(items) || items.length > MAX_REFERENCES) throw new Error(name + " must be a bounded array");
      for (const value of items) values.push(publicMediaURL(value));
    }
    if (values.length > MAX_REFERENCES) throw new Error(target + " exceeds the host reference limit");
    if (values.length) body[target] = values;
  }
  const image = aliasedValue(request, ["image", "input_reference"]);
  const references = body.reference_images || [];
  if (image !== undefined) references.unshift(publicMediaURL(image));
  if (references.length > MAX_REFERENCES) throw new Error("reference_images exceeds the host reference limit");
  if (references.length + (body.reference_audios || []).length + (body.reference_videos || []).length > MAX_REFERENCES)
    throw new Error("References exceed the host reference limit");
  const otherReferences = body.reference_audios || body.reference_videos;
  const action =
    model === MODEL
      ? references.length === 1 && !otherReferences
        ? "image_to_video"
        : references.length || otherReferences
          ? "reference_to_video"
          : "text_to_video"
      : request.mode ||
        (image !== undefined && references.length === 1 && !otherReferences
          ? "image_to_video"
          : references.length || otherReferences
            ? "reference_to_video"
            : "text_to_video");
  if (action === "image_to_video") {
    if (references.length !== 1 || otherReferences) throw new Error("Tianyue image_to_video requires exactly one image");
    body.image = references[0];
    delete body.reference_images;
    return { body, action: "image_to_video" };
  }
  if (action === "text_to_video" && (references.length || otherReferences)) throw new Error("Tianyue text_to_video does not accept references");
  if (action === "reference_to_video" && !references.length && !otherReferences) throw new Error("Tianyue reference_to_video requires media references");
  if (references.length) body.reference_images = references;
  return { body, action };
}

export function buildSubmitRequest(ctx) {
  const model = ctx.upstreamModel || ctx.model;
  const normalized = normalizeRequest(ctx.requestBody, model);
  const request = normalized.body;
  // The official API defines -se as its per-second wire protocol. This controls
  // upstream duration only; custom local pricing comes from channel settings.
  const body = {
    model,
    prompt: request.prompt,
    duration: model.endsWith("-se") ? request.seconds : 1,
    video_duration: request.seconds,
    resolution: request.resolution,
    aspect_ratio: request.aspect_ratio,
    image_urls: request.image ? [request.image] : request.reference_images || [],
  };
  if (request.reference_audios) body.audio_urls = request.reference_audios;
  if (request.reference_videos) body.video_urls = request.reference_videos;
  return { method: "POST", url: ctx.baseUrl + "/v1/videos", headers: { "Content-Type": "application/json", Authorization: "Bearer " + ctx.apiKey }, body };
}

export function extractUsage(ctx) {
  const model = ctx.upstreamModel || ctx.model;
  const request = normalizeRequest(ctx.requestBody, model).body;
  if (model !== MODEL)
    return ctx.usagePurpose === "billing_ratios" ? null : { seconds: request.seconds, requests: 1, resolution: request.resolution.toLowerCase() };
  if (ctx.usagePurpose === "billing_ratios") return null;
  return { requests: 1, resolution: request.resolution };
}

export function parseSubmitResponse(ctx, response) {
  const body = response.body;
  if (!body || typeof body !== "object" || Array.isArray(body)) throw new Error("Tianyue returned an invalid video response");
  if (body.status === "failed" || (body.error && ((typeof body.error === "string" && body.error.trim()) || body.error.message)))
    throw new Error("Tianyue rejected video submission");
  const id = body.id || body.task_id;
  // oxlint-disable no-control-regex -- Reject unsafe task identifiers
  if (typeof id !== "string" || !id || id !== id.trim() || id.length > 191 || /[\r\n\x00]/.test(id))
    throw new Error("Tianyue video task id is missing or invalid");
  // oxlint-enable no-control-regex
  const request = normalizeRequest(ctx.requestBody, ctx.upstreamModel || ctx.model).body;
  return { taskId: id, taskData: body, state: { requestedSeconds: request.seconds } };
}

export function buildQueryRequest(ctx) {
  return { method: "GET", url: ctx.baseUrl + "/v1/videos/" + encodeURIComponent(ctx.taskId), headers: { Authorization: "Bearer " + ctx.apiKey } };
}

function resultURL(body) {
  const metadata = body.metadata && typeof body.metadata === "object" ? body.metadata : {};
  for (const value of [body.url, body.video_url, metadata.url]) {
    if (typeof value === "string" && value.trim()) return publicMediaURL(value);
  }
  throw new Error("Tianyue completed video result URL is missing");
}

export function parseTaskResult(ctx, body) {
  if (!body || typeof body !== "object" || Array.isArray(body)) return { status: "UNKNOWN", reason: "Tianyue returned an invalid video response" };
  const statuses = { queued: "QUEUED", in_progress: "IN_PROGRESS", completed: "SUCCESS", failed: "FAILURE" };
  if (!Object.hasOwn(statuses, body.status)) return { status: "UNKNOWN", reason: "Unrecognized Tianyue video status" };
  const result = { status: statuses[body.status] };
  if (typeof body.progress === "number" && Number.isFinite(body.progress) && body.progress >= 0 && body.progress <= 100) result.progress = body.progress + "%";
  if (result.status === "FAILURE")
    result.reason =
      (typeof body.error === "string" && body.error.trim()) ||
      (body.error && typeof body.error.message === "string" && body.error.message.trim()) ||
      "Tianyue video generation failed";
  if (result.status === "SUCCESS") {
    result.url = resultURL(body);
    if ((ctx.upstreamModel || ctx.model).endsWith("-se") && body.seconds !== undefined && body.seconds !== null && body.seconds !== "") {
      const seconds = videoSeconds(body.seconds);
      if (ctx.state && seconds > ctx.state.requestedSeconds) return { status: "FAILURE", reason: "Tianyue video duration exceeded the requested duration" };
    }
  }
  return result;
}

export function extractUsageOnComplete(ctx, result, body) {
  if (!body || body.status !== "completed" || result.status !== "SUCCESS") return {};
  if ((ctx.upstreamModel || ctx.model) !== MODEL) return {};
  return { requests: 1 };
}

export function listArtifacts(task) {
  return task.status === "SUCCESS" ? [{ key: "video", type: "video", mimeType: "video/mp4" }] : [];
}

export function buildContentRequest(ctx) {
  if (ctx.artifactKey !== "video") throw new Error("artifact_not_found");
  return { method: ctx.clientRequest.method, url: resultURL(ctx.data || {}), credentialless: true };
}

export const protocols = {
  openai_video: {
    decodeRequest(ctx) {
      if (!ctx.body || ctx.body.kind !== "json") throw new Error("Tianyue video requires JSON with public media URLs");
      if (ctx.operation && ctx.operation !== "create") throw new Error("Tianyue video supports generation only");
      if (!ctx.upstreamModel) {
        if (!ctx.body.value || typeof ctx.body.value !== "object" || Array.isArray(ctx.body.value)) throw new Error("JSON object required");
        return { kind: "submit", model: ctx.model, requestBody: ctx.body.value };
      }
      const normalized = normalizeRequest(ctx.body.value, ctx.upstreamModel || ctx.model);
      normalized.body.model = ctx.model;
      return { kind: "submit", model: ctx.model, action: normalized.action, requestBody: normalized.body };
    },
    render() {
      return {};
    },
  },
};
