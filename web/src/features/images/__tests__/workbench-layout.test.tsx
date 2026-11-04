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
import { Blob } from 'node:buffer'
import { webcrypto } from 'node:crypto'

import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import {
  act,
  fireEvent,
  render,
  screen,
  waitFor,
  within,
} from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { AxiosError, AxiosHeaders } from 'axios'
import { afterEach, beforeEach, expect, test, vi } from 'vitest'

import { useAuthStore } from '@/stores/auth-store'

import { VideoCapabilityForm } from '../components/video-capability-form'
import type { ImageModel, VideoModel } from '../types'
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

const referenceImageModel: ImageModel = {
  id: 'gemini-image',
  label: 'Gemini Image',
  platform: 'gemini',
  mode: 'async',
  protocol: 'gemini_bb',
  available: true,
  capability: { max_output_images: 1, max_reference_images: 4 },
}

async function prepareImageSubmission(
  imageModel = referenceImageModel,
  additionalModels: ImageModel[] = []
) {
  const user = userEvent.setup()
  keyApiMocks.getApiKeys.mockResolvedValue({
    data: { items: [{ id: 7, name: 'Primary', status: 1 }] },
  })
  keyApiMocks.fetchTokenKey.mockResolvedValue({
    success: true,
    data: { key: 'image-key' },
  })
  imageApiMocks.getImageCapabilities.mockResolvedValue({
    api_key_id: 7,
    capability_version: 'test',
    gateway_base_url: '',
    models: [imageModel, ...additionalModels],
    platforms: [],
  })
  imageApiMocks.imageRelayRequest.mockResolvedValue({ task_id: 'image_queued' })
  imageApiMocks.getImageTask.mockResolvedValue({
    task: { status: 'queued', progress: 0, cost: 0 },
    results: [],
    events: [],
  })
  render(
    <QueryClientProvider client={client}>
      <ImageWorkbench />
    </QueryClientProvider>
  )

  await user.click(await screen.findByRole('combobox', { name: 'API Key' }))
  await user.click(await screen.findByRole('option', { name: 'Primary' }))
  await user.click(screen.getByRole('combobox', { name: 'Model' }))
  await user.click(
    await screen.findByRole('option', {
      name: `${imageModel.label} · ${imageModel.platform === 'mixed' ? 'Mixed scheduling' : imageModel.platform} · Asynchronous`,
    })
  )
  await user.type(
    screen.getByLabelText('Prompt'),
    'Keep the subject consistent'
  )
  return user
}

beforeEach(() => {
  vi.stubGlobal('Blob', Blob)
  vi.stubGlobal('crypto', webcrypto)
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
  vi.unstubAllGlobals()
  vi.useRealTimers()
  client.clear()
  useAuthStore.getState().auth.reset()
})

test('keeps the prompt editable when session storage is blocked', async () => {
  vi.spyOn(Storage.prototype, 'getItem').mockImplementation(() => {
    throw new DOMException('Session storage is blocked', 'SecurityError')
  })
  const user = userEvent.setup()
  render(
    <QueryClientProvider client={client}>
      <ImageWorkbench />
    </QueryClientProvider>
  )

  const prompt = screen.getByRole('textbox', { name: 'Prompt' })
  await user.type(prompt, 'Draw a quiet lake')

  expect(prompt).toHaveValue('Draw a quiet lake')
})

test('restores the prompt saved in session storage', () => {
  sessionStorage.setItem(
    'new-api:image-prompt:101',
    'Keep the saved composition'
  )
  render(
    <QueryClientProvider client={client}>
      <ImageWorkbench />
    </QueryClientProvider>
  )

  expect(screen.getByRole('textbox', { name: 'Prompt' })).toHaveValue(
    'Keep the saved composition'
  )
})

test('resumes an image task after reopening the workbench for its owner', async () => {
  sessionStorage.setItem('new-api:image-task:101', 'image_in_progress')
  imageApiMocks.getImageTask.mockResolvedValue({
    task: { status: 'queued', progress: 0, cost: 0 },
    results: [],
    events: [],
  })
  render(
    <QueryClientProvider client={client}>
      <ImageWorkbench />
    </QueryClientProvider>
  )
  expect(await screen.findByText('image_in_progress')).toBeVisible()
  expect(screen.getByRole('combobox', { name: 'API Key' })).toBeDisabled()
  expect(screen.getByLabelText('Reference image URLs')).toBeDisabled()
})

test('accepts multiple reference URLs in one field and submits trimmed lines in order', async () => {
  const user = await prepareImageSubmission()
  const references = screen.getByRole('textbox', {
    name: 'Reference image URLs',
  })
  expect(references.tagName).toBe('TEXTAREA')
  expect(
    screen.getAllByRole('textbox', { name: 'Reference image URLs' })
  ).toHaveLength(1)
  expect(references).toHaveAccessibleDescription(
    'Paste one image URL per line. Blank lines are ignored.'
  )
  expect(
    screen.queryByRole('button', { name: /^(?:Add )?Image URL$/ })
  ).not.toBeInTheDocument()

  await user.click(references)
  await user.paste(
    '  https://cdn.example/first.png  \r\n\r\n  \nhttps://cdn.example/second.png\n'
  )
  await user.click(screen.getByRole('button', { name: 'Add text part' }))
  await user.type(screen.getByLabelText('Text part'), 'Use the second palette')
  await user.click(screen.getByRole('button', { name: 'Generate image' }))

  await waitFor(() =>
    expect(imageApiMocks.imageRelayRequest).toHaveBeenCalledOnce()
  )
  expect(
    JSON.parse(String(imageApiMocks.imageRelayRequest.mock.calls[0]?.[2]))
      .messages[0].content
  ).toEqual([
    { type: 'text', text: 'Keep the subject consistent' },
    { type: 'image_url', image_url: { url: 'https://cdn.example/first.png' } },
    { type: 'image_url', image_url: { url: 'https://cdn.example/second.png' } },
    { type: 'text', text: 'Use the second palette' },
  ])
})

