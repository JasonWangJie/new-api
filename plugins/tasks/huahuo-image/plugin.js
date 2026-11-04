export const meta = {
  apiVersion: 1,
  key: "huahuo-image",
  name: "花火生图",
  icon: "text:花火",
  description: { en: "Image generation and editing via Huahuo", zh: "通过花火生成和编辑图片" },
  version: "1.0.0",
  author: { name: "new-api contributors" },
  website: "https://huahuo.ccwu.cc/image",
  baseUrl: "https://huahuo.ccwu.cc",
  models: [],
  fetchMode: "per_task",
  usageSchema: {
    image_count: {
      type: "number",
      unit: "count",
      unitLabel: { en: "image", zh: "张" },
      description: { en: "Image generation unit price", zh: "图片生成单价" },
    },
  },
};

function sizeForRequest(size) {
  if (!size) return undefined;
  const dimensions = /^(\d{1,5})([:x])(\d{1,5})$/i.exec(size);
  if (!dimensions) throw new Error("size must be a ratio or pixel dimensions");
  const width = Number(dimensions[1]),
    height = Number(dimensions[3]);
  if (dimensions[2] === ":") {
    if (width < 1 || height < 1 || width > 32768 || height > 32768 || width / height < 1 / 16 || width / height > 16) {
      throw new Error("size ratio is outside the supported range");
    }
    return width + ":" + height;
  }
  if (width < 64 || height < 64 || width > 32768 || height > 32768) throw new Error("pixel dimensions are outside the supported range");
  return width + "x" + height;
}

export function buildSubmitRequest(ctx) {
  const req = ctx.requestBody;
  const model = ctx.upstreamModel || ctx.model;
  if (!model || typeof req.prompt !== "string" || !req.prompt.trim()) throw new Error("model and prompt are required");
  if (req.n !== 1) throw new Error("Huahuo image tasks require n=1");
  const body = { model, prompt: req.prompt, n: 1, response_format: "url" };
  if (req.resolution) body.resolution = req.resolution;
  if (req.quality) body.quality = req.quality;
  const size = sizeForRequest(req.size);
  const ratio = sizeForRequest(req.aspectRatio);
  if (ratio && !ratio.includes(":")) throw new Error("aspect_ratio must be a ratio");
  if (size && ratio) {
    const [width, height] = size.split(/[:x]/).map(Number);
    const [ratioWidth, ratioHeight] = ratio.split(":").map(Number);
    if (width * ratioHeight !== height * ratioWidth) throw new Error("size conflicts with aspect_ratio");
  }
  if (size || ratio) body.size = size || ratio;
  if (req.images.length) {
    for (const image of req.images) {
      if (!/^https:\/\//i.test(image) && !/^data:image\/(png|jpeg|webp);base64,/i.test(image)) {
        throw new Error("Huahuo requires HTTPS image references or image data URLs");
      }
    }
    body.images = req.images;
  }
  return {
    method: "POST",
    url: ctx.baseUrl + "/v1/images/generations",
    body,
    headers: { Authorization: ctx.authHeader, "Content-Type": "application/json", "Idempotency-Key": ctx.requestHeaders["Idempotency-Key"] },
  };
}

export function buildQueryRequest(ctx) {
  return {
    method: "GET",
    url: ctx.baseUrl + "/v1/images/requests/" + encodeURIComponent(ctx.taskId),
    headers: { Authorization: ctx.authHeader },
  };
}

function parseReceipt(body, httpStatus, query) {
  const state = {};
  if (typeof body?.request_id === "string") state.requestId = body.request_id;
  const code = body?.error?.code;
  if (typeof code === "string") state.code = code;
  const status = body?.status;
  const result = { status: "UNKNOWN", state };
  if (code === "delivery_timeout" || (httpStatus === 200 && status === "failed" && body.delivery_status === "failed")) {
    state.code = "delivery_timeout";
    return result;
  }
  const outputs = body?.data;
  if (
    httpStatus === 200 &&
    (query ? status === "completed" : !status || status === "completed") &&
    !body?.error &&
    Array.isArray(outputs) &&
    outputs.length === 1 &&
    typeof outputs[0]?.url === "string" &&
    /^https:\/\//i.test(outputs[0].url)
  ) {
    return { ...result, status: "SUCCESS", url: outputs[0].url };
  }
  if (outputs != null && (!Array.isArray(outputs) || outputs.length)) return result;
  const terminalFailure = query && httpStatus === 200 && (status === "failed" || status === "not_submitted");
  const rejected =
    !query &&
    !body?.task_id &&
    !status &&
    ((httpStatus === 429 && code === "image_concurrency_limit") ||
      (httpStatus >= 400 && (code === "storage_capacity_exceeded" || code === "storage_check_unavailable")));
  if (terminalFailure || rejected) {
    const creditState = typeof body.credit_state === "string" ? body.credit_state.trim().toLowerCase() : "";
    const creditAmount = typeof body.credit_amount === "number" || typeof body.credit_amount === "string" ? Number(body.credit_amount) : 0;
    // A positive charge indication can prevent a refund, but it never supplies
    // a retail price or turns a failure into a successful image result.
    if (
      body.charged === true ||
      body.billing?.charged === true ||
      body.billing_status === "charged" ||
      body.charge_status === "charged" ||
      body.billing?.status === "charged" ||
      ["charged", "settled", "reserved", "held", "review_required"].includes(creditState) ||
      (!["refunded", "released"].includes(creditState) && Number.isFinite(creditAmount) && creditAmount > 0) ||
      [body.charged_credits, body.credits_charged, body.billing?.charged_credits].some((amount) => typeof amount === "number" && amount > 0)
    ) {
      state.review = true;
      return result;
    }
    return { ...result, status: "FAILURE" };
  }
  if (
    query &&
    ((httpStatus === 200 && ["review_required", "queued", "in_progress", "submitting", "reserved"].includes(status)) ||
      (httpStatus === 409 && ["result_pending", "output_pending", "output_unavailable"].includes(code)))
  ) {
    return { ...result, status: "IN_PROGRESS" };
  }
  return result;
}

export function parseSubmitResponse(ctx, response) {
  const result = parseReceipt(response.body, response.statusCode, false);
  const { state, ...normalized } = result;
  return {
    taskId: ctx.requestHeaders["Idempotency-Key"],
    state,
    immediate: normalized.status === "SUCCESS" || normalized.status === "FAILURE" ? normalized : undefined,
  };
}

export function parseTaskResult(_ctx, body, response) {
  return parseReceipt(body, response.status, true);
}
