const SEEDANCE_RATIOS = ["16:9", "4:3", "1:1", "3:4", "9:16", "21:9", "adaptive"];
// Native Ark content may carry richer provider-native material. Structured
// workbench inputs intentionally stay on public URLs and asset IDs.
const SEEDANCE_PROFILE_SOURCES = ["url", "asset"];

function seedanceMode(name, duration, resolutions, inputs, options) {
  const profile = {
    name,
    duration,
    resolutions,
    defaultResolution: "720p",
    aspectRatios: SEEDANCE_RATIOS,
    defaultAspectRatio: "16:9",
  };
  if (inputs && inputs.length) profile.inputs = inputs;
  if (options) profile.options = options;
  return profile;
}

const DURATION_1X = { values: [5, 10], default: 5 };
const DURATION_20 = { min: 4, max: 15, step: 1, default: 5 };
const DURATION_25 = { min: 4, max: 30, step: 1, default: 5 };
const RESOLUTIONS_1X = ["480p", "720p", "1080p"];
const RESOLUTIONS_2X = ["480p", "720p"];
const VIDEO_OPTIONS = { generateAudio: true, watermark: true, returnLastFrame: true };
const FIRST_FRAME_INPUT = [{ name: "image", kind: "image", sources: SEEDANCE_PROFILE_SOURCES, maxItems: 1, required: true }];
const FRAME_PAIR_INPUTS = [
  { name: "image", kind: "image", sources: SEEDANCE_PROFILE_SOURCES, maxItems: 1, required: true },
  { name: "last_frame", kind: "image", sources: SEEDANCE_PROFILE_SOURCES, maxItems: 1, required: true },
];

function referenceInputs(imageLimit, audioLimit, videoLimit) {
  return [
    { name: "reference_images", kind: "image", sources: SEEDANCE_PROFILE_SOURCES, maxItems: imageLimit },
    { name: "reference_videos", kind: "video", sources: SEEDANCE_PROFILE_SOURCES, maxItems: videoLimit || 3 },
    { name: "reference_audios", kind: "audio", sources: SEEDANCE_PROFILE_SOURCES, maxItems: audioLimit },
  ];
}

function seedanceOperationMode(name, duration, resolutions, imageLimit, totalVideoLimit, audioLimit) {
  const mode = {
    name,
    inputs: [
      { name: "video", kind: "video", sources: SEEDANCE_PROFILE_SOURCES, maxItems: 1, required: true },
      { name: "reference_images", kind: "image", sources: SEEDANCE_PROFILE_SOURCES, maxItems: imageLimit },
      { name: "reference_videos", kind: "video", sources: SEEDANCE_PROFILE_SOURCES, maxItems: totalVideoLimit - 1 },
      { name: "reference_audios", kind: "audio", sources: SEEDANCE_PROFILE_SOURCES, maxItems: audioLimit },
    ],
    options: VIDEO_OPTIONS,
  };
  if (duration) mode.duration = duration;
  if (resolutions) {
    mode.resolutions = resolutions;
    mode.defaultResolution = "720p";
  }
  if (name === "extend_video") {
    mode.extensionDirections = ["forward", "backward"];
    mode.defaultExtensionDirection = "backward";
  }
  return mode;
}

