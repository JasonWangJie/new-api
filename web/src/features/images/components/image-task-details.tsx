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
import { useQuery } from '@tanstack/react-query'
import { ArrowUpRight, Ban, ChevronDown } from 'lucide-react'
import type { ReactNode } from 'react'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'

import { CopyButton } from '@/components/copy-button'
import { StaticDataTable } from '@/components/data-table'
import { Dialog } from '@/components/dialog'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import {
  Collapsible,
  CollapsibleContent,
  CollapsibleTrigger,
} from '@/components/ui/collapsible'
import { useCopyToClipboard } from '@/hooks/use-copy-to-clipboard'
import { formatTimestampToDate, formatUseTime } from '@/lib/format'
import { cn } from '@/lib/utils'

import { getImageTask, imageRequest } from '../api'
import {
  imageLabel,
  imageTimelineDotClass,
  imageTimelineLabel,
} from '../lib/image-labels'
import { terminalImageStatus } from '../lib/image-request'
import { imageDetailHeaderClass } from '../lib/image-visuals'
import {
  imageTaskElapsedSeconds,
  imageTaskSpecifications,
} from '../lib/task-presentation'
import {
  ImagePlatformBadge,
  ImageRequestTypeBadge,
  ImageStatusBadge,
} from './image-badges'
import { ImageResults } from './image-results'
import { VideoResults } from './video-results'

function TaskDetailFields(props: {
  items: { label: string; value: ReactNode }[]
  className?: string
}) {
  return (
    <dl
      className={cn(
        'grid grid-cols-2 gap-x-6 gap-y-5 text-sm md:grid-cols-4',
        props.className
      )}
    >
      {props.items.map((item) => (
        <div key={item.label} className='min-w-0 space-y-1.5'>
          <dt className='text-muted-foreground text-xs'>{item.label}</dt>
          <dd className='font-medium [overflow-wrap:anywhere] break-words'>
            {item.value || '—'}
          </dd>
        </div>
      ))}
    </dl>
  )
}

function DetailSection(props: {
  title: string
  accentClassName?: string
  children: ReactNode
  action?: ReactNode
}) {
  return (
    <section
      className={cn(
        'bg-card/70 space-y-4 rounded-xl border p-4 shadow-sm',
        props.accentClassName
      )}
    >
      <div className='flex items-center justify-between gap-3'>
        <h3 className='text-sm font-semibold'>{props.title}</h3>
        {props.action}
      </div>
      {props.children}
    </section>
  )
}

