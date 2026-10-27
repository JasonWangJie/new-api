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
import { useQuery, useQueryClient } from '@tanstack/react-query'
import type {
  ColumnDef,
  PaginationState,
  RowSelectionState,
  SortingState,
} from '@tanstack/react-table'
import { Ban, Eye, Filter } from 'lucide-react'
import {
  startTransition,
  useCallback,
  useMemo,
  useState,
  type FormEvent,
} from 'react'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'

import { CompactDateTimeRangePicker } from '@/components/compact-date-time-range-picker'
import { ConfirmDialog } from '@/components/confirm-dialog'
import { CopyButton } from '@/components/copy-button'
import {
  DataTableColumnHeader,
  DataTableCardGrid,
  DataTablePage,
  useDataTable,
} from '@/components/data-table'
import { DataTableMobileFilterPanel } from '@/components/data-table/toolbar/mobile-filter-panel'
import { Dialog } from '@/components/dialog'
import { SectionPageLayout } from '@/components/layout'
import { Button } from '@/components/ui/button'
import { Checkbox } from '@/components/ui/checkbox'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { useMediaQuery } from '@/hooks/use-media-query'
import { hasPermission } from '@/lib/admin-permissions'
import dayjs from '@/lib/dayjs'
import { formatTimestampToDate, formatUseTime } from '@/lib/format'
import { cn } from '@/lib/utils'
import { useAuthStore } from '@/stores/auth-store'

import {
  getImageTask,
  getImageTasks,
  getMediaTaskPlatforms,
  imageRequest,
  mediaTasksPath,
} from './api'
import {
  ImagePlatformBadge,
  ImageRequestTypeBadge,
  ImageStatusBadge,
} from './components/image-badges'
import { ImageReliabilityPanel } from './components/image-reliability-panel'
import { ImageSelect } from './components/image-select'
import { ImageTaskDetails } from './components/image-task-details'
import { ImageTaskMobileCard } from './components/image-task-mobile-card'
import { ImageTaskRefreshActions } from './components/image-task-refresh-actions'
import { imageLabel } from './lib/image-labels'
import {
  imageTaskElapsedSeconds,
  imageTaskListSpecifications,
  imageTaskSuccessRate,
  imageTaskIssue,
  imageTaskProgress,
  imageTaskUpstreamProgress,
  imageTaskCanResume,
} from './lib/task-presentation'
import type { ImageTask } from './types'

const TASK_REFRESH_INTERVAL_SECONDS = 10
const TASK_STATES = [
  'queued',
  'invoking',
  'upstream_succeeded',
  'uploading',
  'billing_pending',
  'storage_failed',
  'billing_failed',
  'succeeded',
  'failed',
  'expired',
  'execution_unknown',
]
export function ImageTaskCenter({ admin = false }: { admin?: boolean }) {
  const userId = useAuthStore((state) => state.auth.user?.id)
  return userId ? (
    <TaskCenterSession
      key={`${userId}:${admin}`}
      userId={userId}
      admin={admin}
    />
  ) : null
}