test('ignores an empty reference field when generating from text alone', async () => {
  const user = await prepareImageSubmission()
  await user.type(screen.getByLabelText('Reference image URLs'), ' \n  \n')
  await user.click(screen.getByRole('button', { name: 'Generate image' }))

  await waitFor(() =>
    expect(imageApiMocks.imageRelayRequest).toHaveBeenCalledOnce()
  )
  expect(
    JSON.parse(String(imageApiMocks.imageRelayRequest.mock.calls[0]?.[2]))
      .messages[0].content
  ).toEqual([{ type: 'text', text: 'Keep the subject consistent' }])
})

test('mixed image selection shows shared limits and submits references without fixing a provider', async () => {
  const user = await prepareImageSubmission({
    ...referenceImageModel,
    id: 'seedream-5-pro',
    label: 'Seedream',
    platform: 'mixed',
    protocol: 'openai_images',
    supports_edit: true,
    capability: {
      resolutions: ['1K', '2K'],
      max_output_images: 2,
      max_reference_images: 3,
    },
  })
  expect(
    screen.getByRole('spinbutton', { name: 'Image count' })
  ).toHaveAttribute('max', '2')
  expect(
    screen.queryByRole('combobox', { name: 'Quality' })
  ).not.toBeInTheDocument()
  expect(
    screen.getByText(/Mixed scheduling accepts asynchronous image requests/)
  ).toBeVisible()
  await user.type(
    screen.getByLabelText('Reference image URLs'),
    'https://cdn.example/cabinet.png'
  )
  await user.click(screen.getByRole('button', { name: 'Generate image' }))
  await waitFor(() =>
    expect(imageApiMocks.imageRelayRequest).toHaveBeenCalledOnce()
  )
  const [endpoint, , body] = imageApiMocks.imageRelayRequest.mock.calls[0]
  expect(endpoint).toBe('/v1/images/edits_async')
  expect(JSON.parse(String(body))).toMatchObject({
    model: 'seedream-5-pro',
    resolution: '1K',
    n: 1,
    images: [{ image_url: 'https://cdn.example/cabinet.png' }],
  })
  expect(JSON.parse(String(body))).not.toHaveProperty('provider')
  expect(JSON.parse(String(body))).not.toHaveProperty('platform')
})

test('a mixed model limited to 2K selects and submits its supported resolution', async () => {
  const user = await prepareImageSubmission({
    ...referenceImageModel,
    id: 'mixed-2k',
    label: 'Mixed 2K',
    platform: 'mixed',
    protocol: 'openai_images',
    capability: {
      max_output_images: 1,
      max_reference_images: 3,
      resolutions: ['2K'],
    },
  })

  expect(
    screen.getByRole('combobox', { name: 'Resolution' })
  ).toHaveTextContent('2K')
  await user.click(screen.getByRole('button', { name: 'Generate image' }))
  await waitFor(() =>
    expect(imageApiMocks.imageRelayRequest).toHaveBeenCalledOnce()
  )
  expect(
    JSON.parse(String(imageApiMocks.imageRelayRequest.mock.calls[0][2]))
  ).toMatchObject({ model: 'mixed-2k', resolution: '2K' })
})

test.each([
  { name: 'fixed dimensions', parameters: [], hasRatio: false },
  {
    name: 'fixed resolution',
    parameters: ['ratio', 'aspect_ratio'],
    hasRatio: true,
  },
])(
  'a TabTab alias with $name hides unsupported settings and submits AUTO without pixels',
  async ({ parameters, hasRatio }) => {
    const user = await prepareImageSubmission({
      ...referenceImageModel,
      id: 'private-image-alias',
      label: 'TabTab Alias',
      platform: 'tabtab',
      provider: 'tabtab',
      protocol: 'openai_images',
      supports_reference_generation: true,
      supported_parameters: ['model', 'prompt', 'n', ...parameters],
      capability: {
        max_output_images: 1,
        max_reference_images: 3,
        resolutions: ['1K', '2K', '4K'],
        qualities: ['high'],
        formats: ['png'],
        backgrounds: ['opaque'],
      },
    })
    expect(
      screen.queryByRole('combobox', { name: 'Resolution' })
    ).not.toBeInTheDocument()
    expect(
      screen.queryByRole('combobox', { name: 'Quality' })
    ).not.toBeInTheDocument()
    expect(
      screen.queryByRole('combobox', { name: 'Output format' })
    ).not.toBeInTheDocument()
    expect(
      screen.queryByRole('combobox', { name: 'Background' })
    ).not.toBeInTheDocument()
    if (hasRatio) {
      expect(
        screen.getByRole('combobox', { name: 'Aspect Ratio' })
      ).toHaveTextContent('1:1')
    } else {
      expect(
        screen.queryByRole('combobox', { name: 'Aspect Ratio' })
      ).not.toBeInTheDocument()
    }
    await user.click(screen.getByRole('button', { name: 'Generate image' }))
    await waitFor(() =>
      expect(imageApiMocks.imageRelayRequest).toHaveBeenCalledOnce()
    )
    const body = JSON.parse(
      String(imageApiMocks.imageRelayRequest.mock.calls[0][2])
    )
    expect(body).toMatchObject({
      provider: 'tabtab',
      model: 'private-image-alias',
      resolution: 'AUTO',
      n: 1,
    })
    expect(body).not.toHaveProperty('size')
    if (hasRatio) expect(body.aspect_ratio).toBe('1:1')
    else expect(body).not.toHaveProperty('aspect_ratio')
  }
)

