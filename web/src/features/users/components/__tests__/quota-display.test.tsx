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
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import {
  createMemoryHistory,
  createRootRoute,
  createRoute,
  createRouter,
  RouterProvider,
} from '@tanstack/react-router'
import {
  flexRender,
  getCoreRowModel,
  useReactTable,
} from '@tanstack/react-table'
import {
  act,
  cleanup,
  render,
  screen,
  within,
  waitFor,
} from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { createInstance } from 'i18next'
import { I18nextProvider } from 'react-i18next'
import { afterEach, beforeEach, expect, it, vi } from 'vitest'

import { TooltipProvider } from '@/components/ui/tooltip'
import { BillingHistoryDialog } from '@/features/wallet/components/dialogs/billing-history-dialog'
import { formatCnyCents, parseCnyCents } from '@/features/wallet/lib/billing'
import zh from '@/i18n/locales/zh.json'
import { api } from '@/lib/api'
import { formatTimestampToDate } from '@/lib/format'
import { useAuthStore } from '@/stores/auth-store'
import {
  DEFAULT_CURRENCY_CONFIG,
  useSystemConfigStore,
} from '@/stores/system-config-store'

import type { User, ManageUserQuotaPayload } from '../../types'
import { UserQuotaDialog } from '../user-quota-dialog'
import { useUsersColumns } from '../users-columns'
import { UsersProvider } from '../users-provider'
import { UsersTable } from '../users-table'

const clients: QueryClient[] = []
const i18n = createInstance()
await i18n.init({
  lng: 'en',
  resources: { en: { translation: {} } },
  initAsync: false,
})

function QuotaTable(props: { remaining: number; used: number }) {
  const columns = useUsersColumns().filter((column) =>
    ['quota', 'used_quota'].includes(
      column.id ?? ('accessorKey' in column ? String(column.accessorKey) : '')
    )
  )
  const table = useReactTable({
    columns,
    data: [
      {
        id: 1,
        username: 'test',
        display_name: '',
        role: 1,
        status: 1,
        quota: props.remaining,
        used_quota: props.used,
        request_count: 0,
        group: 'default',
      } as User,
    ],
    getCoreRowModel: getCoreRowModel(),
  })
  return (
    <table>
      <thead>
        {table.getHeaderGroups().map((group) => (
          <tr key={group.id}>
            {group.headers.map((header) => (
              <th key={header.id} data-sortable={header.column.getCanSort()}>
                {flexRender(
                  header.column.columnDef.header,
                  header.getContext()
                )}
              </th>
            ))}
          </tr>
        ))}
      </thead>
      <tbody>
        {table.getRowModel().rows.map((row) => (
          <tr key={row.id}>
            {row.getVisibleCells().map((cell) => (
              <td key={cell.id}>
                {flexRender(cell.column.columnDef.cell, cell.getContext())}
              </td>
            ))}
          </tr>
        ))}
      </tbody>
    </table>
  )
}

beforeEach(() => {
  vi.spyOn(window, 'scrollTo').mockImplementation(() => {})
  useSystemConfigStore
    .getState()
    .setConfig({ currency: { ...DEFAULT_CURRENCY_CONFIG } })
})
afterEach(() => {
  cleanup()
  vi.unstubAllGlobals()
  clients.splice(0).forEach((client) => client.clear())
  useAuthStore.getState().auth.reset()
  useSystemConfigStore
    .getState()
    .setConfig({ currency: { ...DEFAULT_CURRENCY_CONFIG } })
})

