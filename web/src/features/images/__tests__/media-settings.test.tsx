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
  render(
    <QueryClientProvider client={client}>
      <MediaSettingsCard />
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
})
