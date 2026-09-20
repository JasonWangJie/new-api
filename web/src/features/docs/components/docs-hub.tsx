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
import { Link } from '@tanstack/react-router'
import { ArrowLeft, ArrowRight, Menu } from 'lucide-react'
import { useState } from 'react'
import { useTranslation } from 'react-i18next'

import { PublicLayout } from '@/components/layout'
import { Button } from '@/components/ui/button'
import {
  Sheet,
  SheetContent,
  SheetHeader,
  SheetTitle,
} from '@/components/ui/sheet'

import {
  collectDocsHeadings,
  docsPageLinkParams,
  findDocsNeighbors,
  getDocsBundle,
  resolveDocsPage,
} from '../lib/docs-catalog'
import { DocsBlocks } from './docs-blocks'
import { DocsSidebar } from './docs-sidebar'

type DocsHubProps = {
  spaceSlug: string
  pagePath: string
}

export function DocsHub(props: DocsHubProps) {
  const { t } = useTranslation()
  const [mobileOpen, setMobileOpen] = useState(false)
  const bundle = getDocsBundle(props.spaceSlug)
  const page = resolveDocsPage(props.spaceSlug, props.pagePath)

  if (!bundle || !page) {
    return (
      <PublicLayout>
        <div className='mx-auto flex min-h-[50vh] max-w-2xl flex-col items-center justify-center gap-3 text-center'>
          <h1 className='text-2xl font-semibold'>{t('Page not found')}</h1>
          <p className='text-muted-foreground'>
            {t('The requested documentation page does not exist.')}
          </p>
          <Button
            render={
              <Link
                to='/docs/$space/$'
                params={{
                  space: bundle?.space.slug ?? 'quickstart',
                  _splat: bundle?.defaultPath ?? 'tokenrouter',
                }}
              />
            }
          >
            {t('Back to documentation')}
          </Button>
        </div>
      </PublicLayout>
    )
  }

  const headings = collectDocsHeadings(page.content.blocks)
  const neighbors = findDocsNeighbors(props.spaceSlug, props.pagePath)

  return (
    <PublicLayout showMainContainer={false} className='docs-hub'>
      <div className='docs-hub-shell border-border/60 border-t pt-14'>
        <div className='docs-hub-mobile-bar border-border bg-background/95 supports-backdrop-filter:bg-background/80 sticky top-14 z-20 flex items-center gap-3 border-b px-4 py-3 backdrop-blur lg:hidden'>
          <Button
            type='button'
            variant='outline'
            size='sm'
            className='gap-2'
            onClick={() => setMobileOpen(true)}
            aria-expanded={mobileOpen}
          >
            <Menu className='size-4' aria-hidden='true' />
            {t('Contents')}
          </Button>
          <div className='docs-hub-mobile-bar__title truncate text-sm font-medium'>
            {page.title}
          </div>
        </div>

        <div className='docs-hub-canvas mx-auto grid max-w-[90rem] gap-0 lg:grid-cols-[17rem_minmax(0,1fr)] xl:grid-cols-[17rem_minmax(0,1fr)_14rem]'>
          <aside className='docs-hub-sidebar-wrap border-border hidden border-r lg:block'>
            <div className='sticky top-14 max-h-[calc(100vh-3.5rem)] overflow-y-auto p-4'>
              <div className='mb-3 text-sm font-semibold'>{bundle.space.title}</div>
              <DocsSidebar
                spaceSlug={props.spaceSlug}
                navigation={bundle.navigation}
                activePath={props.pagePath}
              />
            </div>
          </aside>

          <main className='docs-hub-main min-w-0 px-4 py-6 sm:px-6 lg:px-8 lg:py-8'>
            <nav
              aria-label={t('Breadcrumb')}
              className='docs-hub-breadcrumbs text-muted-foreground mb-4 flex flex-wrap items-center gap-2 text-sm'
            >
              <Link to='/docs' className='hover:text-foreground'>
                {t('Docs')}
              </Link>
              <span aria-hidden='true'>/</span>
              <span className='text-foreground'>{page.title}</span>
            </nav>

            <header className='docs-hub-page-header mb-6 space-y-2'>
              <h1 className='docs-hub-page-title text-3xl font-bold tracking-tight'>
                {page.title}
              </h1>
              {page.summary ? (
                <p className='docs-hub-page-summary text-muted-foreground text-base leading-7'>
                  {page.summary}
                </p>
              ) : null}
            </header>

            <DocsBlocks
              blocks={page.content.blocks}
              className='docs-page-content'
            />

            <div className='docs-hub-page-nav border-border mt-10 grid gap-3 border-t pt-6 sm:grid-cols-2'>
              {neighbors.previous ? (
                <Link
                  to='/docs/$space/$'
                  params={docsPageLinkParams(
                    props.spaceSlug,
                    neighbors.previous.path
                  )}
                  className='border-border hover:bg-muted/40 flex items-start gap-3 rounded-lg border p-4 transition-colors'
                >
                  <ArrowLeft className='text-muted-foreground mt-0.5 size-4 shrink-0' />
                  <span>
                    <span className='docs-hub-page-nav__dir text-muted-foreground block text-xs'>
                      {t('Previous')}
                    </span>
                    <span className='docs-hub-page-nav__title font-medium'>
                      {neighbors.previous.title}
                    </span>
                  </span>
                </Link>
              ) : (
                <div />
              )}
              {neighbors.next ? (
                <Link
                  to='/docs/$space/$'
                  params={docsPageLinkParams(
                    props.spaceSlug,
                    neighbors.next.path
                  )}
                  className='border-border hover:bg-muted/40 flex items-start justify-end gap-3 rounded-lg border p-4 text-right transition-colors'
                >
                  <span>
                    <span className='docs-hub-page-nav__dir text-muted-foreground block text-xs'>
                      {t('Next')}
                    </span>
                    <span className='docs-hub-page-nav__title font-medium'>
                      {neighbors.next.title}
                    </span>
                  </span>
                  <ArrowRight className='text-muted-foreground mt-0.5 size-4 shrink-0' />
                </Link>
              ) : null}
            </div>
          </main>

          <aside className='docs-hub-toc border-border hidden border-l xl:block'>
            <div className='sticky top-14 max-h-[calc(100vh-3.5rem)] overflow-y-auto p-4'>
              <div className='docs-hub-toc-label text-muted-foreground mb-3 text-xs font-semibold tracking-wide uppercase'>
                {t('On this page')}
              </div>
              <ul className='docs-hub-toc-list space-y-2'>
                {headings
                  .filter((heading) => heading.level <= 3)
                  .map((heading) => (
                    <li key={heading.id}>
                      <a
                        href={`#${heading.id}`}
                        className={cnTocLink(heading.level)}
                      >
                        {heading.title}
                      </a>
                    </li>
                  ))}
              </ul>
            </div>
          </aside>
        </div>
      </div>

      <Sheet open={mobileOpen} onOpenChange={setMobileOpen}>
        <SheetContent side='left' className='w-[min(100%,20rem)] p-0'>
          <SheetHeader className='border-border border-b px-4 py-3'>
            <SheetTitle>{bundle.space.title}</SheetTitle>
          </SheetHeader>
          <div className='overflow-y-auto p-4'>
            <DocsSidebar
              spaceSlug={props.spaceSlug}
              navigation={bundle.navigation}
              activePath={props.pagePath}
              onNavigate={() => setMobileOpen(false)}
            />
          </div>
        </SheetContent>
      </Sheet>
    </PublicLayout>
  )
}

function cnTocLink(level: number): string {
  if (level <= 2) {
    return 'docs-hub-toc-link text-muted-foreground hover:text-foreground block text-sm transition-colors'
  }
  return 'docs-hub-toc-link text-muted-foreground hover:text-foreground block pl-3 text-sm transition-colors'
}
