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
import type { QueryClient } from '@tanstack/react-query'

import { api } from '@/lib/http-client'
import { requireServerSuccess } from '@/lib/server-error-message'
import { statusQueryOptions } from '@/lib/status-query'

import { canvasIntegration, type CanvasIntegration } from './config'

export async function loadCanvasIntegration(
  queryClient: QueryClient
): Promise<CanvasIntegration> {
  const status = await queryClient.fetchQuery({
    ...statusQueryOptions,
    staleTime: 0,
  })
  const config = canvasIntegration(status)
  if (!config.enabled) throw new Error('AI Infinite Canvas is not available')
  return config
}

export async function authorizeCanvas(
  config: CanvasIntegration,
  search: string
): Promise<string> {
  const params = new URLSearchParams(search)
  const expectedCallback = new URL('/auth/callback', config.url).href
  const fields = [
    'client_id',
    'redirect_uri',
    'state',
    'code_challenge',
    'code_challenge_method',
  ] as const
  if (fields.some((field) => params.getAll(field).length !== 1)) {
    throw new Error('Invalid canvas authorization request')
  }
  const request = Object.fromEntries(
    fields.map((field) => [field, params.get(field)])
  )
  if (
    request.client_id !== config.client_id ||
    request.redirect_uri !== expectedCallback ||
    request.code_challenge_method !== 'S256' ||
    !request.state ||
    !/^[A-Za-z0-9_-]{43}$/.test(request.code_challenge ?? '')
  ) {
    throw new Error('Invalid canvas authorization request')
  }
  const response = await api.post<{
    success: boolean
    message?: string
    data: { redirect_url: string }
  }>('/api/integrations/canvas/authorize', request, {
    singleUseAuthorization: true,
    skipErrorHandler: true,
  })
  const result = requireServerSuccess(response.data)
  const destination = new URL(result.data.redirect_url)
  if (
    destination.origin !== config.url ||
    destination.pathname !== '/auth/callback' ||
    destination.username ||
    destination.password ||
    destination.hash
  ) {
    throw new Error('Invalid canvas authorization response')
  }
  return destination.href
}