test.each([
  {
    name: 'fixed dimensions',
    parameters: ['model', 'prompt', 'n'],
    hasRatio: false,
  },
  {
    name: 'fixed resolution',
    parameters: ['model', 'prompt', 'n', 'aspect_ratio'],
    hasRatio: true,
  },
])(
  'a mixed AUTO alias with $name exposes only supported controls and remains providerless',
  async ({ parameters, hasRatio }) => {
    const user = await prepareImageSubmission({
      ...referenceImageModel,
      id: 'fixed-mixed-alias',
      label: 'Fixed Mixed',
      platform: 'mixed',
      protocol: 'openai_images',
      supported_parameters: parameters,
      supports_reference_generation: true,
      capability: {
        max_output_images: 1,
        max_reference_images: 3,
        resolutions: ['AUTO'],
      },
    })
    expect(
      screen.queryByRole('combobox', { name: 'Resolution' })
    ).not.toBeInTheDocument()
    if (hasRatio) {
      expect(
        screen.getByRole('combobox', { name: 'Aspect Ratio' })
      ).toHaveTextContent('1:1')
    } else {
      expect(
        screen.queryByRole('combobox', { name: 'Aspect Ratio' })
      ).not.toBeInTheDocument()
    }
    await user.click(screen.getByRole('button', { name: 'Generate image' }))
    await waitFor(() =>
      expect(imageApiMocks.imageRelayRequest).toHaveBeenCalledOnce()
    )
    const body = JSON.parse(
      String(imageApiMocks.imageRelayRequest.mock.calls[0][2])
    )
    expect(body).toMatchObject({
      model: 'fixed-mixed-alias',
      resolution: 'AUTO',
      n: 1,
    })
    expect(body).not.toHaveProperty('provider')
    expect(body).not.toHaveProperty('size')
    if (hasRatio) expect(body.aspect_ratio).toBe('1:1')
    else expect(body).not.toHaveProperty('aspect_ratio')
  }
)

test('a TabTab model exposes only compatible tier resolutions and sends the selected ratio without a pixel-size approximation', async () => {
  const user = await prepareImageSubmission({
    ...referenceImageModel,
    id: 'tabtab-tier-alias',
    label: 'TabTab Tier',
    platform: 'tabtab',
    provider: 'tabtab',
    protocol: 'openai_images',
    supported_parameters: ['size', 'resolution', 'ratio', 'aspect_ratio'],
    capability: {
      max_output_images: 1,
      max_reference_images: 3,
      resolutions: ['1K', '2K'],
    },
  })
  await user.click(screen.getByRole('combobox', { name: 'Resolution' }))
  expect(screen.queryByRole('option', { name: '4K' })).not.toBeInTheDocument()
  await user.click(screen.getByRole('option', { name: '2K' }))
  await user.click(screen.getByRole('combobox', { name: 'Aspect Ratio' }))
  expect(screen.queryByRole('option', { name: 'auto' })).not.toBeInTheDocument()
  await user.click(screen.getByRole('option', { name: '4:3' }))
  expect(screen.queryByText(/Submitted size:/)).not.toBeInTheDocument()
  await user.click(screen.getByRole('button', { name: 'Generate image' }))
  await waitFor(() =>
    expect(imageApiMocks.imageRelayRequest).toHaveBeenCalledOnce()
  )
  expect(
    JSON.parse(String(imageApiMocks.imageRelayRequest.mock.calls[0][2]))
  ).toMatchObject({ provider: 'tabtab', resolution: '2K', aspect_ratio: '4:3' })
})

