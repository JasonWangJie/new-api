/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.
*/
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import i18next from 'i18next'
import { useState } from 'react'
import { afterEach, describe, expect, it, vi } from 'vitest'

import type { InvoiceEligibleOrder, InvoiceRequest } from '../../types'
import { EligibleOrdersTable } from '../eligible-orders-table'
import { InvoiceApplicationDialog } from '../invoice-application-dialog'
import { InvoiceRequestsTable } from '../invoice-requests-table'

const apiMocks = vi.hoisted(() => ({
  completeInvoice: vi.fn(),
  createInvoice: vi.fn(),
  getEligibleInvoiceOrders: vi.fn(),
  getInvoiceRequests: vi.fn(),
  rejectInvoice: vi.fn(),
}))

vi.mock('../../api', () => apiMocks)
vi.mock('@/lib/handle-server-error', () => ({ handleServerError: vi.fn() }))

const originalMatchMedia = window.matchMedia
let client: QueryClient | undefined

function renderWithQueryClient(content: React.ReactNode): QueryClient {
  client = new QueryClient({
    defaultOptions: {
      queries: { retry: false },
      mutations: { retry: false },
    },
  })
  render(<QueryClientProvider client={client}>{content}</QueryClientProvider>)
  return client
}

function order(id: number, amountCents = 1234): InvoiceEligibleOrder {
  return {
    top_up_id: id,
    trade_no: `order-${id}`,
    amount_cents: amountCents,
    create_time: 1,
    complete_time: 2,
  }
}

afterEach(async () => {
  client?.clear()
  vi.clearAllMocks()
  await i18next.changeLanguage('en')
  i18next.removeResourceBundle('zhCN', 'translation')
  Object.defineProperty(window, 'matchMedia', {
    configurable: true,
    value: originalMatchMedia,
  })
})

