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
import { beforeEach, describe, expect, it, vi } from 'vitest'

import { api } from '@/lib/http-client'

import { authorizeCanvas } from '../api'
import { canvasIntegration, canvasOrigin } from '../config'

vi.mock('@/lib/http-client', () => ({ api: { post: vi.fn() } }))

const config = {
  enabled: true,
  url: 'https://canvas.example.com',
  client_id: 'infinite-canvas',
}
const challenge = 'a'.repeat(43)

function authorizationSearch(): URLSearchParams {
  return new URLSearchParams({
    client_id: config.client_id,
    redirect_uri: `${config.url}/auth/callback`,
    state: 'random-state-from-browser-transaction',
    code_challenge: challenge,
    code_challenge_method: 'S256',
  })
}

beforeEach(() => vi.mocked(api.post).mockReset())

describe('canvas application authorization', () => {
  it.each([
    'javascript:alert(1)',
    'https://canvas.example.com/other',
    'https://alice:secret@canvas.example.com',
    'https://canvas.example.com?apiKey=secret',
    'https://canvas.example.com/#key',
    'http://canvas.example.com',
  ])('rejects untrusted canvas origins: %s', (url) => {
    expect(canvasOrigin(url)).toBeNull()
    expect(
      canvasIntegration({ infinite_canvas: { enabled: true, url } }).enabled
    ).toBe(false)
  })

  it('accepts production HTTPS and explicit loopback development origins', () => {
    expect(canvasOrigin(`${config.url}/`)).toBe(config.url)
    expect(canvasOrigin('http://127.0.0.1:3000')).toBe('http://127.0.0.1:3000')
    expect(canvasIntegration(null).enabled).toBe(false)
    expect(
      canvasIntegration({
        infinite_canvas: { enabled: 'true', url: config.url },
      }).enabled
    ).toBe(false)
  })

  it.each([
    ['client_id', 'another-application'],
    ['redirect_uri', 'https://attacker.example/auth/callback'],
    ['state', ''],
    ['code_challenge', 'short'],
    ['code_challenge_method', 'plain'],
  ])(
    'rejects invalid %s before issuing an authorization',
    async (field, value) => {
      const params = authorizationSearch()
      params.set(field, value)
      await expect(
        authorizeCanvas(config, `?${params.toString()}`)
      ).rejects.toThrow('Invalid canvas authorization request')
      expect(api.post).not.toHaveBeenCalled()
    }
  )

  it('rejects duplicated protocol fields', async () => {
    const params = authorizationSearch()
    params.append('client_id', config.client_id)
    await expect(
      authorizeCanvas(config, `?${params.toString()}`)
    ).rejects.toThrow()
    expect(api.post).not.toHaveBeenCalled()
  })

  it('uses a non-replayed session-authenticated request and excludes supplied identities and credentials', async () => {
    const params = authorizationSearch()
    params.set('user_id', '999')
    params.set('apiKey', 'not-to-forward')
    vi.mocked(api.post).mockResolvedValue({
      data: {
        success: true,
        data: {
          redirect_url: `${config.url}/auth/callback?code=single-use&state=bound`,
        },
      },
    })
    expect(await authorizeCanvas(config, `?${params.toString()}`)).toBe(
      `${config.url}/auth/callback?code=single-use&state=bound`
    )
    expect(api.post).toHaveBeenCalledOnce()
    expect(api.post).toHaveBeenCalledWith(
      '/api/integrations/canvas/authorize',
      {
        client_id: config.client_id,
        redirect_uri: `${config.url}/auth/callback`,
        state: params.get('state'),
        code_challenge: challenge,
        code_challenge_method: 'S256',
      },
      { singleUseAuthorization: true, skipErrorHandler: true }
    )
  })

  it.each([
    'https://attacker.example/auth/callback?code=one',
    `${config.url}/different?code=one`,
    'https://user:secret@canvas.example.com/auth/callback?code=one',
    `${config.url}/auth/callback?code=one#fragment`,
  ])(
    'rejects unexpected authorization destinations: %s',
    async (destination) => {
      vi.mocked(api.post).mockResolvedValue({
        data: { success: true, data: { redirect_url: destination } },
      })
      await expect(
        authorizeCanvas(config, `?${authorizationSearch().toString()}`)
      ).rejects.toThrow('Invalid canvas authorization response')
    }
  )
})
