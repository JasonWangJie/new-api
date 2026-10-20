const PUBLIC_MODEL = "minimax-h3-720p";
const UPSTREAM_MODEL = "MiniMaxH3-720p-realistic";
const MODELS = [PUBLIC_MODEL, UPSTREAM_MODEL];
const ASPECT_RATIOS = ["16:9", "9:16"];
const REQUEST_FIELDS = new Set([
  "model", "prompt", "seconds", "duration", "resolution", "size", "aspect_ratio", "ratio", "aspectRatio", "metadata", "references",
  "image", "input_reference", "reference_images", "images", "image_urls", "referenceImages", "reference_audios", "audios", "audio_urls", "referenceAudios",
]);
const UNSUPPORTED_FIELDS = [
  "video", "videos", "video_urls", "reference_video", "reference_videos", "referenceVideos", "content", "last_frame", "first_image", "last_image", "first_frame_image", "last_frame_image",
  "generate_audio", "watermark", "return_last_frame", "face", "task_mode", "omni_reference_task_type", "extension_direction",
  "priority", "output_format", "execution_expires_after", "service_tier", "safety_identifier", "callback_url",
];
const USAGE_SCHEMA = {
  requests: { type: "number", unit: "count", unitLabel: { en: "video", zh: "条" }, description: { en: "Video generation unit price", zh: "视频生成单价" } },
  resolution: { enum: ["480p", "720p", "1080p", "4k", "2k"], description: { en: "Output video resolution", zh: "输出视频分辨率" } },
};

export const meta = {
  apiVersion: 1,
  key: "yali",
  name: "Yali Video",
  icon: "text:YL",
  version: "1.0.3",
  author: { name: "new-api contributors" },
  baseUrl: "https://api.yaliai.com",
  website: "https://api.yaliai.com/video-api.md",
  description: { en: "Yali MiniMax H3 video generation with image and audio references", zh: "Yali MiniMax H3 图片与音频参考视频生成" },
  models: MODELS,
  fetchMode: "per_task",
  protocols: ["openai_video"],
  usageSchema: USAGE_SCHEMA,
  videoProfiles: MODELS.map((model) => {
    // The verified upstream output is 1312x736 and approximately 15 seconds.
    const resolution = "720p";
    const settings = {
      duration: { values: [15], default: 15 },
      resolutions: [resolution],
      defaultResolution: resolution,
      aspectRatios: ASPECT_RATIOS,
      defaultAspectRatio: "16:9",
    };
    return {
      models: [model],
      modes: [
        { ...settings, name: "text_to_video" },
        { ...settings, name: "reference_to_video", inputs: [
          { name: "reference_images", kind: "image", sources: ["url"], maxItems: 9 },
          { name: "reference_audios", kind: "audio", sources: ["url"], maxItems: 3 },
        ] },
      ],
    };
  }),
};

