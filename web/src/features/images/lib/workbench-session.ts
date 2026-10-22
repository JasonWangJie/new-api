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
import { nanoid } from 'nanoid'

type WorkbenchMedia = 'image' | 'video'
const pendingSubmissions = new Map<
  string,
  { fingerprint: string; key: string }
>()

export function readWorkbenchTask(
  userId: number,
  media: WorkbenchMedia
): string {
  try {
    return sessionStorage.getItem(`new-api:${media}-task:${userId}`) || ''
  } catch {
    return ''
  }
}

export function rememberWorkbenchTask(
  userId: number,
  media: WorkbenchMedia,
  taskId: string
): void {
  if (taskId) {
    pendingSubmissions.delete(`new-api:${media}-submission:v1:${userId}`)
  }
  try {
    if (taskId) {
      sessionStorage.removeItem(`new-api:${media}-submission:v1:${userId}`)
    }
    sessionStorage.setItem(`new-api:${media}-task:${userId}`, taskId)
  } catch {
    // Storage can be disabled or full; the current workbench remains usable.
  }
}

// Keep only an opaque idempotency key and a digest. Prompts, uploads, and API
// credentials never enter this record. A failed submission can be retried with
// the same input after navigating away or refreshing without creating a new job.
export async function workbenchSubmissionKey(
  userId: number,
  media: WorkbenchMedia,
  tokenId: number,
  endpoint: string,
  body: string | FormData
): Promise<string> {
  const parts: BlobPart[] = [JSON.stringify([tokenId, endpoint])]
  if (typeof body === 'string') {
    parts.push(body)
  } else {
    for (const [name, value] of body.entries()) {
      parts.push(
        JSON.stringify([
          name,
          typeof value === 'string'
            ? value
            : [value.name, value.type, value.size],
        ])
      )
      if (typeof value !== 'string') parts.push(value)
    }
  }
  const bytes = new Uint8Array(await new Blob(parts).arrayBuffer())
  let fingerprint: string
  if (globalThis.crypto?.subtle) {
    const digest = await crypto.subtle.digest('SHA-256', bytes)
    fingerprint = Array.from(new Uint8Array(digest), (byte) =>
      byte.toString(16).padStart(2, '0')
    ).join('')
  } else {
    // Plain HTTP management pages do not expose SubtleCrypto. Load the
    // existing pure-JS crypto dependency only for that browser fallback.
    const { default: forge } = await import('node-forge')
    const digest = forge.md.sha256.create()
    for (let offset = 0; offset < bytes.length; offset += 8192) {
      digest.update(
        String.fromCharCode(...bytes.subarray(offset, offset + 8192))
      )
    }
    fingerprint = digest.digest().toHex()
  }
  const storageKey = `new-api:${media}-submission:v1:${userId}`
  const pending = pendingSubmissions.get(storageKey)
  if (pending?.fingerprint === fingerprint) return pending.key
  const key = nanoid()
  try {
    const previous: unknown = JSON.parse(
      sessionStorage.getItem(storageKey) || 'null'
    )
    if (
      previous &&
      typeof previous === 'object' &&
      'fingerprint' in previous &&
      previous.fingerprint === fingerprint &&
      'key' in previous &&
      typeof previous.key === 'string'
    ) {
      pendingSubmissions.set(storageKey, { fingerprint, key: previous.key })
      return previous.key
    }
  } catch {
    // Ignore corrupt or inaccessible browser storage.
  }
  pendingSubmissions.set(storageKey, { fingerprint, key })
  try {
    sessionStorage.setItem(storageKey, JSON.stringify({ fingerprint, key }))
  } catch {
    // The in-memory submission still retains its key for an immediate retry.
  }
  return key
}
