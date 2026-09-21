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
import { ImagePoolForm } from '../settings'

vi.mock('../api', () => ({ imageRequest: vi.fn() }))

const clients: QueryClient[] = []
const openAIOptions = {
  models: [{ id: 'image-alpha', label: 'Image Alpha', channel_ids: [11] }],
  channels: [{ id: 11, name: 'OpenAI primary' }],
}
const geminiOptions = {
  models: [{ id: 'image-beta', label: 'Image Beta', channel_ids: [22] }],
  channels: [{ id: 22, name: 'Gemini account' }],
}

function renderPool(
  pools: Array<{
    binding_key: string
    group: string
    platform: string
    mode: string
    model: string
    resolution: string
    channel_id: number
    priority: number
  }> = []
) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
  clients.push(client)
  const onSuccess = vi.fn().mockResolvedValue(undefined)
  render(
    <QueryClientProvider client={client}>
      <ImagePoolForm
        config={{
          image_providers: ['openai', 'gemini'],
          media_providers: ['openai', 'gemini'],
          runtime: {},
          storage_profiles: [],
          policies: [],
          pools,
          storage_providers: [],
        }}
        onSuccess={onSuccess}
      />
    </QueryClientProvider>
  )
  return { onSuccess }
}

afterEach(() => {
  cleanup()
  for (const client of clients) client.clear()
  clients.length = 0
  vi.clearAllMocks()
})

test('switching platform replaces the selectable models and channels', async () => {
  vi.mocked(imageRequest).mockImplementation(async (url, method) => {
    if (method === 'GET' && url.includes('platform=openai')) {
      return openAIOptions
    }
    if (method === 'GET' && url.includes('platform=gemini')) {
      return geminiOptions
    }
    throw new Error(`Unexpected image request: ${method} ${url}`)
  })
  const user = userEvent.setup()
  renderPool()

  await user.click(screen.getByRole('combobox', { name: 'Pool binding' }))
  await user.click(screen.getByRole('option', { name: 'Model' }))
  const model = await screen.findByRole('combobox', { name: 'Model' })
  await waitFor(() => expect(model).toBeEnabled())
  await user.click(model)
  await user.click(screen.getByRole('option', { name: 'Image Alpha' }))
  const channels = screen.getByRole('combobox', { name: 'Channels' })
  await user.click(channels)
  await user.click(screen.getByRole('option', { name: '#11 · OpenAI primary' }))
  expect(screen.getAllByText('#11 · OpenAI primary').length).toBeGreaterThan(0)
  await user.keyboard('{Escape}')

  await user.click(screen.getByRole('combobox', { name: 'Platform' }))
  await user.click(screen.getByRole('option', { name: 'gemini' }))

  expect(screen.queryByText('#11 · OpenAI primary')).not.toBeInTheDocument()
  await waitFor(() => expect(model).toBeEnabled())
  await user.click(model)
  expect(
    screen.queryByRole('option', { name: 'Image Alpha' })
  ).not.toBeInTheDocument()
  await user.click(screen.getByRole('option', { name: 'Image Beta' }))
  await user.click(screen.getByRole('combobox', { name: 'Channels' }))
  expect(
    screen.queryByRole('option', { name: '#11 · OpenAI primary' })
  ).not.toBeInTheDocument()
  expect(
    screen.getByRole('option', { name: '#22 · Gemini account' })
  ).toBeVisible()
})

test('editing a pool restores its model channels and priorities before saving', async () => {
  vi.mocked(imageRequest).mockImplementation(async (url, method, data) => {
    if (method === 'GET' && url.includes('platform=openai')) {
      return openAIOptions
    }
    if (method === 'GET' && url.includes('platform=gemini')) {
      return geminiOptions
    }
    if (method === 'PUT' && url === '/api/option/images/pool') {
      return data
    }
    throw new Error(`Unexpected image request: ${method} ${url}`)
  })
  const user = userEvent.setup()
  const { onSuccess } = renderPool([
    {
      binding_key: 'saved-pool',
      group: 'premium',
      platform: 'gemini',
      mode: 'model',
      model: 'image-beta',
      resolution: '',
      channel_id: 22,
      priority: 7,
    },
  ])

  await waitFor(() =>
    expect(screen.getByRole('combobox', { name: 'Channels' })).toBeEnabled()
  )
  await user.click(screen.getByRole('button', { name: 'Edit' }))

  await waitFor(() =>
    expect(screen.getByRole('combobox', { name: 'Model' })).toHaveValue(
      'Image Beta'
    )
  )
  expect(screen.getByRole('textbox', { name: 'Group' })).toHaveValue('premium')
  expect(screen.getByRole('combobox', { name: 'Platform' })).toHaveTextContent(
    'gemini'
  )
  expect(screen.getByText('#22 · Gemini account')).toBeVisible()
  expect(screen.getByRole('spinbutton', { name: 'Priority 22' })).toHaveValue(7)

  await user.click(screen.getByRole('button', { name: 'Save' }))

  await waitFor(() =>
    expect(imageRequest).toHaveBeenCalledWith(
      '/api/option/images/pool',
      'PUT',
      {
        group: 'premium',
        platform: 'gemini',
        mode: 'model',
        model: 'image-beta',
        resolution: '',
        channels: [{ id: 22, priority: 7 }],
      }
    )
  )
  expect(onSuccess).toHaveBeenCalledOnce()
})
