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
import { afterEach, expect, test, vi } from 'vitest'

import { useAuthStore } from '@/stores/auth-store'

import { imageRequest } from '../../api'
import { CreateRetryRuleFromError } from '../create-from-error'
import { RetryPolicyCenter, RetryPolicyForm } from '../index'
import {
  newRetryRule,
  RETRY_POLICY_URL,
  retryPolicyChanges,
  copyRetryRule,
} from '../policy'
import { RetryPreview } from '../preview'
import { RetryRuleEditor } from '../rule-editor'
import { RetrySamples } from '../samples'
import type { RetryDecision, RetryPolicy, RetrySample } from '../types'

vi.mock('../../api', () => ({ imageRequest: vi.fn() }))
const clients: QueryClient[] = []
function show(
  element: React.ReactNode,
  client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
) {
  if (!clients.includes(client)) clients.push(client)
  return render(
    <QueryClientProvider client={client}>{element}</QueryClientProvider>
  )
}
function fixture(): RetryPolicy {
  const rule = newRetryRule({
    fact: {
      message: 'reference fetch failed',
      media_type: 'video',
      provider: 'drama',
      model: 'model-a',
      stage: 'poll',
      channel_id: 12,
    },
  })
  rule.name = 'Reference repair'
  return {
    revision: 4,
    pause_resubmissions: false,
    legacy_timeout_enabled: true,
    rules: [rule],
  }
}

function sampleFixture(count: number, first = 1) {
  return Array.from({ length: count }, (_, index) => ({
    fingerprint: `sample-${first + index}`,
    latest_event_id: first + index,
    message: `Failure ${first + index}`,
    media_type: 'video',
    provider: 'drama',
    model: 'video-model',
    channel_id: 1,
    stage: 'poll',
    http_status: 502,
    error_code: 'upstream_failed',
    category: 'unclassified',
    count: 1,
    task_count: 1,
    last_seen: 1,
    matched: false,
  })) satisfies (RetrySample & { latest_event_id: number; matched: boolean })[]
}

function previewResponse() {
  let resolve!: (value: { decision: RetryDecision }) => void
  let reject!: (reason: Error) => void
  const promise = new Promise<{ decision: RetryDecision }>((yes, no) => {
    resolve = yes
    reject = no
  })
  return { promise, resolve, reject }
}

function previewDecision(id: number): { decision: RetryDecision } {
  return {
    decision: {
      matched: true,
      revision: 4,
      action: 'same_channel',
      rule_id: `rule-${id}`,
    },
  }
}
afterEach(() => {
  cleanup()
  useAuthStore.getState().auth.setUser(null)
  clients.splice(0).forEach((client) => client.clear())
  vi.resetAllMocks()
  vi.useRealTimers()
})

test('reopening after a successful save keeps the saved policy and revision', async () => {
  useAuthStore.getState().auth.setUser({ id: 7, username: 'admin', role: 100 })
  const policy = fixture()
  const saved = { ...policy, revision: 5, pause_resubmissions: true }
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Infinity } },
  })
  vi.mocked(imageRequest)
    .mockResolvedValueOnce(policy)
    .mockResolvedValueOnce(saved)
  const page = show(<RetryPolicyCenter />, client)
  const user = userEvent.setup()
  await user.click(
    await screen.findByRole('checkbox', {
      name: 'Pause automatic resubmissions',
    })
  )
  await user.click(screen.getByRole('button', { name: 'Save and activate' }))
  await screen.findByText('Version 5')
  page.unmount()
  show(<RetryPolicyCenter />, client)
  expect(
    screen.getByRole('checkbox', { name: 'Pause automatic resubmissions' })
  ).toBeChecked()
  expect(screen.getByText('Version 5')).toBeVisible()
})