export const meta = {
  apiVersion: 1,
  key: "doubao",
  name: "Doubao Video",
  icon: "Doubao.Color",
  description: {
    en: "Volcengine Doubao Seedance video generation (text-to-video, image-to-video, and video-to-video)",
    zh: "火山引擎豆包 Seedance 视频生成（文生视频、图生视频、视频生视频）",
  },
  version: "1.2.0",
  author: { name: "QuantumNous" },
  channelTypes: [54, 45], // VolcEngine-type channels serve Ark video models with the same wire format
  models: [
    "doubao-seedance-1-0-pro-250528",
    "doubao-seedance-1-0-lite-t2v",
    "doubao-seedance-1-0-lite-i2v",
    "doubao-seedance-1-5-pro-251215",
    "doubao-seedance-2-0-260128",
    "doubao-seedance-2-0-fast-260128",
    "doubao-seedance-2-0-mini-260615",
    "doubao-seedance-2-5-260628",
  ],
  fetchMode: "per_task",
  usageSchema: {
    // Upstream billing tokens (estimated at submit, actual on completion).
    tokens: {
      type: "number",
      unit: "token",
      description: { en: "Billing token unit price", zh: "计费 Token 单价" },
    },
    // Output video resolution; Seedance token unit price varies by resolution tier.
    resolution: {
      enum: ["480p", "720p", "1080p", "4k"],
      enumLabels: {
        "480p": { en: "480p", zh: "480p" },
        "720p": { en: "720p", zh: "720p" },
        "1080p": { en: "1080p", zh: "1080p" },
        "4k": { en: "4k", zh: "4k" },
      },
      description: { en: "Output video resolution", zh: "输出视频分辨率" },
    },
    // Whether the request includes reference video input; Seedance prices video-to-video tokens at a lower unit rate.
    video_input: {
      enum: ["none", "video"],
      enumLabels: { none: { en: "No reference video", zh: "无参考视频" }, video: { en: "With reference video", zh: "有参考视频" } },
      description: { en: "Reference video input", zh: "参考视频输入" },
    },
  },
  // Official Ark formula tokens = (input + output seconds) × W × H × 24 / 1024,
  // 16:9 max-pixel sizes, cross-checked against Volcengine price examples.
  usageExamples: [
    { label: "480p · 5s", facts: { tokens: 48038, resolution: "480p", video_input: "none" } },
    { label: "720p · 5s", facts: { tokens: 108000, resolution: "720p", video_input: "none" } },
    { label: "1080p · 5s", facts: { tokens: 243000, resolution: "1080p", video_input: "none" } },
    { label: "4k · 5s", facts: { tokens: 972000, resolution: "4k", video_input: "none" } },
    { label: "720p · 10s", facts: { tokens: 216000, resolution: "720p", video_input: "none" } },
    { label: "720p · 5s (+4s 输入视频)", facts: { tokens: 194400, resolution: "720p", video_input: "video" } },
  ],
  videoProfiles: [
    {
      models: ["doubao-seedance-1-0-pro-250528"],
      modes: [
        seedanceMode("text_to_video", DURATION_1X, RESOLUTIONS_1X),
        seedanceMode("image_to_video", DURATION_1X, RESOLUTIONS_1X, FIRST_FRAME_INPUT),
        seedanceMode("first_last_frame", DURATION_1X, RESOLUTIONS_1X, FRAME_PAIR_INPUTS),
      ],
    },
    {
      models: ["doubao-seedance-1-0-lite-t2v"],
      modes: [seedanceMode("text_to_video", DURATION_1X, RESOLUTIONS_1X)],
    },
    {
      models: ["doubao-seedance-1-0-lite-i2v"],
      modes: [
        seedanceMode("image_to_video", DURATION_1X, RESOLUTIONS_1X, FIRST_FRAME_INPUT),
        seedanceMode("reference_to_video", DURATION_1X, RESOLUTIONS_1X, [
          { name: "reference_images", kind: "image", sources: SEEDANCE_PROFILE_SOURCES, maxItems: 4, required: true },
        ]),
        seedanceMode("first_last_frame", DURATION_1X, RESOLUTIONS_1X, FRAME_PAIR_INPUTS),
      ],
    },
    {
      models: ["doubao-seedance-1-5-pro-251215"],
      modes: [
        seedanceMode("text_to_video", DURATION_1X, RESOLUTIONS_1X, [], { generateAudio: true, watermark: true, returnLastFrame: true }),
        seedanceMode("image_to_video", DURATION_1X, RESOLUTIONS_1X, FIRST_FRAME_INPUT, { generateAudio: true, watermark: true, returnLastFrame: true }),
        seedanceMode("first_last_frame", DURATION_1X, RESOLUTIONS_1X, FRAME_PAIR_INPUTS, { generateAudio: true, watermark: true, returnLastFrame: true }),
      ],
    },
    {
      models: ["doubao-seedance-2-0-260128"],
      modes: [
        seedanceMode("text_to_video", DURATION_20, ["480p", "720p", "1080p", "4k"], [], VIDEO_OPTIONS),
        seedanceMode("image_to_video", DURATION_20, RESOLUTIONS_2X, FIRST_FRAME_INPUT, VIDEO_OPTIONS),
        seedanceMode("reference_to_video", DURATION_20, RESOLUTIONS_2X, referenceInputs(9, 3), VIDEO_OPTIONS),
        seedanceMode("first_last_frame", DURATION_20, RESOLUTIONS_2X, FRAME_PAIR_INPUTS, VIDEO_OPTIONS),
        seedanceOperationMode("edit_video", DURATION_20, ["480p", "720p", "1080p", "4k"], 9, 3, 3),
        seedanceOperationMode("extend_video", DURATION_20, ["480p", "720p", "1080p", "4k"], 9, 3, 3),
      ],
    },
    {
      models: ["doubao-seedance-2-0-fast-260128", "doubao-seedance-2-0-mini-260615"],
      modes: [
        seedanceMode("text_to_video", DURATION_20, RESOLUTIONS_2X, [], VIDEO_OPTIONS),
        seedanceMode("image_to_video", DURATION_20, RESOLUTIONS_2X, FIRST_FRAME_INPUT, VIDEO_OPTIONS),
        seedanceMode("reference_to_video", DURATION_20, RESOLUTIONS_2X, referenceInputs(9, 3), VIDEO_OPTIONS),
        seedanceMode("first_last_frame", DURATION_20, RESOLUTIONS_2X, FRAME_PAIR_INPUTS, VIDEO_OPTIONS),
        seedanceOperationMode("edit_video", DURATION_20, RESOLUTIONS_2X, 9, 3, 3),
        seedanceOperationMode("extend_video", DURATION_20, RESOLUTIONS_2X, 9, 3, 3),
      ],
    },
    {
      models: ["doubao-seedance-2-5-260628"],
      modes: [
        seedanceMode("text_to_video", DURATION_25, RESOLUTIONS_2X, [], VIDEO_OPTIONS),
        seedanceMode("image_to_video", DURATION_25, RESOLUTIONS_2X, FIRST_FRAME_INPUT, VIDEO_OPTIONS),
        seedanceMode("reference_to_video", DURATION_25, RESOLUTIONS_2X, referenceInputs(30, 10, 10), VIDEO_OPTIONS),
        seedanceMode("first_last_frame", DURATION_25, RESOLUTIONS_2X, FRAME_PAIR_INPUTS, VIDEO_OPTIONS),
        seedanceOperationMode("edit_video", null, RESOLUTIONS_2X, 30, 10, 10),
        seedanceOperationMode("extend_video", DURATION_25, RESOLUTIONS_2X, 30, 10, 10),
      ],
    },
  ],
  routes: [
    { method: "POST", path: "/doubao/api/v3/contents/generations/tasks", type: "submit", decode: "createTask", render: "taskCreated" },
    { method: "GET", path: "/doubao/api/v3/contents/generations/tasks/:task_id", type: "query", render: "taskStatus" },
  ],
  protocols: [{ name: "openai_responses", supports: ["stream", "sync", "background"] }, "openai_video"],
};

