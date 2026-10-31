const RATIOS = ["16:9", "4:3", "1:1", "3:4", "9:16", "21:9"];
const MODELS = {
  "seedance2.0-A": { series: "A", min: 4, max: 15, resolutions: ["480p", "720p", "1080p", "4k"], defaultResolution: "480p", aspectRatios: [...RATIOS, "Auto"], images: 9, videos: 3, audios: 3, maxReferences: 12 },
  "seedance2.5-A": { series: "A", min: 4, max: 30, resolutions: ["480p", "720p"], defaultResolution: "480p", images: 30, videos: 10, audios: 10, maxReferences: 50 },
  "minimax-h3": { series: "H3", min: 4, max: 15, resolutions: ["480p", "720p", "1080p"], defaultResolution: "480p", aspectRatios: ["16:9", "9:16"], images: 9, videos: 3, audios: 3, maxReferences: 12 },
  "seedance2.0-E-720p": { series: "legacy", min: 5, max: 15, resolutions: ["720p"], images: 9, videos: 3, audios: 3 },
  "seedance2.0-E-1080p": { series: "legacy", min: 5, max: 15, resolutions: ["1080p"], images: 9, videos: 3, audios: 3 },
  "seedance2.0-F-720p": { series: "legacy", min: 5, max: 15, resolutions: ["720p"], images: 9, videos: 0, audios: 3 },
  "seedance2.0-fast-F-720p": { series: "legacy", min: 5, max: 15, resolutions: ["720p"], images: 9, videos: 0, audios: 3 },
  "seedance2.0-G-720p": { series: "legacy", min: 5, max: 15, resolutions: ["720p"], images: 9, videos: 3, audios: 3 },
  "seedance2.0-E": { series: "E", min: 4, max: 15, resolutions: ["480p", "720p", "1080p", "4k"], images: 9, videos: 3, audios: 3 },
  "seedance2.0-fast-E": { series: "E", min: 4, max: 15, resolutions: ["720p", "4k"], images: 9, videos: 3, audios: 3 },
  "seedance2.0-Mini-E": { series: "E", min: 4, max: 15, resolutions: ["480p", "720p"], images: 9, videos: 3, audios: 3 },
  "seedance2.5-E": { series: "E", min: 4, max: 30, resolutions: ["480p", "720p", "1080p"], images: 30, videos: 10, audios: 10 },
  "seedance2.0-F": { series: "F", min: 4, max: 15, resolutions: ["720px"], images: 9, videos: 3, audios: 3 },
  "seedance2.5-F": { series: "F", min: 4, max: 30, resolutions: ["720p"], images: 9, videos: 3, audios: 3 },
  "seedance2.0-G": { series: "G", min: 4, max: 15, resolutions: ["480p", "720p", "1080p", "4k"], images: 9, videos: 3, audios: 3 },
  "seedance2.0-fast-G": { series: "G", min: 4, max: 15, resolutions: ["480p", "720p"], images: 9, videos: 3, audios: 3 },
  "seedance2.0-mini-G": { series: "G", min: 4, max: 15, resolutions: ["480p", "720p"], images: 9, videos: 3, audios: 3 },
  "seedance2.5-G": { series: "G", min: 4, max: 30, resolutions: ["480p", "720p", "1080p"], images: 30, videos: 10, audios: 10 },
};

const RESOLUTION_SCHEMA = {
  enum: ["480p", "720p", "720px", "1080p", "4k"],
  description: { en: "Output video resolution", zh: "输出视频分辨率" },
};
const SECONDS_SCHEMA = { type: "number", unit: "second", description: { en: "Video generation unit price", zh: "视频生成单价" } };
const VIDEO_INPUT_SCHEMA = {
  enum: ["none", "video"],
  description: { en: "Reference video input", zh: "参考视频输入" },
  enumLabels: { none: { en: "No reference video", zh: "无参考视频" }, video: { en: "With reference video", zh: "有参考视频" } },
};
const VIDEO_USAGE_SCHEMA = {
  seconds: SECONDS_SCHEMA,
  tokens: { type: "number", unit: "token", description: { en: "Video billing token unit price", zh: "视频计费 Token 单价" } },
  resolution: RESOLUTION_SCHEMA,
  video_input: VIDEO_INPUT_SCHEMA,
};

