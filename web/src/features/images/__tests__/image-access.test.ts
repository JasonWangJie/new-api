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
import { afterEach, expect, test, vi } from 'vitest'

import { api } from '@/lib/api'

import { authenticatedMediaBlob, imageBlob } from '../api'

vi.mock('@/lib/api', () => ({ api: { request: vi.fn() } }))

afterEach(() => {
  vi.resetAllMocks()
  vi.unstubAllGlobals()
})

test('authenticated images use the session client for bytes without an anonymous fetch', async () => {
  const blob = new Blob(['image'], { type: 'image/png' })
  vi.mocked(api.request).mockResolvedValue({ data: blob })
  const anonymousFetch = vi.fn()
  vi.stubGlobal('fetch', anonymousFetch)
  const path = '/api/user/async-image-tasks/asyncimg_private/results/0/view'
  await expect(imageBlob(path, undefined, true)).resolves.toBe(blob)
  expect(api.request).toHaveBeenCalledWith(
    expect.objectContaining({
      url: path,
      responseType: 'blob',
      headers: { Accept: 'image/*' },
    })
  )
  expect(anonymousFetch).not.toHaveBeenCalled()
})

test.each(['https://attacker.example/image.png', '/api/user/self'])(
  'authenticated image loading rejects an untrusted content endpoint: %s',
  async (url) => {
    await expect(imageBlob(url, undefined, true)).rejects.toThrow()
    expect(api.request).not.toHaveBeenCalled()
  }
)

test('a protected link response resolves to session-authenticated bytes', async () => {
  const blob = new Blob(['image'], { type: 'image/png' })
  const path = '/api/user/async-image-tasks/private/results/0/view'
  vi.mocked(api.request)
    .mockResolvedValueOnce({
      data: { success: true, data: { url: path, authenticated: true } },
    })
    .mockResolvedValueOnce({ data: blob })
  const anonymousFetch = vi.fn()
  vi.stubGlobal('fetch', anonymousFetch)
  await expect(imageBlob(path)).resolves.toBe(blob)
  expect(anonymousFetch).not.toHaveBeenCalled()
  expect(api.request).toHaveBeenLastCalledWith(
    expect.objectContaining({ url: path, responseType: 'blob' })
  )
})

test('ordinary signed images keep anonymous fetch behavior', async () => {
  const blob = new Blob(['image'], { type: 'image/png' })
  const anonymousFetch = vi.fn().mockResolvedValue({
    ok: true,
    blob: async () => blob,
  })
  vi.stubGlobal('fetch', anonymousFetch)
  const url = 'https://storage.example/image.png?signature=ordinary'
  await expect(imageBlob(url)).resolves.toBe(blob)
  expect(api.request).not.toHaveBeenCalled()
  expect(anonymousFetch).toHaveBeenCalledWith(
    url,
    expect.objectContaining({ credentials: 'omit', cache: 'no-store' })
  )
})

test('protected video bytes use the session client and never resolve to an upstream link', async () => {
  const blob = new Blob(['video'], { type: 'video/mp4' })
  vi.mocked(api.request).mockResolvedValue({ data: blob })
  const path = '/api/admin/media-tasks/private-task/results/video/view'
  await expect(authenticatedMediaBlob(path)).resolves.toBe(blob)
  expect(api.request).toHaveBeenCalledWith(
    expect.objectContaining({ url: path, responseType: 'blob' })
  )
})

test('protected video preview rejects a non-video response', async () => {
  vi.mocked(api.request).mockResolvedValue({
    data: new Blob(['error page'], { type: 'text/html' }),
  })
  await expect(
    authenticatedMediaBlob('/api/user/media-tasks/private/results/video/view')
  ).rejects.toThrow()
})
