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
import { afterEach, beforeEach, expect, test, vi } from 'vitest'

import { getGroups } from '@/features/users/api'
import { useAuthStore } from '@/stores/auth-store'

import { imageRequest } from '../api'
import { ImagePoolForm } from '../settings'

vi.mock('../api', () => ({ imageRequest: vi.fn() }))
vi.mock('@/features/users/api', () => ({ getGroups: vi.fn() }))
vi.mock('sonner', () => ({ toast: { success: vi.fn(), error: vi.fn() } }))

const clients: QueryClient[] = []
const openAIOptions = {
  models: [{ id: 'image-alpha', label: 'Image Alpha', channel_ids: [11] }],
  channels: [{ id: 11, name: 'OpenAI primary' }],
}
const geminiOptions = {
  models: [{ id: 'image-beta', label: 'Image Beta', channel_ids: [22] }],
  channels: [{ id: 22, name: 'Gemini account' }],
}
const savedBinding = {
  id: 22,
  binding_key: 'saved-pool',
  group: 'premium',
  platform: 'gemini',
  mode: 'model',
  model: 'image-beta',
  resolution: '',
  channel_id: 22,
  priority: 7,
}

function renderPool(
  pools: Array<{
    id?: number
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
  useAuthStore.getState().auth.setUser({
    id: 1,
    username: 'configuration-admin',
    role: 20,
    permissions: {
      admin_permissions: { image_config: { read: true, manage: true } },
    },
  })
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
  clients.push(client)
  const onSuccess = vi.fn().mockResolvedValue(undefined)
  const configuration = {
    image_providers: ['openai', 'gemini'],
    media_providers: ['openai', 'gemini'],
    runtime: {},
    storage_profiles: [],
    policies: [],
    pools,
    storage_providers: [],
  }
  const view = render(
    <QueryClientProvider client={client}>
      <ImagePoolForm config={configuration} onSuccess={onSuccess} />
    </QueryClientProvider>
  )
  return {
    onSuccess,
    refreshPools: (nextPools: typeof pools) =>
      view.rerender(
        <QueryClientProvider client={client}>
          <ImagePoolForm
            config={{ ...configuration, pools: nextPools }}
            onSuccess={onSuccess}
          />
        </QueryClientProvider>
      ),
  }
}

beforeEach(() => {
  vi.mocked(getGroups).mockResolvedValue({
    success: true,
    data: ['default', 'premium'],
  })
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
})

afterEach(() => {
  cleanup()
  for (const client of clients) client.clear()
  clients.length = 0
  useAuthStore.getState().auth.reset()
  vi.clearAllMocks()
})

test('saved pool partitions start collapsed until their header is expanded', async () => {
  const user = userEvent.setup()
  renderPool([savedBinding])

  const partition = screen.getByRole('button', {
    name: /premium.*gemini.*All resolutions/,
  })
  expect(partition).toHaveAttribute('aria-expanded', 'false')
  expect(screen.queryByRole('button', { name: 'Edit' })).not.toBeInTheDocument()

  await user.click(partition)

  expect(partition).toHaveAttribute('aria-expanded', 'true')
  expect(screen.getByRole('button', { name: 'Edit' })).toBeVisible()
})

test('keyboard and toolbar controls expand and collapse pool partitions', async () => {
  const user = userEvent.setup()
  renderPool([
    savedBinding,
    { ...savedBinding, binding_key: 'default-pool', group: 'default' },
  ])
  const partitions = screen.getAllByRole('button', { expanded: false })
  partitions[0].focus()

  await user.keyboard('{Enter}')

  expect(partitions[0]).toHaveAttribute('aria-expanded', 'true')
  expect(partitions[1]).toHaveAttribute('aria-expanded', 'false')
  expect(screen.getAllByRole('table')).toHaveLength(1)

  await user.keyboard(' ')

  expect(partitions[0]).toHaveAttribute('aria-expanded', 'false')
  expect(screen.queryByRole('table')).not.toBeInTheDocument()

  await user.click(screen.getByRole('button', { name: 'Expand all' }))

  for (const partition of partitions) {
    expect(partition).toHaveAttribute('aria-expanded', 'true')
  }
  expect(screen.getAllByRole('table')).toHaveLength(2)

  await user.click(screen.getByRole('button', { name: 'Collapse all' }))

  for (const partition of partitions) {
    expect(partition).toHaveAttribute('aria-expanded', 'false')
  }
  expect(screen.queryByRole('table')).not.toBeInTheDocument()
})

test('refreshing pool data preserves expanded partitions while new partitions stay collapsed', async () => {
  const user = userEvent.setup()
  const { refreshPools } = renderPool([savedBinding])
  await user.click(
    screen.getByRole('button', { name: /premium.*gemini.*All resolutions/ })
  )

  refreshPools([
    savedBinding,
    { ...savedBinding, binding_key: 'another-model', model: 'image-gamma' },
    { ...savedBinding, binding_key: 'default-pool', group: 'default' },
  ])

  expect(
    screen.getByRole('button', { name: /premium.*gemini.*All resolutions/ })
  ).toHaveAttribute('aria-expanded', 'true')
  expect(
    screen.getByRole('button', { name: /default.*gemini.*All resolutions/ })
  ).toHaveAttribute('aria-expanded', 'false')
  expect(screen.getAllByRole('table')).toHaveLength(1)
  const table = screen.getByRole('table', {
    name: 'premium / gemini / All resolutions',
  })
  expect(within(table).getByRole('cell', { name: 'image-gamma' })).toBeVisible()
})

test('an empty channel pool disables expanding all and still offers adding a binding', () => {
  renderPool()

  expect(screen.getByRole('button', { name: 'Expand all' })).toBeDisabled()
  expect(
    screen.getByRole('button', { name: 'Add channel pool binding' })
  ).toBeEnabled()
  expect(
    screen.queryByRole('button', { expanded: false })
  ).not.toBeInTheDocument()
  expect(screen.queryByRole('button', { name: 'Edit' })).not.toBeInTheDocument()
  expect(
    screen.queryByRole('button', { name: 'Delete' })
  ).not.toBeInTheDocument()
})

test('pool partitions sort by group, platform and resolution while their bindings keep natural model order and merged channels', async () => {
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
    {
      ...binding,
      binding_key: 'premium-openai',
      group: 'premium',
      resolution: '1K',
    },
    {
      ...binding,
      binding_key: 'default-gemini',
      platform: 'gemini',
    },
    {
      ...binding,
      binding_key: 'model-only',
      mode: 'model',
      resolution: '',
    },
  ])

  const partitions = screen.getAllByRole('button', { expanded: false })
  const expectedOrder = [
    /default.*gemini.*2K/,
    /default.*openai.*All resolutions/,
    /default.*openai.*1K/,
    /default.*openai.*2K/,
    /default.*openai.*4K/,
    /default.*openai.*10K/,
    /premium.*openai.*1K/,
  ]
  expect(partitions).toHaveLength(expectedOrder.length)
  for (const [index, name] of expectedOrder.entries()) {
    expect(partitions[index]).toHaveAccessibleName(name)
  }
  expect(
    within(partitions[2]).getByLabelText('Pool binding: 2')
  ).toHaveTextContent('2')
  expect(
    within(partitions[3]).getByLabelText('Pool binding: 1')
  ).toHaveTextContent('1')
  expect(screen.queryByRole('table')).not.toBeInTheDocument()

  const user = userEvent.setup()
  await user.click(partitions[2])
  const modelTable = screen.getByRole('table', {
    name: 'default / openai / 1K',
  })
  const rows = within(modelTable).getAllByRole('row').slice(1)
  expect(
    rows.map((row) => within(row).getAllByRole('cell')[1].textContent)
  ).toEqual(['image-2', 'image-10'])
  expect(
    within(modelTable)
      .getAllByRole('columnheader')
      .map((header) => header.textContent)
  ).toEqual(['Pool binding', 'Model', 'Channel', 'Actions'])

  await user.click(partitions[3])
  const mergedTable = screen.getByRole('table', {
    name: 'default / openai / 2K',
  })
  expect(within(mergedTable).getAllByRole('row')).toHaveLength(2)
  const mergedRow = within(mergedTable).getAllByRole('row')[1]
  expect(within(mergedRow).getByText('11 (7), 22 (3)')).toBeVisible()

  await user.click(within(mergedRow).getByRole('button', { name: 'Edit' }))
  expect(
    screen.getByRole('dialog', { name: 'Edit channel pool binding' })
  ).toBeVisible()
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

  expect(
    screen.queryByRole('combobox', { name: 'Group' })
  ).not.toBeInTheDocument()
  await user.click(
    screen.getByRole('button', { name: 'Add channel pool binding' })
  )
  expect(
    screen.getByRole('dialog', { name: 'Add channel pool binding' })
  ).toBeVisible()
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
        id: 22,
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

    await user.click(
      screen.getByRole('button', { name: /premium.*gemini.*All resolutions/ })
    )
    await user.click(screen.getByRole('button', { name: 'Edit' }))

    await waitFor(() =>
      expect(screen.getByRole('combobox', { name: 'Model' })).toHaveValue(
        'Image Beta'
      )
    )
    expect(screen.getByRole('combobox', { name: 'Group' })).toHaveValue(
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
          expected_ids: [22],
          group: 'premium',
          platform: 'gemini',
          mode: 'model',
          model: 'image-beta',
          resolution: '',
          channels: [{ id: 22, priority: 9 }],
        }
      )
    )
    await waitFor(() => expect(onSuccess).toHaveBeenCalledOnce())
    await waitFor(() =>
      expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    )
  }
)

