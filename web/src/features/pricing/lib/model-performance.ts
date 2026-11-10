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
import type { TFunction } from 'i18next'

import type { PerformanceStats } from '@/features/performance-metrics/types'

import type { PricingModel } from '../types'

const FILTERED_MEDIA_KINDS = ['image', 'video'] as const
export type FilteredMediaKind = (typeof FILTERED_MEDIA_KINDS)[number]

export function getFilteredMediaKinds(
  stats: readonly (PerformanceStats | undefined)[]
) {
  return FILTERED_MEDIA_KINDS.filter((kind) =>
    stats.some((entry) => entry?.[`${kind}_filtered_success_rate`] != null)
  )
}

export function filteredMediaRateLabel(
  t: TFunction,
  kind: FilteredMediaKind,
  distinguishWorkloads: boolean
) {
  if (!distinguishWorkloads) return t('Filtered success rate')
  return kind === 'image'
    ? t('Image filtered success rate')
    : t('Video filtered success rate')
}

export function isMediaModel(model: PricingModel): boolean {
  return Boolean(
    model.mixed_video_pricing != null ||
    model.output_modalities?.some(
      (modality) => modality === 'image' || modality === 'video'
    ) ||
    model.supported_endpoint_types?.some(
      (endpoint) =>
        endpoint === 'image-generation' || endpoint === 'openai-video'
    )
  )
}

export function getModelPerformanceStats<T extends PerformanceStats>(
  model: PricingModel,
  stats: T | undefined
): T | undefined {
  if (
    !stats ||
    (stats.workload != null && stats.workload !== 'relay') ||
    !isMediaModel(model)
  ) {
    return stats
  }
  // Legacy relay buckets do not contain generated image/video quantities.
  // Show successful requests for these models, without inventing output units.
  return { ...stats, workload: 'mixed' }
}
