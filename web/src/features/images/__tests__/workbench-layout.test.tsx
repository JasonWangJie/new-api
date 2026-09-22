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
import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, expect, test, vi } from 'vitest'

import { useAuthStore } from '@/stores/auth-store'

import { VideoCapabilityForm } from '../components/video-capability-form'
import type { VideoModel } from '../types'
import { VideoWorkbench } from '../video-workbench'
import { ImageWorkbench } from '../workbench'

const keyApiMocks = vi.hoisted(() => ({
  fetchTokenKey: vi.fn(),
  getApiKeys: vi.fn(),
}))

const imageApiMocks = vi.hoisted(() => ({
  getImageCapabilities: vi.fn(),
  getImageTask: vi.fn(),
  imageBlob: vi.fn(),
  imageRelayRequest: vi.fn(),
  imageRequest: vi.fn(),
}))

vi.mock('@/features/keys/api', () => keyApiMocks)
vi.mock('../api', () => imageApiMocks)

let client: QueryClient

beforeEach(() => {
  client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  useAuthStore
    .getState()
    .auth.setUser({ id: 101, username: 'creator', role: 1 })
  sessionStorage.clear()
  keyApiMocks.fetchTokenKey.mockReset()
  keyApiMocks.getApiKeys.mockReset()
  keyApiMocks.getApiKeys.mockResolvedValue({ data: { items: [] } })
  imageApiMocks.getImageCapabilities.mockReset()
  imageApiMocks.getImageTask.mockReset()
  imageApiMocks.imageBlob.mockReset()
  imageApiMocks.imageRelayRequest.mockReset()
  imageApiMocks.imageRequest.mockReset()
})

afterEach(() => {
  client.clear()
  useAuthStore.getState().auth.reset()
})

test('keeps the model-dependent form inside the remaining scrollable page height', () => {
  render(
    <QueryClientProvider client={client}>
      <ImageWorkbench />
    </QueryClientProvider>
  )

  const tabs = screen.getByRole('tablist').closest('[data-slot="tabs"]')
  const shell = tabs?.parentElement
  const page = screen.getByRole('main')
  const scrollRegion = screen
    .getByRole('button', { name: 'Generate image' })
    .closest<HTMLElement>('.overflow-auto')

  expect(shell).toContainElement(page)
  expect(shell).toHaveClass(
    'flex',
    'min-h-0',
    'flex-1',
    'flex-col',
    'overflow-hidden'
  )
  expect(tabs).toHaveClass('shrink-0')
  expect(page).toContainElement(scrollRegion)
})

const capabilityModel: VideoModel = {
  id: 'grok-imagine-video-1.5',
  label: 'Grok Imagine Video 1.5',
  provider: 'xai',
  available: true,
  protocol: 'openai_video',
  supported_parameters: [],
  max_duration_seconds: 15,
  capability: {
    models: ['grok-imagine-video-1.5'],
    modes: [
      {
        name: 'text_to_video',
        duration: { min: 1, max: 15, step: 1, default: 5 },
        resolutions: ['720p', '1080p'],
        defaultResolution: '1080p',
        aspectRatios: ['16:9', '9:16'],
        defaultAspectRatio: '16:9',
      },
      {
        name: 'reference_to_video',
        duration: { min: 1, max: 15, step: 1, default: 8 },
        resolutions: ['720p'],
        defaultResolution: '720p',
        inputs: [
          {
            name: 'reference_images',
            kind: 'image',
            sources: ['url', 'file_id', 'upload'],
            maxItems: 7,
            required: true,
          },
          {
            name: 'reference_audios',
            kind: 'audio',
            sources: ['voice_id'],
            maxItems: 3,
          },
        ],
        options: { generateAudio: true },
      },
    ],
  },
}

