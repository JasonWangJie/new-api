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
  IMAGE_SUCCESS_BADGE_CLASS,
  imagePlatformVisual,
  imageStatusBadgeClass,
} from '../image-visuals'

describe('image visuals', () => {
  test('success status reuses the emerald list badge treatment', () => {
    expect(imageStatusBadgeClass('succeeded')).toBe(IMAGE_SUCCESS_BADGE_CLASS)
  })

  test('openai and gemini platforms use distinct icon and color shells', () => {
    expect(imagePlatformVisual('openai').iconKey).toBe('OpenAI.Color')
    expect(imagePlatformVisual('gemini').iconKey).toBe('Gemini.Color')
    expect(imagePlatformVisual('openai').className).not.toBe(
      imagePlatformVisual('gemini').className
    )
  })
})