function trimmed(value) {
  return String(value || "").trim();
}

function draftTaskIds(content) {
  const ids = [];
  if (!Array.isArray(content)) return ids;
  for (const item of content) {
    if (!item || typeof item !== "object" || Array.isArray(item)) continue;
    if (item.type !== "draft_task") continue;
    const draft = item.draft_task;
    if (!draft || typeof draft !== "object" || Array.isArray(draft)) continue;
    const id = trimmed(draft.id);
    if (id) ids.push(id);
  }
  return ids;
}

function rewriteDraftTaskContent(content, originTasks) {
  if (!Array.isArray(content)) return content;
  return content.map(function (item) {
    if (!item || typeof item !== "object" || Array.isArray(item) || item.type !== "draft_task") return item;
    const draft = item.draft_task;
    if (!draft || typeof draft !== "object" || Array.isArray(draft) || !trimmed(draft.id)) return item;
    const publicId = trimmed(draft.id);
    let upstream = "";
    if (Array.isArray(originTasks)) {
      for (const task of originTasks) {
        if (task && task.taskId === publicId) {
          upstream = trimmed(task.upstreamTaskId);
          break;
        }
      }
    }
    if (!upstream) throw new Error("origin task is unavailable");
    return { ...item, draft_task: Object.assign({}, draft, { id: upstream }) };
  });
}

function normalizeResolution(value) {
  const raw = trimmed(value).toLowerCase();
  if (["480p", "720p", "1080p", "4k"].includes(raw)) return raw;
  const parts = raw.replace("*", "x").split("x");
  if (parts.length !== 2) return "";
  const max = Math.max(Number(parts[0]), Number(parts[1]));
  if (!Number.isFinite(max) || max <= 0) return "";
  if (max >= 3840) return "4k";
  if (max >= 1920) return "1080p";
  if (max >= 1280) return "720p";
  return "480p";
}

function hasVideo(content) {
  return Array.isArray(content) && content.some((item) => item && (item.type === "video_url" || Object.hasOwn(item, "video_url")));
}

