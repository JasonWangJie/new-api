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
  act,
  fireEvent,
  render,
  screen,
  waitFor,
  within,
} from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { toast, Toaster } from 'sonner'
import { afterEach, beforeEach, expect, test, vi } from 'vitest'

import { ImageDialog } from '@/features/usage-logs/components/dialogs/image-dialog'
import { useAuthStore } from '@/stores/auth-store'

import {
  getImageTask,
  getImageReliability,
  getImageRetainedRequest,
  getImageTasks,
  getMediaTaskPlatforms,
  imageRequest,
} from '../api'
import { ImageTaskDetails } from '../components/image-task-details'
import {
  imageTaskIssue,
  imageTaskProgress,
  imageTaskSuccessRate,
} from '../lib/task-presentation'
import { ImageTaskCenter } from '../task-center'
import type {
  ImageTask,
  ImageTaskDetail,
  ImageReliabilityStats,
} from '../types'

vi.mock('../api', () => ({
  mediaTasksPath: (admin: boolean) =>
    `/api/${admin ? 'admin' : 'user'}/media-tasks`,
  imageTasksPath: (admin: boolean) =>
    `/api/${admin ? 'admin' : 'user'}/async-image-tasks`,
  imageBlob: vi.fn(),
  getImageTasks: vi.fn(),
  getMediaTaskPlatforms: vi.fn(),
  getImageTask: vi.fn(),
  getImageReliability: vi.fn(),
  getImageRetainedRequest: vi.fn(),
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
      http_status: 503,
      error_message: 'Upstream image service temporarily unavailable',
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

test('administrator recovery statistics use the full filter scope and keep unavailable rates unknown', async () => {
  const totals: ImageReliabilityStats = {
    total: 20,
    succeeded: 9,
    failed: 2,
    expired: 1,
    execution_unknown: 3,
    pending: 3,
    postprocess_pending: 1,
    admin_terminated: 1,
    audited_tasks: 12,
    final_success: { succeeded: 9, resolved: 12, percent: 75 },
    first_submission_success: { succeeded: 3, resolved: 12, percent: 25 },
    retry_recovery: { succeeded: 6, resolved: 9, percent: 66.7 },
    account_switch_recovery: { succeeded: 0, resolved: 0, percent: null },
    local_fallback_recovery: { succeeded: 0, resolved: 0, percent: null },
  }
  vi.mocked(getImageReliability).mockResolvedValue({ totals, groups: [] })
  mount(<ImageTaskCenter admin />)
  const user = userEvent.setup()
  expect(getImageReliability).not.toHaveBeenCalled()
  await user.click(
    screen.getByRole('button', { name: 'Image retry recovery statistics' })
  )
  const region = await screen.findByRole('region', {
    name: 'Image retry recovery statistics',
  })
  expect(await within(region).findByText('75.0% (9/12)')).toBeVisible()
  expect(within(region).getAllByText('— (0/0)')).toHaveLength(2)
  const params = vi.mocked(getImageReliability).mock.calls[0]?.[0]
  expect(params?.has('page')).toBe(false)
  expect(params?.has('page_size')).toBe(false)
  expect(params?.get('start_date')).toBeTruthy()
  expect(params?.get('end_date')).toBeTruthy()
  expect(region).toHaveTextContent('Unknown: 3')
  expect(region).toHaveTextContent('Manually terminated: 1')
})

test.each([true, false])(
  'retained failure diagnostics require administrator expansion (admin=%s)',
  async (admin) => {
    const until = Math.floor(Date.now() / 1000) + 3600
    vi.mocked(getImageTask).mockResolvedValue({
      ...detail,
      results: [],
      task: {
        ...task,
        status: 'failed',
        retained_request_available: true,
        request_retain_until: until,
        retry_decision: {
          version: 1,
          stage: 'submit',
          action: 'stop',
          reason: 'invalid_input',
          rule: 'upstream_failed',
          used: 0,
          limit: 0,
          total_used: 0,
          total_limit: 16,
          recorded_at: 1,
        },
      },
    })
    vi.mocked(getImageRetainedRequest)
      .mockRejectedValueOnce(new Error('Retained request unavailable'))
      .mockResolvedValue({
        task_id: task.id,
        retain_until: until,
        request: {
          model: task.model,
          prompt: 'A complete retained prompt',
          prompt_characters: 26,
          prompt_bytes: 26,
          prompt_truncated: false,
          platform: 'openai',
          kind: 'text_to_image',
          count: 1,
          size: '1K',
          resolution: '1K',
          aspect_ratio: '1:1',
          reference_count: 0,
        },
      })
    mount(
      <ImageTaskDetails
        id={task.id}
        admin={admin}
        userId={101}
        onClose={vi.fn()}
        canManage={false}
        onManage={vi.fn()}
      />
    )
    expect(await screen.findByText('Invalid input parameters')).toBeVisible()
    expect(getImageRetainedRequest).not.toHaveBeenCalled()
    const toggle = screen.queryByRole('button', {
      name: 'View retained request',
    })
    if (!admin) {
      expect(toggle).not.toBeInTheDocument()
      return
    }
    const user = userEvent.setup()
    await user.click(
      screen.getByRole('button', { name: 'View retained request' })
    )
    expect(
      await screen.findByText('Retained request unavailable')
    ).toBeVisible()
    await user.click(screen.getByRole('button', { name: 'Retry' }))
    expect(
      await screen.findByLabelText('Retained request diagnostics')
    ).toHaveTextContent('A complete retained prompt')
    expect(getImageRetainedRequest).toHaveBeenCalledWith(
      task.id,
      expect.any(AbortSignal)
    )
  }
)
function mount(content: React.ReactNode) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
  clients.push(client)
  render(<QueryClientProvider client={client}>{content}</QueryClientProvider>)
}
function setMobileViewport() {
  const matchMedia = window.matchMedia.bind(window)
  vi.spyOn(window, 'matchMedia').mockImplementation((query) => ({
    ...matchMedia(query),
    matches: query === '(max-width: 640px)' || matchMedia(query).matches,
  }))
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
  vi.mocked(getMediaTaskPlatforms).mockResolvedValue([
    'azure',
    'gemini',
    'kling',
    'openai',
  ])
  vi.mocked(imageRequest).mockResolvedValue({
    url: 'https://example.com/result.png',
  })
})
afterEach(() => {
  toast.dismiss()
  for (const client of clients) client.clear()
  clients.length = 0
  useAuthStore.getState().auth.reset()
})

test.each([false, true])(
  'administrator successful task cost uses all filtered results after average duration (mobile=%s)',
  async (mobile) => {
    if (mobile) setMobileViewport()
    vi.mocked(getImageTasks).mockResolvedValue({
      items: [
        {
          ...task,
          status: 'succeeded',
          billing_status: 'succeeded',
          cost: 0.12,
        },
      ],
      total: 100,
      pages: 5,
      stats: { succeeded: 96, succeeded_cost: 12.3456789 },
    })
    mount(<ImageTaskCenter admin />)
    await screen.findByRole('button', { name: task.id })
    const statistics = screen.getByLabelText('Task statistics')
    const costLabel = within(statistics).getByText('Successful task cost')
    expect(costLabel.nextElementSibling?.textContent).toBe('$12.345679')
    expect(
      within(statistics)
        .getAllByRole('term')
        .slice(-2)
        .map((term) => term.textContent)
    ).toEqual(['Average duration', 'Successful task cost'])
    if (mobile) {
      expect(costLabel.parentElement).toHaveClass('col-span-6')
      expect(
        within(statistics).getByText('Average duration').parentElement
      ).toHaveClass('col-span-3')
    }
  }
)

test('administrator successful task cost follows applied filters and reset without using draft filters', async () => {
  vi.mocked(getImageTasks).mockImplementation(async (_admin, params) => ({
    items: [task],
    total: 1,
    pages: 1,
    stats: { succeeded_cost: params.get('q') === 'flare' ? 0.25 : 0.75 },
  }))
  mount(<ImageTaskCenter admin />)
  const statistics = screen.getByLabelText('Task statistics')
  expect(await within(statistics).findByText('$0.75')).toBeVisible()
  const user = userEvent.setup()
  await user.type(screen.getByLabelText('Search tasks'), 'flare')
  expect(within(statistics).getByText('$0.75')).toBeVisible()
  await user.click(screen.getByRole('button', { name: 'Apply filters' }))
  expect(await within(statistics).findByText('$0.25')).toBeVisible()
  await user.click(screen.getByRole('button', { name: 'Reset' }))
  expect(await within(statistics).findByText('$0.75')).toBeVisible()
  expect(screen.getByLabelText('Search tasks')).toHaveValue('')
})

test.each([
  { name: 'empty results', succeededCost: 0, expected: '$0' },
  { name: 'missing total', succeededCost: undefined, expected: '—' },
  { name: 'unavailable total', succeededCost: null, expected: '—' },
])(
  'administrator successful task cost displays $expected for $name',
  async ({ succeededCost, expected }) => {
    vi.mocked(getImageTasks).mockResolvedValue({
      items: [],
      total: 0,
      pages: 0,
      stats:
        succeededCost === undefined ? {} : { succeeded_cost: succeededCost },
    })
    mount(<ImageTaskCenter admin />)
    await screen.findByText('No image tasks found')
    expect(
      within(screen.getByLabelText('Task statistics')).getByText(
        'Successful task cost'
      ).nextElementSibling?.textContent
    ).toBe(expected)
  }
)

test('user task statistics do not expose the administrator successful task cost total', async () => {
  vi.mocked(getImageTasks).mockResolvedValue({
    items: [task],
    total: 1,
    pages: 1,
    stats: { succeeded_cost: 12.34 },
  })
  mount(<ImageTaskCenter />)
  await screen.findByRole('button', { name: task.id })
  const statistics = screen.getByLabelText('Task statistics')
  expect(
    within(statistics).queryByText('Successful task cost')
  ).not.toBeInTheDocument()
  expect(within(statistics).queryByText('$12.34')).not.toBeInTheDocument()
})

test.each([
  { mobile: false, status: 'failed' },
  { mobile: true, status: 'failed' },
  { mobile: false, status: 'execution_unknown' },
  { mobile: true, status: 'execution_unknown' },
])(
  '$status keeps its status in the list and shows failure information only in details (mobile=$mobile)',
  async ({ mobile, status }) => {
    if (mobile) setMobileViewport()
    const failedTask = {
      ...task,
      status,
      billing_status: status === 'failed' ? 'refunded' : 'pending',
      error_code: 'upstream_failed',
      error_message:
        status === 'failed'
          ? 'Reference image exceeds the upstream 20MB limit.'
          : 'Upstream transport failed after dispatch',
      failure_code: 610,
      error_summary:
        status === 'failed'
          ? 'Upstream rejected the request; the exact cause is not confirmed'
          : 'Upstream execution could not be confirmed',
      error_suggestion:
        'Do not submit again until an administrator has checked the upstream result and billing.',
      error_retryable: false,
      error_stage: 'submit',
    } satisfies ImageTask
    vi.mocked(getImageTasks).mockResolvedValue({
      items: [failedTask],
      total: 1,
      pages: 1,
      stats: {},
    })
    vi.mocked(getImageTask).mockResolvedValue({
      ...detail,
      task: failedTask,
      results: [],
    })
    mount(<ImageTaskCenter admin />)
    const openDetails = await screen.findByRole('button', { name: task.id })
    const listTask = mobile
      ? screen.getByRole('article', { name: task.id })
      : screen.getByRole('row', { name: new RegExp(task.id) })
    expect(
      within(listTask).getByText(
        status === 'failed' ? 'Failed' : 'Execution unknown'
      )
    ).toBeVisible()
    expect(
      screen.getAllByText(status === 'failed' ? 'Refunded' : 'Pending').length
    ).toBeGreaterThan(0)
    for (const message of [
      failedTask.error_summary,
      failedTask.error_message,
      failedTask.error_suggestion,
      'Task stage',
    ]) {
      expect(screen.queryByText(message)).not.toBeInTheDocument()
    }
    expect(screen.queryByText(/20%/)).not.toBeInTheDocument()
    expect(screen.queryByRole('progressbar')).not.toBeInTheDocument()
    const user = userEvent.setup()
    await user.click(openDetails)
    const dialog = await screen.findByRole('dialog')
    expect(
      await within(dialog).findByText(failedTask.error_summary)
    ).toBeVisible()
    expect(within(dialog).getByText(failedTask.error_suggestion)).toBeVisible()
    expect(within(dialog).getByText('Upstream submission')).toBeVisible()
    expect(
      within(dialog).queryByText(failedTask.error_message)
    ).not.toBeInTheDocument()
    await user.click(
      within(dialog).getByRole('button', { name: 'Upstream diagnostic' })
    )
    expect(within(dialog).getByText(failedTask.error_message)).toBeVisible()
  }
)

test.each([
  {
    action: 'retry_submission',
    rule: 'upstream_failed',
    retryable: true,
    visible: true,
  },
  {
    action: 'poll_same_job',
    rule: 'upstream_failed',
    retryable: true,
    visible: true,
  },
  {
    action: 'poll_same_job',
    rule: 'upstream_pending',
    retryable: true,
    visible: false,
  },
  {
    action: 'manual_recovery',
    rule: 'quota_cache_reservation_unknown',
    retryable: false,
    visible: true,
  },
])(
  '$action/$rule keeps recovery status in the list and recovery information in details',
  async ({ action, rule, retryable, visible }) => {
    const message = 'Previous upstream diagnostic'
    const retryTask: ImageTask = {
      ...task,
      status: 'queued',
      error_code: 'upstream_failed',
      error_message: message,
      error_retryable: retryable,
      next_attempt_at: 1789600040,
      retry_decision: {
        version: 1,
        stage: 'poll',
        action,
        reason: 'temporary_failure',
        rule,
        used: 1,
        limit: 2,
        total_used: 1,
        total_limit: 4,
        recorded_at: 1789600009,
      },
    }
    vi.mocked(getImageTasks).mockResolvedValue({
      items: [retryTask],
      total: 1,
      pages: 1,
      stats: {},
    })
    vi.mocked(getImageTask).mockResolvedValue({
      ...detail,
      task: retryTask,
      results: [],
    })
    mount(<ImageTaskCenter />)
    const openDetails = await screen.findByRole('button', { name: task.id })
    expect(screen.queryByText(message)).not.toBeInTheDocument()
    expect(screen.queryByText('Next retry')).not.toBeInTheDocument()
    expect(screen.queryByText('Total retries')).not.toBeInTheDocument()
    if (visible && retryable) {
      expect(screen.getByText(/Retry scheduled/)).toBeVisible()
    } else {
      expect(screen.queryByText(/Retry scheduled/)).not.toBeInTheDocument()
    }
    const user = userEvent.setup()
    await user.click(openDetails)
    const dialog = await screen.findByRole('dialog')
    if (!visible) {
      await within(dialog).findByRole('heading', { name: 'Retry decision' })
      expect(within(dialog).queryByRole('status')).not.toBeInTheDocument()
      return
    }
    const notice = await within(dialog).findByRole('status')
    expect(within(notice).getByText(message)).toBeVisible()
    if (!retryable) {
      expect(within(notice).queryByText('Next retry')).not.toBeInTheDocument()
      return
    }
    expect(within(notice).getByText('Next retry')).toBeVisible()
    expect(within(notice).getByText('Total retries')).toBeVisible()
    if (action === 'poll_same_job') {
      expect(
        within(notice).getByText(
          'The existing upstream job will be checked again; no new generation will be submitted.'
        )
      ).toBeVisible()
      expect(
        within(notice).queryByText(
          'The task is scheduled for another submission within its retry limits.'
        )
      ).not.toBeInTheDocument()
    }
  }
)

test.each([
  'failed',
  'expired',
  'execution_unknown',
  'storage_failed',
  'billing_failed',
  'succeeded',
])(
  '%s never shows a stale generation percentage or hides a final issue behind an old healthy poll',
  (status) => {
    const finalTask: ImageTask = {
      ...task,
      status,
      error_code: 'old_diagnostic',
      error_message: 'Stored diagnostic',
      retry_decision: {
        version: 1,
        stage: 'poll',
        action: 'poll_same_job',
        reason: 'accepted_job',
        rule: 'upstream_pending',
        used: 0,
        limit: 0,
        total_used: 0,
        total_limit: 4,
        recorded_at: 1,
      },
    }
    expect(imageTaskProgress(finalTask)).toBeNull()
    if (status === 'succeeded') expect(imageTaskIssue(finalTask)).toBeNull()
    else expect(imageTaskIssue(finalTask)).not.toBeNull()
  }
)

test('public details separate a translated explanation from expandable upstream diagnostics', async () => {
  vi.mocked(getImageTask).mockResolvedValue({
    ...detail,
    results: [],
    task: {
      ...task,
      status: 'failed',
      billing_status: 'refunded',
      error_code: 'upstream_failed',
      failure_code: 610,
      error_summary:
        'Upstream rejected the request; the exact cause is not confirmed',
      error_suggestion:
        'Ask the service provider to check the request format, model compatibility, and upstream request ID.',
      error_message: 'Request contains an invalid argument.',
      error_stage: 'submit',
      error_retryable: false,
    },
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
  expect(
    await screen.findByText(
      'Upstream rejected the request; the exact cause is not confirmed'
    )
  ).toBeVisible()
  expect(screen.getByText('Upstream submission')).toBeVisible()
  expect(
    screen.queryByText('Request contains an invalid argument.')
  ).not.toBeInTheDocument()
  const user = userEvent.setup()
  await user.click(screen.getByRole('button', { name: 'Upstream diagnostic' }))
  expect(
    screen.getByText('Request contains an invalid argument.')
  ).toBeVisible()
  expect(
    screen.queryByRole('button', { name: /Attempt history/ })
  ).not.toBeInTheDocument()
})

test('local routing dependency waits show readable decision and stage labels in public details', async () => {
  vi.mocked(getImageTask).mockResolvedValue({
    ...detail,
    results: [],
    task: {
      ...task,
      status: 'queued',
      error_code: 'routing_controls_unavailable',
      error_message: 'Image routing controls are temporarily unavailable',
      error_stage: 'routing',
      error_retryable: true,
      next_attempt_at: 1789600040,
      retry_decision: {
        version: 1,
        stage: 'routing',
        action: 'wait_local_dependency',
        reason: 'routing_controls_unavailable',
        rule: 'routing_controls_unavailable',
        used: 0,
        limit: 0,
        total_used: 0,
        total_limit: 4,
        recorded_at: 1789600009,
      },
    },
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
  expect(
    await screen.findByText('Waiting for image retry controls')
  ).toBeVisible()
  expect(screen.getByText('Scheduling')).toBeVisible()
  expect(
    screen.queryByText('routing_controls_unavailable')
  ).not.toBeInTheDocument()
})

test.each([false, true])(
  'quota uncertainty prevents ordinary user recovery in lists and details (mobile=%s)',
  async (mobile) => {
    if (mobile) setMobileViewport()
    const paused = {
      ...task,
      status: 'billing_failed',
      error_code: 'billing_quota_uncertain',
      can_resume: true,
    }
    vi.mocked(getImageTasks).mockResolvedValue({
      items: [paused],
      total: 1,
      pages: 1,
      stats: {},
    })
    vi.mocked(getImageTask).mockResolvedValue({
      ...detail,
      task: paused,
      results: [],
    })
    mount(<ImageTaskCenter />)
    await screen.findByRole('button', { name: task.id })
    expect(
      screen.queryByRole('button', { name: 'Resume' })
    ).not.toBeInTheDocument()
    const user = userEvent.setup()
    await user.click(screen.getByRole('button', { name: task.id }))
    const details = await screen.findByRole('dialog', {
      name: 'Media task details',
    })
    await within(details).findByText('Studio key')
    expect(
      screen.queryByRole('button', { name: 'Resume' })
    ).not.toBeInTheDocument()
    expect(imageRequest).not.toHaveBeenCalled()
  }
)

test.each([
  { source: 'list', code: 'billing_quota_uncertain', confirmQuota: true },
  { source: 'details', code: 'billing_quota_uncertain', confirmQuota: true },
  { source: 'list', code: 'storage_failed', confirmQuota: false },
])(
  'administrator quota uncertainty recovery requires explicit confirmation without changing ordinary post-processing recovery ($source/$code)',
  async ({ source, code, confirmQuota }) => {
    const paused = {
      ...task,
      status: 'billing_failed',
      error_code: code,
      can_resume: true,
    }
    vi.mocked(getImageTasks).mockResolvedValue({
      items: [paused],
      total: 1,
      pages: 1,
      stats: {},
    })
    vi.mocked(getImageTask).mockResolvedValue({
      ...detail,
      task: paused,
      results: [],
    })
    mount(<ImageTaskCenter admin />)
    await screen.findByRole('button', { name: task.id })
    const user = userEvent.setup()
    if (source === 'details') {
      await user.click(screen.getByRole('button', { name: task.id }))
      await screen.findByRole('dialog', { name: 'Media task details' })
    }
    await user.click(await screen.findByRole('button', { name: 'Resume' }))
    const confirmation = await screen.findByRole('alertdialog', {
      name: 'Resume media post-processing',
    })
    if (confirmQuota) {
      expect(
        within(confirmation).getByText(
          /Ask an administrator to reconcile the quota reservation before trying again\./
        )
      ).toBeVisible()
    } else {
      expect(
        within(confirmation).queryByText(/Ask an administrator to reconcile/)
      ).not.toBeInTheDocument()
    }
    expect(imageRequest).not.toHaveBeenCalled()
    await user.click(
      within(confirmation).getByRole('button', { name: 'Continue' })
    )
    await waitFor(() =>
      expect(imageRequest).toHaveBeenCalledWith(
        `/api/admin/media-tasks/${task.id}/resume`,
        'POST',
        confirmQuota ? { confirm_quota_reconciled: true } : {}
      )
    )
  }
)

test.each([
  { admin: false, mobile: false },
  { admin: true, mobile: false },
  { admin: false, mobile: true },
  { admin: true, mobile: true },
])(
  'admin=$admin mobile=$mobile configured platforms stay visible and apply from page one',
  async ({ admin, mobile }) => {
    if (mobile) setMobileViewport()
    vi.mocked(getImageTasks).mockResolvedValue({
      items: [task],
      total: 45,
      pages: 3,
      stats: {},
    })
    const user = userEvent.setup()
    mount(<ImageTaskCenter admin={admin} />)
    await screen.findByRole('button', { name: task.id })
    const platform = screen.getByRole('combobox', { name: 'Platform' })
    expect(platform).toBeVisible()
    expect(platform).toHaveTextContent('All')
    expect(vi.mocked(getMediaTaskPlatforms).mock.calls.at(-1)?.[0]).toBe(admin)
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    if (!mobile) {
      expect(
        screen.getByRole('button', { name: /^More filters/ })
      ).toHaveAttribute('aria-expanded', 'false')
    }
    await user.click(screen.getByRole('button', { name: 'Go to next page' }))
    await waitFor(() =>
      expect(vi.mocked(getImageTasks).mock.calls.at(-1)?.[1].get('page')).toBe(
        '2'
      )
    )
    if (admin) {
      await user.click(
        screen.getAllByRole('checkbox', { name: `Select task ${task.id}` })[0]
      )
    }

    for (const option of [
      { label: 'Gemini', value: 'gemini', previous: null },
      { label: 'OpenAI', value: 'openai', previous: 'gemini' },
      { label: 'azure', value: 'azure', previous: 'openai' },
      { label: 'kling', value: 'kling', previous: 'azure' },
      { label: 'All', value: null, previous: 'kling' },
    ]) {
      await user.click(platform)
      expect(
        screen.getAllByRole('option').map((item) => item.textContent)
      ).toEqual(['All', 'azure', 'Gemini', 'kling', 'OpenAI'])
      if (option.value === 'openai') {
        await user.keyboard('{End}{Enter}')
      } else {
        await user.click(screen.getByRole('option', { name: option.label }))
      }
      expect(platform).toHaveTextContent(option.label)
      expect(
        vi.mocked(getImageTasks).mock.calls.at(-1)?.[1].get('platform')
      ).toBe(option.previous)
      await user.click(screen.getByRole('button', { name: 'Apply filters' }))
      await waitFor(() =>
        expect(
          vi.mocked(getImageTasks).mock.calls.at(-1)?.[1].get('platform')
        ).toBe(option.value)
      )
      expect(vi.mocked(getImageTasks).mock.calls.at(-1)?.[0]).toBe(admin)
      expect(vi.mocked(getImageTasks).mock.calls.at(-1)?.[1].get('page')).toBe(
        '1'
      )
      if (admin) {
        expect(
          screen.getAllByRole('checkbox', { name: `Select task ${task.id}` })[0]
        ).not.toBeChecked()
      }
    }
  }
)

test.each([
  { configured: [], labels: ['All'] },
  { configured: ['azure'], labels: ['All', 'azure'] },
])(
  'configured=$configured omits unconfigured platforms even when the current page has OpenAI tasks',
  async ({ configured, labels }) => {
    vi.mocked(getMediaTaskPlatforms).mockResolvedValue(configured)
    const user = userEvent.setup()
    mount(<ImageTaskCenter />)
    await screen.findByRole('button', { name: task.id })
    await user.click(screen.getByRole('combobox', { name: 'Platform' }))
    expect(
      screen.getAllByRole('option').map((item) => item.textContent)
    ).toEqual(labels)
  }
)

test('platform selection waits for configured options to load', async () => {
  let complete!: (platforms: string[]) => void
  vi.mocked(getMediaTaskPlatforms).mockReturnValue(
    new Promise<string[]>((resolve) => {
      complete = resolve
    })
  )
  const user = userEvent.setup()
  mount(<ImageTaskCenter />)
  await screen.findByRole('button', { name: task.id })
  const platform = screen.getByRole('combobox', { name: 'Platform' })
  expect(platform).toBeDisabled()
  complete(['gemini'])
  await waitFor(() => expect(platform).toBeEnabled())
  await user.click(platform)
  expect(screen.getAllByRole('option').map((item) => item.textContent)).toEqual(
    ['All', 'Gemini']
  )
})

test('platform lookup failure is visible without inventing configured options', async () => {
  vi.mocked(getMediaTaskPlatforms).mockRejectedValue(
    new Error('Platform lookup unavailable')
  )
  const user = userEvent.setup()
  mount(<ImageTaskCenter />)
  expect(await screen.findByRole('alert')).toHaveTextContent(
    'Platform lookup unavailable'
  )
  await user.click(screen.getByRole('combobox', { name: 'Platform' }))
  expect(screen.getAllByRole('option').map((item) => item.textContent)).toEqual(
    ['All']
  )
})

test('refresh reloads platform options after the configured channels change', async () => {
  vi.mocked(getMediaTaskPlatforms).mockResolvedValueOnce(['openai'])
  const user = userEvent.setup()
  mount(<ImageTaskCenter />)
  await screen.findByRole('button', { name: task.id })
  const platform = screen.getByRole('combobox', { name: 'Platform' })
  await user.click(platform)
  expect(screen.getByRole('option', { name: 'OpenAI' })).toBeVisible()
  await user.keyboard('{Escape}')
  vi.mocked(getMediaTaskPlatforms).mockResolvedValue(['azure'])
  await user.click(screen.getByRole('button', { name: 'Refresh' }))
  await user.click(platform)
  expect(await screen.findByRole('option', { name: 'azure' })).toBeVisible()
  expect(
    screen.queryByRole('option', { name: 'OpenAI' })
  ).not.toBeInTheDocument()
})

test.each([
  { admin: true, mobile: false },
  { admin: false, mobile: true },
])(
  'floating refresh is absent for admin=$admin mobile=$mobile',
  async ({ admin, mobile }) => {
    if (mobile) setMobileViewport()
    mount(<ImageTaskCenter admin={admin} />)
    await screen.findByRole('button', { name: task.id })
    expect(
      screen.queryByRole('button', { name: 'Refresh · Media task center' })
    ).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Refresh' })).toBeVisible()
  }
)

test('mobile administrator floating refresh shares loading, failure recovery and both refresh requests', async () => {
  setMobileViewport()
  const user = userEvent.setup()
  mount(<ImageTaskCenter admin />)
  const floating = await screen.findByRole('button', {
    name: 'Refresh · Media task center',
  })
  const header = screen.getByRole('button', { name: 'Refresh' })
  await waitFor(() => expect(floating).toBeEnabled())
  let finishTasks!: (value: Awaited<ReturnType<typeof getImageTasks>>) => void
  let failPlatforms!: (reason: Error) => void
  vi.mocked(getImageTasks).mockReturnValueOnce(
    new Promise((resolve) => {
      finishTasks = resolve
    })
  )
  vi.mocked(getMediaTaskPlatforms).mockReturnValueOnce(
    new Promise((_, reject) => {
      failPlatforms = reject
    })
  )

  await user.click(floating)
  for (const button of [floating, header]) {
    await waitFor(() => expect(button).toBeDisabled())
    expect(button).toHaveAttribute('aria-busy', 'true')
    expect(button.querySelector('svg')).toHaveClass('animate-spin')
  }
  await user.click(floating)
  expect(getImageTasks).toHaveBeenCalledTimes(2)
  expect(getMediaTaskPlatforms).toHaveBeenCalledTimes(2)
  await act(async () => {
    finishTasks({ items: [task], total: 1, pages: 1, stats: {} })
  })
  expect(floating).toBeDisabled()
  await act(async () => {
    failPlatforms(new Error('Platform refresh unavailable'))
  })
  await waitFor(() => expect(floating).toBeEnabled())
  expect(floating).toHaveAttribute('aria-busy', 'false')
  expect(header).toBeEnabled()
  expect(screen.getByRole('alert')).toHaveTextContent(
    'Platform refresh unavailable'
  )
  await user.click(floating)
  await waitFor(() => expect(getImageTasks).toHaveBeenCalledTimes(3))
  expect(getMediaTaskPlatforms).toHaveBeenCalledTimes(3)
  await waitFor(() => expect(floating).toBeEnabled())
})

test('mobile administrator floating refresh yields to filter and task detail dialogs', async () => {
  setMobileViewport()
  const user = userEvent.setup()
  mount(<ImageTaskCenter admin />)
  const buttonName = 'Refresh · Media task center'
  expect(await screen.findByRole('button', { name: buttonName })).toBeVisible()
  await user.click(screen.getByRole('button', { name: 'More filters' }))
  expect(
    screen.queryByRole('button', { name: buttonName })
  ).not.toBeInTheDocument()
  await user.keyboard('{Escape}')
  expect(await screen.findByRole('button', { name: buttonName })).toBeVisible()
  await user.click(screen.getByRole('button', { name: task.id }))
  await screen.findByRole('dialog')
  expect(
    screen.queryByRole('button', { name: buttonName })
  ).not.toBeInTheDocument()
  await user.keyboard('{Escape}')
  expect(await screen.findByRole('button', { name: buttonName })).toBeVisible()
})

test('mobile administrator floating refresh yields to bulk selection and returns when cleared', async () => {
  setMobileViewport()
  const user = userEvent.setup()
  mount(<ImageTaskCenter admin />)
  const buttonName = 'Refresh · Media task center'
  expect(await screen.findByRole('button', { name: buttonName })).toBeVisible()
  const selection = await screen.findByRole('checkbox', {
    name: 'Select current page',
  })
  await user.click(selection)
  expect(
    screen.queryByRole('button', { name: buttonName })
  ).not.toBeInTheDocument()
  expect(
    screen.getByRole('button', { name: 'Terminate selected' })
  ).toBeEnabled()
  expect(screen.getByRole('button', { name: 'Refresh' })).toBeVisible()
  await user.click(selection)
  expect(await screen.findByRole('button', { name: buttonName })).toBeVisible()
})

test.each([false, true])(
  'admin=%s manual refresh spins until both task and platform requests finish',
  async (admin) => {
    const user = userEvent.setup()
    mount(<ImageTaskCenter admin={admin} />)
    await screen.findByRole('button', { name: task.id })
    const refresh = screen.getByRole('button', { name: 'Refresh' })
    await waitFor(() => expect(refresh).toBeEnabled())
    let finishTasks!: (value: Awaited<ReturnType<typeof getImageTasks>>) => void
    let finishPlatforms!: (value: string[]) => void
    vi.mocked(getImageTasks).mockReturnValueOnce(
      new Promise((resolve) => {
        finishTasks = resolve
      })
    )
    vi.mocked(getMediaTaskPlatforms).mockReturnValueOnce(
      new Promise((resolve) => {
        finishPlatforms = resolve
      })
    )

    await user.click(refresh)
    await waitFor(() => expect(refresh).toHaveAttribute('aria-busy', 'true'))
    expect(refresh).toBeDisabled()
    expect(refresh.querySelector('svg')).toHaveClass('animate-spin')
    await user.click(refresh)
    expect(getImageTasks).toHaveBeenCalledTimes(2)
    expect(getMediaTaskPlatforms).toHaveBeenCalledTimes(2)

    await act(async () => {
      finishTasks({ items: [task], total: 1, pages: 1, stats: {} })
    })
    expect(refresh).toHaveAttribute('aria-busy', 'true')
    expect(refresh).toBeDisabled()
    await act(async () => {
      finishPlatforms(['azure'])
    })
    await waitFor(() => expect(refresh).toBeEnabled())
    expect(refresh).toHaveAttribute('aria-busy', 'false')
    expect(refresh.querySelector('svg')).not.toHaveClass('animate-spin')
  }
)

test.each(['tasks', 'platforms'])(
  '%s request failure clears manual refresh loading and permits retry',
  async (source) => {
    const user = userEvent.setup()
    mount(<ImageTaskCenter />)
    await screen.findByRole('button', { name: task.id })
    const refresh = screen.getByRole('button', { name: 'Refresh' })
    await waitFor(() => expect(refresh).toBeEnabled())
    let fail!: (reason: Error) => void
    const pending = new Promise<never>((_, reject) => {
      fail = reject
    })
    if (source === 'tasks') {
      vi.mocked(getImageTasks).mockReturnValueOnce(pending)
    } else {
      vi.mocked(getMediaTaskPlatforms).mockReturnValueOnce(pending)
    }

    await user.click(refresh)
    await waitFor(() => expect(refresh).toBeDisabled())
    await act(async () => {
      fail(new Error('Refresh unavailable'))
    })
    await waitFor(() => expect(refresh).toBeEnabled())
    expect(refresh).toHaveAttribute('aria-busy', 'false')
    expect(refresh.querySelector('svg')).not.toHaveClass('animate-spin')
    await user.click(refresh)
    await waitFor(() => expect(getImageTasks).toHaveBeenCalledTimes(3))
    await waitFor(() => expect(refresh).toBeEnabled())
  }
)

test.each([false, true])(
  'admin=%s dark mode auto refresh shows enabled status and a countdown that follows polling',
  async (admin) => {
    if (admin) setMobileViewport()
    mount(<ImageTaskCenter admin={admin} />)
    await screen.findByRole('button', { name: task.id })
    const refresh = screen.getByRole('button', { name: 'Refresh' })
    await waitFor(() => expect(refresh).toBeEnabled())
    const autoRefresh = screen.getByRole('button', { name: 'Auto refresh' })
    expect(autoRefresh).toHaveAttribute('aria-pressed', 'false')
    document.documentElement.classList.add('dark')
    vi.useFakeTimers({ toFake: ['Date', 'setInterval', 'clearInterval'] })
    try {
      fireEvent.click(autoRefresh)
      expect(autoRefresh).toHaveAttribute('aria-pressed', 'true')
      expect(autoRefresh).toHaveClass('bg-primary', 'text-primary-foreground')
      expect(autoRefresh).toHaveTextContent('Auto refresh enabled')
      expect(autoRefresh).toHaveTextContent('10s')
      await act(async () => {
        await vi.advanceTimersByTimeAsync(1000)
      })
      expect(autoRefresh).toHaveTextContent('9s')
      let finishTasks!: (
        value: Awaited<ReturnType<typeof getImageTasks>>
      ) => void
      vi.mocked(getImageTasks).mockReturnValueOnce(
        new Promise((resolve) => {
          finishTasks = resolve
        })
      )
      await act(async () => {
        await vi.advanceTimersByTimeAsync(9000)
      })
      expect(getImageTasks).toHaveBeenCalledTimes(2)
      await waitFor(() =>
        expect(autoRefresh).toHaveTextContent('Refreshing...')
      )
      expect(refresh).toHaveAttribute('aria-busy', 'true')
      await act(async () => {
        finishTasks({ items: [task], total: 1, pages: 1, stats: {} })
      })
      await waitFor(() => expect(autoRefresh).toHaveTextContent('10s'))
      await act(async () => {
        await vi.advanceTimersByTimeAsync(3000)
      })
      expect(autoRefresh).toHaveTextContent('7s')
      fireEvent.click(refresh)
      await waitFor(() => expect(getImageTasks).toHaveBeenCalledTimes(3))
      await waitFor(() => expect(autoRefresh).toHaveTextContent('10s'))

      fireEvent.click(autoRefresh)
      expect(autoRefresh).toHaveAttribute('aria-pressed', 'false')
      expect(autoRefresh).toHaveTextContent(/^Auto refresh$/)
      await act(async () => {
        await vi.advanceTimersByTimeAsync(11000)
      })
      expect(getImageTasks).toHaveBeenCalledTimes(3)
      fireEvent.click(autoRefresh)
      expect(autoRefresh).toHaveTextContent('10s')
      fireEvent.click(autoRefresh)
    } finally {
      document.documentElement.classList.remove('dark')
      vi.useRealTimers()
    }
  }
)

test.each([false, true])(
  'mobile=%s reset clears the platform along with other filters',
  async (mobile) => {
    if (mobile) setMobileViewport()
    const user = userEvent.setup()
    mount(<ImageTaskCenter />)
    await screen.findByRole('button', { name: task.id })
    const platform = screen.getByRole('combobox', { name: 'Platform' })
    await user.click(platform)
    await user.click(screen.getByRole('option', { name: 'OpenAI' }))
    await user.type(screen.getByLabelText('Search tasks'), 'flare')
    await user.click(screen.getByRole('button', { name: 'Apply filters' }))
    await waitFor(() =>
      expect(
        vi.mocked(getImageTasks).mock.calls.at(-1)?.[1].get('platform')
      ).toBe('openai')
    )
    if (mobile) {
      await user.click(screen.getByRole('button', { name: 'More filters' }))
      const dialog = await screen.findByRole('dialog', { name: 'More filters' })
      expect(
        within(dialog).queryByRole('combobox', { name: 'Platform' })
      ).not.toBeInTheDocument()
    }
    await user.click(screen.getByRole('button', { name: 'Reset' }))
    if (mobile) {
      await user.click(screen.getByRole('button', { name: 'Close' }))
      await waitFor(() =>
        expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
      )
    }
    expect(platform).toHaveTextContent('All')
    expect(screen.getByLabelText('Search tasks')).toHaveValue('')
    await waitFor(() =>
      expect(
        vi.mocked(getImageTasks).mock.calls.at(-1)?.[1].has('platform')
      ).toBe(false)
    )
    expect(vi.mocked(getImageTasks).mock.calls.at(-1)?.[1].has('q')).toBe(false)
  }
)

test.each([
  { admin: false, mobile: false },
  { admin: true, mobile: false },
  { admin: false, mobile: true },
  { admin: true, mobile: true },
])(
  'admin=$admin mobile=$mobile keeps dates together outside advanced filters and applies a date-only range',
  async ({ admin, mobile }) => {
    if (mobile) setMobileViewport()
    const user = userEvent.setup()
    mount(<ImageTaskCenter admin={admin} />)
    await screen.findByRole('combobox', { name: 'Platform' })
    expect(screen.getByRole('combobox', { name: 'Status' })).toBeVisible()
    expect(
      screen.queryByRole('combobox', { name: 'Media type' })
    ).not.toBeInTheDocument()
    expect(screen.queryByLabelText('Provider')).not.toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: /^Date Range:/ }))
    const start = screen.getByLabelText('Start date')
    const end = screen.getByLabelText('End date')
    expect(start).toHaveAttribute('type', 'date')
    expect(end).toHaveAttribute('type', 'date')
    fireEvent.change(start, { target: { value: '2026-09-01' } })
    fireEvent.change(end, { target: { value: '2026-09-30' } })
    await user.click(screen.getByRole('button', { name: 'Confirm' }))
    expect(
      screen.getByRole('button', { name: /^Date Range:/ })
    ).toHaveTextContent('2026-09-01 ~ 2026-09-30')
    expect(
      vi.mocked(getImageTasks).mock.calls.at(-1)?.[1].get('start_date')
    ).not.toBe('2026-09-01')
    await user.click(screen.getByRole('button', { name: 'Apply filters' }))
    await waitFor(() =>
      expect(
        vi.mocked(getImageTasks).mock.calls.at(-1)?.[1].get('start_date')
      ).toBe('2026-09-01')
    )
    const params = vi.mocked(getImageTasks).mock.calls.at(-1)?.[1]
    expect(params?.get('end_date')).toBe('2026-09-30')
    expect(params?.get('timezone')).toBe(
      Intl.DateTimeFormat().resolvedOptions().timeZone
    )
    await user.click(screen.getByRole('button', { name: 'More filters' }))
    expect(screen.getByLabelText('Model')).toBeVisible()
    expect(screen.queryByLabelText('Start date')).not.toBeInTheDocument()
    expect(screen.queryByLabelText('End date')).not.toBeInTheDocument()
  }
)

test('date presets keep query drafts until applied, reject reversed dates and reset to today', async () => {
  vi.useFakeTimers({ toFake: ['Date'] })
  vi.setSystemTime(new Date(2026, 9, 13, 12))
  try {
    const user = userEvent.setup()
    mount(<ImageTaskCenter />)
    await screen.findByRole('button', { name: /^Date Range:/ })
    await user.click(screen.getByRole('button', { name: /^Date Range:/ }))
    await user.click(screen.getByRole('button', { name: '7 Days' }))
    expect(
      screen.getByRole('button', { name: /^Date Range:/ })
    ).toHaveTextContent('2026-10-07 ~ 2026-10-13')
    expect(
      vi.mocked(getImageTasks).mock.calls.at(-1)?.[1].get('start_date')
    ).toBe('2026-10-13')
    await user.click(screen.getByRole('button', { name: 'Apply filters' }))
    await waitFor(() =>
      expect(
        vi.mocked(getImageTasks).mock.calls.at(-1)?.[1].get('start_date')
      ).toBe('2026-10-07')
    )
    await user.click(screen.getByRole('button', { name: /^Date Range:/ }))
    fireEvent.change(screen.getByLabelText('Start date'), {
      target: { value: '2026-10-14' },
    })
    expect(screen.getByRole('button', { name: 'Confirm' })).toBeDisabled()
    fireEvent.change(screen.getByLabelText('End date'), {
      target: { value: '2026-10-15' },
    })
    expect(screen.getByRole('button', { name: 'Confirm' })).toBeEnabled()
    await user.keyboard('{Escape}')
    expect(
      screen.getByRole('button', { name: /^Date Range:/ })
    ).toHaveTextContent('2026-10-07 ~ 2026-10-13')
    await user.click(screen.getByRole('button', { name: 'Reset' }))
    expect(
      screen.getByRole('button', { name: /^Date Range:/ })
    ).toHaveTextContent('2026-10-13')
    await waitFor(() =>
      expect(
        vi.mocked(getImageTasks).mock.calls.at(-1)?.[1].get('start_date')
      ).toBe('2026-10-13')
    )
    expect(
      vi.mocked(getImageTasks).mock.calls.at(-1)?.[1].get('end_date')
    ).toBe('2026-10-13')
  } finally {
    vi.useRealTimers()
  }
})

test('keeps primary filters visible while advanced fields start collapsed and preserve drafts', async () => {
  const user = userEvent.setup()
  mount(<ImageTaskCenter admin />)
  await screen.findAllByText('Sunburst upstream')
  expect(screen.getByLabelText('Search tasks')).toBeVisible()
  expect(screen.queryByLabelText('Start date')).not.toBeInTheDocument()
  expect(screen.getByRole('button', { name: /^More filters/ })).toHaveAttribute(
    'aria-expanded',
    'false'
  )
  await user.type(screen.getByLabelText('Search tasks'), 'city')
  await user.click(screen.getByRole('button', { name: /^More filters/ }))
  await user.type(screen.getByLabelText('Model'), 'flare')
  await user.click(screen.getByRole('button', { name: /^More filters/ }))
  expect(screen.getByLabelText('Search tasks')).toHaveValue('city')
  await user.click(screen.getByRole('button', { name: 'Apply filters' }))
  await waitFor(() =>
    expect(vi.mocked(getImageTasks).mock.calls.at(-1)?.[1].get('model')).toBe(
      'flare'
    )
  )
  await user.click(screen.getByRole('button', { name: /^More filters/ }))
  expect(screen.getByLabelText('Model')).toHaveValue('flare')
  const statistics = screen.getByLabelText('Task statistics')
  expect(statistics).toHaveTextContent('Success rate96.0%')
  expect(statistics).toHaveTextContent('Processing35')
  expect(statistics).not.toHaveTextContent('500')
  expect(screen.getAllByText('Creative account').length).toBeGreaterThan(0)
  expect(screen.getAllByText('Image to image').length).toBeGreaterThan(0)
})

test.each([
  { admin: false, mobile: false },
  { admin: true, mobile: false },
  { admin: false, mobile: true },
  { admin: true, mobile: true },
])(
  'admin=$admin mobile=$mobile search describes available fields and submits a trimmed keyword on Enter',
  async ({ admin, mobile }) => {
    if (mobile) setMobileViewport()
    const user = userEvent.setup()
    mount(<ImageTaskCenter admin={admin} />)
    await waitFor(() => expect(getImageTasks).toHaveBeenCalled())
    const search = screen.getByRole('textbox', { name: 'Search tasks' })
    expect(search).toHaveAttribute(
      'placeholder',
      admin
        ? 'Task ID, model, specifications or channel name'
        : 'Task ID, model or specifications'
    )
    await user.type(search, '  2K  {Enter}')
    await waitFor(() =>
      expect(vi.mocked(getImageTasks).mock.calls.at(-1)?.[1].get('q')).toBe(
        '2K'
      )
    )
    expect(vi.mocked(getImageTasks).mock.calls.at(-1)?.[0]).toBe(admin)
    expect(vi.mocked(getImageTasks).mock.calls.at(-1)?.[1].get('page')).toBe(
      '1'
    )
  }
)

test.each([
  { admin: false, scope: 'user' },
  { admin: true, scope: 'administrator' },
])(
  'mobile $scope cards group long model names, cost and actions without desktop column widths',
  async ({ admin }) => {
    setMobileViewport()
    const longModel = `gpt-image-${'extended-model-name-'.repeat(6)}`
    vi.mocked(getImageTasks).mockResolvedValue({
      items: [
        { ...task, model: longModel, billing_status: 'settled', cost: 0.12 },
      ],
      total: 1,
      pages: 1,
      stats: {},
    })
    const user = userEvent.setup()
    mount(<ImageTaskCenter admin={admin} />)
    const card = await screen.findByRole('article', { name: task.id })
    const model = within(card).getByRole('heading', { name: longModel })
    expect(model).toHaveClass('wrap-anywhere')
    expect(
      within(card).getByText('Actual cost').nextElementSibling
    ).toHaveTextContent('$0.12')
    expect(card).not.toHaveTextContent('US$')
    expect(
      within(card).getByRole('progressbar', { name: 'Progress' })
    ).toHaveAttribute('aria-valuenow', '20')
    if (admin) {
      expect(within(card).getByText('Creative account')).toBeVisible()
      expect(within(card).getByText('Sunburst upstream')).toBeVisible()
    } else {
      expect(
        within(card).queryByText('Creative account')
      ).not.toBeInTheDocument()
      expect(
        within(card).queryByText('Sunburst upstream')
      ).not.toBeInTheDocument()
    }
    await user.click(within(card).getByRole('button', { name: 'Copy Task ID' }))
    expect(await navigator.clipboard.readText()).toBe(task.id)
    await user.click(within(card).getByRole('button', { name: 'View' }))
    expect(
      await screen.findByRole('dialog', { name: 'Media task details' })
    ).toBeVisible()
  }
)

test('mobile administrator cards highlight selection and enable the existing batch action', async () => {
  setMobileViewport()
  const user = userEvent.setup()
  mount(<ImageTaskCenter admin />)
  const card = await screen.findByRole('article', { name: task.id })
  const selectedAction = screen.getByRole('button', {
    name: 'Terminate selected',
  })
  expect(
    screen.getByLabelText('Search tasks').closest('.overflow-auto')
  ).toContainElement(selectedAction)
  expect(selectedAction).toBeDisabled()
  await user.click(
    within(card).getByRole('checkbox', { name: `Select task ${task.id}` })
  )
  expect(card.closest('[data-slot=data-table-card]')).toHaveAttribute(
    'data-state',
    'selected'
  )
  expect(selectedAction).toBeEnabled()
  await user.click(
    screen.getByRole('checkbox', { name: 'Select current page' })
  )
  expect(within(card).getByRole('checkbox')).not.toBeChecked()
})

test('mobile cards distinguish pending charges from a completed free task', async () => {
  setMobileViewport()
  vi.mocked(getImageTasks).mockResolvedValue({
    items: [
      task,
      {
        ...task,
        id: 'asyncimg_free',
        status: 'succeeded',
        billing_status: 'not_billable',
        progress: 100,
      },
    ],
    total: 2,
    pages: 1,
    stats: {},
  })
  mount(<ImageTaskCenter />)
  const pending = await screen.findByRole('article', { name: task.id })
  const free = screen.getByRole('article', { name: 'asyncimg_free' })
  expect(
    within(pending).getByText('Actual cost').nextElementSibling
  ).toHaveTextContent('—')
  expect(
    within(free).getByText('Actual cost').nextElementSibling
  ).toHaveTextContent('$0')
  expect(within(free).getByText('No charge')).toBeVisible()
  expect(within(free).queryByRole('progressbar')).not.toBeInTheDocument()
})

test('mobile tasks with no results retain the shared empty state without selection controls', async () => {
  setMobileViewport()
  vi.mocked(getImageTasks).mockResolvedValue({
    items: [],
    total: 0,
    pages: 0,
    stats: {},
  })
  mount(<ImageTaskCenter admin />)
  expect(await screen.findByText('No image tasks found')).toBeVisible()
  expect(
    screen.getByText('Adjust filters or create an image from the workbench.')
  ).toBeVisible()
  expect(screen.queryByRole('article')).not.toBeInTheDocument()
  expect(screen.queryByRole('checkbox')).not.toBeInTheDocument()
})

test.each([
  { admin: false, scope: 'user' },
  { admin: true, scope: 'administrator' },
])(
  'mobile $scope tasks scroll with their search and statistics',
  async ({ admin }) => {
    setMobileViewport()
    mount(<ImageTaskCenter admin={admin} />)
    const taskLink = await screen.findByRole('button', { name: task.id })
    const pageScroll = screen
      .getByLabelText('Search tasks')
      .closest<HTMLElement>('.overflow-auto')

    expect(pageScroll).toContainElement(
      screen.getByLabelText('Task statistics')
    )
    expect(pageScroll).toContainElement(taskLink)
    expect(
      screen.getByRole('button', { name: 'Go to next page' })
    ).toBeVisible()
  }
)

test.each([
  { admin: false, scope: 'user' },
  { admin: true, scope: 'administrator' },
])(
  'mobile $scope filters apply from the dialog footer and return to page one',
  async ({ admin }) => {
    setMobileViewport()
    vi.mocked(getImageTasks).mockResolvedValue({
      items: [task],
      total: 45,
      pages: 3,
      stats: {},
    })
    const user = userEvent.setup()
    mount(<ImageTaskCenter admin={admin} />)
    await screen.findByRole('button', { name: task.id })
    expect(screen.queryByRole('combobox', { name: 'Status' })).toBeVisible()
    await user.click(screen.getByRole('button', { name: 'Go to next page' }))
    await waitFor(() =>
      expect(vi.mocked(getImageTasks).mock.calls.at(-1)?.[1].get('page')).toBe(
        '2'
      )
    )
    await user.type(screen.getByLabelText('Search tasks'), 'city')
    await user.click(screen.getByRole('button', { name: /^Date Range:/ }))
    fireEvent.change(screen.getByLabelText('Start date'), {
      target: { value: '2026-10-01' },
    })
    await user.click(screen.getByRole('button', { name: 'Confirm' }))
    await user.click(screen.getByRole('button', { name: 'More filters' }))
    const dialog = await screen.findByRole('dialog', { name: 'More filters' })
    const model = within(dialog).getByLabelText('Model')
    const apply = within(dialog).getByRole('button', { name: 'Apply filters' })
    const fieldsScroll = model.closest<HTMLElement>('.overflow-y-auto')
    expect(fieldsScroll).toContainElement(model)
    expect(fieldsScroll).not.toContainElement(apply)
    expect(within(dialog).getByRole('button', { name: 'Close' })).toBeVisible()
    await user.type(model, 'flare')
    await user.click(
      within(dialog).getByRole('combobox', { name: 'Media type' })
    )
    await user.click(await screen.findByRole('option', { name: 'Video' }))
    expect(
      within(dialog).queryByLabelText('Start date')
    ).not.toBeInTheDocument()
    await user.click(apply)

    await waitFor(() =>
      expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    )
    await waitFor(() =>
      expect(vi.mocked(getImageTasks).mock.calls.at(-1)?.[1].get('model')).toBe(
        'flare'
      )
    )
    const request = vi.mocked(getImageTasks).mock.calls.at(-1)
    expect(request?.[0]).toBe(admin)
    expect(request?.[1].get('q')).toBe('city')
    expect(request?.[1].get('media_type')).toBe('video')
    expect(request?.[1].get('start_date')).toBe('2026-10-01')
    expect(request?.[1].get('page')).toBe('1')
    expect(screen.getByRole('button', { name: 'More filters 2' })).toBeVisible()
  }
)

test.each([
  { admin: false, scope: 'user' },
  { admin: true, scope: 'administrator' },
])(
  'mobile $scope filters preserve dismissed drafts and reset them when reopened',
  async ({ admin }) => {
    setMobileViewport()
    const user = userEvent.setup()
    mount(<ImageTaskCenter admin={admin} />)
    await screen.findByRole('button', { name: task.id })
    await user.type(screen.getByLabelText('Search tasks'), 'city')
    await user.click(screen.getByRole('button', { name: 'More filters' }))
    const dialog = await screen.findByRole('dialog', { name: 'More filters' })
    await user.type(within(dialog).getByLabelText('Model'), 'flare')
    await user.click(within(dialog).getByRole('button', { name: 'Close' }))
    await waitFor(() =>
      expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    )
    expect(
      vi.mocked(getImageTasks).mock.calls.at(-1)?.[1].get('model')
    ).toBeNull()
    await user.click(screen.getByRole('button', { name: 'More filters' }))
    const reopened = await screen.findByRole('dialog', { name: 'More filters' })
    expect(within(reopened).getByLabelText('Model')).toHaveValue('flare')
    await user.click(within(reopened).getByRole('button', { name: 'Reset' }))
    expect(within(reopened).getByLabelText('Model')).toHaveValue('')
    await user.keyboard('{Escape}')
    await waitFor(() =>
      expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    )
    expect(screen.getByLabelText('Search tasks')).toHaveValue('')
    expect(
      screen.getByRole('button', { name: 'Go to next page' })
    ).toBeVisible()
  }
)

test.each([
  { status: 'invoking', media_type: 'image' },
  { status: 'execution_unknown', media_type: 'image' },
  { status: 'execution_unknown', media_type: 'video' },
] as const)(
  'terminates one fixed $media_type $status task after confirmation and retains the dialog when the request fails',
  async ({ status, media_type }) => {
    const user = userEvent.setup()
    const id = media_type === 'video' ? 'video_unknown_execution' : task.id
    vi.mocked(getImageTasks).mockResolvedValue({
      items: [{ ...task, id, task_id: id, status, media_type }],
      total: 1,
      pages: 1,
      stats: {},
    })
    mount(<ImageTaskCenter admin />)
    const terminate = (
      await screen.findAllByRole('button', { name: 'Terminate' })
    )[0]
    await user.click(terminate)
    expect(imageRequest).not.toHaveBeenCalled()
    const confirm = await screen.findByRole('alertdialog', {
      name: 'Terminate media tasks',
    })
    if (status === 'execution_unknown') {
      expect(confirm).toHaveTextContent(
        'Verify the upstream task before continuing. This stops local processing without cancelling the upstream task. Reserved quota is released for images; video billing remains unchanged.'
      )
    }
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
      `/api/admin/media-tasks/${id}/terminate`,
      'POST',
      status === 'execution_unknown' ? { confirm_upstream_checked: true } : {}
    )
    expect(getImageTasks).toHaveBeenCalledTimes(2)
  }
)

test('unknown video details offer termination with upstream verification', async () => {
  const onManage = vi.fn()
  vi.mocked(getImageTask).mockResolvedValue({
    ...detail,
    task: { ...task, media_type: 'video', status: 'execution_unknown' },
    results: [],
  })
  mount(
    <ImageTaskDetails
      id={task.id}
      admin
      userId={101}
      canManage
      onClose={vi.fn()}
      onManage={onManage}
    />
  )
  const user = userEvent.setup()
  await user.click(await screen.findByRole('button', { name: 'Terminate' }))
  expect(onManage).toHaveBeenCalledWith('terminate', true)
})

test.each([
  { admin: false, mobile: false },
  { admin: true, mobile: false },
  { admin: false, mobile: true },
])(
  'video details show requested duration and reference links and open original parameters on demand (admin=$admin mobile=$mobile)',
  async ({ admin, mobile }) => {
    if (mobile) setMobileViewport()
    const parameters = {
      model: 'video-model',
      prompt: 'Continue the camera movement from the reference clip',
      seconds: 8,
      seed: 0,
      generate_audio: false,
    }
    vi.mocked(getImageTask).mockResolvedValue({
      ...detail,
      task: {
        ...task,
        media_type: 'video',
        requested_seconds: 8,
        request_parameters: parameters,
        reference_media: [
          { type: 'image', url: 'https://example.com/first-frame.png' },
          { type: 'video', url: 'https://example.com/source.mp4' },
          { type: 'audio', url: 'https://example.com/voice.mp3' },
        ],
      },
      results: [],
    })
    mount(
      <ImageTaskDetails
        id={task.id}
        admin={admin}
        userId={101}
        canManage={false}
        onClose={vi.fn()}
        onManage={vi.fn()}
      />
    )
    const user = userEvent.setup()
    const details = await screen.findByRole('dialog', {
      name: 'Media task details',
    })
    expect(await within(details).findByText('Requested duration')).toBeVisible()
    expect(within(details).getByText('8s')).toBeVisible()
    expect(within(details).getByText('Time spent')).toBeVisible()
    for (const reference of [
      {
        label: 'Reference image 1',
        url: 'https://example.com/first-frame.png',
      },
      { label: 'Reference video 2', url: 'https://example.com/source.mp4' },
      { label: 'Reference audio 3', url: 'https://example.com/voice.mp3' },
    ]) {
      expect(
        within(details).getByRole('button', {
          name: `Copy URL · ${reference.label}`,
        })
      ).toHaveTextContent(reference.url)
    }
    expect(details.querySelector('img, video, audio')).toBeNull()
    expect(imageRequest).not.toHaveBeenCalled()
    expect(screen.queryByLabelText('Original request parameters')).toBeNull()
    const trigger = within(details).getByRole('button', {
      name: 'View request parameters',
    })
    await user.click(trigger)
    const parametersDialog = await screen.findByRole('dialog', {
      name: 'Original request parameters',
    })
    expect(
      within(parametersDialog).getByLabelText('Original request parameters')
    ).toHaveTextContent('"generate_audio": false')
    expect(
      within(parametersDialog).getByLabelText('Original request parameters')
    ).toHaveTextContent('"seed": 0')
    await user.click(
      within(parametersDialog).getByRole('button', {
        name: 'Copy request parameters',
      })
    )
    expect(await navigator.clipboard.readText()).toBe(
      JSON.stringify(parameters, null, 2)
    )
    await user.keyboard('{Escape}')
    await waitFor(() =>
      expect(
        screen.queryByRole('dialog', { name: 'Original request parameters' })
      ).not.toBeInTheDocument()
    )
    expect(details).toBeVisible()
    await user.click(
      within(details).getByRole('button', {
        name: 'Copy all reference media URLs',
      })
    )
    expect(await navigator.clipboard.readText()).toBe(
      'https://example.com/first-frame.png\nhttps://example.com/source.mp4\nhttps://example.com/voice.mp3'
    )
  }
)

test('video details leave missing historical request data unavailable', async () => {
  vi.mocked(getImageTask).mockResolvedValue({
    ...detail,
    task: { ...task, media_type: 'video' },
    results: [],
  })
  mount(
    <ImageTaskDetails
      id={task.id}
      admin
      userId={101}
      canManage={false}
      onClose={vi.fn()}
      onManage={vi.fn()}
    />
  )
  expect(await screen.findByText('Requested duration')).toBeVisible()
  expect(
    screen.getByRole('button', { name: 'View request parameters' })
  ).toBeDisabled()
  expect(
    screen.getByText(
      'Original request parameters are unavailable for this task.'
    )
  ).toBeVisible()
  expect(screen.queryByText('Reference media')).not.toBeInTheDocument()
  expect(
    screen.queryByRole('dialog', { name: 'Original request parameters' })
  ).not.toBeInTheDocument()
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
  expect(screen.getByText('HTTP status: 503')).toBeVisible()
  expect(
    screen.getByText('Upstream image service temporarily unavailable')
  ).toBeVisible()
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

test.each([
  { videoOnly: false, mobile: false },
  { videoOnly: true, mobile: false },
  { videoOnly: true, mobile: true },
])(
  'batch termination confirms upstream verification when videoOnly=$videoOnly mobile=$mobile',
  async ({ videoOnly, mobile }) => {
    if (mobile) setMobileViewport()
    const user = userEvent.setup()
    const id = videoOnly ? 'video_unknown_execution' : 'asyncimg_unknown'
    vi.mocked(getImageTasks).mockResolvedValue({
      items: [
        { ...task, can_terminate: !videoOnly },
        {
          ...task,
          id,
          task_id: id,
          media_type: videoOnly ? 'video' : 'image',
          status: 'execution_unknown',
        },
      ],
      total: 2,
      pages: 1,
      stats: {},
    })
    mount(<ImageTaskCenter admin />)
    await user.click(
      await screen.findByRole('button', {
        name: `Terminate current page (${videoOnly ? 1 : 2})`,
      })
    )
    const confirmation = await screen.findByRole('alertdialog', {
      name: 'Terminate media tasks',
    })
    expect(confirmation).toHaveTextContent(
      'Verify the upstream task before continuing.'
    )
    expect(confirmation).toHaveTextContent('video billing remains unchanged')
    expect(imageRequest).not.toHaveBeenCalled()
    vi.mocked(imageRequest).mockResolvedValue({})
    vi.mocked(getImageTasks).mockResolvedValue({
      items: [
        { ...task, id, task_id: id, status: 'failed', can_terminate: false },
      ],
      total: 1,
      pages: 1,
      stats: {},
    })

    await user.click(
      within(confirmation).getByRole('button', { name: 'Continue' })
    )

    await waitFor(() =>
      expect(screen.queryByRole('alertdialog')).not.toBeInTheDocument()
    )
    expect(imageRequest).toHaveBeenCalledWith(
      `/api/admin/media-tasks/${id}/terminate`,
      'POST',
      { confirm_upstream_checked: true }
    )
    expect(
      await screen.findByRole('button', { name: 'Terminate current page (0)' })
    ).toBeDisabled()
    expect(
      screen.queryByRole('button', { name: 'Terminate' })
    ).not.toBeInTheDocument()
  }
)

test.each([
  { failure: false, dismiss: false },
  { failure: true, dismiss: false },
  { failure: true, dismiss: true },
])(
  'batch termination failure=$failure dismiss=$dismiss shows a transient summary and retains failure details',
  async ({ failure, dismiss }) => {
    const user = userEvent.setup()
    vi.mocked(getImageTasks).mockResolvedValue({
      items: [
        task,
        { ...task, id: 'asyncimg_second', task_id: 'asyncimg_second' },
      ],
      total: 2,
      pages: 1,
      stats: {},
    })
    vi.mocked(imageRequest).mockResolvedValueOnce({})
    if (failure) {
      vi.mocked(imageRequest).mockRejectedValueOnce(
        new Error('Termination unavailable')
      )
    } else {
      vi.mocked(imageRequest).mockResolvedValueOnce({})
    }
    mount(
      <>
        <ImageTaskCenter admin />
        <Toaster />
      </>
    )
    await user.click(
      await screen.findByRole('button', { name: 'Terminate current page (2)' })
    )
    const confirmation = await screen.findByRole('alertdialog', {
      name: 'Terminate media tasks',
    })
    vi.useFakeTimers({ toFake: ['Date', 'setTimeout', 'clearTimeout'] })
    try {
      fireEvent.click(
        within(confirmation).getByRole('button', { name: 'Continue' })
      )
      await act(async () => {
        await vi.advanceTimersByTimeAsync(50)
      })
      const summary = failure
        ? 'Task termination: 1 succeeded, 1 failed'
        : 'Tasks terminated: 2'
      expect(screen.getByText(summary)).toBeVisible()
      if (failure) {
        expect(
          screen.getByText('asyncimg_second: Termination unavailable')
        ).toBeVisible()
      }
      if (dismiss) {
        fireEvent.click(screen.getByRole('button', { name: 'Close toast' }))
      }
      await act(async () => {
        await vi.advanceTimersByTimeAsync(dismiss ? 400 : 8500)
      })
      expect(screen.queryByText(summary)).not.toBeInTheDocument()
      expect(
        screen.queryByText('asyncimg_second: Termination unavailable')
      ).not.toBeInTheDocument()
      expect(screen.queryByRole('alertdialog')).not.toBeInTheDocument()
    } finally {
      toast.dismiss()
      vi.useRealTimers()
    }
  }
)

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
    await screen.findByRole('button', { name: 'Download image' })
  ).toBeVisible()
  expect(
    screen.queryByRole('button', { name: 'Archive to server' })
  ).not.toBeInTheDocument()
  expect(getImageTask).toHaveBeenCalledWith(
    false,
    task.id,
    expect.any(AbortSignal)
  )
})

