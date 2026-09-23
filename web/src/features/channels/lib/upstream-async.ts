/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For commercial licensing, please contact support@quantumnous.com
*/
import { z } from 'zod'

import type {
  UpstreamAsyncConfig,
  UpstreamAsyncOperation,
  UpstreamAsyncProfile,
  UpstreamAsyncStatusValue,
} from '../types'

const restrictedPathPattern =
  /^[A-Za-z0-9_-]+(?:\.(?:[A-Za-z0-9_-]+|#|[0-9]+))*$/
const allowedPlaceholders = [
  '{task_id}',
  '{model}',
  '{upstream_model}',
  '{api_key}',
]
const requestFieldPattern =
  /^\{request\.([A-Za-z0-9_-]+(?:\.(?:[A-Za-z0-9_-]+|[0-9]+))*)\}$/
const usageTargets = new Set([
  'prompt_tokens',
  'completion_tokens',
  'total_tokens',
  'prompt_cache_hit_tokens',
  'input_tokens',
  'output_tokens',
  'claude_cache_creation_5_m_tokens',
  'claude_cache_creation_1_h_tokens',
  'prompt_tokens_details.cached_tokens',
  'prompt_tokens_details.cached_tokens_details.text_tokens',
  'prompt_tokens_details.cached_tokens_details.audio_tokens',
  'prompt_tokens_details.cached_tokens_details.image_tokens',
  'prompt_tokens_details.cached_creation_tokens',
  'prompt_tokens_details.cache_write_tokens',
  'prompt_tokens_details.text_tokens',
  'prompt_tokens_details.audio_tokens',
  'prompt_tokens_details.image_tokens',
  'completion_tokens_details.reasoning_tokens',
  'completion_tokens_details.text_tokens',
  'completion_tokens_details.audio_tokens',
  'completion_tokens_details.image_tokens',
  'input_tokens_details.cached_tokens',
  'input_tokens_details.cached_tokens_details.text_tokens',
  'input_tokens_details.cached_tokens_details.audio_tokens',
  'input_tokens_details.cached_tokens_details.image_tokens',
  'input_tokens_details.cached_creation_tokens',
  'input_tokens_details.cache_write_tokens',
  'input_tokens_details.text_tokens',
  'input_tokens_details.audio_tokens',
  'input_tokens_details.image_tokens',
  'output_tokens_details.reasoning_tokens',
  'output_tokens_details.text_tokens',
  'output_tokens_details.audio_tokens',
  'output_tokens_details.image_tokens',
])

const pathSchema = z
  .string()
  .trim()
  .min(1, 'Path is required')
  .max(256, 'Path is too long')
  .regex(restrictedPathPattern, 'Use a restricted GJSON path')
const optionalPathSchema = z
  .string()
  .trim()
  .max(256, 'Path is too long')
  .refine(
    (value) => value === '' || restrictedPathPattern.test(value),
    'Use a restricted GJSON path'
  )
const stringRecordSchema = z.record(z.string(), z.string())
const statusValueSchema = z.union([
  z.string().trim().min(1, 'Status strings must not be empty'),
  z.number().finite(),
  z.boolean(),
])

function statusValueKey(value: UpstreamAsyncStatusValue): string {
  return `${typeof value}:${String(value)}`
}

function templateIsValid(value: string): boolean {
  let remaining = value
  for (const placeholder of allowedPlaceholders) {
    remaining = remaining.replaceAll(placeholder, '')
  }
  return !remaining.includes('{') && !remaining.includes('}')
}

function validateTemplateValue(
  value: unknown,
  context: z.RefinementCtx,
  path: PropertyKey[] = []
): void {
  if (typeof value === 'string') {
    if (!templateIsValid(value)) {
      context.addIssue({
        code: 'custom',
        path,
        message: 'Template contains an unsupported placeholder',
      })
    }
    return
  }
  if (Array.isArray(value)) {
    value.forEach((item, index) =>
      validateTemplateValue(item, context, [...path, index])
    )
    return
  }
  if (value && typeof value === 'object') {
    Object.entries(value).forEach(([key, item]) =>
      validateTemplateValue(item, context, [...path, key])
    )
  }
}

function validateSubmitBodyTemplate(
  value: unknown,
  context: z.RefinementCtx,
  path: PropertyKey[] = []
): void {
  if (typeof value === 'string' && value.includes('{task_id}')) {
    context.addIssue({
      code: 'custom',
      path,
      message: 'Template contains an unsupported placeholder',
    })
  }
  if (typeof value === 'string' && requestFieldPattern.test(value)) return
  if (Array.isArray(value)) {
    value.forEach((item, index) =>
      validateSubmitBodyTemplate(item, context, [...path, index])
    )
    return
  }
  if (value && typeof value === 'object') {
    Object.entries(value).forEach(([key, item]) =>
      validateSubmitBodyTemplate(item, context, [...path, key])
    )
    return
  }
  validateTemplateValue(value, context, path)
}

const profileSchema = z
  .object({
    id: z
      .string()
      .trim()
      .min(1, 'Profile ID is required')
      .max(64, 'Profile ID is too long'),
    media_type: z.enum(['image', 'video']),
    models: z
      .array(
        z
          .string()
          .trim()
          .min(1, 'Model name is required')
          .max(191, 'Model name is too long')
      )
      .max(128, 'A profile can contain at most 128 models'),
    operations: z
      .array(z.enum(['generate', 'edit', 'extend']))
      .min(1, 'Select at least one operation')
      .max(3, 'A profile can contain at most three operations'),
    submit: z
      .object({
        task_id_path: pathSchema,
        request: z
          .object({
            method: z.literal('POST'),
            path: z
              .string()
              .trim()
              .min(1)
              .refine(
                (value) =>
                  value.startsWith('/') &&
                  !value.startsWith('//') &&
                  !value.includes('?') &&
                  !value.includes('#'),
                'Use a relative absolute path without query or fragment'
              ),
            headers: stringRecordSchema.optional(),
            query: stringRecordSchema.optional(),
            body: z.record(z.string(), z.unknown()).optional(),
          })
          .strict()
          .optional(),
      })
      .strict(),
    poll: z
      .object({
        request: z
          .object({
            method: z.enum(['GET', 'POST']),
            path: z
              .string()
              .trim()
              .min(1)
              .refine(
                (value) =>
                  value.startsWith('/') &&
                  !value.startsWith('//') &&
                  !value.includes('?') &&
                  !value.includes('#'),
                'Use a relative absolute path without query or fragment'
              ),
            query: stringRecordSchema.optional(),
            headers: stringRecordSchema.optional(),
            body: z.unknown().optional(),
          })
          .strict(),
        response: z
          .object({
            status_path: pathSchema,
            status_values: z
              .object({
                queued: z.array(statusValueSchema).max(64).optional(),
                in_progress: z.array(statusValueSchema).max(64).optional(),
                succeeded: z
                  .array(statusValueSchema)
                  .min(1, 'At least one succeeded value is required')
                  .max(64),
                failed: z
                  .array(statusValueSchema)
                  .min(1, 'At least one failed value is required')
                  .max(64),
              })
              .strict(),
            progress_path: optionalPathSchema.optional(),
            failure_reason_path: optionalPathSchema.optional(),
            result_path: pathSchema,
            usage_paths: stringRecordSchema.optional(),
            download_headers: stringRecordSchema.optional(),
          })
          .strict(),
      })
      .strict(),
  })
  .strict()
  .superRefine((profile, context) => {
    if (profile.submit.request) {
      const submitRequest = profile.submit.request
      if (submitRequest.path.includes('{task_id}')) {
        context.addIssue({
          code: 'custom',
          path: ['submit', 'request', 'path'],
          message: 'Template contains an unsupported placeholder',
        })
      }
      validateTemplateValue(profile.submit.request.path, context, [
        'submit',
        'request',
        'path',
      ])
      validateTemplateValue(profile.submit.request.headers, context, [
        'submit',
        'request',
        'headers',
      ])
      validateTemplateValue(profile.submit.request.query, context, [
        'submit',
        'request',
        'query',
      ])
      for (const [field, entries] of [
        ['query', submitRequest.query],
        ['headers', submitRequest.headers],
      ] as const) {
        for (const [name, value] of Object.entries(entries || {})) {
          if (value.includes('{task_id}')) {
            context.addIssue({
              code: 'custom',
              path: ['submit', 'request', field, name],
              message: 'Template contains an unsupported placeholder',
            })
          }
        }
      }
      validateSubmitBodyTemplate(profile.submit.request.body, context, [
        'submit',
        'request',
        'body',
      ])
      if (
        JSON.stringify(profile.submit.request.body || {}).length >
        64 * 1024
      ) {
        context.addIssue({
          code: 'custom',
          path: ['submit', 'request', 'body'],
          message: 'Submit body template is too large',
        })
      }
    }
    if (
      profile.media_type === 'image' &&
      profile.operations.includes('extend')
    ) {
      context.addIssue({
        code: 'custom',
        path: ['operations'],
        message: 'Image profiles cannot use extend',
      })
    }
    if (
      profile.poll.request.method === 'GET' &&
      profile.poll.request.body !== undefined
    ) {
      context.addIssue({
        code: 'custom',
        path: ['poll', 'request', 'body'],
        message: 'GET polling cannot include a JSON body',
      })
    }
    const seen = new Map<string, string>()
    for (const [name, values] of Object.entries(
      profile.poll.response.status_values
    )) {
      for (const value of values || []) {
        const key = statusValueKey(value)
        const previous = seen.get(key)
        if (previous) {
          context.addIssue({
            code: 'custom',
            path: ['poll', 'response', 'status_values', name],
            message: 'Status values must not overlap',
          })
        }
        seen.set(key, name)
      }
    }
    for (const target of Object.keys(profile.poll.response.usage_paths || {})) {
      if (!usageTargets.has(target)) {
        context.addIssue({
          code: 'custom',
          path: ['poll', 'response', 'usage_paths', target],
          message: 'Unsupported usage target',
        })
      }
    }
    validateTemplateValue(profile.poll.request.path, context, [
      'poll',
      'request',
      'path',
    ])
    validateTemplateValue(profile.poll.request.query, context, [
      'poll',
      'request',
      'query',
    ])
    validateTemplateValue(profile.poll.request.headers, context, [
      'poll',
      'request',
      'headers',
    ])
    validateTemplateValue(profile.poll.request.body, context, [
      'poll',
      'request',
      'body',
    ])
    validateTemplateValue(profile.poll.response.download_headers, context, [
      'poll',
      'response',
      'download_headers',
    ])
  })

export const upstreamAsyncConfigSchema = z
  .object({
    profiles: z
      .array(profileSchema)
      .min(1, 'At least one profile is required')
      .max(32, 'At most 32 profiles are allowed'),
  })
  .strict()
  .superRefine((config, context) => {
    const ids = new Set<string>()
    config.profiles.forEach((profile, index) => {
      if (ids.has(profile.id)) {
        context.addIssue({
          code: 'custom',
          path: ['profiles', index, 'id'],
          message: 'Profile ID must be unique',
        })
      }
      ids.add(profile.id)
      for (let previousIndex = 0; previousIndex < index; previousIndex += 1) {
        const previous = config.profiles[previousIndex]
        const operationOverlap = profile.operations.some((operation) =>
          previous.operations.includes(operation)
        )
        const modelOverlap =
          profile.models.length === 0 ||
          previous.models.length === 0 ||
          profile.models.some((model) => previous.models.includes(model))
        if (
          profile.media_type === previous.media_type &&
          operationOverlap &&
          modelOverlap
        ) {
          context.addIssue({
            code: 'custom',
            path: ['profiles', index, 'models'],
            message: 'Profile selectors overlap an earlier profile',
          })
        }
      }
    })
  })

function jsonTextSchema(
  fallback: unknown,
  expected: 'object' | 'array' | 'value'
) {
  return z.string().superRefine((text, context) => {
    const source = text.trim()
    if (source === '' && (fallback !== undefined || expected === 'value')) {
      return
    }
    try {
      const parsed: unknown = JSON.parse(source)
      if (
        expected === 'object' &&
        (!parsed || typeof parsed !== 'object' || Array.isArray(parsed))
      ) {
        context.addIssue({ code: 'custom', message: 'Enter a JSON object' })
      }
      if (expected === 'array' && !Array.isArray(parsed)) {
        context.addIssue({ code: 'custom', message: 'Enter a JSON array' })
      }
    } catch {
      context.addIssue({ code: 'custom', message: 'Enter valid JSON' })
    }
  })
}

export const upstreamAsyncProfileFormSchema = z.object({
  id: z
    .string()
    .trim()
    .min(1, 'Profile ID is required')
    .max(64, 'Profile ID is too long'),
  media_type: z.enum(['image', 'video']),
  models: z.string(),
  operations: z
    .array(z.enum(['generate', 'edit', 'extend']))
    .min(1, 'Select at least one operation'),
  task_id_path: z.string().trim().min(1, 'Task ID path is required'),
  submit_mode: z.enum(['default', 'custom']),
  submit_path: z.string(),
  submit_query_json: z.string(),
  submit_headers_json: z.string(),
  submit_body_json: z.string(),
  method: z.enum(['GET', 'POST']),
  poll_path: z.string().trim().min(1, 'Polling path is required'),
  query_json: jsonTextSchema({}, 'object'),
  headers_json: jsonTextSchema({}, 'object'),
  body_json: jsonTextSchema(undefined, 'value'),
  status_path: z.string().trim().min(1, 'Status path is required'),
  queued_json: jsonTextSchema([], 'array'),
  in_progress_json: jsonTextSchema([], 'array'),
  succeeded_json: jsonTextSchema(undefined, 'array'),
  failed_json: jsonTextSchema(undefined, 'array'),
  progress_path: z.string(),
  failure_reason_path: z.string(),
  result_path: z.string().trim().min(1, 'Result URL path is required'),
  usage_paths_json: jsonTextSchema({}, 'object'),
  download_headers_json: jsonTextSchema({}, 'object'),
})

export const upstreamAsyncEditorFormSchema = z
  .object({
    profiles: z
      .array(upstreamAsyncProfileFormSchema)
      .min(1, 'At least one profile is required')
      .max(32, 'At most 32 profiles are allowed'),
  })
  .superRefine((form, context) => {
    form.profiles.forEach((profile, index) => {
      if (profile.submit_mode !== 'custom') return
      if (!profile.submit_path.trim()) {
        context.addIssue({
          code: 'custom',
          path: ['profiles', index, 'submit_path'],
          message: 'Submit path is required',
        })
      }
      for (const field of [
        'submit_query_json',
        'submit_headers_json',
        'submit_body_json',
      ] as const) {
        const result = jsonTextSchema({}, 'object').safeParse(profile[field])
        if (!result.success) {
          context.addIssue({
            code: 'custom',
            path: ['profiles', index, field],
            message: result.error.issues[0]?.message || 'Enter valid JSON',
          })
        }
      }
    })
  })

export type UpstreamAsyncProfileForm = z.infer<
  typeof upstreamAsyncProfileFormSchema
>
export type UpstreamAsyncEditorForm = z.infer<
  typeof upstreamAsyncEditorFormSchema
>

export function createUpstreamAsyncProfileForm(): UpstreamAsyncProfileForm {
  return {
    id: 'vendor-video',
    media_type: 'video',
    models: '',
    operations: ['generate'],
    task_id_path: 'data.task_id',
    submit_mode: 'default',
    submit_path: '/v1/videos',
    submit_query_json: '{}',
    submit_headers_json: '{\n  "Authorization": "Bearer {api_key}"\n}',
    submit_body_json: '',
    method: 'GET',
    poll_path: '/v1/tasks/{task_id}',
    query_json: '{}',
    headers_json: '{\n  "Authorization": "Bearer {api_key}"\n}',
    body_json: '',
    status_path: 'data.status',
    queued_json: '["pending", "queued"]',
    in_progress_json: '["processing", "running"]',
    succeeded_json: '["succeeded"]',
    failed_json: '["failed", "cancelled", "expired"]',
    progress_path: 'data.progress',
    failure_reason_path: 'data.error.message',
    result_path: 'data.output.url',
    usage_paths_json: '{}',
    download_headers_json: '{}',
  }
}

function parseJSONText(text: string, fallback: unknown): unknown {
  return text.trim() === '' ? fallback : JSON.parse(text)
}

export function upstreamAsyncConfigToForm(
  config: UpstreamAsyncConfig | null
): UpstreamAsyncEditorForm {
  if (!config || config.profiles.length === 0) {
    return { profiles: [createUpstreamAsyncProfileForm()] }
  }
  return {
    profiles: config.profiles.map((profile) => ({
      id: profile.id,
      media_type: profile.media_type,
      models: profile.models.join(', '),
      operations: [...profile.operations],
      task_id_path: profile.submit.task_id_path,
      submit_mode: profile.submit.request ? 'custom' : 'default',
      submit_path: profile.submit.request?.path || '/v1/videos',
      submit_query_json: JSON.stringify(
        profile.submit.request?.query || {},
        null,
        2
      ),
      submit_headers_json: JSON.stringify(
        profile.submit.request?.headers || {
          Authorization: 'Bearer {api_key}',
        },
        null,
        2
      ),
      submit_body_json:
        profile.submit.request?.body === undefined
          ? ''
          : JSON.stringify(profile.submit.request.body, null, 2),
      method: profile.poll.request.method,
      poll_path: profile.poll.request.path,
      query_json: JSON.stringify(profile.poll.request.query || {}, null, 2),
      headers_json: JSON.stringify(profile.poll.request.headers || {}, null, 2),
      body_json:
        profile.poll.request.body === undefined
          ? ''
          : JSON.stringify(profile.poll.request.body, null, 2),
      status_path: profile.poll.response.status_path,
      queued_json: JSON.stringify(
        profile.poll.response.status_values.queued || []
      ),
      in_progress_json: JSON.stringify(
        profile.poll.response.status_values.in_progress || []
      ),
      succeeded_json: JSON.stringify(
        profile.poll.response.status_values.succeeded
      ),
      failed_json: JSON.stringify(profile.poll.response.status_values.failed),
      progress_path: profile.poll.response.progress_path || '',
      failure_reason_path: profile.poll.response.failure_reason_path || '',
      result_path: profile.poll.response.result_path,
      usage_paths_json: JSON.stringify(
        profile.poll.response.usage_paths || {},
        null,
        2
      ),
      download_headers_json: JSON.stringify(
        profile.poll.response.download_headers || {},
        null,
        2
      ),
    })),
  }
}

export function upstreamAsyncFormToConfig(
  form: UpstreamAsyncEditorForm
): UpstreamAsyncConfig {
  const profiles: UpstreamAsyncProfile[] = form.profiles.map((profile) => {
    const requestBody = parseJSONText(profile.body_json, undefined)
    const submitQuery = (
      profile.submit_mode === 'custom'
        ? parseJSONText(profile.submit_query_json, {})
        : {}
    ) as Record<string, string>
    const query = parseJSONText(profile.query_json, {}) as Record<
      string,
      string
    >
    const headers = parseJSONText(profile.headers_json, {}) as Record<
      string,
      string
    >
    const request: UpstreamAsyncProfile['poll']['request'] = {
      method: profile.method,
      path: profile.poll_path.trim(),
    }
    if (Object.keys(query).length > 0) request.query = query
    if (Object.keys(headers).length > 0) request.headers = headers
    if (requestBody !== undefined) request.body = requestBody
    const queued = parseJSONText(
      profile.queued_json,
      []
    ) as UpstreamAsyncStatusValue[]
    const inProgress = parseJSONText(
      profile.in_progress_json,
      []
    ) as UpstreamAsyncStatusValue[]
    const usagePaths = parseJSONText(profile.usage_paths_json, {}) as Record<
      string,
      string
    >
    const downloadHeaders = parseJSONText(
      profile.download_headers_json,
      {}
    ) as Record<string, string>
    const statusValues: UpstreamAsyncProfile['poll']['response']['status_values'] =
      {
        succeeded: parseJSONText(
          profile.succeeded_json,
          []
        ) as UpstreamAsyncStatusValue[],
        failed: parseJSONText(
          profile.failed_json,
          []
        ) as UpstreamAsyncStatusValue[],
      }
    if (queued.length > 0) statusValues.queued = queued
    if (inProgress.length > 0) statusValues.in_progress = inProgress
    const response: UpstreamAsyncProfile['poll']['response'] = {
      status_path: profile.status_path.trim(),
      status_values: statusValues,
      result_path: profile.result_path.trim(),
    }
    if (profile.progress_path.trim()) {
      response.progress_path = profile.progress_path.trim()
    }
    if (profile.failure_reason_path.trim()) {
      response.failure_reason_path = profile.failure_reason_path.trim()
    }
    if (Object.keys(usagePaths).length > 0) response.usage_paths = usagePaths
    if (Object.keys(downloadHeaders).length > 0) {
      response.download_headers = downloadHeaders
    }
    return {
      id: profile.id.trim(),
      media_type: profile.media_type,
      models: profile.models
        .split(',')
        .map((model) => model.trim())
        .filter(Boolean),
      operations: [...profile.operations] as UpstreamAsyncOperation[],
      submit: {
        task_id_path: profile.task_id_path.trim(),
        ...(profile.submit_mode === 'custom'
          ? {
              request: {
                method: 'POST' as const,
                path: profile.submit_path.trim(),
                ...(Object.keys(submitQuery).length > 0
                  ? { query: submitQuery }
                  : {}),
                headers: parseJSONText(
                  profile.submit_headers_json,
                  {}
                ) as Record<string, string>,
                ...(profile.submit_body_json.trim()
                  ? {
                      body: parseJSONText(
                        profile.submit_body_json,
                        {}
                      ) as Record<string, unknown>,
                    }
                  : {}),
              },
            }
          : {}),
      },
      poll: {
        request,
        response,
      },
    }
  })
  return upstreamAsyncConfigSchema.parse({ profiles }) as UpstreamAsyncConfig
}

export function parseUpstreamAsyncConfig(
  value: unknown
): UpstreamAsyncConfig | null {
  if (!value) return null
  const parsed = upstreamAsyncConfigSchema.safeParse(value)
  return parsed.success ? (parsed.data as UpstreamAsyncConfig) : null
}

export function upstreamAsyncFromSettingsJSON(
  settingsJSON: string | undefined
): UpstreamAsyncConfig | null {
  try {
    const settings: unknown = JSON.parse(settingsJSON || '{}')
    if (!settings || typeof settings !== 'object' || Array.isArray(settings)) {
      return null
    }
    return parseUpstreamAsyncConfig(
      (settings as Record<string, unknown>).upstream_async
    )
  } catch {
    return null
  }
}

export const UPSTREAM_ASYNC_VIDEO_EXAMPLE: UpstreamAsyncConfig = {
  profiles: [
    {
      id: 'vendor-video',
      media_type: 'video',
      models: ['vendor-video-model'],
      operations: ['generate', 'edit', 'extend'],
      submit: { task_id_path: 'data.task_id' },
      poll: {
        request: {
          method: 'GET',
          path: '/v1/tasks/{task_id}',
          headers: { Authorization: 'Bearer {api_key}' },
        },
        response: {
          status_path: 'data.status',
          status_values: {
            queued: ['pending', 'queued'],
            in_progress: ['processing', 'running'],
            succeeded: ['succeeded'],
            failed: ['failed', 'cancelled', 'expired'],
          },
          progress_path: 'data.progress',
          failure_reason_path: 'data.error.message',
          result_path: 'data.output.url',
          usage_paths: { total_tokens: 'usage.total_tokens' },
        },
      },
    },
  ],
}

export const UPSTREAM_ASYNC_IMAGE_EXAMPLE: UpstreamAsyncConfig = {
  profiles: [
    {
      id: 'vendor-image',
      media_type: 'image',
      models: ['vendor-image-model'],
      operations: ['generate', 'edit'],
      submit: { task_id_path: 'request.id' },
      poll: {
        request: {
          method: 'POST',
          path: '/jobs/status',
          headers: { Authorization: 'Bearer {api_key}' },
          body: { id: '{task_id}' },
        },
        response: {
          status_path: 'status',
          status_values: {
            queued: [0],
            in_progress: [1],
            succeeded: [2],
            failed: [-1],
          },
          failure_reason_path: 'error.message',
          result_path: 'images.#.url',
          usage_paths: {
            prompt_tokens: 'usage.prompt_tokens',
            completion_tokens: 'usage.completion_tokens',
            total_tokens: 'usage.total_tokens',
          },
        },
      },
    },
  ],
}

export const UPSTREAM_ASYNC_MAI_IMAGE_EXAMPLE: UpstreamAsyncConfig = {
  profiles: [
    {
      id: 'mai-token-image',
      media_type: 'image',
      models: ['seedream-5-pro'],
      operations: ['generate'],
      submit: {
        task_id_path: 'id',
        request: {
          method: 'POST',
          path: '/v1/videos',
          headers: { Authorization: 'Bearer {api_key}' },
          body: {
            model: '{upstream_model}',
            prompt: '{request.prompt}',
            aspect_ratio: '{request.aspect_ratio}',
            quality: '1440p',
            image_urls: '{request.image_urls}',
            create_count: '{request.create_count}',
          },
        },
      },
      poll: {
        request: {
          method: 'GET',
          path: '/v1/videos/{task_id}',
          headers: { Authorization: 'Bearer {api_key}' },
        },
        response: {
          status_path: 'status',
          status_values: {
            queued: ['queued'],
            in_progress: ['in_progress'],
            succeeded: ['completed'],
            failed: ['failed', 'cancelled'],
          },
          progress_path: 'progress',
          failure_reason_path: 'error.message',
          result_path: 'video_url',
        },
      },
    },
  ],
}