test.each([200, 409, 500])(
  'returning during a save retains its submitted draft and result (status=%s)',
  async (status) => {
    useAuthStore
      .getState()
      .auth.setUser({ id: 7, username: 'admin', role: 100 })
    const policy = fixture()
    const saved = { ...policy, revision: 5, pause_resubmissions: true }
    let resolveResponse!: (policy: RetryPolicy) => void
    let rejectResponse!: (error: Error) => void
    const response = new Promise<RetryPolicy>((resolve, reject) => {
      resolveResponse = resolve
      rejectResponse = reject
    })
    const client = new QueryClient({
      defaultOptions: { queries: { retry: false, staleTime: Infinity } },
    })
    vi.mocked(imageRequest)
      .mockResolvedValueOnce(policy)
      .mockReturnValueOnce(response)
    const page = show(<RetryPolicyCenter />, client)
    const user = userEvent.setup()
    await user.click(
      await screen.findByRole('checkbox', {
        name: 'Pause automatic resubmissions',
      })
    )
    await user.click(screen.getByRole('button', { name: 'Save and activate' }))
    page.unmount()
    const resumed = show(<RetryPolicyCenter />, client)
    expect(
      screen.queryByRole('button', { name: 'Save and activate' })
    ).not.toBeInTheDocument()
    await act(async () => {
      if (status === 200) {
        resolveResponse(saved)
      } else {
        rejectResponse(
          Object.assign(new Error('Save unavailable'), {
            isAxiosError: true,
            response: { status },
          })
        )
      }
    })
    expect(
      await screen.findByRole('checkbox', {
        name: 'Pause automatic resubmissions',
      })
    ).toBeChecked()
    if (status === 200) {
      expect(screen.getByText('Version 5')).toBeVisible()
      expect(
        screen.getByRole('button', { name: 'Save and activate' })
      ).toBeDisabled()
    } else {
      expect(screen.getByText(/Version 4/)).toHaveTextContent('Unsaved changes')
      if (status === 409) {
        expect(
          screen.getByRole('button', { name: 'Reload saved settings' })
        ).toBeVisible()
        expect(
          screen.getByRole('button', { name: 'Save and activate' })
        ).toBeDisabled()
      } else {
        expect(screen.getByRole('alert')).toHaveTextContent('Save unavailable')
        expect(
          screen.getByRole('button', { name: 'Save and activate' })
        ).toBeEnabled()
      }
      resumed.unmount()
      show(<RetryPolicyCenter />, client)
      expect(
        screen.getByRole('checkbox', { name: 'Pause automatic resubmissions' })
      ).toBeChecked()
      expect(screen.getByRole('alert')).toBeVisible()
    }
  }
)

test('failed conflict reload preserves the draft and successful reload survives reopening', async () => {
  useAuthStore.getState().auth.setUser({ id: 7, username: 'admin', role: 100 })
  const policy = fixture()
  const latest = { ...policy, revision: 5 }
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Infinity } },
  })
  vi.mocked(imageRequest)
    .mockResolvedValueOnce(policy)
    .mockRejectedValueOnce(
      Object.assign(new Error('Conflict'), {
        isAxiosError: true,
        response: { status: 409 },
      })
    )
    .mockRejectedValueOnce(new Error('Reload unavailable'))
    .mockResolvedValueOnce(latest)
  const page = show(<RetryPolicyCenter />, client)
  const user = userEvent.setup()
  await user.click(
    await screen.findByRole('checkbox', {
      name: 'Pause automatic resubmissions',
    })
  )
  await user.click(screen.getByRole('button', { name: 'Save and activate' }))
  const reload = await screen.findByRole('button', {
    name: 'Reload saved settings',
  })
  await user.click(reload)
  await waitFor(() => expect(reload).toBeEnabled())
  expect(
    screen.getByRole('checkbox', { name: 'Pause automatic resubmissions' })
  ).toBeChecked()
  expect(screen.getByText(/Version 4/)).toHaveTextContent('Unsaved changes')
  await user.click(reload)
  await screen.findByText('Version 5')
  page.unmount()
  show(<RetryPolicyCenter />, client)
  expect(screen.getByText('Version 5')).toBeVisible()
  expect(
    screen.getByRole('checkbox', { name: 'Pause automatic resubmissions' })
  ).not.toBeChecked()
  expect(
    screen.queryByRole('button', { name: 'Reload saved settings' })
  ).not.toBeInTheDocument()
})

test('returning during a rollback adopts its saved revision and keeps it on the next visit', async () => {
  useAuthStore.getState().auth.setUser({ id: 7, username: 'admin', role: 100 })
  const policy = fixture()
  const restored = { ...policy, revision: 5, pause_resubmissions: true }
  let resolveResponse!: (policy: RetryPolicy) => void
  const response = new Promise<RetryPolicy>((resolve) => {
    resolveResponse = resolve
  })
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Infinity } },
  })
  vi.mocked(imageRequest).mockImplementation(async (url) => {
    if (url === `${RETRY_POLICY_URL}/rollback`) return response as never
    if (url === `${RETRY_POLICY_URL}/versions/3`) {
      return { policy: { ...restored, revision: 3 } } as never
    }
    if (url.includes('/versions?')) {
      return {
        items: [{ revision: 3, created_at: 1, actor_id: 7, action: 'save' }],
        total: 1,
      } as never
    }
    return policy as never
  })
  const page = show(<RetryPolicyCenter />, client)
  const user = userEvent.setup()
  await user.click(await screen.findByRole('tab', { name: 'Version history' }))
  await user.click(
    await screen.findByRole('button', { name: 'Compare version' })
  )
  const restore = await screen.findByRole('button', {
    name: 'Restore this version',
  })
  await waitFor(() => expect(restore).toBeEnabled())
  await user.click(restore)
  await user.click(
    within(await screen.findByRole('alertdialog')).getByRole('button', {
      name: 'Continue',
    })
  )
  page.unmount()
  const resumed = show(<RetryPolicyCenter />, client)
  expect(
    screen.queryByRole('button', { name: 'Save and activate' })
  ).not.toBeInTheDocument()
  await act(async () => resolveResponse(restored))
  expect(await screen.findByText('Version 5')).toBeVisible()
  expect(
    screen.getByRole('checkbox', { name: 'Pause automatic resubmissions' })
  ).toBeChecked()
  resumed.unmount()
  show(<RetryPolicyCenter />, client)
  expect(screen.getByText('Version 5')).toBeVisible()
  expect(
    screen.getByRole('checkbox', { name: 'Pause automatic resubmissions' })
  ).toBeChecked()
})

