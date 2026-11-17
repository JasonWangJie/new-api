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
  cleanup,
  render,
  screen,
  waitFor,
  within,
} from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { createInstance } from 'i18next'
import { I18nextProvider } from 'react-i18next'
import { afterEach, expect, test, vi } from 'vitest'

import zhLocale from '@/i18n/locales/zh.json'

import { imageRequest } from '../api'
import {
  ImageCleanupConfigForm,
  ImageCleanupPanel,
} from '../components/image-cleanup-settings'
import type { CleanupConfig, CleanupJob } from '../lib/image-cleanup'
import type { ImageStorageProfile } from '../types'

vi.mock('../api', () => ({ imageRequest: vi.fn() }))
const permissions = vi.hoisted(() => ({ manage: true }))
vi.mock('@/lib/admin-permissions', () => ({
  hasPermission: (_user: unknown, _resource: string, action: string) =>
    action === 'read' || permissions.manage,
}))
afterEach(() => {
  cleanup()
  vi.useRealTimers()
  vi.resetAllMocks()
  permissions.manage = true
})

const profiles: ImageStorageProfile[] = [
  {
    profile_id: 'revision-1',
    channel_id: 'r2-main',
    name: 'Main R2',
    class: 'temporary',
    backend: 's3',
    provider: 'r2',
    root: '',
    endpoint: '',
    bucket: 'images',
    region: 'auto',
    prefix: '',
    active: true,
    path_style: false,
  },
]
const job: CleanupJob = {
  job_id: 'cleanup-1',
  source: 'manual',
  status: 'preview_ready',
  scope: 'all',
  filters: '{}',
  cutoff_at: 1700000000,
  scanned: 12,
  matched_items: 3,
  matched_bytes: 1048576,
  protected_items: 9,
  reference_skipped: 4,
  retention_skipped: 3,
  location_skipped: 2,
  deleted: 0,
  skipped: 0,
  deleted_bytes: 0,
  errors: 0,
  processed: 0,
  total: 3,
  created_at: 1700000001,
  next_attempt_at: 0,
  preview_fingerprint: 'preview-token',
}
const settings: CleanupConfig = {
  revision: 3,
  enabled: false,
  concurrency: 1,
  requests_per_second: 2,
  batch_size: 50,
  scan_pages_per_second: 1,
  request_timeout_seconds: 15,
  cpu_pause_percent: 70,
  memory_pause_percent: 80,
  policies: [],
}
function renderPanel() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
  render(
    <QueryClientProvider client={client}>
      <ImageCleanupPanel profiles={profiles} />
    </QueryClientProvider>
  )
  return client
}
function mockJobs() {
  vi.mocked(imageRequest).mockImplementation(async (url) => {
    if (url.includes('?')) return { items: [], next_cursor: '' }
    return job
  })
}

test('manual cleanup freezes the selected age and channel in the confirmed preview', async () => {
  mockJobs()
  renderPanel()
  expect(screen.getByText(/New previews use image age/)).toHaveTextContent(
    'existing previews keep their original rules'
  )
  const user = userEvent.setup()
  await user.click(screen.getByRole('combobox', { name: 'Storage channel' }))
  await user.click(screen.getByRole('option', { name: 'Main R2' }))
  await user.click(screen.getByRole('combobox', { name: 'Age unit' }))
  await user.click(screen.getByRole('option', { name: 'Hours' }))
  await user.clear(screen.getByRole('spinbutton', { name: 'Older than' }))
  await user.type(screen.getByRole('spinbutton', { name: 'Older than' }), '48')
  await user.click(screen.getByRole('button', { name: 'Preview cleanup' }))
  await user.click(
    await screen.findByRole('button', { name: 'Create cleanup job' })
  )
  const dialog = screen.getByRole('alertdialog')
  expect(
    within(dialog).getByText('Protected files: 9', { exact: false })
  ).toBeVisible()
  expect(dialog).toHaveTextContent('Live references: 4')
  expect(dialog).toHaveTextContent('Within retention period: 3')
  expect(dialog).toHaveTextContent('Shared or unknown storage location: 2')
  await user.click(within(dialog).getByRole('button', { name: 'Continue' }))
  await waitFor(() =>
    expect(imageRequest).toHaveBeenCalledWith(
      '/api/admin/image-library/cleanup-jobs',
      'POST',
      {
        scope: 'all',
        filters: { channel_id: 'r2-main', older_than: 48, age_unit: 'hours' },
        preview_fingerprint: 'preview-token',
      }
    )
  )
})