function seedanceMediaURL(value, name) {
  let url = value;
  if (value && typeof value === "object" && !Array.isArray(value)) {
    if (Object.hasOwn(value, "file_id")) {
      throw new Error(name + " does not support file_id; use a public http(s) URL or asset:// ID");
    }
    url = value.url;
    if (!url && value.image_url) url = typeof value.image_url === "string" ? value.image_url : value.image_url.url;
    if (!url && value.video_url) url = typeof value.video_url === "string" ? value.video_url : value.video_url.url;
    if (!url && value.audio_url) url = typeof value.audio_url === "string" ? value.audio_url : value.audio_url.url;
  }
  url = trimmed(url);
  if (!url || (!/^https?:\/\//i.test(url) && !/^asset:\/\//i.test(url))) {
    throw new Error(name + " must use a public http(s) URL or asset:// ID; Seedance file uploads are not supported");
  }
  return url;
}

function seedanceValues(value, name) {
  if (value === undefined || value === null || value === "") return [];
  if (!Array.isArray(value)) throw new Error(`${name} must be an array`);
  return value;
}

function seedanceMediaItem(type, role, url) {
  const field = `${type.replace("_url", "")}_url`;
  const item = { type, role };
  item[field] = { url };
  return item;
}

function seedanceContent(req, action, extensionDirection) {
  if (req.metadata !== undefined && (!req.metadata || typeof req.metadata !== "object" || Array.isArray(req.metadata))) {
    throw new Error("metadata must be an object");
  }
  const metadata = req.metadata || {};
  const content = [];
  if (req.video !== undefined && req.video !== null && req.video !== "") {
    content.push(seedanceMediaItem("video_url", "reference_video", seedanceMediaURL(req.video, "video")));
  }
  const first = req.image === undefined ? req.input_reference : req.image;
  let hasFirst = first !== undefined && first !== null && first !== "";
  if (hasFirst) content.push(seedanceMediaItem("image_url", "first_frame", seedanceMediaURL(first, "image")));

  const images = seedanceValues(req.images, "images");
  for (let index = 0; index < images.length; index++) {
    const role = !hasFirst && index === 0 ? "first_frame" : "reference_image";
    content.push(seedanceMediaItem("image_url", role, seedanceMediaURL(images[index], `images[${index}]`)));
    if (role === "first_frame") hasFirst = true;
  }
  if (req.last_frame !== undefined && req.last_frame !== null && req.last_frame !== "") {
    content.push(seedanceMediaItem("image_url", "last_frame", seedanceMediaURL(req.last_frame, "last_frame")));
  }

  for (const entry of [
    ["reference_images", "image_url", "reference_image"],
    ["reference_videos", "video_url", "reference_video"],
    ["reference_audios", "audio_url", "reference_audio"],
  ]) {
    const values = seedanceValues(req[entry[0]], entry[0]);
    for (let index = 0; index < values.length; index++) {
      content.push(seedanceMediaItem(entry[1], entry[2], seedanceMediaURL(values[index], `${entry[0]}[${index}]`)));
    }
  }

  const nativeContent = metadata.content === undefined ? [] : metadata.content;
  if (!Array.isArray(nativeContent)) throw new Error("metadata.content must be an array");
  const nativeTexts = [];
  for (let index = 0; index < nativeContent.length; index++) {
    const item = nativeContent[index];
    if (!item || typeof item !== "object" || Array.isArray(item)) throw new Error("metadata.content entries must be objects");
    if (item.type === "text") {
      if (typeof item.text === "string" && trimmed(item.text)) nativeTexts.push(item.text);
      continue;
    }
    if (item.type === "video_url" || item.role === "reference_video" || item.video_url !== undefined) {
      seedanceMediaURL(item.video_url === undefined ? item.url : item.video_url, `metadata.content[${index}] video`);
    }
    content.push(item);
  }
  const userPrompt = trimmed(req.prompt) || nativeTexts.join("\n");
  let prompt = userPrompt;
  if (action === "edit_video") prompt = `视频编辑任务：请基于 @视频1 进行编辑。\n${userPrompt}`;
  if (action === "extend_video") {
    const directionText = extensionDirection === "forward" ? "向前" : "向后";
    prompt = `视频延长任务：请${directionText}延长 @视频1。\n${userPrompt}`;
  }
  if (prompt || content.length === 0) content.push({ type: "text", text: prompt });
  return { metadata, content, userPrompt };
}

function seedanceContentFacts(content) {
  const facts = { firstFrames: 0, lastFrames: 0, referenceImages: 0, referenceVideos: 0, referenceAudios: 0 };
  for (const item of content || []) {
    if (!item || typeof item !== "object" || Array.isArray(item) || item.type === "text") continue;
    if (item.type === "draft_task") {
      facts.referenceVideos++;
      continue;
    }
    const role = trimmed(item.role);
    const type = trimmed(item.type);
    if (role === "first_frame") facts.firstFrames++;
    else if (role === "last_frame") facts.lastFrames++;
    else if (!role && type === "image_url" && facts.firstFrames === 0) facts.firstFrames++;
    else if (role === "reference_video" || type === "video_url") facts.referenceVideos++;
    else if (role === "reference_audio" || type === "audio_url") facts.referenceAudios++;
    else if (role === "reference_image" || type === "image_url") facts.referenceImages++;
  }
  return facts;
}

function seedanceAction(facts) {
  if (facts.lastFrames > 0) return "first_last_frame";
  if (facts.referenceImages > 0 || facts.referenceVideos > 0 || facts.referenceAudios > 0) return "reference_to_video";
  if (facts.firstFrames > 0) return "image_to_video";
  return "text_to_video";
}

function seedanceModeProfile(model, action) {
  for (const profile of meta.videoProfiles || []) {
    if (!profile.models.includes(model)) continue;
    for (const mode of profile.modes) {
      if (mode.name === action) return mode;
    }
    return null;
  }
  return undefined;
}

function validateSeedanceDuration(duration, profile) {
  if (!Number.isInteger(duration)) throw new Error("duration must be an integer");
  if (Array.isArray(profile.duration.values)) {
    if (!profile.duration.values.includes(duration)) throw new Error("duration is not supported by this Seedance model");
    return;
  }
  if (duration < profile.duration.min || duration > profile.duration.max || (duration - profile.duration.min) % profile.duration.step !== 0) {
    throw new Error("duration is outside this Seedance model's supported range");
  }
}

function validateSeedanceRequest(model, body, action, facts) {
  const profile = seedanceModeProfile(model, action);
  if (profile === null) throw new Error(`${action} is not supported by ${model}`);
  if (profile === undefined) {
    const seconds = Number(body.duration || 5);
    if (!Number.isInteger(seconds) || seconds < 1 || seconds > 3600) throw new Error("duration must be between 1 and 3600");
    return;
  }
  if (profile.duration) {
    const duration = body.duration === undefined ? profile.duration.default : Number(body.duration);
    validateSeedanceDuration(duration, profile);
    body.duration = duration;
  } else if (body.duration !== undefined) {
    throw new Error("duration is fixed by this Seedance model and mode");
  }
  if (profile.resolutions) {
    body.resolution = normalizeResolution(body.resolution || "720p");
    if (!profile.resolutions.includes(body.resolution)) throw new Error("resolution is not supported for this Seedance model and mode");
  } else if (body.resolution !== undefined) {
    throw new Error("resolution is fixed by this Seedance model and mode");
  }
  if (Array.isArray(profile.aspectRatios) && profile.aspectRatios.length) {
    body.ratio = trimmed(body.ratio || body.aspect_ratio) || profile.defaultAspectRatio;
    delete body.aspect_ratio;
    if (!profile.aspectRatios.includes(body.ratio)) throw new Error("ratio is not supported by Seedance");
  } else if (action === "edit_video" || action === "extend_video") {
    body.ratio = "adaptive";
    delete body.aspect_ratio;
  }
  if (facts.firstFrames > 1) throw new Error("only one first_frame is supported");
  if (facts.lastFrames > 1) throw new Error("only one last_frame is supported");
  if (action === "first_last_frame" && facts.firstFrames !== 1) throw new Error("first_last_frame requires one first_frame");
  const primaryVideos = action === "edit_video" || action === "extend_video" ? 1 : 0;
  const counts = {
    video: primaryVideos,
    image: facts.firstFrames,
    last_frame: facts.lastFrames,
    reference_images: facts.referenceImages,
    reference_videos: Math.max(0, facts.referenceVideos - primaryVideos),
    reference_audios: facts.referenceAudios,
  };
  const declaredInputs = new Set((profile.inputs || []).map((input) => input.name));
  if (action === "edit_video" || action === "extend_video") {
    for (const name of Object.keys(counts)) {
      if (counts[name] > 0 && !declaredInputs.has(name)) {
        throw new Error(name + " is not supported by this Seedance model and mode");
      }
    }
  }
  for (const input of profile.inputs || []) {
    if (input.required && !counts[input.name]) throw new Error(input.name + " is required for this model and mode");
    if (Object.hasOwn(counts, input.name) && counts[input.name] > input.maxItems) {
      throw new Error(input.name + " supports at most " + input.maxItems + " items for this model");
    }
  }
  if (action === "reference_to_video" && facts.referenceImages + facts.referenceVideos + facts.referenceAudios === 0) {
    throw new Error("reference_to_video requires reference media");
  }
  if (model.indexOf("seedance-2-0-") >= 0 && facts.referenceAudios > 0 && facts.firstFrames + facts.referenceImages + facts.referenceVideos === 0) {
    throw new Error("Seedance 2.0 reference audio requires an image or video reference");
  }
  if ((action === "edit_video" || action === "extend_video") && facts.referenceImages > 0 && !["480p", "720p"].includes(body.resolution)) {
    throw new Error("Seedance operations with reference images support at most 720p");
  }
  for (const name of ["generate_audio", "watermark", "return_last_frame"]) {
    if (body[name] !== undefined && typeof body[name] !== "boolean") throw new Error(`${name} must be a boolean`);
  }
  const optionNames = { generate_audio: "generateAudio", watermark: "watermark", return_last_frame: "returnLastFrame" };
  for (const name of Object.keys(optionNames)) {
    if (body[name] === true && !(profile.options && profile.options[optionNames[name]])) {
      throw new Error(name + " is not supported by this Seedance model and mode");
    }
  }
}

function prepareSeedanceBody(req, model, originTasks, forcedAction) {
  const operation = forcedAction === "edit_video" || forcedAction === "extend_video" ? forcedAction : "";
  const metadata = req.metadata && typeof req.metadata === "object" && !Array.isArray(req.metadata) ? req.metadata : {};
  const hasField = function (...names) {
    return names.some(function (name) {
      return Object.hasOwn(req, name) || Object.hasOwn(metadata, name);
    });
  };
  if (!operation && Object.hasOwn(req, "video")) throw new Error("video is only supported by Seedance edit and extension endpoints");
  if (operation) {
    if (!trimmed(req.prompt)) throw new Error("prompt is required for Seedance video editing and extension");
    if (!Object.hasOwn(req, "video") || req.video === null || (typeof req.video === "string" && !trimmed(req.video))) {
      throw new Error("video is required for Seedance video editing and extension");
    }
    for (const name of ["image", "input_reference", "images", "last_frame", "ratio", "aspect_ratio"]) {
      if (hasField(name)) throw new Error(name + " is not supported for this Seedance video operation");
    }
    for (const name of ["action", "operation", "mode", "source_task_id"]) {
      if (hasField(name)) throw new Error(name + " cannot override the Seedance video operation");
    }
    for (const name of ["model", "provider", "prompt", "video", "extension_direction"]) {
      if (Object.hasOwn(metadata, name)) throw new Error("metadata." + name + " cannot override a structured Seedance video field");
    }
  }
  if (operation === "extend_video" && hasField("duration") && hasField("seconds")) {
    throw new Error("duration and seconds cannot both be provided");
  }
  const direction = operation === "extend_video" ? trimmed(req.extension_direction) || "backward" : "";
  if (operation === "extend_video" && direction !== "forward" && direction !== "backward") {
    throw new Error("extension_direction must be forward or backward");
  }
  if (operation !== "extend_video" && hasField("extension_direction")) {
    throw new Error("extension_direction is only supported for Seedance video extension");
  }
  const prepared = seedanceContent(req, operation, direction);
  const body = { model: model, content: [], ...prepared.metadata };
  body.content = prepared.content;
  const seconds = req.seconds === undefined ? req.duration : req.seconds;
  if (seconds !== undefined) body.duration = Number(seconds);
  if (req.resolution !== undefined) body.resolution = req.resolution;
  else if (req.size !== undefined) {
    body.resolution = normalizeResolution(req.size);
    if (!body.resolution) throw new Error("size must be a supported resolution or valid pixel size");
  }
  if (req.ratio !== undefined) body.ratio = req.ratio;
  else if (req.aspect_ratio !== undefined) body.ratio = req.aspect_ratio;
  for (const name of ["generate_audio", "watermark", "return_last_frame"]) {
    if (req[name] !== undefined) body[name] = req[name];
  }
  const facts = seedanceContentFacts(body.content);
  const action = operation || seedanceAction(facts);
  validateSeedanceRequest(model, body, action, facts);
  if (action === "edit_video" && model === "doubao-seedance-2-5-260628") body.duration = -1;
  delete body.seconds;
  delete body.size;
  delete body.video;
  delete body.extension_direction;
  body.model = model;
  if (originTasks !== undefined) body.content = rewriteDraftTaskContent(body.content, originTasks);
  return { body, action, facts };
}

// Max-pixel 16:9 dimensions per resolution tier. Used when ratio is absent or
// adaptive so the submit-time estimate overestimates rather than underestimates.
// Official Ark formula: tokens = seconds × width × height × 24 / 1024.
// Video input duration is omitted; extractUsageOnComplete overlays the real bill.
function resolutionMaxPixels(resolution) {
  if (resolution === "480p") return [854, 480];
  if (resolution === "1080p") return [1920, 1080];
  if (resolution === "4k") return [3840, 2160];
  return [1280, 720];
}

function estimateTokens(seconds, resolution) {
  const dims = resolutionMaxPixels(resolution);
  return (seconds * dims[0] * dims[1] * 24) / 1024;
}

function videoInputRatio(model, resolution, content) {
  const video = hasVideo(content);
  const res = trimmed(resolution).toLowerCase();
  if (model === "doubao-seedance-2-5-260628") {
    if (res === "1080p") return video ? 7.0 / 10.7 : 11.7 / 10.7;
    return video ? 42 / 70 : 1;
  }
  if (model === "doubao-seedance-2-0-260128") {
    if (res === "1080p") return video ? 31 / 46 : 51 / 46;
    if (res === "4k") return video ? 16 / 46 : 26 / 46;
    return video ? 28 / 46 : 1;
  }
  if (model === "doubao-seedance-2-0-fast-260128") return video ? 22 / 37 : 1;
  if (model === "doubao-seedance-2-0-mini-260615") return video ? 14 / 23 : 1;
  return 1;
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
      let content = [item];
      if (item.content !== undefined) content = Array.isArray(item.content) ? item.content : [item.content];
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
    images,
  };
}