function TaskCenterSession({
  userId,
  admin,
}: {
  userId: number
  admin: boolean
}) {
  const { t } = useTranslation()
  const searchPlaceholder = admin
    ? t('Task ID, model, specifications or channel name')
    : t('Task ID, model or specifications')
  const queryClient = useQueryClient()
  const isMobile = useMediaQuery('(max-width: 640px)')
  const user = useAuthStore((state) => state.auth.user)
  const canManage = admin && hasPermission(user, 'async_image_task', 'manage')
  const today = useMemo(() => {
    const now = new Date()
    return `${now.getFullYear()}-${String(now.getMonth() + 1).padStart(2, '0')}-${String(now.getDate()).padStart(2, '0')}`
  }, [])
  const [draft, setDraft] = useState<Record<string, string>>({
    start_date: today,
    end_date: today,
    timezone: Intl.DateTimeFormat().resolvedOptions().timeZone,
    q: '',
    status: '',
    platform: '',
    billing_status: '',
    request_type: '',
    media_type: '',
  })
  const [filters, setFilters] = useState(draft)
  const [pagination, setPagination] = useState<PaginationState>({
    pageIndex: 0,
    pageSize: 20,
  })
  const [sorting, setSorting] = useState<SortingState>([
    { id: 'created_at', desc: true },
  ])
  const [selection, setSelection] = useState<RowSelectionState>({})
  const [detail, setDetail] = useState('')
  const [autoRefresh, setAutoRefresh] = useState(false)
  const [mobileFiltersOpen, setMobileFiltersOpen] = useState(false)
  const [operation, setOperation] = useState<{
    ids: string[]
    action: 'resume' | 'terminate' | 'batch-terminate'
    confirmUpstreamChecked?: boolean
    confirmQuotaReconciled?: boolean
  } | null>(null)
  const [busy, setBusy] = useState(false)
  const params = new URLSearchParams({
    ...Object.fromEntries(
      Object.entries(filters).filter(([, value]) => !!value)
    ),
    page: String(pagination.pageIndex + 1),
    page_size: String(pagination.pageSize),
    sort_by: sorting[0]?.id || 'created_at',
    sort_order: sorting[0]?.desc ? 'desc' : 'asc',
  })
  const tasks = useQuery({
    queryKey: ['image-task-center', userId, admin, params.toString()],
    queryFn: ({ signal }) => getImageTasks(admin, params, signal),
    refetchInterval: autoRefresh ? TASK_REFRESH_INTERVAL_SECONDS * 1000 : false,
  })
  const platforms = useQuery({
    queryKey: ['image-task-platforms', userId, admin],
    queryFn: ({ signal }) => getMediaTaskPlatforms(admin, signal),
  })
  const openTask = useCallback((id: string) => {
    startTransition(() => setDetail(id))
  }, [])
  const prefetchTask = useCallback(
    (id: string) => {
      void queryClient.prefetchQuery({
        queryKey: ['image-task-details', userId, admin, id],
        queryFn: ({ signal }) => getImageTask(admin, id, signal),
        staleTime: 10_000,
      })
    },
    [admin, queryClient, userId]
  )
  const columns = useMemo<ColumnDef<ImageTask, unknown>[]>(
    () => [
      ...(canManage
        ? [
            {
              id: 'select',
              header: ({ table }) => (
                <Checkbox
                  aria-label={t('Select current page')}
                  checked={table.getIsAllPageRowsSelected()}
                  indeterminate={table.getIsSomePageRowsSelected()}
                  onCheckedChange={(value) =>
                    table.toggleAllPageRowsSelected(value)
                  }
                />
              ),
              cell: ({ row }) => (
                <Checkbox
                  aria-label={`${t('Select task')} ${row.original.id}`}
                  checked={row.getIsSelected()}
                  onCheckedChange={(value) => row.toggleSelected(value)}
                />
              ),
              enableSorting: false,
              size: 36,
            } as ColumnDef<ImageTask, unknown>,
          ]
        : []),
      {
        id: 'task_id',
        header: t('Task ID'),
        cell: ({ row }) => (
          <div className='w-32 space-y-1'>
            <div className='flex min-w-0 items-center gap-1'>
              <Button
                variant='link'
                className='h-auto min-w-0 flex-1 justify-start overflow-hidden p-0 font-mono text-[11px]'
                title={row.original.id}
                onClick={() => openTask(row.original.id)}
                onMouseEnter={() => prefetchTask(row.original.id)}
              >
                <span className='truncate'>{row.original.id}</span>
              </Button>
              <CopyButton
                value={row.original.id}
                tooltip={t('Copy Task ID')}
                className='size-7 shrink-0'
                iconClassName='size-3'
              />
            </div>
            <p className='text-muted-foreground truncate text-xs'>
              {t(imageLabel(row.original.protocol))}
            </p>
          </div>
        ),
        enableSorting: false,
        size: 152,
      },
      {
        id: 'created_at',
        accessorKey: 'created_at',
        header: ({ column }) => (
          <DataTableColumnHeader column={column} title={t('Task time')} />
        ),
        cell: ({ row }) => (
          <dl className='space-y-1 text-[11px] tabular-nums'>
            {[
              { label: t('Submitted at'), value: row.original.created_at },
              { label: t('Started at'), value: row.original.started_at },
              { label: t('Finished at'), value: row.original.finished_at },
            ].map((item) => (
              <div key={item.label} className='flex gap-1.5'>
                <dt className='text-muted-foreground'>{item.label}</dt>
                <dd>{formatTimestampToDate(item.value)}</dd>
              </div>
            ))}
            <div className='flex gap-1.5 font-medium'>
              <dt>{t('Time spent')}</dt>
              <dd>
                {row.original.created_at
                  ? formatUseTime(imageTaskElapsedSeconds(row.original) || 0)
                  : '—'}
              </dd>
            </div>
          </dl>
        ),
        size: 212,
      },
      {
        id: 'platform',
        header: t('Platform'),
        cell: ({ row }) => (
          <div className='space-y-1.5 text-xs'>
            <ImagePlatformBadge
              platform={row.original.provider || row.original.platform}
            />
            <ImageRequestTypeBadge
              requestType={row.original.request_type}
              className='font-normal'
            />
          </div>
        ),
        enableSorting: false,
        size: 128,
      },
      {
        id: 'model',
        header: t('Model / specifications'),
        cell: ({ row }) => (
          <div className='w-36 space-y-1 text-xs'>
            <p
              className='truncate font-mono font-medium'
              title={row.original.model}
            >
              {row.original.model}
            </p>
            <p className='text-muted-foreground whitespace-normal'>
              {imageTaskListSpecifications(row.original, t)}{' '}
              {row.original.aspect_ratio}
            </p>
          </div>
        ),
        enableSorting: false,
        size: 166,
      },
      {
        id: 'status',
        accessorKey: 'status',
        header: ({ column }) => (
          <DataTableColumnHeader column={column} title={t('Status')} />
        ),
        cell: ({ row }) => {
          const issue = imageTaskIssue(row.original)
          const progress = imageTaskProgress(row.original)
          const upstreamProgress = imageTaskUpstreamProgress(row.original)
          return (
            <div className='space-y-2'>
              <ImageStatusBadge
                status={row.original.status}
                errorCode={
                  issue && !issue.pendingAction
                    ? row.original.error_code
                    : undefined
                }
                pendingAction={issue?.pendingAction}
              />
              <p className='text-muted-foreground text-xs'>
                {progress !== null && <>{progress}% · </>}
                {t(imageLabel(row.original.billing_status))}
              </p>
              {upstreamProgress !== null && (
                <p className='text-muted-foreground text-xs wrap-anywhere whitespace-normal tabular-nums'>
                  {t('Upstream progress')} {upstreamProgress}%
                </p>
              )}
            </div>
          )
        },
        size: 108,
      },
      {
        id: 'image_count',
        accessorKey: 'image_count',
        header: ({ column }) => (
          <DataTableColumnHeader column={column} title={t('Media / storage')} />
        ),
        cell: ({ row }) => (
          <span className='text-xs tabular-nums'>
            {row.original.result_count} / {row.original.image_count}
            <span className='text-muted-foreground block'>
              {row.original.storage_providers?.join(', ') || t('Not stored')}
            </span>
          </span>
        ),
        size: 88,
      },
      ...(admin
        ? [
            {
              id: 'user',
              header: t('User'),
              cell: ({ row }) => (
                <p
                  className='w-30 truncate text-xs'
                  title={row.original.user_name}
                >
                  {row.original.user_name || t('User unavailable')}
                </p>
              ),
              enableSorting: false,
              size: 138,
            } as ColumnDef<ImageTask, unknown>,
            {
              id: 'channel',
              header: t('Channel'),
              cell: ({ row }) => (
                <p
                  className='w-32 truncate text-xs'
                  title={row.original.channel_name}
                >
                  {row.original.channel_name ||
                    (row.original.channel_id
                      ? t('Channel unavailable')
                      : t('Not selected'))}
                </p>
              ),
              enableSorting: false,
              size: 150,
            } as ColumnDef<ImageTask, unknown>,
          ]
        : []),
      {
        id: 'quota',
        header: ({ column }) => (
          <DataTableColumnHeader column={column} title={t('Actual cost')} />
        ),
        cell: ({ row }) => (
          <span
            className={
              ['succeeded', 'not_billable'].includes(
                row.original.billing_status
              )
                ? 'font-mono text-xs text-emerald-700 dark:text-emerald-400'
                : 'text-muted-foreground font-mono text-xs'
            }
          >
            {['succeeded', 'not_billable', 'settled', 'reserved'].includes(
              row.original.billing_status
            )
              ? `$${row.original.cost.toFixed(6).replace(/0+$/, '').replace(/\.$/, '')}`
              : '—'}
          </span>
        ),
        size: 86,
      },
      {
        id: 'actions',
        header: t('Actions'),
        cell: ({ row }) => (
          <div className='flex flex-wrap gap-1'>
            <Button
              size='xs'
              variant='ghost'
              onClick={() => openTask(row.original.id)}
              onMouseEnter={() => prefetchTask(row.original.id)}
            >
              <Eye className='size-3.5' aria-hidden />
              {t('View')}
            </Button>
            {imageTaskCanResume(row.original, admin, canManage) && (
              <Button
                size='sm'
                variant='outline'
                onClick={() =>
                  setOperation({
                    ids: [row.original.id],
                    action: 'resume',
                    confirmQuotaReconciled:
                      admin &&
                      row.original.error_code === 'billing_quota_uncertain',
                  })
                }
              >
                {t('Resume')}
              </Button>
            )}
            {(canManage || (!admin && row.original.media_type === 'video')) &&
              row.original.can_terminate && (
                <Button
                  size='icon-sm'
                  variant='ghost'
                  aria-label={t('Terminate')}
                  title={t('Terminate')}
                  onClick={() =>
                    setOperation({
                      ids: [row.original.id],
                      action: 'terminate',
                      confirmUpstreamChecked:
                        row.original.status === 'execution_unknown',
                    })
                  }
                >
                  <Ban className='size-3.5' aria-hidden />
                </Button>
              )}
          </div>
        ),
        enableSorting: false,
        size: 118,
      },
    ],
    [admin, canManage, openTask, prefetchTask, t]
  )
  const { table } = useDataTable({
    data: tasks.data?.items || [],
    columns,
    totalCount: tasks.data?.total || 0,
    manualPagination: true,
    manualSorting: true,
    manualFiltering: true,
    pagination,
    onPaginationChange: setPagination,
    sorting,
    onSortingChange: setSorting,
    enableRowSelection: canManage,
    rowSelection: selection,
    onRowSelectionChange: setSelection,
    getRowId: (row) => row.id,
  })
  const field = (key: string, label: string, type = 'text') => (
    <div key={key} className='space-y-1.5'>
      <Label htmlFor={`task-filter-${key}`}>{t(label)}</Label>
      <Input
        id={`task-filter-${key}`}
        type={type}
        placeholder={key === 'q' ? searchPlaceholder : undefined}
        value={draft[key] || ''}
        onChange={(event) =>
          setDraft((previous) => ({ ...previous, [key]: event.target.value }))
        }
      />
    </div>
  )
  const choices = (values: string[]) => [
    { value: '', label: t('All') },
    ...values.map((value) => ({ value, label: t(imageLabel(value)) })),
  ]
  const confirm = async () => {
    if (!operation) return
    setBusy(true)
    try {
      if (operation.action === 'batch-terminate') {
        const failures: { id: string; reason: string }[] = []
        for (const id of operation.ids) {
          try {
            await imageRequest(
              `${mediaTasksPath(true)}/${id}/terminate`,
              'POST',
              operation.confirmUpstreamChecked
                ? { confirm_upstream_checked: true }
                : undefined
            )
          } catch (error) {
            failures.push({
              id,
              reason:
                error instanceof Error ? error.message : t('Operation failed'),
            })
          }
        }
        const succeeded = operation.ids.length - failures.length
        if (failures.length) {
          toast.error(
            t('Task termination: {{succeeded}} succeeded, {{failed}} failed', {
              succeeded,
              failed: failures.length,
            }),
            {
              description: (
                <ul className='max-h-40 space-y-1 overflow-auto [overflow-wrap:anywhere]'>
                  {failures.map((failure) => (
                    <li key={failure.id}>
                      {failure.id}: {failure.reason}
                    </li>
                  ))}
                </ul>
              ),
              duration: 8000,
              closeButton: true,
            }
          )
        } else {
          toast.success(
            t('Tasks terminated: {{count}}', { count: succeeded }),
            {
              duration: 5000,
              closeButton: true,
            }
          )
        }
      } else {
        const body: Record<string, boolean> = {}
        if (operation.confirmUpstreamChecked) {
          body.confirm_upstream_checked = true
        }
        if (
          admin &&
          operation.action === 'resume' &&
          operation.confirmQuotaReconciled
        ) {
          body.confirm_quota_reconciled = true
        }
        await imageRequest(
          `${mediaTasksPath(admin)}/${operation.ids[0]}/${operation.action}`,
          'POST',
          body
        )
      }
      await queryClient.invalidateQueries({
        queryKey: ['image-task-center', userId, admin],
      })
      await queryClient.invalidateQueries({
        queryKey: ['image-task-details', userId, admin],
      })
      setSelection({})
      setOperation(null)
    } catch (error) {
      toast.error(
        error instanceof Error ? error.message : t('Image request failed')
      )
    } finally {
      setBusy(false)
    }
  }
  const successRate = imageTaskSuccessRate(tasks.data?.stats)
  let confirmationDescription = t(
    'Selected task IDs are fixed for this operation. Late results cannot change terminated tasks.'
  )
  if (operation?.confirmUpstreamChecked) {
    confirmationDescription = `${t('Verify the upstream task before continuing. This stops local processing without cancelling the upstream task. Reserved quota is released for images; video billing remains unchanged.')} ${confirmationDescription}`
  }
  if (operation?.action === 'resume') {
    confirmationDescription = t(
      'Retrieve results again and retry storage and pending billing. Media generation is never repeated.'
    )
    if (operation.confirmQuotaReconciled) {
      confirmationDescription = `${t('Ask an administrator to reconcile the quota reservation before trying again.')} ${confirmationDescription}`
    }
  }
  function applyFilters(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    setFilters({ ...draft, q: draft.q.trim() })
    setPagination((previous) => ({ ...previous, pageIndex: 0 }))
    setSelection({})
    setMobileFiltersOpen(false)
  }
  function resetFilters() {
    const reset = Object.fromEntries(Object.keys(draft).map((key) => [key, '']))
    reset.start_date = today
    reset.end_date = today
    reset.timezone = Intl.DateTimeFormat().resolvedOptions().timeZone
    setDraft(reset)
    setFilters(reset)
    setPagination((previous) => ({ ...previous, pageIndex: 0 }))
    setSelection({})
  }
  const primaryFilters = (
    <>
      <div className='col-span-2 min-w-0 xl:col-span-5'>
        {field('q', 'Search tasks')}
      </div>
      <div className='min-w-0 xl:col-span-2'>
        <ImageSelect
          className='flex flex-col gap-1.5 space-y-0'
          label={t('Platform')}
          value={draft.platform}
          options={choices(platforms.data || [])}
          disabled={platforms.isPending}
          onChange={(value) =>
            setDraft((previous) => ({ ...previous, platform: value }))
          }
        />
      </div>
      <div className='min-w-0 xl:col-span-2'>
        <ImageSelect
          className='flex flex-col gap-1.5 space-y-0'
          label={t('Status')}
          value={draft.status}
          options={choices(TASK_STATES)}
          onChange={(value) =>
            setDraft((previous) => ({ ...previous, status: value }))
          }
        />
      </div>
      <div className='col-span-2 min-w-0 space-y-1.5 xl:col-span-3'>
        <Label htmlFor='task-filter-date-range'>{t('Date Range')}</Label>
        <CompactDateTimeRangePicker
          id='task-filter-date-range'
          precision='day'
          start={
            draft.start_date ? dayjs(draft.start_date).toDate() : undefined
          }
          end={draft.end_date ? dayjs(draft.end_date).toDate() : undefined}
          className={cn('min-w-0', isMobile && 'min-h-11')}
          onChange={({ start, end }) =>
            setDraft((previous) => ({
              ...previous,
              start_date: start ? dayjs(start).format('YYYY-MM-DD') : '',
              end_date: end ? dayjs(end).format('YYYY-MM-DD') : '',
            }))
          }
        />
      </div>
    </>
  )
  const advancedFilters = (
    <>
      {field('model', 'Model')}
      <ImageSelect
        className='flex flex-col gap-1.5 space-y-0'
        label={t('Media type')}
        value={draft.media_type}
        options={[
          { value: '', label: t('All') },
          { value: 'image', label: t('Image') },
          { value: 'video', label: t('Video') },
        ]}
        onChange={(value) =>
          setDraft((previous) => ({
            ...previous,
            media_type: value,
          }))
        }
      />
      <ImageSelect
        className='flex flex-col gap-1.5 space-y-0'
        label={t('Request type')}
        value={draft.request_type}
        options={choices([
          'text_to_image',
          'image_to_image',
          'text_to_video',
          'image_to_video',
          'video',
        ])}
        onChange={(value) =>
          setDraft((previous) => ({
            ...previous,
            request_type: value,
          }))
        }
      />
      <ImageSelect
        className='flex flex-col gap-1.5 space-y-0'
        label={t('Billing status')}
        value={draft.billing_status}
        options={choices(['pending', 'succeeded', 'not_billable', 'failed'])}
        onChange={(value) =>
          setDraft((previous) => ({
            ...previous,
            billing_status: value,
          }))
        }
      />
      {field('group', 'Group')}
      {field('api_key_id', 'API Key ID', 'number')}
      {admin && (
        <>
          {field('channel_id', 'Channel ID', 'number')}
          {field('user_id', 'User ID', 'number')}
        </>
      )}
    </>
  )
  const advancedFilterCount = [
    'model',
    'group',
    'api_key_id',
    'media_type',
    'request_type',
    'billing_status',
    'channel_id',
    'user_id',
  ].filter((key) => filters[key]).length
  const moreFiltersLabel = (
    <>
      {t('More filters')}
      {advancedFilterCount > 0 && (
        <>
          {' '}
          <span className='bg-primary/10 text-primary rounded px-1.5 text-xs tabular-nums'>
            {advancedFilterCount}
          </span>
        </>
      )}
    </>
  )
  const filterActions = (
    <>
      <Button type='button' variant='ghost' onClick={resetFilters}>
        {t('Reset')}
      </Button>
      <Button
        type='submit'
        form={
          mobileFiltersOpen
            ? 'image-task-advanced-filter-form'
            : 'image-task-filter-form'
        }
      >
        {t('Apply filters')}
      </Button>
    </>
  )
  const currentPending = (tasks.data?.items || []).filter(
    (task) => task.can_terminate
  )
  const stats = tasks.data?.stats
  const statistics = [
    {
      label: t('Processing'),
      value: (stats?.queued || 0) + (stats?.processing || 0),
      color: 'text-amber-700 dark:text-amber-400',
      shell: 'border-amber-500/20 bg-amber-500/10',
      dot: 'bg-amber-500',
    },
    {
      label: t('Succeeded'),
      value: stats?.succeeded || 0,
      color: 'text-emerald-700 dark:text-emerald-400',
      shell: 'border-emerald-500/20 bg-emerald-500/10',
      dot: 'bg-emerald-500',
    },
    {
      label: t('Failed'),
      value: stats?.failed || 0,
      color: 'text-rose-700 dark:text-rose-400',
      shell: 'border-rose-500/20 bg-rose-500/10',
      dot: 'bg-rose-500',
    },
    {
      label: t('Success rate'),
      value: successRate === null ? '—' : `${successRate.toFixed(1)}%`,
      color: 'text-emerald-700 dark:text-emerald-400',
      shell: 'border-emerald-500/20 bg-emerald-500/10',
      dot: 'bg-emerald-500',
    },
    {
      label: t('Average duration'),
      value:
        stats?.average_duration_ms == null
          ? '—'
          : formatUseTime(stats.average_duration_ms / 1000),
      color: 'text-cyan-700 dark:text-cyan-400',
      shell: 'border-cyan-500/20 bg-cyan-500/10',
      dot: 'bg-cyan-500',
    },
    {
      label: t('Successful task cost'),
      value:
        stats?.succeeded_cost == null
          ? '—'
          : `$${stats.succeeded_cost.toFixed(6).replace(/0+$/, '').replace(/\.$/, '')}`,
      color: 'text-primary',
      shell: 'border-primary/20 bg-primary/10',
      dot: 'bg-primary',
    },
  ]
  const batchActions = canManage && (
    <>
      <Button
        size='sm'
        variant='destructive'
        disabled={!currentPending.length || busy}
        onClick={() =>
          setOperation({
            ids: currentPending.map((task) => task.id),
            action: 'batch-terminate',
            confirmUpstreamChecked: currentPending.some(
              (task) => task.status === 'execution_unknown'
            ),
          })
        }
      >
        <Ban className='size-3.5' aria-hidden />
        {t('Terminate current page')} ({currentPending.length})
      </Button>
      <Button
        size='sm'
        variant='destructive'
        disabled={!table.getSelectedRowModel().rows.length}
        onClick={() =>
          setOperation({
            ids: table.getSelectedRowModel().rows.map((row) => row.original.id),
            action: 'batch-terminate',
            confirmUpstreamChecked: table
              .getSelectedRowModel()
              .rows.some((row) => row.original.status === 'execution_unknown'),
          })
        }
      >
        {t('Terminate selected')}
      </Button>
    </>
  )
  return (
    <SectionPageLayout fixedContent={!isMobile} stackActionsOnMobile>
      <SectionPageLayout.Title>
        {t('Media task center')}
      </SectionPageLayout.Title>
      <SectionPageLayout.Actions>
        <ImageTaskRefreshActions
          floating={
            admin &&
            isMobile &&
            !table.getSelectedRowModel().rows.length &&
            !detail &&
            !operation &&
            !mobileFiltersOpen
          }
          autoRefresh={autoRefresh}
          refreshIntervalSeconds={TASK_REFRESH_INTERVAL_SECONDS}
          tasksFetching={tasks.isFetching}
          tasksUpdatedAt={Math.max(tasks.dataUpdatedAt, tasks.errorUpdatedAt)}
          platformsFetching={platforms.isFetching}
          onAutoRefreshChange={setAutoRefresh}
          onRefresh={() => {
            void tasks.refetch()
            void platforms.refetch()
          }}
        />
        {!isMobile && batchActions}
      </SectionPageLayout.Actions>
      <SectionPageLayout.Content>
        <div
          className={cn('flex flex-col gap-3', !isMobile && 'h-full min-h-0')}
        >
          <dl
            aria-label={t('Task statistics')}
            className={
              isMobile
                ? 'grid shrink-0 grid-cols-6 gap-2'
                : 'flex shrink-0 flex-wrap gap-2'
            }
          >
            {statistics.map((item, index) => (
              <div
                key={item.label}
                className={cn(
                  'flex rounded-lg border px-3 py-2 text-xs shadow-sm',
                  item.shell,
                  item.color,
                  isMobile
                    ? 'flex-col items-start gap-1'
                    : 'items-center gap-2',
                  isMobile && (index < 3 ? 'col-span-2' : 'col-span-3'),
                  isMobile && index === 5 && 'col-span-6'
                )}
              >
                <dt className='flex items-center gap-1.5'>
                  <span
                    className={cn('size-1.5 shrink-0 rounded-full', item.dot)}
                    aria-hidden
                  />
                  {item.label}
                </dt>
                <dd
                  className={cn(
                    'font-semibold tabular-nums',
                    isMobile && 'text-base leading-6'
                  )}
                >
                  {item.value}
                </dd>
              </div>
            ))}
          </dl>
          {admin && filters.media_type !== 'video' && (
            <ImageReliabilityPanel
              userId={userId}
              filters={filters}
              autoRefresh={autoRefresh}
            />
          )}
          {isMobile ? (
            <>
              <form
                id='image-task-filter-form'
                className='bg-card/50 grid grid-cols-2 items-end gap-3 rounded-lg border p-3 [&_[data-slot=select-trigger]]:min-h-11 [&_input]:min-h-11'
                onSubmit={applyFilters}
              >
                {primaryFilters}
                <div className='col-span-2 flex flex-wrap items-center gap-1.5 border-t pt-2.5 [&>button]:min-h-11 [&>button]:whitespace-normal'>
                  <Button
                    type='button'
                    variant='outline'
                    className='mr-auto min-w-0'
                    aria-haspopup='dialog'
                    aria-expanded={mobileFiltersOpen}
                    onClick={() => setMobileFiltersOpen(true)}
                  >
                    <Filter className='size-4' aria-hidden />
                    {moreFiltersLabel}
                  </Button>
                  {filterActions}
                </div>
              </form>
              <Dialog
                open={mobileFiltersOpen}
                onOpenChange={setMobileFiltersOpen}
                title={t('More filters')}
                contentClassName='max-h-[calc(100dvh-2rem)]'
                scrollAreaClassName='max-h-[calc(100dvh-12rem)]'
                footerClassName='grid grid-cols-2 [&>button]:min-h-11'
                footer={filterActions}
              >
                <form
                  id='image-task-advanced-filter-form'
                  className='grid grid-cols-1 gap-3 min-[360px]:grid-cols-2 [&_[data-slot=select-trigger]]:min-h-11 [&_input]:min-h-11'
                  onSubmit={applyFilters}
                >
                  {advancedFilters}
                </form>
              </Dialog>
            </>
          ) : (
            <form
              id='image-task-filter-form'
              className='shrink-0'
              onSubmit={applyFilters}
            >
              <DataTableMobileFilterPanel
                compact
                defaultOpen={false}
                triggerLabel={moreFiltersLabel}
                className='gap-3 p-3'
                summary={
                  <div className='grid grid-cols-2 items-end gap-3 xl:grid-cols-12'>
                    {primaryFilters}
                  </div>
                }
                actions={filterActions}
              >
                <div className='grid max-h-[32dvh] grid-cols-2 gap-3 overflow-y-auto border-t p-1 pt-3 lg:grid-cols-4'>
                  {advancedFilters}
                </div>
              </DataTableMobileFilterPanel>
            </form>
          )}
          {(tasks.isError || platforms.isError) && (
            <p role='alert' className='text-destructive shrink-0 text-sm'>
              {tasks.error?.message || platforms.error?.message}
            </p>
          )}
          <div className={isMobile ? undefined : 'min-h-0 flex-1'}>
            <DataTablePage
              table={table}
              columns={columns}
              toolbarProps={null}
              fixedHeight={!isMobile}
              compactPagination={isMobile}
              isLoading={tasks.isLoading}
              isFetching={tasks.isFetching}
              emptyTitle={t('No image tasks found')}
              emptyDescription={t(
                'Adjust filters or create an image from the workbench.'
              )}
              applyHeaderSize
              pinnedColumns={[{ columnId: 'actions', side: 'right' }]}
              showMobileBulkActions
              mobile={
                isMobile && (
                  <div className='space-y-2'>
                    {canManage && !!table.getRowModel().rows.length && (
                      <div className='grid grid-cols-2 items-center gap-x-2 rounded-lg border px-2.5 pb-2.5 [&>button]:min-h-11 [&>button]:min-w-0 [&>button]:whitespace-normal'>
                        <label className='col-span-2 flex min-h-11 items-center gap-2 text-xs'>
                          <Checkbox
                            checked={table.getIsAllPageRowsSelected()}
                            indeterminate={table.getIsSomePageRowsSelected()}
                            onCheckedChange={(checked) =>
                              table.toggleAllPageRowsSelected(checked)
                            }
                          />
                          {t('Select current page')}
                        </label>
                        {batchActions}
                      </div>
                    )}
                    <DataTableCardGrid
                      table={table}
                      isLoading={tasks.isLoading}
                      emptyTitle={t('No image tasks found')}
                      emptyDescription={t(
                        'Adjust filters or create an image from the workbench.'
                      )}
                      gridClassName='grid min-w-0 grid-cols-1 gap-3'
                      getRowClassName={() =>
                        'min-w-0 rounded-xl p-3.5 shadow-sm'
                      }
                      renderCard={(row, { isSelected }) => (
                        <ImageTaskMobileCard
                          row={row}
                          isSelected={isSelected}
                          admin={admin}
                          onView={openTask}
                          onPrefetch={prefetchTask}
                        />
                      )}
                    />
                  </div>
                )
              }
              getColumnClassName={(_id, section) =>
                section === 'cell' ? 'py-4 align-middle' : undefined
              }
            />
          </div>
          {detail && (
            <ImageTaskDetails
              key={detail}
              id={detail}
              admin={admin}
              userId={userId}
              onClose={() => setDetail('')}
              canManage={canManage}
              onManage={(
                action,
                confirmUpstreamChecked,
                confirmQuotaReconciled
              ) =>
                setOperation({
                  ids: [detail],
                  action,
                  confirmUpstreamChecked,
                  confirmQuotaReconciled,
                })
              }
            />
          )}
          <ConfirmDialog
            open={!!operation}
            onOpenChange={(open) => {
              if (!open && !busy) setOperation(null)
            }}
            title={t(
              operation?.action === 'resume'
                ? 'Resume media post-processing'
                : 'Terminate media tasks'
            )}
            desc={confirmationDescription}
            destructive={operation?.action !== 'resume'}
            isLoading={busy}
            handleConfirm={() => void confirm()}
          />
        </div>
      </SectionPageLayout.Content>
    </SectionPageLayout>
  )
}
