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

const words = z
  .array(z.string())
  .transform((values) => [
    ...new Set(values.map((value) => value.trim()).filter(Boolean)),
  ])
const ids = z.array(z.number().int().positive())
const action = z.enum([
  'same_channel',
  'other_channel',
  'local_same_channel',
  'local_other_channel',
  'stop',
  'manual_review',
  'poll_same_job',
  'resume_download',
  'resume_billing',
])
export const retryRuleSchema = z.object({
  id: z.string().min(1),
  name: z.string().trim().min(1).max(120),
  enabled: z.boolean(),
  category: z.string().trim().min(1).max(64),
  scope: z.object({
    media_types: words.optional(),
    providers: words.optional(),
    models: words.optional(),
    channel_ids: ids.optional(),
    stages: words.optional(),
    categories: words.optional(),
  }),
  match: z.object({
    http_statuses: z.array(z.number().int().min(100).max(599)).optional(),
    error_codes: words.optional(),
    contains_any: words.optional(),
    contains_all: words.optional(),
    excludes: words.optional(),
    regex: z.string().max(1024).optional(),
  }),
  steps: z
    .array(
      z.object({
        id: z.string().min(1),
        action,
        max_attempts: z.number().int().min(1).max(100),
        delay_seconds: z.number().int().min(0).max(86400),
        backoff: z.enum(['fixed', 'exponential']).optional(),
        max_delay_seconds: z.number().int().min(0).max(86400).optional(),
      })
    )
    .min(1)
    .max(8),
  max_attempts: z.number().int().min(0).max(100).optional(),
  on_exhausted: action,
  allowed_channel_ids: ids.optional(),
  excluded_channel_ids: ids.optional(),
})
