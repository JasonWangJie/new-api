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
import { useTranslation } from 'react-i18next'

import { cn } from '@/lib/utils'

import { docsPageLinkParams } from '../lib/docs-catalog'
import type { DocsNavNode } from '../types'

type DocsSidebarProps = {
  spaceSlug: string
  navigation: DocsNavNode[]
  activePath: string
  onNavigate?: () => void
  className?: string
}

function DocsNavPages(props: {
  spaceSlug: string
  nodes: DocsNavNode[]
  activePath: string
  depth?: number
  onNavigate?: () => void
}) {
  const depth = props.depth ?? 0

  return (
    <ul className={cn('space-y-1', depth > 0 && 'mt-1 ml-3 border-l pl-3')}>
      {props.nodes.map((node) => {
        if (node.type === 'group') {
          return (
            <li key={`group-${node.id}`}>
              <div className='text-muted-foreground px-2 py-1.5 text-xs font-semibold tracking-wide uppercase'>
                {node.title}
              </div>
              {node.children?.length ? (
                <DocsNavPages
                  spaceSlug={props.spaceSlug}
                  nodes={node.children}
                  activePath={props.activePath}
                  depth={depth + 1}
                  onNavigate={props.onNavigate}
                />
              ) : null}
            </li>
          )
        }

        if (!node.path) return null
        const isActive = props.activePath === node.path

        return (
          <li key={`page-${node.id}`}>
            <Link
              to='/docs/$space/$'
              params={docsPageLinkParams(props.spaceSlug, node.path)}
              onClick={props.onNavigate}
              className={cn(
                'block rounded-md px-2 py-1.5 text-sm transition-colors',
                isActive
                  ? 'bg-primary/10 text-primary font-medium'
                  : 'text-muted-foreground hover:bg-muted hover:text-foreground'
              )}
              aria-current={isActive ? 'page' : undefined}
            >
              {node.title}
            </Link>
            {node.children?.length ? (
              <DocsNavPages
                spaceSlug={props.spaceSlug}
                nodes={node.children}
                activePath={props.activePath}
                depth={depth + 1}
                onNavigate={props.onNavigate}
              />
            ) : null}
          </li>
        )
      })}
    </ul>
  )
}

export function DocsSidebar(props: DocsSidebarProps) {
  const { t } = useTranslation()

  return (
    <nav
      aria-label={t('Documentation navigation')}
      className={cn('docs-hub-sidebar space-y-4', props.className)}
    >
      <DocsNavPages
        spaceSlug={props.spaceSlug}
        nodes={props.navigation}
        activePath={props.activePath}
        onNavigate={props.onNavigate}
      />
    </nav>
  )
}
