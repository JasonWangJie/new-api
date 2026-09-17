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
export const IMAGE_SUCCESS_BADGE_CLASS =
  'border-emerald-500/30 bg-emerald-500/10 text-emerald-700 dark:text-emerald-400'

export const IMAGE_QUEUED_BADGE_CLASS =
  'border-amber-500/30 bg-amber-500/10 text-amber-800 dark:text-amber-300'

export const IMAGE_PROCESSING_BADGE_CLASS =
  'border-blue-500/30 bg-blue-500/10 text-blue-800 dark:text-blue-300'

export const IMAGE_FAILED_BADGE_CLASS =
  'border-rose-500/30 bg-rose-500/10 text-rose-700 dark:text-rose-400'

const PROCESSING_STATUSES = new Set([
  'invoking',
  'processing',
  'upstream_succeeded',
  'uploading',
  'billing_pending',
  'storage_failed',
  'billing_failed',
])

const FAILED_STATUSES = new Set([
  'failed',
  'expired',
  'execution_unknown',
])

export function imageStatusBadgeClass(
  status: string,
  errorCode?: string | number | boolean | null
): string | undefined {
  if (status === 'succeeded') return IMAGE_SUCCESS_BADGE_CLASS
  if (status === 'queued') return IMAGE_QUEUED_BADGE_CLASS
  if (FAILED_STATUSES.has(status) || errorCode) return IMAGE_FAILED_BADGE_CLASS
  if (PROCESSING_STATUSES.has(status)) return IMAGE_PROCESSING_BADGE_CLASS
  return undefined
}

export function imageStatusBadgeVariant(
  status: string,
  errorCode?: string | number | boolean | null
): 'warning' | 'outline' {
  if (FAILED_STATUSES.has(status) || errorCode) return 'warning'
  return 'outline'
}

export function imagePlatformVisual(platform: string): {
  iconKey: string
  className: string
} {
  if (platform === 'gemini') {
    return {
      iconKey: 'Gemini.Color',
      className:
        'border-sky-500/30 bg-sky-500/10 text-sky-800 dark:border-sky-400/30 dark:bg-sky-500/15 dark:text-sky-300',
    }
  }
  return {
    iconKey: 'OpenAI.Color',
    className:
      'border-emerald-500/30 bg-emerald-500/10 text-emerald-800 dark:border-emerald-400/30 dark:bg-emerald-500/15 dark:text-emerald-300',
  }
}

export function imageRequestTypeClass(requestType: string): string {
  if (requestType === 'image_to_image') {
    return 'border-violet-500/25 bg-violet-500/10 text-violet-800 dark:text-violet-300'
  }
  return 'border-cyan-500/25 bg-cyan-500/10 text-cyan-800 dark:text-cyan-300'
}

export function imageDetailHeaderClass(status: string): string {
  if (status === 'succeeded') {
    return 'border-emerald-500/20 from-emerald-500/10 via-background to-background'
  }
  if (FAILED_STATUSES.has(status)) {
    return 'border-rose-500/20 from-rose-500/10 via-background to-background'
  }
  if (status === 'queued') {
    return 'border-amber-500/20 from-amber-500/10 via-background to-background'
  }
  return 'border-blue-500/20 from-blue-500/10 via-background to-background'
}