test('changing the cutoff invalidates the ready preview before deletion can be requested', async () => {
  mockJobs()
  renderPanel()
  const user = userEvent.setup()
  await user.click(screen.getByRole('combobox', { name: 'Storage channel' }))
  await user.click(screen.getByRole('option', { name: 'Main R2' }))
  await user.click(screen.getByRole('button', { name: 'Preview cleanup' }))
  expect(
    await screen.findByRole('button', { name: 'Create cleanup job' })
  ).toBeVisible()
  await user.clear(screen.getByRole('spinbutton', { name: 'Older than' }))
  await user.type(screen.getByRole('spinbutton', { name: 'Older than' }), '90')
  expect(
    screen.queryByRole('button', { name: 'Create cleanup job' })
  ).not.toBeInTheDocument()
  expect(
    vi
      .mocked(imageRequest)
      .mock.calls.some(
        ([url, method]) =>
          url === '/api/admin/image-library/cleanup-jobs' && method === 'POST'
      )
  ).toBe(false)
})

test('storage cleanup shows only the latest five jobs without paging and refresh keeps the separate preview', async () => {
  let items = Array.from({ length: 7 }, (_, index) => ({
    ...job,
    job_id: `history-${7 - index}`,
    source: index === 0 ? 'manual_age' : job.source,
    status: 'completed',
  }))
  vi.mocked(imageRequest).mockImplementation(async (url) => {
    if (url.includes('?')) return { items, next_cursor: 'older-history' }
    return job
  })
  renderPanel()
  const table = await screen.findByRole('table')
  await within(table).findByText('history-7')
  expect(within(table).getAllByRole('row')).toHaveLength(6)
  expect(within(table).getAllByText('Manual')).toHaveLength(5)
  expect(within(table).getByText('history-3')).toBeVisible()
  expect(within(table).queryByText('history-2')).not.toBeInTheDocument()
  expect(within(table).queryByText('history-1')).not.toBeInTheDocument()
  expect(
    screen.queryByRole('button', { name: 'Previous' })
  ).not.toBeInTheDocument()
  expect(screen.queryByRole('button', { name: 'Next' })).not.toBeInTheDocument()
  expect(
    screen.getByText('Showing the 5 most recent storage cleanup jobs.')
  ).toBeVisible()
  expect(
    screen.getByText(
      'Finished job records are automatically removed after 24 hours. Unfinished jobs and jobs awaiting deletion verification are kept.'
    )
  ).toBeVisible()
  const user = userEvent.setup()
  await user.click(screen.getByRole('combobox', { name: 'Storage channel' }))
  await user.click(screen.getByRole('option', { name: 'Main R2' }))
  await user.click(screen.getByRole('button', { name: 'Preview cleanup' }))
  expect(
    await screen.findByRole('button', { name: 'Create cleanup job' })
  ).toBeVisible()
  items = [{ ...job, job_id: 'history-8', status: 'completed' }, ...items]
  await user.click(screen.getByRole('button', { name: 'Refresh' }))
  expect(await within(table).findByText('history-8')).toBeVisible()
  expect(within(table).getAllByRole('row')).toHaveLength(6)
  expect(within(table).queryByText('history-3')).not.toBeInTheDocument()
  expect(
    screen.getByRole('button', { name: 'Create cleanup job' })
  ).toBeVisible()
  const listCalls = vi
    .mocked(imageRequest)
    .mock.calls.filter(([url]) => url.includes('?'))
  expect(listCalls.length).toBeGreaterThanOrEqual(2)
  expect(
    listCalls.every(
      ([url]) => url === '/api/admin/image-library/cleanup-jobs?limit=5'
    )
  ).toBe(true)
  expect(imageRequest).toHaveBeenCalledWith(
    '/api/admin/image-library/cleanup-jobs/cleanup-1',
    'GET',
    undefined,
    expect.any(AbortSignal)
  )
})

