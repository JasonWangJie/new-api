const IMAGE_TYPES = ["image/png", "image/jpeg", "image/webp"];
const MAX_IMAGE_BYTES = 32 * 1024 * 1024;
const IMAGE_FIELDS = ["image", "input_reference", "input_reference[]", "input_images", "reference_images", "images", "image_urls"];
const VIDEO_FIELDS = ["video_reference", "input_videos", "reference_videos", "videos", "video_urls"];
const AUDIO_FIELDS = ["audio_references", "input_audios", "reference_audios", "audios", "audio_urls"];
const START_FIELDS = ["first_frame", "start_frame"];
const END_FIELDS = ["last_frame", "end_frame"];
const REQUEST_FIELDS = new Set([
  "model",
  "prompt",
  "seconds",
  "duration",
  "size",
  "resolution",
  "aspect_ratio",
  "ratio",
  "references",
  "mode",
  ...IMAGE_FIELDS,
  ...VIDEO_FIELDS,
  ...AUDIO_FIELDS,
  ...START_FIELDS,
  ...END_FIELDS,
]);

export const meta = {
  apiVersion: 1,
  key: "huahuo-video",
  name: "HUAHUO Video",
  icon: "text:HH",
  version: "1.0.0",
  author: { name: "new-api contributors" },
  baseUrl: "https://huahuo.ccwu.cc",
  website: "https://huahuo.ccwu.cc/video",
  description: { en: "HUAHUO video generation with channel-configured models", zh: "HUAHUO 渠道自定义模型视频生成" },
  models: [],
  fetchMode: "per_task",
  protocols: ["openai_video"],
  usageSchema: {
    seconds: { type: "number", unit: "second", description: { en: "Video generation unit price", zh: "视频生成单价" } },
    requests: { type: "number", unit: "count", unitLabel: { en: "video", zh: "条" }, description: { en: "Video generation unit price", zh: "视频生成单价" } },
  },
};

function taskId(value) {
  if (typeof value !== "string" || !/^[A-Za-z0-9_-][A-Za-z0-9_.-]{0,190}$/.test(value)) throw new Error("Invalid HUAHUO video task id");
  return value;
}

function conflictingTaskId(body) {
  return ["task_id", "taskId"].some((field) => Object.hasOwn(body, field) && body[field] !== body.id);
}