function responsesVideoText(ctx) {
  const artifact = ctx && ctx.artifacts && ctx.artifacts.video;
  const url = trimmed(artifact && artifact.url);
  if (!url) throw new Error("video artifact is unavailable");
  const escaped = url.replaceAll("&", "&amp;").replaceAll('"', "&quot;").replaceAll("<", "&lt;").replaceAll(">", "&gt;");
  return `<video controls src="${escaped}"></video>`;
}

export const native = {
  createTask(ctx) {
    if (!ctx.body || ctx.body.kind !== "json") throw new Error("JSON body required");
    const body = ctx.body.value;
    if (!body || typeof body !== "object" || Array.isArray(body)) throw new Error("request body must be an object");
    const model = trimmed(body.model);
    if (!model) throw new Error("model is required");
    if (body.content !== undefined && !Array.isArray(body.content)) throw new Error("content must be an array");
    const content = Array.isArray(body.content) ? body.content : [];
    const texts = [];
    let hasReference = false;
    for (const item of content) {
      if (!item || typeof item !== "object" || Array.isArray(item)) continue;
      if (item.type === "text" && typeof item.text === "string") texts.push(item.text);
      else hasReference = true;
    }
    if (!texts.length && !hasReference) throw new Error("content is required");
    const requestBody = {
      model: model,
      prompt: texts
        .filter(function (text) {
          return trimmed(text);
        })
        .join("\n"),
      metadata: body,
    };
    const seconds = Number(body.duration);
    if (Number.isFinite(seconds) && seconds > 0) requestBody.seconds = seconds;
    const intent = { kind: "submit", model: model, action: hasReference ? "image_to_video" : "text_to_video", requestBody: requestBody };
    const originTaskIds = draftTaskIds(content);
    if (originTaskIds.length) intent.originTaskIds = originTaskIds;
    return intent;
  },
  taskCreated(ctx, task) {
    const data = task.data && typeof task.data === "object" && !Array.isArray(task.data) ? task.data : {};
    return Object.assign({}, data, { id: task.task_id });
  },
  taskStatus(ctx, task) {
    if (task.data && typeof task.data === "object" && !Array.isArray(task.data)) return Object.assign({}, task.data, { id: task.task_id });
    const statusMap = { NOT_START: "queued", SUBMITTED: "queued", QUEUED: "queued", IN_PROGRESS: "running", SUCCESS: "succeeded", FAILURE: "failed" };
    const output = { id: task.task_id, status: statusMap[task.status] || "queued" };
    if (task.fail_reason) output.error = { message: task.fail_reason };
    return output;
  },
  error(ctx, error) {
    return { error: { code: error.code, message: error.message } };
  },
};