test('idle storage cleanup history refreshes after a minute to remove server-expired records', async () => {
  vi.useFakeTimers()
  let items: CleanupJob[] = [{ ...job, status: 'completed' }]
  vi.mocked(imageRequest).mockImplementation(async () => ({
    items,
    next_cursor: '',
  }))
  renderPanel()
  await act(async () => {
    await vi.advanceTimersByTimeAsync(1)
  })
  expect(screen.getByText('cleanup-1')).toBeVisible()
  items = []
  await act(async () => {
    await vi.advanceTimersByTimeAsync(60000)
  })
  expect(screen.queryByText('cleanup-1')).not.toBeInTheDocument()
  expect(screen.getByText('No items found')).toBeVisible()
  expect(
    vi
      .mocked(imageRequest)
      .mock.calls.every(
        ([url, method]) =>
          url === '/api/admin/image-library/cleanup-jobs?limit=5' &&
          method === 'GET'
      )
  ).toBe(true)
})

test('scheduled and manual cleanup retain removed channels and prefer current names without duplicate revisions', async () => {
  mockJobs()
  const revisions: ImageStorageProfile[] = [
    {
      ...profiles[0],
      profile_id: 'removed-new',
      channel_id: 'removed-r2',
      name: 'Removed R2',
      current_revision: false,
    },
    {
      ...profiles[0],
      profile_id: 'old-main',
      name: 'Old main name',
      current_revision: false,
    },
    {
      ...profiles[0],
      profile_id: 'removed-old',
      channel_id: 'removed-r2',
      name: 'Old removed name',
      current_revision: false,
    },
    { ...profiles[0], current_revision: true },
  ]
  render(
    <QueryClientProvider client={new QueryClient()}>
      <ImageCleanupConfigForm
        initial={{
          ...settings,
          policies: [
            {
              id: 'retained-policy',
              name: 'Existing cleanup',
              enabled: true,
              channel_id: 'removed-r2',
              older_than: 180,
              age_unit: 'days',
              schedule_type: 'daily',
              interval_minutes: 1440,
              daily_time: '03:00',
              timezone: 'Asia/Shanghai',
            },
          ],
        }}
        profiles={revisions}
        canManage
        onSaved={vi.fn()}
      />
      <ImageCleanupPanel profiles={revisions} />
    </QueryClientProvider>
  )
  const user = userEvent.setup()
  const selectors = screen.getAllByRole('combobox', { name: 'Storage channel' })
  expect(selectors[0]).toHaveTextContent('Removed R2')
  for (const selector of selectors) {
    await user.click(selector)
    expect(screen.getAllByRole('option')).toHaveLength(2)
    expect(screen.getByRole('option', { name: 'Main R2' })).toBeVisible()
    expect(
      screen.queryByRole('option', { name: 'Old main name' })
    ).not.toBeInTheDocument()
    await user.click(screen.getByRole('option', { name: 'Removed R2' }))
  }
  await user.click(screen.getByRole('button', { name: 'Preview cleanup' }))
  await waitFor(() =>
    expect(imageRequest).toHaveBeenCalledWith(
      '/api/admin/image-library/cleanup-jobs/preview',
      'POST',
      {
        scope: 'all',
        filters: {
          channel_id: 'removed-r2',
          older_than: 180,
          age_unit: 'days',
        },
      }
    )
  )
})

