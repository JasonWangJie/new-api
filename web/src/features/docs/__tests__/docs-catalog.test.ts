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
import { describe, expect, it } from 'vitest'

import {
  findDocsNeighbors,
  getDefaultDocsHref,
  getDocsBundle,
  parseInternalDocsHref,
  resolveDocsPage,
} from '../lib/docs-catalog'

describe('docs catalog', () => {
  it('loads the quickstart bundle with rewritten api.aiimg.lol hosts', () => {
    const bundle = getDocsBundle('quickstart')
    expect(bundle).not.toBeNull()
    expect(bundle?.defaultPath).toBe('tokenrouter')
    expect(getDefaultDocsHref()).toBe('/docs/quickstart/tokenrouter')

    const page = resolveDocsPage('quickstart', 'token-api-config')
    expect(page?.title).toContain('令牌')
    const serialized = JSON.stringify(page)
    expect(serialized).toContain('https://api.aiimg.lol')
    expect(serialized).not.toContain('hejuapi.com')
  })

  it('returns previous and next pages from navigation order', () => {
    const neighbors = findDocsNeighbors('quickstart', 'token-api-config')
    expect(neighbors.previous?.path).toBeTruthy()
    expect(neighbors.next?.path).toBeTruthy()
    expect(neighbors.previous?.path).not.toBe('token-api-config')
    expect(neighbors.next?.path).not.toBe('token-api-config')
  })

  it('parses internal documentation hrefs into route params', () => {
    expect(parseInternalDocsHref('/docs/quickstart/tokenrouter/manual')).toEqual(
      {
        space: 'quickstart',
        _splat: 'tokenrouter/manual',
      }
    )
    expect(parseInternalDocsHref('https://api.aiimg.lol/v1')).toBeNull()
  })
})
