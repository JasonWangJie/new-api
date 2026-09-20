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
import quickstartZh from '../data/quickstart.zh.json'

import type {
  DocsBlock,
  DocsBundle,
  DocsFlatPage,
  DocsNavNode,
  DocsPage,
} from '../types'

const bundles: Record<string, DocsBundle> = {
  quickstart: quickstartZh as DocsBundle,
}

export function getDocsBundle(spaceSlug: string): DocsBundle | null {
  return bundles[spaceSlug] ?? null
}

export function getDefaultDocsHref(): string {
  const bundle = bundles.quickstart
  return `/docs/${bundle.space.slug}/${bundle.defaultPath}`
}

export function listDocsSpaces(): DocsBundle['space'][] {
  return Object.values(bundles).map((bundle) => bundle.space)
}

export function resolveDocsPage(
  spaceSlug: string,
  pagePath: string
): DocsPage | null {
  const bundle = getDocsBundle(spaceSlug)
  if (!bundle) return null
  return bundle.pages[pagePath] ?? null
}

export function flattenDocsPages(nodes: DocsNavNode[]): DocsFlatPage[] {
  const pages: DocsFlatPage[] = []

  const visit = (items: DocsNavNode[]) => {
    for (const item of items) {
      if (item.type === 'page' && item.path) {
        pages.push({
          path: item.path,
          title: item.title,
          slug: item.slug,
        })
      }
      if (item.children?.length) {
        visit(item.children)
      }
    }
  }

  visit(nodes)
  return pages
}

export function findDocsNeighbors(
  spaceSlug: string,
  pagePath: string
): { previous: DocsFlatPage | null; next: DocsFlatPage | null } {
  const bundle = getDocsBundle(spaceSlug)
  if (!bundle) {
    return { previous: null, next: null }
  }

  const pages = flattenDocsPages(bundle.navigation)
  const index = pages.findIndex((page) => page.path === pagePath)
  if (index < 0) {
    return { previous: null, next: null }
  }

  return {
    previous: index > 0 ? pages[index - 1] : null,
    next: index < pages.length - 1 ? pages[index + 1] : null,
  }
}

export type DocsHeading = {
  id: string
  title: string
  level: number
}

function inlineText(block: DocsBlock): string {
  return (block.content ?? [])
    .map((node) => ('text' in node ? node.text : ''))
    .join('')
}

export function collectDocsHeadings(blocks: DocsBlock[]): DocsHeading[] {
  const headings: DocsHeading[] = []

  const visit = (items: DocsBlock[]) => {
    for (const block of items) {
      if (block.type === 'heading') {
        const level =
          typeof block.props?.level === 'number' ? block.props.level : 2
        const anchor =
          typeof block.props?.anchor === 'string' && block.props.anchor
            ? block.props.anchor
            : block.id
        headings.push({
          id: anchor,
          title: inlineText(block),
          level,
        })
      }
      if (block.children?.length) {
        visit(block.children)
      }
    }
  }

  visit(blocks)
  return headings
}

export function docsPageHref(spaceSlug: string, pagePath: string): string {
  return `/docs/${spaceSlug}/${pagePath}`
}

export function docsPageLinkParams(
  spaceSlug: string,
  pagePath: string
): { space: string; _splat: string } {
  return {
    space: spaceSlug,
    _splat: pagePath,
  }
}

export function parseInternalDocsHref(
  href: string
): { space: string; _splat: string } | null {
  const match = href.match(/^\/docs\/([^/]+)\/(.+)$/)
  if (!match) return null
  return {
    space: match[1],
    _splat: match[2],
  }
}

export function isExternalHref(href: string): boolean {
  return /^https?:\/\//i.test(href) || href.startsWith('mailto:')
}