function videoProfile(model, config) {
  const sources = config.series === "G" ? ["url", "asset"] : config.series === "legacy" ? ["url", "data_uri"] : ["url"];
  const referenceInputs = [{ name: "reference_images", kind: "image", sources, maxItems: config.images }];
  if (config.videos) referenceInputs.push({ name: "reference_videos", kind: "video", sources, maxItems: config.videos });
  referenceInputs.push({ name: "reference_audios", kind: "audio", sources, maxItems: config.audios });
  const base = {
    duration: { min: config.min, max: config.max, step: 1, default: config.series === "G" ? 5 : config.min },
    resolutions: config.resolutions,
    defaultResolution: config.defaultResolution || (config.series === "E" ? config.resolutions[0] : config.resolutions.includes("720p") ? "720p" : config.resolutions[0]),
    aspectRatios: config.aspectRatios || (config.series === "G" ? [...RATIOS, "adaptive"] : RATIOS),
    defaultAspectRatio: config.series === "G" ? "adaptive" : "16:9",
  };
  if (config.series === "F" || config.series === "H3") base.options = { generateAudio: true };
  if (config.series === "G") base.options = { generateAudio: true, watermark: true, returnLastFrame: true };
  const modes = [{ ...base, name: "text_to_video" }, { ...base, name: "reference_to_video", inputs: referenceInputs }];
  if (model === "seedance2.0-A") {
    const frame = { name: "image", kind: "image", sources, maxItems: 1, required: true };
    modes.push({ ...base, name: "first_last_frame", aspectRatios: ["Auto"], defaultAspectRatio: "Auto", inputs: [frame, { ...frame, name: "last_frame" }] });
  }
  if (config.series === "G") {
    const frame = { name: "image", kind: "image", sources, maxItems: 1, required: true };
    const pinned = model === "seedance2.5-G" ? { ...base, aspectRatios: ["adaptive"], defaultAspectRatio: "adaptive" } : base;
    modes.push({ ...pinned, name: "image_to_video", inputs: [frame] });
    modes.push({ ...pinned, name: "first_last_frame", inputs: [frame, { ...frame, name: "last_frame" }] });
  }
  if (model === "seedance2.5-G") {
    const operationInputs = [{ name: "video", kind: "video", sources, maxItems: 1, required: true }, ...referenceInputs.map((input) => input.name === "reference_videos" ? { ...input, maxItems: config.videos - 1 } : input)];
    const edit = { ...base, name: "edit_video", inputs: operationInputs, aspectRatios: ["adaptive"], defaultAspectRatio: "adaptive" };
    delete edit.duration;
    modes.push(edit);
    modes.push({ ...base, name: "extend_video", inputs: operationInputs, aspectRatios: ["adaptive"], defaultAspectRatio: "adaptive", extensionDirections: ["backward"], defaultExtensionDirection: "backward" });
  }
  return { models: [model], modes };
}

