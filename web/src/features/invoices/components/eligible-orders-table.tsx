/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.
*/
import { useQuery } from '@tanstack/react-query'
import type {
  ColumnDef,
  ColumnFiltersState,
  OnChangeFn,
  PaginationState,
  RowSelectionState,
  Updater,
} from '@tanstack/react-table'
import { FileCheck2 } from 'lucide-react'
import { useCallback, useMemo, useState } from 'react'
import { useTranslation } from 'react-i18next'

import { CopyButton } from '@/components/copy-button'
import {
  DataTableBulkActions,
  DataTablePage,
  useDataTable,
} from '@/components/data-table'
import { Button } from '@/components/ui/button'
import { Checkbox } from '@/components/ui/checkbox'

import { getEligibleInvoiceOrders } from '../api'
import {
  formatInvoiceAmount,
  formatInvoiceDateTime,
  sumInvoiceOrders,
} from '../lib/format'
import { reconcileInvoiceOrderSelection } from '../lib/selection'
import type { InvoiceEligibleOrder } from '../types'

type EligibleOrdersTableProps = {
  adminUserId?: number
  selectedOrders: Map<number, InvoiceEligibleOrder>
  onSelectedOrdersChange: (orders: Map<number, InvoiceEligibleOrder>) => void
  actionLabel: string
  onAction: () => void
  actionDisabled?: boolean
}

function resolveUpdater<T>(updater: Updater<T>, previous: T): T {
  return typeof updater === 'function'
    ? (updater as (value: T) => T)(previous)
    : updater
}

export function EligibleOrdersTable(props: EligibleOrdersTableProps) {
  const { t, i18n } = useTranslation()
  const [pagination, setPagination] = useState<PaginationState>({
    pageIndex: 0,
    pageSize: 20,
  })
  const [keyword, setKeyword] = useState('')
  const [columnFilters, setColumnFilters] = useState<ColumnFiltersState>([])

  const query = useQuery({
    queryKey: [
      'invoice-eligible-orders',
      props.adminUserId ?? 'self',
      pagination.pageIndex + 1,
      pagination.pageSize,
      keyword,
    ],
    queryFn: () =>
      getEligibleInvoiceOrders({
        page: pagination.pageIndex + 1,
        pageSize: pagination.pageSize,
        keyword,
        adminUserId: props.adminUserId,
      }),
    enabled: props.adminUserId === undefined || props.adminUserId > 0,
    placeholderData: (previous) => previous,
  })

  const orders = useMemo(() => query.data?.items ?? [], [query.data?.items])
  const rowSelection = useMemo<RowSelectionState>(() => {
    const selection: RowSelectionState = {}
    for (const id of props.selectedOrders.keys()) selection[String(id)] = true
    return selection
  }, [props.selectedOrders])

  const columns = useMemo<ColumnDef<InvoiceEligibleOrder, unknown>[]>(
    () => [
      {
        id: 'select',
        size: 44,
        enableSorting: false,
        header: ({ table }) => (
          <Checkbox
            aria-label={t('Select current page')}
            checked={table.getIsAllPageRowsSelected()}
            indeterminate={table.getIsSomePageRowsSelected()}
            onCheckedChange={(value) =>
              table.toggleAllPageRowsSelected(Boolean(value))
            }
          />
        ),
        cell: ({ row }) => (
          <Checkbox
            aria-label={t('Select order {{order}}', {
              order: row.original.trade_no,
            })}
            checked={row.getIsSelected()}
            onCheckedChange={(value) => row.toggleSelected(Boolean(value))}
          />
        ),
        meta: { mobileHidden: true },
      },
      {
        accessorKey: 'trade_no',
        header: t('Recharge order number'),
        cell: ({ row }) => (
          <div className='flex min-w-0 items-center gap-1'>
            <span className='truncate font-mono text-xs'>
              {row.original.trade_no}
            </span>
            <CopyButton
              value={row.original.trade_no}
              className='size-7'
              tooltip={t('Copy order number')}
            />
          </div>
        ),
        meta: { mobileTitle: true, label: t('Recharge order number') },
      },
      {
        accessorKey: 'amount_cents',
        header: t('Invoice amount'),
        cell: ({ row }) => (
          <span className='font-medium tabular-nums'>
            {formatInvoiceAmount(
              row.original.amount_cents,
              i18n.resolvedLanguage
            )}
          </span>
        ),
        meta: { label: t('Invoice amount') },
      },
      {
        accessorKey: 'complete_time',
        header: t('Payment completed at'),
        cell: ({ row }) => (
          <time className='text-sm whitespace-nowrap tabular-nums'>
            {formatInvoiceDateTime(
              row.original.complete_time,
              i18n.resolvedLanguage
            )}
          </time>
        ),
        meta: { label: t('Payment completed at') },
      },
    ],
    [i18n.resolvedLanguage, t]
  )

  const onRowSelectionChange = useCallback<OnChangeFn<RowSelectionState>>(
    (updater) => {
      const nextSelection = resolveUpdater(updater, rowSelection)
      const nextOrders = reconcileInvoiceOrderSelection(
        props.selectedOrders,
        orders,
        nextSelection
      )
      props.onSelectedOrdersChange(nextOrders)
    },
    [orders, props, rowSelection]
  )

  const onGlobalFilterChange = useCallback<OnChangeFn<string>>(
    (updater) => {
      const nextKeyword = resolveUpdater(updater, keyword)
      setKeyword(nextKeyword)
      setPagination((current) => ({ ...current, pageIndex: 0 }))
      props.onSelectedOrdersChange(new Map())
    },
    [keyword, props]
  )

  const { table } = useDataTable({
    data: orders,
    columns,
    getRowId: (order) => String(order.top_up_id),
    enableRowSelection: true,
    rowSelection,
    onRowSelectionChange,
    globalFilter: keyword,
    onGlobalFilterChange,
    columnFilters,
    onColumnFiltersChange: setColumnFilters,
    pagination,
    onPaginationChange: setPagination,
    manualFiltering: true,
    manualPagination: true,
    totalCount: query.data?.total ?? 0,
  })

  const selectedTotal = sumInvoiceOrders(props.selectedOrders.values())
  const bulkActions = (
    <DataTableBulkActions
      table={table}
      entityName={t('recharge order')}
      selectedCount={props.selectedOrders.size}
      onClearSelection={() => props.onSelectedOrdersChange(new Map())}
    >
      <span className='px-1 text-sm font-medium tabular-nums'>
        {formatInvoiceAmount(selectedTotal, i18n.resolvedLanguage)}
      </span>
      <Button
        size='sm'
        className='h-8 gap-1.5'
        disabled={props.actionDisabled}
        onClick={props.onAction}
      >
        <FileCheck2 aria-hidden='true' />
        {props.actionLabel}
      </Button>
    </DataTableBulkActions>
  )

  return (
    <DataTablePage
      table={table}
      columns={columns}
      isLoading={query.isLoading}
      isFetching={query.isFetching}
      emptyTitle={t('No invoiceable recharge orders')}
      emptyDescription={t(
        'Paid Epay recharge orders that have not been invoiced will appear here.'
      )}
      toolbarProps={{
        searchPlaceholder: t('Search by recharge order number...'),
        searchDebounceMs: 400,
      }}
      mobileProps={{ enableRowSelection: true }}
      showMobileBulkActions
      bulkActions={bulkActions}
      applyHeaderSize
      fixedHeight={false}
      paginationInFooter={false}
    />
  )
}
