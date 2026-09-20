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
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'

import { STATUS_QUERY_KEY } from '@/lib/status-query'

import { DocsHub } from '../components/docs-hub'

async function renderDocs(pagePath = 'token-api-config') {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  client.setQueryData(STATUS_QUERY_KEY, {
    system_name: 'Test API',
  })

  const root = createRootRoute()
  const docs = createRoute({
    getParentRoute: () => root,
    path: '/docs/$space/$',
    component: () => <DocsHub spaceSlug='quickstart' pagePath={pagePath} />,
  })
  const router = createRouter({
    routeTree: root.addChildren([docs]),
    history: createMemoryHistory({
      initialEntries: [`/docs/quickstart/${pagePath}`],
    }),
  })
  await router.load()
  render(
    <QueryClientProvider client={client}>
      <RouterProvider router={router} />
    </QueryClientProvider>
  )
}

describe('docs hub', () => {
  it('renders the documentation page title and rewritten base URL', async () => {
    await renderDocs('token-api-config')
    expect(
      screen.getByRole('heading', { level: 1, name: '令牌选择与 API 配置' })
    ).toBeInTheDocument()
    expect(
      screen.getAllByText('https://api.aiimg.lol/v1').length
    ).toBeGreaterThan(0)
    expect(screen.queryByText(/hejuapi\.com/)).not.toBeInTheDocument()
  })

  it('opens the mobile contents sheet and lists navigation pages', async () => {
    const user = userEvent.setup()
    await renderDocs('token-api-config')
    await user.click(screen.getByRole('button', { name: 'Contents' }))
    expect(
      screen.getAllByRole('link', { name: '令牌选择与 API 配置' }).length
    ).toBeGreaterThan(0)
  })
})
