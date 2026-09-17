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
import { Blob as NativeBlob } from 'node:buffer'

import { IDBFactory, IDBKeyRange } from 'fake-indexeddb'
import { beforeEach, describe, expect, it, vi } from 'vitest'

import type { LocalImage } from '../../types'

const fixture = (userId: number, id: string): LocalImage => ({
  id,
  userId,
  blob: new NativeBlob(['original-image'], { type: 'image/png' }) as Blob,
  createdAt: Date.now(),
  expiresAt: Date.now() + 86400000,
  metadata: {
    title: 'Private title',
    private_prompt: 'Private prompt',
    public_title: '',
    share_prompt: false,
    platform: 'openai',
    generation_mode: 'realtime',
    source_type: 'realtime_import',
    model: 'image-model',
    requested_size: '1K',
    aspect_ratio: '1:1',
    quality: 'high',
    content_type: 'image/png',
    byte_size: 14,
    checksum_sha256: 'original-checksum',
    client_blob_key: id,
  },
})
beforeEach(() => {
  vi.resetModules()
  vi.stubGlobal('indexedDB', new IDBFactory())
  vi.stubGlobal('IDBKeyRange', IDBKeyRange)
})

describe('local image ownership and original retention', () => {
  it('round-trips real Blob bytes and isolates the same local ID across users', async () => {
    const storage = await import('../local-images')
    await storage.saveLocalImage(fixture(1, 'same-id'))
    await storage.saveLocalImage(fixture(2, 'same-id'))
    const first = await storage.listLocalImages(1)
    const second = await storage.listLocalImages(2)
    expect(first).toHaveLength(1)
    expect(second).toHaveLength(1)
    expect(await first[0].blob.text()).toBe('original-image')
    expect(first[0].metadata.private_prompt).toBe('Private prompt')
    await storage.deleteLocalImage(1, 'same-id')
    expect(await storage.listLocalImages(1)).toEqual([])
    expect(await storage.listLocalImages(2)).toHaveLength(1)
  })
  it('keeps pending originals when the local gallery image is deleted', async () => {
    const storage = await import('../local-images')
    const item = fixture(1, 'original')
    await storage.saveLocalImage(item)
    await storage.saveLocalImage(item, 'pending')
    await storage.deleteLocalImage(1, item.id)
    expect(await storage.listLocalImages(1)).toEqual([])
    const pending = await storage.listLocalImages(1, 'pending')
    expect(await pending[0].blob.text()).toBe('original-image')
  })
  it('prunes expired Blob storage when opening the library without affecting another user', async () => {
    const storage = await import('../local-images')
    const first = fixture(1, 'expired')
    const second = fixture(2, 'active')
    await storage.saveLocalImage(first)
    await storage.saveLocalImage(second)
    vi.spyOn(Date, 'now').mockReturnValue(first.expiresAt + 1)
    expect(await storage.listLocalImages(1)).toEqual([])
    vi.restoreAllMocks()
    expect(await storage.listLocalImages(1)).toEqual([])
    expect(await storage.listLocalImages(2)).toHaveLength(1)
  })
  it('aborts saving when the signed-in owner changes during the transaction', async () => {
    const storage = await import('../local-images')
    let current = true
    await expect(
      storage.saveLocalImage(fixture(1, 'late'), 'library', () => {
        const allowed = current
        current = false
        return allowed
      })
    ).rejects.toThrow('Image owner changed')
    expect(await storage.listLocalImages(1)).toEqual([])
  })
  it('evicts expired and oldest library images using actual Blob bytes, while pending quota rejects instead of evicting', async () => {
    const { selectLocalImageEvictions } = await import('../local-images')
    const old = fixture(1, 'old')
    old.createdAt = 1
    old.blob = { size: 120 * 1048576 } as Blob
    const newer = fixture(1, 'newer')
    newer.createdAt = 2
    newer.blob = { size: 90 * 1048576 } as Blob
    const expired = fixture(1, 'expired')
    expired.expiresAt = 0
    expect(selectLocalImageEvictions([newer, expired, old], 'library')).toEqual(
      ['expired', 'old']
    )
    old.blob = { size: 250 * 1048576 } as Blob
    newer.blob = { size: 200 * 1048576 } as Blob
    expect(() => selectLocalImageEvictions([old, newer], 'pending')).toThrow(
      'Pending submission storage quota exceeded'
    )
  })
})
