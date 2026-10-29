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
import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, expect, test, vi } from 'vitest'

import { getGroups } from '@/features/users/api'

import { imageRequest } from '../api'
import { ImageRoutingPreview } from '../components/image-routing-preview'

vi.mock('../api', () => ({ imageRequest: vi.fn() }))
vi.mock('@/features/users/api', () => ({ getGroups: vi.fn() }))
let client: QueryClient
beforeEach(() => {
  client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
  vi.mocked(getGroups).mockResolvedValue({ success: true, data: ['default'] })
})
afterEach(() => client.clear())

function renderPreview() {
  return render(
    <QueryClientProvider client={client}>
      <ImageRoutingPreview
        config={{
          runtime: {},
          media_providers: ['openai', 'xai'],
          storage_profiles: [],
          policies: [],
          pools: [],
          storage_providers: [],
        }}
      />
    </QueryClientProvider>
  )
}

test('previews eligible and excluded routes without generating media and clears stale results after input changes', async () => {
  const candidate = {
    group: 'default',
    binding_key: 'pool-1',
    binding_found: true,
    channel_id: 41,
    name: 'Primary',
    upstream_model: 'upstream-image',
    priority: 1,
    weight: 3,
    available_accounts: 2,
    reasons: [],
  }
  vi.mocked(imageRequest).mockResolvedValue({
    binding_key: 'pool-1',
    binding_found: true,
    routing_groups: ['default'],
    eligible: [candidate],
    excluded: [
      {
        ...candidate,
        channel_id: 42,
        name: 'Disabled',
        reasons: ['channel_disabled'],
      },
    ],
    reasons: [],
    warnings: ['configuration_preview_only'],
  })
  const user = userEvent.setup()
  renderPreview()
  await user.type(screen.getByRole('textbox', { name: 'Model' }), 'image-model')
  await user.type(
    screen.getByRole('spinbutton', { name: 'API key ID (optional)' }),
    '7'
  )
  await user.click(screen.getByRole('button', { name: 'Preview routing' }))
  expect(await screen.findByText('Channel is disabled')).toBeVisible()
  expect(screen.getByText('Eligible by configuration')).toBeVisible()
  expect(screen.getByText(/Live capacity, cooldowns/)).toBeVisible()
  expect(imageRequest).toHaveBeenCalledWith(
    '/api/option/images/routing/preview',
    'POST',
    {
      group: 'default',
      platform: 'openai',
      media_type: 'image',
      execution_mode: 'async',
      model: 'image-model',
      resolution: '1K',
      operation: 'text_to_image',
      token_id: 7,
      seconds: undefined,
    }
  )
  await user.clear(screen.getByRole('textbox', { name: 'Resolution' }))
  expect(screen.queryByRole('table')).not.toBeInTheDocument()
  expect(screen.getByRole('button', { name: 'Preview routing' })).toBeDisabled()
})

test('video preview uses provider defaults and shows policy errors even when no channels exist', async () => {
  vi.mocked(imageRequest).mockResolvedValue({
    binding_found: false,
    binding_key: '',
    eligible: [],
    excluded: [],
    reasons: ['policy_missing'],
    warnings: ['configuration_preview_only'],
  })
  const user = userEvent.setup()
  renderPreview()
  await user.click(screen.getByRole('combobox', { name: 'Media type' }))
  await user.click(screen.getByRole('option', { name: 'Video' }))
  await user.type(screen.getByRole('textbox', { name: 'Model' }), 'video-model')
  await user.click(screen.getByRole('button', { name: 'Preview routing' }))
  expect(await screen.findByRole('alert')).toHaveTextContent(
    'No media policy configured'
  )
  await waitFor(() =>
    expect(imageRequest).toHaveBeenCalledWith(
      '/api/option/images/routing/preview',
      'POST',
      expect.objectContaining({
        media_type: 'video',
        operation: 'text_to_video',
        resolution: '',
      })
    )
  )
})

