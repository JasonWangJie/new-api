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
import type { LocalImage } from '../types'

export const LOCAL_IMAGE_LIMITS = {
  library: { days: 30, items: 100, bytes: 200 * 1024 * 1024 },
  pending: { days: 90, items: 40, bytes: 400 * 1024 * 1024 },
} as const
export type LocalImageStore = keyof typeof LOCAL_IMAGE_LIMITS

let database: Promise<IDBDatabase> | undefined
function openImages(): Promise<IDBDatabase> {
  if (database) return database
  database = new Promise((resolve, reject) => {
    const request = indexedDB.open('new-api-images-v1', 1)
    request.onupgradeneeded = () => {
      for (const name of ['library', 'pending']) {
        const store = request.result.createObjectStore(name, {
          keyPath: ['userId', 'id'],
        })
        store.createIndex('userId', 'userId')
      }
    }
    request.onsuccess = () => {
      const db = request.result
      db.addEventListener('versionchange', () => {
        db.close()
        database = undefined
      })
      resolve(db)
    }
    request.addEventListener('error', () => {
      database = undefined
      reject(request.error)
    })
    request.onblocked = () => {
      database = undefined
      reject(new Error('Image storage is blocked by another tab'))
    }
  })
  return database
}

export function selectLocalImageEvictions(
  items: LocalImage[],
  kind: LocalImageStore,
  incoming?: LocalImage,
  now = Date.now()
): string[] {
  const limits = LOCAL_IMAGE_LIMITS[kind]
  if (incoming && incoming.blob.size > limits.bytes) {
    throw new Error('Image exceeds local storage quota')
  }
  const removed = items
    .filter((item) => item.expiresAt <= now)
    .map((item) => item.id)
  const active = items
    .filter((item) => item.expiresAt > now && item.id !== incoming?.id)
    .sort((a, b) => a.createdAt - b.createdAt || a.id.localeCompare(b.id))
  let bytes = active.reduce(
    (total, item) => total + item.blob.size,
    incoming?.blob.size || 0
  )
  let count = active.length + (incoming ? 1 : 0)
  for (const item of active) {
    if (count <= limits.items && bytes <= limits.bytes) break
    // Pending originals must survive until explicit withdrawal or expiry.
    if (kind === 'pending') {
      throw new Error('Pending submission storage quota exceeded')
    }
    removed.push(item.id)
    bytes -= item.blob.size
    count--
  }
  return removed
}

export async function listLocalImages(
  userId: number,
  kind: LocalImageStore = 'library'
): Promise<LocalImage[]> {
  const db = await openImages()
  return new Promise((resolve, reject) => {
    const transaction = db.transaction(kind, 'readwrite')
    const store = transaction.objectStore(kind)
    const request = transaction
      .objectStore(kind)
      .index('userId')
      .getAll(IDBKeyRange.only(userId))
    const images: LocalImage[] = []
    request.onsuccess = () => {
      const now = Date.now()
      for (const item of request.result as LocalImage[]) {
        if (item.expiresAt <= now) store.delete([userId, item.id])
        else images.push(item)
      }
      images.sort((a, b) => b.createdAt - a.createdAt)
    }
    transaction.oncomplete = () => resolve(images)
    transaction.addEventListener('abort', () =>
      reject(transaction.error || new Error('Local image storage was aborted'))
    )
    transaction.addEventListener('error', () => reject(transaction.error))
    request.addEventListener('error', () => reject(request.error))
  })
}

export async function saveLocalImage(
  item: LocalImage,
  kind: LocalImageStore = 'library',
  isCurrentOwner: () => boolean = () => true
): Promise<void> {
  const db = await openImages()
  if (!isCurrentOwner()) throw new Error('Image owner changed')
  return new Promise((resolve, reject) => {
    const transaction = db.transaction(kind, 'readwrite')
    const store = transaction.objectStore(kind)
    const request = store.index('userId').getAll(IDBKeyRange.only(item.userId))
    request.onsuccess = () => {
      try {
        if (!isCurrentOwner()) throw new Error('Image owner changed')
        const evictions = selectLocalImageEvictions(
          request.result as LocalImage[],
          kind,
          item
        )
        for (const id of evictions) store.delete([item.userId, id])
        store.put({
          ...item,
          metadata: {
            ...item.metadata,
            byte_size: item.blob.size,
            content_type: item.blob.type,
          },
        })
      } catch (error) {
        transaction.abort()
        reject(error)
      }
    }
    transaction.oncomplete = () => resolve()
    transaction.addEventListener('error', () => reject(transaction.error))
    transaction.addEventListener('abort', () =>
      reject(transaction.error || new Error('Local image storage was aborted'))
    )
  })
}

export async function deleteLocalImage(
  userId: number,
  id: string,
  kind: LocalImageStore = 'library'
): Promise<void> {
  const db = await openImages()
  return new Promise((resolve, reject) => {
    const transaction = db.transaction(kind, 'readwrite')
    transaction.objectStore(kind).delete([userId, id])
    transaction.oncomplete = () => resolve()
    transaction.addEventListener('error', () => reject(transaction.error))
  })
}

export async function imageChecksum(blob: Blob): Promise<string> {
  const digest = await crypto.subtle.digest('SHA-256', await blob.arrayBuffer())
  return [...new Uint8Array(digest)]
    .map((byte) => byte.toString(16).padStart(2, '0'))
    .join('')
}
