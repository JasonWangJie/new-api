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
import { useMutation, useQuery } from '@tanstack/react-query'
import { useState } from 'react'
import { useTranslation } from 'react-i18next'

import { StaticDataTable } from '@/components/data-table'
import { Dialog } from '@/components/dialog'
import { Button } from '@/components/ui/button'
import { formatDateStr, formatTimestampToDate } from '@/lib/format'

import { imageRequest } from '../api'
import { imageLabel } from '../lib/image-labels'
import { RetryField, RetrySelect } from './fields'
import { ACTION_LABELS, CATEGORY_LABELS, RETRY_POLICY_URL } from './policy'
import type {
  RetryDecision,
  RetryDraftSource,
  RetryFact,
  RetryPolicy,
  RetrySample,
  RetrySampleEvent,
  RetryAction,
} from './types'

export function RetrySamples(props: {
  canManage: boolean
  policy: RetryPolicy
  onDraft: (source: RetryDraftSource) => void
  onTest: (source: RetryDraftSource) => void
}) {
  const { t } = useTranslation()
  const [page, setPage] = useState(1)
  const [selected, setSelected] = useState<RetrySample | null>(null)
  const [filters, setFilters] = useState(() => {
    const today = formatDateStr(new Date())
    return {
      media_type: '',
      provider: '',
      model: '',
      stage: '',
      channel_id: '',
      category: '',
      matched: '',
      start_date: today,
      end_date: today,
    }
  })
  const [applied, setApplied] = useState(filters)
  const params = new URLSearchParams(
    Object.entries(applied).filter(
      ([key, value]) => value && !key.endsWith('_date')
    )
  )
  if (applied.start_date) {
    params.set(
      'start_timestamp',
      String(
        Math.floor(new Date(`${applied.start_date}T00:00:00`).getTime() / 1000)
      )
    )
  }
  if (applied.end_date) {
    params.set(
      'end_timestamp',
      String(
        Math.floor(new Date(`${applied.end_date}T23:59:59`).getTime() / 1000)
      )
    )
  }
  params.set('page', String(page))
  params.set('page_size', '20')
  const samples = useQuery({
    queryKey: ['retry-policy-samples', applied, page],
    queryFn: () =>
      imageRequest<{
        items: (RetrySample & { latest_event_id: number; matched: boolean })[]
        total: number
      }>(`${RETRY_POLICY_URL}/samples?${params}`),
  })
  const batch = useMutation({
    mutationFn: async () => {
      const results = []
      for (const sample of samples.data?.items ?? []) {
        const result = await imageRequest<{
          decision: RetryDecision
          decision_source?: string
        }>(`${RETRY_POLICY_URL}/preview`, 'POST', {
          policy: props.policy,
          event_id: sample.latest_event_id,
        })
        results.push({
          sample,
          decision: result.decision,
          decisionSource: result.decision_source,
        })
      }
      return results
    },
  })
  const event = useMutation({
    mutationFn: async (input: { id: number; draft: boolean }) => {
      const record = await imageRequest<{ fact: RetryFact; task_id: string }>(
        `${RETRY_POLICY_URL}/samples/${input.id}`
      )
      return {
        source: {
          fact: record.fact,
          event_id: input.id,
          task_id: record.task_id,
        },
        draft: input.draft,
      }
    },
    onSuccess: (result) =>
      result.draft ? props.onDraft(result.source) : props.onTest(result.source),
  })
  return (
    <div className='space-y-4'>
      <form
        className='grid gap-3 sm:grid-cols-3'
        onSubmit={(e) => {
          e.preventDefault()
          setApplied(filters)
          setPage(1)
          setSelected(null)
        }}
      >
        <RetrySelect
          label='Media type'
          value={filters.media_type}
          options={{ '': 'All', image: 'Image', video: 'Video' }}
          onChange={(media_type) => setFilters({ ...filters, media_type })}
        />
        {(
          [
            'provider',
            'model',
            'stage',
            'channel_id',
            'category',
            'start_date',
            'end_date',
          ] as const
        ).map((key) => (
          <RetryField
            key={key}
            label={
              {
                provider: 'Provider',
                model: 'Model',
                stage: 'Stage',
                channel_id: 'Channel ID',
                category: 'Error category',
                start_date: 'Start date',
                end_date: 'End date',
              }[key]
            }
            type={key.endsWith('date') ? 'date' : 'text'}
            value={filters[key]}
            onChange={(value) => setFilters({ ...filters, [key]: value })}
          />
        ))}
        <RetrySelect
          label='Rule match'
          value={filters.matched}
          options={{ '': 'All', true: 'Matched', false: 'Unmatched' }}
          onChange={(matched) => setFilters({ ...filters, matched })}
        />
        <Button type='submit'>{t('Apply filters')}</Button>
      </form>
      <Button
        variant='outline'
        disabled={batch.isPending || !samples.data?.items.length}
        onClick={() => batch.mutate()}
      >
        {t('Test visible samples')}
      </Button>
      {batch.data && (
        <details className='rounded-lg border p-3'>
          <summary className='cursor-pointer' role='status'>
            {t('Custom rules matched {{matched}} of {{total}} samples', {
              matched: batch.data.filter((item) => item.decision.matched)
                .length,
              total: batch.data.length,
            })}
          </summary>
          <StaticDataTable
            data={batch.data}
            getRowKey={({ sample }) => sample.fingerprint}
            columns={[
              {
                id: 'message',
                header: t('Error message'),
                className: 'min-w-48',
                cell: ({ sample }) => (
                  <p className='max-w-lg break-words whitespace-normal'>
                    {sample.message}
                  </p>
                ),
              },
              {
                id: 'rule',
                header: t('Matched rule'),
                cell: ({ decision, decisionSource }) =>
                  decisionSource === 'current_builtin'
                    ? t('Current built-in policy')
                    : decision.rule_id || t('Unmatched'),
              },
              {
                id: 'category',
                header: t('Error category'),
                cell: ({ decision }) =>
                  t(
                    CATEGORY_LABELS[decision.category ?? ''] ??
                      decision.category ??
                      '—'
                  ),
              },
              {
                id: 'action',
                header: t('Planned action'),
                cell: ({ decision }) =>
                  t(
                    ACTION_LABELS[decision.action as RetryAction] ??
                      imageLabel(decision.action)
                  ),
              },
              {
                id: 'blocked',
                header: t('Blocking reason'),
                cell: ({ decision }) => decision.blocked_reason || '—',
              },
            ]}
          />
        </details>
      )}
      {batch.error && <p role='alert'>{batch.error.message}</p>}
      {samples.isPending && <p role='status'>{t('Loading...')}</p>}
      {samples.error && <p role='alert'>{samples.error.message}</p>}
      {event.error && <p role='alert'>{event.error.message}</p>}
      <StaticDataTable
        data={samples.data?.items ?? []}
        emptyContent={t('No error samples')}
        getRowKey={(sample) => sample.fingerprint}
        columns={[
          {
            id: 'message',
            header: t('Error message'),
            className: 'min-w-64',
            cell: (sample) => (
              <div className='max-w-lg space-y-1 whitespace-normal'>
                <p className='break-words'>{sample.message}</p>
                <p className='text-muted-foreground text-xs'>
                  {sample.media_type} · {sample.provider} · {sample.model} ·{' '}
                  {sample.stage}
                </p>
                <p className='text-muted-foreground text-xs'>
                  {sample.http_status || '—'} · {sample.error_code || '—'}
                </p>
              </div>
            ),
          },
          {
            id: 'count',
            header: t('Occurrences'),
            cell: (sample) => sample.count,
          },
          {
            id: 'tasks',
            header: t('Tasks'),
            cell: (sample) => sample.task_count,
          },
          {
            id: 'rule',
            header: t('Matched rule'),
            cell: (sample) => sample.rule_id || t('Unmatched'),
          },
          {
            id: 'executed',
            header: t('Executed actions'),
            cell: (sample) => sample.executed_count ?? 0,
          },
          {
            id: 'succeeded',
            header: t('Tasks later succeeded'),
            cell: (sample) => sample.succeeded_task_count ?? 0,
          },
          {
            id: 'last',
            header: t('Last seen'),
            cell: (sample) => formatTimestampToDate(sample.last_seen),
          },
          {
            id: 'actions',
            header: t('Actions'),
            cell: (sample) => (
              <div className='flex flex-wrap gap-2'>
                <Button variant='outline' onClick={() => setSelected(sample)}>
                  {t('Details')}
                </Button>
                <Button
                  variant='outline'
                  disabled={event.isPending}
                  onClick={() =>
                    event.mutate({ id: sample.latest_event_id, draft: false })
                  }
                >
                  {t('Test')}
                </Button>
                {props.canManage && (
                  <Button
                    variant='outline'
                    disabled={event.isPending}
                    onClick={() =>
                      event.mutate({ id: sample.latest_event_id, draft: true })
                    }
                  >
                    {t('Create rule from error')}
                  </Button>
                )}
              </div>
            ),
          },
        ]}
      />
      <div className='flex justify-end gap-2'>
        <Button
          variant='outline'
          disabled={page === 1}
          onClick={() => setPage(page - 1)}
        >
          {t('Previous')}
        </Button>
        <Button
          variant='outline'
          disabled={page * 20 >= (samples.data?.total ?? 0)}
          onClick={() => setPage(page + 1)}
        >
          {t('Next')}
        </Button>
      </div>
      {selected && (
        <RetrySampleEvents
          sample={selected}
          filters={params.toString()}
          canManage={props.canManage}
          onClose={() => setSelected(null)}
          onSelect={(id, draft) => {
            setSelected(null)
            event.mutate({ id, draft })
          }}
        />
      )}
    </div>
  )
}

