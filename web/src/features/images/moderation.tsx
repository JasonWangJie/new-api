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
import type { ColumnDef, RowSelectionState } from '@tanstack/react-table'
import { useCallback, useMemo, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'

import { ConfirmDialog } from '@/components/confirm-dialog'
import { DataTablePage, useDataTable } from '@/components/data-table'
import { Dialog } from '@/components/dialog'
import { SectionPageLayout } from '@/components/layout'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Checkbox } from '@/components/ui/checkbox'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Tabs, TabsList, TabsTrigger } from '@/components/ui/tabs'
import { Textarea } from '@/components/ui/textarea'
import { hasPermission } from '@/lib/admin-permissions'
import { useAuthStore } from '@/stores/auth-store'

import { imageRequest } from './api'
import { ImageResults } from './components/image-results'
import { ImageSelect } from './components/image-select'
import { imageLabel } from './lib/image-labels'
import type { CursorPage } from './types'

const MODERATION_TABS = [
  {
    id: 'publications',
    label: 'Stored-image submissions',
    path: '/api/admin/image-plaza/publications',
  },
  {
    id: 'submission-requests',
    label: 'Local submissions',
    path: '/api/admin/image-plaza/submission-requests',
  },
  { id: 'reports', label: 'Reports', path: '/api/admin/image-plaza/reports' },
  {
    id: 'assets',
    label: 'Server image assets',
    path: '/api/admin/image-library',
  },
  {
    id: 'cleanup-jobs',
    label: 'Cleanup jobs',
    path: '/api/admin/image-library/cleanup-jobs',
  },
] as const
type ModerationRow = {
  id: string | number
  user_id: number
  title?: string
  status?: string
  reason?: string
  metadata?: string
  category?: string
  message?: string
  resolution?: string
  asset_id?: string
  job_id?: string
  processed?: number
  total?: number
  errors?: number
  created_at: number
}
type AssetRow = { item: ModerationRow; view_url: string }

