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
import { Ban, Eye, RefreshCw } from 'lucide-react'
import { useMemo, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'

import { ConfirmDialog } from '@/components/confirm-dialog'
import { CopyButton } from '@/components/copy-button'
import {
  DataTableColumnHeader,
  DataTablePage,
  useDataTable,
} from '@/components/data-table'
import { DataTableMobileFilterPanel } from '@/components/data-table/toolbar/mobile-filter-panel'
import { SectionPageLayout } from '@/components/layout'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Checkbox } from '@/components/ui/checkbox'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { hasPermission } from '@/lib/admin-permissions'
import { formatTimestampToDate, formatUseTime } from '@/lib/format'
import { useAuthStore } from '@/stores/auth-store'

import { getImageTasks, imageRequest, imageTasksPath } from './api'
import { ImageSelect } from './components/image-select'
import { ImageTaskDetails } from './components/image-task-details'
import { imageLabel } from './lib/image-labels'
import {
  imageTaskElapsedSeconds,
  imageTaskSpecifications,
  imageTaskSuccessRate,
} from './lib/task-presentation'
import type { ImageTask } from './types'

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
const TASK_FILTER_FIELDS = [
  { key: 'model', label: 'Model' },
  { key: 'group', label: 'Group' },
  { key: 'api_key_id', label: 'API Key ID' },
  { key: 'task_id', label: 'Task ID' },
] as const

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
  const queryClient = useQueryClient()
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
    protocol: '',
    billing_status: '',
    request_type: '',
    storage_provider: '',
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
  const [autoRefresh, setAutoRefresh] = useState(true)
  const [operation, setOperation] = useState<{
    ids: string[]
    action: 'resume' | 'terminate' | 'batch-terminate'
  } | null>(null)
  const [busy, setBusy] = useState(false)
  const [feedback, setFeedback] = useState<
    { id: string; status: string; reason?: string }[]
  >([])
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
    refetchInterval: autoRefresh ? 10000 : false,
  })
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
                onClick={() => setDetail(row.original.id)}
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
          <div className='space-y-1 text-xs'>
            <p className='font-medium'>
              {t(imageLabel(row.original.platform))}
            </p>
            <p className='text-muted-foreground'>
              {t(imageLabel(row.original.request_type))}
            </p>
          </div>
        ),
        enableSorting: false,
        size: 82,
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
              {imageTaskSpecifications(row.original)}{' '}
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
        cell: ({ row }) => (
          <div className='space-y-1'>
            <Badge
              variant={row.original.error_code ? 'warning' : 'outline'}
              className={
                row.original.status === 'succeeded'
                  ? 'border-emerald-500/30 bg-emerald-500/10 text-emerald-700 dark:text-emerald-400'
                  : undefined
              }
            >
              {t(imageLabel(row.original.status))}
            </Badge>
            <p className='text-muted-foreground text-xs'>
              {row.original.progress}% ·{' '}
              {t(imageLabel(row.original.billing_status))}
            </p>
          </div>
        ),
        size: 108,
      },
      {
        id: 'image_count',
        accessorKey: 'image_count',
        header: ({ column }) => (
          <DataTableColumnHeader
            column={column}
            title={t('Images / storage')}
          />
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
          <span className='font-mono text-xs'>
            {['succeeded', 'not_billable'].includes(row.original.billing_status)
              ? `US$${row.original.cost.toFixed(6).replace(/0+$/, '').replace(/\.$/, '')}`
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
              onClick={() => setDetail(row.original.id)}
            >
              <Eye className='size-3.5' aria-hidden />
              {t('View')}
            </Button>
            {canManage && row.original.can_resume && (
              <Button
                size='sm'
                variant='outline'
                onClick={() =>
                  setOperation({ ids: [row.original.id], action: 'resume' })
                }
              >
                {t('Resume')}
              </Button>
            )}
            {canManage && row.original.can_terminate && (
              <Button
                size='icon-sm'
                variant='ghost'
                aria-label={t('Terminate')}
                title={t('Terminate')}
                onClick={() =>
                  setOperation({ ids: [row.original.id], action: 'terminate' })
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
    [admin, canManage, t]
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
        const response = await imageRequest<{
          items: { task_id: string; status: string; message?: string }[]
        }>(`${imageTasksPath(true)}/batch-terminate`, 'POST', {
          task_ids: operation.ids,
        })
        setFeedback(
          response.items.map((item) => ({
            id: item.task_id,
            status: item.status,
            reason: item.message,
          }))
        )
      } else {
        await imageRequest(
          `${imageTasksPath(true)}/${operation.ids[0]}/${operation.action}`,
          'POST',
          {}
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
  const currentPending = (tasks.data?.items || []).filter(
    (task) => task.can_terminate
  )
  const stats = tasks.data?.stats
  const statistics = [
    {
      label: t('Processing'),
      value: (stats?.queued || 0) + (stats?.processing || 0),
      color: 'text-amber-700 dark:text-amber-400',
    },
    {
      label: t('Succeeded'),
      value: stats?.succeeded || 0,
      color: 'text-emerald-700 dark:text-emerald-400',
    },
    {
      label: t('Failed'),
      value: stats?.failed || 0,
      color: 'text-rose-700 dark:text-rose-400',
    },
    {
      label: t('Success rate'),
      value: successRate === null ? '—' : `${successRate.toFixed(1)}%`,
      color: 'text-emerald-700 dark:text-emerald-400',
    },
    {
      label: t('Average duration'),
      value:
        stats?.average_duration_ms == null
          ? '—'
          : formatUseTime(stats.average_duration_ms / 1000),
      color: 'text-cyan-700 dark:text-cyan-400',
    },
  ]
  return (
    <SectionPageLayout fixedContent stackActionsOnMobile>
      <SectionPageLayout.Title>
        {t(admin ? 'Admin Image Tasks' : 'Async Image Tasks')}
      </SectionPageLayout.Title>
      <SectionPageLayout.Actions>
        <Button
          size='sm'
          variant={autoRefresh ? 'secondary' : 'outline'}
          aria-pressed={autoRefresh}
          onClick={() => setAutoRefresh((previous) => !previous)}
        >
          <RefreshCw className='size-3.5' aria-hidden />
          {t('Auto refresh')}
        </Button>
        <Button
          variant='outline'
          size='sm'
          onClick={() => void tasks.refetch()}
        >
          {t('Refresh')}
        </Button>
        {canManage && (
          <>
            <Button
              size='sm'
              variant='destructive'
              disabled={!currentPending.length || busy}
              onClick={() =>
                setOperation({
                  ids: currentPending.map((task) => task.id),
                  action: 'batch-terminate',
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
                  ids: table
                    .getSelectedRowModel()
                    .rows.map((row) => row.original.id),
                  action: 'batch-terminate',
                })
              }
            >
              {t('Terminate selected')}
            </Button>
          </>
        )}
      </SectionPageLayout.Actions>
      <SectionPageLayout.Content>
        <div className='flex h-full min-h-0 flex-col gap-3'>
          <dl
            aria-label={t('Task statistics')}
            className='flex shrink-0 flex-wrap gap-2'
          >
            {statistics.map((item) => (
              <div
                key={item.label}
                className={`bg-muted/20 flex items-center gap-2 rounded-md border px-3 py-2 text-xs ${item.color}`}
              >
                <dt>{item.label}</dt>
                <dd className='font-semibold tabular-nums'>{item.value}</dd>
              </div>
            ))}
          </dl>
          <form
            id='image-task-filter-form'
            className='shrink-0'
            onSubmit={(event) => {
              event.preventDefault()
              setFilters(draft)
              setPagination((previous) => ({ ...previous, pageIndex: 0 }))
              setSelection({})
            }}
          >
            <DataTableMobileFilterPanel
              compact
              defaultOpen={false}
              summary={
                <div className='grid grid-cols-2 gap-3 xl:grid-cols-6'>
                  <div className='col-span-2'>{field('q', 'Search tasks')}</div>
                  <ImageSelect
                    label={t('Status')}
                    value={draft.status}
                    options={choices(TASK_STATES)}
                    onChange={(value) =>
                      setDraft((previous) => ({ ...previous, status: value }))
                    }
                  />
                  <ImageSelect
                    label={t('Platform')}
                    value={draft.platform}
                    options={choices(['openai', 'gemini'])}
                    onChange={(value) =>
                      setDraft((previous) => ({ ...previous, platform: value }))
                    }
                  />
                  <ImageSelect
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
                    label={t('Storage provider')}
                    value={draft.storage_provider}
                    options={choices([
                      'local',
                      'aws',
                      'aliyun',
                      'tencent',
                      'qiniu',
                      'r2',
                      'custom_s3',
                    ])}
                    onChange={(value) =>
                      setDraft((previous) => ({
                        ...previous,
                        storage_provider: value,
                      }))
                    }
                  />
                </div>
              }
              actions={
                <>
                  <span className='text-muted-foreground mr-auto hidden text-xs sm:block'>
                    {filters.start_date} — {filters.end_date}
                  </span>
                  <Button
                    type='button'
                    variant='outline'
                    onClick={() => {
                      const reset = Object.fromEntries(
                        Object.keys(draft).map((key) => [key, ''])
                      )
                      reset.start_date = today
                      reset.end_date = today
                      reset.timezone =
                        Intl.DateTimeFormat().resolvedOptions().timeZone
                      setDraft(reset)
                      setFilters(reset)
                      setPagination((previous) => ({
                        ...previous,
                        pageIndex: 0,
                      }))
                      setSelection({})
                    }}
                  >
                    {t('Reset')}
                  </Button>
                  <Button type='submit' form='image-task-filter-form'>
                    {t('Apply filters')}
                  </Button>
                </>
              }
            >
              <div className='mt-3 grid max-h-[32dvh] grid-cols-2 gap-3 overflow-y-auto border-t p-1 pt-3 sm:grid-cols-3 xl:grid-cols-5'>
                {field('start_date', 'Start date', 'date')}
                {field('end_date', 'End date', 'date')}
                <ImageSelect
                  label={t('Protocol')}
                  value={draft.protocol}
                  options={choices(['bb', 'sc'])}
                  onChange={(value) =>
                    setDraft((previous) => ({ ...previous, protocol: value }))
                  }
                />
                <ImageSelect
                  label={t('Billing status')}
                  value={draft.billing_status}
                  options={choices([
                    'pending',
                    'succeeded',
                    'not_billable',
                    'failed',
                  ])}
                  onChange={(value) =>
                    setDraft((previous) => ({
                      ...previous,
                      billing_status: value,
                    }))
                  }
                />
                {TASK_FILTER_FIELDS.map(({ key, label }) => field(key, label))}
                {admin && (
                  <>
                    {field('channel_id', 'Channel ID', 'number')}
                    {field('user_id', 'User ID', 'number')}
                  </>
                )}
              </div>
            </DataTableMobileFilterPanel>
          </form>
          {tasks.isError && (
            <p role='alert' className='text-destructive shrink-0 text-sm'>
              {tasks.error.message}
            </p>
          )}
          {feedback.length > 0 && (
            <div
              aria-live='polite'
              className='bg-muted/30 max-h-24 shrink-0 overflow-auto rounded-lg p-2 text-xs'
            >
              {feedback.map((result) => (
                <p key={result.id}>
                  {result.id}: {t(imageLabel(result.status))} {result.reason}
                </p>
              ))}
            </div>
          )}
          <div className='min-h-0 flex-1'>
            <DataTablePage
              table={table}
              columns={columns}
              toolbarProps={null}
              isLoading={tasks.isLoading}
              isFetching={tasks.isFetching}
              emptyTitle={t('No image tasks found')}
              emptyDescription={t(
                'Adjust filters or create an image from the workbench.'
              )}
              applyHeaderSize
              pinnedColumns={[{ columnId: 'actions', side: 'right' }]}
              showMobileBulkActions
              mobileProps={{ enableRowSelection: canManage }}
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
              onManage={(action) => setOperation({ ids: [detail], action })}
            />
          )}
          <ConfirmDialog
            open={!!operation}
            onOpenChange={(open) => {
              if (!open && !busy) setOperation(null)
            }}
            title={t(
              operation?.action === 'resume'
                ? 'Resume image post-processing'
                : 'Terminate image tasks'
            )}
            desc={t(
              operation?.action === 'resume'
                ? 'Only storage and billing are retried. The image is never generated again.'
                : 'Selected task IDs are fixed for this operation. Late results cannot change terminated tasks.'
            )}
            destructive={operation?.action !== 'resume'}
            isLoading={busy}
            handleConfirm={() => void confirm()}
          />
        </div>
      </SectionPageLayout.Content>
    </SectionPageLayout>
  )
}