function RetrySampleEvents(props: {
  sample: RetrySample
  filters: string
  canManage: boolean
  onClose: () => void
  onSelect: (id: number, draft: boolean) => void
}) {
  const { t } = useTranslation()
  const [page, setPage] = useState(1)
  const params = new URLSearchParams(props.filters)
  params.set('page', String(page))
  const events = useQuery({
    queryKey: [
      'retry-sample-events',
      props.sample.fingerprint,
      props.filters,
      page,
    ],
    queryFn: () =>
      imageRequest<{ items: RetrySampleEvent[]; total: number }>(
        `${RETRY_POLICY_URL}/samples/${encodeURIComponent(props.sample.fingerprint)}/events?${params}`
      ),
  })
  return (
    <Dialog
      open
      onOpenChange={(open) => {
        if (!open) props.onClose()
      }}
      title={`${t('Error samples')} · ${t('Details')}`}
      description={props.sample.message}
      descriptionClassName='break-words'
      contentClassName='sm:max-w-5xl'
      footer={
        <>
          <Button
            variant='outline'
            disabled={page === 1}
            onClick={() => setPage(page - 1)}
          >
            {t('Previous')}
          </Button>
          <Button
            variant='outline'
            disabled={page * 20 >= (events.data?.total ?? 0)}
            onClick={() => setPage(page + 1)}
          >
            {t('Next')}
          </Button>
        </>
      }
    >
      {events.isPending && <p role='status'>{t('Loading...')}</p>}
      {events.error && <p role='alert'>{events.error.message}</p>}
      <StaticDataTable
        data={events.data?.items ?? []}
        getRowKey={(record) => String(record.id)}
        emptyContent={t('No error samples')}
        columns={[
          {
            id: 'task',
            header: t('Task ID'),
            className: 'min-w-48',
            cell: (record) => (
              <div className='space-y-1 whitespace-normal'>
                <p className='break-all'>{record.task_id}</p>
                <p className='text-muted-foreground text-xs'>
                  {formatTimestampToDate(record.created_at)}
                </p>
              </div>
            ),
          },
          {
            id: 'channel',
            header: t('Channel ID'),
            cell: (record) => record.channel_id || '—',
          },
          {
            id: 'category',
            header: t('Error category'),
            cell: (record) =>
              t(CATEGORY_LABELS[record.category] ?? record.category),
          },
          {
            id: 'rule',
            header: t('Matched rule'),
            cell: (record) => (
              <div className='whitespace-normal'>
                <p>{record.rule_id || t('Unmatched')}</p>
                <p>
                  {t('Version')}: {record.revision}
                </p>
              </div>
            ),
          },
          {
            id: 'action',
            header: t('Actual action'),
            cell: (record) =>
              t(
                ACTION_LABELS[record.action as RetryAction] ??
                  imageLabel(record.action)
              ),
          },
          {
            id: 'executed',
            header: t('Executed actions'),
            cell: (record) => t(record.executed ? 'Yes' : 'No'),
          },
          {
            id: 'outcome',
            header: t('Status'),
            cell: (record) => t(imageLabel(record.outcome || 'pending')),
          },
          {
            id: 'actions',
            header: t('Actions'),
            cell: (record) => (
              <div className='flex flex-wrap gap-2'>
                <Button
                  variant='outline'
                  onClick={() => props.onSelect(record.id, false)}
                >
                  {t('Test')}
                </Button>
                {props.canManage && (
                  <Button
                    variant='outline'
                    onClick={() => props.onSelect(record.id, true)}
                  >
                    {t('Create rule from error')}
                  </Button>
                )}
              </div>
            ),
          },
        ]}
      />
    </Dialog>
  )
}