test('cancelling the confirmation does not create a cleanup job', async () => {
  mockJobs()
  renderPanel()
  const user = userEvent.setup()
  await user.click(screen.getByRole('combobox', { name: 'Storage channel' }))
  await user.click(screen.getByRole('option', { name: 'Main R2' }))
  await user.click(screen.getByRole('button', { name: 'Preview cleanup' }))
  await user.click(
    await screen.findByRole('button', { name: 'Create cleanup job' })
  )
  await user.click(
    within(screen.getByRole('alertdialog')).getByRole('button', {
      name: 'Cancel',
    })
  )
  expect(
    vi
      .mocked(imageRequest)
      .mock.calls.some(
        ([url, method]) =>
          url === '/api/admin/image-library/cleanup-jobs' && method === 'POST'
      )
  ).toBe(false)
})

test('a running job can be paused and cancellation explains completed deletions', async () => {
  let status = 'running'
  vi.mocked(imageRequest).mockImplementation(async (url) => {
    if (url.endsWith('/pause')) status = 'paused'
    if (url.includes('?')) {
      return { items: [{ ...job, status }], next_cursor: '' }
    }
    return { ...job, status }
  })
  renderPanel()
  const user = userEvent.setup()
  await user.click(await screen.findByRole('button', { name: 'Pause' }))
  expect(await screen.findByRole('button', { name: 'Resume' })).toBeVisible()
  await user.click(screen.getByRole('button', { name: 'Cancel cleanup' }))
  expect(screen.getByRole('alertdialog')).toHaveTextContent(
    'Requests already sent may finish'
  )
  await user.click(
    within(screen.getByRole('alertdialog')).getByRole('button', {
      name: 'Continue',
    })
  )
  await waitFor(() =>
    expect(imageRequest).toHaveBeenCalledWith(
      '/api/admin/image-library/cleanup-jobs/cleanup-1/cancel',
      'POST',
      {}
    )
  )
})

test('read-only administrators see job results without execution controls', async () => {
  permissions.manage = false
  vi.mocked(imageRequest).mockResolvedValue({
    items: [
      {
        ...job,
        status: 'completed_with_errors',
        errors: 1,
        last_error: 'Object storage unavailable',
      },
    ],
    next_cursor: '',
  })
  renderPanel()
  expect(await screen.findByText('Object storage unavailable')).toBeVisible()
  expect(screen.getByText('Live references: 4', { exact: false })).toBeVisible()
  expect(
    screen.queryByRole('button', { name: 'Preview cleanup' })
  ).not.toBeInTheDocument()
  expect(
    screen.queryByRole('button', { name: 'Cancel cleanup' })
  ).not.toBeInTheDocument()
})

test('cleanup limits save separately from the scheduling switch', async () => {
  vi.mocked(imageRequest).mockResolvedValue({
    ...settings,
    revision: 4,
    concurrency: 2,
  })
  const client = new QueryClient()
  render(
    <QueryClientProvider client={client}>
      <ImageCleanupConfigForm
        initial={settings}
        profiles={profiles}
        canManage
        onSaved={vi.fn().mockResolvedValue(undefined)}
      />
    </QueryClientProvider>
  )
  const user = userEvent.setup()
  const concurrency = screen.getByRole('spinbutton', {
    name: 'Cleanup concurrency',
  })
  expect(concurrency).toHaveAttribute('max', '8')
  await user.clear(concurrency)
  await user.type(concurrency, '2')
  await user.click(
    screen.getByRole('button', { name: 'Save cleanup settings' })
  )
  await waitFor(() =>
    expect(imageRequest).toHaveBeenCalledWith(
      '/api/admin/image-library/cleanup-config',
      'PUT',
      { ...settings, concurrency: 2 }
    )
  )
})