test.each(['Cancel', 'Escape'])(
  '%s discards pool edits and reopening restores the saved binding',
  async (dismiss) => {
    const user = userEvent.setup()
    renderPool([savedBinding])
    await user.click(
      screen.getByRole('button', { name: /premium.*gemini.*All resolutions/ })
    )
    const edit = screen.getByRole('button', { name: 'Edit' })
    await user.click(edit)
    const weight = await screen.findByRole('spinbutton', { name: 'Weight 22' })
    await user.clear(weight)
    await user.type(weight, '19')

    if (dismiss === 'Cancel') {
      await user.click(screen.getByRole('button', { name: 'Cancel' }))
    } else {
      await user.keyboard('{Escape}')
    }

    await waitFor(() =>
      expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    )
    expect(screen.getByRole('cell', { name: '22 (7)' })).toBeVisible()
    await waitFor(() => expect(edit).toHaveFocus())
    expect(
      vi
        .mocked(imageRequest)
        .mock.calls.filter(([, method]) => method === 'PUT')
    ).toHaveLength(0)

    await user.click(edit)
    expect(screen.getByRole('spinbutton', { name: 'Weight 22' })).toHaveValue(7)
    expect(screen.getByRole('combobox', { name: 'Group' })).toHaveValue(
      'premium'
    )
  }
)

