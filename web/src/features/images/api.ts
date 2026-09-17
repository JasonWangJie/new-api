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
import { api } from '@/lib/api'

import type { ImageCapabilities, ImageTaskDetail, ImageTaskList } from './types'

export async function imageRequest<T>(
  url: string,
  method = 'GET',
  data?: unknown,
  signal?: AbortSignal,
  headers?: Record<string, string>
): Promise<T> {
  const response = await api.request<{
    success: boolean
    message: string
    data: T
  }>({ url, method, data, signal, headers, disableDuplicate: true })
  if (!response.data.success) throw new Error(response.data.message)
  return response.data.data
}

export const getImageCapabilities = (id: number, signal?: AbortSignal) =>
  imageRequest<ImageCapabilities>(
    `/api/user/image-workbench/capabilities/${id}`,
    'GET',
    undefined,
    signal
  )
export const imageTasksPath = (admin: boolean) =>
  `/api/${admin ? 'admin' : 'user'}/async-image-tasks`
export const getImageTasks = (
  admin: boolean,
  params: URLSearchParams,
  signal?: AbortSignal
) =>
  imageRequest<ImageTaskList>(
    `${imageTasksPath(admin)}?${params}`,
    'GET',
    undefined,
    signal
  )
export const getImageTask = (
  admin: boolean,
  id: string,
  signal?: AbortSignal
) =>
  imageRequest<ImageTaskDetail>(
    `${imageTasksPath(admin)}/${encodeURIComponent(id)}`,
    'GET',
    undefined,
    signal
  )

// Public relay requests use the selected API key. The session client's refresh
// interceptor must never replace that key or replay a generation request.
export async function imageRelayRequest(
  url: string,
  token: string,
  body?: string,
  key?: string,
  signal?: AbortSignal
): Promise<Record<string, unknown>> {
  const response = await fetch(url, {
    method: body === undefined ? 'GET' : 'POST',
    credentials: 'omit',
    cache: 'no-store',
    signal,
    headers: {
      Authorization: `Bearer ${token.startsWith('sk-') ? token : `sk-${token}`}`,
      ...(body === undefined ? {} : { 'Content-Type': 'application/json' }),
      ...(key ? { 'Idempotency-Key': key } : {}),
    },
    body,
  })
  const data = (await response.json()) as Record<string, unknown>
  if (!response.ok) {
    const detail = data.error as { message?: string } | undefined
    throw new Error(detail?.message || `HTTP ${response.status}`)
  }
  return data
}

export async function imageBlob(
  url: string,
  signal?: AbortSignal
): Promise<Blob> {
  // Gateway view paths need a signed URL; browsers can load them via <img>,
  // but download fetch must use the resolved object URL without session cookies.
  let target = url
  if (url.startsWith('/') || url.startsWith(`${window.location.origin}/`)) {
    const resolved = new URL(url, window.location.origin)
    target = (
      await imageRequest<{ url: string }>(
        `${resolved.pathname}${resolved.search}`,
        'GET',
        undefined,
        signal
      )
    ).url
  }
  const response = await fetch(target, {
    signal,
    credentials: 'omit',
    cache: 'no-store',
  })
  if (!response.ok) throw new Error(`HTTP ${response.status}`)
  const blob = await response.blob()
  if (
    !['image/png', 'image/jpeg', 'image/webp'].includes(blob.type) ||
    !blob.size
  ) {
    throw new Error('Unsupported image format')
  }
  return blob
}
