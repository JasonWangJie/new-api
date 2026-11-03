// The documented name is a manifest example, never a runtime whitelist.
// The host replaces model/profile metadata with saved channel configurations.
export const meta = {
  apiVersion: 1,
  key: "sudashui",
  name: "Sudashui Video",
  icon: "text:苏打水",
  version: "1.0.0",
  author: { name: "new-api contributors" },
  baseUrl: "https://api.sudashuiapi.com",
  website: "https://api-docs.sudashuiapi.com/",
  description: { en: "Sudashui video generation with channel-configured models", zh: "苏打水渠道自定义模型视频生成" },
  models: ["sd-api-2-fast-720p"],
  fetchMode: "per_task",
  protocols: ["openai_video"],
  usageSchema: {
    seconds: { type: "number", unit: "second", description: { en: "Video generation unit price", zh: "视频生成单价" } },
    requests: { type: "number", unit: "count", unitLabel: { en: "video", zh: "条" }, description: { en: "Video generation unit price", zh: "视频生成单价" } },
  },
  videoRetryInputs: [
    { path: "/image", kind: "image" },
    { path: "/input_reference", kind: "image" },
    { path: "/first_frame", kind: "image" },
    { path: "/last_frame", kind: "image" },
    { path: "/reference_images/*", kind: "image" },
    { path: "/reference_videos/*", kind: "video" },
    { path: "/reference_audios/*", kind: "audio" },
    { path: "/references/*/source", kind: "media" },
  ],
};

const FIELDS = new Set([
  "model",
  "prompt",
  "seconds",
  "duration",
  "resolution",
  "size",
  "aspect_ratio",
  "ratio",
  "mode",
  "image",
  "input_reference",
  "first_frame",
  "last_frame",
  "reference_images",
  "reference_videos",
  "reference_audios",
  "references",
]);

function alias(body, names) {
  let value;
  for (const name of names) {
    if (body[name] === undefined) continue;
    if (value !== undefined && String(value) !== String(body[name])) throw new Error(names.join(" and ") + " must match");
    value = body[name];
  }
  return value;
}