export function buildSubmitRequest(ctx) {
  const req = ctx.requestBody || {};
  const prepared = prepareSeedanceBody(req, ctx.upstreamModel || ctx.model || req.model, ctx.originTasks, ctx.action);
  return {
    url: `${ctx.baseUrl}/api/v3/contents/generations/tasks`,
    method: "POST",
    headers: { "Content-Type": "application/json", Accept: "application/json", Authorization: `Bearer ${ctx.apiKey}` },
    body: prepared.body,
    action: prepared.action,
    rewriteModel: prepared.body.model,
  };
}

export function parseSubmitResponse(ctx, resp) {
  if (!resp.body || !resp.body.id) throw new Error("task_id is empty");
  return { taskId: resp.body.id, taskData: resp.body };
}

export function extractUsage(ctx) {
  const req = ctx.requestBody || {};
  const metadata = req.metadata || {};
  const content = seedanceContent(req, ctx.action, req.extension_direction || metadata.extension_direction).content;
  if (ctx.usagePurpose === "billing_ratios") {
    const ratio = videoInputRatio(ctx.upstreamModel || ctx.model, req.resolution || metadata.resolution || req.size, content);
    return ratio === 1 ? null : { video_input_ratio: ratio };
  }
  let seconds = Number(req.seconds || req.duration || metadata.duration || 0);
  if (!Number.isFinite(seconds) || seconds <= 0) {
    const frames = Number(metadata.frames);
    seconds = Number.isFinite(frames) && frames > 0 ? Math.floor(frames / 24) : 5;
  }
  if (seconds <= 0) seconds = 5;
  seconds = Math.min(seconds, 3600);
  if (ctx.action === "edit_video" || ctx.action === "extend_video") {
    const inputSeconds = (ctx.upstreamModel || ctx.model) === "doubao-seedance-2-5-260628" ? 30 : 15;
    const outputSeconds = ctx.action === "edit_video" && (ctx.upstreamModel || ctx.model) === "doubao-seedance-2-5-260628" ? 30 : seconds;
    seconds = inputSeconds + outputSeconds;
  }
  const rawResolution = req.resolution || metadata.resolution || req.size;
  const raw = trimmed(rawResolution).toLowerCase();
  const recognized = ["480p", "720p", "1080p", "4k"].includes(raw) || raw.replace("*", "x").split("x").length === 2;
  const resolution = recognized ? normalizeResolution(rawResolution) : "720p";
  return {
    tokens: estimateTokens(seconds, resolution),
    resolution,
    video_input: hasVideo(content) ? "video" : "none",
  };
}

