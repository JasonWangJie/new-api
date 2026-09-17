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
import { Dialog } from '@/components/dialog'
import { SectionPageLayout } from '@/components/layout'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Checkbox } from '@/components/ui/checkbox'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { hasPermission } from '@/lib/admin-permissions'
import { useAuthStore } from '@/stores/auth-store'

import {
  getImageTask,
  getImageTasks,
  imageRequest,
  imageTasksPath,
} from './api'
import { ImageResults } from './components/image-results'
import { ImageSelect } from './components/image-select'
import { imageLabel } from './lib/image-labels'
import { terminalImageStatus } from './lib/image-request'
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
  { key: 'storage_provider', label: 'Storage provider' },
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
    refetchInterval: 10000,
  })
  const columns = useMemo<ColumnDef<ImageTask, unknown>[]>(
    () => [
      ...(admin
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
              size: 44,
            } as ColumnDef<ImageTask, unknown>,
          ]
        : []),
      {
        id: 'task_id',
        header: t('Task ID'),
        cell: ({ row }) => (
          <div className='max-w-64'>
            <Button
              variant='link'
              className='h-auto max-w-full truncate p-0 font-mono text-xs'
              onClick={() => setDetail(row.original.id)}
            >
              {row.original.id}
            </Button>
            <p className='text-muted-foreground truncate text-xs'>
              {row.original.model}
            </p>
          </div>
        ),
        enableSorting: false,
      },
      {
        id: 'status',
        accessorKey: 'status',
        header: ({ column }) => (
          <DataTableColumnHeader column={column} title={t('Status')} />
        ),
        cell: ({ row }) => (
          <div className='space-y-1'>
            <Badge variant={row.original.error_code ? 'warning' : 'outline'}>
              {t(imageLabel(row.original.status))}
            </Badge>
            <p className='text-muted-foreground text-xs'>
              {row.original.progress}% ·{' '}
              {t(imageLabel(row.original.billing_status))}
            </p>
          </div>
        ),
      },
      {
        id: 'platform',
        header: t('Platform'),
        cell: ({ row }) => (
          <div className='text-xs'>
            {row.original.platform} · {row.original.protocol}
            <p className='text-muted-foreground'>
              {row.original.request_type} · {row.original.group}
            </p>
          </div>
        ),
        enableSorting: false,
      },
      {
        id: 'image_count',
        accessorKey: 'image_count',
        header: ({ column }) => (
          <DataTableColumnHeader column={column} title={t('Images')} />
        ),
        cell: ({ row }) => (
          <span className='text-xs tabular-nums'>
            {row.original.result_count} / {row.original.image_count}
            <span className='text-muted-foreground block'>
              {row.original.actual_size || row.original.requested_size}
            </span>
          </span>
        ),
      },
      {
        id: 'quota',
        header: ({ column }) => (
          <DataTableColumnHeader column={column} title={t('Cost')} />
        ),
        cell: ({ row }) => (
          <span className='font-mono text-xs'>
            ${row.original.cost.toFixed(6)}
          </span>
        ),
      },
      {
        id: 'created_at',
        accessorKey: 'created_at',
        header: ({ column }) => (
          <DataTableColumnHeader column={column} title={t('Created At')} />
        ),
        cell: ({ row }) => (
          <span className='text-xs'>
            {new Date(row.original.created_at * 1000).toLocaleString()}
          </span>
        ),
      },
      ...(admin
        ? [
            {
              id: 'channel',
              header: t('Channel / User'),
              cell: ({ row }) => (
                <span className='text-xs'>
                  {row.original.channel_id || '—'} / {row.original.user_id}
                </span>
              ),
              enableSorting: false,
            } as ColumnDef<ImageTask, unknown>,
          ]
        : []),
      {
        id: 'actions',
        header: t('Actions'),
        cell: ({ row }) => (
          <div className='flex flex-wrap gap-1'>
            <Button
              size='sm'
              variant='outline'
              onClick={() => setDetail(row.original.id)}
            >
              {t('Details')}
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
                size='sm'
                variant='ghost'
                onClick={() =>
                  setOperation({ ids: [row.original.id], action: 'terminate' })
                }
              >
                {t('Terminate')}
              </Button>
            )}
          </div>
        ),
        enableSorting: false,
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
    enableRowSelection: admin,
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
  return (
    <SectionPageLayout fixedContent>
      <SectionPageLayout.Title>
        {t(admin ? 'Admin Image Tasks' : 'Async Image Tasks')}
      </SectionPageLayout.Title>
      <SectionPageLayout.Actions>
        <Button
          variant='outline'
          size='sm'
          onClick={() => void tasks.refetch()}
        >
          {t('Refresh')}
        </Button>
        {canManage && (
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
        )}
      </SectionPageLayout.Actions>
      <SectionPageLayout.Content>
        <div className='flex h-full min-h-0 flex-col gap-3'>
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
              actions={
                <Button type='submit' form='image-task-filter-form'>
                  {t('Apply filters')}
                </Button>
              }
            >
              <div className='grid max-h-[32dvh] grid-cols-2 gap-3 overflow-y-auto p-1 sm:grid-cols-3 xl:grid-cols-5'>
                {field('q', 'Search tasks')}
                {field('start_date', 'Start date', 'date')}
                {field('end_date', 'End date', 'date')}
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
          <div className='grid shrink-0 grid-cols-3 gap-2 sm:grid-cols-6'>
            {[
              'total',
              'queued',
              'processing',
              'succeeded',
              'failed',
              'image_count',
            ].map((name) => (
              <div key={name} className='rounded-lg border px-3 py-2'>
                <p className='text-muted-foreground text-xs'>
                  {t(imageLabel(name))}
                </p>
                <p className='text-lg font-semibold tabular-nums'>
                  {tasks.data?.stats[name] || 0}
                </p>
              </div>
            ))}
          </div>
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
              showMobileBulkActions
              mobileProps={{ enableRowSelection: admin }}
            />
          </div>
          {detail && (
            <ImageTaskDetails
              key={detail}
              id={detail}
              admin={admin}
              userId={userId}
              onClose={() => setDetail('')}
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

function ImageTaskDetails({
  id,
  admin,
  userId,
  onClose,
}: {
  id: string
  admin: boolean
  userId: number
  onClose: () => void
}) {
  const { t } = useTranslation()
  const task = useQuery({
    queryKey: ['image-task-details', userId, admin, id],
    queryFn: ({ signal }) => getImageTask(admin, id, signal),
    refetchInterval: (query) =>
      query.state.data &&
      terminalImageStatus(
        query.state.data.task.status,
        query.state.data.task.next_attempt_at
      )
        ? false
        : 5000,
  })
  const images = useQuery({
    queryKey: [
      'image-task-detail-results',
      userId,
      admin,
      id,
      task.data?.results,
    ],
    queryFn: ({ signal }) =>
      Promise.all(
        (task.data?.results || []).map(async (result) => ({
          id: String(result.image_index),
          url: (
            await imageRequest<{ url: string }>(
              result.view_url,
              'GET',
              undefined,
              signal
            )
          ).url,
          description: `${result.width} × ${result.height} · ${(result.byte_size / 1048576).toFixed(2)} MiB`,
        }))
      ),
    enabled: !!task.data?.results.length,
    staleTime: 30000,
  })
  const archive = async (index: string) => {
    try {
      await imageRequest('/api/user/image-library/from-task', 'POST', {
        task_id: id,
        image_index: Number(index),
      })
      toast.success(t('Archived to server storage'))
    } catch (error) {
      toast.error(
        error instanceof Error ? error.message : t('Image request failed')
      )
    }
  }
  return (
    <Dialog
      open
      onOpenChange={(open) => {
        if (!open) onClose()
      }}
      title={t('Image task details')}
      description={id}
      contentClassName='sm:max-w-5xl'
    >
      <div className='space-y-5'>
        {task.isLoading && <p>{t('Loading...')}</p>}
        {task.isError && <p role='alert'>{task.error.message}</p>}
        {task.data && (
          <>
            <div className='flex flex-wrap items-center gap-3'>
              <Badge variant='outline'>
                {t(imageLabel(task.data.task.status))}
              </Badge>
              <span className='text-sm'>
                {task.data.task.model} · {task.data.task.platform}
              </span>
              <CopyButton value={id} size='sm'>
                {t('Copy Task ID')}
              </CopyButton>
            </div>
            {task.data.task.error_message && (
              <p className='text-destructive text-sm'>
                {task.data.task.error_message}
              </p>
            )}
            <ImageResults
              images={images.data || []}
              actions={
                !admin
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
            <dl className='grid grid-cols-2 gap-3 rounded-xl border p-3 text-sm'>
              {[
                { key: 'billing_status', label: 'Billing status' },
                { key: 'group', label: 'Group' },
                { key: 'api_key_id', label: 'API Key ID' },
                { key: 'retry_count', label: 'Retries' },
              ].map(({ key, label }) => (
                <div key={key}>
                  <dt className='text-muted-foreground'>{t(label)}</dt>
                  <dd>
                    {key === 'billing_status'
                      ? t(imageLabel(task.data.task.billing_status))
                      : String(task.data.task[key as keyof ImageTask] ?? '')}
                  </dd>
                </div>
              ))}
            </dl>
            {admin && task.data.task.attempts && (
              <pre className='bg-muted overflow-auto rounded-lg p-3 text-xs'>
                {task.data.task.attempts}
              </pre>
            )}
            <section className='space-y-2'>
              <h4 className='text-sm font-semibold'>{t('Task events')}</h4>
              {task.data.events.map((event) => (
                <div key={event.id} className='border-l-2 pl-3 text-xs'>
                  <span className='text-muted-foreground'>
                    {new Date(event.created_at * 1000).toLocaleString()}
                  </span>
                  <p>
                    {t(imageLabel(event.status))} · {event.event_type}
                  </p>
                  {event.message && (
                    <p className='text-muted-foreground'>{event.message}</p>
                  )}
                </div>
              ))}
            </section>
          </>
        )}
      </div>
    </Dialog>
  )
}