function mediaURL(value) {
  // Network access and DNS/IP checks are also enforced by the host downloader.
  if (typeof value !== "string" || value.length > 8192 || !/^https?:\/\/[^\s\\/@?#]+(?:[/?#]|$)/i.test(value))
    throw new Error("Media must use HTTP(S) URLs without credentials");
  for (const char of value) if (char.charCodeAt(0) <= 32 || char.charCodeAt(0) === 127 || char === "\\") throw new Error("Invalid media URL characters");
  const match = /^https?:\/\/([a-z0-9](?:[a-z0-9.-]*[a-z0-9])?)(?::([0-9]{1,5}))?(?:[/?#].*)?$/i.exec(value);
  if (!match || (match[2] && (Number(match[2]) < 1 || Number(match[2]) > 65535))) throw new Error("Invalid media URL");
  const host = match[1].toLowerCase();
  if (!host.includes(".") || [".local", ".localhost", ".internal"].some((suffix) => host.endsWith(suffix))) throw new Error("Media must use public hosts");
  if (/^[0-9.]+$/.test(host)) {
    const parts = host.split(".");
    const ip = parts.map(Number);
    if (ip.length !== 4 || parts.some((part, i) => String(ip[i]) !== part || ip[i] > 255)) throw new Error("Invalid media IP address");
    if (
      [0, 10, 127].includes(ip[0]) ||
      ip[0] >= 224 ||
      (ip[0] === 169 && ip[1] === 254) ||
      (ip[0] === 172 && ip[1] >= 16 && ip[1] <= 31) ||
      (ip[0] === 192 && ip[1] === 168) ||
      (ip[0] === 100 && ip[1] >= 64 && ip[1] <= 127)
    )
      throw new Error("Media must use public hosts");
  }
  return value;
}

function normalize(body, model) {
  if (!body || typeof body !== "object" || Array.isArray(body)) throw new Error("JSON object required");
  for (const key of Object.keys(body)) if (!FIELDS.has(key)) throw new Error("Unsupported Sudashui field: " + key);
  if (typeof body.prompt !== "string" || !body.prompt.trim()) throw new Error("prompt is required");
  const value = alias(body, ["seconds", "duration"]);
  const seconds = typeof value === "number" || (typeof value === "string" && value.trim()) ? Number(value) : NaN;
  if (!Number.isInteger(seconds) || seconds < 1 || seconds > 3600) throw new Error("duration must be an integer between 1 and 3600");
  const resolution = alias(body, ["resolution", "size"]);
  if (typeof resolution !== "string" || !resolution.trim()) throw new Error("resolution is required");
  const aspectRatio = alias(body, ["aspect_ratio", "ratio"]) || "16:9";
  if (!["1:1", "3:4", "4:3", "9:16", "16:9", "21:9", "adaptive"].includes(aspectRatio)) throw new Error("Invalid aspect_ratio");
  const request = { model, prompt: body.prompt, seconds, resolution: resolution.toLowerCase(), aspect_ratio: aspectRatio };
  if (body.mode !== undefined && !["references", "frames"].includes(body.mode)) throw new Error("mode must be references or frames");
  const groups = { image: [], video: [], audio: [] };
  let first = alias(body, ["image", "input_reference", "first_frame"]);
  let last = body.last_frame;
  let hasFrame = body.first_frame !== undefined;
  for (const [kind, field] of [
    ["image", "reference_images"],
    ["video", "reference_videos"],
    ["audio", "reference_audios"],
  ]) {
    if (body[field] === undefined) continue;
    const items = Array.isArray(body[field]) ? body[field] : [body[field]];
    if (items.length > 64) throw new Error("Too many " + field);
    groups[kind].push(...items.map(mediaURL));
  }
  if (body.references !== undefined) {
    if (!Array.isArray(body.references) || body.references.length > 192) throw new Error("references must be a bounded array");
    for (const ref of body.references) {
      if (!ref || !Object.hasOwn(groups, ref.type)) throw new Error("Invalid reference type");
      if (ref.role === "first_frame" || ref.role === "last_frame") {
        hasFrame = true;
        if (ref.type !== "image") throw new Error("Frames must be images");
        if (ref.role === "first_frame") {
          if (first !== undefined) throw new Error("Duplicate first frame");
          first = ref.source;
        } else {
          if (last !== undefined) throw new Error("Duplicate last frame");
          last = ref.source;
        }
      } else {
        if (ref.role && !["reference", "reference_" + ref.type].includes(ref.role)) throw new Error("Invalid reference role");
        groups[ref.type].push(mediaURL(ref.source));
      }
    }
  }
  const hasReferences = Object.values(groups).some((items) => items.length);
  if (last !== undefined || body.mode === "frames" || hasFrame) {
    if (first === undefined || last === undefined) throw new Error("frames requires both first and last frame");
    if (hasReferences || body.mode === "references") throw new Error("Frames cannot be combined with references");
    request.image = mediaURL(first);
    request.last_frame = mediaURL(last);
    return { body: request, action: "first_last_frame" };
  }
  if (first !== undefined) groups.image.unshift(mediaURL(first));
  for (const [kind, items] of Object.entries(groups)) {
    if (items.length > 64) throw new Error("Too many references");
    if (items.length) request["reference_" + (kind === "image" ? "images" : kind === "video" ? "videos" : "audios")] = items;
  }
  if (first !== undefined && !hasReferences) {
    request.image = groups.image[0];
    delete request.reference_images;
    return { body: request, action: "image_to_video" };
  }
  return { body: request, action: hasReferences ? "reference_to_video" : "text_to_video" };
}

export function buildSubmitRequest(ctx) {
  const body = normalize(ctx.requestBody, ctx.upstreamModel || ctx.model).body;
  const payload = { aspectRatio: body.aspect_ratio, mode: body.last_frame ? "frames" : "references" };
  if (body.last_frame) {
    payload.firstFrameUrl = body.image;
    payload.lastFrameUrl = body.last_frame;
  } else {
    payload.imageUrls = body.image ? [body.image] : body.reference_images || [];
    payload.videoUrls = body.reference_videos || [];
    payload.audioUrls = body.reference_audios || [];
  }
  return {
    method: "POST",
    url: ctx.baseUrl + "/v1/video/generations",
    headers: { Authorization: "Bearer " + ctx.apiKey, "Content-Type": "application/json" },
    body: { model: body.model, prompt: body.prompt, duration: body.seconds, metadata: { payload: JSON.stringify(payload) } },
  };
}

export function extractUsage(ctx) {
  const body = normalize(ctx.requestBody, ctx.upstreamModel || ctx.model).body;
  // The host selects the saved channel's billing unit. Completion never replaces
  // accepted duration/count with vendor display prices or unverified deduction.
  if (ctx.usagePurpose === "billing_ratios") return null;
  return { seconds: body.seconds, requests: 1, resolution: body.resolution };
}

function failure(body) {
  let message = body && (body.fail_reason || body.message || (body.error && body.error.message));
  if (typeof message === "string") {
    try {
      const inner = JSON.parse(message);
      message = inner.message || inner.err_code || message;
    } catch {
      /* Plain vendor message. */
    }
  }
  return typeof message === "string" && message.trim() ? message : "Sudashui video generation failed";
}

export function parseSubmitResponse(ctx, response) {
  const body = response.body;
  if (
    !body ||
    typeof body !== "object" ||
    Array.isArray(body) ||
    body.error ||
    (body.code && body.code !== "success") ||
    ["failed", "FAILURE"].includes(body.status)
  )
    throw new Error(failure(body));
  const id = body.task_id || body.id;
  if (typeof id !== "string" || !id || id !== id.trim() || id.length > 191 || /[\s/\\?#]/.test(id) || (body.id && body.task_id && body.id !== body.task_id))
    throw new Error("Invalid Sudashui task id");
  return { taskId: id, taskData: body };
}

export function buildQueryRequest(ctx) {
  return { method: "GET", url: ctx.baseUrl + "/v1/video/generations/" + encodeURIComponent(ctx.taskId), headers: { Authorization: "Bearer " + ctx.apiKey } };
}

function resultURL(body) {
  const data = (body && body.data) || {};
  const inner = data.data || {};
  return mediaURL(data.result_url || (inner.creations && inner.creations[0] && inner.creations[0].url));
}

export function parseTaskResult(ctx, body) {
  if (!body || typeof body !== "object" || Array.isArray(body)) return { status: "UNKNOWN", reason: "Invalid Sudashui response" };
  // A failed lookup does not prove that an accepted generation failed.
  if (body.code && body.code !== "success") return { status: "UNKNOWN", reason: failure(body) };
  const data = body.data;
  if (!data || typeof data !== "object" || Array.isArray(data)) return { status: "UNKNOWN", reason: "Missing Sudashui task data" };
  if (typeof data.task_id !== "string" || data.task_id !== ctx.taskId) throw new Error("Sudashui task id mismatch");
  const inner = data.data || {};
  const states = { NOT_START: "NOT_START", SUBMITTED: "QUEUED", IN_PROGRESS: "IN_PROGRESS", SUCCESS: "SUCCESS", FAILURE: "FAILURE" };
  const innerStates = { processing: "IN_PROGRESS", queueing: "QUEUED", success: "SUCCESS", failed: "FAILURE" };
  const status = data.status !== undefined ? states[data.status] : innerStates[inner.state];
  if (!status) return { status: "UNKNOWN", reason: "Unrecognized Sudashui status" };
  if ((inner.state === "failed" && status !== "FAILURE") || (inner.state === "success" && status !== "SUCCESS"))
    throw new Error("Conflicting Sudashui task states");
  const result = { status };
  const progress = Number(String(data.progress === undefined ? "" : data.progress).replace(/%$/, ""));
  if (Number.isFinite(progress) && progress >= 0 && progress <= 100) result.progress = progress + "%";
  if (status === "FAILURE") result.reason = data.fail_reason || inner.message || inner.err_code || failure(body);
  if (status === "SUCCESS") result.url = resultURL(body);
  return result;
}

export function listArtifacts(task) {
  return task.status === "SUCCESS" ? [{ key: "video", type: "video", mimeType: "video/mp4" }] : [];
}

export function buildContentRequest(ctx) {
  if (ctx.artifactKey !== "video") throw new Error("artifact_not_found");
  return { method: ctx.clientRequest.method, url: resultURL(ctx.data), credentialless: true };
}

export const protocols = {
  openai_video: {
    decodeRequest(ctx) {
      if (!ctx.body || ctx.body.kind !== "json" || (ctx.operation && ctx.operation !== "create"))
        throw new Error("Sudashui supports JSON video generation only");
      const request = normalize(ctx.body.value, ctx.model);
      return { kind: "submit", model: ctx.model, action: request.action, requestBody: request.body };
    },
    render() {
      return {};
    },
  },
};