test('removes a rule from the draft after confirmation and saves it with the current revision', async () => {
  const policy = fixture()
  vi.mocked(imageRequest).mockResolvedValueOnce({
    ...policy,
    revision: 5,
    rules: [],
  })
  show(<RetryPolicyForm initial={policy} canManage />)
  const user = userEvent.setup()
  await user.click(screen.getByRole('button', { name: 'Delete' }))
  const confirmation = await screen.findByRole('alertdialog')
  await user.click(within(confirmation).getByRole('button', { name: 'Cancel' }))
  expect(screen.getByRole('row', { name: /Reference repair/ })).toBeVisible()

  await user.click(screen.getByRole('button', { name: 'Delete' }))
  await user.click(
    within(await screen.findByRole('alertdialog')).getByRole('button', {
      name: 'Delete',
    })
  )
  expect(screen.queryByRole('row', { name: /Reference repair/ })).toBeNull()
  expect(imageRequest).not.toHaveBeenCalled()

  await user.click(screen.getByRole('button', { name: 'Save and activate' }))
  await waitFor(() =>
    expect(imageRequest).toHaveBeenCalledWith(RETRY_POLICY_URL, 'PUT', {
      ...policy,
      rules: [],
    })
  )
})

test('shows recently edited rules first while moving rules by execution priority', async () => {
  const policy = fixture()
  const original = {
    ...policy.rules[0],
    updated_at: Date.UTC(2026, 10, 2),
  }
  const recent = {
    ...copyRetryRule(original),
    name: 'New reference rule',
    updated_at: Date.UTC(2026, 10, 3),
  }
  policy.rules = [original, recent]
  vi.mocked(imageRequest).mockResolvedValueOnce({
    ...policy,
    revision: 5,
    rules: [recent, original],
  })
  show(<RetryPolicyForm initial={policy} canManage />)
  expect(screen.getAllByRole('row')[1]).toHaveTextContent('New reference rule')
  const user = userEvent.setup()
  await user.click(
    within(screen.getByRole('row', { name: /New reference rule/ })).getByRole(
      'button',
      { name: 'Move up' }
    )
  )
  await user.click(screen.getByRole('button', { name: 'Save and activate' }))
  await waitFor(() =>
    expect(imageRequest).toHaveBeenCalledWith(RETRY_POLICY_URL, 'PUT', {
      ...policy,
      rules: [recent, original],
    })
  )
})

test('keeps rule edits and revision after a concurrent save conflict', async () => {
  const policy = fixture()
  const error = Object.assign(new Error('stale'), {
    isAxiosError: true,
    response: { status: 409 },
  })
  vi.mocked(imageRequest).mockRejectedValueOnce(error)
  show(<RetryPolicyForm initial={policy} canManage />)
  const user = userEvent.setup()
  await user.click(
    screen.getByRole('checkbox', { name: 'Enabled: Reference repair' })
  )
  await user.click(screen.getByRole('button', { name: 'Save and activate' }))
  expect(await screen.findByRole('alert')).toHaveTextContent(
    'Your edits have been kept.'
  )
  expect(
    screen.getByRole('checkbox', { name: 'Enabled: Reference repair' })
  ).not.toBeChecked()
  expect(imageRequest).toHaveBeenCalledWith(
    RETRY_POLICY_URL,
    'PUT',
    expect.objectContaining({
      revision: 4,
      rules: [
        expect.objectContaining({ id: policy.rules[0].id, enabled: false }),
      ],
    })
  )
  vi.mocked(imageRequest).mockResolvedValueOnce({ ...policy, revision: 5 })
  await user.click(
    screen.getByRole('button', { name: 'Reload saved settings' })
  )
  await waitFor(() =>
    expect(
      screen.getByRole('checkbox', { name: 'Enabled: Reference repair' })
    ).toBeChecked()
  )
})