test('a daily policy retains its timezone and age unit when enabled', async () => {
  vi.mocked(imageRequest).mockImplementation(
    async (_url, _method, input) => input
  )
  const client = new QueryClient()
  render(
    <QueryClientProvider client={client}>
      <ImageCleanupConfigForm
        initial={settings}
        profiles={profiles}
        canManage
        onSaved={vi.fn().mockResolvedValue(undefined)}
      />
    </QueryClientProvider>
  )
  const user = userEvent.setup()
  await user.click(screen.getByRole('button', { name: 'Add cleanup policy' }))
  await user.type(screen.getByLabelText('Policy name'), 'Nightly R2')
  await user.click(screen.getByRole('checkbox', { name: 'Enable policy' }))
  await user.click(
    screen.getByRole('checkbox', { name: 'Enable scheduled cleanup' })
  )
  await user.click(
    screen.getByRole('button', { name: 'Save cleanup settings' })
  )
  await waitFor(() =>
    expect(imageRequest).toHaveBeenCalledWith(
      '/api/admin/image-library/cleanup-config',
      'PUT',
      expect.objectContaining({
        enabled: true,
        policies: [
          expect.objectContaining({
            name: 'Nightly R2',
            enabled: true,
            channel_id: 'r2-main',
            older_than: 180,
            age_unit: 'days',
            schedule_type: 'daily',
            daily_time: '03:00',
            timezone: 'Asia/Shanghai',
          }),
        ],
      })
    )
  )
})

test('an interval policy can be saved without an unused daily time', async () => {
  const initial: CleanupConfig = {
    ...settings,
    policies: [
      {
        id: 'hourly',
        name: 'Hourly',
        enabled: false,
        channel_id: 'r2-main',
        older_than: 48,
        age_unit: 'hours',
        schedule_type: 'interval',
        interval_minutes: 60,
        daily_time: '',
        timezone: 'Asia/Shanghai',
      },
    ],
  }
  vi.mocked(imageRequest).mockResolvedValue(initial)
  const client = new QueryClient()
  render(
    <QueryClientProvider client={client}>
      <ImageCleanupConfigForm
        initial={initial}
        profiles={profiles}
        canManage
        onSaved={vi.fn().mockResolvedValue(undefined)}
      />
    </QueryClientProvider>
  )
  const interval = screen.getByRole('spinbutton', { name: 'Interval (hours)' })
  expect(interval).toHaveValue(1)
  expect(interval).toHaveAttribute('min', String(5 / 60))
  expect(interval).toHaveAttribute('max', '720')
  await userEvent
    .setup()
    .click(screen.getByRole('button', { name: 'Save cleanup settings' }))
  await waitFor(() =>
    expect(imageRequest).toHaveBeenCalledWith(
      '/api/admin/image-library/cleanup-config',
      'PUT',
      initial
    )
  )
})

test('interval hours are converted to minutes when a cleanup policy is saved', async () => {
  const initial: CleanupConfig = {
    ...settings,
    policies: [
      {
        id: 'interval',
        name: 'Interval policy',
        enabled: false,
        channel_id: 'r2-main',
        older_than: 48,
        age_unit: 'hours',
        schedule_type: 'interval',
        interval_minutes: 60,
        daily_time: '',
        timezone: 'Asia/Shanghai',
      },
    ],
  }
  vi.mocked(imageRequest).mockImplementation(
    async (_url, _method, input) => input
  )
  const client = new QueryClient()
  render(
    <QueryClientProvider client={client}>
      <ImageCleanupConfigForm
        initial={initial}
        profiles={profiles}
        canManage
        onSaved={vi.fn().mockResolvedValue(undefined)}
      />
    </QueryClientProvider>
  )
  const user = userEvent.setup()
  const interval = screen.getByRole('spinbutton', { name: 'Interval (hours)' })
  await user.clear(interval)
  await user.type(interval, '6')
  await user.click(
    screen.getByRole('button', { name: 'Save cleanup settings' })
  )
  await waitFor(() =>
    expect(imageRequest).toHaveBeenCalledWith(
      '/api/admin/image-library/cleanup-config',
      'PUT',
      expect.objectContaining({
        policies: [expect.objectContaining({ interval_minutes: 360 })],
      })
    )
  )
})

