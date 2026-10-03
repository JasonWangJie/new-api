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
import { flexRender, type Row } from '@tanstack/react-table'
import { useTranslation } from 'react-i18next'

import { CopyButton } from '@/components/copy-button'
import { Button } from '@/components/ui/button'
import { Checkbox } from '@/components/ui/checkbox'
import { Progress } from '@/components/ui/progress'
import { formatTimestampToDate, formatUseTime } from '@/lib/format'

import { imageLabel } from '../lib/image-labels'
import {
  imageTaskElapsedSeconds,
  imageTaskSpecifications,
} from '../lib/task-presentation'
import type { ImageTask } from '../types'
import {
  ImagePlatformBadge,
  ImageRequestTypeBadge,
  ImageStatusBadge,
} from './image-badges'

export function ImageTaskMobileCard(props: {
  row: Row<ImageTask>
  isSelected: boolean
  admin: boolean
  onView: (id: string) => void
  onPrefetch: (id: string) => void
}) {
  const { t } = useTranslation()
  const task = props.row.original
  const cells = props.row.getVisibleCells()
  const selectCell = cells.find((cell) => cell.column.id === 'select')
  const costCell = cells.find((cell) => cell.column.id === 'quota')
  const actionsCell = cells.find((cell) => cell.column.id === 'actions')
  const progress = Number.isFinite(task.progress)
    ? Math.min(100, Math.max(0, task.progress))
    : 0
  const active = [
    'queued',
    'invoking',
    'processing',
    'upstream_succeeded',
    'uploading',
    'billing_pending',
  ].includes(task.status)
  const submittedAt = formatTimestampToDate(task.created_at)

  return (
    <article aria-label={task.id} className='min-w-0 space-y-2.5'>
      <header className='space-y-1'>
        <div className='flex items-start justify-between gap-2'>
          <div className='min-w-0 flex-1 space-y-2'>
            <div className='flex min-w-0 flex-wrap items-center gap-1.5'>
              <ImagePlatformBadge
                platform={task.provider || task.platform}
                className='max-w-full wrap-anywhere whitespace-normal'
              />
              <ImageRequestTypeBadge
                requestType={task.request_type}
                className='h-auto max-w-full py-0.5 font-normal wrap-anywhere whitespace-normal'
              />
              <span className='text-muted-foreground font-mono text-[10px]'>
                {t(imageLabel(task.protocol))}
              </span>
            </div>
            <h3 className='text-sm leading-5 font-semibold wrap-anywhere'>
              {task.model || '—'}
            </h3>
          </div>
          {selectCell && (
            <label className='-mt-2 flex size-11 shrink-0 items-center justify-center'>
              <Checkbox
                className='size-5'
                aria-label={`${t('Select task')} ${task.id}`}
                checked={props.isSelected}
                onCheckedChange={(checked) => props.row.toggleSelected(checked)}
              />
            </label>
          )}
        </div>
        <div className='flex min-w-0 items-center gap-1'>
          <Button
            variant='link'
            className='text-muted-foreground h-9 min-w-0 flex-1 justify-start px-0 font-mono text-[11px]'
            title={task.id}
            onClick={() => props.onView(task.id)}
            onMouseEnter={() => props.onPrefetch(task.id)}
            onFocus={() => props.onPrefetch(task.id)}
          >
            <span className='truncate'>{task.id}</span>
          </Button>
          <CopyButton
            value={task.id}
            tooltip={t('Copy Task ID')}
            className='size-11'
            iconClassName='size-3.5'
          />
        </div>
      </header>

      <div className='bg-muted/40 space-y-2 rounded-lg p-2.5'>
        <div className='flex items-center justify-between gap-2'>
          <ImageStatusBadge
            status={task.status}
            errorCode={task.error_code}
            className='h-auto min-w-0 shrink py-0.5 wrap-anywhere whitespace-normal'
          />
          <span className='text-muted-foreground shrink-0 text-[11px] tabular-nums'>
            {t('Progress')} {progress}%
          </span>
        </div>
        {active && <Progress value={progress} aria-label={t('Progress')} />}
      </div>

      <dl className='[&_dt]:text-muted-foreground grid grid-cols-2 gap-x-4 gap-y-2.5 text-xs [&_dd]:mt-1 [&_dd]:leading-4 [&_dd]:font-medium [&_dd]:wrap-anywhere [&_dt]:text-[11px] [&>div]:min-w-0'>
        <div>
          <dt>{t('Resolution')}</dt>
          <dd>
            {imageTaskSpecifications(task)}
            {task.aspect_ratio && (
              <span className='text-muted-foreground block font-normal'>
                {task.aspect_ratio}
              </span>
            )}
          </dd>
        </div>
        <div>
          <dt>{t('Media / storage')}</dt>
          <dd className='tabular-nums'>
            {task.result_count} / {task.image_count}
            <span className='text-muted-foreground block font-normal'>
              {task.storage_providers?.join(', ') || t('Not stored')}
            </span>
          </dd>
        </div>
        <div>
          <dt>{t('Submitted at')}</dt>
          <dd className='tabular-nums'>
            {submittedAt.split(' ').map((part) => (
              <span key={part} className='block'>
                {part}
              </span>
            ))}
          </dd>
        </div>
        <div>
          <dt>{t('Time spent')}</dt>
          <dd className='tabular-nums'>
            {task.created_at
              ? formatUseTime(imageTaskElapsedSeconds(task) || 0)
              : '—'}
          </dd>
        </div>
        {props.admin && (
          <>
            <div>
              <dt>{t('User')}</dt>
              <dd>{task.user_name || t('User unavailable')}</dd>
            </div>
            <div>
              <dt>{t('Channel')}</dt>
              <dd>
                {task.channel_name ||
                  (task.channel_id
                    ? t('Channel unavailable')
                    : t('Not selected'))}
              </dd>
            </div>
          </>
        )}
      </dl>

      <footer className='flex flex-wrap items-center justify-between gap-2 border-t pt-3'>
        <dl className='min-w-28 flex-1 space-y-1'>
          <div className='flex items-baseline gap-2'>
            <dt className='text-muted-foreground text-[11px]'>
              {t('Actual cost')}
            </dt>
            <dd className='tabular-nums [&>span]:text-sm [&>span]:font-semibold'>
              {costCell &&
                flexRender(
                  costCell.column.columnDef.cell,
                  costCell.getContext()
                )}
            </dd>
          </div>
          <div className='text-muted-foreground text-[11px]'>
            <dt className='sr-only'>{t('Billing status')}</dt>
            <dd>{t(imageLabel(task.billing_status))}</dd>
          </div>
        </dl>
        <div className='ml-auto flex max-w-full justify-end [&_button]:min-h-11 [&_button]:min-w-11 [&_button]:text-xs [&>div]:justify-end [&>div]:gap-2'>
          {actionsCell &&
            flexRender(
              actionsCell.column.columnDef.cell,
              actionsCell.getContext()
            )}
        </div>
      </footer>
    </article>
  )
}