export function buildQueryRequest(ctx) {
  return {
    url: `${ctx.baseUrl}/api/v3/contents/generations/tasks/${ctx.taskId}`,
    method: "GET",
    headers: { Accept: "application/json", "Content-Type": "application/json", Authorization: `Bearer ${ctx.apiKey}` },
  };
}

export function parseTaskResult(ctx, body) {
  if (body.status === "pending" || body.status === "queued") return { status: "QUEUED", progress: "10%" };
  if (body.status === "processing" || body.status === "running") return { status: "IN_PROGRESS", progress: "50%" };
  if (body.status === "succeeded") {
    const result = { status: "SUCCESS", progress: "100%", url: body.content && body.content.video_url ? body.content.video_url : "" };
    const usage = body.usage || {};
    const completionTokens = Number(usage.completion_tokens || 0);
    const totalTokens = Number(usage.total_tokens || 0);
    if (Number.isFinite(completionTokens) && completionTokens > 0) result.completionTokens = completionTokens;
    if (Number.isFinite(totalTokens) && totalTokens > 0) result.totalTokens = totalTokens;
    return result;
  }
  if (body.status === "failed" || body.status === "expired" || body.status === "cancelled") {
    const reason = body.error && body.error.message ? body.error.message : body.status;
    return { status: "FAILURE", progress: "100%", reason };
  }
  return { status: "UNKNOWN", reason: `unrecognized status: ${String(body.status || "")}` };
}

function artifactData(ctx) {
  const data = (ctx && ctx.data) || {};
  if (data.data && typeof data.data === "object" && data.data.task_id && Object.hasOwn(data.data, "data")) return data.data.data || {};
  return data;
}

export function listArtifacts(task) {
  if (task.status !== "SUCCESS") return [];
  const content = artifactData(task).content || {};
  const artifacts = [];
  if (trimmed(content.video_url)) artifacts.push({ key: "video", type: "video" });
  if (trimmed(content.last_frame_url)) artifacts.push({ key: "last_frame", type: "image", mimeType: "image/png" });
  return artifacts;
}

export function buildContentRequest(ctx) {
  const content = artifactData(ctx).content || {};
  const urls = { video: content.video_url, last_frame: content.last_frame_url };
  const url = trimmed(urls[ctx.artifactKey]);
  if (!url) throw new Error("artifact_not_found");
  return { url, method: ctx.clientRequest.method, credentialless: true };
}

export function extractUsageOnComplete(task, taskResult, body) {
  if (!body || body.status !== "succeeded") return {};
  const facts = {};
  const usage = body.usage || {};
  let tokens = Number(usage.completion_tokens);
  if (!Number.isFinite(tokens) || tokens <= 0) tokens = Number(usage.total_tokens);
  if (Number.isFinite(tokens) && tokens > 0) facts.tokens = tokens;
  const content = body.content || {};
  const resolution = trimmed(content.resolution || body.resolution).toLowerCase();
  if (["480p", "720p", "1080p", "4k"].includes(resolution)) facts.resolution = resolution;
  return facts;
}