test('switching from background to synchronous preview clears stale routes and explains incompatible protocols', async () => {
  vi.mocked(imageRequest).mockResolvedValue({
    binding_found: false,
    routing_groups: ['default'],
    eligible: [],
    excluded: [{
      channel_id: 43, group: 'default', name: 'Background only',
      upstream_model: 'image-model', priority: 0, weight: 1,
      available_accounts: 1, reasons: ['synchronous_protocol_not_supported'],
    }],
    reasons: [], warnings: [],
  })
  const user = userEvent.setup()
  renderPreview()
  await user.type(screen.getByRole('textbox', { name: 'Model' }), 'image-model')
  await user.click(screen.getByRole('button', { name: 'Preview routing' }))
  expect(await screen.findByRole('table')).toBeVisible()
  await user.click(screen.getByRole('combobox', { name: 'Execution mode' }))
  await user.click(screen.getByRole('option', { name: 'Synchronous image API' }))
  expect(screen.queryByRole('table')).not.toBeInTheDocument()
  await user.click(screen.getByRole('button', { name: 'Preview routing' }))
  expect(await screen.findByRole('cell', { name: 'Channel supports background tasks only or does not accept this synchronous API' })).toBeVisible()
  expect(imageRequest).toHaveBeenLastCalledWith('/api/option/images/routing/preview', 'POST', expect.objectContaining({ execution_mode: 'sync', model: 'image-model', resolution: '1K' }))
})

test('mixed preview explains missing bindings and incompatible image parameters in readable language', async () => {
  vi.mocked(imageRequest).mockResolvedValue({
    binding_found: false,
    binding_key: '',
    routing_groups: ['default'],
    eligible: [],
    excluded: [
      {
        group: 'default',
        channel_id: 45,
        name: 'Ark',
        upstream_model: 'seedream',
        provider: 'volcengine',
        protocol: 'openai_images',
        priority: 0,
        weight: 1,
        available_accounts: 1,
        reasons: ['image_parameters_not_supported'],
      },
    ],
    reasons: ['mixed_pool_missing'],
    warnings: [],
  })
  const user = userEvent.setup()
  renderPreview()
  await user.click(screen.getByRole('combobox', { name: 'Platform' }))
  await user.click(screen.getByRole('option', { name: 'Mixed scheduling' }))
  await user.type(screen.getByRole('textbox', { name: 'Model' }), 'seedream')
  await user.click(screen.getByRole('button', { name: 'Preview routing' }))

  expect(await screen.findByRole('alert')).toHaveTextContent(
    'No mixed channel pool binding is configured'
  )
  expect(
    screen.getByRole('cell', {
      name: 'Image parameters are not supported',
    })
  ).toBeVisible()
})

test('failed route preview exposes retry and does not retain previous candidates', async () => {
  vi.mocked(imageRequest)
    .mockRejectedValueOnce(new Error('Preview unavailable'))
    .mockResolvedValueOnce({
      binding_found: false,
      eligible: [],
      excluded: [],
      reasons: [],
      warnings: [],
    })
  const user = userEvent.setup()
  renderPreview()
  await user.type(screen.getByRole('textbox', { name: 'Model' }), 'image-model')
  await user.click(screen.getByRole('button', { name: 'Preview routing' }))
  expect(await screen.findByText('Preview unavailable')).toBeVisible()
  await user.click(screen.getByRole('button', { name: 'Retry' }))
  await waitFor(() =>
    expect(screen.queryByText('Preview unavailable')).not.toBeInTheDocument()
  )
  expect(await screen.findByRole('table')).toBeVisible()
})

