import type { TFunction } from 'i18next'
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

import type { ImageStorageProfile } from '../types'

export function imageCleanupChannelOptions(profiles: ImageStorageProfile[]) {
  const channels = new Map<string, ImageStorageProfile>()
  // Snapshots are newest first; removed channels still need cleanup access.
  for (const profile of profiles) {
    const channelID = profile.channel_id || profile.profile_id
    const selected = channels.get(channelID)
    if (
      !selected ||
      (selected.current_revision === false &&
        profile.current_revision !== false)
    ) {
      channels.set(channelID, profile)
    }
  }
  return Array.from(channels, ([value, profile]) => ({
    value,
    label: profile.name || value,
  }))
}

export const cleanupPolicySchema = z
  .object({
    id: z.string().min(1),
    name: z.string().trim().min(1).max(200),
    enabled: z.boolean(),
    channel_id: z.string().min(1),
    older_than: z.number().int().min(1).max(876000),
    age_unit: z.enum(['hours', 'days']),
    schedule_type: z.enum(['interval', 'daily']),
    interval_minutes: z.number().int(),
    daily_time: z.string(),
    timezone: z.string().min(1).max(100),
    next_run_at: z.number().int().nonnegative().optional(),
  })
  .superRefine((value, context) => {
    if (
      value.schedule_type === 'interval' &&
      (value.interval_minutes < 5 || value.interval_minutes > 43200)
    ) {
      context.addIssue({
        code: 'custom',
        path: ['interval_minutes'],
        message: 'Check this value.',
      })
    }
    if (
      value.schedule_type === 'daily' &&
      !/^([01]\d|2[0-3]):[0-5]\d$/.test(value.daily_time)
    ) {
      context.addIssue({
        code: 'custom',
        path: ['daily_time'],
        message: 'Check this value.',
      })
    }
    if (value.age_unit === 'days' && value.older_than > 36500) {
      context.addIssue({
        code: 'custom',
        path: ['older_than'],
        message: 'Check this value.',
      })
    }
  })
export const cleanupConfigSchema = z.object({
  revision: z.number().int().nonnegative(),
  enabled: z.boolean(),
  concurrency: z.number().int().min(1).max(8),
  requests_per_second: z.number().int().min(1).max(20),
  batch_size: z.number().int().min(20).max(200),
  scan_pages_per_second: z.number().int().min(1).max(5),
  request_timeout_seconds: z.number().int().min(5).max(60),
  cpu_pause_percent: z.number().int().min(30).max(90),
  memory_pause_percent: z.number().int().min(40).max(90),
  runtime: z
    .object({
      active_workers: z.number().int().nonnegative(),
      pressure_reason: z.string(),
      sampled_at: z.number().int().nonnegative(),
    })
    .optional(),
  policies: z.array(cleanupPolicySchema).max(50),
})
export type CleanupConfig = z.infer<typeof cleanupConfigSchema>
export type CleanupPolicy = z.infer<typeof cleanupPolicySchema>
export const cleanupRequestSchema = z
  .object({
    scope: z.enum(['expired', 'deleted', 'user', 'async_results', 'all']),
    channel_id: z.string().min(1),
    older_than: z.number().int().min(1).max(876000),
    age_unit: z.enum(['hours', 'days']),
    user_id: z.number().int().nonnegative(),
  })
  .superRefine((value, context) => {
    if (value.age_unit === 'days' && value.older_than > 36500) {
      context.addIssue({
        code: 'custom',
        path: ['older_than'],
        message: 'Check this value.',
      })
    }
    if (value.scope === 'user' && value.user_id < 1) {
      context.addIssue({
        code: 'custom',
        path: ['user_id'],
        message: 'Check this value.',
      })
    }
  })
export type CleanupRequestForm = z.infer<typeof cleanupRequestSchema>
export type CleanupRequest = {
  scope: string
  filters: {
    channel_id: string
    older_than: number
    age_unit: 'hours' | 'days'
    user_id?: number
  }
}
export type CleanupJob = {
  job_id: string
  source: string
  status: string
  scope: string
  filters: string
  policy_id?: string
  cutoff_at: number
  scanned: number
  matched_items: number
  matched_bytes: number
  protected_items: number
  reference_skipped?: number
  retention_skipped?: number
  location_skipped?: number
  deleted: number
  skipped: number
  deleted_bytes: number
  errors: number
  processed: number
  total: number
  created_at: number
  next_attempt_at: number
  last_error?: string
  preview_fingerprint?: string
}
export const cleanupJobsPath = '/api/admin/image-library/cleanup-jobs'
export function cleanupIsActive(job: CleanupJob) {
  return ['previewing', 'queued', 'running'].includes(job.status)
}

export function translateImageCleanupMessage(message: string, t: TFunction) {
  const prefix = 'Cleanup configuration is unavailable: '
  if (message.startsWith(prefix)) {
    return t('Cleanup configuration is unavailable: {{reason}}', {
      reason: t(message.slice(prefix.length)),
    })
  }
  return t(message)
}