export const protocols = {
  openai_responses: {
    decodeRequest(ctx) {
      if (!ctx.body || ctx.body.kind !== "json") throw new Error("JSON body required");
      const req = ctx.body.value;
      if (!req || typeof req !== "object" || Array.isArray(req)) throw new Error("request body must be an object");
      const model = trimmed(req.model);
      if (!model) throw new Error("model is required");
      if (req.input !== undefined && typeof req.input !== "string" && !Array.isArray(req.input)) throw new Error("input must be a string or array");
      if (req.images !== undefined && !Array.isArray(req.images)) throw new Error("images must be an array");
      if (req.metadata !== undefined && (!req.metadata || typeof req.metadata !== "object" || Array.isArray(req.metadata))) {
        throw new Error("metadata must be an object");
      }
      const input = responsesInput(req);
      const prompt = input.prompt || trimmed(req.prompt);
      const images = [];
      for (const image of [req.image, req.input_reference, ...(req.images || []), ...input.images]) {
        if (trimmed(image) && !images.includes(trimmed(image))) images.push(trimmed(image));
      }
      if (!prompt && images.length === 0) throw new Error("input is required");
      const metadata = Object.assign({}, req.metadata || {});
      if (Object.prototype.hasOwnProperty.call(req, "resolution")) metadata.resolution = req.resolution;
      else if (req.size && !metadata.resolution) {
        metadata.resolution = normalizeResolution(req.size);
        if (!metadata.resolution) throw new Error("size must be a supported resolution or valid pixel size");
      }
      const requestBody = { model: model, prompt: prompt, metadata: metadata };
      if (images.length) requestBody.images = images;
      if (Object.prototype.hasOwnProperty.call(req, "seconds")) requestBody.seconds = req.seconds;
      else if (Object.prototype.hasOwnProperty.call(req, "duration")) requestBody.seconds = req.duration;
      if (Object.prototype.hasOwnProperty.call(req, "size")) requestBody.size = req.size;
      const intent = { kind: "submit", model: model, action: images.length ? "image_to_video" : "text_to_video", requestBody: requestBody };
      const originTaskIds = draftTaskIds(metadata.content);
      if (originTaskIds.length) intent.originTaskIds = originTaskIds;
      return intent;
    },
    renderEvents(ctx, task, previousState) {
      const status = String(task.status || "UNKNOWN").toUpperCase();
      const value = Number(String(task.progress || "").replace("%", ""));
      const progress = Number.isFinite(value) && value >= 0 && value <= 100 ? value : null;
      const state = { status: status, progress: progress };
      if (status === "SUCCESS") {
        const text = responsesVideoText(ctx);
        const events = previousState && previousState.status === status ? [] : [{ type: "output", data: text }];
        return { events: events, state: state, done: true };
      }
      if (status === "FAILURE") {
        return { events: [{ type: "error", code: "task_failed", message: task.fail_reason || "task failed" }], state: state, done: true };
      }
      if (previousState && previousState.status === status && previousState.progress === progress) return { events: [], state: state, done: false };
      const event = { type: "progress", message: status.toLowerCase() };
      if (progress !== null) event.progress = progress;
      return { events: [event], state: state, done: false };
    },
    renderFinal(ctx, _task) {
      return {
        output: [
          {
            type: "message",
            status: "completed",
            role: "assistant",
            content: [{ type: "output_text", text: responsesVideoText(ctx), annotations: [], logprobs: [] }],
          },
        ],
        metadata: { vendor: "doubao" },
      };
    },
  },
};

const legacyRenderers = {
  openai_video(task) {
    const data = task.data || {};
    const statusMap = { NOT_START: "queued", SUBMITTED: "queued", QUEUED: "queued", IN_PROGRESS: "in_progress", SUCCESS: "completed", FAILURE: "failed" };
    const output = {
      id: task.task_id,
      object: "video",
      model: task.properties ? task.properties.origin_model_name || "" : "",
      status: statusMap[task.status] || "unknown",
      progress: Number(String(task.progress || "0").replace("%", "")),
      created_at: task.created_at,
      completed_at: task.updated_at,
    };
    if (data.status === "failed") output.error = { message: data.error ? data.error.message || "" : "", code: data.error ? data.error.code || "" : "" };
    return output;
  },
};

protocols.openai_video = {
  decodeRequest(ctx) {
    if (!ctx.body || (ctx.body.kind !== "json" && ctx.body.kind !== "multipart")) throw new Error("JSON or multipart body required");
    let req;
    if (ctx.body.kind === "json") {
      if (!ctx.body.value || typeof ctx.body.value !== "object" || Array.isArray(ctx.body.value)) throw new Error("JSON object required");
      req = Object.assign({}, ctx.body.value);
    } else {
      if ((ctx.body.files || []).length) {
        throw new Error("Seedance file uploads are not supported; use a public http(s) URL or asset:// ID in the reference fields");
      }
      req = {};
      const fields = ctx.body.fields || {};
      for (const name of Object.keys(fields)) {
        const values = fields[name] || [];
        if (values.length > 1) throw new Error(name + " must be provided once");
        req[name] = values[0];
      }
      for (const name of ["metadata", "images", "reference_images", "reference_videos", "reference_audios"]) {
        if (req[name] === undefined) continue;
        try {
          req[name] = JSON.parse(req[name]);
        } catch {
          throw new Error(name + " must be valid JSON");
        }
      }
      for (const name of ["generate_audio", "watermark", "return_last_frame"]) {
        if (req[name] === undefined) continue;
        if (req[name] !== "true" && req[name] !== "false") throw new Error(name + " must be true or false");
        req[name] = req[name] === "true";
      }
      if (req.seconds !== undefined) req.seconds = Number(req.seconds);
      if (req.duration !== undefined) req.duration = Number(req.duration);
    }
    const model = ctx.upstreamModel || ctx.model;
    const operation = ctx.operation === "edit" ? "edit_video" : ctx.operation === "extend" ? "extend_video" : "";
    if (operation && ctx.body.kind !== "json") throw new Error("Seedance video editing and extension require a JSON body");
    const prepared = prepareSeedanceBody(req, model, undefined, operation);
    return {
      kind: "submit",
      model: ctx.model,
      action: prepared.action,
      requestBody: Object.assign({}, req, { model: ctx.model }),
    };
  },
  render(ctx, task) {
    return legacyRenderers.openai_video(task);
  },
};
