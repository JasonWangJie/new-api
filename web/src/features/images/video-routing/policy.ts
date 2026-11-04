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

import type { VideoRoutingRule } from './types'

export const VIDEO_ROUTING_URL = '/api/option/images/video-routing-rules'

export const videoRoutingRuleSchema = z.object({
  id: z.string().min(1),
  name: z.string().trim().min(1).max(128),
  enabled: z.boolean(),
  match: z.object({
    groups: z.array(z.string().min(1)).max(100),
    models: z.array(z.string().min(1)).max(100),
    resolutions: z.array(z.string().min(1)).max(100),
    modes: z.array(z.string().min(1)).max(100),
    video_inputs: z.array(z.enum(['none', 'video'])).max(2),
    seconds: z
      .object({
        min: z.number().int().min(1).max(3600).optional(),
        max: z.number().int().min(1).max(3600).optional(),
        min_exclusive: z.boolean(),
        max_exclusive: z.boolean(),
      })
      .refine((range) => {
        if (
          (range.min === undefined && range.min_exclusive) ||
          (range.max === undefined && range.max_exclusive)
        ) {
          return false
        }
        const first =
          (range.min ?? 1) +
          (range.min !== undefined && range.min_exclusive ? 1 : 0)
        const last =
          (range.max ?? 3600) -
          (range.max !== undefined && range.max_exclusive ? 1 : 0)
        return first <= last
      }),
  }),
  targets: z
    .array(
      z.object({
        channel_id: z.number().int().positive(),
        model: z.string().trim().min(1),
        resolution: z.string(),
      })
    )
    .min(1)
    .max(20)
    .refine(
      (targets) =>
        new Set(targets.map((target) => target.channel_id)).size ===
        targets.length,
      { message: 'Targets in a rule must use different channels.' }
    ),
  on_exhausted: z.enum(['original', 'fail']),
})

export function newVideoRoutingRule(): VideoRoutingRule {
  return {
    id: crypto.randomUUID(),
    name: '',
    enabled: true,
    match: {
      groups: [],
      models: [],
      resolutions: [],
      modes: [],
      video_inputs: [],
      seconds: { min_exclusive: false, max_exclusive: false },
    },
    targets: [{ channel_id: 0, model: '', resolution: '' }],
    on_exhausted: 'original',
  }
}

export function moveVideoRoutingItem<T>(
  items: T[],
  index: number,
  offset: number
): T[] {
  const destination = index + offset
  if (destination < 0 || destination >= items.length) return items
  const moved = [...items]
  const [item] = moved.splice(index, 1)
  moved.splice(destination, 0, item)
  return moved
}
