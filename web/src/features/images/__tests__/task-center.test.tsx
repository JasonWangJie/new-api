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
  fireEvent,
  render,
  screen,
  waitFor,
  within,
} from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, expect, test, vi } from 'vitest'

import { ImageDialog } from '@/features/usage-logs/components/dialogs/image-dialog'
import { useAuthStore } from '@/stores/auth-store'

import { getImageTask, getImageTasks, imageRequest } from '../api'
import { ImageTaskDetails } from '../components/image-task-details'
import { imageTaskSuccessRate } from '../lib/task-presentation'
import { ImageTaskCenter } from '../task-center'
import type { ImageTask, ImageTaskDetail } from '../types'

vi.mock('../api', () => ({
  mediaTasksPath: (admin: boolean) =>
    `/api/${admin ? 'admin' : 'user'}/media-tasks`,
  imageTasksPath: (admin: boolean) =>
    `/api/${admin ? 'admin' : 'user'}/async-image-tasks`,
  imageBlob: vi.fn(),
  getImageTasks: vi.fn(),
  getImageTask: vi.fn(),
  imageRequest: vi.fn(),
}))
vi.mock('@/lib/lobe-icon', () => ({
  getLobeIcon: () => null,
}))
vi.mock('@lobehub/icons', () => ({}))
const clients: QueryClient[] = []
const task: ImageTask = {
  id: 'asyncimg_display_contract',
  task_id: 'asyncimg_display_contract',
  platform: 'openai',
  protocol: 'bb',
  model: 'gpt-image-2.5-flare',
  request_type: 'image_to_image',
  status: 'invoking',
  billing_status: 'pending',
  progress: 20,
  image_count: 1,
  result_count: 0,
  cost: 0,
  prompt_summary: 'A futuristic city',
  requested_size: '2048x1186',
  requested_resolution: '2K',
  actual_size: '',
  aspect_ratio: '16:9',
  group: 'Creative group',
  api_key_id: 201,
  api_key_name: 'Studio key',
  user_name: 'Creative account',
  channel_name: 'Sunburst upstream',
  storage_providers: [],
  user_id: 101,
  channel_id: 301,
  attempts: 'secret-fingerprint-canary',
  reference_urls:
    '["https://example.com/reference.png","https://example.com/reference-2.png","data:image/png;base64,abc"]',
  attempt_history: [
    {
      channel_name: 'Previous upstream',
      key_index: 2,
      started_at: 1789600001,
      finished_at: 1789600009,
      dispatched: true,
      reference_mode: 'local',
      error_code: 503,
    },
  ],
  retry_count: 1,
  created_at: 1789600000,
  started_at: 1789600001,
  finished_at: 0,
  expires_at: 1789686400,
  next_attempt_at: 0,
  error_code: '',
  error_message: '',
  can_resume: false,
  can_terminate: true,
}
const detail: ImageTaskDetail = {
  task,
  results: [
    {
      id: 1,
      image_index: 0,
      width: 2048,
      height: 1186,
      content_type: 'image/png',
      byte_size: 1048576,
      checksum: '',
      view_url:
        '/api/admin/async-image-tasks/asyncimg_display_contract/results/0/view',
      url: 'https://example.com/result.png',
      expires_at: 1789686400,
    },
  ],
  events: [
    {
      id: 1,
      event_type: 'queued',
      status: 'queued',
      message: 'admin-only-event-canary',
      created_at: 1789600000,
    },
  ],
}
function mount(content: React.ReactNode) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
  clients.push(client)
  render(<QueryClientProvider client={client}>{content}</QueryClientProvider>)
}
beforeEach(() => {
  // jsdom has no Web Animations API; native preview geometry is checked in the browser.
  Object.defineProperty(HTMLElement.prototype, 'getAnimations', {
    configurable: true,
    value: () => [],
  })
  useAuthStore
    .getState()
    .auth.setUser({ id: 101, username: 'creator', role: 100 })
  vi.mocked(getImageTasks).mockResolvedValue({
    items: [task],
    total: 1,
    pages: 1,
    stats: {
      queued: 30,
      processing: 5,
      succeeded: 96,
      failed: 4,
      image_count: 500,
      average_duration_ms: 54000,
    },
  })
  vi.mocked(getImageTask).mockResolvedValue(detail)
  vi.mocked(imageRequest).mockResolvedValue({
    url: 'https://example.com/result.png',
  })
})
afterEach(() => {
  for (const client of clients) client.clear()
  clients.length = 0
  useAuthStore.getState().auth.reset()
})

