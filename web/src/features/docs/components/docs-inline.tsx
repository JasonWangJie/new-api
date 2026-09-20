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

import { cn } from '@/lib/utils'

import { isExternalHref, parseInternalDocsHref } from '../lib/docs-catalog'
import type { DocsInlineNode, DocsTextStyles } from '../types'

function styleClassName(styles?: DocsTextStyles): string {
  return cn(
    styles?.bold && 'font-semibold',
    styles?.italic && 'italic',
    styles?.strike && 'line-through',
    styles?.code &&
      'bg-muted rounded px-1 py-0.5 font-mono text-[0.85em] font-normal'
  )
}

export function DocsInline(props: {
  content?: DocsInlineNode[]
  className?: string
}) {
  if (!props.content?.length) return null

  return (
    <span className={props.className}>
      {props.content.map((node, index) => {
        const key = `${node.type}-${index}-${'text' in node ? node.text.slice(0, 12) : index}`
        if (node.type === 'link') {
          const className = cn(
            'text-primary underline-offset-4 hover:underline',
            styleClassName(node.styles)
          )
          if (isExternalHref(node.href)) {
            return (
              <a
                key={key}
                href={node.href}
                target='_blank'
                rel='noopener noreferrer'
                className={className}
              >
                {node.text}
              </a>
            )
          }
          const docsParams = parseInternalDocsHref(node.href)
          if (docsParams) {
            return (
              <Link
                key={key}
                to='/docs/$space/$'
                params={docsParams}
                className={className}
              >
                {node.text}
              </Link>
            )
          }
          return (
            <Link key={key} to={node.href} className={className}>
              {node.text}
            </Link>
          )
        }

        return (
          <span key={key} className={styleClassName(node.styles)}>
            {node.text}
          </span>
        )
      })}
    </span>
  )
}