export const meta = {
  apiVersion: 1,
  key: "drama",
  name: "Drama Video A / E / F / G / H3",
  icon: "text:DR",
  version: "1.1.6",
  author: { name: "new-api contributors" },
  baseUrl: "https://drama.dafeiyangapi.top",
  website: "https://drama.dafeiyangapi.top/docs/drama",
  description: { en: "Drama Seedance A, E, F, G and MiniMax H3 video generation", zh: "Drama Seedance A、E、F、G 与 MiniMax H3 视频生成" },
  models: Object.keys(MODELS),
  fetchMode: "per_task",
  protocols: ["openai_video"],
  usageSchema: VIDEO_USAGE_SCHEMA,
  usageExamples: [{ label: "720p · 5s", facts: { seconds: 5, tokens: 108000, resolution: "720p", video_input: "none" } }],
  usageProfiles: Object.entries(MODELS).map(([model, config]) => {
    // Pricing includes the four standard rows and any provider-specific values.
    // Actual generation limits remain in config.resolutions and videoProfiles.
    const resolution = { ...RESOLUTION_SCHEMA, enum: [...new Set(["480p", "720p", "1080p", "4k", ...config.resolutions])] };
    let schema = { ...VIDEO_USAGE_SCHEMA, resolution };
    if (config.series === "E") {
      schema = {
        requests: { type: "number", unit: "count", unitLabel: { en: "video", zh: "条" }, description: { en: "Video generation unit price", zh: "视频生成单价" } },
        resolution,
        video_input: VIDEO_INPUT_SCHEMA,
      };
    } else if (config.series === "legacy") {
      schema = { seconds: SECONDS_SCHEMA, resolution, video_input: VIDEO_INPUT_SCHEMA };
    } else if (["A", "H3"].includes(config.series)) {
      schema = { seconds: SECONDS_SCHEMA, resolution, video_input: VIDEO_INPUT_SCHEMA };
    }
    const profile = { models: [model], schema };
    if (Object.hasOwn(schema, "tokens")) {
      const sampleResolution = config.resolutions.includes("720p") ? "720p" : config.resolutions[0];
      profile.examples = [{ label: sampleResolution + " · 5s", facts: { seconds: 5, tokens: 108000, resolution: sampleResolution, video_input: "none" } }];
    }
    return profile;
  }),
  videoProfiles: Object.keys(MODELS).map((model) => videoProfile(model, MODELS[model])),
};

const REQUEST_FIELDS = new Set([
  "model", "prompt", "seconds", "duration", "resolution", "size", "aspect_ratio", "ratio", "aspectRatio", "references",
  "image", "input_reference", "last_frame", "first_image", "last_image", "video", "reference_images", "reference_videos", "reference_audios",
  "images", "image_urls", "referenceImages", "videos", "video_urls", "referenceVideos", "audios", "audio_urls", "referenceAudios",
  "generate_audio", "watermark", "return_last_frame", "task_mode", "omni_reference_task_type", "extension_direction",
  "priority", "output_format", "execution_expires_after", "service_tier", "safety_identifier", "callback_url", "metadata",
]);
const G_FIELDS = ["priority", "output_format", "execution_expires_after", "service_tier", "safety_identifier", "callback_url"];