test('switching TabTab pixel, fixed, and tier aliases resets hidden settings and selects the new model resolution', async () => {
  const shared: ImageModel = {
    ...referenceImageModel,
    platform: 'tabtab',
    provider: 'tabtab',
    protocol: 'openai_images',
    supports_reference_generation: true,
    capability: { max_output_images: 1, max_reference_images: 3 },
  }
  const user = await prepareImageSubmission(
    {
      ...shared,
      id: 'pixel-alias',
      label: 'Pixel Alias',
      supported_parameters: [
        'model',
        'prompt',
        'n',
        'size',
        'width',
        'height',
        'quality',
        'output_format',
        'background',
      ],
      capability: {
        ...shared.capability,
        resolutions: ['1K', '2K'],
        qualities: ['high'],
        formats: ['png'],
        backgrounds: ['opaque'],
      },
    },
    [
      {
        ...shared,
        id: 'lite-alias',
        label: 'Lite Alias',
        supported_parameters: ['model', 'prompt', 'n', 'aspect_ratio'],
        capability: { ...shared.capability, resolutions: ['AUTO'] },
      },
      {
        ...shared,
        id: 'fixed-alias',
        label: 'Fixed Alias',
        supported_parameters: ['model', 'prompt', 'n'],
        capability: { ...shared.capability, resolutions: ['AUTO'] },
      },
      {
        ...shared,
        id: 'tier-alias',
        label: 'Tier Alias',
        supported_parameters: [
          'model',
          'prompt',
          'n',
          'size',
          'resolution',
          'aspect_ratio',
        ],
        capability: { ...shared.capability, resolutions: ['4K'] },
      },
    ]
  )
  await user.click(screen.getByRole('combobox', { name: 'Quality' }))
  await user.click(screen.getByRole('option', { name: 'high' }))
  await user.click(screen.getByRole('combobox', { name: 'Output format' }))
  await user.click(screen.getByRole('option', { name: 'png' }))
  await user.click(screen.getByRole('combobox', { name: 'Background' }))
  await user.click(screen.getByRole('option', { name: 'opaque' }))
  await user.click(screen.getByRole('combobox', { name: 'Resolution' }))
  await user.click(screen.getByRole('option', { name: '2K' }))
  await user.click(screen.getByRole('combobox', { name: 'Aspect Ratio' }))
  await user.click(screen.getByRole('option', { name: '4:3' }))

  await user.click(screen.getByRole('combobox', { name: 'Model' }))
  await user.click(
    screen.getByRole('option', { name: 'Lite Alias · tabtab · Asynchronous' })
  )
  expect(
    screen.queryByRole('combobox', { name: 'Resolution' })
  ).not.toBeInTheDocument()
  expect(
    screen.queryByRole('combobox', { name: 'Quality' })
  ).not.toBeInTheDocument()
  expect(
    screen.queryByRole('combobox', { name: 'Output format' })
  ).not.toBeInTheDocument()
  expect(
    screen.queryByRole('combobox', { name: 'Background' })
  ).not.toBeInTheDocument()
  expect(
    screen.getByRole('combobox', { name: 'Aspect Ratio' })
  ).toHaveTextContent('4:3')

  await user.click(screen.getByRole('combobox', { name: 'Model' }))
  await user.click(
    screen.getByRole('option', { name: 'Fixed Alias · tabtab · Asynchronous' })
  )
  expect(
    screen.queryByRole('combobox', { name: 'Resolution' })
  ).not.toBeInTheDocument()
  expect(
    screen.queryByRole('combobox', { name: 'Aspect Ratio' })
  ).not.toBeInTheDocument()

  await user.click(screen.getByRole('combobox', { name: 'Model' }))
  await user.click(
    screen.getByRole('option', { name: 'Tier Alias · tabtab · Asynchronous' })
  )
  expect(
    screen.getByRole('combobox', { name: 'Resolution' })
  ).toHaveTextContent('4K')
  expect(
    screen.getByRole('combobox', { name: 'Aspect Ratio' })
  ).toHaveTextContent('1:1')
  expect(
    screen.queryByRole('spinbutton', { name: 'Image count' })
  ).not.toBeInTheDocument()
  await user.click(screen.getByRole('button', { name: 'Generate image' }))
  await waitFor(() =>
    expect(imageApiMocks.imageRelayRequest).toHaveBeenCalledOnce()
  )
  expect(
    JSON.parse(String(imageApiMocks.imageRelayRequest.mock.calls[0][2]))
  ).toEqual({
    provider: 'tabtab',
    model: 'tier-alias',
    prompt: 'Keep the subject consistent',
    resolution: '4K',
    aspect_ratio: '1:1',
    n: 1,
  })
})

test.each(['newapi', 'volcengine', 'gemini'])(
  'an OpenAI images model on %s exposes and submits its supported output count',
  async (provider) => {
    const user = await prepareImageSubmission({
      ...referenceImageModel,
      id: 'batch-image',
      label: 'Batch image',
      platform: provider,
      provider,
      protocol: 'openai_images',
      capability: {
        max_output_images: 4,
        max_reference_images: 3,
        resolutions: ['1K'],
      },
    })
    const count = screen.getByRole('spinbutton', { name: 'Image count' })
    expect(count).toHaveAttribute('max', '4')
    await user.clear(count)
    await user.type(count, '3')
    await user.click(screen.getByRole('button', { name: 'Generate image' }))
    await waitFor(() =>
      expect(imageApiMocks.imageRelayRequest).toHaveBeenCalledOnce()
    )
    expect(
      JSON.parse(String(imageApiMocks.imageRelayRequest.mock.calls[0][2]))
    ).toMatchObject({ provider, n: 3 })
  }
)

test('Gemini native requests keep a single output even when the catalog limit is larger', async () => {
  const user = await prepareImageSubmission({
    ...referenceImageModel,
    platform: 'vertex',
    provider: 'vertex',
    protocol: 'gemini_native',
    capability: { max_output_images: 4, max_reference_images: 3 },
  })
  expect(
    screen.queryByRole('spinbutton', { name: 'Image count' })
  ).not.toBeInTheDocument()
  await user.click(screen.getByRole('button', { name: 'Generate image' }))
  await waitFor(() =>
    expect(imageApiMocks.imageRelayRequest).toHaveBeenCalledOnce()
  )
  expect(
    JSON.parse(String(imageApiMocks.imageRelayRequest.mock.calls[0][2]))
  ).toMatchObject({ provider: 'vertex', n: 1 })
})

