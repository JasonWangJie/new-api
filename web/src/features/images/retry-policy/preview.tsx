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
import { useMutation } from '@tanstack/react-query'
import { useState } from 'react'
import { useTranslation } from 'react-i18next'

import { Dialog } from '@/components/dialog'
import { Button } from '@/components/ui/button'

import { imageRequest } from '../api'
import { imageLabel } from '../lib/image-labels'
import { RetryField } from './fields'
import {
  ACTION_LABELS,
  CATEGORY_LABELS,
  ELIGIBILITY_LABELS,
  RETRY_POLICY_URL,
} from './policy'
import type {
  RetryAction,
  RetryDecision,
  RetryDraftSource,
  RetryPolicy,
  RetryState,
  RetryEligibility,
} from './types'

export function RetryPreview(props: {
  policy: RetryPolicy
  source?: RetryDraftSource
  onClose: () => void
}) {
  const { t } = useTranslation()
  const [message, setMessage] = useState(props.source?.fact.message ?? '')
  const [edited, setEdited] = useState(false)
  const preview = useMutation({
    mutationFn: () =>
      imageRequest<{
        decision: RetryDecision
        decision_source?:
          | 'current_builtin'
          | 'current_custom'
          | 'custom_matcher'
        mode: string
        state?: RetryState
        eligibility?: RetryEligibility
        recorded_decision?: RetryDecision
        recorded_decision_source?: string
        state_source?: string
      }>(`${RETRY_POLICY_URL}/preview`, 'POST', {
        policy: props.policy,
        fact: { ...props.source?.fact, message },
        task_id: edited ? undefined : props.source?.task_id,
        event_id: edited ? undefined : props.source?.event_id,
      }),
  })
  let eligibilityLabel = 'Not checked'
  if (preview.data?.eligibility?.checked) {
    eligibilityLabel = preview.data.eligibility.eligible
      ? 'Available'
      : 'Unavailable'
  }
  const matchedRule = props.policy.rules.find(
    (rule) => rule.id === preview.data?.decision.rule_id
  )
  return (
    <Dialog
      open
      onOpenChange={(open) => {
        if (!open) props.onClose()
      }}
      title={t('Test retry policy')}
      description={t(
        'Testing never sends an upstream request or changes billing.'
      )}
      footer={
        <Button
          disabled={preview.isPending || !message.trim()}
          onClick={() => preview.mutate()}
        >
          {t('Run test')}
        </Button>
      }
    >
      <div className='space-y-4'>
        <RetryField
          label='Error message'
          multiline
          value={message}
          onChange={(value) => {
            setMessage(value)
            setEdited(true)
            preview.reset()
          }}
        />
        {(edited || (!props.source?.task_id && !props.source?.event_id)) && (
          <p className='text-muted-foreground text-sm'>
            {t(
              'Text matching only. A task is required to verify execution eligibility.'
            )}
          </p>
        )}
        {preview.error && (
          <p role='alert' className='text-destructive'>
            {preview.error.message}
          </p>
        )}
        {preview.data && (
          <div
            role='status'
            className='bg-muted/30 space-y-3 rounded-lg border p-4'
          >
            <p className='font-semibold'>
              {preview.data.decision_source === 'current_builtin' ? (
                t('Current built-in policy')
              ) : (
                <>
                  {preview.data.decision.matched
                    ? t('Matched rule')
                    : t('No matching custom rule')}
                  : {preview.data.decision.rule_id || '—'}
                </>
              )}
            </p>
            <dl className='grid grid-cols-2 gap-2 text-sm'>
              <dt>{t('Error category')}</dt>
              <dd>
                {t(
                  CATEGORY_LABELS[preview.data.decision.category ?? ''] ??
                    (preview.data.decision.category || '—')
                )}
              </dd>
              <dt>{t('Version')}</dt>
              <dd>{preview.data.decision.revision}</dd>
              <dt>{t('Requested action')}</dt>
              <dd>
                {t(
                  ACTION_LABELS[
                    preview.data.decision.requested_action as RetryAction
                  ] ?? imageLabel(preview.data.decision.requested_action || '—')
                )}
              </dd>
              <dt>{t('Planned action')}</dt>
              <dd>
                {t(
                  ACTION_LABELS[preview.data.decision.action as RetryAction] ??
                    imageLabel(preview.data.decision.action || '—')
                )}
              </dd>
              <dt>{t('Delay (seconds)')}</dt>
              <dd>{preview.data.decision.delay_seconds ?? 0}</dd>
              <dt>{t('Blocking reason')}</dt>
              <dd className='break-words'>
                {preview.data.decision.blocked_reason || '—'}
              </dd>
            </dl>
            {preview.data.state && preview.data.mode === 'sample' && (
              <dl className='grid grid-cols-2 gap-2 border-t pt-3 text-sm'>
                <dt>
                  {t('Task')} · {t('Used / Remaining')}
                </dt>
                <dd>
                  {preview.data.state.total_used} /{' '}
                  {Math.max(
                    0,
                    preview.data.state.total_limit -
                      preview.data.state.total_used
                  )}
                </dd>
              </dl>
            )}
            {preview.data.decision.matched && (
              <dl className='grid grid-cols-2 gap-2 border-t pt-3 text-sm'>
                <dt>
                  {t('Rule')} · {t('Used / Remaining')}
                </dt>
                <dd>
                  {preview.data.decision.rule_used ?? 0} /{' '}
                  {Math.max(
                    0,
                    (preview.data.decision.rule_limit ?? 0) -
                      (preview.data.decision.rule_used ?? 0)
                  )}
                </dd>
                <dt>
                  {t('Step')} · {t('Used / Remaining')}
                </dt>
                <dd>
                  {preview.data.decision.step_used ?? 0} /{' '}
                  {Math.max(
                    0,
                    (preview.data.decision.step_limit ?? 0) -
                      (preview.data.decision.step_used ?? 0)
                  )}
                </dd>
              </dl>
            )}
            {preview.data.eligibility && (
              <section className='space-y-2 border-t pt-3 text-sm'>
                <p className='font-medium'>
                  {t('Current execution eligibility')}: {t(eligibilityLabel)}
                </p>
                {preview.data.eligibility.reason && (
                  <p>
                    {t(
                      ELIGIBILITY_LABELS[preview.data.eligibility.reason] ??
                        imageLabel(preview.data.eligibility.reason)
                    )}
                  </p>
                )}
                {!!preview.data.eligibility.candidate_channel_ids?.length && (
                  <p className='break-words'>
                    {t('Channels')}:{' '}
                    {preview.data.eligibility.candidate_channel_ids.join(', ')}
                  </p>
                )}
                {preview.data.eligibility.warnings?.map((warning) => (
                  <p className='text-muted-foreground' key={warning}>
                    {t(ELIGIBILITY_LABELS[warning] ?? imageLabel(warning))}
                  </p>
                ))}
              </section>
            )}
            {preview.data.recorded_decision && (
              <section className='space-y-2 border-t pt-3 text-sm'>
                <p className='font-medium'>
                  {t('Recorded action')}:{' '}
                  {t(
                    ACTION_LABELS[
                      preview.data.recorded_decision.action as RetryAction
                    ] ?? imageLabel(preview.data.recorded_decision.action)
                  )}
                </p>
                <p className='text-muted-foreground'>
                  {t('This action was recorded when the error occurred.')}
                </p>
              </section>
            )}
            {preview.data.mode === 'sample' && (
              <p className='text-muted-foreground text-sm'>
                {t(
                  preview.data.state_source === 'current_task'
                    ? 'Uses the recorded error with current task budgets and deadlines. Eligibility is checked again before execution.'
                    : 'Uses the recorded task state and checks deadlines at the current time. Current eligibility is rechecked before execution.'
                )}
              </p>
            )}
            {matchedRule && (
              <details className='text-sm'>
                <summary className='cursor-pointer'>
                  {t('Rule match')}: {matchedRule.name}
                </summary>
                <pre className='mt-2 max-h-52 overflow-auto text-xs break-all whitespace-pre-wrap'>
                  {JSON.stringify(
                    { scope: matchedRule.scope, match: matchedRule.match },
                    null,
                    2
                  )}
                </pre>
              </details>
            )}
            {preview.data.mode === 'text_only' && (
              <p className='text-muted-foreground text-sm'>
                {t(
                  'Text matching only. A task is required to verify execution eligibility.'
                )}
              </p>
            )}
          </div>
        )}
      </div>
    </Dialog>
  )
}