function modelConfig(model) {
  if (!Object.hasOwn(MODELS, model)) throw new Error("Unsupported Drama video model");
  return MODELS[model];
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

function mediaSource(value, type, config) {
  if (value && typeof value === "object" && !Array.isArray(value)) value = value.url || value.source;
  // oxlint-disable no-control-regex -- Public media URLs must reject control characters
  if (typeof value !== "string" || !value || value !== value.trim() || /[\s\\\x00-\x1f]/.test(value)) throw new Error("Media source must be a public HTTPS URL");
  // oxlint-enable no-control-regex
  if (/^https:\/\/[^/?#@]+(?:[/?#].*)?$/i.test(value)) return value;
  if (config.series === "G" && /^asset:\/\/[A-Za-z0-9_-]+$/.test(value)) return value;
  if (config.series === "legacy" && value.startsWith("data:" + type + "/") && /^data:[^;,]+;base64,[A-Za-z0-9+/]+={0,2}$/.test(value)) return value;
  throw new Error("Media source is not supported by this Drama model");
}

function validateAReferencePrompt(prompt, references) {
  const counts = { image: 0, video: 0, audio: 0 };
  const mentioned = { image: new Set(), video: new Set(), audio: new Set() };
  for (const reference of references) counts[reference.type]++;
  const types = { "图": "image", "图片": "image", "视频": "video", "音频": "audio" };
  for (const match of prompt.matchAll(/@(图片|图|视频|音频)([+-]?[0-9]+(?:\.[0-9]+)?)/g)) {
    const type = types[match[1]];
    const index = Number(match[2]);
    if (!/^[1-9][0-9]*$/.test(match[2]) || !Number.isSafeInteger(index) || index > counts[type]) throw new Error("A series reference index must be a positive integer without leading zeros and within the supplied media count");
    mentioned[type].add(index);
  }
  for (const type of ["image", "video", "audio"]) {
    if (mentioned[type].size !== counts[type]) throw new Error("Every A series reference must be mentioned in prompt with @图N, @图片N, @视频N or @音频N");
  }
}

function normalizeRequest(request, model, operation) {
  const config = modelConfig(model);
  if (!request || typeof request !== "object" || Array.isArray(request)) throw new Error("JSON object required");
  for (const name of Object.keys(request)) {
    if (!REQUEST_FIELDS.has(name)) throw new Error(name + " is not supported by this Drama model");
  }
  const req = Object.assign({}, request);
  if (req.first_image !== undefined || req.last_image !== undefined) {
    if (model !== "seedance2.0-A") throw new Error("first_image and last_image require seedance2.0-A");
    if (req.first_image !== undefined) {
      if (req.image !== undefined || req.input_reference !== undefined) throw new Error("first_image conflicts with image or input_reference");
      req.image = req.first_image;
    }
    if (req.last_image !== undefined) {
      if (req.last_frame !== undefined) throw new Error("last_image conflicts with last_frame");
      req.last_frame = req.last_image;
    }
  }
  if (req.metadata !== undefined) {
    if (!req.metadata || typeof req.metadata !== "object" || Array.isArray(req.metadata)) throw new Error("metadata must be an object");
    for (const name of Object.keys(req.metadata)) {
      if (![...G_FIELDS, "generate_audio", "watermark", "return_last_frame"].includes(name)) throw new Error("metadata." + name + " is not supported");
      if (Object.hasOwn(req, name)) throw new Error("metadata." + name + " conflicts with a structured field");
      req[name] = req.metadata[name];
    }
  }
  if (typeof req.prompt !== "string" || !req.prompt.trim()) throw new Error("prompt is required");
  const prompt = req.prompt.trim();
  if (Array.from(prompt).length > (config.series === "G" ? 20000 : config.series === "legacy" ? 4000 : 20000)) throw new Error("prompt exceeds this Drama model's length limit");
  const requestedType = req.omni_reference_task_type;
  if (requestedType !== undefined && (model !== "seedance2.5-G" || !["auto", "reference", "edit", "extend"].includes(requestedType))) throw new Error("omni_reference_task_type is not supported");
  let action = operation === "edit" || operation === "edit_video" ? "edit_video" : operation === "extend" || operation === "extend_video" ? "extend_video" : "";
  if (requestedType === "edit" || requestedType === "extend") {
    const nativeAction = requestedType + "_video";
    if (action && action !== nativeAction) throw new Error("omni_reference_task_type conflicts with the video operation");
    action = nativeAction;
  } else if (action && requestedType !== undefined && requestedType !== "auto") {
    throw new Error("omni_reference_task_type conflicts with the video operation");
  }
  if (action && model !== "seedance2.5-G") throw new Error("Video editing and extension require seedance2.5-G");
  const rawSeconds = aliasedValue(req, ["seconds", "duration"]);
  let seconds = config.min;
  if (action === "edit_video") {
    if (rawSeconds !== undefined && rawSeconds !== -1 && rawSeconds !== "-1") throw new Error("Video editing uses automatic duration; omit seconds");
  } else {
    if (rawSeconds === undefined && config.series === "G") throw new Error("seconds is required for this Drama model");
    if (rawSeconds !== undefined && !["number", "string"].includes(typeof rawSeconds)) throw new Error("seconds must be an integer");
    seconds = rawSeconds === undefined ? config.min : Number(rawSeconds);
    if (!Number.isInteger(seconds) || seconds < config.min || seconds > config.max) throw new Error("seconds must be between " + config.min + " and " + config.max);
  }
  const rawResolution = aliasedValue(req, ["resolution", "size"]);
  const defaultResolution = config.defaultResolution || (config.series === "E" ? config.resolutions[0] : config.resolutions.includes("720p") ? "720p" : config.resolutions[0]);
  const resolution = rawResolution === undefined ? defaultResolution : typeof rawResolution === "string" ? rawResolution.toLowerCase() : "";
  if (!config.resolutions.includes(resolution)) throw new Error("resolution is not supported by this Drama model");
  const rawRatio = aliasedValue(req, ["aspect_ratio", "ratio", "aspectRatio"]);
  let ratio = rawRatio === undefined ? config.series === "G" ? "adaptive" : "16:9" : rawRatio;
  if (model === "seedance2.0-A" && typeof ratio === "string" && ratio.toLowerCase() === "auto") ratio = "Auto";
  const ratios = config.aspectRatios || [...RATIOS, ...(config.series === "G" ? ["adaptive"] : [])];
  if (!ratios.includes(ratio)) throw new Error("aspect_ratio is not supported by this Drama model");

  const references = [];
  if (req.references !== undefined) {
    if (!Array.isArray(req.references) || req.references.length > (config.maxReferences || config.images + config.videos + config.audios)) throw new Error("references exceeds this Drama model's media limit");
    for (const reference of req.references) {
      if (!reference || typeof reference !== "object" || Array.isArray(reference) || !["image", "video", "audio"].includes(reference.type)) throw new Error("Invalid reference media type");
      for (const name of Object.keys(reference)) {
        if (!["type", "role", "source"].includes(name)) throw new Error("Unsupported reference field: " + name);
      }
      let role = reference.role;
      if (config.series === "G") {
        if (!["reference_" + reference.type, ...(reference.type === "image" ? ["first_frame", "last_frame"] : [])].includes(role)) throw new Error("G series references require an explicit matching role");
      } else if (!(model === "seedance2.0-A" && reference.type === "image" && ["first_frame", "last_frame"].includes(role))) {
        if (config.series === "H3" && role === undefined) role = "reference";
        if (!["reference", "reference_" + reference.type].includes(role)) throw new Error("This Drama model only supports ordinary references");
        role = "reference";
      }
      references.push({ type: reference.type, role, source: mediaSource(reference.source, reference.type, config) });
    }
  }
  for (const entry of [
    ["image", ["reference_images", "images", "image_urls", "referenceImages"]],
    ["video", ["reference_videos", "videos", "video_urls", "referenceVideos"]],
    ["audio", ["reference_audios", "audios", "audio_urls", "referenceAudios"]],
  ]) {
    for (const name of entry[1]) {
      if (req[name] === undefined) continue;
      if (!Array.isArray(req[name]) || req[name].length > config.images + config.videos + config.audios) throw new Error(name + " must be a bounded array");
      for (const value of req[name]) references.push({ type: entry[0], role: config.series === "G" ? "reference_" + entry[0] : "reference", source: mediaSource(value, entry[0], config) });
    }
  }
  if (req.image !== undefined && req.input_reference !== undefined) throw new Error("image and input_reference cannot both be provided");
  const first = req.image === undefined ? req.input_reference : req.image;
  if (first !== undefined) references.push({ type: "image", role: config.series === "G" || model === "seedance2.0-A" ? "first_frame" : "reference", source: mediaSource(first, "image", config) });
  if (req.last_frame !== undefined) {
    if (config.series !== "G" && model !== "seedance2.0-A") throw new Error("This Drama model does not support last_frame");
    references.push({ type: "image", role: "last_frame", source: mediaSource(req.last_frame, "image", config) });
  }
  if (req.video !== undefined) {
    if (!action) throw new Error("video is only supported for video editing and extension");
    references.unshift({ type: "video", role: "reference_video", source: mediaSource(req.video, "video", config) });
  }
  const counts = { image: 0, video: 0, audio: 0 };
  const seen = new Set();
  let firstFrame, lastFrame;
  for (const reference of references) {
    if (seen.has(reference.source)) throw new Error("Duplicate reference media source");
    seen.add(reference.source);
    counts[reference.type]++;
    if (reference.role === "first_frame") {
      if (firstFrame) throw new Error("Only one first_frame is supported");
      firstFrame = reference.source;
    }
    if (reference.role === "last_frame") {
      if (lastFrame) throw new Error("Only one last_frame is supported");
      lastFrame = reference.source;
    }
  }
  if (counts.image > config.images || counts.video > config.videos || counts.audio > config.audios || ((config.series === "F" || config.series === "H3") && counts.video + counts.audio > 3) || (config.maxReferences && references.length > config.maxReferences)) throw new Error("Reference media exceeds this Drama model's limits");
  if (lastFrame && !firstFrame) throw new Error("last_frame requires first_frame");
  if (firstFrame && references.length !== (lastFrame ? 2 : 1)) throw new Error("Pinned frames cannot be mixed with reference media");
  if (firstFrame && requestedType === "reference") throw new Error("omni_reference_task_type reference cannot use pinned frames");
  if (model === "seedance2.0-A" && firstFrame) {
    if (!lastFrame) throw new Error("A 2.0 first_frame requires last_frame");
    if (rawRatio === undefined) ratio = "Auto";
    if (ratio !== "Auto") throw new Error("A 2.0 paired frames require aspect_ratio Auto");
  }
  if (config.series === "A" && !firstFrame) validateAReferencePrompt(prompt, references);
  if (config.series === "G" && model !== "seedance2.5-G" && counts.audio && !counts.image && !counts.video) throw new Error("Audio references require an image or video reference");
  if (action && (!counts.video || firstFrame)) throw new Error("Video editing and extension require a reference video");
  if (model === "seedance2.5-G" && (action || firstFrame) && ratio !== "adaptive") throw new Error("This video mode requires aspect_ratio adaptive");
  if (req.extension_direction !== undefined && (action !== "extend_video" || req.extension_direction !== "backward")) throw new Error("Only backward video extension is supported");
  if (!action) action = lastFrame ? "first_last_frame" : firstFrame ? "image_to_video" : references.length ? "reference_to_video" : "text_to_video";
  const taskMode = model === "seedance2.0-A" && firstFrame ? "first_last_frame" : references.length ? "references" : "text";
  if (req.task_mode !== undefined && req.task_mode !== taskMode) throw new Error("task_mode conflicts with the supplied media");

  const body = { model: req.model, prompt, resolution, aspect_ratio: ratio };
  if (action !== "edit_video") body.seconds = seconds;
  // Keep one ordered representation across repeated decode and driver calls.
  // Regrouping aliases here would change mixed-media and pinned-frame positions.
  if (references.length) body.references = references;
  if (action === "extend_video") body.extension_direction = "backward";
  for (const name of ["generate_audio", "watermark", "return_last_frame"]) {
    if (req[name] === undefined) continue;
    if (typeof req[name] !== "boolean") throw new Error(name + " must be a boolean");
    if (config.series !== "G" && !(["F", "H3"].includes(config.series) && name === "generate_audio")) throw new Error(name + " is not supported by this Drama model");
    body[name] = req[name];
  }
  for (const name of G_FIELDS) {
    if (req[name] === undefined) continue;
    if (config.series !== "G") throw new Error(name + " is not supported by this Drama model");
    body[name] = req[name];
  }
  if (body.priority !== undefined && (!Number.isInteger(body.priority) || body.priority < 0 || body.priority > 9)) throw new Error("priority must be between 0 and 9");
  if (body.execution_expires_after !== undefined && (!Number.isInteger(body.execution_expires_after) || body.execution_expires_after < 3600 || body.execution_expires_after > 259200)) throw new Error("execution_expires_after must be between 3600 and 259200");
  if (body.output_format !== undefined && (model !== "seedance2.5-G" || !["mp4", "mov"].includes(body.output_format))) throw new Error("output_format is not supported");
  if (body.service_tier !== undefined && body.service_tier !== "default") throw new Error("service_tier must be default");
  if (body.safety_identifier !== undefined && (typeof body.safety_identifier !== "string" || Array.from(body.safety_identifier).length > 256)) throw new Error("safety_identifier must be at most 256 characters");
  if (body.callback_url !== undefined) body.callback_url = mediaSource(body.callback_url, "video", { series: "E" });
  return { body, action, references };
}

export function buildSubmitRequest(ctx) {
  const model = ctx.upstreamModel || ctx.model;
  const prepared = normalizeRequest(ctx.requestBody, model, ctx.action);
  const config = modelConfig(model);
  const body = { model, prompt: prepared.body.prompt, resolution: prepared.body.resolution, aspect_ratio: prepared.body.aspect_ratio, seconds: prepared.action === "edit_video" ? -1 : prepared.body.seconds };
  if (prepared.references.length) body.references = prepared.references;
  for (const name of ["generate_audio", "watermark", "return_last_frame", ...G_FIELDS]) {
    if (prepared.body[name] !== undefined) body[name] = prepared.body[name];
  }
  if (config.series === "legacy") body.task_mode = prepared.references.length ? "references" : "text";
  if (["edit_video", "extend_video"].includes(prepared.action)) body.omni_reference_task_type = prepared.action === "edit_video" ? "edit" : "extend";
  return { method: "POST", url: ctx.baseUrl + (config.series === "legacy" ? "/v1/video/generations" : "/v1/videos"), headers: { "Content-Type": "application/json", Authorization: "Bearer " + ctx.apiKey }, body, action: prepared.action };
}

export function extractUsage(ctx) {
  // Legacy ratio pricing must not multiply a per-request price by token estimates.
  if (ctx.usagePurpose === "billing_ratios") return null;
  const model = ctx.upstreamModel || ctx.model;
  const config = modelConfig(model);
  const prepared = normalizeRequest(ctx.requestBody, model, ctx.action);
  const resolution = prepared.body.resolution;
  const videoInput = prepared.references.some((reference) => reference.type === "video");
  const videoInputFact = videoInput ? "video" : "none";
  if (config.series === "E") return { requests: 1, resolution, video_input: videoInputFact };
  const seconds = prepared.action === "edit_video" ? config.max : prepared.body.seconds;
  const facts = { seconds, resolution, video_input: videoInputFact };
  if (config.series === "F" || config.series === "G") {
    // Ark-style pixel/time estimate for pre-consume only. The gateway's
    // actual total_tokens replaces it at completion, including reference videos.
    const pixels = { "480p": 854 * 480, "720p": 1280 * 720, "720px": 1280 * 720, "1080p": 1920 * 1080, "4k": 3840 * 2160 }[resolution];
    facts.tokens = Math.ceil((seconds + (videoInput ? config.max : 0)) * pixels * 24 / 1024);
  }
  return facts;
}

export function parseSubmitResponse(ctx, response) {
  const body = response.body;
  const taskId = body && (body.id || body.task_id);
  if (typeof taskId !== "string" || !taskId.trim()) throw new Error("Upstream video task id is missing");
  return { taskId, taskData: body, state: extractUsage(ctx) };
}

export function buildQueryRequest(ctx) {
  const config = modelConfig(ctx.upstreamModel || ctx.model);
  return { method: "GET", url: ctx.baseUrl + (config.series === "legacy" ? "/v1/video/generations/" : "/v1/videos/") + encodeURIComponent(ctx.taskId), headers: { Authorization: "Bearer " + ctx.apiKey } };
}

function completionUsage(ctx, body) {
  const config = modelConfig(ctx.upstreamModel || ctx.model);
  const facts = {};
  if (config.series === "E") return facts;
  const metadata = body.metadata && typeof body.metadata === "object" ? body.metadata : {};
  const billing = metadata.billing && typeof metadata.billing === "object" ? metadata.billing : {};
  const requiresTokens = config.series === "G" || (config.series === "F" && ((ctx.state || {}).video_input === "video" || billing.mode === "per_million_tokens"));
  if (requiresTokens && (billing.settle_state === "pending" || billing.settled === false)) return null;
  if (requiresTokens && billing.settle_state === "failed") throw new Error("Upstream video billing settlement failed");
  if (config.series === "F" || config.series === "G") {
    const sources = config.series === "F" ? [metadata.usage, body.usage] : [body.usage, metadata.usage];
    for (const usage of sources) {
      if (usage === undefined || usage === null) continue;
      if (typeof usage !== "object" || Array.isArray(usage)) throw new Error("Invalid upstream video token usage");
      const tokens = usage.total_tokens === undefined ? usage.completion_tokens === undefined ? usage.output_tokens : usage.completion_tokens : usage.total_tokens;
      if (tokens === undefined) continue;
      if (typeof tokens !== "number" || !Number.isSafeInteger(tokens) || tokens < 0 || tokens > 2147483647) throw new Error("Invalid upstream video token usage");
      facts.tokens = tokens;
      break;
    }
  }
  if (requiresTokens && facts.tokens === undefined) return null;
  const duration = body.seconds === undefined ? body.duration === undefined ? billing.seconds : body.duration : body.seconds;
  // Extension output can include the original clip; keep the requested extension
  // seconds. An edit may echo -1 instead of reporting a measured output duration.
  const automaticEdit = ctx.action === "edit_video" && (duration === -1 || duration === "-1");
  if (duration !== undefined && ctx.action !== "extend_video" && !automaticEdit) {
    if (!["number", "string"].includes(typeof duration) || !Number.isFinite(Number(duration)) || Number(duration) <= 0 || Number(duration) > 3600) throw new Error("Invalid upstream video duration");
    facts.seconds = Number(duration);
  }
  return facts;
}

export function parseTaskResult(ctx, body) {
  if (!body || typeof body !== "object" || Array.isArray(body)) return { status: "UNKNOWN", reason: "Unrecognized upstream video response" };
  const statuses = { queued: "QUEUED", in_progress: "IN_PROGRESS", completed: "SUCCESS", failed: "FAILURE" };
  let status = typeof body.status === "string" && Object.hasOwn(statuses, body.status) ? statuses[body.status] : undefined;
  if (!status) return { status: "UNKNOWN", reason: "Unrecognized upstream video status: " + String(body.status || "") };
  if (status === "SUCCESS" && completionUsage(ctx, body) === null) status = "IN_PROGRESS";
  const result = { status };
  if (typeof body.progress === "number" && Number.isFinite(body.progress) && body.progress >= 0 && body.progress <= 100) result.progress = (status === "IN_PROGRESS" ? Math.min(99, body.progress) : body.progress) + "%";
  if (status === "FAILURE") {
    let reason = "";
    if (typeof body.error === "string") {
      reason = body.error.trim();
    } else if (body.error && typeof body.error.message === "string") {
      reason = body.error.message.trim();
    }
    if (!reason) {
      const metadata = body.metadata;
      let candidate = "";
      if (metadata && typeof metadata === "object" && !Array.isArray(metadata) && typeof metadata.url === "string") {
        candidate = metadata.url.trim();
      }
      // Some failed Drama jobs place their diagnostic in the result URL field.
      // Result locations themselves must not be exposed as an error reason.
      if (candidate && !/^(?:(?:https?|data|blob|file|ftp|s3|gs):|\/)/i.test(candidate)) reason = candidate;
    }
    result.reason = reason || "Video generation failed; upstream did not provide an error reason";
  }
  return result;
}

export function extractUsageOnComplete(ctx, _result, body) {
  if (!body || body.status !== "completed") return {};
  return completionUsage(ctx, body) || {};
}

export function listArtifacts(task) {
  if (task.status !== "SUCCESS") return [];
  const artifacts = [{ key: "video", type: "video" }];
  if (task.data && typeof task.data.last_frame_url === "string" && task.data.last_frame_url) artifacts.push({ key: "last_frame", type: "image" });
  return artifacts;
}

export function buildContentRequest(ctx) {
  if (ctx.artifactKey === "last_frame" && ctx.data && typeof ctx.data.last_frame_url === "string" && ctx.data.last_frame_url) return { method: ctx.clientRequest.method, url: ctx.data.last_frame_url, credentialless: true };
  if (ctx.artifactKey !== "video") throw new Error("artifact_not_found");
  return { method: ctx.clientRequest.method, url: ctx.baseUrl + "/v1/videos/" + encodeURIComponent(ctx.upstreamTaskId) + "/content", headers: { Authorization: "Bearer " + ctx.apiKey } };
}

export const protocols = {
  openai_video: {
    decodeRequest(ctx) {
      if (!ctx.body || ctx.body.kind !== "json") throw new Error("Drama video requires JSON with public media URLs or asset references");
      const prepared = normalizeRequest(ctx.body.value, ctx.upstreamModel || ctx.model, ctx.operation);
      prepared.body.model = ctx.model;
      return { kind: "submit", model: ctx.model, action: prepared.action, requestBody: prepared.body };
    },
    render(_ctx, task) {
      const result = {};
      if (task.fail_reason) result.error = { message: task.fail_reason };
      // The host supplies public identity, lifecycle and its own content route.
      // Never publish a provider URL or the provider's separate billing ledger.
      return result;
    },
  },
};