function renderIntervalPolicies(intervals: number[], canManage = true) {
  const initial: CleanupConfig = {
    ...settings,
    policies: intervals.map((minutes, index) => ({
      id: `interval-${index}`,
      name: `Interval policy ${index}`,
      enabled: false,
      channel_id: 'r2-main',
      older_than: 48,
      age_unit: 'hours',
      schedule_type: 'interval',
      interval_minutes: minutes,
      daily_time: '',
      timezone: 'Asia/Shanghai',
    })),
  }
  vi.mocked(imageRequest).mockImplementation(
    async (_url, _method, input) => input
  )
  render(
    <QueryClientProvider client={new QueryClient()}>
      <ImageCleanupConfigForm
        initial={initial}
        profiles={profiles}
        canManage={canManage}
        onSaved={vi.fn().mockResolvedValue(undefined)}
      />
    </QueryClientProvider>
  )
  return initial
}

test.each([5, 15, 61, 43200])(
  'a cleanup interval of %s minutes can be entered and saved exactly',
  async (minutes) => {
    renderIntervalPolicies([60])
    const user = userEvent.setup()
    await user.click(screen.getByRole('combobox', { name: 'Interval unit' }))
    await user.click(screen.getByRole('option', { name: 'Minutes' }))
    const input = screen.getByRole('spinbutton', { name: 'Interval (minutes)' })
    expect(input).toHaveValue(60)
    await user.clear(input)
    await user.type(input, String(minutes))
    await user.click(
      screen.getByRole('button', { name: 'Save cleanup settings' })
    )
    await waitFor(() =>
      expect(imageRequest).toHaveBeenCalledWith(
        '/api/admin/image-library/cleanup-config',
        'PUT',
        expect.objectContaining({
          policies: [expect.objectContaining({ interval_minutes: minutes })],
        })
      )
    )
    const payload = vi.mocked(imageRequest).mock.calls[0][2] as CleanupConfig
    expect(payload.policies[0]).not.toHaveProperty('interval_unit')
  }
)

test.each([5, 61])(
  'an existing %s minute policy survives switching display units and saving',
  async (minutes) => {
    const initial = renderIntervalPolicies([minutes])
    const user = userEvent.setup()
    expect(
      screen.getByRole('spinbutton', { name: 'Interval (minutes)' })
    ).toHaveValue(minutes)
    await user.click(screen.getByRole('combobox', { name: 'Interval unit' }))
    await user.click(screen.getByRole('option', { name: 'Hours' }))
    expect(
      screen.getByRole('spinbutton', { name: 'Interval (hours)' })
    ).toHaveValue(minutes / 60)
    await user.click(
      screen.getByRole('button', { name: 'Save cleanup settings' })
    )
    await waitFor(() =>
      expect(imageRequest).toHaveBeenCalledWith(
        '/api/admin/image-library/cleanup-config',
        'PUT',
        initial
      )
    )
  }
)

test.each(['', '4', '43201', '5.5'])(
  'an invalid minute interval "%s" prevents saving',
  async (value) => {
    renderIntervalPolicies([15])
    const user = userEvent.setup()
    const input = screen.getByRole('spinbutton', { name: 'Interval (minutes)' })
    await user.clear(input)
    if (value) await user.type(input, value)
    await user.click(
      screen.getByRole('button', { name: 'Save cleanup settings' })
    )
    expect(input).not.toBeValid()
    expect(imageRequest).not.toHaveBeenCalled()
  }
)

test('decimal hours are saved as exact whole minutes without floating point rejection', async () => {
  renderIntervalPolicies([60])
  const user = userEvent.setup()
  const input = screen.getByRole('spinbutton', { name: 'Interval (hours)' })
  await user.clear(input)
  await user.type(input, '1.05')
  await user.click(
    screen.getByRole('button', { name: 'Save cleanup settings' })
  )
  await waitFor(() =>
    expect(imageRequest).toHaveBeenCalledWith(
      '/api/admin/image-library/cleanup-config',
      'PUT',
      expect.objectContaining({
        policies: [expect.objectContaining({ interval_minutes: 63 })],
      })
    )
  )
})