test('keeps primary filters visible while advanced fields start collapsed and preserve drafts', async () => {
  const user = userEvent.setup()
  mount(<ImageTaskCenter admin />)
  await screen.findAllByText('Sunburst upstream')
  expect(screen.getByLabelText('Search tasks')).toBeVisible()
  expect(screen.queryByLabelText('Start date')).not.toBeInTheDocument()
  expect(screen.getByRole('button', { name: 'Expand' })).toHaveAttribute(
    'aria-expanded',
    'false'
  )
  await user.type(screen.getByLabelText('Search tasks'), 'city')
  await user.click(screen.getByRole('button', { name: 'Expand' }))
  await user.type(screen.getByLabelText('Model'), 'flare')
  await user.click(screen.getByRole('button', { name: 'Collapse' }))
  expect(screen.getByLabelText('Search tasks')).toHaveValue('city')
  await user.click(screen.getByRole('button', { name: 'Apply filters' }))
  await waitFor(() =>
    expect(vi.mocked(getImageTasks).mock.calls.at(-1)?.[1].get('model')).toBe(
      'flare'
    )
  )
  await user.click(screen.getByRole('button', { name: 'Expand' }))
  expect(screen.getByLabelText('Model')).toHaveValue('flare')
  const statistics = screen.getByLabelText('Task statistics')
  expect(statistics).toHaveTextContent('Success rate96.0%')
  expect(statistics).toHaveTextContent('Processing35')
  expect(statistics).not.toHaveTextContent('500')
  expect(screen.getAllByText('Creative account').length).toBeGreaterThan(0)
  expect(screen.getAllByText('Image to image').length).toBeGreaterThan(0)
})

test('terminates one fixed task after confirmation and retains the dialog when the request fails', async () => {
  const user = userEvent.setup()
  mount(<ImageTaskCenter admin />)
  const terminate = (
    await screen.findAllByRole('button', { name: 'Terminate' })
  )[0]
  await user.click(terminate)
  expect(imageRequest).not.toHaveBeenCalled()
  const confirm = await screen.findByRole('alertdialog', {
    name: 'Terminate image tasks',
  })
  vi.mocked(imageRequest).mockRejectedValueOnce(
    new Error('Termination unavailable')
  )
  await user.click(within(confirm).getByRole('button', { name: 'Continue' }))
  await waitFor(() =>
    expect(
      within(confirm).getByRole('button', { name: 'Continue' })
    ).toBeEnabled()
  )
  expect(confirm).toBeInTheDocument()
  vi.mocked(imageRequest).mockResolvedValueOnce({})
  await user.click(within(confirm).getByRole('button', { name: 'Continue' }))
  await waitFor(() =>
    expect(screen.queryByRole('alertdialog')).not.toBeInTheDocument()
  )
  expect(imageRequest).toHaveBeenLastCalledWith(
    '/api/admin/media-tasks/asyncimg_display_contract/terminate',
    'POST',
    {}
  )
  expect(getImageTasks).toHaveBeenCalledTimes(2)
})

