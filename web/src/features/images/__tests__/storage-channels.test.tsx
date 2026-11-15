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
import { cleanup, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, expect, test, vi } from 'vitest'

import { imageRequest } from '../api'
import { ImageStorageForm } from '../components/image-storage-settings'
import type { ImageAdminConfiguration } from '../types'

vi.mock('../api', () => ({ imageRequest: vi.fn() }))
const permissions = vi.hoisted(() => ({ manage: true }))
vi.mock('@/lib/admin-permissions', () => ({
  hasPermission: () => permissions.manage,
}))

afterEach(() => {
  cleanup()
  vi.resetAllMocks()
  permissions.manage = true
})

const config: ImageAdminConfiguration = {
  runtime: {},
  policies: [],
  pools: [],
  storage_revision: 7,
  storage_providers: ['local', 's3', 'r2'],
  storage_profiles: [
    {
      profile_id: 'profile-1',
      channel_id: 'channel-1',
      name: 'Main R2',
      current_revision: true,
      credential_configured: true,
      class: 'temporary',
      backend: 's3',
      provider: 'r2',
      root: '',
      endpoint: 'https://account.r2.cloudflarestorage.com',
      bucket: 'images',
      region: 'auto',
      prefix: 'generated',
      active: true,
      path_style: false,
    },
  ],
}

function renderStorage(initial = config) {
  const onSuccess = vi.fn().mockResolvedValue(undefined)
  const client = new QueryClient({
    defaultOptions: { mutations: { retry: false }, queries: { retry: false } },
  })
  render(
    <QueryClientProvider client={client}>
      <ImageStorageForm config={initial} onSuccess={onSuccess} />
    </QueryClientProvider>
  )
  return onSuccess
}

test('new channels are saved inactive without changing the current writer', async () => {
  vi.mocked(imageRequest).mockResolvedValue({})
  renderStorage()
  const user = userEvent.setup()
  await user.type(screen.getByLabelText('Storage channel name'), 'Backup local')
  await user.click(screen.getByRole('button', { name: 'Save storage channel' }))
  await waitFor(() =>
    expect(imageRequest).toHaveBeenCalledWith(
      '/api/option/images/storage',
      'POST',
      expect.objectContaining({
        revision: 7,
        profile: expect.objectContaining({
          name: 'Backup local',
          active: false,
        }),
      })
    )
  )
})

test('editing a channel leaves saved credentials unchanged when fields are blank', async () => {
  vi.mocked(imageRequest).mockResolvedValue({})
  renderStorage()
  const user = userEvent.setup()
  await user.click(screen.getByRole('button', { name: 'Edit' }))
  expect(screen.getByLabelText('Access key')).toHaveValue('')
  expect(screen.getByLabelText('Secret key')).toHaveValue('')
  await user.click(screen.getByRole('button', { name: 'Save storage channel' }))
  await waitFor(() => expect(imageRequest).toHaveBeenCalled())
  expect(vi.mocked(imageRequest).mock.calls[0].slice(0, 2)).toEqual([
    '/api/option/images/storage/profile-1',
    'PUT',
  ])
  expect(vi.mocked(imageRequest).mock.calls[0][2]).not.toHaveProperty(
    'credentials'
  )
})

test('the active channel explains replacement without offering a failing stop action', () => {
  renderStorage()
  expect(
    screen.queryByRole('button', { name: 'Stop new writes' })
  ).not.toBeInTheDocument()
  expect(
    screen.getByText(
      'Enable another channel for the same purpose to replace the current writer.'
    )
  ).toBeVisible()
  expect(imageRequest).not.toHaveBeenCalled()
})

test('read-only administrators cannot change channels or run write tests', () => {
  permissions.manage = false
  renderStorage()
  expect(
    screen.queryByRole('button', { name: 'Save storage channel' })
  ).not.toBeInTheDocument()
  expect(
    screen.queryByRole('button', { name: 'Test storage' })
  ).not.toBeInTheDocument()
  expect(screen.getByText('Main R2')).toBeVisible()
})