test('renders video fields from capability and prevents additional JSON overrides', async () => {
  const user = userEvent.setup()
  const onSubmit = vi.fn()
  render(
    <VideoCapabilityForm
      model={capabilityModel}
      busy={false}
      onChanged={vi.fn()}
      onSubmit={onSubmit}
    />
  )

  expect(screen.queryByLabelText('Reference images *')).not.toBeInTheDocument()
  await user.click(screen.getByRole('combobox', { name: 'Generation mode' }))
  await user.click(screen.getByRole('option', { name: 'References to video' }))
  expect(screen.getByLabelText('Reference images *')).toBeVisible()
  expect(
    screen.getByRole('spinbutton', { name: 'Duration (seconds)' })
  ).toHaveValue(8)
  expect(
    screen.getByRole('combobox', { name: 'Resolution' })
  ).toHaveTextContent('720p')

  await user.type(
    screen.getByLabelText('Prompt'),
    'Keep the subject consistent'
  )
  await user.type(
    screen.getByLabelText('Reference images *'),
    'https://cdn.example/one.png'
  )
  const additional = screen.getByLabelText('Additional parameters')
  await user.clear(additional)
  fireEvent.change(additional, { target: { value: '{"model":"unsafe"}' } })
  await user.click(screen.getByRole('button', { name: 'Generate video' }))
  expect(await screen.findByRole('alert')).toHaveTextContent(
    'Additional parameters cannot override model.'
  )
  expect(onSubmit).not.toHaveBeenCalled()

  await user.clear(additional)
  fireEvent.change(additional, {
    target: { value: '{"source_task_id":"old-task"}' },
  })
  await user.click(screen.getByRole('button', { name: 'Generate video' }))
  expect(await screen.findByRole('alert')).toHaveTextContent(
    'Additional parameters cannot override source_task_id.'
  )
  expect(onSubmit).not.toHaveBeenCalled()

  await user.clear(additional)
  fireEvent.change(additional, { target: { value: '{}' } })
  await user.click(
    screen.getByRole('checkbox', { name: 'Generate synchronized audio' })
  )
  await user.click(screen.getByRole('button', { name: 'Generate video' }))
  expect(onSubmit).toHaveBeenCalledWith({
    mode: 'reference_to_video',
    body: {
      model: 'grok-imagine-video-1.5',
      provider: 'xai',
      prompt: 'Keep the subject consistent',
      seconds: 8,
      resolution: '720p',
      reference_images: ['https://cdn.example/one.png'],
      generate_audio: true,
    },
    files: [],
  })
})

test('hides file upload when a Seedance capability accepts URL assets only', () => {
  const seedance: VideoModel = {
    ...capabilityModel,
    id: 'doubao-seedance-2-0-260128',
    provider: 'doubao',
    capability: {
      models: ['doubao-seedance-2-0-260128'],
      modes: [
        {
          name: 'image_to_video',
          duration: { min: 4, max: 15, step: 1, default: 5 },
          resolutions: ['720p'],
          defaultResolution: '720p',
          inputs: [
            {
              name: 'image',
              kind: 'image',
              sources: ['url', 'asset', 'data_uri'],
              maxItems: 1,
              required: true,
            },
          ],
        },
      ],
    },
  }
  render(
    <VideoCapabilityForm
      model={seedance}
      busy={false}
      onChanged={vi.fn()}
      onSubmit={vi.fn()}
    />
  )
  expect(
    screen.queryByLabelText('Upload First frame or reference image')
  ).not.toBeInTheDocument()
  expect(
    screen.getByText(
      'This provider does not accept browser file uploads here. Use a public URL or asset:// ID.'
    )
  ).toBeVisible()
  expect(
    screen.getByText('Allowed sources: url, asset. Maximum: 1.')
  ).toBeVisible()
})

