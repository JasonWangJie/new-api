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

import { imageRequest } from '../../api'
import { RetryPolicyForm } from '../index'
import {
  newRetryRule,
  RETRY_POLICY_URL,
  retryPolicyChanges,
  copyRetryRule,
} from '../policy'
import { RetryPreview } from '../preview'
import { RetryRuleEditor } from '../rule-editor'
import { RetrySamples } from '../samples'
import type { RetryPolicy } from '../types'

vi.mock('../../api', () => ({ imageRequest: vi.fn() }))
const clients: QueryClient[] = []
function show(element: React.ReactNode) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
  clients.push(client)
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
afterEach(() => {
  cleanup()
  clients.splice(0).forEach((client) => client.clear())
  vi.clearAllMocks()
  vi.useRealTimers()
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