it('shows balance above secondary usage text and opens quota details on click', async () => {
  render(
    <I18nextProvider i18n={i18n}>
      <QuotaTable remaining={1900} used={1100} />
    </I18nextProvider>
  )
  expect(
    screen.getByRole('columnheader', { name: 'Available Balance ($)' })
  ).toHaveAttribute('data-sortable', 'true')
  expect(screen.getAllByRole('columnheader')).toHaveLength(1)
  const cells = screen.getAllByRole('cell')
  expect(within(cells[0]).getByText('0.0038')).toBeInTheDocument()
  expect(cells).toHaveLength(1)
  expect(
    within(cells[0]).queryByText('Available Balance')
  ).not.toBeInTheDocument()
  expect(within(cells[0]).getByText('0.0022')).toBeInTheDocument()
  expect(screen.getByText('0.0038').parentElement).toHaveClass('grid-cols-1')
  expect(screen.getByText('Used amount').parentElement).toHaveAttribute(
    'data-table-text',
    'secondary'
  )
  expect(screen.queryByRole('progressbar')).not.toBeInTheDocument()
  expect(screen.queryByText('0.006')).not.toBeInTheDocument()
  const trigger = screen.getByRole('button', {
    name: 'Available Balance 0.0038; Used amount 0.0022',
  })
  await userEvent.click(trigger)
  const detail = await screen.findByRole('dialog', { name: 'Quota ($)' })
  expect(within(detail).getByText('Available Balance')).toBeInTheDocument()
  expect(within(detail).getByText('Total Used')).toBeInTheDocument()
  expect(within(detail).getByText('0.0038')).toBeInTheDocument()
  expect(within(detail).getByText('0.0022')).toBeInTheDocument()
  expect(within(detail).queryByRole('progressbar')).not.toBeInTheDocument()
  await userEvent.keyboard('{Escape}')
  await waitFor(() =>
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
  )
  expect(trigger).toHaveFocus()
})

it.each([0, 500000])(
  'shows usage for a zero balance only when used quota is nonzero (used=%s)',
  async (used) => {
    render(
      <I18nextProvider i18n={i18n}>
        <QuotaTable remaining={0} used={used} />
      </I18nextProvider>
    )
    if (used === 0) {
      expect(screen.getAllByRole('cell')[0]).toHaveTextContent(/^No Quota$/)
      expect(screen.queryByText('Used amount')).not.toBeInTheDocument()
      await userEvent.click(screen.getByRole('button', { name: 'No Quota' }))
      const detail = await screen.findByRole('dialog', { name: 'Quota ($)' })
      expect(within(detail).getAllByText('0')).toHaveLength(2)
      return
    }
    expect(screen.queryByText('No Quota')).not.toBeInTheDocument()
    expect(
      within(screen.getAllByRole('cell')[0]).getByText('0')
    ).toBeInTheDocument()
    expect(
      within(screen.getAllByRole('cell')[0]).getByText('1')
    ).toBeInTheDocument()
  }
)

it('preserves negative balances in details opened with the keyboard', async () => {
  render(
    <I18nextProvider i18n={i18n}>
      <QuotaTable remaining={-500000} used={1000000} />
    </I18nextProvider>
  )
  expect(screen.getByText('-1')).toHaveClass('text-destructive')
  expect(screen.getByText('2')).toBeInTheDocument()
  await userEvent.tab()
  await userEvent.keyboard('{Enter}')
  const detail = await screen.findByRole('dialog')
  expect(within(detail).getByText('-1')).toBeInTheDocument()
  expect(within(detail).getByText('2')).toBeInTheDocument()
})

it('shows the custom symbol only in the column header', () => {
  useSystemConfigStore.getState().setConfig({
    currency: {
      ...DEFAULT_CURRENCY_CONFIG,
      quotaDisplayType: 'CUSTOM',
      customCurrencySymbol: '🐱',
      customCurrencyExchangeRate: 1,
    },
  })
  render(
    <I18nextProvider i18n={i18n}>
      <QuotaTable remaining={1900} used={1100} />
    </I18nextProvider>
  )
  expect(
    screen.getByRole('columnheader', { name: 'Available Balance (🐱)' })
  ).toBeInTheDocument()
  for (const cell of screen.getAllByRole('cell')) {
    expect(cell).not.toHaveTextContent('🐱')
  }
  expect(
    within(screen.getAllByRole('cell')[0]).getByText('0.0038')
  ).toBeInTheDocument()
  expect(
    within(screen.getAllByRole('cell')[0]).getByText('0.0022')
  ).toBeInTheDocument()
})

function UsersPage() {
  return (
    <TooltipProvider>
      <UsersProvider>
        <UsersTable />
      </UsersProvider>
    </TooltipProvider>
  )
}

