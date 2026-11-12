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

export type ImageHostingConfiguration = {
  revision: number
  enabled: boolean
  image_enabled: boolean
  base_url: string
  file_upload_path: string
  url_upload_path: string
  file_upload_method: 'POST' | 'PUT'
  url_upload_method: 'POST' | 'PUT'
  file_field: string
  url_field: string
  response_url_path: string
  token_header: string
  token_prefix: string
  extra_fields: Record<string, string | number | boolean>
  timeout_seconds: number
  retries: number
  concurrency: number
  failure_threshold: number
  cooldown_seconds: number
  url_fallback: 'local' | 'upstream'
  allow_private_network: boolean
  has_token: boolean
  runtime?: {
    state: 'closed' | 'open' | 'half_open'
    failures: number
    in_flight: number
    retry_after_seconds: number
  }
}

export const imageHostingFormSchema = z
  .object({
    enabled: z.boolean(),
    image_enabled: z.boolean(),
    base_url: z
      .string()
      .trim()
      .max(2048)
      .refine((value) => {
        if (!value) return true
        try {
          const url = new URL(value)
          return (
            url.protocol === 'https:' &&
            !url.username &&
            !url.password &&
            !url.search &&
            !url.hash
          )
        } catch {
          return false
        }
      }, 'Check this value.'),
    file_upload_path: z
      .string()
      .trim()
      .min(1)
      .max(512)
      .refine(
        (value) => value.startsWith('/') && !value.startsWith('//'),
        'Check this value.'
      ),
    url_upload_path: z
      .string()
      .trim()
      .min(1)
      .max(512)
      .refine(
        (value) => value.startsWith('/') && !value.startsWith('//'),
        'Check this value.'
      ),
    file_upload_method: z.enum(['POST', 'PUT']),
    url_upload_method: z.enum(['POST', 'PUT']),
    file_field: z.string().trim().min(1).max(64),
    url_field: z.string().trim().min(1).max(64),
    response_url_path: z.string().trim().min(1).max(128),
    token_header: z.string().trim().min(1).max(64),
    token_prefix: z.string().max(64),
    token: z.string().max(4096),
    clear_token: z.boolean(),
    extra_fields_json: z.string().superRefine((value, context) => {
      try {
        const fields: unknown = JSON.parse(value)
        if (
          fields === null ||
          Array.isArray(fields) ||
          typeof fields !== 'object' ||
          Object.values(fields).some(
            (field) =>
              typeof field !== 'string' &&
              typeof field !== 'boolean' &&
              !(typeof field === 'number' && Number.isFinite(field))
          )
        ) {
          throw new Error('invalid fields')
        }
      } catch {
        context.addIssue({
          code: 'custom',
          message:
            'Extra upload fields must be a JSON object with string, number or boolean values.',
        })
      }
    }),
    timeout_seconds: z.number().int().min(1).max(120),
    retries: z.number().int().min(0).max(3),
    concurrency: z.number().int().min(1).max(32),
    failure_threshold: z.number().int().min(1).max(100),
    cooldown_seconds: z.number().int().min(1).max(3600),
    url_fallback: z.enum(['local', 'upstream']),
    allow_private_network: z.boolean(),
  })
  .superRefine((values, context) => {
    if (values.enabled && !values.base_url) {
      context.addIssue({
        code: 'custom',
        path: ['base_url'],
        message: 'Check this value.',
      })
    }
  })

export type ImageHostingFormValues = z.infer<typeof imageHostingFormSchema>

export function imageHostingFormValues(
  config: ImageHostingConfiguration
): ImageHostingFormValues {
  return {
    enabled: config.enabled,
    image_enabled: config.image_enabled,
    base_url: config.base_url,
    file_upload_path: config.file_upload_path,
    url_upload_path: config.url_upload_path,
    file_upload_method: config.file_upload_method,
    url_upload_method: config.url_upload_method,
    file_field: config.file_field,
    url_field: config.url_field,
    response_url_path: config.response_url_path,
    token_header: config.token_header,
    token_prefix: config.token_prefix,
    token: '',
    clear_token: false,
    extra_fields_json: JSON.stringify(config.extra_fields, null, 2),
    timeout_seconds: config.timeout_seconds,
    retries: config.retries,
    concurrency: config.concurrency,
    failure_threshold: config.failure_threshold,
    cooldown_seconds: config.cooldown_seconds,
    url_fallback: config.url_fallback,
    allow_private_network: config.allow_private_network,
  }
}