test('creates a scoped draft from a task without saving it', async () => {
  const policy = fixture()
  show(
    <RetryPolicyForm
      initial={policy}
      canManage
      source={{
        fact: {
          message: 'fetch failed',
          media_type: 'video',
          provider: 'drama',
          model: 'm1',
          channel_id: 12,
          stage: 'poll',
        },
      }}
    />
  )
  expect(await screen.findByLabelText('Providers (one per line)')).toHaveValue(
    'drama'
  )
  expect(screen.getByLabelText('Models (one per line)')).toHaveValue('m1')
  expect(
    screen.getByLabelText('Any matching keyword (one per line)')
  ).toHaveValue('fetch failed')
  expect(imageRequest).not.toHaveBeenCalled()
})

test('keeps numeric multiline entries and step identities while reordering actions', async () => {
  const rule = fixture().rules[0]
  const apply = vi.fn()
  show(<RetryRuleEditor rule={rule} onClose={vi.fn()} onApply={apply} />)
  const user = userEvent.setup()
  await user.click(screen.getByRole('button', { name: 'Add recovery step' }))
  const groups = screen.getAllByRole('group')
  await user.selectOptions(
    within(groups[1]).getByLabelText('Action'),
    'other_channel'
  )
  await user.click(within(groups[1]).getByRole('button', { name: 'Move up' }))
  await user.click(screen.getByRole('button', { name: 'Advanced conditions' }))
  const ids = screen.getByLabelText('Allowed target channel IDs (one per line)')
  await user.type(ids, '12{Enter}34')
  expect(ids).toHaveValue('12\n34')
  await user.click(screen.getByRole('button', { name: 'Apply to draft' }))
  await waitFor(() => expect(apply).toHaveBeenCalled())
  expect(apply.mock.calls[0][0]).toMatchObject({
    allowed_channel_ids: [12, 34],
    steps: [
      { action: 'other_channel' },
      { id: rule.steps[0].id, action: 'same_channel' },
    ],
  })
})

test('validates invalid channel IDs before applying an edited rule', async () => {
  const apply = vi.fn()
  show(
    <RetryRuleEditor
      rule={fixture().rules[0]}
      onClose={vi.fn()}
      onApply={apply}
    />
  )
  const user = userEvent.setup()
  await user.click(screen.getByRole('button', { name: 'Advanced conditions' }))
  await user.type(
    screen.getByLabelText('Allowed target channel IDs (one per line)'),
    '-1'
  )
  await user.click(screen.getByRole('button', { name: 'Apply to draft' }))
  expect(await screen.findByRole('alert')).toHaveTextContent(
    'Check rule fields'
  )
  expect(apply).not.toHaveBeenCalled()
})

test('uses only the preview endpoint and switches edited historical messages to text matching', async () => {
  vi.mocked(imageRequest).mockResolvedValue({
    mode: 'text_only',
    decision: {
      matched: true,
      rule_id: 'rule',
      revision: 4,
      action: 'manual_review',
      blocked_reason: 'execution_unknown',
    },
  })
  show(
    <RetryPreview
      policy={fixture()}
      source={{
        event_id: 5,
        task_id: 'task-test',
        fact: { message: 'fetch failed' },
      }}
      onClose={vi.fn()}
    />
  )
  const user = userEvent.setup()
  await user.type(screen.getByLabelText('Error message'), ' again')
  await user.click(screen.getByRole('button', { name: 'Run test' }))
  expect(await screen.findByRole('status')).toHaveTextContent(
    'execution_unknown'
  )
  expect(imageRequest).toHaveBeenCalledTimes(1)
  expect(imageRequest).toHaveBeenCalledWith(
    `${RETRY_POLICY_URL}/preview`,
    'POST',
    expect.objectContaining({
      task_id: undefined,
      event_id: undefined,
      fact: expect.objectContaining({ message: 'fetch failed again' }),
    })
  )
})

test('read-only administrators can preview but cannot alter rules or global switches', () => {
  show(<RetryPolicyForm initial={fixture()} canManage={false} />)
  expect(
    screen.queryByRole('button', { name: 'Save and activate' })
  ).not.toBeInTheDocument()
  expect(
    screen.queryByRole('button', { name: 'Add rule' })
  ).not.toBeInTheDocument()
  expect(
    screen.queryByRole('button', { name: 'Delete' })
  ).not.toBeInTheDocument()
  expect(
    screen.getByRole('checkbox', { name: 'Pause automatic resubmissions' })
  ).toHaveAttribute('aria-disabled', 'true')
  expect(
    screen.getByRole('button', { name: 'Test retry policy' })
  ).toBeEnabled()
})