async function renderUsersList(emptyInvitation = false) {
  useAuthStore.getState().auth.setUser({ id: 1, username: 'admin', role: 100 })
  const get = vi.spyOn(api, 'get').mockResolvedValue({
    data: {
      success: true,
      data: {
        items: [
          {
            id: 2,
            username: 'long-user-name-for-table-layout',
            display_name: 'A display name',
            role: 1,
            status: 1,
            quota: 1900,
            used_quota: 1100,
            request_count: 0,
            created_at: Math.floor(Date.now() / 1000) - 86400,
            last_login_at: Math.floor(Date.now() / 1000) - 20,
            group: 'default',
            aff_count: emptyInvitation ? 0 : 2,
            aff_history_quota: emptyInvitation ? 0 : 500000,
            inviter_id: emptyInvitation ? 0 : 42,
          },
        ],
        total: 1,
      },
    },
  })
  const root = createRootRoute()
  const auth = createRoute({ getParentRoute: () => root, id: '_authenticated' })
  const users = createRoute({
    getParentRoute: () => auth,
    path: 'users/',
    component: UsersPage,
  })
  const router = createRouter({
    routeTree: root.addChildren([auth.addChildren([users])]),
    history: createMemoryHistory({ initialEntries: ['/users/'] }),
  })
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  clients.push(client)
  await router.load()
  render(
    <I18nextProvider i18n={i18n}>
      <QueryClientProvider client={client}>
        <RouterProvider router={router} />
      </QueryClientProvider>
    </I18nextProvider>
  )
  await screen.findByText('long-user-name-for-table-layout')
  return get
}

it('sends balance sorting to the server and keeps invitation details on two lines', async () => {
  const get = await renderUsersList()
  expect(
    screen.getByRole('columnheader', { name: 'User Group' })
  ).toBeInTheDocument()
  await userEvent.click(
    screen.getByRole('button', { name: 'Available Balance ($)' })
  )
  await userEvent.click(screen.getByRole('menuitem', { name: 'Desc' }))
  await waitFor(() =>
    expect(get).toHaveBeenCalledWith('/api/user/', {
      params: expect.objectContaining({ sort_by: 'quota', sort_order: 'desc' }),
    })
  )
  expect(
    screen.queryByRole('button', { name: 'Total Used' })
  ).not.toBeInTheDocument()
  expect(screen.getByText('Inviter ID: 42')).toBeInTheDocument()
  expect(screen.getByText(/Invited 2 users · Earnings:/)).toBeInTheDocument()
})

it('shows balance above usage on mobile cards in Chinese', async () => {
  const originalMatchMedia = window.matchMedia
  vi.spyOn(window, 'matchMedia').mockImplementation((query) => ({
    ...originalMatchMedia(query),
    matches: query.includes('max-width'),
  }))
  i18n.addResourceBundle('zh', 'translation', zh.translation)
  await i18n.changeLanguage('zh')
  try {
    await renderUsersList()
    expect(screen.getByText('可用余额 ($)')).toBeInTheDocument()
    expect(screen.getByText('已用')).toBeInTheDocument()
    expect(screen.getByText('0.0038')).toBeInTheDocument()
    expect(screen.getByText('0.0022')).toBeInTheDocument()
    expect(screen.queryByRole('table')).not.toBeInTheDocument()
    await userEvent.click(
      screen.getByRole('button', { name: /可用余额 0.0038/ })
    )
    const detail = await screen.findByRole('dialog', { name: '额度 ($)' })
    expect(within(detail).getByText('累计已用')).toBeInTheDocument()
    expect(within(detail).getByText('0.0022')).toBeInTheDocument()
    await userEvent.keyboard('{Escape}')
  } finally {
    await i18n.changeLanguage('en')
  }
})

it('combines creation and last login into one column with full dates visible directly', async () => {
  vi.spyOn(Date, 'now').mockReturnValue(Date.UTC(2026, 8, 8, 12))
  await renderUsersList(true)
  expect(screen.getByRole('columnheader', { name: /Time/ })).toBeInTheDocument()
  expect(
    screen.queryByRole('columnheader', { name: /Last Login/ })
  ).not.toBeInTheDocument()
  const row = screen.getByRole('row', {
    name: /long-user-name-for-table-layout/,
  })
  expect(within(row).getByText('—')).toBeInTheDocument()
  const times = within(row).getAllByRole('time')
  expect(times).toHaveLength(2)
  expect(times[0]).toHaveTextContent(
    formatTimestampToDate(Math.floor(Date.now() / 1000) - 86400)
  )
  expect(times[1]).toHaveTextContent(
    formatTimestampToDate(Math.floor(Date.now() / 1000) - 20)
  )
  expect(within(row).queryByText('Just now')).not.toBeInTheDocument()
  expect(within(row).getByText('Created')).toBeInTheDocument()
  expect(within(row).getByText('Last Login')).toBeInTheDocument()
  expect(screen.queryByText('No Inviter')).not.toBeInTheDocument()
  await userEvent.click(screen.getByRole('button', { name: 'View' }))
  await userEvent.click(screen.getByRole('menuitemcheckbox', { name: 'Time' }))
  expect(
    screen.queryByRole('columnheader', { name: /Time/ })
  ).not.toBeInTheDocument()
})