test('waits for all uploaded references before generating and preserves their selected order', async () => {
  const user = await prepareImageSubmission()
  const readers: FileReader[] = []
  const NativeFileReader = FileReader
  vi.stubGlobal(
    'FileReader',
    class extends NativeFileReader {
      override readAsDataURL() {
        readers.push(this)
      }
    }
  )
  const upload = screen.getByLabelText('Upload reference image')
  await user.upload(upload, [
    new File(['first'], 'first.png', { type: 'image/png' }),
    new File(['second'], 'second.png', { type: 'image/png' }),
  ])

  expect(readers).toHaveLength(2)
  const loadingButton = screen.getByRole('button', { name: 'Loading...' })
  expect(loadingButton).toBeDisabled()
  expect(upload).toBeDisabled()
  expect(screen.getByLabelText('Reference image URLs')).toBeDisabled()
  await user.click(loadingButton)
  expect(imageApiMocks.imageRelayRequest).not.toHaveBeenCalled()

  await act(async () => {
    Object.defineProperty(readers[1], 'result', {
      value: 'data:image/png;base64,c2Vjb25k',
    })
    readers[1].dispatchEvent(new ProgressEvent('load'))
  })
  expect(screen.getByRole('button', { name: 'Loading...' })).toBeDisabled()
  expect(screen.getByLabelText('Reference image URLs')).toBeDisabled()
  expect(imageApiMocks.imageRelayRequest).not.toHaveBeenCalled()

  await act(async () => {
    Object.defineProperty(readers[0], 'result', {
      value: 'data:image/png;base64,Zmlyc3Q=',
    })
    readers[0].dispatchEvent(new ProgressEvent('load'))
  })

  expect(screen.getByRole('button', { name: 'Generate image' })).toBeEnabled()
  expect(upload).toBeEnabled()
  expect(screen.getByLabelText('Reference image URLs')).toBeEnabled()
  expect(await screen.findAllByAltText('Reference image')).toHaveLength(2)
  await user.click(screen.getByRole('button', { name: 'Add text part' }))
  await user.type(screen.getByLabelText('Text part'), 'Preserve both subjects')
  await user.click(screen.getByRole('button', { name: 'Generate image' }))

  await waitFor(() =>
    expect(imageApiMocks.imageRelayRequest).toHaveBeenCalledOnce()
  )
  expect(
    JSON.parse(String(imageApiMocks.imageRelayRequest.mock.calls[0]?.[2]))
      .messages[0].content
  ).toEqual([
    { type: 'text', text: 'Keep the subject consistent' },
    { type: 'image_url', image_url: { url: 'data:image/png;base64,Zmlyc3Q=' } },
    { type: 'image_url', image_url: { url: 'data:image/png;base64,c2Vjb25k' } },
    { type: 'text', text: 'Preserve both subjects' },
  ])
})

test('disables reference editing while an image submission is pending', async () => {
  const user = await prepareImageSubmission()
  keyApiMocks.fetchTokenKey.mockReturnValue(new Promise(() => {}))
  await user.click(screen.getByRole('button', { name: 'Generate image' }))

  expect(
    await screen.findByRole('button', { name: 'Submitting' })
  ).toBeDisabled()
  expect(screen.getByLabelText('Reference image URLs')).toBeDisabled()
})

test('switches media with the keyboard and shows a named results area and empty state for each tab', async () => {
  const user = userEvent.setup()
  render(
    <QueryClientProvider client={client}>
      <ImageWorkbench />
    </QueryClientProvider>
  )

  const tabs = screen.getByRole('tablist', { name: 'Media type' })
  const imageTab = within(tabs).getByRole('tab', { name: 'Image' })
  const videoTab = within(tabs).getByRole('tab', { name: 'Video' })
  expect(imageTab).toHaveAttribute('aria-selected', 'true')
  expect(
    screen.getByRole('form', { name: 'Image generation settings' })
  ).toBeVisible()
  expect(
    within(screen.getByRole('region', { name: 'Results' })).getByText(
      'Your images will appear here'
    )
  ).toBeVisible()

  await user.click(imageTab)
  await user.keyboard('{ArrowRight}')

  expect(videoTab).toHaveFocus()
  await user.keyboard('{Enter}')
  expect(videoTab).toHaveAttribute('aria-selected', 'true')
  expect(imageTab).toHaveAttribute('aria-selected', 'false')
  expect(
    within(screen.getByRole('region', { name: 'Results' })).getByText(
      'Your videos will appear here'
    )
  ).toBeVisible()

  await user.keyboard('{ArrowLeft}')
  expect(imageTab).toHaveFocus()
  await user.keyboard('{Enter}')
  expect(imageTab).toHaveAttribute('aria-selected', 'true')
  expect(screen.getByLabelText('Reference image URLs')).toBeVisible()
})