describe('invoice components', () => {
  it('renders invoice dates with the project Chinese locale code', async () => {
    i18next.addResourceBundle(
      'zhCN',
      'translation',
      { Pending: '待处理' },
      true,
      true
    )
    await i18next.changeLanguage('zhCN')
    expect(i18next.resolvedLanguage).toBe('zhCN')
    const createdAt = 1_700_000_000
    apiMocks.getInvoiceRequests.mockResolvedValue({
      page: 1,
      page_size: 20,
      total: 1,
      items: [
        {
          id: 10,
          user_id: 1,
          username: 'user',
          display_name: 'User',
          source: 'user',
          status: 'pending',
          company_name: 'Example Co.',
          tax_id: 'TAX-001',
          email: 'billing@example.com',
          total_amount_cents: 1234,
          create_time: createdAt,
          processed_time: 0,
          operator_id: 0,
          operator_username: '',
          reject_reason: '',
          note: '',
          items: [],
        } satisfies InvoiceRequest,
      ],
    })

    renderWithQueryClient(<InvoiceRequestsTable />)

    expect(
      await screen.findByText(
        new Date(createdAt * 1000).toLocaleString('zh-CN')
      )
    ).toBeInTheDocument()
  })

  it('supports mobile selection and clears the selection when searching', async () => {
    Object.defineProperty(window, 'matchMedia', {
      configurable: true,
      value: (query: string): MediaQueryList => ({
        matches:
          query.includes('max-width: 640px') ||
          (query.includes('prefers-reduced-motion') &&
            !query.includes('no-preference')),
        media: query,
        onchange: null,
        addListener: () => undefined,
        removeListener: () => undefined,
        addEventListener: () => undefined,
        removeEventListener: () => undefined,
        dispatchEvent: () => false,
      }),
    })
    apiMocks.getEligibleInvoiceOrders.mockResolvedValue({
      page: 1,
      page_size: 20,
      total: 1,
      items: [order(1)],
    })
    const onAction = vi.fn()

    function Harness() {
      const [selected, setSelected] = useState(
        new Map<number, InvoiceEligibleOrder>()
      )
      return (
        <>
          <output data-testid='selection-size'>{selected.size}</output>
          <EligibleOrdersTable
            selectedOrders={selected}
            onSelectedOrdersChange={setSelected}
            actionLabel='Apply for invoice'
            actionDisabled={selected.size === 0}
            onAction={onAction}
          />
        </>
      )
    }

    renderWithQueryClient(<Harness />)
    const user = userEvent.setup()
    await screen.findByText('order-1')
    await user.click(screen.getByRole('checkbox', { name: 'Select row 1' }))
    expect(screen.getByTestId('selection-size')).toHaveTextContent('1')
    await user.click(screen.getByRole('button', { name: 'Apply for invoice' }))
    expect(onAction).toHaveBeenCalledOnce()

    await user.type(
      screen.getByPlaceholderText('Search by recharge order number...'),
      'different'
    )
    await waitFor(() =>
      expect(screen.getByTestId('selection-size')).toHaveTextContent('0')
    )
  })

  it('preserves enterprise fields and refreshes orders after a failed submission', async () => {
    apiMocks.createInvoice.mockRejectedValue(new Error('order conflict'))
    const selectedOrder = order(2)
    const onSelectionReset = vi.fn()
    const onOpenChange = vi.fn()
    const queryClient = renderWithQueryClient(
      <InvoiceApplicationDialog
        open
        onOpenChange={onOpenChange}
        selectedOrders={new Map([[selectedOrder.top_up_id, selectedOrder]])}
        totalAmountCents={selectedOrder.amount_cents}
        minAmountCents={0}
        onSelectionReset={onSelectionReset}
      />
    )
    const invalidate = vi.spyOn(queryClient, 'invalidateQueries')
    const user = userEvent.setup()

    await user.type(screen.getByLabelText('Company name'), 'Example Co.')
    await user.type(screen.getByLabelText('Enterprise tax ID'), 'TAX-001')
    await user.type(
      screen.getByLabelText('Invoice email'),
      'billing@example.com'
    )
    await user.click(screen.getByRole('button', { name: 'Submit application' }))

    await waitFor(() => expect(apiMocks.createInvoice).toHaveBeenCalledOnce())
    expect(screen.getByLabelText('Company name')).toHaveValue('Example Co.')
    expect(screen.getByLabelText('Enterprise tax ID')).toHaveValue('TAX-001')
    expect(screen.getByLabelText('Invoice email')).toHaveValue(
      'billing@example.com'
    )
    expect(onSelectionReset).toHaveBeenCalledOnce()
    expect(onOpenChange).not.toHaveBeenCalled()
    expect(invalidate).toHaveBeenCalledWith({
      queryKey: ['invoice-eligible-orders'],
    })
  })

  it('shows the rejection reason and processing audit data in user records', async () => {
    const rejected: InvoiceRequest = {
      id: 9,
      user_id: 1,
      username: 'user',
      display_name: 'User',
      source: 'user',
      status: 'rejected',
      company_name: 'Example Co.',
      tax_id: 'TAX-001',
      email: 'billing@example.com',
      total_amount_cents: 1234,
      create_time: 1,
      processed_time: 2,
      operator_id: 7,
      operator_username: 'reviewer',
      reject_reason: 'Company details do not match',
      note: '',
      items: [
        {
          id: 1,
          request_id: 9,
          top_up_id: 2,
          trade_no: 'order-2',
          amount_cents: 1234,
          create_time: 1,
          complete_time: 2,
        },
      ],
    }
    apiMocks.getInvoiceRequests.mockResolvedValue({
      page: 1,
      page_size: 20,
      total: 1,
      items: [rejected],
    })

    renderWithQueryClient(<InvoiceRequestsTable />)

    expect(
      await screen.findByText('Rejection reason: Company details do not match')
    ).toBeInTheDocument()
    expect(screen.getByText(/Processed by: reviewer/)).toBeInTheDocument()
  })
})
