/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.
*/
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import type {
  ColumnDef,
  ColumnFiltersState,
  PaginationState,
} from '@tanstack/react-table'
import type { TFunction } from 'i18next'
import { CheckCircle2, Eye, XCircle } from 'lucide-react'
import { useMemo, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'

import { ConfirmDialog } from '@/components/confirm-dialog'
import { CopyButton } from '@/components/copy-button'
import { DataTablePage, useDataTable } from '@/components/data-table'
import { Dialog } from '@/components/dialog'
import { StatusBadge, type StatusVariant } from '@/components/status-badge'
import { Button } from '@/components/ui/button'
import { Textarea } from '@/components/ui/textarea'
import { handleServerError } from '@/lib/handle-server-error'

import { completeInvoice, getInvoiceRequests, rejectInvoice } from '../api'
import { formatInvoiceAmount } from '../lib/format'
import type { InvoiceRequest, InvoiceStatus } from '../types'

type InvoiceRequestsTableProps = {
  admin?: boolean
  canManage?: boolean
}

const statusVariants: Record<InvoiceStatus, StatusVariant> = {
  pending: 'warning',
  completed: 'success',
  rejected: 'danger',
}

function getInvoiceStatusLabel(status: InvoiceStatus, t: TFunction): string {
  if (status === 'pending') return t('Pending')
  if (status === 'completed') return t('Completed')
  return t('Rejected')
}

function InvoiceDetailsDialog(props: {
  request: InvoiceRequest | null
  onOpenChange: (open: boolean) => void
}) {
  const { t, i18n } = useTranslation()
  const request = props.request
  if (!request) return null

  return (
    <Dialog
      open
      onOpenChange={props.onOpenChange}
      title={t('Invoice record #{{id}}', { id: request.id })}
      description={t('{{count}} recharge orders, totaling {{amount}}', {
        count: request.items.length,
        amount: formatInvoiceAmount(
          request.total_amount_cents,
          i18n.resolvedLanguage
        ),
      })}
      contentClassName='sm:max-w-3xl'
    >
      <div className='grid gap-3 text-sm sm:grid-cols-2'>
        <div>
          <span className='text-muted-foreground'>{t('Status')}</span>
          <div className='mt-1'>
            <StatusBadge
              copyable={false}
              variant={statusVariants[request.status]}
              label={getInvoiceStatusLabel(request.status, t)}
            />
          </div>
        </div>
        <div>
          <span className='text-muted-foreground'>{t('Source')}</span>
          <p className='mt-1 font-medium'>
            {t(
              request.source === 'admin_history'
                ? 'Historical invoice supplement'
                : 'User application'
            )}
          </p>
        </div>
        {request.source === 'user' ? (
          <>
            <div>
              <span className='text-muted-foreground'>{t('Company name')}</span>
              <p className='mt-1 font-medium break-words'>
                {request.company_name}
              </p>
            </div>
            <div>
              <span className='text-muted-foreground'>
                {t('Enterprise tax ID')}
              </span>
              <p className='mt-1 font-mono text-xs break-all'>
                {request.tax_id}
              </p>
            </div>
            <div className='sm:col-span-2'>
              <span className='text-muted-foreground'>
                {t('Invoice email')}
              </span>
              <p className='mt-1 break-all'>{request.email}</p>
            </div>
          </>
        ) : null}
        {request.reject_reason ? (
          <div className='border-destructive/30 bg-destructive/5 rounded-lg border p-3 sm:col-span-2'>
            <span className='text-destructive font-medium'>
              {t('Rejection reason')}
            </span>
            <p className='mt-1 whitespace-pre-wrap'>{request.reject_reason}</p>
          </div>
        ) : null}
        {request.note ? (
          <div className='rounded-lg border p-3 sm:col-span-2'>
            <span className='text-muted-foreground'>{t('Admin note')}</span>
            <p className='mt-1 whitespace-pre-wrap'>{request.note}</p>
          </div>
        ) : null}
        <div className='sm:col-span-2'>
          <span className='text-muted-foreground'>{t('Recharge orders')}</span>
          <div className='mt-2 divide-y rounded-lg border'>
            {request.items.map((item) => (
              <div
                key={item.id}
                className='flex flex-col gap-1 px-3 py-2 sm:flex-row sm:items-center sm:justify-between'
              >
                <div className='flex min-w-0 items-center gap-1'>
                  <span className='truncate font-mono text-xs'>
                    {item.trade_no}
                  </span>
                  <CopyButton
                    value={item.trade_no}
                    className='size-7'
                    tooltip={t('Copy order number')}
                  />
                </div>
                <span className='font-medium tabular-nums'>
                  {formatInvoiceAmount(
                    item.amount_cents,
                    i18n.resolvedLanguage
                  )}
                </span>
              </div>
            ))}
          </div>
        </div>
        {request.processed_time > 0 ? (
          <div className='sm:col-span-2'>
            <span className='text-muted-foreground'>{t('Processed by')}</span>
            <p className='mt-1'>
              {request.operator_username || `#${request.operator_id}`}
              {' · '}
              {new Date(request.processed_time * 1000).toLocaleString(
                i18n.resolvedLanguage
              )}
            </p>
          </div>
        ) : null}
      </div>
    </Dialog>
  )
}

export function InvoiceRequestsTable(props: InvoiceRequestsTableProps) {
  const { t, i18n } = useTranslation()
  const queryClient = useQueryClient()
  const [pagination, setPagination] = useState<PaginationState>({
    pageIndex: 0,
    pageSize: 20,
  })
  const [keyword, setKeyword] = useState('')
  const [columnFilters, setColumnFilters] = useState<ColumnFiltersState>([])
  const [details, setDetails] = useState<InvoiceRequest | null>(null)
  const [completeTarget, setCompleteTarget] = useState<InvoiceRequest | null>(
    null
  )
  const [rejectTarget, setRejectTarget] = useState<InvoiceRequest | null>(null)
  const [rejectReason, setRejectReason] = useState('')
  const status =
    ((
      columnFilters.find((item) => item.id === 'status')?.value as
        | string[]
        | undefined
    )?.[0] as InvoiceStatus | undefined) ?? ''

  const query = useQuery({
    queryKey: [
      'invoice-requests',
      props.admin ? 'admin' : 'self',
      pagination.pageIndex + 1,
      pagination.pageSize,
      keyword,
      status,
    ],
    queryFn: () =>
      getInvoiceRequests({
        page: pagination.pageIndex + 1,
        pageSize: pagination.pageSize,
        status,
        keyword,
        admin: props.admin,
      }),
    placeholderData: (previous) => previous,
  })

  const invalidateInvoiceData = async () => {
    await Promise.all([
      queryClient.invalidateQueries({ queryKey: ['invoice-requests'] }),
      queryClient.invalidateQueries({ queryKey: ['invoice-eligible-orders'] }),
    ])
  }
  const completion = useMutation({
    mutationFn: (request: InvoiceRequest) => completeInvoice(request.id),
    onSuccess: async () => {
      toast.success(t('Invoice marked as completed'))
      setCompleteTarget(null)
      await invalidateInvoiceData()
    },
    onError: async (error) => {
      handleServerError(error, t('Failed to complete invoice'))
      await invalidateInvoiceData()
    },
  })
  const rejection = useMutation({
    mutationFn: (request: InvoiceRequest) =>
      rejectInvoice(request.id, rejectReason.trim()),
    onSuccess: async () => {
      toast.success(t('Invoice application rejected'))
      setRejectTarget(null)
      setRejectReason('')
      await invalidateInvoiceData()
    },
    onError: async (error) => {
      handleServerError(error, t('Failed to reject invoice'))
      await invalidateInvoiceData()
    },
  })

  const columns = useMemo<ColumnDef<InvoiceRequest, unknown>[]>(() => {
    const result: ColumnDef<InvoiceRequest, unknown>[] = [
      {
        accessorKey: 'id',
        header: t('Record ID'),
        cell: ({ row }) => (
          <span className='font-mono text-xs'>#{row.original.id}</span>
        ),
        meta: { mobileTitle: true, label: t('Record ID') },
      },
    ]
    if (props.admin) {
      result.push({
        id: 'user',
        header: t('User'),
        cell: ({ row }) => (
          <div className='min-w-0'>
            <p className='truncate font-medium'>
              {row.original.display_name || row.original.username || '-'}
            </p>
            <p className='text-muted-foreground truncate text-xs'>
              #{row.original.user_id} · {row.original.username || '-'}
            </p>
          </div>
        ),
        meta: { label: t('User') },
      })
    }
    result.push(
      {
        accessorKey: 'status',
        header: t('Status'),
        cell: ({ row }) => (
          <StatusBadge
            copyable={false}
            variant={statusVariants[row.original.status]}
            label={getInvoiceStatusLabel(row.original.status, t)}
          />
        ),
        meta: { mobileBadge: true, label: t('Status') },
      },
      {
        id: 'company',
        header: t('Invoice information'),
        cell: ({ row }) =>
          row.original.source === 'admin_history' ? (
            <span>{t('Historical invoice supplement')}</span>
          ) : (
            <div className='min-w-0'>
              <p className='truncate font-medium'>
                {row.original.company_name}
              </p>
              <p className='text-muted-foreground truncate text-xs'>
                {row.original.email}
              </p>
              {row.original.reject_reason ? (
                <p className='text-destructive mt-1 line-clamp-2 text-xs'>
                  {t('Rejection reason')}: {row.original.reject_reason}
                </p>
              ) : null}
            </div>
          ),
        meta: { label: t('Invoice information') },
      },
      {
        accessorKey: 'total_amount_cents',
        header: t('Invoice amount'),
        cell: ({ row }) => (
          <div>
            <p className='font-medium tabular-nums'>
              {formatInvoiceAmount(
                row.original.total_amount_cents,
                i18n.resolvedLanguage
              )}
            </p>
            <p className='text-muted-foreground text-xs'>
              {t('{{count}} orders', { count: row.original.items.length })}
            </p>
          </div>
        ),
        meta: { label: t('Invoice amount') },
      },
      {
        accessorKey: 'create_time',
        header: t('Applied at'),
        cell: ({ row }) => (
          <div className='text-xs'>
            <p className='whitespace-nowrap tabular-nums'>
              {new Date(row.original.create_time * 1000).toLocaleString(
                i18n.resolvedLanguage
              )}
            </p>
            {row.original.processed_time ? (
              <>
                <p className='text-muted-foreground mt-1 whitespace-nowrap'>
                  {t('Processed')}:{' '}
                  {new Date(row.original.processed_time * 1000).toLocaleString(
                    i18n.resolvedLanguage
                  )}
                </p>
                <p className='text-muted-foreground mt-0.5 truncate'>
                  {t('Processed by')}:{' '}
                  {row.original.operator_username ||
                    `#${row.original.operator_id}`}
                </p>
              </>
            ) : null}
          </div>
        ),
        meta: { label: t('Applied at') },
      },
      {
        id: 'actions',
        header: t('Actions'),
        enableSorting: false,
        cell: ({ row }) => (
          <div className='flex flex-wrap justify-end gap-1'>
            <Button
              size='icon'
              variant='ghost'
              className='size-8'
              aria-label={t('View invoice details')}
              onClick={() => setDetails(row.original)}
            >
              <Eye aria-hidden='true' />
            </Button>
            {props.admin &&
            props.canManage &&
            row.original.status === 'pending' ? (
              <>
                <Button
                  size='icon'
                  variant='ghost'
                  className='text-success size-8'
                  aria-label={t('Mark invoice completed')}
                  onClick={() => setCompleteTarget(row.original)}
                >
                  <CheckCircle2 aria-hidden='true' />
                </Button>
                <Button
                  size='icon'
                  variant='ghost'
                  className='text-destructive size-8'
                  aria-label={t('Reject invoice application')}
                  onClick={() => {
                    setRejectReason('')
                    setRejectTarget(row.original)
                  }}
                >
                  <XCircle aria-hidden='true' />
                </Button>
              </>
            ) : null}
          </div>
        ),
        meta: { mobileHidden: true },
      }
    )
    return result
  }, [i18n.resolvedLanguage, props.admin, props.canManage, t])

  const { table } = useDataTable({
    data: query.data?.items ?? [],
    columns,
    pagination,
    onPaginationChange: setPagination,
    globalFilter: keyword,
    onGlobalFilterChange: (updater) => {
      setKeyword((current) =>
        typeof updater === 'function' ? updater(current) : updater
      )
      setPagination((current) => ({ ...current, pageIndex: 0 }))
    },
    columnFilters,
    onColumnFiltersChange: (updater) => {
      setColumnFilters((current) =>
        typeof updater === 'function' ? updater(current) : updater
      )
      setPagination((current) => ({ ...current, pageIndex: 0 }))
    },
    manualFiltering: true,
    manualPagination: true,
    totalCount: query.data?.total ?? 0,
  })

  return (
    <>
      <DataTablePage
        table={table}
        columns={columns}
        isLoading={query.isLoading}
        isFetching={query.isFetching}
        emptyTitle={t('No invoice records')}
        emptyDescription={t(
          'Invoice applications and historical records will appear here.'
        )}
        toolbarProps={{
          searchPlaceholder: props.admin
            ? t('Search by user ID, username, name or email...')
            : t('Search invoice records...'),
          searchDebounceMs: 400,
          filters: [
            {
              columnId: 'status',
              title: t('Status'),
              singleSelect: true,
              options: [
                { value: 'pending', label: t('Pending') },
                { value: 'completed', label: t('Completed') },
                { value: 'rejected', label: t('Rejected') },
              ],
            },
          ],
        }}
        fixedHeight={false}
        paginationInFooter={false}
      />

      <InvoiceDetailsDialog
        request={details}
        onOpenChange={(open) => !open && setDetails(null)}
      />
      <ConfirmDialog
        open={completeTarget !== null}
        onOpenChange={(open) => !open && setCompleteTarget(null)}
        title={t('Mark this invoice as completed?')}
        desc={t(
          'Confirm that the invoice has been issued offline. The associated orders will remain occupied.'
        )}
        confirmText={
          completion.isPending ? t('Saving...') : t('Mark completed')
        }
        isLoading={completion.isPending}
        handleConfirm={() => {
          if (completeTarget) completion.mutate(completeTarget)
        }}
      />
      <ConfirmDialog
        destructive
        open={rejectTarget !== null}
        onOpenChange={(open) => {
          if (!open && !rejection.isPending) setRejectTarget(null)
        }}
        title={t('Reject this invoice application?')}
        desc={t(
          'The user will see the reason and can use these recharge orders in a new application.'
        )}
        confirmText={rejection.isPending ? t('Rejecting...') : t('Reject')}
        isLoading={rejection.isPending}
        disabled={!rejectReason.trim()}
        handleConfirm={() => {
          if (rejectTarget && rejectReason.trim()) {
            rejection.mutate(rejectTarget)
          }
        }}
      >
        <div className='space-y-2'>
          <label
            className='text-sm font-medium'
            htmlFor='invoice-reject-reason'
          >
            {t('Rejection reason')}
          </label>
          <Textarea
            id='invoice-reject-reason'
            value={rejectReason}
            maxLength={1000}
            placeholder={t('Explain why this invoice application was rejected')}
            onChange={(event) => setRejectReason(event.currentTarget.value)}
          />
        </div>
      </ConfirmDialog>
    </>
  )
}