it('updates the header unit and converted amounts together when currency settings change', () => {
  render(
    <I18nextProvider i18n={i18n}>
      <QuotaTable remaining={500000} used={1000000} />
    </I18nextProvider>
  )
  act(() =>
    useSystemConfigStore.getState().setConfig({
      currency: {
        ...DEFAULT_CURRENCY_CONFIG,
        quotaDisplayType: 'CNY',
        usdExchangeRate: 7,
      },
    })
  )
  expect(
    screen.getByRole('columnheader', { name: 'Available Balance (¥)' })
  ).toBeInTheDocument()
  expect(
    within(screen.getAllByRole('cell')[0]).getByText('7')
  ).toBeInTheDocument()
  expect(
    within(screen.getAllByRole('cell')[0]).getByText('14')
  ).toBeInTheDocument()
  for (const cell of screen.getAllByRole('cell')) {
    expect(cell).not.toHaveTextContent('¥')
  }
})

function renderQuotaAdjustment() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  clients.push(client)
  const onSuccess = vi.fn()
  const onOpenChange = vi.fn()
  render(
    <QueryClientProvider client={client}>
      <I18nextProvider i18n={i18n}>
        <UserQuotaDialog
          open
          onOpenChange={onOpenChange}
          userId={2}
          currentQuota={1000}
          onSuccess={onSuccess}
        />
      </I18nextProvider>
    </QueryClientProvider>
  )
  return { onSuccess, onOpenChange }
}

it('confirms received RMB independently of fractional credited quota', async () => {
  const getRandomValues = crypto.getRandomValues.bind(crypto)
  vi.stubGlobal('crypto', { getRandomValues })
  const requests: ManageUserQuotaPayload[] = []
  vi.spyOn(api, 'post').mockImplementation(async (url, body) => {
    if (url === '/api/user/quota/preview') {
      return {
        data: {
          success: true,
          data: {
            before: 1000,
            after: 251000,
            delta_quota: '250000',
            suggested_payment_amount_cents: 730,
          },
        },
      } as never
    }
    requests.push(body as ManageUserQuotaPayload)
    return {
      data: { success: true, data: { order: { trade_no: 'MANUAL_paid' } } },
    } as never
  })
  const result = renderQuotaAdjustment()
  const user = userEvent.setup()
  await user.type(screen.getByRole('spinbutton', { name: /Amount/ }), '0.5')
  const cash = await screen.findByLabelText('Received amount (CNY)')
  expect(cash).toHaveValue('7.30')
  await user.clear(cash)
  await user.type(cash, '12.345')
  expect(screen.getByRole('button', { name: 'Confirm' })).toBeDisabled()
  await user.clear(cash)
  await user.type(cash, '12.34')
  await user.click(screen.getByRole('button', { name: 'Confirm' }))
  await waitFor(() => expect(result.onSuccess).toHaveBeenCalledOnce())
  expect(requests).toHaveLength(1)
  expect(requests[0]).toMatchObject({
    id: 2,
    action: 'add_quota',
    mode: 'add',
    value: 250000,
    payment_amount_cents: 1234,
    expected_delta_quota: '250000',
  })
  expect(requests[0].idempotency_key).toMatch(
    /^[\da-f]{8}(?:-[\da-f]{4}){3}-[\da-f]{12}$/
  )
})