test.each(['image', 'video'] as const)(
  'a missing restored %s task is forgotten so the workbench can start a new request',
  async (media) => {
    sessionStorage.setItem(`new-api:${media}-task:101`, 'removed_task')
    imageApiMocks.getImageTask.mockRejectedValue(
      new AxiosError(
        'Task not found',
        'ERR_BAD_REQUEST',
        undefined,
        undefined,
        {
          status: 404,
          statusText: 'Not Found',
          data: { success: false, message: 'Task not found' },
          headers: {},
          config: { headers: new AxiosHeaders() },
        }
      )
    )
    render(
      <QueryClientProvider client={client}>
        {media === 'image' ? (
          <ImageWorkbench />
        ) : (
          <VideoWorkbench userId={101} />
        )}
      </QueryClientProvider>
    )
    await waitFor(() =>
      expect(sessionStorage.getItem(`new-api:${media}-task:101`)).toBe('')
    )
    expect(screen.getByRole('combobox', { name: 'API Key' })).toBeEnabled()
  }
)

test.each(['image', 'video'] as const)(
  'a temporary query failure preserves the restored %s task for later polling',
  async (media) => {
    sessionStorage.setItem(`new-api:${media}-task:101`, 'pending_task')
    imageApiMocks.getImageTask.mockRejectedValue(
      new AxiosError(
        'Service unavailable',
        'ERR_BAD_RESPONSE',
        undefined,
        undefined,
        {
          status: 503,
          statusText: 'Service Unavailable',
          data: {},
          headers: {},
          config: { headers: new AxiosHeaders() },
        }
      )
    )
    render(
      <QueryClientProvider client={client}>
        {media === 'image' ? (
          <ImageWorkbench />
        ) : (
          <VideoWorkbench userId={101} />
        )}
      </QueryClientProvider>
    )
    expect(await screen.findByRole('alert')).toBeVisible()
    expect(sessionStorage.getItem(`new-api:${media}-task:101`)).toBe(
      'pending_task'
    )
    if (media === 'image') {
      expect(screen.getByRole('combobox', { name: 'API Key' })).toBeDisabled()
    }
  }
)