test('shows captured legacy scope, limits and the existing budget configuration entry', async () => {
  const policy = fixture()
  policy.legacy_timeout_scope_keys = ['captured-image-policy']
  policy.legacy_timeout_config = { upstream_timeout_max_retries: 2 }
  policy.legacy_video_channel_ids = [71]
  show(<RetryPolicyForm initial={policy} canManage />)
  const user = userEvent.setup()
  await user.click(screen.getByText('Captured timeout compatibility policy'))
  expect(screen.getByText('captured-image-policy')).toBeVisible()
  expect(screen.getByText('upstream_timeout_max_retries')).toBeVisible()
  expect(screen.getByText(/Video.*Channels: 71/)).toBeVisible()
  expect(
    screen.getByText('View media settings and retry budgets').closest('a')
  ).toHaveAttribute('href', '/system-settings/operations/images')
})

test('version comparison reports field changes and copies receive independent stable budgets', () => {
  const policy = fixture()
  const next = structuredClone(policy)
  next.revision += 1
  next.rules[0].steps[0].max_attempts = 2
  expect(retryPolicyChanges(policy, next)).toEqual([
    { path: 'rules.0.steps.0.max_attempts', before: 1, after: 2 },
  ])
  const copy = copyRetryRule(policy.rules[0])
  expect(copy.id).not.toBe(policy.rules[0].id)
  expect(copy.steps[0].id).not.toBe(policy.rules[0].steps[0].id)
  expect(copy.steps[0].max_attempts).toBe(1)
})

test('shows current eligibility separately from the recorded built-in action', async () => {
  vi.mocked(imageRequest).mockResolvedValueOnce({
    decision: { matched: false, revision: 8, action: '' },
    mode: 'sample',
    state_source: 'current_task',
    state: { total_used: 2, total_limit: 5 },
    eligibility: {
      checked: true,
      eligible: false,
      reason: 'historical_task_not_restarted',
      warnings: ['remote_reference_download_not_performed'],
    },
    recorded_decision: { action: 'other_channel', revision: 2 },
    recorded_decision_source: 'recorded_builtin',
  })
  show(
    <RetryPreview
      policy={fixture()}
      source={{ event_id: 7, fact: { message: 'historical error' } }}
      onClose={vi.fn()}
    />
  )
  await userEvent
    .setup()
    .click(screen.getByRole('button', { name: 'Run test' }))
  expect(
    await screen.findByText(
      'Historical tasks require an explicit manual restart'
    )
  ).toBeVisible()
  expect(
    screen.getByText('Preview does not download remote reference media')
  ).toBeVisible()
  expect(screen.getByText(/Recorded action/)).toHaveTextContent(
    'Retry on another channel'
  )
  expect(
    screen.getByText(/Uses the recorded error with current task budgets/)
  ).toBeVisible()
  expect(screen.getByText('2 / 3')).toBeVisible()
})

test('identifies the current built-in plan and translates its legacy action separately from history', async () => {
  vi.mocked(imageRequest).mockResolvedValueOnce({
    decision: {
      matched: false,
      revision: 9,
      action: 'switch_account',
      requested_action: 'retry_submission',
    },
    decision_source: 'current_builtin',
    mode: 'sample',
    state_source: 'current_task',
    recorded_decision: { action: 'wait_capacity', revision: 5 },
    recorded_decision_source: 'recorded_builtin',
  })
  show(
    <RetryPreview
      policy={fixture()}
      source={{ event_id: 8, fact: { message: 'temporary error' } }}
      onClose={vi.fn()}
    />
  )
  await userEvent
    .setup()
    .click(screen.getByRole('button', { name: 'Run test' }))
  expect(await screen.findByText('Current built-in policy')).toBeVisible()
  expect(screen.queryByText(/No matching custom rule/)).not.toBeInTheDocument()
  expect(screen.getByText('Switch account')).toBeVisible()
  expect(screen.getByText('Retry generation submission')).toBeVisible()
  expect(screen.getByText(/Recorded action/)).toHaveTextContent(
    'Wait for capacity'
  )
  expect(
    screen.getByText('This action was recorded when the error occurred.')
  ).toBeVisible()
})

