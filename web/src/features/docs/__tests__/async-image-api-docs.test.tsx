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
  cleanup,
  fireEvent,
  render,
  screen,
  within,
} from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { AppHeader } from '@/components/layout/components/app-header'
import { PublicLayout } from '@/components/layout/components/public-layout'
import { RichContent } from '@/components/rich-content'
import { SidebarProvider } from '@/components/ui/sidebar'
import { STATUS_QUERY_KEY } from '@/lib/status-query'
import { Route as AsyncImageApiDocsRoute } from '@/routes/async-image-api-docs'
import { useAuthStore } from '@/stores/auth-store'
import { useSystemConfigStore } from '@/stores/system-config-store'

const clients: QueryClient[] = []

async function renderDocs(
  options: {
    initialPath?: '/' | '/async-image-api-docs'
    docsEnabled?: boolean
    appHeader?: boolean
  } = {}
) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  clients.push(client)
  client.setQueryData(STATUS_QUERY_KEY, {
    system_name: 'Test API',
    HeaderNavModules: JSON.stringify({ docs: options.docsEnabled ?? true }),
  })
  client.setQueryData(['notice'], { success: true, data: '' })

  const root = createRootRoute()
  const home = createRoute({
    getParentRoute: () => root,
    path: '/',
    component: () =>
      options.appHeader ? (
        <SidebarProvider>
          <AppHeader
            showSearch={false}
            showNotifications={false}
            showConfigDrawer={false}
            showProfileDropdown={false}
          />
        </SidebarProvider>
      ) : (
        <PublicLayout>Home</PublicLayout>
      ),
  })
  const docs = createRoute({
    getParentRoute: () => root,
    path: '/async-image-api-docs',
    component: AsyncImageApiDocsRoute.options.component,
  })
  const router = createRouter({
    routeTree: root.addChildren([home, docs]),
    history: createMemoryHistory({
      initialEntries: [options.initialPath ?? '/async-image-api-docs'],
    }),
  })
  await router.load()
  render(
    <QueryClientProvider client={client}>
      <RouterProvider router={router} />
    </QueryClientProvider>
  )
  return router
}