export function ImageModeration() {
  const { t } = useTranslation()
  const user = useAuthStore((state) => state.auth.user)
  const canManage = hasPermission(user, 'image_moderation', 'manage')
  const [tab, setTab] = useState('publications')
  const [status, setStatus] = useState('')
  const [cursor, setCursor] = useState('')
  const [history, setHistory] = useState<string[]>([])
  const [selection, setSelection] = useState<RowSelectionState>({})
  const [operation, setOperation] = useState<{
    ids: string[]
    action: string
  } | null>(null)
  const [reason, setReason] = useState('')
  const [busy, setBusy] = useState(false)
  const [preview, setPreview] = useState<{ id: string; url: string } | null>(
    null
  )
  const [metadata, setMetadata] = useState('')
  const [scope, setScope] = useState('expired')
  const [cleanupUser, setCleanupUser] = useState('')
  const [cleanupPreview, setCleanupPreview] = useState<{
    matched_items: number
    matched_bytes: number
    preview_fingerprint: string
  } | null>(null)
  const [cleanupConfirm, setCleanupConfirm] = useState(false)
  const [feedback, setFeedback] = useState<
    { id: string; status: string; reason?: string }[]
  >([])
  const active =
    MODERATION_TABS.find((candidate) => candidate.id === tab) ||
    MODERATION_TABS[0]
  const list = useQuery({
    queryKey: ['image-moderation', user?.id, tab, status, cursor],
    queryFn: ({ signal }) =>
      imageRequest<CursorPage<ModerationRow | AssetRow>>(
        `${active.path}?${new URLSearchParams({ status, cursor, limit: '30' })}`,
        'GET',
        undefined,
        signal
      ),
    refetchInterval: 10000,
  })
  const stats = useQuery({
    queryKey: ['image-storage-stats', user?.id],
    queryFn: ({ signal }) =>
      imageRequest<Record<string, number>>(
        '/api/admin/image-library/stats',
        'GET',
        undefined,
        signal
      ),
    refetchInterval: 30000,
  })
  const rows = (list.data?.items || []).map((item) =>
    'item' in item ? item.item : item
  )
  const statValues: Record<string, string | number | undefined> = {
    ...stats.data,
    total_bytes: stats.data
      ? `${((stats.data.total_bytes || 0) / 1048576).toFixed(1)} MiB`
      : undefined,
  }
  const view = useCallback(
    async (row: ModerationRow) => {
      try {
        const path =
          tab === 'assets'
            ? `/api/admin/image-library/${row.id}/view`
            : `/api/admin/image-plaza/publications/${row.id}/view`
        const output = await imageRequest<{ url: string }>(path)
        setPreview({ id: String(row.id), url: output.url })
      } catch (error) {
        toast.error(
          error instanceof Error ? error.message : t('Failed to load image')
        )
      }
    },
    [tab, t]
  )
  const openOperation = (ids: string[], action: string) => {
    setOperation({ ids, action })
    setReason('')
  }
  const columns = useMemo<ColumnDef<ModerationRow, unknown>[]>(
    () => [
      {
        id: 'select',
        header: ({ table }) => (
          <Checkbox
            aria-label={t('Select current page')}
            checked={table.getIsAllRowsSelected()}
            indeterminate={table.getIsSomeRowsSelected()}
            onCheckedChange={(value) => table.toggleAllRowsSelected(value)}
          />
        ),
        cell: ({ row }) => (
          <Checkbox
            aria-label={`${t('Select')} ${row.original.id}`}
            checked={row.getIsSelected()}
            onCheckedChange={(value) => row.toggleSelected(value)}
          />
        ),
        enableSorting: false,
        size: 44,
      },
      {
        id: 'id',
        header: t('ID'),
        cell: ({ row }) => (
          <div className='max-w-64'>
            <p className='truncate font-mono text-xs'>
              {row.original.job_id || row.original.id}
            </p>
            <p className='text-muted-foreground truncate text-xs'>
              {row.original.title ||
                row.original.category ||
                row.original.user_id}
            </p>
          </div>
        ),
      },
      {
        id: 'status',
        header: t('Status'),
        cell: ({ row }) => (
          <Badge variant='outline'>
            {t(imageLabel(row.original.status || 'active'))}
          </Badge>
        ),
      },
      {
        id: 'details',
        header: t('Details'),
        cell: ({ row }) => (
          <div className='max-w-72 text-xs'>
            {row.original.metadata ? (
              <Button
                size='sm'
                variant='link'
                onClick={() => setMetadata(row.original.metadata || '')}
              >
                {t('Review metadata')}
              </Button>
            ) : (
              row.original.message ||
              row.original.reason ||
              (row.original.total !== undefined
                ? `${row.original.processed || 0} / ${row.original.total} · ${t('Errors')}: ${row.original.errors || 0}`
                : '—')
            )}
          </div>
        ),
      },
      {
        id: 'created_at',
        header: t('Created At'),
        cell: ({ row }) => (
          <span className='text-xs'>
            {new Date(row.original.created_at * 1000).toLocaleString()}
          </span>
        ),
      },
      {
        id: 'actions',
        header: t('Actions'),
        cell: ({ row }) => (
          <div className='flex flex-wrap gap-1'>
            {['publications', 'assets'].includes(tab) && (
              <Button
                size='sm'
                variant='outline'
                onClick={() => void view(row.original)}
              >
                {t('Preview')}
              </Button>
            )}
            {canManage &&
              ['publications', 'submission-requests'].includes(tab) &&
              row.original.status === 'pending_review' && (
                <>
                  <Button
                    size='sm'
                    onClick={() =>
                      openOperation([String(row.original.id)], 'approve')
                    }
                  >
                    {t('Approve')}
                  </Button>
                  <Button
                    size='sm'
                    variant='outline'
                    onClick={() =>
                      openOperation([String(row.original.id)], 'reject')
                    }
                  >
                    {t('Reject')}
                  </Button>
                </>
              )}
            {canManage &&
              tab === 'publications' &&
              row.original.status === 'published' && (
                <Button
                  size='sm'
                  variant='outline'
                  onClick={() =>
                    openOperation([String(row.original.id)], 'hide')
                  }
                >
                  {t('Hide')}
                </Button>
              )}
            {canManage &&
              tab === 'publications' &&
              row.original.status === 'hidden' && (
                <Button
                  size='sm'
                  variant='outline'
                  onClick={() =>
                    openOperation([String(row.original.id)], 'restore')
                  }
                >
                  {t('Restore')}
                </Button>
              )}
            {canManage &&
              tab === 'reports' &&
              row.original.status === 'open' && (
                <>
                  <Button
                    size='sm'
                    variant='outline'
                    onClick={() =>
                      openOperation([String(row.original.id)], 'resolved')
                    }
                  >
                    {t('Resolve')}
                  </Button>
                  <Button
                    size='sm'
                    variant='ghost'
                    onClick={() =>
                      openOperation([String(row.original.id)], 'dismissed')
                    }
                  >
                    {t('Dismiss')}
                  </Button>
                </>
              )}
          </div>
        ),
      },
    ],
    [tab, canManage, t, view]
  )
  const { table } = useDataTable({
    data: rows,
    columns,
    manualPagination: true,
    enableSorting: false,
    enableRowSelection: canManage && tab === 'publications',
    rowSelection: selection,
    onRowSelectionChange: setSelection,
    getRowId: (row) => String(row.id),
  })
  const confirm = async () => {
    if (!operation) return
    setBusy(true)
    try {
      if (operation.ids.length > 1) {
        const result = await imageRequest<{
          items: { id: string; status: string; reason?: string }[]
        }>('/api/admin/image-plaza/publications/batch', 'POST', {
          publication_ids: operation.ids,
          action: operation.action,
          reason,
        })
        setFeedback(result.items)
      } else if (tab === 'reports') {
        await imageRequest(
          `${active.path}/${operation.ids[0]}/resolve`,
          'POST',
          { status: operation.action, resolution: reason }
        )
      } else {
        await imageRequest(
          `${active.path}/${operation.ids[0]}/${operation.action}`,
          'POST',
          { reason }
        )
      }
      setOperation(null)
      setSelection({})
      await Promise.all([list.refetch(), stats.refetch()])
    } catch (error) {
      toast.error(
        error instanceof Error ? error.message : t('Image request failed')
      )
    } finally {
      setBusy(false)
    }
  }
  const previewCleanup = async () => {
    try {
      setCleanupPreview(
        await imageRequest(
          '/api/admin/image-library/cleanup-jobs/preview',
          'POST',
          { scope, filters: { user_id: cleanupUser ? Number(cleanupUser) : 0 } }
        )
      )
    } catch (error) {
      toast.error(
        error instanceof Error ? error.message : t('Image request failed')
      )
    }
  }
  const createCleanup = async () => {
    if (!cleanupPreview) return
    setBusy(true)
    try {
      await imageRequest('/api/admin/image-library/cleanup-jobs', 'POST', {
        scope,
        filters: { user_id: cleanupUser ? Number(cleanupUser) : 0 },
        preview_fingerprint: cleanupPreview.preview_fingerprint,
      })
      setCleanupConfirm(false)
      setCleanupPreview(null)
      await list.refetch()
    } catch (error) {
      toast.error(
        error instanceof Error ? error.message : t('Image request failed')
      )
    } finally {
      setBusy(false)
    }
  }
  return (
    <SectionPageLayout>
      <SectionPageLayout.Title>{t('Image Moderation')}</SectionPageLayout.Title>
      <SectionPageLayout.Actions>
        <Button variant='outline' size='sm' onClick={() => void list.refetch()}>
          {t('Refresh')}
        </Button>
        <a
          href='/system-settings/operations/images'
          className='text-sm underline'
        >
          {t('Image Settings')}
        </a>
      </SectionPageLayout.Actions>
      <SectionPageLayout.Content>
        <div className='space-y-4'>
          <div className='grid grid-cols-2 gap-2 sm:grid-cols-5'>
            {[
              { key: 'item_count', label: 'Server image assets' },
              { key: 'total_bytes', label: 'Stored bytes' },
              { key: 'pending_review', label: 'Pending review' },
              { key: 'published', label: 'Published' },
              { key: 'open_reports', label: 'Open reports' },
            ].map(({ key, label }) => (
              <div key={key} className='rounded-lg border p-3'>
                <p className='text-muted-foreground text-xs'>{t(label)}</p>
                <p className='font-semibold tabular-nums'>
                  {statValues[key] ?? '—'}
                </p>
              </div>
            ))}
          </div>
          <Tabs
            value={tab}
            onValueChange={(value) => {
              setTab(String(value))
              setCursor('')
              setHistory([])
              setStatus('')
              setSelection({})
            }}
          >
            <TabsList className='h-auto flex-wrap justify-start'>
              {MODERATION_TABS.map((item) => (
                <TabsTrigger key={item.id} value={item.id}>
                  {t(item.label)}
                </TabsTrigger>
              ))}
            </TabsList>
          </Tabs>
          <div className='flex flex-wrap items-end gap-3'>
            <div className='w-56'>
              <ImageSelect
                label={t('Status')}
                value={status}
                options={[
                  { value: '', label: t('All') },
                  ...[
                    'pending_review',
                    'approved_pending_sync',
                    'synced',
                    'published',
                    'hidden',
                    'rejected',
                    'withdrawn',
                    'open',
                    'resolved',
                    'dismissed',
                    'queued',
                    'running',
                    'completed',
                    'completed_with_errors',
                  ].map((value) => ({ value, label: t(imageLabel(value)) })),
                ]}
                onChange={(value) => {
                  setStatus(value)
                  setCursor('')
                  setHistory([])
                  setSelection({})
                }}
              />
            </div>
            {canManage && tab === 'publications' && (
              <>
                <Button
                  disabled={!table.getSelectedRowModel().rows.length}
                  onClick={() =>
                    openOperation(
                      table
                        .getSelectedRowModel()
                        .rows.map((row) => String(row.original.id)),
                      'approve'
                    )
                  }
                >
                  {t('Approve selected')}
                </Button>
                <Button
                  variant='outline'
                  disabled={!table.getSelectedRowModel().rows.length}
                  onClick={() =>
                    openOperation(
                      table
                        .getSelectedRowModel()
                        .rows.map((row) => String(row.original.id)),
                      'reject'
                    )
                  }
                >
                  {t('Reject selected')}
                </Button>
              </>
            )}
          </div>
          {tab === 'submission-requests' && (
            <p
              role='status'
              className='bg-muted/30 rounded-lg border p-3 text-sm'
            >
              {t(
                'Local submissions contain metadata only. Approval allows later sync; the original image has not been reviewed.'
              )}
            </p>
          )}
          {list.isError && (
            <p role='alert' className='text-destructive text-sm'>
              {list.error.message}
            </p>
          )}
          {feedback.length > 0 && (
            <div aria-live='polite' className='text-xs'>
              {feedback.map((result) => (
                <p key={result.id}>
                  {result.id}: {t(imageLabel(result.status))} {result.reason}
                </p>
              ))}
            </div>
          )}
          <DataTablePage
            table={table}
            columns={columns}
            toolbarProps={null}
            showPagination={false}
            isLoading={list.isLoading}
            isFetching={list.isFetching}
            emptyTitle={t('No items found')}
            showMobileBulkActions
            mobileProps={{
              enableRowSelection: canManage && tab === 'publications',
            }}
          />
          <div className='flex justify-end gap-2'>
            <Button
              variant='outline'
              disabled={!history.length}
              onClick={() => {
                setCursor(history.at(-1) || '')
                setHistory((previous) => previous.slice(0, -1))
                setSelection({})
              }}
            >
              {t('Previous')}
            </Button>
            <Button
              variant='outline'
              disabled={!list.data?.next_cursor}
              onClick={() => {
                setHistory((previous) => [...previous, cursor])
                setCursor(list.data?.next_cursor || '')
                setSelection({})
              }}
            >
              {t('Next')}
            </Button>
          </div>
          {canManage && tab === 'cleanup-jobs' && (
            <section className='space-y-3 rounded-xl border p-4'>
              <h3 className='font-medium'>{t('Clean unreferenced images')}</h3>
              <div className='grid gap-3 sm:grid-cols-3'>
                <ImageSelect
                  label={t('Scope')}
                  value={scope}
                  options={[
                    'expired',
                    'deleted',
                    'user',
                    'async_results',
                    'all',
                  ].map((value) => ({ value, label: t(imageLabel(value)) }))}
                  onChange={(value) => {
                    setScope(value)
                    setCleanupPreview(null)
                  }}
                />
                <div className='space-y-2'>
                  <Label htmlFor='cleanup-user'>{t('User ID')}</Label>
                  <Input
                    id='cleanup-user'
                    type='number'
                    min={1}
                    value={cleanupUser}
                    onChange={(event) => {
                      setCleanupUser(event.target.value)
                      setCleanupPreview(null)
                    }}
                  />
                </div>
                <Button
                  variant='outline'
                  className='self-end'
                  onClick={() => void previewCleanup()}
                >
                  {t('Preview cleanup')}
                </Button>
              </div>
              {cleanupPreview && (
                <div className='flex items-center justify-between gap-3 text-sm'>
                  <span>
                    {cleanupPreview.matched_items} ·{' '}
                    {(cleanupPreview.matched_bytes / 1048576).toFixed(1)} MiB
                  </span>
                  <Button
                    variant='destructive'
                    onClick={() => setCleanupConfirm(true)}
                  >
                    {t('Create cleanup job')}
                  </Button>
                </div>
              )}
            </section>
          )}
          <ConfirmDialog
            open={!!operation}
            onOpenChange={(open) => {
              if (!open && !busy) setOperation(null)
            }}
            title={t('Confirm moderation action')}
            desc={t(
              'This action uses the selected item IDs. Rejected and hidden items require a reason.'
            )}
            isLoading={busy}
            handleConfirm={() => void confirm()}
            destructive={
              operation?.action === 'reject' || operation?.action === 'hide'
            }
          >
            <Label htmlFor='moderation-reason'>
              {t(tab === 'reports' ? 'Resolution notes' : 'Reason')}
            </Label>
            <Textarea
              id='moderation-reason'
              value={reason}
              onChange={(event) => setReason(event.target.value)}
              maxLength={10000}
            />
          </ConfirmDialog>
          <ConfirmDialog
            open={cleanupConfirm}
            onOpenChange={setCleanupConfirm}
            title={t('Delete unreferenced image files')}
            desc={t(
              'References are checked again before deletion. Deleted files cannot be restored.'
            )}
            destructive
            isLoading={busy}
            handleConfirm={() => void createCleanup()}
          />
          {preview && (
            <Dialog
              open
              onOpenChange={(open) => {
                if (!open) setPreview(null)
              }}
              title={t('Image Preview')}
              description={preview.id}
              contentClassName='sm:max-w-3xl'
            >
              <ImageResults images={[preview]} />
            </Dialog>
          )}
          {metadata && (
            <Dialog
              open
              onOpenChange={(open) => {
                if (!open) setMetadata('')
              }}
              title={t('Review metadata')}
              description={t('The original is held on the user’s device.')}
            >
              <pre className='bg-muted overflow-auto rounded-lg p-3 text-xs whitespace-pre-wrap'>
                {metadata}
              </pre>
            </Dialog>
          )}
        </div>
      </SectionPageLayout.Content>
    </SectionPageLayout>
  )
}