test('displays translated built-in category names while preserving their stored identity', async () => {
  const rule = fixture().rules[0]
  const apply = vi.fn()
  show(<RetryRuleEditor rule={rule} onClose={vi.fn()} onApply={apply} />)
  expect(screen.getByLabelText('Error category')).toHaveValue('Unclassified')
  await userEvent
    .setup()
    .click(screen.getByRole('button', { name: 'Apply to draft' }))
  await waitFor(() =>
    expect(apply).toHaveBeenCalledWith(
      expect.objectContaining({ category: 'unclassified' }),
      expect.anything()
    )
  )
})

test('opens error samples in a wider viewport-bounded dialog and wraps long values', async () => {
  useAuthStore.getState().auth.setUser({ id: 7, username: 'admin', role: 100 })
  const policy = fixture()
  const [sample] = sampleFixture(1)
  sample.message = `Upstream returned ${'unbroken-error-context'.repeat(12)}`
  const ruleId = policy.rules[0].id
  vi.mocked(imageRequest)
    .mockResolvedValueOnce(policy)
    .mockResolvedValueOnce({
      items: [{ ...sample, matched: true, rule_id: ruleId }],
      total: 1,
    })
  const user = userEvent.setup()
  show(<CreateRetryRuleFromError source={{ fact: sample }} />)

  await user.click(
    screen.getByRole('button', { name: 'Create rule from error' })
  )
  const editor = await screen.findByRole('dialog', { name: 'Edit retry rule' })
  await user.click(within(editor).getByRole('button', { name: 'Cancel' }))
  const dialog = await screen.findByRole('dialog', {
    name: 'Error and retry rules',
  })
  await user.click(within(dialog).getByRole('tab', { name: 'Error samples' }))

  const message = await within(dialog).findByText(sample.message)
  const table = within(dialog).getByRole('table')
  expect(dialog).toHaveClass('sm:max-w-[min(90rem,calc(100%-2rem))]')
  expect(table).toHaveClass('[&_th]:whitespace-normal')
  expect(message.parentElement).toHaveClass('wrap-anywhere')
  expect(within(table).getByText(ruleId)).toHaveClass('wrap-anywhere')
  expect(within(table).getByRole('button', { name: 'Details' })).toBeVisible()
})

test('initializes sample filters and the first request to today and allows a custom range', async () => {
  vi.setSystemTime(new Date(2026, 10, 4, 12))
  vi.mocked(imageRequest)
    .mockResolvedValueOnce({ items: [], total: 0 })
    .mockResolvedValueOnce({ items: [], total: 0 })
  show(
    <RetrySamples
      canManage
      policy={fixture()}
      onDraft={vi.fn()}
      onTest={vi.fn()}
    />
  )
  const startDate = screen.getByLabelText('Start date')
  expect(startDate).toHaveValue('2026-11-04')
  expect(screen.getByLabelText('End date')).toHaveValue('2026-11-04')
  expect(imageRequest).toHaveBeenCalledWith(
    `${RETRY_POLICY_URL}/samples?start_timestamp=${Date.parse('2026-11-04T00:00:00') / 1000}&end_timestamp=${Date.parse('2026-11-04T23:59:59') / 1000}&page=1&page_size=20`
  )
  const user = userEvent.setup()
  await user.clear(startDate)
  await user.type(startDate, '2026-11-03')
  await user.click(screen.getByRole('button', { name: 'Apply filters' }))
  await waitFor(() =>
    expect(imageRequest).toHaveBeenLastCalledWith(
      `${RETRY_POLICY_URL}/samples?start_timestamp=${Date.parse('2026-11-03T00:00:00') / 1000}&end_timestamp=${Date.parse('2026-11-04T23:59:59') / 1000}&page=1&page_size=20`
    )
  )
})

test('opens sample occurrences and creates a draft from the selected task failure', async () => {
  const onDraft = vi.fn()
  vi.mocked(imageRequest)
    .mockResolvedValueOnce({
      items: [
        {
          fingerprint: 'sample-hash',
          message: 'reference failed',
          media_type: 'image',
          provider: 'openai',
          model: 'image-model',
          stage: 'prepare',
          count: 2,
          task_count: 2,
          last_seen: 1,
          latest_event_id: 8,
        },
      ],
      total: 1,
    })
    .mockResolvedValueOnce({
      items: [
        {
          id: 7,
          task_id: 'task-with-error',
          channel_id: 22,
          created_at: 1,
          category: 'reference_download',
          revision: 3,
          action: 'local_same_channel',
          executed: true,
          outcome: 'succeeded',
        },
      ],
      total: 1,
    })
    .mockResolvedValueOnce({
      task_id: 'task-with-error',
      fact: { message: 'reference failed', channel_id: 22, stage: 'prepare' },
    })
  show(
    <RetrySamples
      canManage
      policy={fixture()}
      onDraft={onDraft}
      onTest={vi.fn()}
    />
  )
  const user = userEvent.setup()
  await user.click(await screen.findByRole('button', { name: 'Details' }))
  const dialog = await screen.findByRole('dialog')
  expect(await within(dialog).findByText('task-with-error')).toBeVisible()
  expect(within(dialog).getByText('22')).toBeVisible()
  expect(within(dialog).getByText('Succeeded')).toBeVisible()
  await user.click(
    within(dialog).getByRole('button', { name: 'Create rule from error' })
  )
  await waitFor(() =>
    expect(onDraft).toHaveBeenCalledWith(
      expect.objectContaining({ task_id: 'task-with-error', event_id: 7 })
    )
  )
  const listURL = vi.mocked(imageRequest).mock.calls[0][0]
  expect(imageRequest).toHaveBeenCalledWith(
    `${RETRY_POLICY_URL}/samples/sample-hash/events?${listURL.split('?')[1]}`
  )
})

