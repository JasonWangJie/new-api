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
import { t } from 'i18next'

import { api } from '@/lib/api'

import type {
  ImageCapabilities,
  ImageTaskDetail,
  ImageTaskList,
  ImageRetainedRequest,
  ImageReliabilityReport,
} from './types'

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
export const getImageRetainedRequest = (id: string, signal?: AbortSignal) =>
  imageRequest<ImageRetainedRequest>(
    `${imageTasksPath(true)}/${encodeURIComponent(id)}/request`,
    'GET',
    undefined,
    signal
  )
export const getImageReliability = (
  params: URLSearchParams,
  signal?: AbortSignal
) =>
  imageRequest<ImageReliabilityReport>(
    `${imageTasksPath(true)}/reliability?${params}`,
    'GET',
    undefined,
    signal
  )
export const mediaTasksPath = (admin: boolean) =>
  `/api/${admin ? 'admin' : 'user'}/media-tasks`
export const getMediaTaskPlatforms = (admin: boolean, signal?: AbortSignal) =>
  imageRequest<string[]>(
    `${mediaTasksPath(admin)}/platforms`,
    'GET',
    undefined,
    signal
  )
export const getImageTasks = (
  admin: boolean,
  params: URLSearchParams,
  signal?: AbortSignal
) =>
  imageRequest<ImageTaskList>(
    `${mediaTasksPath(admin)}?${params}`,
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
    `${id.startsWith('asyncimg_') ? imageTasksPath(admin) : mediaTasksPath(admin)}/${encodeURIComponent(id)}`,
    'GET',
    undefined,
    signal
  )

// Public relay requests use the selected API key. The session client's refresh
// interceptor must never replace that key or replay a generation request.
export async function imageRelayRequest(
  url: string,
  token: string,
  body?: string | FormData,
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
      ...(body === undefined || body instanceof FormData
        ? {}
        : { 'Content-Type': 'application/json' }),
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
  signal?: AbortSignal,
  authenticated = false
): Promise<Blob> {
  let target = url
  if (
    !authenticated &&
    (url.startsWith('/') || url.startsWith(`${window.location.origin}/`))
  ) {
    const resolved = new URL(url, window.location.origin)
    const link = await imageRequest<{ url: string; authenticated?: boolean }>(
      `${resolved.pathname}${resolved.search}`,
      'GET',
      undefined,
      signal
    )
    target = link.url
    authenticated = link.authenticated === true
  }
  let blob: Blob
  if (authenticated) {
    blob = await authenticatedMediaBlob(target, signal, 'image/*')
  } else {
    const response = await fetch(target, {
      signal,
      credentials: 'omit',
      cache: 'no-store',
    })
    if (!response.ok) throw new Error(`HTTP ${response.status}`)
    blob = await response.blob()
  }
  if (
    !['image/png', 'image/jpeg', 'image/webp'].includes(blob.type) ||
    !blob.size
  ) {
    throw new Error(t('Unsupported image format'))
  }
  return blob
}

export async function authenticatedMediaBlob(
  url: string,
  signal?: AbortSignal,
  accept = 'video/*'
): Promise<Blob> {
  const resolved = new URL(url, window.location.origin)
  if (
    resolved.origin !== window.location.origin ||
    !/^\/api\/(user|admin)\/(async-image-tasks|media-tasks)\/[^/]+\/results\/[^/]+\/view$/.test(
      resolved.pathname
    )
  ) {
    throw new Error(t('Media preview failed. Please try again.'))
  }
  const response = await api.request<Blob>({
    url: `${resolved.pathname}${resolved.search}`,
    method: 'GET',
    responseType: 'blob',
    headers: { Accept: accept },
    disableDuplicate: true,
    signal,
  })
  if (
    !response.data.size ||
    (accept === 'video/*' && !response.data.type.startsWith('video/'))
  ) {
    throw new Error(t('Media preview failed. Please try again.'))
  }
  return response.data
}