it('preserves the exact confirmation request after an uncertain network result', async () => {
  const requests: ManageUserQuotaPayload[] = []
  vi.spyOn(api, 'post').mockImplementation(async (url, body) => {
    if (url === '/api/user/quota/preview') {
      return {
        data: {
          success: true,
          data: {
            before: 1000,
            after: 501000,
            delta_quota: '500000',
            suggested_payment_amount_cents: 730,
          },
        },
      } as never
    }
    requests.push(structuredClone(body as ManageUserQuotaPayload))
    if (requests.length === 1) throw new Error('network result unavailable')
    if (requests.length === 2) {
      return {
        data: { success: false, message: 'permission check unavailable' },
      } as never
    }
    return {
      data: {
        success: true,
        data: { replayed: true, order: { trade_no: 'MANUAL_original' } },
      },
    } as never
  })
  const result = renderQuotaAdjustment()
  const user = userEvent.setup()
  await user.type(screen.getByRole('spinbutton', { name: /Amount/ }), '1')
  await screen.findByLabelText('Received amount (CNY)')
  await user.click(screen.getByRole('button', { name: 'Confirm' }))
  const retry = await screen.findByRole('button', {
    name: 'Retry confirmation',
  })
  expect(screen.getByRole('button', { name: 'Cancel' })).toBeDisabled()
  expect(screen.getByLabelText('Received amount (CNY)')).toBeDisabled()
  await user.click(retry)
  await waitFor(() => expect(requests).toHaveLength(2))
  expect(screen.getByRole('button', { name: 'Cancel' })).toBeDisabled()
  await user.click(screen.getByRole('button', { name: 'Retry confirmation' }))
  await waitFor(() => expect(result.onSuccess).toHaveBeenCalledOnce())
  expect(requests).toHaveLength(3)
  expect(requests[1]).toEqual(requests[0])
  expect(requests[2]).toEqual(requests[0])
})

it('creates a gift order with explicit zero cents', async () => {
  const requests: ManageUserQuotaPayload[] = []
  vi.spyOn(api, 'post').mockImplementation(async (url, body) => {
    if (url === '/api/user/quota/preview') {
      return {
        data: {
          success: true,
          data: {
            before: 1000,
            after: 501000,
            delta_quota: '500000',
            suggested_payment_amount_cents: 730,
          },
        },
      } as never
    }
    requests.push(body as ManageUserQuotaPayload)
    return { data: { success: true } } as never
  })
  renderQuotaAdjustment()
  const user = userEvent.setup()
  await user.type(screen.getByRole('spinbutton', { name: /Amount/ }), '1')
  const cash = await screen.findByLabelText('Received amount (CNY)')
  await user.clear(cash)
  await user.type(cash, '0')
  expect(
    screen.getByText(
      'A zero payment creates a gift order that cannot be invoiced.'
    )
  ).toBeInTheDocument()
  await user.click(screen.getByRole('button', { name: 'Confirm' }))
  await waitFor(() => expect(requests).toHaveLength(1))
  expect(requests[0].payment_amount_cents).toBe(0)
})

it('refreshes a stale override and keeps the edited received amount', async () => {
  let conflict = false
  const requests: ManageUserQuotaPayload[] = []
  vi.spyOn(api, 'post').mockImplementation(async (url, body) => {
    if (url === '/api/user/quota/preview') {
      return {
        data: {
          success: true,
          data: {
            before: conflict ? 1100000 : 1000000,
            after: 1500000,
            delta_quota: conflict ? '400000' : '500000',
            suggested_payment_amount_cents: conflict ? 584 : 730,
          },
        },
      } as never
    }
    requests.push(body as ManageUserQuotaPayload)
    if (!conflict) {
      conflict = true
      throw {
        response: { status: 409, data: { code: 'MANUAL_QUOTA_CONFLICT' } },
      }
    }
    return { data: { success: true } } as never
  })
  renderQuotaAdjustment()
  const user = userEvent.setup()
  await user.click(screen.getByRole('button', { name: 'Override' }))
  await user.type(screen.getByRole('spinbutton', { name: /Amount/ }), '3')
  const cash = await screen.findByLabelText('Received amount (CNY)')
  await user.clear(cash)
  await user.type(cash, '12.34')
  await user.click(screen.getByRole('button', { name: 'Confirm' }))
  await waitFor(() =>
    expect(screen.getByText(/Current quota/)).toHaveTextContent('2.2')
  )
  await waitFor(() =>
    expect(screen.getByRole('button', { name: 'Confirm' })).toBeEnabled()
  )
  expect(cash).toHaveValue('12.34')
  await user.click(screen.getByRole('button', { name: 'Confirm' }))
  await waitFor(() => expect(requests).toHaveLength(2))
  expect(requests[1]).toMatchObject({
    expected_delta_quota: '400000',
    payment_amount_cents: 1234,
  })
  expect(requests[1].idempotency_key).not.toBe(requests[0].idempotency_key)
})