test('hours that convert to fractional minutes cannot be saved', async () => {
  renderIntervalPolicies([60])
  const user = userEvent.setup()
  const input = screen.getByRole('spinbutton', { name: 'Interval (hours)' })
  await user.clear(input)
  await user.type(input, '0.11')
  await user.click(
    screen.getByRole('button', { name: 'Save cleanup settings' })
  )
  await waitFor(() => expect(input).toHaveAttribute('aria-invalid', 'true'))
  expect(screen.getByRole('alert')).toHaveTextContent('Check this value.')
  expect(imageRequest).not.toHaveBeenCalled()
})

test('removing a policy preserves the remaining policy display unit and duration', async () => {
  renderIntervalPolicies([60, 120])
  const user = userEvent.setup()
  await user.click(
    screen.getAllByRole('combobox', { name: 'Interval unit' })[1]
  )
  await user.click(screen.getByRole('option', { name: 'Minutes' }))
  await user.click(screen.getAllByRole('button', { name: 'Remove policy' })[0])
  expect(
    screen.getByRole('spinbutton', { name: 'Interval (minutes)' })
  ).toHaveValue(120)
  await user.click(
    screen.getByRole('button', { name: 'Save cleanup settings' })
  )
  await waitFor(() =>
    expect(imageRequest).toHaveBeenCalledWith(
      '/api/admin/image-library/cleanup-config',
      'PUT',
      expect.objectContaining({
        policies: [
          expect.objectContaining({ id: 'interval-1', interval_minutes: 120 }),
        ],
      })
    )
  )
})

test('read-only cleanup settings disable the interval input and unit selector', () => {
  renderIntervalPolicies([15], false)
  expect(
    screen.getByRole('spinbutton', { name: 'Interval (minutes)' })
  ).toBeDisabled()
  expect(screen.getByRole('combobox', { name: 'Interval unit' })).toBeDisabled()
})

test.each([1900000000, 0])(
  'saved next execution %s is displayed but never submitted as cleanup configuration',
  async (nextRun) => {
    const initial: CleanupConfig = {
      ...settings,
      enabled: nextRun > 0,
      policies: [
        {
          id: 'scheduled',
          name: 'Scheduled policy',
          enabled: true,
          channel_id: 'r2-main',
          older_than: 48,
          age_unit: 'hours',
          schedule_type: 'interval',
          interval_minutes: 60,
          daily_time: '',
          timezone: 'Asia/Shanghai',
          next_run_at: nextRun,
        },
      ],
    }
    vi.mocked(imageRequest).mockResolvedValue(initial)
    const client = new QueryClient()
    const view = render(
      <QueryClientProvider client={client}>
        <ImageCleanupConfigForm
          initial={initial}
          profiles={profiles}
          canManage
          onSaved={vi.fn().mockResolvedValue(undefined)}
        />
      </QueryClientProvider>
    )
    const nextRunStatus = screen.getByLabelText(
      'Next scheduled run (saved settings)'
    )
    expect(nextRunStatus).toHaveTextContent(
      nextRun ? new Date(nextRun * 1000).toLocaleString() : 'Disabled'
    )
    const refreshedNextRun = nextRun ? nextRun + 3600 : 0
    view.rerender(
      <QueryClientProvider client={client}>
        <ImageCleanupConfigForm
          initial={{
            ...initial,
            policies: initial.policies.map((policy) => ({
              ...policy,
              next_run_at: refreshedNextRun,
            })),
          }}
          profiles={profiles}
          canManage
          onSaved={vi.fn().mockResolvedValue(undefined)}
        />
      </QueryClientProvider>
    )
    expect(
      screen.getByLabelText('Next scheduled run (saved settings)')
    ).toHaveTextContent(
      refreshedNextRun
        ? new Date(refreshedNextRun * 1000).toLocaleString()
        : 'Disabled'
    )
    await userEvent
      .setup()
      .click(screen.getByRole('button', { name: 'Save cleanup settings' }))
    await waitFor(() => expect(imageRequest).toHaveBeenCalled())
    const payload = vi.mocked(imageRequest).mock.calls[0][2] as CleanupConfig
    expect(payload.policies[0]).not.toHaveProperty('next_run_at')
  }
)

