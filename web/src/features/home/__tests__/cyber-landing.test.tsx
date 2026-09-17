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
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { beforeEach, describe, expect, it, vi } from 'vitest'

import { STATUS_QUERY_KEY } from '@/lib/status-query'

import { CyberLanding } from '../components/cyber-landing'

async function renderLanding(
  isAuthenticated = false,
  docsLink = 'https://docs.example.com/guide'
) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  client.setQueryData(STATUS_QUERY_KEY, { docs_link: docsLink })
  const root = createRootRoute()
  const home = createRoute({
    getParentRoute: () => root,
    path: '/',
    component: () => <CyberLanding isAuthenticated={isAuthenticated} />,
  })
  const destination = createRoute({
    getParentRoute: () => root,
    path: '/sign-up',
    component: () => <p>Registration destination</p>,
  })
  const router = createRouter({
    routeTree: root.addChildren([home, destination]),
    history: createMemoryHistory({ initialEntries: ['/'] }),
  })
  await router.load()
  render(
    <QueryClientProvider client={client}>
      <RouterProvider router={router} />
    </QueryClientProvider>
  )
}

describe('cyber landing interactions', () => {
  beforeEach(() => {
    vi.spyOn(window, 'scrollTo').mockImplementation(() => {})
  })
  it('supports pausing and resuming the decorative motion with the keyboard', async () => {
    const user = userEvent.setup()
    await renderLanding()
    const pause = screen.getByRole('button', { name: 'Pause animations' })
    pause.focus()
    await user.keyboard('{Enter}')
    expect(screen.getByRole('main')).toHaveAttribute('data-motion', 'paused')
    const resume = screen.getByRole('button', { name: 'Resume animations' })
    expect(resume).toHaveAttribute('aria-pressed', 'true')
    expect(
      screen.getByRole('heading', {
        level: 1,
        name: 'Let imagination break new dimensions.',
      })
    ).toBeVisible()
    expect(
      screen.queryByText('Open source. Built for your stack.')
    ).not.toBeInTheDocument()
    for (const title of [
      'Connect a universe of models.',
      'Make imagination visible.',
      'Your rules. Your creative freedom.',
    ]) {
      const card = screen.getByRole('article', { name: title })
      expect(card).toBeVisible()
      expect(card.querySelector('p')).toBeVisible()
    }
    await user.keyboard('{Enter}')
    expect(screen.getByRole('main')).toHaveAttribute('data-motion', 'running')
    expect(
      screen.getByRole('button', { name: 'Pause animations' })
    ).toHaveAttribute('aria-pressed', 'false')
  })

  it('routes a visitor to registration from the main entry point', async () => {
    const user = userEvent.setup()
    await renderLanding()
    const links = screen.getAllByRole('link', { name: 'Get Started' })
    expect(links).toHaveLength(2)
    expect(links[0]).toHaveAttribute('href', '/sign-up')
    await user.click(links[0])
    expect(await screen.findByText('Registration destination')).toBeVisible()
  })

  it('offers the dashboard at both entry points for a signed-in user', async () => {
    await renderLanding(true)
    for (const link of screen.getAllByRole('link', {
      name: 'Go to Dashboard',
    })) {
      expect(link).toHaveAttribute('href', '/dashboard')
    }
    expect(
      screen.queryByRole('link', { name: 'Get Started' })
    ).not.toBeInTheDocument()
  })

  it('switches protocol examples manually and respects the configured documentation address', async () => {
    const user = userEvent.setup()
    await renderLanding()
    expect(
      screen.getByRole('link', { name: 'Read the documentation' })
    ).toHaveAttribute('href', 'https://docs.example.com/guide')
    expect(
      screen.getByRole('link', { name: 'Explore the API' })
    ).toHaveAttribute('href', '#protocols')
    expect(screen.getByText('Illustrative examples')).toBeVisible()
    await user.click(screen.getByRole('button', { name: 'Claude' }))
    await waitFor(() =>
      expect(screen.getByRole('button', { name: 'Claude' })).toHaveAttribute(
        'aria-pressed',
        'true'
      )
    )
    expect(screen.getByText('/v1/messages', { selector: 'code' })).toBeVisible()
    await user.click(screen.getByRole('button', { name: 'Gemini' }))
    await waitFor(() =>
      expect(
        screen.getByText('/v1beta/models/{model}:generateContent', {
          selector: 'code',
        })
      ).toBeVisible()
    )
  })
})