test('batch previews run at most four requests and retain sample order when one request fails', async () => {
  const pending = new Map<number, ReturnType<typeof previewResponse>>()
  let active = 0
  let peak = 0
  vi.mocked(imageRequest).mockImplementation((url, _method, data) => {
    if (url.includes('/samples?')) {
      return Promise.resolve({ items: sampleFixture(6), total: 6 })
    }
    const id = (data as { event_id: number }).event_id
    const response = previewResponse()
    pending.set(id, response)
    active++
    peak = Math.max(peak, active)
    return response.promise.finally(() => active--)
  })
  show(
    <RetrySamples
      canManage
      policy={fixture()}
      onDraft={vi.fn()}
      onTest={vi.fn()}
    />
  )
  const user = userEvent.setup()
  const button = screen.getByRole('button', { name: 'Test visible samples' })
  expect(button).toBeDisabled()
  await screen.findByText('Failure 6')
  await user.click(button)
  expect(button).toBeDisabled()
  expect([...pending.keys()]).toEqual([1, 2, 3, 4])
  await user.click(button)
  expect(pending.size).toBe(4)
  await act(async () => {
    pending.get(3)?.reject(new Error('Preview context unavailable'))
    pending.get(2)?.resolve(previewDecision(2))
  })
  expect([...pending.keys()]).toEqual([1, 2, 3, 4, 5, 6])
  await act(async () => {
    for (const id of [6, 4, 5, 1]) {
      pending.get(id)?.resolve(previewDecision(id))
    }
  })
  const summary = await screen.findByText(/Custom rules matched 5 of 6 samples/)
  expect(summary).toHaveTextContent('Failed: 1')
  await user.click(summary)
  const rows = within(
    screen.getByRole('table', { name: 'Test visible samples' })
  )
    .getAllByRole('row')
    .slice(1)
  expect(
    rows.map((row) => within(row).getAllByRole('cell')[0].textContent)
  ).toEqual([
    'Failure 1',
    'Failure 2',
    'Failure 3',
    'Failure 4',
    'Failure 5',
    'Failure 6',
  ])
  expect(within(rows[2]).getByText('Test failed')).toBeVisible()
  expect(within(rows[2]).getByRole('alert')).toHaveTextContent(
    'Preview context unavailable'
  )
  expect(within(rows[5]).getByText('rule-6')).toBeVisible()
  expect(peak).toBe(4)
  expect(active).toBe(0)
  expect(button).toBeEnabled()
})

test('queued previews and results keep the policy and samples captured at the click', async () => {
  const policy = fixture()
  const items = sampleFixture(5)
  const pending = new Map<number, ReturnType<typeof previewResponse>>()
  const submitted: { policy: RetryPolicy; event_id: number }[] = []
  vi.mocked(imageRequest).mockImplementation((url, _method, data) => {
    if (url.includes('/samples?')) return Promise.resolve({ items, total: 5 })
    const input = data as { policy: RetryPolicy; event_id: number }
    submitted.push(structuredClone(input))
    const response = previewResponse()
    pending.set(input.event_id, response)
    return response.promise
  })
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Infinity } },
  })
  const page = show(
    <RetrySamples
      canManage
      policy={policy}
      onDraft={vi.fn()}
      onTest={vi.fn()}
    />,
    client
  )
  const user = userEvent.setup()
  await screen.findByText('Failure 5')
  await user.click(screen.getByRole('button', { name: 'Test visible samples' }))
  policy.rules[0].name = 'Edited after test started'
  items[4].message = 'New sample contents'
  items[4].latest_event_id = 99
  page.rerender(
    <QueryClientProvider client={client}>
      <RetrySamples
        canManage
        policy={policy}
        onDraft={vi.fn()}
        onTest={vi.fn()}
      />
    </QueryClientProvider>
  )
  await act(async () => pending.get(1)?.resolve(previewDecision(1)))
  expect([...pending.keys()]).toEqual([1, 2, 3, 4, 5])
  expect(submitted.map((input) => input.policy.rules[0].name)).toEqual(
    Array(5).fill('Reference repair')
  )
  await act(async () => {
    for (const id of [2, 3, 4, 5]) {
      pending.get(id)?.resolve(previewDecision(id))
    }
  })
  const summary = await screen.findByText('Custom rules matched 5 of 5 samples')
  await user.click(summary)
  const results = within(
    screen.getByRole('table', { name: 'Test visible samples' })
  )
  expect(results.getByText('Failure 5')).toBeVisible()
  expect(results.queryByText('New sample contents')).not.toBeInTheDocument()
})

