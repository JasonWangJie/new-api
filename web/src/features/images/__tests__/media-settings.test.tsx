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
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { toast } from 'sonner'
import { afterEach, expect, test, vi } from 'vitest'

import { imageRequest } from '../api'
import { MediaSettingsCard } from '../media-settings'
import { VideoConfiguration } from '../video-settings'

vi.mock('../api', () => ({ imageRequest: vi.fn() }))

const clients: QueryClient[] = []
const mediaSettings = {
  video_async_enabled: false,
  local_path: 'data/media',
  retention_days: 90,
  signed_url_expiry_seconds: 3600,
  max_file_bytes: 1_073_741_824,
  download_timeout_seconds: 900,
  download_concurrency: 2,
  storage_retry_attempts: 5,
}

afterEach(() => {
  for (const client of clients) client.clear()
  clients.length = 0
})

test('shows success feedback after media settings are saved', async () => {
  vi.mocked(imageRequest).mockResolvedValue(mediaSettings)
  const success = vi.spyOn(toast, 'success').mockReturnValue('media-saved')
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
  clients.push(client)
  const onSuccess = vi.fn().mockResolvedValue(undefined)
  render(
    <QueryClientProvider client={client}>
      <MediaSettingsCard onSuccess={onSuccess} />
    </QueryClientProvider>
  )
  const user = userEvent.setup()
  const path = await screen.findByRole('textbox', {
    name: 'Local storage path',
  })
  await user.clear(path)
  await user.type(path, 'data/video')
  await user.click(screen.getByRole('button', { name: 'Save' }))

  await waitFor(() =>
    expect(imageRequest).toHaveBeenCalledWith(
      '/api/option/images/media',
      'PUT',
      { ...mediaSettings, local_path: 'data/video' }
    )
  )
  expect(success).toHaveBeenCalledWith('Saved')
  expect(onSuccess).toHaveBeenCalledOnce()
})

test('shows readiness and imports copy-ready xAI and Seedance video policies', async () => {
  vi.mocked(imageRequest).mockImplementation(async (url, method, body) => {
    if (url === '/api/option/images/media') return mediaSettings
    if (url === '/api/option/images/policy' && method === 'PUT') return body
    throw new Error(`Unexpected request: ${method} ${url}`)
  })
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
  clients.push(client)
  const onSuccess = vi.fn().mockResolvedValue(undefined)
  render(
    <QueryClientProvider client={client}>
      <VideoConfiguration
        config={{
          runtime: {},
          storage_profiles: [],
          policies: [],
          pools: [],
          storage_providers: [],
          media_providers: ['xai', 'doubao'],
          video_readiness: {
            ready: false,
            checks: [
              { key: 'video_async_enabled', ready: true },
              {
                key: 'payload_encryption',
                ready: false,
                reason:
                  'Asynchronous media payload encryption keys are unavailable',
              },
            ],
          },
          video_plugin_templates: [
            {
              provider: 'xai',
              name: 'xAI Video',
              models: ['grok-imagine-video-1.5'],
              video_profiles: [],
            },
            {
              provider: 'doubao',
              name: 'Doubao Video',
              models: ['doubao-seedance-2-0-260128'],
              video_profiles: [],
            },
          ],
        }}
        onSuccess={onSuccess}
      />
    </QueryClientProvider>
  )
  const user = userEvent.setup()

  expect(screen.getByText('Global video switch')).toBeVisible()
  expect(screen.getByText('Payload encryption key')).toBeVisible()
  expect(
    screen.getByText(
      'Asynchronous media payload encryption keys are unavailable'
    )
  ).toBeVisible()
  const catalog = screen.getByRole('textbox', {
    name: 'Combined image and video model catalog',
  })
  expect((catalog as HTMLTextAreaElement).value).toContain(
    'grok-imagine-video-1.5'
  )

  await user.click(screen.getByRole('combobox', { name: 'Provider' }))
  await user.click(screen.getByRole('option', { name: /Doubao Video/ }))
  await user.click(
    screen.getByRole('button', { name: 'Import Seedance video models' })
  )
  expect((catalog as HTMLTextAreaElement).value).toContain(
    'doubao-seedance-2-0-260128'
  )
  expect((catalog as HTMLTextAreaElement).value).toContain(
    '"media_type": "video"'
  )
  expect(screen.getByText(/Seedance has no built-in USD price/)).toBeVisible()

  await user.click(
    screen.getByRole('button', { name: 'Video configuration examples' })
  )
  expect(screen.getByRole('dialog')).toHaveTextContent('Multimodal references')
  expect(screen.getByRole('dialog')).toHaveTextContent('asset://character')
  expect(screen.getByRole('dialog')).toHaveTextContent('/v1/videos/edits_async')
  expect(screen.getByRole('dialog')).toHaveTextContent(
    '/v1/videos/extensions_async'
  )
  expect(screen.getByRole('dialog')).toHaveTextContent('"output_format":"mov"')
  expect(screen.getByRole('dialog')).toHaveTextContent(
    '/v1/media/tasks_async/$TASK_ID'
  )
})
