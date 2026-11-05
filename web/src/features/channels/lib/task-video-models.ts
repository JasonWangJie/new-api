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
import type { TaskVideoModelConfig, TaskVideoModelDetail } from '../types'

const PRESET_VIDEO_PLUGINS = new Set([
  'wolong',
  'drama',
  'yali',
  'tabtab',
  'nldw',
  'yuxibox',
  'tianyue',
])

export function hasPresetTaskVideoModels(key: string | undefined): boolean {
  return key !== undefined && PRESET_VIDEO_PLUGINS.has(key)
}

export function isChannelConfiguredVideoPlugin(
  key: string | undefined
): boolean {
  return (
    key === 'sudashui' ||
    key === 'huahuo-video' ||
    hasPresetTaskVideoModels(key)
  )
}

export function taskVideoCatalogConfig(
  detail?: TaskVideoModelDetail,
  manualOnly = false
): TaskVideoModelConfig {
  return {
    duration: detail?.duration ?? {},
    resolutions: detail?.resolutions ?? [],
    aspect_ratios: detail?.aspect_ratios ?? [],
    modes: detail?.modes ?? [
      'text_to_video',
      'reference_to_video',
      'image_to_video',
    ],
    references: detail?.references ?? {
      images: null,
      videos: null,
      audios: null,
      ...(manualOnly ? { image_upload: false, image_data_uri: false } : {}),
    },
    billing_mode: detail?.billing_mode ?? '',
    ...(manualOnly ? { durations_by_resolution: {} } : {}),
  }
}

export function importMissingTaskVideoModels(
  current: Record<string, TaskVideoModelConfig>,
  models: string[],
  details?: Record<string, TaskVideoModelDetail>,
  manualOnly = false
): Record<string, TaskVideoModelConfig> {
  const next = { ...current }
  for (const model of models) {
    if (!Object.hasOwn(next, model)) {
      next[model] = taskVideoCatalogConfig(details?.[model], manualOnly)
    }
  }
  return next
}
