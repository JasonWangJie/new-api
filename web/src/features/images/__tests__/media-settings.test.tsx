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
import {
  QueryClient,
  QueryClientProvider,
  useQuery,
} from '@tanstack/react-query'
import { act, fireEvent, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { toast } from 'sonner'
import { afterEach, expect, test, vi } from 'vitest'

import { evaluateBillingExpression } from '@/features/pricing/lib/billing-expression/runtime'
import type { BillingUsageSchema } from '@/features/pricing/types'
import { getGroups } from '@/features/users/api'
import { api } from '@/lib/api'
import { useAuthStore } from '@/stores/auth-store'

import { imageRequest } from '../api'
import { MixedVideoPricing } from '../components/mixed-video-pricing'
import { MediaSettingsCard } from '../media-settings'
import { ImageSettings } from '../settings'
import type { ImageAdminConfiguration } from '../types'
import { VideoConfiguration } from '../video-settings'

vi.mock('../api', () => ({ imageRequest: vi.fn() }))
vi.mock('@/features/users/api', () => ({ getGroups: vi.fn() }))

const clients: QueryClient[] = []
const initialUser = useAuthStore.getState().auth.user
const mediaSettings = {
  revision: 0,
  video_async_enabled: false,
  local_path: 'data/media',
  retention_days: 90,
  signed_url_expiry_seconds: 3600,
  max_file_bytes: 1_073_741_824,
  download_timeout_seconds: 900,
  download_concurrency: 2,
  submit_concurrency: 2,
  status_concurrency: 8,
  submit_timeout_seconds: 120,
  poll_timeout_seconds: 30,
  reconciliation_timeout_minutes: 1440,
  reconciliation_poll_interval_seconds: 60,
  storage_retry_attempts: 5,
}

const imageRuntimeSettings = {
  failed_request_retention_hours: 24,
  account_cooldown_seconds: 30,
  supplier_retry_capacity: 10,
  supplier_retry_window_seconds: 60,
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
}

function MixedPolicyVersion() {
  const configuration = useQuery({
    queryKey: ['image-configuration', 1],
    queryFn: () => imageRequest<ImageAdminConfiguration>('/api/option/images'),
    enabled: false,
  })
  return (
    <output data-testid='mixed-policy-version'>
      {configuration.data?.policies[0]?.version}
    </output>
  )
}

test('loads the default OpenAI policy fields and preserves them on a direct save', async () => {
  const policy = {
    id: 75,
    group: 'default',
    platform: 'openai',
    enabled: true,
    async_enabled: false,
    pool_mode: 'model',
    models: JSON.stringify(
      [{ id: 'configured-image', media_type: 'image', resolutions: ['2K'] }],
      null,
      2
    ),
    version: 7,
  }
  let configuration: ImageAdminConfiguration = {
    runtime: imageRuntimeSettings,
    storage_profiles: [],
    pools: [],
    storage_providers: [],
    policies: [policy],
  }
  useAuthStore.getState().auth.setUser({
    id: 1,
    username: 'policy-config-admin',
    role: 20,
    permissions: {
      admin_permissions: { image_config: { read: true, manage: true } },
    },
  })
  vi.mocked(getGroups).mockResolvedValue({ success: true, data: ['default'] })
  vi.mocked(imageRequest).mockImplementation(async (url, method, body) => {
    if (url === '/api/option/images') return structuredClone(configuration)
    if (url === '/api/option/images/runtime-status') {
      return {
        node: 'test',
        effective: { running: false, active_workers: 0, worker_concurrency: 0 },
        configured: { worker_concurrency: 0, recovery_interval_seconds: 30 },
      }
    }
    if (url === '/api/option/images/policy' && method === 'PUT') {
      const saved = {
        ...(body as ImageAdminConfiguration['policies'][number]),
        version: 8,
      }
      configuration = { ...configuration, policies: [saved] }
      return saved
    }
    throw new Error(`Unexpected request: ${method} ${url}`)
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
  const user = userEvent.setup()
  await user.click(await screen.findByRole('tab', { name: 'Image policies' }))
  expect(
    screen.getByRole('textbox', { name: 'Model capabilities' })
  ).toHaveValue(policy.models)
  expect(
    screen.getByRole('combobox', { name: 'Pool binding' })
  ).toHaveTextContent('Model')
  expect(
    screen.getByRole('checkbox', { name: 'Enable image platform' })
  ).toBeChecked()
  expect(
    screen.getByRole('checkbox', { name: 'Allow background task APIs' })
  ).not.toBeChecked()
  await user.click(screen.getByRole('button', { name: 'Save' }))
  await waitFor(() =>
    expect(imageRequest).toHaveBeenCalledWith(
      '/api/option/images/policy',
      'PUT',
      policy
    )
  )
})

test.each(['image', 'video'] as const)(
  'mixed %s policy preserves its draft and write version through refetch, conflict and subsequent saves',
  async (editor) => {
    const image = { id: 'shared-image', media_type: 'image' }
    const video = {
      id: 'shared-video',
      media_type: 'video',
      video_overrides: {},
    }
    let server: ImageAdminConfiguration = {
      runtime: imageRuntimeSettings,
      storage_profiles: [],
      pools: [],
      storage_providers: [],
      policies: [
        {
          id: 91,
          group: 'default',
          platform: 'mixed',
          enabled: true,
          async_enabled: true,
          pool_mode: 'model_resolution',
          models: JSON.stringify([image, video]),
          version: 9,
        },
      ],
      video_plugin_templates: [
        { provider: 'xai', name: 'xAI', models: [], video_profiles: [] },
      ],
    }
    const writes: ImageAdminConfiguration['policies'][number][] = []
    let pendingWrite: Promise<void> | undefined
    useAuthStore.getState().auth.setUser({
      id: 1,
      username: 'policy-config-admin',
      role: 20,
      permissions: {
        admin_permissions: { image_config: { read: true, manage: true } },
      },
    })
    vi.mocked(getGroups).mockResolvedValue({ success: true, data: ['default'] })
    vi.mocked(imageRequest).mockImplementation(async (url, method, body) => {
      if (url === '/api/option/images') return structuredClone(server)
      if (url === '/api/option/images/media') return mediaSettings
      if (url === '/api/option/images/runtime-status') {
        return {
          node: 'test',
          effective: {
            running: false,
            active_workers: 0,
            worker_concurrency: 0,
          },
          configured: { worker_concurrency: 0, recovery_interval_seconds: 30 },
        }
      }
      if (url === '/api/option/images/policy' && method === 'PUT') {
        const input = body as ImageAdminConfiguration['policies'][number]
        writes.push(structuredClone(input))
        if (input.version !== server.policies[0].version) {
          throw Object.assign(new Error('stale policy'), {
            isAxiosError: true,
            response: { status: 409 },
          })
        }
        await pendingWrite
        const saved = { ...input, version: input.version + 1 }
        server = { ...server, policies: [saved] }
        return structuredClone(saved)
      }
      throw new Error(`Unexpected request: ${method} ${url}`)
    })
    const client = new QueryClient({
      defaultOptions: {
        queries: { retry: false },
        mutations: { retry: false },
      },
    })
    clients.push(client)
    render(
      <QueryClientProvider client={client}>
        <ImageSettings />
        <MixedPolicyVersion />
      </QueryClientProvider>
    )
    const user = userEvent.setup()
    await user.click(
      await screen.findByRole('tab', {
        name: editor === 'image' ? 'Image policies' : 'Video configuration',
      })
    )
    const selector = editor === 'image' ? 'Platform' : 'Provider'
    await user.click(screen.getByRole('combobox', { name: selector }))
    await user.click(screen.getByRole('option', { name: 'Mixed scheduling' }))
    const catalog = screen.getByRole('textbox', {
      name: editor === 'image' ? 'Model capabilities' : 'Video model catalog',
    })
    const own = editor === 'image' ? image : video
    const other = { ...(editor === 'image' ? video : image), label: 'New peer' }
    const draft = JSON.stringify([{ ...own, label: 'My draft' }])
    fireEvent.change(catalog, { target: { value: draft } })
    await act(async () => {
      await client.refetchQueries({ queryKey: ['image-configuration', 1] })
    })
    expect(catalog).toHaveValue(draft)
    server = {
      ...server,
      policies: [
        {
          ...server.policies[0],
          id: 92,
          enabled: false,
          pool_mode: 'resolution',
          models: JSON.stringify(
            editor === 'image' ? [own, other] : [other, own]
          ),
          version: 10,
        },
      ],
    }
    await act(async () => {
      await client.refetchQueries({ queryKey: ['image-configuration', 1] })
    })
    await waitFor(() =>
      expect(screen.getByTestId('mixed-policy-version')).toHaveTextContent('10')
    )
    expect(catalog).toHaveValue(draft)
    const save = screen.getByRole('button', {
      name: editor === 'image' ? 'Save' : 'Save video policy',
    })
    await user.click(save)
    await screen.findByRole('button', { name: 'Reload saved settings' })
    expect(writes[0]).toMatchObject({ id: 91, version: 9, enabled: true })
    expect(catalog).toHaveValue(draft)
    expect(save).toBeDisabled()
    await user.click(
      screen.getByRole('button', { name: 'Reload saved settings' })
    )
    await waitFor(() =>
      expect(catalog).toHaveValue(JSON.stringify([own], null, 2))
    )
    expect(save).toBeEnabled()
    expect(
      screen.getByRole('checkbox', {
        name:
          editor === 'image'
            ? 'Enable image platform'
            : 'Enable media platform',
      })
    ).not.toBeChecked()
    for (const [label, expectedVersion] of [
      ['After reload', 10],
      ['Second save', 11],
    ] as const) {
      const edited = { ...own, label }
      let finishWrite: (() => void) | undefined
      pendingWrite = new Promise<void>((resolve) => {
        finishWrite = resolve
      })
      fireEvent.change(catalog, { target: { value: JSON.stringify([edited]) } })
      await user.click(save)
      expect(catalog).toBeDisabled()
      expect(screen.getByRole('combobox', { name: selector })).toBeDisabled()
      expect(
        screen.getByRole('combobox', { name: 'Pool binding' })
      ).toBeDisabled()
      finishWrite?.()
      await waitFor(() =>
        expect(screen.getByTestId('mixed-policy-version')).toHaveTextContent(
          String(expectedVersion + 1)
        )
      )
      expect(writes.at(-1)).toMatchObject({
        id: 92,
        version: expectedVersion,
        enabled: false,
        async_enabled: true,
        pool_mode: 'resolution',
        models: JSON.stringify(
          editor === 'image' ? [edited, other] : [other, edited]
        ),
      })
    }
    expect(writes.map((write) => write.version)).toEqual([9, 10, 11])
    await user.click(screen.getByRole('combobox', { name: selector }))
    await user.click(
      screen.getByRole('option', {
        name: editor === 'image' ? 'openai' : 'xAI · xai',
      })
    )
    await user.click(screen.getByRole('combobox', { name: selector }))
    await user.click(screen.getByRole('option', { name: 'Mixed scheduling' }))
    expect(catalog).toHaveValue(
      JSON.stringify([{ ...own, label: 'Second save' }], null, 2)
    )
  }
)

test('preserves the video draft on a version conflict until saved settings are reloaded', async () => {
  const conflict = Object.assign(new Error('stale configuration'), {
    isAxiosError: true,
    response: { status: 409 },
  })
  vi.mocked(imageRequest)
    .mockResolvedValueOnce(mediaSettings)
    .mockRejectedValueOnce(conflict)
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
  const timeout = await screen.findByRole('spinbutton', {
    name: 'Video upstream polling timeout (seconds)',
  })
  await user.clear(timeout)
  await user.type(timeout, '45')
  await user.click(screen.getByRole('button', { name: 'Save' }))
  expect(await screen.findByRole('alert')).toHaveTextContent(
    'Your edits have been kept.'
  )
  expect(timeout).toHaveValue(45)
  vi.mocked(imageRequest).mockResolvedValueOnce({
    ...mediaSettings,
    revision: 4,
    poll_timeout_seconds: 60,
  })
  await user.click(
    screen.getByRole('button', { name: 'Reload saved settings' })
  )
  await waitFor(() =>
    expect(
      screen.getByRole('spinbutton', {
        name: 'Video upstream polling timeout (seconds)',
      })
    ).toHaveValue(60)
  )
  expect(screen.queryByRole('alert')).not.toBeInTheDocument()
})

afterEach(() => {
  for (const client of clients) client.clear()
  clients.length = 0
  useAuthStore.getState().auth.setUser(initialUser)
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
  const submitConcurrency = screen.getByRole('spinbutton', {
    name: 'Video submit concurrency',
  })
  const statusConcurrency = screen.getByRole('spinbutton', {
    name: 'Video local status sync concurrency',
  })
  const submitTimeout = screen.getByRole('spinbutton', {
    name: 'Video submit timeout (seconds)',
  })
  const pollTimeout = screen.getByRole('spinbutton', {
    name: 'Video upstream polling timeout (seconds)',
  })
  expect(pollTimeout).toHaveValue(30)
  expect(pollTimeout).toHaveAttribute('min', '1')
  expect(pollTimeout).toHaveAttribute('max', '300')
  await user.clear(submitConcurrency)
  await user.type(submitConcurrency, '3')
  await user.clear(statusConcurrency)
  await user.type(statusConcurrency, '9')
  await user.clear(submitTimeout)
  await user.type(submitTimeout, '140')
  await user.clear(pollTimeout)
  await user.click(screen.getByRole('button', { name: 'Save' }))
  expect(pollTimeout).toBeInvalid()
  expect(imageRequest).not.toHaveBeenCalledWith(
    '/api/option/images/media',
    'PUT',
    expect.anything()
  )
  await user.type(pollTimeout, '45')
  await user.click(screen.getByRole('button', { name: 'Save' }))

  await waitFor(() =>
    expect(imageRequest).toHaveBeenCalledWith(
      '/api/option/images/media',
      'PUT',
      {
        ...mediaSettings,
        local_path: 'data/video',
        submit_concurrency: 3,
        status_concurrency: 9,
        submit_timeout_seconds: 140,
        poll_timeout_seconds: 45,
      }
    )
  )
  expect(success).toHaveBeenCalledWith('Saved')
  expect(onSuccess).toHaveBeenCalledOnce()
})

test('uses a 30 second polling timeout when older settings omit it', async () => {
  vi.mocked(imageRequest).mockResolvedValue({
    ...mediaSettings,
    poll_timeout_seconds: undefined,
  })
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
  clients.push(client)
  render(
    <QueryClientProvider client={client}>
      <MediaSettingsCard />
    </QueryClientProvider>
  )
  expect(
    await screen.findByRole('spinbutton', {
      name: 'Video upstream polling timeout (seconds)',
    })
  ).toHaveValue(30)
  expect(
    screen.getByRole('spinbutton', { name: 'Video download timeout (seconds)' })
  ).toHaveValue(900)
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

test('mixed video configuration preserves images and saves separate public model prices', async () => {
  vi.mocked(imageRequest).mockImplementation(async (url, method, body) => {
    if (url === '/api/option/images/media') return mediaSettings
    if (method === 'PUT') return body
    throw new Error(`Unexpected request: ${method} ${url}`)
  })
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
  clients.push(client)
  const models = JSON.stringify([
    { id: 'shared-image', media_type: 'image' },
    { id: 'shared-video', media_type: 'video', video_overrides: {} },
  ])
  render(
    <QueryClientProvider client={client}>
      <VideoConfiguration
        config={{
          runtime: {},
          storage_profiles: [],
          pools: [],
          storage_providers: [],
          policies: [
            {
              group: 'default',
              platform: 'mixed',
              enabled: true,
              async_enabled: true,
              pool_mode: 'model_resolution',
              models,
              version: 4,
            },
          ],
          mixed_video_billing_expr: { 'shared-video': '1' },
          video_plugin_templates: [
            {
              provider: 'drama',
              name: 'Drama',
              models: [],
              video_profiles: [],
            },
          ],
        }}
        onSuccess={vi.fn().mockResolvedValue(undefined)}
      />
    </QueryClientProvider>
  )
  const user = userEvent.setup()
  await user.click(screen.getByRole('combobox', { name: 'Provider' }))
  await user.click(screen.getByRole('option', { name: 'Mixed scheduling' }))
  expect(
    screen.getByRole('textbox', {
      name: 'Video model catalog',
    })
  ).toHaveValue(
    JSON.stringify(
      [{ id: 'shared-video', media_type: 'video', video_overrides: {} }],
      null,
      2
    )
  )
  await user.click(screen.getByRole('button', { name: 'Save video policy' }))
  await waitFor(() =>
    expect(imageRequest).toHaveBeenCalledWith(
      '/api/option/images/policy',
      'PUT',
      expect.objectContaining({ platform: 'mixed', models, version: 4 })
    )
  )
  const prices = screen.getByRole('textbox', {
    name: 'Public video model billing expressions',
  })
  expect(prices).toHaveValue(JSON.stringify({ 'shared-video': '1' }, null, 2))
  fireEvent.input(prices, { target: { value: '{"shared-video":"2"}' } })
  await user.click(screen.getByRole('combobox', { name: 'Provider' }))
  await user.click(screen.getByRole('option', { name: 'Drama · drama' }))
  await user.click(screen.getByRole('combobox', { name: 'Provider' }))
  await user.click(screen.getByRole('option', { name: 'Mixed scheduling' }))
  expect(
    screen.getByRole('textbox', {
      name: 'Public video model billing expressions',
    })
  ).toHaveValue('{"shared-video":"2"}')
  await user.click(
    screen.getByRole('button', { name: 'Save mixed video prices' })
  )
  await waitFor(() =>
    expect(imageRequest).toHaveBeenCalledWith('/api/option/', 'PUT', {
      key: 'billing_setting.mixed_video_billing_expr',
      value: '{"shared-video":"2"}',
    })
  )
})

const mixedVideoPricingSchema: BillingUsageSchema = {
  requests: {
    type: 'number',
    unit: 'count',
    description: 'Video operation unit price',
  },
  seconds: {
    type: 'number',
    unit: 'second',
    description: 'Video duration unit price',
  },
  resolution: {
    enum: ['720p', '1080p'],
    description: 'Output video resolution',
  },
  action: { enum: ['generate', 'edit', 'extend'] },
  video_input: {
    enum: ['none', 'video'],
    description: 'Reference video input',
    enumLabels: { none: 'No reference video', video: 'With reference video' },
  },
}

test('mixed pricing inherits model prices until an explicit override is saved and can return to inheritance', async () => {
  const expression =
    'tier("seconds", u("seconds") * 0.8) * (u("video_input") == "video" ? 1.45 : 1)'
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  clients.push(client)
  vi.mocked(imageRequest).mockResolvedValue({})
  render(
    <QueryClientProvider client={client}>
      <MixedVideoPricing
        config={{
          runtime: {},
          storage_profiles: [],
          pools: [],
          storage_providers: [],
          policies: [],
          mixed_video_usage_schema: mixedVideoPricingSchema,
          mixed_video_model_billing_expr: { 'shared-video': expression },
          mixed_video_billing_expr: {
            other: 'tier("other", u("requests") * 2)',
          },
        }}
        catalog='[{"id":"shared-video","media_type":"video"}]'
        onSuccess={vi.fn().mockResolvedValue(undefined)}
      />
    </QueryClientProvider>
  )
  const user = userEvent.setup()
  await user.click(screen.getByRole('combobox', { name: 'Public video model' }))
  await user.click(screen.getByRole('option', { name: 'shared-video' }))
  expect(
    screen.getByRole('combobox', { name: 'Mixed video price source' })
  ).toHaveTextContent('Use model pricing')
  expect(
    screen.getByRole('textbox', { name: 'With reference video multiplier' })
  ).toBeDisabled()
  expect(
    screen.getByRole('link', { name: 'Open model pricing' })
  ).toHaveAttribute('href', '/system-settings/billing/model-pricing')
  expect(imageRequest).not.toHaveBeenCalled()
  await user.click(
    screen.getByRole('combobox', { name: 'Mixed video price source' })
  )
  await user.click(
    screen.getByRole('option', { name: 'Custom mixed video price' })
  )
  const multiplier = screen.getByRole('textbox', {
    name: 'With reference video multiplier',
  })
  expect(multiplier).toBeEnabled()
  expect(multiplier).toHaveValue('1.45')
  await user.clear(multiplier)
  await user.type(multiplier, '1.8')
  await user.click(
    screen.getByRole('button', { name: 'Save mixed video prices' })
  )
  await waitFor(() => expect(imageRequest).toHaveBeenCalled())
  const saved = vi.mocked(imageRequest).mock.calls.at(-1)?.[2] as {
    value: string
  }
  const prices = JSON.parse(saved.value) as Record<string, string>
  expect(prices.other).toBe('tier("other", u("requests") * 2)')
  const cost = evaluateBillingExpression(prices['shared-video'], {
    usage: { seconds: 30, video_input: 'video' },
  })
  expect(cost.status).toBe('success')
  if (cost.status === 'success') expect(cost.cost).toBeCloseTo(43.2)
  await user.click(
    screen.getByRole('combobox', { name: 'Mixed video price source' })
  )
  await user.click(screen.getByRole('option', { name: 'Use model pricing' }))
  expect(
    screen.getByRole('textbox', { name: 'With reference video multiplier' })
  ).toHaveValue('1.45')
  await user.click(
    screen.getByRole('button', { name: 'Save mixed video prices' })
  )
  await waitFor(() =>
    expect(imageRequest).toHaveBeenLastCalledWith('/api/option/', 'PUT', {
      key: 'billing_setting.mixed_video_billing_expr',
      value: JSON.stringify({ other: 'tier("other", u("requests") * 2)' }),
    })
  )
})

test('inherited mixed pricing reflects refreshed model prices without saving an override', async () => {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  clients.push(client)
  const config: ImageAdminConfiguration = {
    runtime: {},
    storage_profiles: [],
    pools: [],
    storage_providers: [],
    policies: [],
    mixed_video_usage_schema: mixedVideoPricingSchema,
    mixed_video_billing_expr: {},
    mixed_video_model_billing_expr: {
      'shared-video':
        'tier("s", u("seconds") * 0.8) * (u("video_input") == "video" ? 1.45 : 1)',
    },
  }
  const pricing = (current: ImageAdminConfiguration) => (
    <QueryClientProvider client={client}>
      <MixedVideoPricing
        config={current}
        catalog='[{"id":"shared-video","media_type":"video"}]'
        onSuccess={vi.fn().mockResolvedValue(undefined)}
      />
    </QueryClientProvider>
  )
  vi.mocked(imageRequest).mockResolvedValue({})
  const view = render(pricing(config))
  view.rerender(
    pricing({
      ...config,
      mixed_video_model_billing_expr: {
        'shared-video':
          'tier("s", u("seconds") * 0.8) * (u("video_input") == "video" ? 1.8 : 1)',
      },
    })
  )
  await waitFor(() =>
    expect(
      screen.getByRole('textbox', { name: 'With reference video multiplier' })
    ).toHaveValue('1.8')
  )
  expect(
    screen.getByRole('textbox', { name: 'With reference video multiplier' })
  ).toBeDisabled()
  expect(imageRequest).not.toHaveBeenCalled()
  const user = userEvent.setup()
  await user.click(
    screen.getByRole('button', { name: 'Save mixed video prices' })
  )
  await waitFor(() =>
    expect(imageRequest).toHaveBeenCalledWith('/api/option/', 'PUT', {
      key: 'billing_setting.mixed_video_billing_expr',
      value: '{}',
    })
  )
})

test('mixed pricing keeps its draft and reports a refresh failure after saving through media settings', async () => {
  const configuration: ImageAdminConfiguration = {
    runtime: imageRuntimeSettings,
    storage_profiles: [],
    pools: [],
    storage_providers: [],
    policies: [
      {
        id: 1,
        group: 'default',
        platform: 'mixed',
        enabled: true,
        async_enabled: true,
        pool_mode: 'model',
        models: '[{"id":"shared-video","media_type":"video"}]',
        version: 1,
      },
    ],
    mixed_video_usage_schema: mixedVideoPricingSchema,
    mixed_video_model_billing_expr: {
      'shared-video': 'tier("s", u("seconds") * 0.8)',
    },
    mixed_video_billing_expr: {
      'shared-video': 'tier("s", u("seconds") * 0.8) * 1.45',
    },
  }
  useAuthStore
    .getState()
    .auth.setUser({ id: 1, username: 'pricing-admin', role: 100 })
  let refreshFailed = false
  vi.mocked(imageRequest).mockImplementation(async (url, method, body) => {
    if (url === '/api/option/images') {
      if (refreshFailed) throw new Error('Configuration refresh failed')
      return structuredClone(configuration)
    }
    if (url === '/api/option/images/media') return mediaSettings
    if (url === '/api/option/images/runtime-status') {
      return {
        node: 'test',
        effective: { running: false, active_workers: 0, worker_concurrency: 0 },
        configured: { worker_concurrency: 0, recovery_interval_seconds: 30 },
      }
    }
    if (url === '/api/option/' && method === 'PUT') {
      configuration.mixed_video_billing_expr = JSON.parse(
        (body as { value: string }).value
      )
      return {}
    }
    throw new Error(`Unexpected request: ${method} ${url}`)
  })
  const errorToast = vi.spyOn(toast, 'error').mockReturnValue('refresh-error')
  const successToast = vi.spyOn(toast, 'success').mockReturnValue('saved')
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  clients.push(client)
  render(
    <QueryClientProvider client={client}>
      <ImageSettings />
    </QueryClientProvider>
  )
  const user = userEvent.setup()
  await user.click(
    await screen.findByRole('tab', { name: 'Video configuration' })
  )
  await user.click(screen.getByRole('combobox', { name: 'Provider' }))
  await user.click(screen.getByRole('option', { name: 'Mixed scheduling' }))
  await user.click(
    screen.getByRole('combobox', { name: 'Mixed video price source' })
  )
  await user.click(screen.getByRole('option', { name: 'Use model pricing' }))
  refreshFailed = true
  await user.click(
    screen.getByRole('button', { name: 'Save mixed video prices' })
  )
  await waitFor(() =>
    expect(errorToast).toHaveBeenCalledWith('Configuration refresh failed')
  )
  expect(successToast).not.toHaveBeenCalled()
  expect(
    screen.getByRole('combobox', { name: 'Mixed video price source' })
  ).toHaveTextContent('Use model pricing')
  expect(configuration.mixed_video_billing_expr).toEqual({})
  refreshFailed = false
  await user.click(
    screen.getByRole('button', { name: 'Save mixed video prices' })
  )
  await waitFor(() => expect(successToast).toHaveBeenCalledWith('Saved'))
})

test.each([undefined, 'tier("tokens", u("tokens") * 0.1)'])(
  'inherited missing or incompatible model price is shown as needing configuration: %s',
  (expression) => {
    const client = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    })
    clients.push(client)
    render(
      <QueryClientProvider client={client}>
        <MixedVideoPricing
          config={{
            runtime: {},
            storage_profiles: [],
            pools: [],
            storage_providers: [],
            policies: [],
            mixed_video_usage_schema: mixedVideoPricingSchema,
            mixed_video_model_billing_expr: expression
              ? { 'shared-video': expression }
              : {},
            mixed_video_billing_expr: {},
          }}
          catalog='[{"id":"shared-video","media_type":"video"}]'
          onSuccess={vi.fn().mockResolvedValue(undefined)}
        />
      </QueryClientProvider>
    )
    expect(
      screen.getByText(
        'Configure a compatible model billing expression or set a custom mixed video price.'
      )
    ).toBeInTheDocument()
    expect(
      screen.getByRole('combobox', { name: 'Mixed video price source' })
    ).toHaveTextContent('Use model pricing')
    expect(imageRequest).not.toHaveBeenCalled()
  }
)

test('mixed visual pricing edits reference multipliers once and retains the draft across configuration refreshes', async () => {
  const expression =
    '(u("resolution") == "720p" ? tier("720p", u("requests") * 6) : tier("1080p", u("requests") * 9)) * (u("video_input") == "video" ? 1.5 : 1.25)'
  const config: ImageAdminConfiguration = {
    runtime: {},
    storage_profiles: [],
    pools: [],
    storage_providers: [],
    policies: [],
    mixed_video_usage_schema: mixedVideoPricingSchema,
    mixed_video_billing_expr: {
      'shared-video': expression,
      'custom-video': 'max(1, u("seconds"))',
    },
  }
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  clients.push(client)
  const renderPricing = (current: ImageAdminConfiguration) => (
    <QueryClientProvider client={client}>
      <MixedVideoPricing
        config={current}
        catalog='[]'
        onSuccess={vi.fn().mockResolvedValue(undefined)}
      />
    </QueryClientProvider>
  )
  vi.mocked(imageRequest).mockResolvedValue({})
  const view = render(renderPricing(config))
  const user = userEvent.setup()
  expect(
    screen.getByRole('textbox', { name: 'Without reference video multiplier' })
  ).toHaveValue('1.25')
  expect(screen.queryByText('Request rule pricing')).not.toBeInTheDocument()
  expect(imageRequest).not.toHaveBeenCalled()
  const multiplier = screen.getByRole('textbox', {
    name: 'With reference video multiplier',
  })
  await user.clear(multiplier)
  await user.type(multiplier, '1.8')
  view.rerender(
    renderPricing({
      ...config,
      mixed_video_billing_expr: { 'shared-video': '1' },
    })
  )
  expect(multiplier).toHaveValue('1.8')
  await user.click(
    screen.getByRole('button', { name: 'Save mixed video prices' })
  )
  await waitFor(() =>
    expect(imageRequest).toHaveBeenCalledWith(
      '/api/option/',
      'PUT',
      expect.objectContaining({
        key: 'billing_setting.mixed_video_billing_expr',
      })
    )
  )
  const saved = vi
    .mocked(imageRequest)
    .mock.calls.find(([url]) => url === '/api/option/')?.[2] as {
    value: string
  }
  const prices = JSON.parse(saved.value) as Record<string, string>
  expect(prices['custom-video']).toBe('max(1, u("seconds"))')
  expect(prices['shared-video']).not.toContain('u("seconds")')
  for (const [resolution, video_input, cost] of [
    ['720p', 'none', 7.5],
    ['720p', 'video', 10.8],
    ['1080p', 'none', 11.25],
    ['1080p', 'video', 16.2],
  ] as const) {
    const result = evaluateBillingExpression(prices['shared-video'], {
      usage: { requests: 1, resolution, video_input },
    })
    expect(result.status).toBe('success')
    if (result.status === 'success') expect(result.cost).toBeCloseTo(cost)
  }
})

test.each([
  '  tier("custom", ceil(u("seconds") / 5) * 2)  ',
  'tier("s", u("seconds") * 0.6) * (header("x-priority") == "high" ? 2 : 1)',
  'tier("s", u("seconds") * (header("x-priority") == "high" ? 2 : 1))',
  'tier("s", u("seconds") * (param("priority") == "high" ? 2 : 1))',
])(
  'mixed custom expression remains raw and intact when opened and saved: %s',
  async (expression) => {
    const client = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    })
    clients.push(client)
    vi.mocked(imageRequest).mockResolvedValue({})
    render(
      <QueryClientProvider client={client}>
        <MixedVideoPricing
          config={{
            runtime: {},
            storage_profiles: [],
            pools: [],
            storage_providers: [],
            policies: [],
            mixed_video_usage_schema: mixedVideoPricingSchema,
            mixed_video_billing_expr: { custom: expression },
          }}
          catalog='[]'
          onSuccess={vi.fn().mockResolvedValue(undefined)}
        />
      </QueryClientProvider>
    )
    expect(
      screen.getByRole('textbox', { name: 'Billing expression' })
    ).toHaveValue(expression)
    expect(
      screen.queryByRole('textbox', { name: 'With reference video multiplier' })
    ).not.toBeInTheDocument()
    const user = userEvent.setup()
    await user.click(screen.getByRole('combobox', { name: 'Editor mode' }))
    await user.click(screen.getByRole('option', { name: 'Visual editor' }))
    expect(screen.getByRole('alertdialog')).toHaveTextContent(
      'Switching discards the entire expression'
    )
    await user.click(screen.getByRole('button', { name: 'Cancel' }))
    expect(
      screen.getByRole('textbox', { name: 'Billing expression' })
    ).toHaveValue(expression)
    await user.click(
      screen.getByRole('button', { name: 'Save mixed video prices' })
    )
    await waitFor(() =>
      expect(imageRequest).toHaveBeenCalledWith('/api/option/', 'PUT', {
        key: 'billing_setting.mixed_video_billing_expr',
        value: JSON.stringify({ custom: expression }),
      })
    )
  }
)

test('mixed pricing copies only the selected complete provider expression without saving or multiplying the existing price', async () => {
  useAuthStore
    .getState()
    .auth.setUser({ id: 1, username: 'pricing-admin', role: 100 })
  const copied =
    'tier("seconds", u("seconds") * 0.6) * (u("video_input") == "video" ? 1.65 : 1.45)'
  const load = vi.spyOn(api, 'get').mockResolvedValue({
    data: {
      success: true,
      data: {
        entries: [
          {
            model_name: 'seedance2.0-A',
            version: 'v1',
            configured: {},
            effective: {
              'billing_setting.billing_mode': 'tiered_expr',
              'billing_setting.billing_expr':
                'tier("default", u("seconds") * 1)',
            },
            plugin_variants: [
              {
                plugin_key: 'drama',
                plugin_name: 'Drama',
                configured: copied,
                effective: copied,
                compatible: true,
                usage_schema: mixedVideoPricingSchema,
              },
              {
                plugin_key: 'request-rules',
                plugin_name: 'Request rules',
                configured: '',
                effective:
                  'tier("s", u("seconds") * 0.6) * (header("x-priority") == "high" ? 2 : 1)',
                compatible: true,
                usage_schema: mixedVideoPricingSchema,
              },
              ...[
                {
                  plugin_key: 'nested-header',
                  plugin_name: 'Nested header',
                  effective:
                    'tier("s", u("seconds") * (header("x-priority") == "high" ? 2 : 1))',
                },
                {
                  plugin_key: 'nested-param',
                  plugin_name: 'Nested parameter',
                  effective:
                    'tier("s", u("seconds") * (param("priority") == "high" ? 2 : 1))',
                },
                {
                  plugin_key: 'token-input',
                  plugin_name: 'Token input',
                  effective: 'tier("s", p * 0.6)',
                },
                {
                  plugin_key: 'dynamic-usage',
                  plugin_name: 'Dynamic usage key',
                  effective: 'tier("s", u("sec" + "onds") * 0.6)',
                },
              ].map((variant) => ({
                ...variant,
                configured: '',
                compatible: true,
                usage_schema: mixedVideoPricingSchema,
              })),
            ],
          },
        ],
        options: {},
        empty_version: 'empty',
      },
    },
  })
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  clients.push(client)
  render(
    <QueryClientProvider client={client}>
      <MixedVideoPricing
        config={{
          runtime: {},
          storage_profiles: [],
          pools: [],
          storage_providers: [],
          policies: [],
          mixed_video_usage_schema: mixedVideoPricingSchema,
          mixed_video_billing_expr: {
            'seedance2.0-A': 'tier("old", u("seconds") * 0.6) * 1.45',
          },
        }}
        catalog='[]'
        onSuccess={vi.fn().mockResolvedValue(undefined)}
      />
    </QueryClientProvider>
  )
  const user = userEvent.setup()
  expect(load).not.toHaveBeenCalled()
  await user.click(screen.getByRole('button', { name: 'Load existing prices' }))
  await user.click(
    await screen.findByRole('combobox', { name: 'Price source' })
  )
  expect(
    screen.queryByRole('option', { name: 'Request rules' })
  ).not.toBeInTheDocument()
  for (const name of [
    'Nested header',
    'Nested parameter',
    'Token input',
    'Dynamic usage key',
  ]) {
    expect(screen.queryByRole('option', { name })).not.toBeInTheDocument()
  }
  await user.click(screen.getByRole('option', { name: 'Drama' }))
  await user.click(screen.getByRole('button', { name: 'Copy existing price' }))
  expect(
    screen.getByRole('textbox', { name: 'Without reference video multiplier' })
  ).toHaveValue('1.45')
  expect(
    screen.getByRole('textbox', { name: 'With reference video multiplier' })
  ).toHaveValue('1.65')
  expect(imageRequest).not.toHaveBeenCalled()
  await user.click(
    screen.getByRole('button', {
      name: 'Public video model billing expressions',
    })
  )
  expect(
    screen.getByRole('textbox', {
      name: 'Public video model billing expressions',
    })
  ).toHaveValue(JSON.stringify({ 'seedance2.0-A': copied }, null, 2))
})

test.each(['selecting the mixed platform', 'replacing the image catalog'])(
  '%s preserves mixed videos when saving the image policy',
  async (trigger) => {
    const image = { id: 'shared-image', media_type: 'image' }
    const video = {
      id: 'shared-video',
      media_type: 'video',
      video_overrides: {},
    }
    const replacement = { id: 'replacement-image', media_type: 'image' }
    const config = {
      runtime: imageRuntimeSettings,
      storage_profiles: [],
      pools: [],
      storage_providers: [],
      policies: [
        {
          group: 'default',
          platform: 'mixed',
          enabled: true,
          async_enabled: true,
          pool_mode: 'model_resolution',
          models: JSON.stringify([image, video]),
          version: 7,
        },
      ],
    }
    useAuthStore.getState().auth.setUser({
      id: 1,
      username: 'image-config-admin',
      role: 20,
      permissions: {
        admin_permissions: { image_config: { read: true, manage: true } },
      },
    })
    vi.mocked(getGroups).mockResolvedValue({ success: true, data: ['default'] })
    vi.mocked(imageRequest).mockImplementation(async (url, method, body) => {
      if (url === '/api/option/images') return config
      if (url === '/api/option/images/runtime-status') {
        return {
          node: 'test',
          effective: {
            running: false,
            active_workers: 0,
            worker_concurrency: 0,
          },
          configured: { worker_concurrency: 0, recovery_interval_seconds: 30 },
        }
      }
      if (url === '/api/option/images/policy' && method === 'PUT') return body
      throw new Error(`Unexpected request: ${method} ${url}`)
    })
    const client = new QueryClient({
      defaultOptions: {
        queries: { retry: false },
        mutations: { retry: false },
      },
    })
    clients.push(client)
    render(
      <QueryClientProvider client={client}>
        <ImageSettings />
      </QueryClientProvider>
    )
    const user = userEvent.setup()
    await user.click(await screen.findByRole('tab', { name: 'Image policies' }))
    if (trigger === 'selecting the mixed platform') {
      await user.click(screen.getByRole('combobox', { name: 'Platform' }))
      await user.click(screen.getByRole('option', { name: 'Mixed scheduling' }))
      expect(
        JSON.parse(
          (
            screen.getByRole('textbox', {
              name: 'Model capabilities',
            }) as HTMLTextAreaElement
          ).value
        )
      ).toEqual([image])
    } else {
      await user.click(screen.getByRole('button', { name: 'Edit' }))
      fireEvent.change(
        screen.getByRole('textbox', { name: 'Model capabilities' }),
        { target: { value: JSON.stringify([replacement]) } }
      )
    }
    await user.click(screen.getByRole('button', { name: 'Save' }))
    await waitFor(() =>
      expect(imageRequest).toHaveBeenCalledWith(
        '/api/option/images/policy',
        'PUT',
        expect.objectContaining({
          platform: 'mixed',
          pool_mode: 'model_resolution',
          enabled: true,
          version: 7,
          models: JSON.stringify([
            trigger === 'selecting the mixed platform' ? image : replacement,
            video,
          ]),
        })
      )
    )
  }
)

test('pasting a video-only catalog preserves saved mixed images', async () => {
  const image = { id: 'shared-image', media_type: 'image' }
  const video = {
    id: 'replacement-video',
    media_type: 'video',
    video_overrides: {},
  }
  vi.mocked(imageRequest).mockImplementation(async (url, method, body) => {
    if (url === '/api/option/images/media') return mediaSettings
    if (method === 'PUT') return body
    throw new Error(`Unexpected request: ${method} ${url}`)
  })
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
  clients.push(client)
  render(
    <QueryClientProvider client={client}>
      <VideoConfiguration
        config={{
          runtime: {},
          storage_profiles: [],
          pools: [],
          storage_providers: [],
          policies: [
            {
              group: 'default',
              platform: 'mixed',
              enabled: true,
              async_enabled: true,
              pool_mode: 'model_resolution',
              models: JSON.stringify([image]),
              version: 8,
            },
          ],
        }}
        onSuccess={vi.fn().mockResolvedValue(undefined)}
      />
    </QueryClientProvider>
  )
  const user = userEvent.setup()
  await user.click(screen.getByRole('combobox', { name: 'Provider' }))
  await user.click(screen.getByRole('option', { name: 'Mixed scheduling' }))
  fireEvent.change(
    screen.getByRole('textbox', { name: 'Video model catalog' }),
    { target: { value: JSON.stringify([video]) } }
  )
  await user.click(screen.getByRole('button', { name: 'Save video policy' }))
  await waitFor(() =>
    expect(imageRequest).toHaveBeenCalledWith(
      '/api/option/images/policy',
      'PUT',
      expect.objectContaining({
        platform: 'mixed',
        models: JSON.stringify([image, video]),
        version: 8,
      })
    )
  )
})

test.each([
  { editor: 'image', type: ' IMAGE ', valid: true },
  { editor: 'video', type: ' VIDEO ', valid: true },
  { editor: 'image', type: 'unknown-media', valid: false },
  { editor: 'video', type: 'unknown-media', valid: false },
  { editor: 'image', type: 7, valid: false },
  { editor: 'video', type: 7, valid: false },
])(
  'mixed $editor catalog validates and normalizes media type $type',
  async (testCase) => {
    const image = { id: 'shared-image', media_type: 'image' }
    const video = {
      id: 'shared-video',
      media_type: 'video',
      video_overrides: {},
    }
    const config = {
      runtime: imageRuntimeSettings,
      storage_profiles: [],
      pools: [],
      storage_providers: [],
      policies: [
        {
          group: 'default',
          platform: 'mixed',
          enabled: true,
          async_enabled: true,
          pool_mode: 'model_resolution',
          models: JSON.stringify([image, video]),
          version: 9,
        },
      ],
    }
    useAuthStore.getState().auth.setUser({
      id: 1,
      username: 'media-config-admin',
      role: 20,
      permissions: {
        admin_permissions: { image_config: { read: true, manage: true } },
      },
    })
    vi.mocked(getGroups).mockResolvedValue({ success: true, data: ['default'] })
    vi.mocked(imageRequest).mockImplementation(async (url, method, body) => {
      if (url === '/api/option/images') return config
      if (url === '/api/option/images/media') return mediaSettings
      if (url === '/api/option/images/runtime-status') {
        return {
          node: 'test',
          effective: {
            running: false,
            active_workers: 0,
            worker_concurrency: 0,
          },
          configured: { worker_concurrency: 0, recovery_interval_seconds: 30 },
        }
      }
      if (url === '/api/option/images/policy' && method === 'PUT') return body
      throw new Error(`Unexpected request: ${method} ${url}`)
    })
    const client = new QueryClient({
      defaultOptions: {
        queries: { retry: false },
        mutations: { retry: false },
      },
    })
    clients.push(client)
    render(
      <QueryClientProvider client={client}>
        {testCase.editor === 'image' ? (
          <ImageSettings />
        ) : (
          <VideoConfiguration
            config={config}
            onSuccess={vi.fn().mockResolvedValue(undefined)}
          />
        )}
      </QueryClientProvider>
    )
    const user = userEvent.setup()
    if (testCase.editor === 'image') {
      await user.click(
        await screen.findByRole('tab', { name: 'Image policies' })
      )
    }
    await user.click(
      screen.getByRole('combobox', {
        name: testCase.editor === 'image' ? 'Platform' : 'Provider',
      })
    )
    await user.click(screen.getByRole('option', { name: 'Mixed scheduling' }))
    const catalog = screen.getByRole('textbox', {
      name:
        testCase.editor === 'image'
          ? 'Model capabilities'
          : 'Video model catalog',
    })
    const draft = JSON.stringify([
      {
        ...(testCase.editor === 'image' ? image : video),
        media_type: testCase.type,
      },
    ])
    fireEvent.change(catalog, { target: { value: draft } })
    const errorToast = vi
      .spyOn(toast, 'error')
      .mockReturnValue('invalid-media-type')
    await user.click(
      screen.getByRole('button', {
        name: testCase.editor === 'image' ? 'Save' : 'Save video policy',
      })
    )
    if (testCase.valid) {
      await waitFor(() =>
        expect(imageRequest).toHaveBeenCalledWith(
          '/api/option/images/policy',
          'PUT',
          expect.objectContaining({
            platform: 'mixed',
            version: 9,
            models: JSON.stringify([image, video]),
          })
        )
      )
    } else {
      await waitFor(() =>
        expect(errorToast).toHaveBeenCalledWith(
          'Model capabilities must be JSON objects with a valid image or video media type.'
        )
      )
      expect(
        vi
          .mocked(imageRequest)
          .mock.calls.filter(
            ([url, method]) =>
              url === '/api/option/images/policy' && method === 'PUT'
          )
      ).toHaveLength(0)
      expect(catalog).toHaveValue(draft)
    }
  }
)