test('a failed pool save keeps the dialog and draft available for retry', async () => {
  const user = userEvent.setup()
  const { onSuccess } = renderPool([savedBinding])
  await user.click(
    screen.getByRole('button', { name: /premium.*gemini.*All resolutions/ })
  )
  await user.click(screen.getByRole('button', { name: 'Edit' }))
  const weight = screen.getByRole('spinbutton', { name: 'Weight 22' })
  await user.clear(weight)
  await user.type(weight, '19')
  await waitFor(() =>
    expect(screen.getByRole('button', { name: 'Save' })).toBeEnabled()
  )
  vi.mocked(imageRequest).mockRejectedValueOnce(new Error('Pool save failed'))

  await user.click(screen.getByRole('button', { name: 'Save' }))

  await waitFor(() =>
    expect(toast.error).toHaveBeenCalledWith('Pool save failed')
  )
  expect(
    screen.getByRole('dialog', { name: 'Edit channel pool binding' })
  ).toBeVisible()
  expect(weight).toHaveValue(19)
  expect(onSuccess).not.toHaveBeenCalled()
  expect(screen.getByRole('button', { name: 'Save' })).toBeEnabled()
  await user.click(screen.getByRole('button', { name: 'Save' }))
  await waitFor(() =>
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
  )
  expect(onSuccess).toHaveBeenCalledOnce()
})

test('a pending pool save blocks dismissal and duplicate submissions until it succeeds', async () => {
  const user = userEvent.setup()
  let finish: () => void = () => undefined
  const pending = new Promise<void>((resolve) => {
    finish = resolve
  })
  const { onSuccess } = renderPool([savedBinding])
  await user.click(
    screen.getByRole('button', { name: /premium.*gemini.*All resolutions/ })
  )
  await user.click(screen.getByRole('button', { name: 'Edit' }))
  const save = screen.getByRole('button', { name: 'Save' })
  await waitFor(() => expect(save).toBeEnabled())
  vi.mocked(imageRequest).mockImplementationOnce(() => pending)

  await user.click(save)

  await waitFor(() => expect(save).toBeDisabled())
  expect(save).toHaveAttribute('aria-busy', 'true')
  expect(screen.getByRole('button', { name: 'Cancel' })).toBeDisabled()
  await user.keyboard('{Escape}')
  expect(
    screen.getByRole('dialog', { name: 'Edit channel pool binding' })
  ).toBeVisible()
  await user.click(save)
  expect(
    vi.mocked(imageRequest).mock.calls.filter(([, method]) => method === 'PUT')
  ).toHaveLength(1)
  finish()
  await waitFor(() =>
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
  )
  expect(onSuccess).toHaveBeenCalledOnce()
})

