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

test('pool rows follow model and resolution order while keeping their grouped channels and edit target', async () => {
  vi.mocked(imageRequest).mockResolvedValue(openAIOptions)
  const binding = {
    group: 'default',
    platform: 'openai',
    mode: 'model_resolution',
    model: 'image-2',
    resolution: '2K',
    channel_id: 11,
    priority: 7,
  }
  renderPool([
    {
      ...binding,
      binding_key: 'image-10-1k',
      model: 'image-10',
      resolution: '1K',
    },
    { ...binding, binding_key: 'image-2-10k', resolution: '10K' },
    { ...binding, binding_key: 'image-2-4k', resolution: '4K' },
    { ...binding, binding_key: 'image-2-2k' },
    { ...binding, binding_key: 'image-2-1k', resolution: '1K' },
    {
      ...binding,
      binding_key: 'image-2-2k',
      channel_id: 22,
      priority: 3,
    },
  ])

  const rows = within(screen.getByRole('table')).getAllByRole('row').slice(1)
  expect(
    rows.map((row) => {
      const cells = within(row).getAllByRole('cell')
      return [cells[3].textContent, cells[4].textContent]
    })
  ).toEqual([
    ['image-2', '1K'],
    ['image-2', '2K'],
    ['image-2', '4K'],
    ['image-2', '10K'],
    ['image-10', '1K'],
  ])
  expect(within(rows[1]).getByText('11 (7), 22 (3)')).toBeVisible()

  const user = userEvent.setup()
  await user.click(within(rows[1]).getByRole('button', { name: 'Edit' }))
  expect(screen.getByRole('combobox', { name: 'Model' })).toHaveValue(
    'image-2 · Unavailable'
  )
  expect(
    screen.getByRole('combobox', { name: 'Resolution' })
  ).toHaveTextContent('2K')
  expect(screen.getByRole('spinbutton', { name: 'Weight 11' })).toHaveValue(7)
  expect(screen.getByRole('spinbutton', { name: 'Weight 22' })).toHaveValue(3)
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

test.each([7, -3])(
  'editing a pool restores its channels and normalizes saved weight %s before saving',
  async (savedWeight) => {
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
        priority: savedWeight,
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
    expect(screen.getByRole('textbox', { name: 'Group' })).toHaveValue(
      'premium'
    )
    expect(
      screen.getByRole('combobox', { name: 'Platform' })
    ).toHaveTextContent('gemini')
    expect(screen.getByText('#22 · Gemini account')).toBeVisible()
    const weight = screen.getByRole('spinbutton', { name: 'Weight 22' })
    expect(weight).toHaveValue(Math.max(0, savedWeight))
    expect(weight).toHaveAttribute('min', '0')
    expect(weight).toHaveAccessibleDescription(
      'Pool weights share traffic proportionally. All zero means equal traffic; otherwise, zero-weight channels are excluded.'
    )
    await user.clear(weight)
    await user.type(weight, '9')

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
          channels: [{ id: 22, priority: 9 }],
        }
      )
    )
    expect(onSuccess).toHaveBeenCalledOnce()
  }
)