export function ImageTaskDetails(props: {
  id: string
  admin: boolean
  userId: number
  onClose: () => void
  canManage: boolean
  onManage: (action: 'resume' | 'terminate') => void
}) {
  const { t } = useTranslation()
  const { copyToClipboard } = useCopyToClipboard()
  const task = useQuery({
    queryKey: ['image-task-details', props.userId, props.admin, props.id],
    queryFn: ({ signal }) => getImageTask(props.admin, props.id, signal),
    staleTime: 10_000,
    refetchInterval: (query) =>
      query.state.data &&
      terminalImageStatus(
        query.state.data.task.status,
        query.state.data.task.next_attempt_at
      )
        ? false
        : 5000,
  })
  const results = task.data?.results || []
  const images = useQuery({
    queryKey: [
      'image-task-detail-results',
      props.userId,
      props.admin,
      props.id,
      results
        .map(
          (result) => `${result.image_index}:${result.url || result.view_url}`
        )
        .join('|'),
    ],
    queryFn: async ({ signal }) =>
      Promise.all(
        results.map(async (result) => {
          const url =
            result.url ||
            (
              await imageRequest<{ url: string }>(
                result.view_url,
                'GET',
                undefined,
                signal
              )
            ).url
          return {
            id: String(result.image_index),
            url,
            description: `${result.width} × ${result.height} · ${(result.byte_size / 1048576).toFixed(2)} MiB`,
          }
        })
      ),
    enabled: results.length > 0 && task.data?.task.media_type !== 'video',
    staleTime: 60_000,
  })
  const archive = async (index: string) => {
    try {
      await imageRequest('/api/user/image-library/from-task', 'POST', {
        task_id: props.id,
        image_index: Number(index),
      })
      toast.success(t('Archived to server storage'))
    } catch (error) {
      toast.error(
        error instanceof Error ? error.message : t('Image request failed')
      )
    }
  }
  const data = task.data?.task
  let references: string[] = []
  if (props.admin && data?.reference_urls) {
    try {
      const parsed: unknown = JSON.parse(data.reference_urls)
      if (Array.isArray(parsed)) {
        references = [
          ...new Set(
            parsed.filter((value): value is string => typeof value === 'string')
          ),
        ]
      }
    } catch {
      references = []
    }
  }
  const attempts = props.admin ? data?.attempt_history || [] : []
  const canResume = Boolean(
    data?.can_resume && (props.canManage || !props.admin)
  )
  const canTerminate = Boolean(
    data?.can_terminate &&
    (props.canManage || (!props.admin && data.media_type === 'video'))
  )
  return (
    <Dialog
      open
      onOpenChange={(open) => {
        if (!open) props.onClose()
      }}
      title={t('Media task details')}
      description={t(
        'Task timing, request specifications and generated results.'
      )}
      contentClassName='sm:max-w-6xl'
      headerClassName='border-b pb-4'
      descriptionClassName='sr-only'
      bodyClassName='space-y-6'
    >
      {task.isLoading && <p role='status'>{t('Loading...')}</p>}
      {task.isError && (
        <p role='alert' className='text-destructive'>
          {task.error.message}
        </p>
      )}
      {data && task.data && (
        <>
          <section
            className={cn(
              'space-y-4 rounded-xl border bg-gradient-to-br p-4 shadow-sm',
              imageDetailHeaderClass(data.status)
            )}
          >
            <div className='flex flex-wrap items-center justify-between gap-3'>
              <div className='flex flex-wrap items-center gap-2'>
                <ImageStatusBadge
                  status={data.status}
                  errorCode={data.error_code}
                />
                <ImagePlatformBadge platform={data.platform} />
                <ImageRequestTypeBadge requestType={data.request_type} />
              </div>
              {(canResume || canTerminate) && (
                <div className='flex gap-2'>
                  {canResume && (
                    <Button
                      size='sm'
                      variant='outline'
                      onClick={() => props.onManage('resume')}
                    >
                      {t('Resume')}
                    </Button>
                  )}
                  {canTerminate && (
                    <Button
                      size='sm'
                      variant='destructive'
                      onClick={() => props.onManage('terminate')}
                    >
                      <Ban className='size-3.5' aria-hidden />
                      {t('Terminate')}
                    </Button>
                  )}
                </div>
              )}
            </div>
            <div className='flex min-w-0 items-center gap-2'>
              <p className='min-w-0 font-mono text-xs font-semibold break-all'>
                {props.id}
              </p>
              <CopyButton
                value={props.id}
                tooltip={t('Copy Task ID')}
                className='size-8 shrink-0'
              />
            </div>
            {data.prompt_summary && (
              <p className='text-muted-foreground text-sm break-words whitespace-pre-wrap'>
                {data.prompt_summary}
              </p>
            )}
            {data.error_message && (
              <div
                role='status'
                className='text-destructive border-destructive/20 bg-destructive/5 space-y-1 rounded-lg border p-3 text-sm'
              >
                <p className='text-xs font-medium'>{t('Failure reason')}</p>
                <p className='break-words'>{data.error_message}</p>
              </div>
            )}
            {references.length > 0 && (
              <div className='space-y-2'>
                <h3 className='text-muted-foreground text-xs font-medium'>
                  {t('Reference images')}
                </h3>
                <p className='text-muted-foreground text-xs'>
                  {t(
                    'Click the URL to copy it. Use the button on the right to open the reference image in a new tab.'
                  )}
                </p>
                {references.map((url, index) => {
                  if (!/^https?:\/\//i.test(url)) {
                    return (
                      <p key={url} className='text-muted-foreground text-xs'>
                        {t('Reference image {{index}}', { index: index + 1 })} ·{' '}
                        {t('Embedded image')}
                      </p>
                    )
                  }
                  return (
                    <div
                      key={url}
                      className='bg-background/70 flex min-h-10 max-w-3xl items-center gap-2 rounded-md border px-3 py-2 text-xs'
                    >
                      <span className='text-primary shrink-0'>
                        {t('Reference image {{index}}', { index: index + 1 })}
                      </span>
                      <button
                        type='button'
                        className='hover:text-primary min-w-0 flex-1 truncate text-left font-mono'
                        title={url}
                        aria-label={`${t('Copy URL')} · ${t('Reference image {{index}}', { index: index + 1 })}`}
                        onClick={() => void copyToClipboard(url)}
                      >
                        {url}
                      </button>
                      <Button
                        type='button'
                        size='icon'
                        variant='ghost'
                        className='size-8 shrink-0'
                        aria-label={`${t('Open in new tab')} · ${t('Reference image {{index}}', { index: index + 1 })}`}
                        onClick={() =>
                          window.open(url, '_blank', 'noopener,noreferrer')
                        }
                      >
                        <ArrowUpRight className='size-4' aria-hidden />
                      </Button>
                    </div>
                  )
                })}
              </div>
            )}
          </section>
          <DetailSection
            title={t('Task time')}
            accentClassName='border-l-4 border-l-cyan-500/70'
          >
            <TaskDetailFields
              className='xl:grid-cols-6'
              items={[
                {
                  label: t('Submitted at'),
                  value: formatTimestampToDate(data.created_at),
                },
                {
                  label: t('Started at'),
                  value: formatTimestampToDate(data.started_at),
                },
                {
                  label: t('Finished at'),
                  value: formatTimestampToDate(data.finished_at),
                },
                {
                  label: t('Time spent'),
                  value: data.created_at
                    ? formatUseTime(imageTaskElapsedSeconds(data) || 0)
                    : '—',
                },
                {
                  label: t('Actual cost'),
                  value: [
                    'succeeded',
                    'not_billable',
                    'settled',
                    'reserved',
                  ].includes(data.billing_status) ? (
                    <span className='text-emerald-700 dark:text-emerald-400'>
                      {`US$${data.cost.toFixed(6).replace(/0+$/, '').replace(/\.$/, '')}`}
                    </span>
                  ) : (
                    '—'
                  ),
                },
                { label: t('Retries'), value: String(data.retry_count) },
              ]}
            />
          </DetailSection>
          <DetailSection
            title={t('Request and billing')}
            accentClassName='border-l-4 border-l-violet-500/70'
          >
            <TaskDetailFields
              items={[
                {
                  label: t('Model'),
                  value: (
                    <span className='font-mono text-xs'>{data.model}</span>
                  ),
                },
                { label: t('Protocol'), value: t(imageLabel(data.protocol)) },
                {
                  label: t('Requested / actual size'),
                  value: imageTaskSpecifications(data),
                },
                { label: t('Aspect ratio'), value: data.aspect_ratio },
                {
                  label: t('Images'),
                  value: `${data.result_count} / ${data.image_count}`,
                },
                {
                  label: t('Billing status'),
                  value: t(imageLabel(data.billing_status)),
                },
                {
                  label: t('Storage provider'),
                  value: data.storage_providers?.join(', ') || t('Not stored'),
                },
                {
                  label: t('Expires at'),
                  value: formatTimestampToDate(data.expires_at),
                },
              ]}
            />
          </DetailSection>
          <DetailSection
            title={t('Scheduling context')}
            accentClassName='border-l-4 border-l-sky-500/70'
          >
            <TaskDetailFields
              items={[
                ...(props.admin
                  ? [
                      {
                        label: t('User'),
                        value: data.user_name || t('User unavailable'),
                      },
                    ]
                  : []),
                {
                  label: t('API Key'),
                  value: data.api_key_name || t('Key unavailable'),
                },
                { label: t('Group'), value: data.group },
                ...(props.admin
                  ? [
                      {
                        label: t('Channel'),
                        value:
                          data.channel_name ||
                          (data.channel_id
                            ? t('Channel unavailable')
                            : t('Not selected')),
                      },
                    ]
                  : []),
              ]}
            />
          </DetailSection>
          {props.admin && (
            <DetailSection
              title={t('Account attempts and reconciliation')}
              accentClassName='border-l-4 border-l-orange-500/70'
              action={
                <Badge variant='outline'>
                  {data.reconciliation_status
                    ? t(imageLabel(data.reconciliation_status))
                    : '—'}
                </Badge>
              }
            >
              <TaskDetailFields
                items={[
                  { label: t('Attempts'), value: String(attempts.length) },
                  {
                    label: t('Reconciliation status'),
                    value: data.reconciliation_status
                      ? t(imageLabel(data.reconciliation_status))
                      : '—',
                  },
                  {
                    label: t('Next retry'),
                    value: formatTimestampToDate(data.next_attempt_at),
                  },
                ]}
              />
              {data.attempt_history_unavailable && (
                <p role='alert' className='text-muted-foreground text-xs'>
                  {t('Attempt history is unavailable for this task.')}
                </p>
              )}
              {attempts.length > 0 && (
                <Collapsible
                  defaultOpen={false}
                  className='overflow-hidden rounded-lg border'
                >
                  <CollapsibleTrigger
                    render={
                      <Button
                        variant='ghost'
                        className='h-12 w-full justify-start rounded-none px-4'
                      />
                    }
                  >
                    <ChevronDown className='size-4' aria-hidden />
                    {t('Attempt history')} ({attempts.length})
                  </CollapsibleTrigger>
                  <CollapsibleContent>
                    <StaticDataTable
                      className='rounded-none border-x-0 border-b-0'
                      tableClassName='text-xs'
                      data={attempts}
                      getRowKey={(_item, index) => index}
                      columns={[
                        {
                          id: 'started_at',
                          header: t('Started at'),
                          cell: (attempt) =>
                            formatTimestampToDate(attempt.started_at),
                        },
                        {
                          id: 'finished_at',
                          header: t('Finished at'),
                          cell: (attempt) =>
                            formatTimestampToDate(attempt.finished_at),
                        },
                        {
                          id: 'status',
                          header: t('Status'),
                          cell: (attempt) => {
                            let status = t('Not selected')
                            if (attempt.started_at) status = t('Selected')
                            if (attempt.dispatched) status = t('Processing')
                            if (attempt.finished_at) {
                              status = t(
                                attempt.error_code ? 'Failed' : 'Succeeded'
                              )
                            }
                            return (
                              <Badge
                                variant={
                                  attempt.error_code ? 'warning' : 'outline'
                                }
                                className={
                                  attempt.finished_at && !attempt.error_code
                                    ? 'border-emerald-500/30 bg-emerald-500/10 text-emerald-700 dark:text-emerald-400'
                                    : undefined
                                }
                              >
                                {status}
                              </Badge>
                            )
                          },
                        },
                        {
                          id: 'channel',
                          header: t('Channel'),
                          cell: (attempt) => (
                            <div className='min-w-32'>
                              <p>
                                {attempt.channel_name ||
                                  t('Channel unavailable')}
                              </p>
                              <p className='text-muted-foreground'>
                                {t('Account slot {{index}}', {
                                  index: attempt.key_index + 1,
                                })}
                              </p>
                            </div>
                          ),
                        },
                        {
                          id: 'error_code',
                          header: t('Error code'),
                          cell: (attempt) => attempt.error_code || '—',
                        },
                        {
                          id: 'reference_mode',
                          header: t('Reference transport'),
                          cell: (attempt) =>
                            t(imageLabel(attempt.reference_mode)),
                        },
                      ]}
                    />
                  </CollapsibleContent>
                </Collapsible>
              )}
            </DetailSection>
          )}
          <DetailSection
            title={t(
              data.media_type === 'video'
                ? 'Generated videos'
                : 'Generated images'
            )}
            accentClassName='border-l-4 border-l-emerald-500/70'
            action={
              <span className='text-muted-foreground text-xs'>
                {data.storage_providers?.join(', ')}
              </span>
            }
          >
            {data.media_type === 'video' && (
              <VideoResults
                results={results}
                onRefresh={() => {
                  void task.refetch()
                }}
              />
            )}
            {data.media_type !== 'video' && images.isLoading && (
              <p role='status' className='text-muted-foreground text-sm'>
                {t('Loading...')}
              </p>
            )}
            {data.media_type !== 'video' && images.isError && (
              <p role='alert' className='text-destructive text-sm'>
                {images.error.message}
              </p>
            )}
            {data.media_type !== 'video' &&
              (images.data?.length ? (
                <ImageResults
                  images={images.data}
                  previewMode='original'
                  actions={
                    !props.admin
                      ? (index) => (
                          <Button
                            size='sm'
                            variant='outline'
                            onClick={() => void archive(index)}
                          >
                            {t('Archive to server')}
                          </Button>
                        )
                      : undefined
                  }
                />
              ) : (
                !images.isLoading &&
                !images.isError && (
                  <p className='text-muted-foreground text-sm'>
                    {t('No generated images yet')}
                  </p>
                )
              ))}
          </DetailSection>
          <DetailSection
            title={t('Stage timeline')}
            accentClassName='border-l-4 border-l-blue-500/70'
          >
            <ol className='space-y-0'>
              {task.data.events.map((event) => (
                <li
                  key={event.id}
                  className='relative ml-1 border-l pb-5 pl-5 last:border-l-transparent last:pb-0'
                >
                  <span
                    className={cn(
                      'absolute top-1.5 -left-[5px] size-2 rounded-full',
                      imageTimelineDotClass(event)
                    )}
                    aria-hidden
                  />
                  <div className='flex flex-wrap items-center justify-between gap-2'>
                    <span className='text-sm font-medium'>
                      {t(imageTimelineLabel(event))}
                    </span>
                    <time
                      className='text-muted-foreground text-xs tabular-nums'
                      dateTime={new Date(event.created_at * 1000).toISOString()}
                    >
                      {formatTimestampToDate(event.created_at)}
                    </time>
                  </div>
                  {props.admin && event.message && (
                    <p className='text-muted-foreground mt-1 text-xs break-words'>
                      {t(event.message)}
                    </p>
                  )}
                </li>
              ))}
            </ol>
          </DetailSection>
        </>
      )}
    </Dialog>
  )
}