function mediaURL(value, kind) {
  if (value && typeof value === "object" && !Array.isArray(value)) {
    if (Object.keys(value).length !== 1 || !Object.hasOwn(value, "url")) throw new Error("Invalid HUAHUO media URL object");
    value = value.url;
  }
  if (typeof value !== "string") throw new Error("HUAHUO references require a public HTTPS URL");
  if (kind === "image" && value.startsWith("data:")) {
    const comma = value.indexOf(",");
    const prefix = value.slice(0, comma);
    const encoded = value.slice(comma + 1);
    const padding = encoded.endsWith("==") ? 2 : encoded.endsWith("=") ? 1 : 0;
    if (
      !IMAGE_TYPES.some((type) => prefix === "data:" + type + ";base64") ||
      !encoded.length ||
      encoded.length % 4 ||
      (encoded.length * 3) / 4 - padding > MAX_IMAGE_BYTES ||
      !/^[A-Za-z0-9+/]+={0,2}$/.test(encoded)
    ) {
      throw new Error("HUAHUO inline images must be PNG, JPEG or WebP data URLs up to 32 MiB");
    }
    return value;
  }
  if (value.length > 8192 || /[\s\\]/.test(value) || Array.from(value).some((character) => character.charCodeAt(0) < 32 || character.charCodeAt(0) === 127)) {
    throw new Error("Invalid HUAHUO reference URL");
  }
  const match = /^https:\/\/([a-z0-9](?:[a-z0-9.-]*[a-z0-9])?)(?::([0-9]{1,5}))?(?:[/?][^#]*)?$/i.exec(value);
  if (!match || (match[2] && (Number(match[2]) < 1 || Number(match[2]) > 65535)))
    throw new Error("HUAHUO references require a public HTTPS URL without credentials");
  const host = match[1].toLowerCase();
  const labels = host.split(".");
  if (
    host.length > 253 ||
    labels.length < 2 ||
    ["local", "localhost", "internal", "localdomain", "lan"].includes(labels[labels.length - 1]) ||
    labels.some((label) => !label || label.length > 63 || label.startsWith("-") || label.endsWith("-"))
  ) {
    throw new Error("HUAHUO reference host must be public");
  }
  if (/^(?:[0-9]+|0x[0-9a-f]+)$/.test(labels[labels.length - 1])) {
    const parts = labels.map(Number);
    const [first, second, third] = parts;
    if (
      parts.length !== 4 ||
      parts.some((part, index) => String(part) !== labels[index] || part > 255) ||
      [0, 10, 127].includes(first) ||
      first >= 224 ||
      (first === 169 && second === 254) ||
      (first === 172 && second >= 16 && second <= 31) ||
      (first === 192 && (second === 168 || (second === 0 && [0, 2].includes(third)))) ||
      (first === 100 && second >= 64 && second <= 127) ||
      (first === 198 && ([18, 19].includes(second) || (second === 51 && third === 100))) ||
      (first === 203 && second === 0 && third === 113)
    ) {
      throw new Error("HUAHUO reference host must be public");
    }
  }
  return value;
}

function selectedField(body, fields) {
  const names = fields.filter((name) => Object.hasOwn(body, name));
  if (names.length > 1) throw new Error("Only one of " + names.join(", ") + " can be provided");
  return names[0];
}

function dimension(body, fields) {
  const values = fields.filter((name) => Object.hasOwn(body, name)).map((name) => body[name]);
  if (values.some((value) => String(value).toLowerCase() !== String(values[0]).toLowerCase())) {
    throw new Error(fields.join(" and ") + " must match");
  }
  return values[0];
}

function references(value, kind) {
  if (value === undefined) return [];
  if (typeof value === "string" && /^\s*\[/.test(value)) value = JSON.parse(value);
  const items = Array.isArray(value) ? value : [value];
  if (items.length > 64) throw new Error("Too many HUAHUO " + kind + " references");
  return items.map((item) => mediaURL(item, kind));
}

function multipartFields(body) {
  const request = {};
  for (const [name, values] of Object.entries(body.fields)) {
    if (!REQUEST_FIELDS.has(name) || !Array.isArray(values) || values.length !== 1) throw new Error("Invalid HUAHUO multipart field: " + name);
    request[name] = values[0];
  }
  return request;
}

function decode(body) {
  if (!body || !["json", "multipart"].includes(body.kind)) throw new Error("HUAHUO video requires JSON or multipart");
  const request = body.kind === "json" ? body.value : multipartFields(body);
  if (!request || typeof request !== "object" || Array.isArray(request)) throw new Error("HUAHUO video request must be an object");
  for (const name of Object.keys(request)) if (!REQUEST_FIELDS.has(name)) throw new Error("Unsupported HUAHUO video field: " + name);
  if (request.mode !== undefined && request.mode !== "first_last_frame") throw new Error("Unsupported HUAHUO video mode");
  if (typeof request.prompt !== "string" || !request.prompt.trim()) throw new Error("prompt is required");
  const rawSeconds = dimension(request, ["seconds", "duration"]);
  const seconds = typeof rawSeconds === "number" || (typeof rawSeconds === "string" && /^[0-9]+$/.test(rawSeconds)) ? Number(rawSeconds) : NaN;
  if (!Number.isInteger(seconds) || seconds < 1 || seconds > 3600) throw new Error("seconds must be a whole number from 1 to 3600");
  const sizeIsRatio = typeof request.size === "string" && /^(?:[1-9][0-9]{0,2}:[1-9][0-9]{0,2}|adaptive)$/.test(request.size);
  const resolution = dimension(request, sizeIsRatio ? ["resolution"] : ["resolution", "size"]);
  if (typeof resolution !== "string" || !/^[A-Za-z0-9][A-Za-z0-9_.-]{0,31}$/.test(resolution)) throw new Error("resolution is required");
  const ratio = dimension(request, ["aspect_ratio", "ratio"]) ?? (sizeIsRatio ? request.size : undefined);
  if (sizeIsRatio && request.size !== ratio) throw new Error("size and aspect_ratio must match");
  if (typeof ratio !== "string" || !/^(?:[1-9][0-9]{0,2}:[1-9][0-9]{0,2}|adaptive)$/.test(ratio)) throw new Error("aspect_ratio is required");

  const normalized = { prompt: request.prompt, seconds, resolution: resolution.toLowerCase(), aspect_ratio: ratio };
  const imageField = selectedField(request, IMAGE_FIELDS);
  const groups = {
    image: references(request[imageField], "image"),
    video: references(request[selectedField(request, VIDEO_FIELDS)], "video"),
    audio: references(request[selectedField(request, AUDIO_FIELDS)], "audio"),
  };
  let start = request[selectedField(request, START_FIELDS)];
  let end = request[selectedField(request, END_FIELDS)];
  if (request.references !== undefined) {
    const items = typeof request.references === "string" && body.kind === "multipart" ? JSON.parse(request.references) : request.references;
    if (!Array.isArray(items) || items.length > 64) throw new Error("references must be a bounded array");
    for (const item of items) {
      if (!item || typeof item !== "object" || !Object.hasOwn(groups, item.type) || Object.keys(item).some((key) => !["type", "role", "source"].includes(key)))
        throw new Error("Invalid HUAHUO reference");
      if (item.type === "image" && item.role === "first_frame" && start === undefined) start = item.source;
      else if (item.type === "image" && item.role === "last_frame" && end === undefined) end = item.source;
      else if (item.role === undefined || ["reference", "reference_" + item.type].includes(item.role)) groups[item.type].push(mediaURL(item.source, item.type));
      else throw new Error("Invalid or duplicate HUAHUO reference role");
    }
  }

  const files = body.kind === "multipart" ? body.files : [];
  if (files.length) {
    const field = files[0].field;
    if (
      imageField !== undefined ||
      groups.image.length ||
      !["image", "input_reference", "input_reference[]", "reference_images"].includes(field) ||
      files.length > 64
    )
      throw new Error("Use a single HUAHUO reference image upload field");
    let total = 0;
    for (const file of files) {
      if (
        file.field !== field ||
        !IMAGE_TYPES.includes(file.mimeType) ||
        !Number.isInteger(file.size) ||
        file.size < 1 ||
        file.size > MAX_IMAGE_BYTES ||
        typeof file.ref !== "string" ||
        !file.ref
      ) {
        throw new Error("HUAHUO uploads require PNG, JPEG or WebP image files up to 32 MiB each");
      }
      total += file.size;
      groups.image.push({ __fileRef: file.ref, encoding: "dataUrl" });
    }
    if (total > 2 * MAX_IMAGE_BYTES) throw new Error("HUAHUO reference image uploads exceed 64 MiB");
  }
  if (Object.values(groups).reduce((total, group) => total + group.length, 0) > 64) throw new Error("Too many HUAHUO media references");

  // The public API uses image plus last_frame for the frame-pair form.
  if (start === undefined && (end !== undefined || request.mode === "first_last_frame") && imageField === "image" && groups.image.length === 1)
    start = groups.image.pop();
  if (start !== undefined || end !== undefined || request.mode === "first_last_frame") {
    if (start === undefined || Object.values(groups).some((group) => group.length))
      throw new Error("HUAHUO frames require a start frame and cannot be combined with ordinary references");
    normalized.image = mediaURL(start, "image");
    if (end !== undefined) normalized.last_frame = mediaURL(end, "image");
    const frameBytes = [normalized.image, normalized.last_frame].reduce((total, frame) => {
      if (!frame || !frame.startsWith("data:")) return total;
      const encoded = frame.length - frame.indexOf(",") - 1;
      return total + (encoded * 3) / 4 - (frame.endsWith("==") ? 2 : frame.endsWith("=") ? 1 : 0);
    }, 0);
    if (frameBytes > MAX_IMAGE_BYTES) throw new Error("HUAHUO inline frames exceed 32 MiB in total");
    return { action: "first_last_frame", requestBody: normalized };
  }
  if (
    groups.image.length === 1 &&
    (imageField === "image" || (files.length === 1 && files[0].field === "image")) &&
    !groups.video.length &&
    !groups.audio.length
  ) {
    normalized.image = groups.image[0];
    return { action: "image_to_video", requestBody: normalized };
  }
  for (const [kind, items] of Object.entries(groups)) if (items.length) normalized["reference_" + kind + "s"] = items;
  return { action: Object.values(groups).some((group) => group.length) ? "reference_to_video" : "text_to_video", requestBody: normalized };
}

export function buildSubmitRequest(ctx) {
  const request = ctx.requestBody;
  const body = {
    model: ctx.upstreamModel || ctx.model,
    prompt: request.prompt,
    seconds: String(request.seconds),
    size: request.aspect_ratio,
    resolution: request.resolution,
  };
  const descriptor = { method: "POST", url: videoURL(ctx), headers: { Authorization: "Bearer " + ctx.apiKey, "Idempotency-Key": taskId(ctx.publicTaskId) } };
  if (ctx.action === "first_last_frame") {
    body.start_frame = request.image;
    if (request.last_frame !== undefined) body.end_frame = request.last_frame;
  }
  if (request.reference_videos) body.video_reference = request.reference_videos;
  if (request.reference_audios) body.audio_references = request.reference_audios;
  const images = ctx.action === "image_to_video" ? [request.image] : request.reference_images || [];
  if (images.some((image) => typeof image === "object")) {
    const parts = Object.entries(body).map(([name, value]) => ({ name, value: Array.isArray(value) ? JSON.stringify(value) : value }));
    for (const image of images) parts.push({ name: "input_reference", fileRef: image.__fileRef });
    return { ...descriptor, bodyType: "multipart", parts };
  }
  if (images.length) body.input_reference = images;
  return { ...descriptor, headers: { ...descriptor.headers, "Content-Type": "application/json" }, body };
}

function videoURL(ctx) {
  return ctx.baseUrl.replace(/\/+$/, "") + "/v1/videos";
}

export function extractUsage(ctx) {
  if (ctx.usagePurpose === "billing_ratios") return null;
  return { requests: 1, seconds: ctx.requestBody.seconds, resolution: ctx.requestBody.resolution };
}

export function parseSubmitResponse(_ctx, response) {
  const body = response.body;
  if (!body || typeof body !== "object" || Array.isArray(body) || conflictingTaskId(body)) throw new Error("Invalid HUAHUO video response");
  // A valid receipt takes precedence over an HTTP error or uncertain status.
  return { taskId: taskId(body.id), taskData: {} };
}

export function buildQueryRequest(ctx) {
  return { method: "GET", url: videoURL(ctx) + "/" + encodeURIComponent(taskId(ctx.taskId)), headers: { Authorization: "Bearer " + ctx.apiKey } };
}

function failureReason(ctx, error) {
  let message = typeof error?.message === "string" ? error.message : "";
  for (const secret of [ctx.apiKey, ctx.taskId]) if (secret) message = message.split(secret).join("[redacted]");
  return (
    message
      .replace(/https?:\/\/\S+/gi, "[url]")
      .replace(/\bBearer\s+\S+/gi, "Bearer [redacted]")
      .trim()
      .slice(0, 512) || "HUAHUO video generation failed"
  );
}

function failedChargeUnconfirmed(body) {
  const state = typeof body.credit_state === "string" ? body.credit_state.trim().toLowerCase() : "";
  if (["released", "refunded"].includes(state)) return false;
  if (["charged", "settled", "reserved", "held", "review_required"].includes(state)) return true;
  const amount = body.credit_amount;
  return (typeof amount === "number" || typeof amount === "string") && Number(amount) > 0;
}

export function parseTaskResult(ctx, body) {
  if (!body || typeof body !== "object" || Array.isArray(body) || body.id !== ctx.taskId || conflictingTaskId(body))
    return { status: "UNKNOWN", reason: "Invalid HUAHUO video task response" };
  const statuses = {
    queued: "QUEUED",
    submitting: "SUBMITTED",
    reserved: "SUBMITTED",
    review_required: "IN_PROGRESS",
    in_progress: "IN_PROGRESS",
    processing: "IN_PROGRESS",
    completed: "SUCCESS",
    failed: "FAILURE",
  };
  if (!Object.hasOwn(statuses, body.status)) return { status: "UNKNOWN", reason: "Unrecognized HUAHUO video status" };
  const status = statuses[body.status];
  const result = { status };
  if (status === "SUCCESS" && (body.success === false || (body.error && (typeof body.error !== "object" || Object.keys(body.error).length))))
    return { status: "UNKNOWN", reason: "Conflicting HUAHUO video result" };
  if (status === "FAILURE") {
    if (body.progress === 100 && body.progress_source === "confirmed") return { status: "UNKNOWN", reason: "Conflicting HUAHUO video result" };
    // Generation failure alone does not prove the supplier released its hold.
    if (failedChargeUnconfirmed(body)) return { status: "UNKNOWN", reason: "HUAHUO video failed with unresolved upstream credits" };
    result.reason = failureReason(ctx, body.error);
  }
  if (
    ["IN_PROGRESS", "QUEUED"].includes(status) &&
    typeof body.progress === "number" &&
    Number.isFinite(body.progress) &&
    body.progress >= 0 &&
    body.progress <= 100
  ) {
    result.progress = Math.min(99, body.progress) + "%";
  }
  return result;
}

export function listArtifacts(task) {
  return task.status === "SUCCESS" ? [{ key: "video", type: "video", mimeType: "video/mp4" }] : [];
}

export function buildContentRequest(ctx) {
  if (ctx.artifactKey !== "video") throw new Error("artifact_not_found");
  return {
    method: ctx.clientRequest.method,
    url: videoURL(ctx) + "/" + encodeURIComponent(taskId(ctx.upstreamTaskId)) + "/content",
    headers: { Authorization: "Bearer " + ctx.apiKey },
  };
}

export const protocols = {
  openai_video: {
    decodeRequest(ctx) {
      if (ctx.operation && ctx.operation !== "create") throw new Error("HUAHUO video supports generation only");
      const result = decode(ctx.body);
      return { kind: "submit", model: ctx.model, action: result.action, requestBody: { model: ctx.model, ...result.requestBody } };
    },
    render(_ctx, task) {
      return task.fail_reason ? { error: { message: task.fail_reason } } : {};
    },
  },
};