test('copying a binding into another group replaces choices and preserves the source', async () => {
  vi.mocked(imageRequest).mockImplementation(async (url, method, data) => {
    if (method === 'GET' && url.includes('group=default')) return openAIOptions
    if (method === 'GET' && url.includes('group=premium')) return geminiOptions
    if (method === 'PUT' && url === '/api/option/images/pool') return data
    throw new Error(`Unexpected image request: ${method} ${url}`)
  })
  const user = userEvent.setup()
  renderPool([savedBinding])
  await user.click(
    screen.getByRole('button', { name: /premium.*gemini.*All resolutions/ })
  )
  await user.click(screen.getByRole('button', { name: 'Edit' }))
  await waitFor(() =>
    expect(screen.getByRole('combobox', { name: 'Model' })).toHaveValue(
      'Image Beta'
    )
  )
  const group = screen.getByRole('combobox', { name: 'Group' })
  expect(group).toBeDisabled()
  expect(screen.getByRole('combobox', { name: 'Platform' })).toBeDisabled()
  expect(screen.getByRole('combobox', { name: 'Pool binding' })).toBeDisabled()
  expect(screen.getByRole('combobox', { name: 'Model' })).toBeDisabled()
  await user.click(screen.getByRole('button', { name: 'Copy binding' }))
  expect(screen.getByRole('button', { name: 'Save' })).toBeDisabled()
  expect(screen.getByRole('alert')).toHaveTextContent(
    'A binding already exists for this target. Edit that binding instead.'
  )
  await user.click(group)
  await user.clear(group)
  await user.type(group, 'default')
  expect(await screen.findByRole('option', { name: 'default' })).toBeVisible()
  await user.keyboard('{ArrowDown}{Enter}')

  expect(group).toHaveValue('default')
  const model = screen.getByRole('combobox', { name: 'Model' })
  expect(model).toHaveValue('')
  expect(
    screen.queryByRole('spinbutton', { name: 'Weight 22' })
  ).not.toBeInTheDocument()
  expect(screen.getByRole('combobox', { name: 'Channels' })).toBeDisabled()
  await waitFor(() => expect(model).toBeEnabled())
  await user.click(model)
  expect(
    screen.queryByRole('option', { name: 'Image Beta' })
  ).not.toBeInTheDocument()
  await user.click(screen.getByRole('option', { name: 'Image Alpha' }))
  await user.click(screen.getByRole('combobox', { name: 'Channels' }))
  expect(
    screen.queryByRole('option', { name: '#22 · Gemini account' })
  ).not.toBeInTheDocument()
  await user.click(screen.getByRole('option', { name: '#11 · OpenAI primary' }))
  await user.keyboard('{Escape}')
  expect(screen.getByRole('spinbutton', { name: 'Weight 11' })).toHaveValue(0)
  await user.click(screen.getByRole('button', { name: 'Save' }))
  await waitFor(() =>
    expect(imageRequest).toHaveBeenCalledWith(
      '/api/option/images/pool',
      'PUT',
      {
        expected_ids: [],
        group: 'default',
        platform: 'gemini',
        mode: 'model',
        model: 'image-alpha',
        resolution: '',
        channels: [{ id: 11, priority: 0 }],
      }
    )
  )
})

test('reopening a closed binding submits the original placeholder row ID for concurrency protection', async () => {
  const user = userEvent.setup()
  renderPool([{ ...savedBinding, id: 50, channel_id: 0 }])
  await user.click(
    screen.getByRole('button', { name: /premium.*gemini.*All resolutions/ })
  )
  await user.click(screen.getByRole('button', { name: 'Edit' }))
  expect(screen.getByRole('status')).toHaveTextContent(
    'Saving an empty channel list closes this binding to new requests.'
  )
  const channels = screen.getByRole('combobox', { name: 'Channels' })
  await waitFor(() => expect(channels).toBeEnabled())
  await user.click(channels)
  await user.click(screen.getByRole('option', { name: '#22 · Gemini account' }))
  await user.keyboard('{Escape}')
  await user.click(screen.getByRole('button', { name: 'Save' }))
  await waitFor(() =>
    expect(imageRequest).toHaveBeenCalledWith(
      '/api/option/images/pool',
      'PUT',
      expect.objectContaining({
        expected_ids: [50],
        channels: [{ id: 22, priority: 0 }],
      })
    )
  )
})