test('allows an xAI last-frame request without a prompt or first frame', async () => {
  const user = userEvent.setup()
  const onSubmit = vi.fn()
  const frameModel: VideoModel = {
    ...capabilityModel,
    capability: {
      models: ['grok-imagine-video-1.5'],
      modes: [
        {
          name: 'first_last_frame',
          duration: { min: 1, max: 15, step: 1, default: 5 },
          resolutions: ['720p'],
          defaultResolution: '720p',
          inputs: [
            {
              name: 'image',
              kind: 'image',
              sources: ['url', 'file_id', 'upload'],
              maxItems: 1,
            },
            {
              name: 'last_frame',
              kind: 'image',
              sources: ['url', 'file_id', 'upload'],
              maxItems: 1,
              required: true,
            },
          ],
        },
      ],
    },
  }

  render(
    <VideoCapabilityForm
      model={frameModel}
      busy={false}
      onChanged={vi.fn()}
      onSubmit={onSubmit}
    />
  )

  expect(screen.getByLabelText('Prompt')).not.toBeRequired()
  await user.type(
    screen.getByLabelText('Last frame *'),
    'https://cdn.example/last.png'
  )
  await user.click(screen.getByRole('button', { name: 'Generate video' }))
  expect(onSubmit).toHaveBeenCalledWith({
    mode: 'first_last_frame',
    body: {
      model: 'grok-imagine-video-1.5',
      provider: 'xai',
      seconds: 5,
      resolution: '720p',
      last_frame: 'https://cdn.example/last.png',
    },
    files: [],
  })
})

test('resets locked fields and renders source video and extension direction by mode', async () => {
  const user = userEvent.setup()
  const onSubmit = vi.fn()
  const classicXai: VideoModel = {
    ...capabilityModel,
    id: 'grok-imagine-video',
    capability: {
      models: ['grok-imagine-video'],
      modes: [
        {
          name: 'edit_video',
          inputs: [
            {
              name: 'video',
              kind: 'video',
              sources: ['url', 'data_uri', 'file_id'],
              maxItems: 1,
              required: true,
            },
          ],
        },
        {
          name: 'extend_video',
          duration: { min: 2, max: 10, step: 1, default: 6 },
          extensionDirections: ['backward'],
          defaultExtensionDirection: 'backward',
          inputs: [
            {
              name: 'video',
              kind: 'video',
              sources: ['url', 'data_uri', 'file_id'],
              maxItems: 1,
              required: true,
            },
          ],
        },
      ],
    },
  }

  render(
    <VideoCapabilityForm
      model={classicXai}
      busy={false}
      onChanged={vi.fn()}
      onSubmit={onSubmit}
    />
  )

  expect(screen.getByLabelText('Source video *')).toBeVisible()
  expect(screen.queryByLabelText('Duration (seconds)')).not.toBeInTheDocument()
  expect(screen.queryByLabelText('Resolution')).not.toBeInTheDocument()
  expect(screen.queryByLabelText('Upload Source video')).not.toBeInTheDocument()
  await user.type(screen.getByLabelText('Prompt'), 'Replace the background')
  await user.type(
    screen.getByLabelText('Source video *'),
    'https://cdn.example/source.mp4'
  )
  await user.click(screen.getByRole('button', { name: 'Edit video' }))
  expect(onSubmit).toHaveBeenLastCalledWith({
    mode: 'edit_video',
    body: {
      model: 'grok-imagine-video',
      provider: 'xai',
      prompt: 'Replace the background',
      video: 'https://cdn.example/source.mp4',
    },
    files: [],
  })

  await user.click(screen.getByRole('combobox', { name: 'Generation mode' }))
  await user.click(screen.getByRole('option', { name: 'Extend video' }))
  expect(
    screen.getByRole('spinbutton', { name: 'Duration (seconds)' })
  ).toHaveValue(6)
  expect(
    screen.getByRole('combobox', { name: 'Extension direction' })
  ).toHaveTextContent('Backward')
  expect(screen.getByLabelText('Source video *')).toHaveValue('')
  expect(screen.getByLabelText('Additional parameters')).toHaveValue('{}')
})

