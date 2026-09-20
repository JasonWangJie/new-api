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
import {
  AlertTriangle,
  ChevronRight,
  Info,
  Lightbulb,
} from 'lucide-react'
import { Fragment, type ReactNode } from 'react'
import { useTranslation } from 'react-i18next'

import { CopyButton } from '@/components/copy-button'
import { cn } from '@/lib/utils'

import {
  isExternalHref,
  parseInternalDocsHref,
} from '../lib/docs-catalog'
import type { DocsBlock } from '../types'
import { DocsInline } from './docs-inline'

type DocsBlocksProps = {
  blocks: DocsBlock[]
  className?: string
}

function asString(value: unknown): string {
  return typeof value === 'string' ? value : ''
}

function asNumber(value: unknown, fallback: number): number {
  return typeof value === 'number' ? value : fallback
}

function CalloutIcon(props: { variant: string }) {
  if (props.variant === 'warning' || props.variant === 'danger') {
    return <AlertTriangle className='size-4 shrink-0' aria-hidden='true' />
  }
  if (props.variant === 'tip' || props.variant === 'success') {
    return <Lightbulb className='size-4 shrink-0' aria-hidden='true' />
  }
  return <Info className='size-4 shrink-0' aria-hidden='true' />
}

function CodeGroup(props: { block: DocsBlock }) {
  const { t } = useTranslation()
  const tabs = Array.isArray(props.block.props?.tabs)
    ? (props.block.props.tabs as Array<{
        code?: string
        label?: string
        language?: string
        filename?: string
      }>)
    : []
  const tab = tabs[0]
  const code = tab?.code ?? ''
  const label = tab?.label || tab?.filename || tab?.language || 'Plain'

  return (
    <div className='docs-code-group border-border bg-muted/40 overflow-hidden rounded-lg border'>
      <div className='border-border flex items-center justify-between gap-3 border-b px-3 py-2'>
        <span className='text-muted-foreground truncate text-xs font-medium'>
          {label}
        </span>
        <CopyButton
          value={code}
          size='sm'
          className='size-7'
          aria-label={t('Copy to clipboard')}
        />
      </div>
      <pre className='overflow-x-auto p-3 text-sm leading-relaxed'>
        <code>{code}</code>
      </pre>
    </div>
  )
}

function calloutClassName(variant: string): string {
  if (variant === 'warning' || variant === 'danger') {
    return 'border-amber-500/30 bg-amber-500/10'
  }
  if (variant === 'tip' || variant === 'success') {
    return 'border-emerald-500/30 bg-emerald-500/10'
  }
  return 'border-sky-500/30 bg-sky-500/10'
}

function DocsTable(props: { block: DocsBlock }) {
  const rows = Array.isArray(props.block.props?.rows)
    ? (props.block.props.rows as Array<{
        cells: Array<{ text?: string }>
      }>)
    : []
  const headerRows = asNumber(props.block.props?.headerRows, 0)
  if (rows.length === 0) return null

  return (
    <div className='border-border overflow-x-auto rounded-lg border'>
      <table className='w-full min-w-[28rem] border-collapse text-sm'>
        <tbody>
          {rows.map((row, rowIndex) => {
            const isHeader = rowIndex < headerRows
            const CellTag = isHeader ? 'th' : 'td'
            const rowKey = `${props.block.id}:${row.cells
              .map((cell) => cell.text ?? '')
              .join('\u0001')}`
            return (
              <tr
                key={rowKey}
                className={cn(
                  'border-border border-b last:border-b-0',
                  isHeader && 'bg-muted/50'
                )}
              >
                {row.cells.map((cell) => (
                  <CellTag
                    key={`${rowKey}:${cell.text ?? ''}`}
                    className={cn(
                      'px-3 py-2 text-left align-top',
                      isHeader && 'font-semibold'
                    )}
                  >
                    {cell.text ?? ''}
                  </CellTag>
                ))}
              </tr>
            )
          })}
        </tbody>
      </table>
    </div>
  )
}

