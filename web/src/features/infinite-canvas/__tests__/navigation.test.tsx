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
import {
  createMemoryHistory,
  createRootRoute,
  createRoute,
  createRouter,
  Outlet,
  RouterProvider,
} from '@tanstack/react-router'
import { cleanup, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { NavGroup } from '@/components/layout/components/nav-group'
import { SidebarProvider } from '@/components/ui/sidebar'
import { SearchProvider, useSearch } from '@/context/search-provider'
import { ThemeProvider } from '@/context/theme-provider'
import { useSidebarData } from '@/hooks/use-sidebar-data'
import { useAuthStore } from '@/stores/auth-store'
import { useSystemConfigStore } from '@/stores/system-config-store'

// These cases use the root navigation, not an embedded settings workspace.
vi.mock('@/components/layout/lib/sidebar-view-registry', () => ({
  getNavGroupsForPath: () => null,
}))

const getAnimations = Object.getOwnPropertyDescriptor(
  Element.prototype,
  'getAnimations'
)

function SidebarEntries() {
  const groups = useSidebarData().navGroups
  const items = groups
    .flatMap((group) => group.items)
    .filter((item) => item.url === '/canvas' || item.url === '/playground')
  return <NavGroup title='Navigation' items={items} />
}

function OpenCommands() {
  const search = useSearch()
  return (
    <button type='button' onClick={() => search.setOpen(true)}>
      Open commands
    </button>
  )
}

async function renderNavigation(
  surface: 'sidebar' | 'commands',
  expanded = true
) {
  const root = createRootRoute({
    component: () => (
      <ThemeProvider defaultTheme='light'>
        {surface === 'sidebar' ? (
          <SidebarProvider defaultOpen={expanded}>
            <SidebarEntries />
          </SidebarProvider>
        ) : (
          <SearchProvider>
            <OpenCommands />
          </SearchProvider>
        )}
        <Outlet />
      </ThemeProvider>
    ),
  })
  const home = createRoute({
    getParentRoute: () => root,
    path: '/',
    component: () => <p>Original platform</p>,
  })
  const canvas = createRoute({
    getParentRoute: () => root,
    path: '/canvas',
    component: () => <p>Canvas destination</p>,
  })
  const playground = createRoute({
    getParentRoute: () => root,
    path: '/playground',
    component: () => <p>Playground destination</p>,
  })
  const router = createRouter({
    routeTree: root.addChildren([home, canvas, playground]),
    history: createMemoryHistory({ initialEntries: ['/'] }),
  })
  await router.load()
  render(<RouterProvider router={router} />)
  await screen.findByText('Original platform')
  return router
}

beforeEach(() => {
  // JSDOM omits the animation API used by the command menu's scroll area.
  Object.defineProperty(Element.prototype, 'getAnimations', {
    configurable: true,
    value: () => [],
  })
  localStorage.clear()
  useAuthStore.getState().auth.reset()
  useSystemConfigStore.getState().setConfig({
    infiniteCanvas: {
      enabled: true,
      url: 'https://canvas.example.com',
      client_id: 'infinite-canvas',
    },
  })
  vi.spyOn(window, 'open').mockImplementation(() => null)
  vi.spyOn(window, 'scrollTo').mockImplementation(() => undefined)
})

afterEach(() => {
  cleanup()
  if (getAnimations) {
    Object.defineProperty(Element.prototype, 'getAnimations', getAnimations)
  } else {
    Reflect.deleteProperty(Element.prototype, 'getAnimations')
  }
  useAuthStore.getState().auth.reset()
  useSystemConfigStore.getState().setConfig({
    infiniteCanvas: { enabled: false, url: '', client_id: '' },
  })
  document.documentElement.classList.remove('light', 'dark')
  localStorage.clear()
  vi.restoreAllMocks()
})

describe('canvas navigation keeps the original platform open', () => {
  it.each([true, false])(
    'opens the sidebar canvas entry in a separate tab when expanded=%s',
    async (expanded) => {
      const router = await renderNavigation('sidebar', expanded)
      const link = screen.getByRole('link', { name: /AI Infinite Canvas/ })
      expect(link).toHaveAttribute('href', '/canvas')
      expect(link).toHaveAttribute('target', '_blank')
      expect(link).toHaveAttribute('rel', 'noopener noreferrer')
      expect(link).toHaveAccessibleName(/Open in new tab/)

      await userEvent.setup().click(link)

      expect(router.state.location.pathname).toBe('/')
      expect(screen.getByText('Original platform')).toBeInTheDocument()
      expect(screen.queryByText('Canvas destination')).not.toBeInTheDocument()
    }
  )

  it('continues to navigate ordinary sidebar entries in the current tab', async () => {
    const router = await renderNavigation('sidebar')
    const link = screen.getByRole('link', { name: 'Playground' })
    expect(link).not.toHaveAttribute('target')

    await userEvent.setup().click(link)

    await waitFor(() =>
      expect(router.state.location.pathname).toBe('/playground')
    )
    expect(
      await screen.findByText('Playground destination')
    ).toBeInTheDocument()
    expect(window.open).not.toHaveBeenCalled()
  })

  it('opens the canvas command in a separate tab without replacing the current route', async () => {
    const router = await renderNavigation('commands')
    const user = userEvent.setup()
    await user.click(screen.getByRole('button', { name: 'Open commands' }))
    await user.click(
      await screen.findByRole('option', { name: /AI Infinite Canvas/ })
    )

    expect(window.open).toHaveBeenCalledExactlyOnceWith(
      '/canvas',
      '_blank',
      'noopener,noreferrer'
    )
    expect(router.state.location.pathname).toBe('/')
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    expect(screen.getByText('Original platform')).toBeInTheDocument()
  })

  it('continues to navigate ordinary commands in the current tab', async () => {
    const router = await renderNavigation('commands')
    const user = userEvent.setup()
    await user.click(screen.getByRole('button', { name: 'Open commands' }))
    await user.click(await screen.findByRole('option', { name: 'Playground' }))

    await waitFor(() =>
      expect(router.state.location.pathname).toBe('/playground')
    )
    expect(
      await screen.findByText('Playground destination')
    ).toBeInTheDocument()
    expect(window.open).not.toHaveBeenCalled()
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
  })
})