test.each([false, true])(
  'administrators with read permission cannot select or terminate tasks (mobile=%s)',
  async (mobile) => {
    if (mobile) setMobileViewport()
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
  }
)

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

test.each([
  { admin: false, canManage: false, canResume: true, allowed: true },
  { admin: true, canManage: true, canResume: true, allowed: true },
  { admin: true, canManage: false, canResume: true, allowed: false },
  { admin: false, canManage: false, canResume: false, allowed: false },
])(
  'video upstream fallback refresh uses recovery only when allowed: %j',
  async ({ admin, canManage, canResume, allowed }) => {
    const user = userEvent.setup()
    const onManage = vi.fn()
    const link = 'https://cdn.example.com/video.mp4?signature=for-owner'
    vi.mocked(getImageTask).mockResolvedValue({
      task: {
        ...task,
        media_type: 'video',
        request_type: 'text_to_video',
        status: 'succeeded',
        storage_status: 'upstream',
        billing_status: 'settled',
        can_resume: canResume,
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
        admin={admin}
        userId={101}
        canManage={canManage}
        onClose={vi.fn()}
        onManage={onManage}
      />
    )
    await screen.findByText('Upstream link')
    expect(document.querySelector('video')).toHaveAttribute('src', link)
    const refresh = screen.queryByRole('button', { name: 'Refresh link' })
    if (allowed) {
      expect(refresh).toBeVisible()
      await user.click(screen.getByRole('button', { name: 'Refresh link' }))
      expect(onManage).toHaveBeenCalledWith('resume')
    } else {
      expect(refresh).not.toBeInTheDocument()
    }
    expect(screen.getByText('Download').closest('a')).toHaveAttribute(
      'href',
      link
    )
  }
)