function renderSingleBlock(block: DocsBlock): ReactNode {
  switch (block.type) {
    case 'heading': {
      const level = asNumber(block.props?.level, 2)
      const anchor = asString(block.props?.anchor) || block.id
      const className = cn(
        'docs-hub-anchor-target scroll-mt-24 font-semibold tracking-tight',
        level <= 1 && 'text-3xl',
        level === 2 && 'mt-8 text-2xl',
        level === 3 && 'mt-6 text-xl',
        level >= 4 && 'mt-4 text-lg'
      )
      const children = <DocsInline content={block.content} />
      if (level <= 1) {
        return (
          <h1 id={anchor} className={className}>
            {children}
          </h1>
        )
      }
      if (level === 2) {
        return (
          <h2 id={anchor} className={className}>
            {children}
          </h2>
        )
      }
      if (level === 3) {
        return (
          <h3 id={anchor} className={className}>
            {children}
          </h3>
        )
      }
      return (
        <h4 id={anchor} className={className}>
          {children}
        </h4>
      )
    }
    case 'paragraph':
      return (
        <p className='text-muted-foreground leading-7'>
          <DocsInline content={block.content} />
        </p>
      )
    case 'quote':
      return (
        <blockquote className='border-border text-muted-foreground border-l-2 pl-4 italic'>
          <DocsInline content={block.content} />
        </blockquote>
      )
    case 'callout': {
      const variant = asString(block.props?.variant) || 'info'
      const title = asString(block.props?.title)
      return (
        <aside
          className={cn(
            'docs-callout rounded-lg border px-4 py-3',
            calloutClassName(variant)
          )}
        >
          <div className='flex items-start gap-3'>
            <CalloutIcon variant={variant} />
            <div className='min-w-0 space-y-1'>
              {title ? (
                <div className='text-sm font-semibold'>{title}</div>
              ) : null}
              <div className='text-sm leading-6'>
                <DocsInline content={block.content} />
              </div>
            </div>
          </div>
        </aside>
      )
    }
    case 'codeGroup':
      return <CodeGroup block={block} />
    case 'table':
      return <DocsTable block={block} />
    case 'steps':
      return (
        <div className='space-y-4'>
          <DocsBlocks blocks={block.children ?? []} />
        </div>
      )
    case 'step': {
      const title = asString(block.props?.title)
      return (
        <section className='border-border rounded-lg border p-4'>
          {title ? <h3 className='mb-3 text-base font-semibold'>{title}</h3> : null}
          <div className='space-y-3'>
            <DocsBlocks blocks={block.children ?? []} />
          </div>
        </section>
      )
    }
    case 'toggle': {
      const title =
        asString(block.props?.title) ||
        (block.content ?? [])
          .map((node) => ('text' in node ? node.text : ''))
          .join('')
      const defaultOpen = Boolean(block.props?.defaultOpen)
      return (
        <details
          open={defaultOpen || undefined}
          className='border-border group rounded-lg border px-3 py-1'
        >
          <summary className='cursor-pointer list-none py-2 text-sm font-medium marker:content-none [&::-webkit-details-marker]:hidden'>
            {title}
          </summary>
          <div className='space-y-3 pb-3'>
            <DocsBlocks blocks={block.children ?? []} />
          </div>
        </details>
      )
    }
    case 'docLink': {
      const href = asString(block.props?.href)
      const title = asString(block.props?.title) || href
      if (!href) return null
      const className =
        'docs-doc-link border-border hover:bg-muted/50 flex items-center justify-between gap-3 rounded-lg border px-4 py-3 transition-colors'
      const body = (
        <>
          <span className='font-medium'>{title}</span>
          <ChevronRight className='text-muted-foreground size-4 shrink-0' />
        </>
      )
      if (isExternalHref(href)) {
        return (
          <a
            href={href}
            target='_blank'
            rel='noopener noreferrer'
            className={className}
          >
            {body}
          </a>
        )
      }
      const docsParams = parseInternalDocsHref(href)
      if (docsParams) {
        return (
          <Link
            to='/docs/$space/$'
            params={docsParams}
            className={className}
          >
            {body}
          </Link>
        )
      }
      return (
        <Link to={href} className={className}>
          {body}
        </Link>
      )
    }
    case 'modelCard': {
      const model = asString(block.props?.model)
      const provider = asString(block.props?.provider)
      const capability = asString(block.props?.capability)
      return (
        <div className='border-border rounded-lg border p-4'>
          <div className='text-base font-semibold'>{model}</div>
          {provider ? (
            <div className='text-muted-foreground mt-1 text-sm'>{provider}</div>
          ) : null}
          {capability ? (
            <div className='text-muted-foreground mt-2 text-sm'>{capability}</div>
          ) : null}
        </div>
      )
    }
    case 'numberedListItem':
    case 'bulletListItem':
      return (
        <li className='text-muted-foreground leading-7'>
          <DocsInline content={block.content} />
          {block.children?.length ? (
            <div className='mt-2 space-y-2'>
              <DocsBlocks blocks={block.children} />
            </div>
          ) : null}
        </li>
      )
    default:
      if (block.children?.length) {
        return <DocsBlocks blocks={block.children} />
      }
      if (block.content?.length) {
        return (
          <p className='text-muted-foreground leading-7'>
            <DocsInline content={block.content} />
          </p>
        )
      }
      return null
  }
}

export function DocsBlocks(props: DocsBlocksProps) {
  const nodes: ReactNode[] = []
  let index = 0

  while (index < props.blocks.length) {
    const block = props.blocks[index]
    if (
      block.type === 'numberedListItem' ||
      block.type === 'bulletListItem'
    ) {
      const listType = block.type
      const items: DocsBlock[] = []
      while (
        index < props.blocks.length &&
        props.blocks[index].type === listType
      ) {
        items.push(props.blocks[index])
        index += 1
      }
      const ListTag = listType === 'numberedListItem' ? 'ol' : 'ul'
      nodes.push(
        <ListTag
          key={`${listType}-${items[0]?.id ?? index}`}
          className={cn(
            'text-muted-foreground space-y-2 pl-5',
            listType === 'numberedListItem' ? 'list-decimal' : 'list-disc'
          )}
        >
          {items.map((item) => (
            <Fragment key={item.id}>{renderSingleBlock(item)}</Fragment>
          ))}
        </ListTag>
      )
      continue
    }

    nodes.push(
      <Fragment key={block.id}>{renderSingleBlock(block)}</Fragment>
    )
    index += 1
  }

  return <div className={cn('space-y-4', props.className)}>{nodes}</div>
}