describe('public async image API documentation', () => {
  beforeEach(() => {
    localStorage.clear()
    useAuthStore.getState().auth.reset()
    useSystemConfigStore.getState().setLoading(false)
    vi.spyOn(window, 'scrollTo').mockImplementation(() => {})
    Object.defineProperty(window, 'scrollY', { configurable: true, value: 0 })
  })

  afterEach(() => {
    cleanup()
    for (const client of clients) client.clear()
    clients.length = 0
    useAuthStore.getState().auth.reset()
    useSystemConfigStore.getState().setLoading(true)
    Object.defineProperty(window, 'scrollY', { configurable: true, value: 0 })
    localStorage.clear()
  })

  it('renders the API documentation tables and examples when opened without a session', async () => {
    const router = await renderDocs()
    const article = screen.getByRole('article', {
      name: 'Async image API docs',
    })

    expect(
      within(article).getByRole('heading', {
        level: 1,
        name: 'new-api 异步图片与视频 API 接口文档',
      })
    ).toBeVisible()
    expect(
      within(article).getByRole('cell', {
        name: 'POST /v1/images/generations_async',
      })
    ).toBeInTheDocument()
    expect(article.querySelector('pre code')).toHaveTextContent(
      "export BASE_URL='https://api.your-domain.com'"
    )
    expect(router.state.location.pathname).toBe('/async-image-api-docs')
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
  })

  it('opens the document from the public desktop menu without a sign-in prompt', async () => {
    const user = userEvent.setup()
    const router = await renderDocs({ initialPath: '/' })
    const link = screen.getByTitle('Async image API docs')

    expect(link).toHaveAttribute('href', '/async-image-api-docs')
    await user.click(link)

    expect(
      await screen.findByRole('article', { name: 'Async image API docs' })
    ).toBeInTheDocument()
    expect(router.state.location.pathname).toBe('/async-image-api-docs')
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
  })

  it('opens the document from the public mobile menu and restores page scrolling', async () => {
    const user = userEvent.setup()
    const router = await renderDocs({ initialPath: '/' })
    const toggle = screen.getByRole('button', {
      name: 'Toggle navigation menu',
    })
    expect(toggle).toHaveAttribute('aria-expanded', 'false')
    expect(
      screen.getAllByRole('link', { name: 'Async image API docs' })
    ).toHaveLength(1)
    await user.click(toggle)
    expect(toggle).toHaveAttribute('aria-expanded', 'true')
    expect(document.body).toHaveStyle({ overflow: 'hidden' })

    const links = screen.getAllByRole('link', { name: 'Async image API docs' })
    const mobileLink = links.find((link) => !link.hasAttribute('title'))
    expect(mobileLink).toBeDefined()
    if (mobileLink) await user.click(mobileLink)

    expect(
      await screen.findByRole('article', { name: 'Async image API docs' })
    ).toBeInTheDocument()
    expect(router.state.location.pathname).toBe('/async-image-api-docs')
    expect(document.body.style.overflow).toBe('')
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
  })

  it('keeps desktop menu labels readable and header dimensions stable after scrolling', async () => {
    await renderDocs({ initialPath: '/' })
    const header = screen.getByRole('banner')
    const navigation = within(header).getByRole('navigation')
    const frame = navigation.parentElement
    const desktopLink = screen.getByTitle('Async image API docs')
    const desktopLinks = within(header)
      .getAllByRole('link')
      .filter((link) => link.hasAttribute('title'))

    expect(desktopLink.parentElement).toHaveClass('hidden', 'xl:flex')
    for (const link of desktopLinks) {
      expect(link).toHaveClass('shrink-0', 'whitespace-nowrap')
      expect(link).not.toHaveClass('truncate')
    }
    expect(frame).toHaveClass('w-full', 'max-w-[90rem]')
    expect(navigation).toHaveClass('h-16')

    Object.defineProperty(window, 'scrollY', { configurable: true, value: 100 })
    fireEvent.scroll(window)

    expect(navigation).toHaveClass('bg-background/90')
    expect(frame).toHaveClass('w-full', 'max-w-[90rem]')
    expect(navigation).toHaveClass('h-16')
  })

  it('keeps the app header mobile menu reachable and navigates to the document', async () => {
    const user = userEvent.setup()
    const router = await renderDocs({ initialPath: '/', appHeader: true })
    const toggle = screen.getByRole('button', {
      name: 'Toggle navigation menu',
    })

    // jsdom does not apply Tailwind media queries. Guard the actual regression:
    // a desktop-only wrapper must not hide the mobile dropdown trigger.
    expect(toggle.closest('.hidden')).toBeNull()
    expect(toggle).toHaveAttribute('aria-expanded', 'false')
    await user.click(toggle)
    expect(toggle).toHaveAttribute('aria-expanded', 'true')
    await user.click(
      await screen.findByRole('menuitem', { name: 'Async image API docs' })
    )

    expect(
      await screen.findByRole('article', { name: 'Async image API docs' })
    ).toBeInTheDocument()
    expect(router.state.location.pathname).toBe('/async-image-api-docs')
  })

  it('hides the menu when docs are disabled while keeping the anonymous direct link readable', async () => {
    await renderDocs({ docsEnabled: false })

    expect(
      screen.queryByRole('link', { name: 'Async image API docs' })
    ).not.toBeInTheDocument()
    expect(
      screen.getByRole('article', { name: 'Async image API docs' })
    ).toBeInTheDocument()
  })

  it('keeps chapter links while hiding the admin guide hint and external repository links', async () => {
    await renderDocs()
    const article = screen.getByRole('article', {
      name: 'Async image API docs',
    })

    expect(
      within(article).getByRole('heading', { name: '9. 旧版兼容入口' })
    ).toHaveAttribute('id', '9-旧版兼容入口')
    expect(
      within(article).getByRole('heading', { name: '6.3 结果链接' })
    ).toHaveAttribute('id', '63-结果链接')
    const chapter = within(article).getByRole('link', { name: '第 9 节' })
    expect(decodeURIComponent(chapter.getAttribute('href') ?? '')).toBe(
      '#9-旧版兼容入口'
    )
    expect(chapter).not.toHaveAttribute('target')

    expect(article).toHaveTextContent('new-api / QuantumNous')
    expect(article).not.toHaveTextContent(
      /github\.com|JasonWangJie\/new-api|wiki\//i
    )
    expect(article).not.toHaveTextContent('Drama 管理员操作见')
    expect(within(article).queryByText('后台照填手册')).not.toBeInTheDocument()
    for (const link of within(article).getAllByRole('link')) {
      expect(link.getAttribute('href')).toMatch(/^#/)
    }
  })

  it('preserves default Markdown links and gives repeated document headings distinct anchors', () => {
    const content =
      '# API `code`\n\n# API `code`\n\n[Chapter](#api-code)\n\n[Repository](https://github.com/example/project)\n\n[Unsafe](javascript:alert%281%29)'
    const view = render(<RichContent content={content} />)
    expect(screen.getByRole('link', { name: 'Chapter' })).toHaveAttribute(
      'target',
      '_blank'
    )
    expect(screen.getAllByRole('heading')[0]).not.toHaveAttribute('id')
    expect(screen.getByRole('link', { name: 'Repository' })).toHaveAttribute(
      'href',
      'https://github.com/example/project'
    )

    view.rerender(
      <RichContent content={content} documentNavigation fragmentLinksOnly />
    )

    expect(screen.getAllByRole('heading')[0]).toHaveAttribute('id', 'api-code')
    expect(screen.getAllByRole('heading')[1]).toHaveAttribute(
      'id',
      'api-code-1'
    )
    expect(screen.getByRole('link', { name: 'Chapter' })).toHaveAttribute(
      'href',
      '#api-code'
    )
    expect(screen.getByRole('link', { name: 'Chapter' })).not.toHaveAttribute(
      'target'
    )
    expect(
      screen.queryByRole('link', { name: 'Repository' })
    ).not.toBeInTheDocument()
    expect(screen.getByText('Repository')).toBeInTheDocument()
    expect(screen.getByText('Unsafe')).not.toHaveAttribute('href')
  })
})
