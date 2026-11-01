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
import type {
  RetryAction,
  RetryDraftSource,
  RetryRule,
  RetryStep,
} from './types'

export const RETRY_POLICY_URL = '/api/option/images/retry-policy'
export const ACTION_LABELS: Record<RetryAction, string> = {
  same_channel: 'Retry on the same channel',
  other_channel: 'Retry on another channel',
  local_same_channel: 'Download locally, then retry on the same channel',
  local_other_channel: 'Download locally, then retry on another channel',
  stop: 'Stop automatic retries',
  manual_review: 'Manual reconciliation',
  poll_same_job: 'Query the existing task',
  resume_download: 'Retry result storage',
  resume_billing: 'Retry settlement',
}
export const CATEGORY_LABELS: Record<string, string> = {
  reference_download: 'Reference download failure',
  transient: 'Temporary upstream failure',
  capacity: 'Rate limit or capacity',
  account: 'Upstream account issue',
  invalid_input: 'Invalid input',
  content_policy: 'Content policy',
  execution_unknown: 'Unknown execution result',
  storage: 'Result storage failure',
  billing: 'Settlement failure',
  unclassified: 'Unclassified',
}
export const ELIGIBILITY_LABELS: Record<string, string> = {
  task_context_unavailable: 'Current task context is unavailable',
  historical_task_not_restarted:
    'Historical tasks require an explicit manual restart',
  eligibility_changed: 'Task eligibility has changed',
  reservation_unavailable: 'Frozen billing reservation is unavailable',
  no_compatible_channel: 'No compatible execution channel is available',
  account_cooldown: 'Eligible accounts are cooling down',
  reference_unavailable: 'Reference media is unavailable or expired',
  routing_controls_unavailable: 'Routing checks are temporarily unavailable',
  local_capacity_busy: 'Local execution capacity is busy',
  output_already_available:
    'Successful output can only resume storage or billing',
  dispatch_checks_repeated_before_send:
    'Permissions and budgets are checked again before sending',
  remote_reference_download_not_performed:
    'Preview does not download remote reference media',
  text_matching_only:
    'Text matching only. A task is required to verify execution eligibility.',
}
export function newRetryStep(action: RetryAction = 'same_channel'): RetryStep {
  return {
    id: crypto.randomUUID(),
    action,
    max_attempts: 1,
    delay_seconds: 15,
    backoff: 'fixed',
  }
}
export function newRetryRule(source?: RetryDraftSource): RetryRule {
  const fact = source?.fact
  return {
    id: crypto.randomUUID(),
    name: '',
    enabled: true,
    category: 'unclassified',
    scope: {
      media_types: fact?.media_type ? [fact.media_type] : [],
      providers: fact?.provider ? [fact.provider] : [],
      models: fact?.model ? [fact.model] : [],
      channel_ids: fact?.channel_id ? [fact.channel_id] : [],
      stages: fact?.stage ? [fact.stage] : [],
    },
    match: {
      contains_any: fact?.message ? [fact.message.slice(0, 512)] : [],
      http_statuses: fact?.http_status ? [fact.http_status] : [],
      error_codes: fact?.error_code ? [fact.error_code] : [],
    },
    steps: [newRetryStep()],
    on_exhausted: 'manual_review',
  }
}
export function copyRetryRule(rule: RetryRule): RetryRule {
  return {
    ...structuredClone(rule),
    id: crypto.randomUUID(),
    steps: rule.steps.map((step) => ({ ...step, id: crypto.randomUUID() })),
  }
}
export function moveRetryItem<T>(
  items: T[],
  index: number,
  offset: number
): T[] {
  const result = [...items]
  const target = index + offset
  if (target < 0 || target >= items.length) return result
  ;[result[index], result[target]] = [result[target], result[index]]
  return result
}
export function retryLines(value: string): string[] {
  return [
    ...new Set(
      value
        .split('\n')
        .map((line) => line.trim())
        .filter(Boolean)
    ),
  ]
}

export function retryPolicyChanges(
  before: unknown,
  after: unknown,
  path = ''
): { path: string; before: unknown; after: unknown }[] {
  if (Object.is(before, after)) return []
  if (
    before !== null &&
    after !== null &&
    typeof before === 'object' &&
    typeof after === 'object'
  ) {
    const left = before as Record<string, unknown>
    const right = after as Record<string, unknown>
    return [...new Set([...Object.keys(left), ...Object.keys(right)])]
      .filter((key) => path || key !== 'revision')
      .flatMap((key) =>
        retryPolicyChanges(left[key], right[key], path ? `${path}.${key}` : key)
      )
  }
  return [{ path, before, after }]
}