test('admin details show named attempts and generated images without rendering raw fingerprints', async () => {
  const user = userEvent.setup()
  mount(
    <ImageTaskDetails
      id={task.id}
      admin
      userId={101}
      canManage
      onClose={vi.fn()}
      onManage={vi.fn()}
    />
  )
  await screen.findByText('Studio key')
  await user.click(screen.getByRole('button', { name: /Attempt history/ }))
  expect(await screen.findByText('Previous upstream')).toBeVisible()
  expect(screen.getByText('Account slot 3')).toBeVisible()
  expect(screen.getByText('Studio key')).toBeVisible()
  expect(
    screen.queryByText('secret-fingerprint-canary')
  ).not.toBeInTheDocument()
  expect(
    screen.getByRole('button', { name: /Copy URL.*Reference image 1/ })
  ).toHaveTextContent('https://example.com/reference.png')
  expect(
    screen.getByRole('button', { name: /Copy URL.*Reference image 2/ })
  ).toHaveTextContent('https://example.com/reference-2.png')
  expect(screen.getByText(/Embedded image/)).toBeVisible()
  expect(
    screen.getByRole('button', { name: 'Copy all reference image URLs' })
  ).toBeVisible()
  expect(
    screen.getByRole('button', { name: /Open in new tab.*Reference image 1/ })
  ).toBeVisible()
  expect(
    screen.queryByRole('img', { name: /reference/i })
  ).not.toBeInTheDocument()
  expect(
    screen.getByText(
      'Click the URL to copy it. Use the button on the right to open the reference image in a new tab.'
    )
  ).toBeVisible()
  const openWindow = vi.spyOn(window, 'open').mockImplementation(() => null)
  await user.click(
    screen.getByRole('button', { name: 'Copy all reference image URLs' })
  )
  expect(await navigator.clipboard.readText()).toBe(
    'https://example.com/reference.png\nhttps://example.com/reference-2.png'
  )
  await user.click(
    screen.getByRole('button', { name: /Copy URL.*Reference image 1/ })
  )
  expect(await navigator.clipboard.readText()).toBe(
    'https://example.com/reference.png'
  )
  await user.click(
    screen.getByRole('button', { name: /Open in new tab.*Reference image 1/ })
  )
  expect(openWindow).toHaveBeenCalledWith(
    'https://example.com/reference.png',
    '_blank',
    'noopener,noreferrer'
  )
  openWindow.mockRestore()
  await user.click(
    await screen.findByRole('button', { name: 'Image Preview: 1' })
  )
  const preview = await screen.findByRole('dialog', { name: 'Image Preview' })
  const image = within(preview).getByAltText('Generated image')
  expect(image).toHaveAttribute('src', 'https://example.com/result.png')
  expect(imageRequest).not.toHaveBeenCalled()
  fireEvent.error(image)
  expect(within(preview).getByText('Failed to load image')).toBeVisible()
  fireEvent.load(image)
  expect(
    within(preview).queryByText('Failed to load image')
  ).not.toBeInTheDocument()
  await user.keyboard('{Escape}')
  await waitFor(() =>
    expect(
      screen.queryByRole('dialog', { name: 'Image Preview' })
    ).not.toBeInTheDocument()
  )
  expect(
    screen.getByRole('dialog', { name: 'Media task details' })
  ).toBeVisible()
})

test('user details hide admin routing, references and account history even if a fixture includes them', async () => {
  mount(
    <ImageTaskDetails
      id={task.id}
      admin={false}
      userId={101}
      canManage={false}
      onClose={vi.fn()}
      onManage={vi.fn()}
    />
  )
  await screen.findByText('Studio key')
  expect(screen.getByText('Creative group')).toBeVisible()
  for (const text of [
    'Creative account',
    'Sunburst upstream',
    'Previous upstream',
    'Account attempts and reconciliation',
    'admin-only-event-canary',
  ]) {
    expect(screen.queryByText(text)).not.toBeInTheDocument()
  }
  expect(
    screen.queryByRole('button', { name: /Copy URL.*Reference image/ })
  ).not.toBeInTheDocument()
  expect(
    screen.queryByRole('button', { name: /Open in new tab.*Reference image/ })
  ).not.toBeInTheDocument()
  expect(
    screen.queryByRole('button', { name: 'Copy all reference image URLs' })
  ).not.toBeInTheDocument()
  expect(
    screen.queryByRole('button', { name: 'Terminate' })
  ).not.toBeInTheDocument()
  expect(
    await screen.findByRole('button', { name: 'Archive to server' })
  ).toBeVisible()
  expect(getImageTask).toHaveBeenCalledWith(
    false,
    task.id,
    expect.any(AbortSignal)
  )
})

test('administrators with read permission cannot select or terminate tasks', async () => {
  useAuthStore.getState().auth.setUser({
    id: 101,
    username: 'auditor',
    role: 10,
    permissions: {
      admin_permissions: { async_image_task: { read: true, manage: false } },
    },
  })
  mount(<ImageTaskCenter admin />)
  await screen.findAllByText('Sunburst upstream')
  expect(screen.queryByRole('checkbox')).not.toBeInTheDocument()
  expect(
    screen.queryByRole('button', { name: /Terminate/ })
  ).not.toBeInTheDocument()
})

test('success rate excludes unfinished tasks and stays undefined before any terminal result', () => {
  expect(
    imageTaskSuccessRate({
      succeeded: 96,
      failed: 4,
      queued: 300,
      processing: 50,
    })
  ).toBe(96)
  expect(imageTaskSuccessRate({ queued: 300 })).toBeNull()
  expect(imageTaskSuccessRate({ succeeded: 0, failed: 4 })).toBe(0)
})

