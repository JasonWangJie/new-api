import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
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
import { render, screen } from '@testing-library/react'
import { afterEach, expect, test, vi } from 'vitest'

import { imageRequest } from '../api'
import { ImageRuntimeStatus } from '../components/image-runtime-status'

vi.mock('../api', () => ({ imageRequest: vi.fn() }))

afterEach(() => vi.clearAllMocks())

function renderStatus() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  const view = render(
    <QueryClientProvider client={client}>
      <ImageRuntimeStatus />
    </QueryClientProvider>
  )
  return { ...view, client }
}

test('distinguishes saved settings from the serving node while workers drain', async () => {
  vi.mocked(imageRequest).mockResolvedValue({
    node: 'worker-a',
    configured: { worker_concurrency: 2, recovery_interval_seconds: 15 },
    effective: {
      running: true,
      worker_concurrency: 2,
      active_workers: 4,
      recovery_interval_seconds: 30,
    },
  })
  const view = renderStatus()
  expect(await screen.findByText('worker-a')).toBeVisible()
  expect(
    screen.getByRole('columnheader', { name: 'Saved value' })
  ).toBeVisible()
  expect(
    screen.getByRole('columnheader', { name: 'Effective value' })
  ).toBeVisible()
  expect(screen.getByText('Workers still finishing tasks: 2')).toBeVisible()
  expect(screen.getByText('Settings are being applied')).toBeVisible()
  expect(imageRequest).toHaveBeenCalledWith(
    '/api/option/images/runtime-status',
    'GET',
    undefined,
    expect.any(AbortSignal)
  )
  view.unmount()
  view.client.clear()
})

test('does not present saved settings as effective when workers are stopped', async () => {
  vi.mocked(imageRequest).mockResolvedValue({
    node: 'worker-b',
    configured: { worker_concurrency: 4, recovery_interval_seconds: 30 },
    effective: {
      running: false,
      worker_concurrency: 0,
      active_workers: 0,
      recovery_interval_seconds: 0,
    },
  })
  const view = renderStatus()
  expect(
    await screen.findByText('Image workers are not running on this node')
  ).toBeVisible()
  expect(
    screen.queryByText('Settings are being applied')
  ).not.toBeInTheDocument()
  expect(screen.queryByRole('table')).not.toBeInTheDocument()
  view.unmount()
  view.client.clear()
})

test('reports unavailable worker status without claiming settings are effective', async () => {
  vi.mocked(imageRequest).mockRejectedValue(new Error('offline'))
  const view = renderStatus()
  expect(await screen.findByRole('alert')).toHaveTextContent(
    'Worker status is unavailable'
  )
  expect(screen.queryByRole('table')).not.toBeInTheDocument()
  view.unmount()
  view.client.clear()
})