test('storage activity shows the serving node snapshot and can be refreshed', async () => {
  const client = new QueryClient()
  const onSuccess = vi.fn().mockResolvedValue(undefined)
  render(
    <QueryClientProvider client={client}>
      <ImageStorageForm
        config={{
          ...config,
          storage_upload_status: { limit: 4, active: 2, queued: 3 },
        }}
        onSuccess={onSuccess}
      />
    </QueryClientProvider>
  )
  expect(
    screen.getByRole('region', { name: 'Storage upload concurrency' })
  ).toHaveTextContent('Active: 2')
  await userEvent.setup().click(screen.getByRole('button', { name: 'Refresh' }))
  expect(onSuccess).toHaveBeenCalledOnce()
})

test('enabling a saved channel requires confirmation and submits the configuration revision', async () => {
  vi.mocked(imageRequest).mockResolvedValue({})
  renderStorage({
    ...config,
    storage_profiles: config.storage_profiles.map((profile) => ({
      ...profile,
      active: false,
    })),
  })
  const user = userEvent.setup()
  await user.click(screen.getByRole('button', { name: 'Enable channel' }))
  expect(imageRequest).not.toHaveBeenCalled()
  await user.click(screen.getByRole('button', { name: 'Continue' }))
  await waitFor(() =>
    expect(imageRequest).toHaveBeenCalledWith(
      '/api/option/images/storage/profile-1/activate',
      'POST',
      { revision: 7 }
    )
  )
})

test('failed storage saves retain the draft for correction', async () => {
  vi.mocked(imageRequest).mockRejectedValue(new Error('Configuration changed'))
  renderStorage()
  const user = userEvent.setup()
  await user.type(screen.getByLabelText('Storage channel name'), 'Backup local')
  await user.click(screen.getByRole('button', { name: 'Save storage channel' }))
  expect(await screen.findByRole('alert')).toHaveTextContent(
    'Configuration changed'
  )
  expect(screen.getByLabelText('Storage channel name')).toHaveValue(
    'Backup local'
  )
})

test.each(['save', 'rotate'] as const)(
  'failed %s requests keep storage credentials out of cached errors',
  async (action) => {
    const client = new QueryClient({
      defaultOptions: { mutations: { retry: false } },
    })
    vi.mocked(imageRequest).mockRejectedValue({
      isAxiosError: true,
      response: {
        status: 400,
        data: { success: false, message: 'Storage access denied' },
      },
      config: {
        data: {
          credentials: {
            access_key: 'private-access',
            secret_key: 'private-secret',
          },
        },
      },
    })
    render(
      <QueryClientProvider client={client}>
        <ImageStorageForm config={config} onSuccess={vi.fn()} />
      </QueryClientProvider>
    )
    const user = userEvent.setup()
    await user.click(screen.getByRole('button', { name: 'Edit' }))
    await user.type(screen.getByLabelText('Access key'), 'private-access')
    await user.type(screen.getByLabelText('Secret key'), 'private-secret')
    if (action === 'save') {
      await user.click(
        screen.getByRole('button', { name: 'Save storage channel' })
      )
    } else {
      await user.click(
        screen.getByRole('button', { name: 'Rotate channel credentials' })
      )
      await user.click(screen.getByRole('button', { name: 'Continue' }))
    }
    expect(await screen.findByRole('alert')).toHaveTextContent(
      'Storage access denied'
    )
    expect(screen.getByLabelText('Secret key')).toHaveValue('private-secret')
    const errors = JSON.stringify(
      client
        .getMutationCache()
        .getAll()
        .map((mutation) => ({
          variables: mutation.state.variables,
          error: mutation.state.error,
        }))
    )
    expect(errors).not.toContain('private-access')
    expect(errors).not.toContain('private-secret')
  }
)