test('existing standard image previews retain their image URL and copy action', async () => {
  const user = userEvent.setup()
  mount(
    <ImageDialog
      imageUrl='https://example.com/standard.png'
      open
      onOpenChange={vi.fn()}
    />
  )
  const preview = await screen.findByRole('dialog', { name: 'Image Preview' })
  expect(
    within(preview).getByText('https://example.com/standard.png')
  ).toBeVisible()
  expect(
    within(preview).getByRole('button', { name: 'Copy to clipboard' })
  ).toBeVisible()
  await user.click(
    within(preview).getByRole('button', { name: 'Copy to clipboard' })
  )
  expect(await navigator.clipboard.readText()).toBe(
    'https://example.com/standard.png'
  )
})

test('video task details play local results and refresh an expired link', async () => {
  const user = userEvent.setup()
  const link = '/v1/media/objects/local-video?expires=1789690000&access=example'
  const video = {
    ...task,
    media_type: 'video' as const,
    provider: 'sora',
    platform: 'sora',
    request_type: 'text_to_video',
    status: 'succeeded',
    billing_status: 'settled',
    storage_status: 'succeeded',
    cost: 0.5,
  }
  vi.mocked(getImageTask).mockResolvedValue({
    task: video,
    events: [],
    results: [{ ...detail.results[0], url: link, view_url: link }],
  })
  mount(
    <ImageTaskDetails
      id={video.task_id}
      admin={false}
      userId={101}
      onClose={() => {}}
      canManage={false}
      onManage={() => {}}
    />
  )
  await screen.findByText('Generated videos')
  expect(document.querySelector('video')).toHaveAttribute('src', link)
  expect(screen.getByText('Download').closest('a')).toHaveAttribute(
    'href',
    link
  )
  const calls = vi.mocked(getImageTask).mock.calls.length
  await user.click(screen.getByRole('button', { name: 'Refresh link' }))
  await waitFor(() =>
    expect(vi.mocked(getImageTask).mock.calls.length).toBeGreaterThan(calls)
  )
  expect(document.body).not.toHaveTextContent('secret-fingerprint-canary')
})

test('failed image downloads show the upstream link without a local archive action', async () => {
  const user = userEvent.setup()
  const link = 'https://cdn.example.com/image.png?signature=for-owner'
  vi.mocked(getImageTask).mockResolvedValue({
    task: {
      ...task,
      status: 'failed',
      error_code: 'result_download_failed',
      result_count: 1,
    },
    events: [
      {
        id: 1,
        event_type: 'upstream_result_received',
        status: 'upstream_succeeded',
        message: '',
        created_at: 1789600000,
      },
    ],
    results: [
      {
        ...detail.results[0],
        url: link,
        view_url: link,
        source: 'upstream',
        width: 0,
        height: 0,
        byte_size: 0,
      },
    ],
  })
  mount(
    <ImageTaskDetails
      id={task.id}
      admin={false}
      userId={101}
      canManage={false}
      onClose={vi.fn()}
      onManage={vi.fn()}
    />
  )
  await screen.findByText('Upstream link')
  expect(screen.getByAltText('Generated image')).toHaveAttribute('src', link)
  expect(
    screen.queryByRole('button', { name: 'Archive to server' })
  ).not.toBeInTheDocument()
  const openWindow = vi.spyOn(window, 'open').mockImplementation(() => null)
  await user.click(screen.getByRole('button', { name: 'Download image' }))
  expect(openWindow).toHaveBeenCalledWith(link, '_blank', 'noopener,noreferrer')
  openWindow.mockRestore()
  await user.click(screen.getByRole('button', { name: 'Copy link' }))
  expect(await navigator.clipboard.readText()).toBe(link)
})

test('video upstream fallback plays its direct link without offering to refresh it', async () => {
  const link = 'https://cdn.example.com/video.mp4?signature=for-owner'
  vi.mocked(getImageTask).mockResolvedValue({
    task: {
      ...task,
      media_type: 'video',
      request_type: 'text_to_video',
      status: 'succeeded',
      storage_status: 'upstream',
      billing_status: 'settled',
    },
    events: [],
    results: [
      {
        ...detail.results[0],
        url: link,
        view_url: link,
        source: 'upstream',
        byte_size: 0,
      },
    ],
  })
  mount(
    <ImageTaskDetails
      id={task.id}
      admin={false}
      userId={101}
      canManage={false}
      onClose={vi.fn()}
      onManage={vi.fn()}
    />
  )
  await screen.findByText('Upstream link')
  expect(document.querySelector('video')).toHaveAttribute('src', link)
  expect(
    screen.queryByRole('button', { name: 'Refresh link' })
  ).not.toBeInTheDocument()
  expect(screen.getByText('Download').closest('a')).toHaveAttribute(
    'href',
    link
  )
})
