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
import { cleanup, render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, describe, expect, it } from 'vitest'

import { AuthLayout } from '@/features/auth/auth-layout'
import { useSystemConfigStore } from '@/stores/system-config-store'

async function renderAuthLayout(children: React.ReactNode = <p>Auth body</p>) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  const root = createRootRoute()
  const auth = createRoute({
    getParentRoute: () => root,
    path: '/sign-in',
    component: () => <AuthLayout>{children}</AuthLayout>,
  })
  const home = createRoute({
    getParentRoute: () => root,
    path: '/',
    component: () => <p>Home destination</p>,
  })
  const router = createRouter({
    routeTree: root.addChildren([auth, home]),
    history: createMemoryHistory({ initialEntries: ['/sign-in'] }),
  })
  await router.load()
  render(
    <QueryClientProvider client={client}>
      <RouterProvider router={router} />
    </QueryClientProvider>
  )
  return router
}

describe('auth cyber layout', () => {
  beforeEach(() => {
    useSystemConfigStore.setState({
      ...useSystemConfigStore.getInitialState(),
      loading: false,
      config: {
        ...useSystemConfigStore.getInitialState().config,
        systemName: 'New API',
        logo: '/logo.png',
      },
    })
  })

  afterEach(() => {
    cleanup()
    useSystemConfigStore.setState(useSystemConfigStore.getInitialState(), true)
  })

  it('renders the cyber shell with brand, panel body, and motion controls', async () => {
    await renderAuthLayout(<p>Panel content</p>)

    const shell = document.querySelector('.auth-cyber')
    expect(shell).not.toBeNull()
    expect(shell).toHaveClass('dark')
    expect(shell).toHaveAttribute('data-motion', 'running')

    const brand = screen.getByRole('link', { name: /New API/i })
    expect(brand).toHaveAttribute('href', '/')
    expect(screen.getByRole('img', { name: 'Logo' })).toBeVisible()
    expect(screen.getByText('Identity gateway')).toBeVisible()
    expect(screen.getByText('Panel content')).toBeVisible()
    expect(document.querySelector('.auth-cyber-panel')).not.toBeNull()
    expect(document.querySelector('.auth-cyber-backdrop')).not.toBeNull()

    const pause = screen.getByRole('button', { name: 'Pause animations' })
    expect(pause).toHaveAttribute('aria-pressed', 'false')
  })

  it('pauses decorative motion when the toggle is activated', async () => {
    const user = userEvent.setup()
    await renderAuthLayout()

    const pause = screen.getByRole('button', { name: 'Pause animations' })
    await user.click(pause)

    const shell = document.querySelector('.auth-cyber')
    expect(shell).toHaveAttribute('data-motion', 'paused')
    expect(
      screen.getByRole('button', { name: 'Resume animations' })
    ).toHaveAttribute('aria-pressed', 'true')
  })

  it('keeps the auth panel vertically centered in a full viewport shell', async () => {
    await renderAuthLayout()

    const shell = document.querySelector('.auth-cyber-shell')
    const main = document.querySelector('.auth-cyber-main')
    expect(shell).toHaveClass('auth-cyber-shell')
    expect(main).toHaveClass('auth-cyber-main')
  })
})
