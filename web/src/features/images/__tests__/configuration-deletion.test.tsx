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
import {
  cleanup,
  render,
  screen,
  waitFor,
  within,
} from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { toast } from 'sonner'
import { afterEach, expect, test, vi } from 'vitest'

import { useAuthStore } from '@/stores/auth-store'

import { imageRequest } from '../api'
import { ImageSettings } from '../settings'
import type { ImageAdminConfiguration } from '../types'

vi.mock('../api', () => ({ imageRequest: vi.fn() }))
vi.mock('sonner', () => ({ toast: { success: vi.fn(), error: vi.fn() } }))

const clients: QueryClient[] = []
const bindingKey = 'a'.repeat(64)
const cases = [
  {
    tab: 'Image policies',
    title: 'Delete platform policy',
    url: '/api/option/images/policy',
  },
  {
    tab: 'Channel pools',
    title: 'Delete channel pool binding',
    url: `/api/option/images/pool/${bindingKey}`,
  },
]

function renderSettings(manage = true, deletion?: () => Promise<unknown>) {
  useAuthStore.getState().auth.setUser({
    id: 1,
    username: 'configuration-admin',
    role: 20,
    permissions: {
      admin_permissions: { image_config: { read: true, manage } },
    },
  })
  let config: ImageAdminConfiguration = {
    image_providers: ['openai', 'gemini'],
    media_providers: ['openai', 'gemini'],
    runtime: {
      async_enabled: false,
      worker_concurrency: 1,
      image_concurrency: 1,
      max_reference_images: 14,
      worker_lease_seconds: 120,
      execution_timeout_seconds: 1200,
      account_attempt_timeout_seconds: 300,
      signed_url_expiry_seconds: 3600,
      input_retention_hours: 24,
      task_retention_days: 90,
      result_retention_days: 90,
      reference_fetch_max_retries: 2,
      storage_retry_attempts: 5,
      billing_retry_attempts: 10,
    },
    storage_profiles: [],
    storage_providers: [],
    policies: [
      {
        group: 'default',
        platform: 'gemini',
        pool_mode: 'model_resolution',
        enabled: true,
        async_enabled: true,
        models: '[]',
        version: 3,
      },
    ],
    pools: [11, 12].map((channelId) => ({
      binding_key: bindingKey,
      group: 'default',
      platform: 'gemini',
      mode: 'model_resolution',
      model: 'gemini-image',
      resolution: '1K',
      channel_id: channelId,
      priority: 0,
    })),
  }
  vi.mocked(imageRequest).mockImplementation(async (url, method) => {
    if (url === '/api/option/images' && method === 'GET') return config
    if (url.startsWith('/api/option/images/pool/options?')) {
      return { models: [], channels: [] }
    }
    if (method === 'DELETE') {
      await deletion?.()
      config = url.endsWith('/policy')
        ? { ...config, policies: [] }
        : { ...config, pools: [] }
      return null
    }
    throw new Error(`Unexpected image request: ${method} ${url}`)
  })
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
  clients.push(client)
  render(
    <QueryClientProvider client={client}>
      <ImageSettings />
    </QueryClientProvider>
  )
}

afterEach(() => {
  cleanup()
  for (const client of clients) client.clear()
  clients.length = 0
  useAuthStore.getState().auth.reset()
  vi.clearAllMocks()
})

test.each(cases)(
  'cancelling deletion in $tab leaves the configuration intact',
  async ({ tab, title }) => {
    const user = userEvent.setup()
    renderSettings()
    await user.click(await screen.findByRole('tab', { name: tab }))
    await user.click(screen.getByRole('button', { name: 'Delete' }))
    const dialog = screen.getByRole('alertdialog', { name: title })
    expect(dialog).toHaveTextContent('default / gemini')
    await user.click(within(dialog).getByRole('button', { name: 'Cancel' }))
    await waitFor(() =>
      expect(screen.queryByRole('alertdialog')).not.toBeInTheDocument()
    )
    expect(imageRequest).not.toHaveBeenCalledWith(
      expect.anything(),
      'DELETE',
      expect.anything()
    )
    expect(screen.getByRole('row', { name: /default gemini/ })).toBeVisible()
  }
)

test.each(cases)(
  'confirming deletion in $tab blocks duplicate requests and refreshes the list',
  async ({ tab, title, url }) => {
    const user = userEvent.setup()
    let finish: () => void = () => undefined
    const pending = new Promise<void>((resolve) => {
      finish = resolve
    })
    renderSettings(true, () => pending)
    await user.click(await screen.findByRole('tab', { name: tab }))
    await user.click(screen.getByRole('button', { name: 'Delete' }))
    const dialog = screen.getByRole('alertdialog', { name: title })
    const confirm = within(dialog).getByRole('button', { name: 'Delete' })
    await user.click(confirm)
    await waitFor(() => expect(confirm).toBeDisabled())
    expect(
      within(dialog).getByRole('button', { name: 'Cancel' })
    ).toBeDisabled()
    await user.click(confirm)
    const requests = vi
      .mocked(imageRequest)
      .mock.calls.filter(([, method]) => method === 'DELETE')
    expect(requests).toHaveLength(1)
    expect(requests[0]).toEqual(
      tab === 'Image policies'
        ? [url, 'DELETE', { group: 'default', platform: 'gemini', version: 3 }]
        : [url, 'DELETE']
    )
    finish()
    await waitFor(() =>
      expect(screen.queryByRole('alertdialog')).not.toBeInTheDocument()
    )
    expect(
      screen.queryByRole('row', { name: /default gemini/ })
    ).not.toBeInTheDocument()
    expect(toast.success).toHaveBeenCalledWith('Deleted')
  }
)

test.each(cases)(
  'a failed deletion in $tab preserves the row and permits retry',
  async ({ tab, title }) => {
    const user = userEvent.setup()
    renderSettings(true, async () => {
      throw new Error('Configuration changed; reload and retry')
    })
    await user.click(await screen.findByRole('tab', { name: tab }))
    await user.click(screen.getByRole('button', { name: 'Delete' }))
    const dialog = screen.getByRole('alertdialog', { name: title })
    await user.click(within(dialog).getByRole('button', { name: 'Delete' }))
    await waitFor(() =>
      expect(toast.error).toHaveBeenCalledWith(
        'Configuration changed; reload and retry'
      )
    )
    expect(within(dialog).getByRole('button', { name: 'Delete' })).toBeEnabled()
    expect(within(dialog).getByRole('button', { name: 'Cancel' })).toBeEnabled()
    expect(toast.success).not.toHaveBeenCalled()
    await user.click(within(dialog).getByRole('button', { name: 'Cancel' }))
    expect(screen.getByRole('row', { name: /default gemini/ })).toBeVisible()
  }
)

test.each(cases)(
  'read-only users cannot delete configuration in $tab',
  async ({ tab }) => {
    const user = userEvent.setup()
    renderSettings(false)
    await user.click(await screen.findByRole('tab', { name: tab }))
    const remove = screen.getByRole('button', { name: 'Delete' })
    expect(remove).toBeDisabled()
    await user.click(remove)
    expect(screen.queryByRole('alertdialog')).not.toBeInTheDocument()
    expect(
      vi
        .mocked(imageRequest)
        .mock.calls.some(([, method]) => method === 'DELETE')
    ).toBe(false)
  }
)
