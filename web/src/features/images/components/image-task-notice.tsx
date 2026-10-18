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
import { useTranslation } from 'react-i18next'

import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert'
import { formatTimestampToDate } from '@/lib/format'
import { cn } from '@/lib/utils'

import { imageLabel } from '../lib/image-labels'
import { imageTaskIssue } from '../lib/task-presentation'
import type { ImageTask } from '../types'

export function ImageTaskNotice(props: {
  task: ImageTask
  diagnostics?: boolean
  className?: string
}) {
  const { t } = useTranslation()
  const issue = imageTaskIssue(props.task)
  if (!issue) return null
  const diagnostic =
    props.task.media_type === 'video' &&
    typeof props.task.error_detail === 'string'
      ? props.task.error_detail
      : props.task.error_message
  const retryCount =
    props.task.retry_decision?.total_used ?? props.task.retry_count
  return (
    <Alert
      role='status'
      variant={issue.pendingAction ? 'default' : 'destructive'}
      className={cn(
        'min-w-0 space-y-1 wrap-anywhere whitespace-normal',
        issue.pendingAction
          ? 'border-amber-500/30 bg-amber-500/5'
          : 'border-destructive/25 bg-destructive/5',
        props.className
      )}
    >
      <AlertTitle className='text-xs leading-5'>
        {t(imageLabel(issue.summary), issue.summaryParams || {})}
      </AlertTitle>
      <AlertDescription className='space-y-2 text-xs leading-5'>
        {diagnostic && (
          <div className='space-y-1'>
            {props.diagnostics && (
              <div className='font-medium'>{t('Failure reason')}</div>
            )}
            <div
              className={cn(
                'whitespace-pre-wrap',
                !props.diagnostics && 'line-clamp-2'
              )}
            >
              {diagnostic}
            </div>
          </div>
        )}
        {issue.suggestion && <div>{t(issue.suggestion)}</div>}
        <dl className='flex flex-wrap gap-x-3 gap-y-1 text-[11px] [&>div]:flex [&>div]:gap-1'>
          {issue.stage && (
            <div>
              <dt>{t('Task stage')}:</dt>
              <dd>{t(imageLabel(issue.stage))}</dd>
            </div>
          )}
          <div>
            <dt>{t('Billing status')}:</dt>
            <dd>{t(imageLabel(props.task.billing_status))}</dd>
          </div>
          {issue.pendingAction && (
            <>
              <div>
                <dt>{t('Total retries')}</dt>
                <dd>{retryCount}</dd>
              </div>
              {issue.nextAttemptAt > 0 && (
                <div>
                  <dt>{t('Next retry')}</dt>
                  <dd>{formatTimestampToDate(issue.nextAttemptAt)}</dd>
                </div>
              )}
            </>
          )}
        </dl>
      </AlertDescription>
    </Alert>
  )
}