function mediaURL(value) {
  if (value && typeof value === "object" && !Array.isArray(value)) value = value.url || value.source;
  // oxlint-disable no-control-regex -- Reject whitespace and controls in media URLs
  if (typeof value !== "string" || !value || value.length > 8192 || /[\s\\\x00-\x1f\x7f]/.test(value)) throw new Error("Media references must be HTTP(S) URLs");
  // oxlint-enable no-control-regex
  const match = /^https?:\/\/([^/?#]+)(?:[/?#].*)?$/i.exec(value);
  if (!match || match[1].includes("@") || match[1].includes("%")) throw new Error("Media references must be HTTP(S) URLs without credentials");
  const authority = match[1].toLowerCase();
  const hostMatch = /^([a-z0-9](?:[a-z0-9.-]*[a-z0-9])?)(?::([0-9]{1,5}))?$/.exec(authority);
  if (!hostMatch || (hostMatch[2] && (Number(hostMatch[2]) < 1 || Number(hostMatch[2]) > 65535))) throw new Error("Media reference URL host is invalid");
  const host = hostMatch[1];
  if (!host.includes(".") || host.endsWith(".local") || host.endsWith(".localhost") || host.split(".").some((label) => !label || label.startsWith("-") || label.endsWith("-"))) throw new Error("Media references must use public hosts");
  if (/^[0-9.]+$/.test(host)) {
    const octets = host.split(".").map(Number);
    if (octets.length !== 4 || octets.some((part) => !Number.isInteger(part) || part > 255) || host.split(".").some((part) => String(Number(part)) !== part)) throw new Error("Media reference IP address is invalid");
    if ([0, 10, 127].includes(octets[0]) || octets[0] >= 224 || octets[0] === 169 && octets[1] === 254 || octets[0] === 172 && octets[1] >= 16 && octets[1] <= 31 || octets[0] === 192 && octets[1] === 168 || octets[0] === 100 && octets[1] >= 64 && octets[1] <= 127) throw new Error("Media references must use public hosts");
  }
  return value;
}

function aliasedValue(request, names) {
  let value;
  for (const name of names) {
    if (!Object.hasOwn(request, name)) continue;
    if (value !== undefined && String(value).toLowerCase() !== String(request[name]).toLowerCase()) throw new Error(names.join(" and ") + " must match");
    value = request[name];
  }
  return value;
}

function videoAPIError(value) {
  if (typeof value === "string") return value.trim();
  if (!value || typeof value !== "object" || Array.isArray(value)) return "";
  if (typeof value.message === "string" && value.message.trim()) return value.message.trim();
  const code = value.code;
  if (typeof code === "string" && code.trim() && code.trim() !== "0" || typeof code === "number" && Number.isFinite(code) && code !== 0) return "Yali video error: " + String(code);
  return "";
}

function normalizeRequest(request, model) {
  if (!MODELS.includes(model)) throw new Error("Unsupported Yali video model");
  if (!request || typeof request !== "object" || Array.isArray(request)) throw new Error("JSON object required");
  for (const name of Object.keys(request)) {
    if (!REQUEST_FIELDS.has(name)) throw new Error(name + " is not supported by this Yali model");
  }
  const metadata = request.metadata === undefined ? {} : request.metadata;
  if (!metadata || typeof metadata !== "object" || Array.isArray(metadata)) throw new Error("metadata must be an object");
  for (const name of Object.keys(metadata)) {
    if (REQUEST_FIELDS.has(name) || UNSUPPORTED_FIELDS.includes(name)) throw new Error("metadata." + name + " is not supported by this Yali model");
  }
  if (typeof request.prompt !== "string" || !request.prompt.trim()) throw new Error("prompt is required");
  if (Array.from(request.prompt).length > 20000) throw new Error("prompt exceeds the video length limit");
  const rawSeconds = aliasedValue(request, ["seconds", "duration"]);
  if (rawSeconds !== undefined && !["string", "number"].includes(typeof rawSeconds)) throw new Error("seconds must be 15");
  const seconds = rawSeconds === undefined ? 15 : Number(rawSeconds);
  if (!Number.isInteger(seconds) || seconds !== 15) throw new Error("seconds must be 15");
  const defaultResolution = "720p";
  const rawResolution = aliasedValue(request, ["resolution", "size"]);
  const resolution = rawResolution === undefined ? defaultResolution : typeof rawResolution === "string" ? rawResolution.toLowerCase() : "";
  if (resolution !== defaultResolution) throw new Error("resolution must be " + defaultResolution + " for this Yali model");
  const rawRatio = aliasedValue(request, ["aspect_ratio", "ratio", "aspectRatio"]);
  const ratio = rawRatio === undefined ? "16:9" : rawRatio;
  if (!ASPECT_RATIOS.includes(ratio)) throw new Error("aspect_ratio must be 16:9 or 9:16");

  const images = [];
  const audios = [];
  for (const [names, destination, limit] of [
    [["reference_images", "images", "image_urls", "referenceImages"], images, 9],
    [["reference_audios", "audios", "audio_urls", "referenceAudios"], audios, 3],
  ]) {
    for (const name of names) {
      if (request[name] === undefined) continue;
      if (!Array.isArray(request[name]) || request[name].length > limit) throw new Error(name + " exceeds the media limit");
      for (const value of request[name]) destination.push(mediaURL(value));
    }
  }
  if (request.image !== undefined && request.input_reference !== undefined) throw new Error("image and input_reference cannot both be provided");
  const image = request.input_reference === undefined ? request.image : request.input_reference;
  if (image !== undefined) images.push(mediaURL(image));
  if (metadata.reference_audio !== undefined) {
    if (!Array.isArray(metadata.reference_audio) || metadata.reference_audio.length > 3) throw new Error("metadata.reference_audio exceeds the media limit");
    if (audios.length) throw new Error("reference_audios and metadata.reference_audio cannot both be provided");
    for (const value of metadata.reference_audio) audios.push(mediaURL(value));
  }
  if (request.references !== undefined) {
    if (!Array.isArray(request.references) || request.references.length > 12) throw new Error("references exceeds the media limit");
    for (const reference of request.references) {
      if (!reference || typeof reference !== "object" || Array.isArray(reference) || !["image", "audio"].includes(reference.type)) throw new Error("Only image and audio references are supported");
      if (reference.role !== undefined && !["reference", "reference_" + reference.type].includes(reference.role)) throw new Error("Only ordinary media references are supported");
      for (const name of Object.keys(reference)) {
        if (!["type", "role", "source"].includes(name)) throw new Error("Unsupported reference field: " + name);
      }
      (reference.type === "image" ? images : audios).push(mediaURL(reference.source));
    }
  }
  if (images.length > 9 || audios.length > 3) throw new Error("Yali supports at most 9 image and 3 audio references");
  const body = { model, prompt: request.prompt.trim(), seconds, resolution, aspect_ratio: ratio };
  if (images.length) body.reference_images = images;
  if (audios.length) body.reference_audios = audios;
  return { body, action: images.length || audios.length ? "reference_to_video" : "text_to_video" };
}

export function buildSubmitRequest(ctx) {
  const normalized = normalizeRequest(ctx.requestBody, ctx.model);
  let model = ctx.upstreamModel || ctx.model;
  if (model === PUBLIC_MODEL) model = UPSTREAM_MODEL;
  if (model !== UPSTREAM_MODEL) throw new Error("Unsupported Yali upstream model");
  const request = normalized.body;
  // This provider calls its medium resolution 768P. The observed output is
  // 1312x736, so the public capability and frozen billing fact remain 720p.
  const body = { model, prompt: request.prompt, seconds: request.seconds, resolution: "768P", aspect_ratio: request.aspect_ratio };
  if (request.reference_images) body.images = request.reference_images;
  if (request.reference_audios) body.metadata = { reference_audio: request.reference_audios };
  return { url: ctx.baseUrl + "/v1/videos", method: "POST", headers: { "Content-Type": "application/json", Authorization: "Bearer " + ctx.apiKey }, body };
}

export function parseSubmitResponse(_ctx, response) {
  const body = response.body;
  if (!body || typeof body !== "object" || Array.isArray(body)) throw new Error("Yali returned an invalid video response");
  const error = videoAPIError(body.error);
  if (error) throw new Error(error);
  if (body.status === "failed") throw new Error("Yali video submission failed");
  if (typeof body.id !== "string" || !body.id || body.id !== body.id.trim()) throw new Error("Yali video task id is missing");
  return { taskId: body.id, taskData: body };
}

export function extractUsage(ctx) {
  if (ctx.usagePurpose === "billing_ratios") return null;
  const normalized = normalizeRequest(ctx.requestBody, ctx.model);
  return { requests: 1, resolution: normalized.body.resolution };
}

export function buildQueryRequest(ctx) {
  return { url: ctx.baseUrl + "/v1/videos/" + encodeURIComponent(ctx.taskId), method: "GET", headers: { Authorization: "Bearer " + ctx.apiKey } };
}

export function parseTaskResult(_ctx, body) {
  if (!body || typeof body !== "object" || Array.isArray(body)) return { status: "UNKNOWN", reason: "Yali returned an invalid video response" };
  const statuses = { queued: "QUEUED", in_progress: "IN_PROGRESS", completed: "SUCCESS", failed: "FAILURE" };
  const status = Object.hasOwn(statuses, body.status) ? statuses[body.status] : undefined;
  const error = videoAPIError(body.error);
  if (error || status === "FAILURE") return { status: "FAILURE", reason: error || "Yali video generation failed" };
  if (!status) return { status: "UNKNOWN", reason: "Unrecognized Yali video status" };
  const result = { status };
  if (typeof body.progress === "number" && Number.isFinite(body.progress) && body.progress >= 0 && body.progress <= 100) result.progress = body.progress + "%";
  return result;
}

export function listArtifacts(task) {
  return task.status === "SUCCESS" ? [{ key: "video", type: "video", mimeType: "video/mp4" }] : [];
}

export function buildContentRequest(ctx) {
  if (ctx.artifactKey !== "video") throw new Error("artifact_not_found");
  return { url: ctx.baseUrl + "/v1/videos/" + encodeURIComponent(ctx.upstreamTaskId) + "/content", method: ctx.clientRequest.method, headers: { Authorization: "Bearer " + ctx.apiKey } };
}

export const protocols = {
  openai_video: {
    decodeRequest(ctx) {
      if (!ctx.body || ctx.body.kind !== "json") throw new Error("Yali video requires JSON with public media URLs");
      if (ctx.operation && ctx.operation !== "create") throw new Error("Yali video supports generation only");
      const normalized = normalizeRequest(ctx.body.value, ctx.model);
      return { kind: "submit", model: ctx.model, action: normalized.action, requestBody: normalized.body };
    },
    render(_ctx, task) {
      return task.fail_reason ? { error: { message: task.fail_reason } } : {};
    },
  },
};