test('mixed preview limits media to images and displays the actual provider and protocol', async () => {
  vi.mocked(imageRequest).mockResolvedValue({
    binding_found: true,
    binding_key: 'mixed-pool',
    eligible: [
      {
        channel_id: 45,
        name: 'Ark native',
        group: 'default',
        binding_key: 'mixed-pool',
        binding_found: true,
        upstream_model: 'doubao-seedream-5-0-pro-260628',
        provider: 'volcengine',
        protocol: 'openai_images',
        priority: 0,
        weight: 5,
        available_accounts: 1,
        reasons: [],
      },
    ],
    excluded: [],
    reasons: [],
    warnings: [],
  })
  const user = userEvent.setup()
  renderPreview()
  await user.click(screen.getByRole('combobox', { name: 'Media type' }))
  await user.click(screen.getByRole('option', { name: 'Video' }))
  await user.click(screen.getByRole('combobox', { name: 'Platform' }))
  await user.click(screen.getByRole('option', { name: 'Mixed scheduling' }))
  expect(screen.getByRole('combobox', { name: 'Media type' })).toBeDisabled()
  expect(
    screen.getByRole('combobox', { name: 'Media type' })
  ).toHaveTextContent('Image')
  await user.type(
    screen.getByRole('textbox', { name: 'Model' }),
    'seedream-5-pro'
  )
  await user.click(screen.getByRole('button', { name: 'Preview routing' }))
  expect(await screen.findByRole('cell', { name: 'volcengine' })).toBeVisible()
  expect(screen.getByRole('cell', { name: 'openai_images' })).toBeVisible()
  expect(imageRequest).toHaveBeenCalledWith(
    '/api/option/images/routing/preview',
    'POST',
    expect.objectContaining({
      platform: 'mixed',
      media_type: 'image',
      resolution: '1K',
      operation: 'text_to_image',
    })
  )
})

test('automatic routing keeps the same channel distinct across actual groups and pool bindings', async () => {
  vi.mocked(getGroups).mockResolvedValue({
    success: true,
    data: ['default', 'auto', 'premium'],
  })
  const candidate = {
    channel_id: 41,
    name: 'Shared provider',
    upstream_model: 'video-default',
    group: 'default',
    binding_key: 'default-pool',
    binding_found: true,
    priority: 1,
    weight: 3,
    available_accounts: 2,
    reasons: [],
  }
  vi.mocked(imageRequest).mockResolvedValue({
    group: 'auto',
    routing_groups: ['default', 'premium'],
    binding_key: '',
    binding_found: true,
    eligible: [
      candidate,
      {
        ...candidate,
        group: 'premium',
        upstream_model: 'video-premium',
        binding_key: 'premium-pool',
        weight: 8,
      },
    ],
    excluded: [],
    reasons: [],
    warnings: ['configuration_preview_only'],
  })
  const user = userEvent.setup()
  renderPreview()
  const group = screen.getByRole('combobox', { name: 'Group' })
  await user.clear(group)
  await user.type(group, 'auto')
  await user.click(await screen.findByRole('option', { name: 'auto' }))
  await user.click(screen.getByRole('combobox', { name: 'Media type' }))
  await user.click(screen.getByRole('option', { name: 'Video' }))
  await user.type(screen.getByRole('textbox', { name: 'Model' }), 'video-model')
  await user.type(
    screen.getByRole('spinbutton', { name: 'API key ID (optional)' }),
    '7'
  )
  await user.click(screen.getByRole('button', { name: 'Preview routing' }))
  const table = await screen.findByRole('table')
  expect(imageRequest).toHaveBeenCalledWith(
    '/api/option/images/routing/preview',
    'POST',
    expect.objectContaining({ group: 'auto', media_type: 'video', token_id: 7 })
  )
  expect(screen.getByText('Routing groups: default, premium')).toBeVisible()
  const rows = within(table).getAllByRole('row').slice(1)
  expect(rows).toHaveLength(2)
  expect(within(rows[0]).getByRole('cell', { name: 'default' })).toBeVisible()
  expect(
    within(rows[0]).getByRole('cell', { name: 'default-pool' })
  ).toBeVisible()
  expect(
    within(rows[0]).getByRole('cell', { name: 'video-default' })
  ).toBeVisible()
  expect(within(rows[1]).getByRole('cell', { name: 'premium' })).toBeVisible()
  expect(
    within(rows[1]).getByRole('cell', { name: 'premium-pool' })
  ).toBeVisible()
  expect(
    within(rows[1]).getByRole('cell', { name: 'video-premium' })
  ).toBeVisible()
})