test('continues video polling while a failed save has a scheduled retry', async () => {
  vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval'] })
  sessionStorage.setItem('new-api:video-task:101', 'video_saving')
  imageApiMocks.getImageTask
    .mockResolvedValueOnce({
      task: {
        status: 'storage_failed',
        progress: 90,
        cost: 2,
        next_attempt_at: 1234,
      },
      results: [],
      events: [],
    })
    .mockResolvedValue({
      task: { status: 'succeeded', progress: 100, cost: 2, next_attempt_at: 0 },
      results: [],
      events: [],
    })
  render(
    <QueryClientProvider client={client}>
      <VideoWorkbench userId={101} />
    </QueryClientProvider>
  )
  expect(await screen.findByRole('status')).toHaveTextContent('Storage failed')
  await act(async () => {
    await vi.advanceTimersByTimeAsync(3100)
  })
  await waitFor(() =>
    expect(screen.getByRole('status')).toHaveTextContent('Succeeded')
  )
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

test.each([
  {
    name: 'provider collection with one URL',
    mixed: false,
    limits: [2],
    count: 1,
    array: true,
  },
  {
    name: 'provider collection with two URLs',
    mixed: false,
    limits: [2],
    count: 2,
    array: true,
  },
  {
    name: 'mixed single input with one URL',
    mixed: true,
    limits: [1],
    count: 1,
    array: false,
  },
  {
    name: 'mixed single URL with a wider candidate',
    mixed: true,
    limits: [1, 2],
    count: 1,
    array: false,
  },
  {
    name: 'mixed collection with two URLs',
    mixed: true,
    limits: [1, 2],
    count: 2,
    array: true,
  },
])('$name preserves every input and its wire shape', async (testCase) => {
  const variants = testCase.limits.map((limit) => ({
    models: ['custom-video'],
    modes: [
      {
        name: 'image_to_video' as const,
        inputs: [
          {
            name: 'image' as const,
            kind: 'image' as const,
            sources: ['url' as const],
            maxItems: limit,
            required: true,
          },
        ],
      },
    ],
  }))
  const model: VideoModel = {
    ...capabilityModel,
    id: 'custom-video',
    provider: testCase.mixed ? '' : 'custom-video',
    routing_platform: testCase.mixed ? 'mixed' : undefined,
    capability: variants[0],
    capability_variants: testCase.mixed ? variants : undefined,
  }
  const onSubmit = vi.fn()
  render(
    <VideoCapabilityForm
      model={model}
      busy={false}
      onChanged={vi.fn()}
      onSubmit={onSubmit}
    />
  )
  const user = userEvent.setup()
  const urls = [
    'https://cdn.example/one.png',
    'https://cdn.example/two.png',
  ].slice(0, testCase.count)
  await user.type(
    screen.getByLabelText('First frame or reference image *'),
    urls.join('\n')
  )
  await user.click(screen.getByRole('button', { name: 'Generate video' }))
  expect(onSubmit).toHaveBeenCalledOnce()
  expect(onSubmit.mock.calls[0][0].body.image).toEqual(
    testCase.array ? urls : urls[0]
  )
})

test('mixed video keeps complete capability combinations and submits without a provider', async () => {
  const first = {
    name: 'text_to_video' as const,
    duration: { values: [5], default: 5 },
    resolutions: ['720p'],
    defaultResolution: '720p',
  }
  const second = {
    ...first,
    duration: { values: [10], default: 10 },
    resolutions: ['1080p'],
    defaultResolution: '1080p',
  }
  const mixed: VideoModel = {
    ...capabilityModel,
    id: 'shared-video',
    provider: '',
    routing_platform: 'mixed',
    capability: { models: ['shared-video'], modes: [first] },
    capability_variants: [
      { models: ['shared-video'], modes: [first] },
      { models: ['shared-video'], modes: [second] },
    ],
  }
  const onSubmit = vi.fn()
  render(
    <VideoCapabilityForm
      model={mixed}
      busy={false}
      onChanged={vi.fn()}
      onSubmit={onSubmit}
    />
  )
  const user = userEvent.setup()
  await user.type(screen.getByLabelText('Prompt'), 'A city at dawn')
  await user.click(screen.getByRole('combobox', { name: 'Resolution' }))
  await user.click(screen.getByRole('option', { name: '1080p' }))
  expect(
    screen.getByRole('combobox', { name: 'Duration (seconds)' })
  ).toHaveTextContent('5')
  expect(screen.getByRole('button', { name: 'Generate video' })).toBeDisabled()
  expect(screen.getByRole('status')).toHaveTextContent(
    'No mixed video channel supports this complete request.'
  )
  await user.click(screen.getByRole('combobox', { name: 'Duration (seconds)' }))
  await user.click(screen.getByRole('option', { name: '10' }))
  await user.click(screen.getByRole('button', { name: 'Generate video' }))
  expect(onSubmit).toHaveBeenCalledWith({
    mode: 'text_to_video',
    body: {
      model: 'shared-video',
      prompt: 'A city at dawn',
      seconds: 10,
      resolution: '1080p',
    },
    files: [],
  })
})

test('video workbench uses the duration allowed by the chosen resolution', async () => {
  const model: VideoModel = {
    ...capabilityModel,
    provider: 'huahuo-video',
    capability: {
      ...capabilityModel.capability,
      modes: [
        {
          name: 'text_to_video',
          resolutions: ['720p', '1080p'],
          defaultResolution: '720p',
          duration: { values: [5, 10], default: 10 },
          durationsByResolution: { '1080p': { values: [5], default: 5 } },
        },
      ],
    },
  }
  const onSubmit = vi.fn()
  render(
    <VideoCapabilityForm
      model={model}
      busy={false}
      onChanged={vi.fn()}
      onSubmit={onSubmit}
    />
  )
  const user = userEvent.setup()
  await user.type(screen.getByLabelText('Prompt'), 'A city at dawn')
  await user.click(screen.getByRole('combobox', { name: 'Resolution' }))
  await user.click(screen.getByRole('option', { name: '1080p' }))
  await user.click(screen.getByRole('combobox', { name: 'Duration (seconds)' }))
  expect(screen.queryByRole('option', { name: '10' })).not.toBeInTheDocument()
  await user.click(screen.getByRole('option', { name: '5' }))
  await user.click(screen.getByRole('button', { name: 'Generate video' }))
  expect(onSubmit).toHaveBeenCalledWith({
    mode: 'text_to_video',
    body: {
      model: model.id,
      prompt: 'A city at dawn',
      provider: 'huahuo-video',
      resolution: '1080p',
      seconds: 5,
    },
    files: [],
  })
})

test('mixed video preserves omitted booleans and explicit false without changing reference arrays', async () => {
  const mode = {
    name: 'reference_to_video' as const,
    inputs: [
      {
        name: 'reference_images' as const,
        kind: 'image' as const,
        sources: ['url' as const],
        maxItems: 1,
        required: true,
      },
    ],
    options: { generateAudio: true },
  }
  const mixed: VideoModel = {
    ...capabilityModel,
    id: 'shared-reference',
    provider: '',
    routing_platform: 'mixed',
    capability: { models: ['shared-reference'], modes: [mode] },
    capability_variants: [{ models: ['shared-reference'], modes: [mode] }],
  }
  const onSubmit = vi.fn()
  render(
    <VideoCapabilityForm
      model={mixed}
      busy={false}
      onChanged={vi.fn()}
      onSubmit={onSubmit}
    />
  )
  const user = userEvent.setup()
  await user.type(
    screen.getByLabelText('Reference images *'),
    'asset://private-account'
  )
  expect(screen.getByRole('button', { name: 'Generate video' })).toBeDisabled()
  await user.clear(screen.getByLabelText('Reference images *'))
  await user.type(
    screen.getByLabelText('Reference images *'),
    'https://cdn.example/ref.png'
  )
  await user.click(screen.getByRole('button', { name: 'Generate video' }))
  expect(onSubmit.mock.calls[0][0].body).toEqual({
    model: 'shared-reference',
    reference_images: ['https://cdn.example/ref.png'],
  })
  await user.click(
    screen.getByRole('combobox', { name: 'Generate synchronized audio' })
  )
  await user.click(screen.getByRole('option', { name: 'No' }))
  await user.click(screen.getByRole('button', { name: 'Generate video' }))
  expect(onSubmit.mock.calls[1][0].body).toEqual({
    model: 'shared-reference',
    reference_images: ['https://cdn.example/ref.png'],
    generate_audio: false,
  })
})

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

test.each(['mixed', 'huahuo-video'])(
  'preserves a single start frame for %s routing',
  async (provider) => {
    const mixed = provider === 'mixed'
    const profile: VideoModel['capability'] = {
      models: [capabilityModel.id],
      modes: [
        {
          name: 'first_last_frame',
          inputs: [
            {
              name: 'image',
              kind: 'image',
              sources: ['url'],
              maxItems: 1,
              required: true,
            },
          ],
        },
      ],
    }
    const model: VideoModel = {
      ...capabilityModel,
      provider: mixed ? '' : provider,
      routing_platform: mixed ? 'mixed' : undefined,
      capability: profile,
      capability_variants: mixed ? [profile] : undefined,
    }
    const onSubmit = vi.fn()
    render(
      <VideoCapabilityForm
        model={model}
        busy={false}
        onChanged={vi.fn()}
        onSubmit={onSubmit}
      />
    )
    const user = userEvent.setup()
    const start = 'https://cdn.example/start.png'
    await user.type(
      screen.getByLabelText('First frame or reference image *'),
      start
    )
    if (!mixed) {
      expect(screen.getByLabelText('Prompt')).toBeRequired()
      await user.click(screen.getByRole('button', { name: 'Generate video' }))
      expect(onSubmit).not.toHaveBeenCalled()
    }
    await user.type(screen.getByLabelText('Prompt'), 'Start from the image')
    await user.click(screen.getByRole('button', { name: 'Generate video' }))
    expect(onSubmit).toHaveBeenCalledWith({
      mode: 'first_last_frame',
      body: {
        model: model.id,
        prompt: 'Start from the image',
        ...(mixed
          ? { start_frame: start }
          : { provider, mode: 'first_last_frame', image: start }),
      },
      files: [],
    })
  }
)

test('Huahuo rejects combining reference image links and uploaded files in the workbench', async () => {
  const model: VideoModel = {
    ...capabilityModel,
    provider: 'huahuo-video',
    capability: {
      ...capabilityModel.capability,
      modes: [
        {
          name: 'reference_to_video',
          inputs: [
            {
              name: 'reference_images',
              kind: 'image',
              sources: ['url', 'upload'],
              maxItems: 2,
            },
          ],
        },
      ],
    },
  }
  const onSubmit = vi.fn()
  render(
    <VideoCapabilityForm
      model={model}
      busy={false}
      onChanged={vi.fn()}
      onSubmit={onSubmit}
    />
  )
  const user = userEvent.setup()
  await user.type(screen.getByLabelText('Prompt'), 'Use a reference image')
  const links = screen.getByLabelText('Reference images', { exact: true })
  await user.type(links, 'https://cdn.example/reference.png')
  const file = new File(['image'], 'reference.png', { type: 'image/png' })
  await user.upload(screen.getByLabelText('Upload Reference images'), file)
  await user.click(screen.getByRole('button', { name: 'Generate video' }))
  expect(screen.getByRole('alert')).toHaveTextContent(
    'Use either image links or uploaded files for reference images.'
  )
  expect(onSubmit).not.toHaveBeenCalled()
  await user.clear(links)
  await user.click(screen.getByRole('button', { name: 'Generate video' }))
  expect(onSubmit).toHaveBeenCalledWith({
    mode: 'reference_to_video',
    body: {
      model: model.id,
      provider: 'huahuo-video',
      prompt: 'Use a reference image',
    },
    files: [{ field: 'reference_images', file }],
  })
})

test('switching video modes clears native uploads so the same image can be selected again', async () => {
  const input = {
    name: 'image' as const,
    kind: 'image' as const,
    sources: ['url' as const, 'upload' as const],
    maxItems: 1,
    required: true,
  }
  const profile = {
    models: [capabilityModel.id],
    modes: [
      { name: 'image_to_video' as const, inputs: [input] },
      {
        name: 'first_last_frame' as const,
        inputs: [
          { ...input, required: false },
          { ...input, name: 'last_frame' as const },
        ],
      },
    ],
  }
  const model: VideoModel = {
    ...capabilityModel,
    provider: '',
    routing_platform: 'mixed',
    capability: profile,
    capability_variants: [profile],
  }
  const onSubmit = vi.fn()
  render(
    <VideoCapabilityForm
      model={model}
      busy={false}
      onChanged={vi.fn()}
      onSubmit={onSubmit}
    />
  )
  const user = userEvent.setup()
  const image = new File(['image'], 'first.png', { type: 'image/png' })
  await user.upload(
    screen.getByLabelText('Upload First frame or reference image'),
    image
  )
  await user.click(screen.getByRole('combobox', { name: 'Generation mode' }))
  await user.click(screen.getByRole('option', { name: 'First and last frame' }))
  expect(
    (
      screen.getByLabelText(
        'Upload First frame or reference image'
      ) as HTMLInputElement
    ).files
  ).toHaveLength(0)
  await user.upload(
    screen.getByLabelText('Upload Last frame'),
    new File(['last'], 'last.png', { type: 'image/png' })
  )
  await user.click(screen.getByRole('combobox', { name: 'Generation mode' }))
  await user.click(screen.getByRole('option', { name: 'Image to video' }))
  const upload = screen.getByLabelText(
    'Upload First frame or reference image'
  ) as HTMLInputElement
  expect(upload.files).toHaveLength(0)
  await user.upload(upload, image)
  await user.click(screen.getByRole('button', { name: 'Generate video' }))
  expect(onSubmit).toHaveBeenCalledWith({
    mode: 'image_to_video',
    body: { model: model.id },
    files: [{ field: 'image', file: image }],
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
