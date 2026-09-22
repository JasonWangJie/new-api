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
import { describe, expect, test } from 'vitest'

import {
  upstreamAsyncConfigSchema,
  upstreamAsyncConfigToForm,
  upstreamAsyncEditorFormSchema,
  upstreamAsyncFormToConfig,
  upstreamAsyncFromSettingsJSON,
  UPSTREAM_ASYNC_IMAGE_EXAMPLE,
  UPSTREAM_ASYNC_VIDEO_EXAMPLE,
} from '../upstream-async'

describe('upstream async channel configuration', () => {
  test('round-trips typed status values and request templates', () => {
    const form = upstreamAsyncConfigToForm(UPSTREAM_ASYNC_IMAGE_EXAMPLE)
    expect(upstreamAsyncEditorFormSchema.safeParse(form).success).toBe(true)
    const restored = upstreamAsyncFormToConfig(form)

    expect(restored).toEqual(UPSTREAM_ASYNC_IMAGE_EXAMPLE)
    expect(restored.profiles[0].poll.response.status_values.succeeded).toEqual([
      2,
    ])
    expect(restored.profiles[0].poll.request.body).toEqual({
      id: '{task_id}',
    })
  })

  test('rejects overlapping selectors and status sets without coercing JSON types', () => {
    const overlapping = structuredClone(UPSTREAM_ASYNC_VIDEO_EXAMPLE)
    overlapping.profiles.push({
      ...structuredClone(overlapping.profiles[0]),
      id: 'overlap',
      models: [],
      poll: {
        ...structuredClone(overlapping.profiles[0].poll),
        response: {
          ...structuredClone(overlapping.profiles[0].poll.response),
          status_values: {
            succeeded: [1],
            failed: [1],
          },
        },
      },
    })

    const result = upstreamAsyncConfigSchema.safeParse(overlapping)
    expect(result.success).toBe(false)
    if (!result.success) {
      expect(result.error.issues.map((issue) => issue.message)).toEqual(
        expect.arrayContaining([
          'Status values must not overlap',
          'Profile selectors overlap an earlier profile',
        ])
      )
    }
  })

  test('reads only a valid upstream_async property from channel settings', () => {
    const settings = JSON.stringify({
      allow_speed: true,
      upstream_async: UPSTREAM_ASYNC_VIDEO_EXAMPLE,
    })
    expect(upstreamAsyncFromSettingsJSON(settings)).toEqual(
      UPSTREAM_ASYNC_VIDEO_EXAMPLE
    )
    expect(
      upstreamAsyncFromSettingsJSON(
        '{"upstream_async":{"profiles":[],"unknown":true}}'
      )
    ).toBeNull()
  })
})
