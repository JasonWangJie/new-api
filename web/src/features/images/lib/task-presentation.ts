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
import type { ImageTask, ImageTaskList } from '../types'

const FINAL_ISSUE_STATUSES = new Set([
  'failed',
  'expired',
  'execution_unknown',
  'storage_failed',
  'billing_failed',
])
const RETRY_ACTIONS = new Set([
  'same_account_retry',
  'switch_account',
  'retry_submission',
  'local_reference_fallback',
  'poll_same_job',
  'resume_output',
])
const WAIT_ACTIONS = new Set([
  'wait_local_dependency',
  'wait_account_cooldown',
  'wait_supplier_budget',
  'wait_capacity',
])

export function imageTaskCanResume(
  task: ImageTask,
  admin: boolean,
  canManage: boolean
): boolean {
  return (
    task.can_resume &&
    (canManage || !admin) &&
    (admin || task.error_code !== 'billing_quota_uncertain')
  )
}

export function imageTaskProgress(task: ImageTask): number | null {
  if (task.status === 'succeeded' || FINAL_ISSUE_STATUSES.has(task.status)) {
    return null
  }
  return Number.isFinite(task.progress)
    ? Math.min(100, Math.max(0, task.progress))
    : 0
}

export function imageTaskIssue(task: ImageTask): {
  summary: string
  suggestion: string
  stage: string
  pendingAction?: 'retry' | 'wait'
  nextAttemptAt: number
} | null {
  if (task.status === 'succeeded') return null
  const decision = task.retry_decision
  const active = ['queued', 'invoking', 'processing'].includes(task.status)
  // A successful poll can retain a previous diagnostic while the accepted job runs.
  if (
    active &&
    decision?.action === 'poll_same_job' &&
    ['upstream_pending', 'recovered', 'capacity_wait'].includes(decision.rule)
  ) {
    return null
  }
  const finalIssue = FINAL_ISSUE_STATUSES.has(task.status)
  const hasError = Boolean(
    task.error_code ||
    task.failure_code ||
    task.error_summary ||
    task.error_message
  )
  if (
    !finalIssue &&
    !(active && (hasError || WAIT_ACTIONS.has(decision?.action || '')))
  ) {
    return null
  }
  let pendingAction: 'retry' | 'wait' | undefined
  const canContinue =
    active || ['storage_failed', 'billing_failed'].includes(task.status)
  if (canContinue && task.error_retryable !== false && decision) {
    if (WAIT_ACTIONS.has(decision.action)) pendingAction = 'wait'
    if (RETRY_ACTIONS.has(decision.action)) pendingAction = 'retry'
  }
  let suggestion = task.error_suggestion || ''
  if (!suggestion && pendingAction) {
    if (decision?.action === 'poll_same_job') {
      suggestion =
        'The existing upstream job will be checked again; no new generation will be submitted.'
    } else if (decision?.action === 'resume_output') {
      suggestion =
        'The existing output will be processed again; generation will not be repeated.'
    } else if (decision?.action === 'wait_local_dependency') {
      suggestion =
        'The task is waiting for a local dependency to recover before dispatch.'
    } else if (pendingAction === 'wait') {
      suggestion =
        'The task is waiting for capacity before another upstream submission.'
    } else {
      suggestion =
        'The task is scheduled for another submission within its retry limits.'
    }
  }
  if (
    !suggestion &&
    ['manual_recovery', 'reconcile_unknown'].includes(decision?.action || '')
  ) {
    suggestion =
      'Do not submit again until an administrator has checked the upstream result and billing.'
  }
  return {
    summary:
      task.error_summary ||
      (task.error_message ? 'Failure reason' : task.status),
    suggestion,
    stage: task.error_stage || decision?.stage || task.stage || '',
    pendingAction,
    nextAttemptAt: pendingAction
      ? task.next_attempt_at || decision?.next_attempt_at || 0
      : 0,
  }
}

export function imageTaskElapsedSeconds(
  task: ImageTask,
  now = Date.now() / 1000
): number | null {
  if (!task.created_at) return null
  return Math.max(0, (task.finished_at || now) - task.created_at)
}

export function imageTaskSpecifications(task: ImageTask): string {
  const requested = task.requested_resolution || task.requested_size
  if (requested && task.actual_size && requested !== task.actual_size) {
    return `${requested} → ${task.actual_size}`
  }
  return task.actual_size || requested || '—'
}

export function imageTaskSuccessRate(
  stats?: ImageTaskList['stats']
): number | null {
  const succeeded = stats?.succeeded || 0
  const finished = succeeded + (stats?.failed || 0)
  return finished > 0 ? (succeeded / finished) * 100 : null
}