it('keeps decreasing overrides on the existing request contract', async () => {
  const requests: ManageUserQuotaPayload[] = []
  vi.spyOn(api, 'post').mockImplementation(async (url, body) => {
    if (url === '/api/user/quota/preview') {
      return {
        data: {
          success: true,
          data: {
            before: 1000,
            after: 0,
            delta_quota: '-1000',
            suggested_payment_amount_cents: 0,
          },
        },
      } as never
    }
    requests.push(body as ManageUserQuotaPayload)
    return { data: { success: true } } as never
  })
  renderQuotaAdjustment()
  const user = userEvent.setup()
  await user.click(screen.getByRole('button', { name: 'Override' }))
  await user.type(screen.getByRole('spinbutton', { name: /Amount/ }), '0')
  await waitFor(() =>
    expect(screen.getByRole('button', { name: 'Confirm' })).toBeEnabled()
  )
  expect(
    screen.queryByLabelText('Received amount (CNY)')
  ).not.toBeInTheDocument()
  await user.click(screen.getByRole('button', { name: 'Confirm' }))
  await waitFor(() => expect(requests).toHaveLength(1))
  expect(requests[0]).toEqual({
    id: 2,
    action: 'add_quota',
    mode: 'override',
    value: 0,
  })
})

it('round-trips confirmed cents without accepting excess precision or unsafe integers', () => {
  expect(parseCnyCents('12.34')).toBe(1234)
  expect(formatCnyCents(1234)).toBe('12.34')
  expect(parseCnyCents('0')).toBe(0)
  expect(parseCnyCents('12.345')).toBeNull()
  expect(parseCnyCents('-1')).toBeNull()
  expect(parseCnyCents('90071992547409.91')).toBe(Number.MAX_SAFE_INTEGER)
  expect(formatCnyCents(Number.MAX_SAFE_INTEGER)).toBe('90071992547409.91')
  expect(parseCnyCents('90071992547409.92')).toBeNull()
})

it('shows manual billing with exact credited amount and received RMB', async () => {
  vi.spyOn(api, 'get').mockResolvedValue({
    data: {
      success: true,
      data: {
        total: 1,
        items: [
          {
            id: 1,
            user_id: 2,
            amount: 250000,
            money: 12.34,
            trade_no: 'MANUAL_billing',
            payment_method: 'manual_backend',
            credited_quota: '250000',
            credited_amount: '0.5',
            payment_amount_cents: 1234,
            payment_currency: 'CNY',
            create_time: 1700000000,
            complete_time: 1700000000,
            status: 'success',
          },
        ],
      },
    },
  })
  render(
    <I18nextProvider i18n={i18n}>
      <BillingHistoryDialog open onOpenChange={vi.fn()} />
    </I18nextProvider>
  )
  expect(await screen.findByText('Manual backend')).toBeInTheDocument()
  expect(screen.getByText('0.5 USD')).toBeInTheDocument()
  expect(screen.getByText('¥12.34')).toBeInTheDocument()
  expect(screen.queryByText('manual_backend')).not.toBeInTheDocument()
})

it('labels raw quota mode as tokens without introducing a currency symbol', () => {
  useSystemConfigStore.getState().setConfig({
    currency: { ...DEFAULT_CURRENCY_CONFIG, quotaDisplayType: 'TOKENS' },
  })
  render(
    <I18nextProvider i18n={i18n}>
      <QuotaTable remaining={100} used={200} />
    </I18nextProvider>
  )
  expect(
    screen.getByRole('columnheader', { name: 'Available Balance (Tokens)' })
  ).toBeInTheDocument()
  expect(
    within(screen.getAllByRole('cell')[0]).getByText('100')
  ).toBeInTheDocument()
  expect(
    within(screen.getAllByRole('cell')[0]).getByText('200')
  ).toBeInTheDocument()
})