test('submits edit and extension modes to their durable endpoints', async () => {
  const user = userEvent.setup()
  const classicXai: VideoModel = {
    ...capabilityModel,
    id: 'grok-imagine-video',
    label: 'Grok Imagine Video',
    capability: {
      models: ['grok-imagine-video'],
      modes: [
        {
          name: 'edit_video',
          inputs: [
            {
              name: 'video',
              kind: 'video',
              sources: ['url', 'data_uri', 'file_id'],
              maxItems: 1,
              required: true,
            },
          ],
        },
        {
          name: 'extend_video',
          duration: { min: 2, max: 10, step: 1, default: 6 },
          extensionDirections: ['backward'],
          defaultExtensionDirection: 'backward',
          inputs: [
            {
              name: 'video',
              kind: 'video',
              sources: ['url', 'data_uri', 'file_id'],
              maxItems: 1,
              required: true,
            },
          ],
        },
      ],
    },
  }
  keyApiMocks.getApiKeys.mockResolvedValue({
    success: true,
    data: {
      items: [
        {
          id: 7,
          name: 'Primary',
          key: 'masked',
          status: 1,
          remain_quota: 100,
          used_quota: 0,
          unlimited_quota: false,
          expired_time: -1,
          created_time: 1,
          accessed_time: 1,
          group: 'default',
          auto_groups: null,
          cross_group_retry: false,
          model_limits_enabled: false,
          model_limits: '',
          allow_ips: '',
        },
      ],
      total: 1,
      page: 1,
      page_size: 100,
    },
  })
  keyApiMocks.fetchTokenKey.mockResolvedValue({
    success: true,
    data: { key: 'video-key' },
  })
  imageApiMocks.getImageCapabilities.mockResolvedValue({
    api_key_id: 7,
    capability_version: 'test',
    gateway_base_url: '',
    models: [],
    platforms: [],
    video_models: [classicXai],
  })
  imageApiMocks.imageRelayRequest
    .mockResolvedValueOnce({ task_id: 'video_edit' })
    .mockResolvedValueOnce({ task_id: 'video_extend' })
  imageApiMocks.getImageTask.mockResolvedValue({
    task: { status: 'queued', progress: 0, cost: 0 },
    results: [],
    events: [],
  })

  render(
    <QueryClientProvider client={client}>
      <VideoWorkbench userId={101} />
    </QueryClientProvider>
  )

  await user.click(await screen.findByRole('combobox', { name: 'API Key' }))
  await user.click(screen.getByRole('option', { name: 'Primary' }))
  await user.click(await screen.findByRole('combobox', { name: 'Model' }))
  await user.click(
    screen.getByRole('option', { name: 'Grok Imagine Video · xai' })
  )
  await user.type(screen.getByLabelText('Prompt'), 'Replace the sky')
  await user.type(
    screen.getByLabelText('Source video *'),
    'https://cdn.example/source.mp4'
  )
  await user.click(screen.getByRole('button', { name: 'Edit video' }))
  await waitFor(() =>
    expect(imageApiMocks.imageRelayRequest).toHaveBeenCalled()
  )
  expect(imageApiMocks.imageRelayRequest.mock.calls[0]?.[0]).toBe(
    '/v1/videos/edits_async'
  )
  expect(
    JSON.parse(String(imageApiMocks.imageRelayRequest.mock.calls[0]?.[2]))
  ).toMatchObject({
    model: 'grok-imagine-video',
    provider: 'xai',
    prompt: 'Replace the sky',
    video: 'https://cdn.example/source.mp4',
  })

  await user.click(screen.getByRole('combobox', { name: 'Generation mode' }))
  await user.click(screen.getByRole('option', { name: 'Extend video' }))
  await user.type(screen.getByLabelText('Prompt'), 'Continue backward')
  fireEvent.change(screen.getByLabelText('Source video *'), {
    target: { value: '{"file_id":"file-video"}' },
  })
  await user.click(screen.getByRole('button', { name: 'Extend video' }))
  await waitFor(() =>
    expect(imageApiMocks.imageRelayRequest).toHaveBeenCalledTimes(2)
  )
  expect(imageApiMocks.imageRelayRequest.mock.calls[1]?.[0]).toBe(
    '/v1/videos/extensions_async'
  )
  expect(
    JSON.parse(String(imageApiMocks.imageRelayRequest.mock.calls[1]?.[2]))
  ).toMatchObject({
    seconds: 6,
    extension_direction: 'backward',
    video: { file_id: 'file-video' },
  })
})