test.each(['page', 'filters'])(
  'changing %s cancels the old batch and isolates late responses from the new preview',
  async (change) => {
    const pending = new Map<number, ReturnType<typeof previewResponse>>()
    const signals: AbortSignal[] = []
    vi.mocked(imageRequest).mockImplementation((url, _method, data, signal) => {
      if (url.includes('/samples?')) {
        const params = new URL(url, 'https://example.test').searchParams
        const changed =
          params.get('page') === '2' || params.get('provider') === 'other'
        return Promise.resolve({
          items: changed ? sampleFixture(1, 11) : sampleFixture(5),
          total: 21,
        })
      }
      const response = previewResponse()
      pending.set((data as { event_id: number }).event_id, response)
      if (!signal) throw new Error('Preview cancellation signal missing')
      signals.push(signal)
      // Deliberately ignore cancellation to exercise late network completion.
      return response.promise
    })
    show(
      <RetrySamples
        canManage
        policy={fixture()}
        onDraft={vi.fn()}
        onTest={vi.fn()}
      />
    )
    const user = userEvent.setup()
    await screen.findByText('Failure 5')
    await user.click(
      screen.getByRole('button', { name: 'Test visible samples' })
    )
    expect(pending.size).toBe(4)
    if (change === 'page') {
      await user.click(screen.getByRole('button', { name: 'Next' }))
    } else {
      await user.type(screen.getByLabelText('Provider'), 'other')
      await user.click(screen.getByRole('button', { name: 'Apply filters' }))
    }
    expect(signals).toHaveLength(4)
    expect(signals.every((signal) => signal.aborted)).toBe(true)
    await screen.findByText('Failure 11')
    await user.click(
      screen.getByRole('button', { name: 'Test visible samples' })
    )
    expect(signals.at(-1)?.aborted).toBe(false)
    await act(async () => {
      for (const id of [1, 2, 3, 4]) {
        pending.get(id)?.resolve(previewDecision(id))
      }
    })
    expect([...pending.keys()]).toEqual([1, 2, 3, 4, 11])
    expect(screen.queryByText(/Custom rules matched/)).not.toBeInTheDocument()
    await act(async () => pending.get(11)?.resolve(previewDecision(11)))
    const summary = await screen.findByText(
      'Custom rules matched 1 of 1 samples'
    )
    await user.click(summary)
    const results = within(
      screen.getByRole('table', { name: 'Test visible samples' })
    )
    expect(results.getByText('Failure 11')).toBeVisible()
    expect(results.queryByText('Failure 1')).not.toBeInTheDocument()
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
  }
)

test('unmounting cancels running previews and never starts the queued samples', async () => {
  const pending: ReturnType<typeof previewResponse>[] = []
  const signals: AbortSignal[] = []
  vi.mocked(imageRequest).mockImplementation((url, _method, _data, signal) => {
    if (url.includes('/samples?')) {
      return Promise.resolve({ items: sampleFixture(5), total: 5 })
    }
    const response = previewResponse()
    pending.push(response)
    if (!signal) throw new Error('Preview cancellation signal missing')
    signals.push(signal)
    return response.promise
  })
  const page = show(
    <RetrySamples
      canManage
      policy={fixture()}
      onDraft={vi.fn()}
      onTest={vi.fn()}
    />
  )
  await screen.findByText('Failure 5')
  await userEvent
    .setup()
    .click(screen.getByRole('button', { name: 'Test visible samples' }))
  expect(pending).toHaveLength(4)
  page.unmount()
  expect(signals.every((signal) => signal.aborted)).toBe(true)
  await act(async () => {
    pending.forEach((response, index) =>
      response.resolve(previewDecision(index + 1))
    )
  })
  expect(pending).toHaveLength(4)
})