test('refreshing the runtime snapshot updates status without discarding draft limits or submitting readonly fields', async () => {
  const initial: CleanupConfig = {
    ...settings,
    runtime: {
      active_workers: 2,
      pressure_reason: 'CPU pressure',
      sampled_at: 1900000000,
    },
  }
  const client = new QueryClient()
  const onSaved = vi.fn().mockResolvedValue(undefined)
  const view = render(
    <QueryClientProvider client={client}>
      <ImageCleanupConfigForm
        initial={initial}
        profiles={profiles}
        canManage
        onSaved={onSaved}
      />
    </QueryClientProvider>
  )
  expect(
    screen.getByRole('region', { name: 'Cleanup runtime status' })
  ).toHaveTextContent('Active cleanup workers: 2')
  expect(
    screen.getByRole('region', { name: 'Cleanup runtime status' })
  ).toHaveTextContent('CPU pressure')
  const user = userEvent.setup()
  await user.clear(
    screen.getByRole('spinbutton', { name: 'Cleanup concurrency' })
  )
  await user.type(
    screen.getByRole('spinbutton', { name: 'Cleanup concurrency' }),
    '3'
  )
  await user.click(screen.getByRole('button', { name: 'Refresh' }))
  expect(onSaved).toHaveBeenCalledOnce()
  const refreshed: CleanupConfig = {
    ...initial,
    runtime: { active_workers: 0, pressure_reason: '', sampled_at: 1900000030 },
  }
  view.rerender(
    <QueryClientProvider client={client}>
      <ImageCleanupConfigForm
        initial={refreshed}
        profiles={profiles}
        canManage
        onSaved={onSaved}
      />
    </QueryClientProvider>
  )
  expect(
    screen.getByRole('spinbutton', { name: 'Cleanup concurrency' })
  ).toHaveValue(3)
  expect(
    screen.getByRole('region', { name: 'Cleanup runtime status' })
  ).toHaveTextContent('Active cleanup workers: 0')
  expect(
    screen.queryByText('CPU pressure', { exact: false })
  ).not.toBeInTheDocument()
  vi.mocked(imageRequest).mockResolvedValue(refreshed)
  await user.click(
    screen.getByRole('button', { name: 'Save cleanup settings' })
  )
  await waitFor(() => expect(imageRequest).toHaveBeenCalled())
  expect(vi.mocked(imageRequest).mock.calls[0][2]).not.toHaveProperty('runtime')
})

test('cleanup pressure and versioned storage diagnostics follow the selected interface language', async () => {
  const i18n = createInstance()
  await i18n.init({
    lng: 'zh',
    fallbackLng: 'en',
    resources: { zh: zhLocale, en: { translation: {} } },
  })
  vi.mocked(imageRequest).mockResolvedValue({
    items: [
      {
        ...job,
        status: 'paused',
        last_error: 'Physical cleanup is unavailable for versioned storage',
      },
    ],
    next_cursor: '',
  })
  const client = new QueryClient()
  render(
    <I18nextProvider i18n={i18n}>
      <QueryClientProvider client={client}>
        <ImageCleanupConfigForm
          initial={{
            ...settings,
            runtime: {
              active_workers: 0,
              pressure_reason: 'Cleanup is waiting for an initial CPU sample',
              sampled_at: 0,
            },
          }}
          profiles={profiles}
          canManage={false}
          onSaved={vi.fn()}
        />
        <ImageCleanupPanel profiles={profiles} />
      </QueryClientProvider>
    </I18nextProvider>
  )
  expect(
    screen.getByText('清理正在等待首次 CPU 采样', { exact: false })
  ).toBeVisible()
  expect(
    await screen.findByText('启用版本控制的存储不支持物理清理')
  ).toBeVisible()
  expect(
    screen.getByText('已确认删除对象的记录大小', { exact: false })
  ).toBeVisible()
  expect(
    screen.queryByText('已释放存储', { exact: false })
  ).not.toBeInTheDocument()
  expect(screen.getByText(/^已删除文件:/)).toBeVisible()
  expect(screen.queryByText('已注销', { exact: false })).not.toBeInTheDocument()
})
