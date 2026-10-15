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
import { RefreshIcon } from '@hugeicons/core-free-icons'
import { HugeiconsIcon } from '@hugeicons/react'
import { useEffect } from 'react'
import { useTranslation } from 'react-i18next'

import { Button } from '@/components/ui/button'
import { useCountdown } from '@/hooks/use-countdown'
import { cn } from '@/lib/utils'

interface ImageTaskRefreshActionsProps {
  autoRefresh: boolean
  refreshIntervalSeconds: number
  tasksFetching: boolean
  tasksUpdatedAt: number
  platformsFetching: boolean
  onAutoRefreshChange: (enabled: boolean) => void
  onRefresh: () => void
}

export function ImageTaskRefreshActions(props: ImageTaskRefreshActionsProps) {
  const { t } = useTranslation()
  const { secondsLeft, start, stop } = useCountdown({
    initialSeconds: props.refreshIntervalSeconds,
  })
  const refreshing = props.tasksFetching || props.platformsFetching

  useEffect(() => {
    if (!props.autoRefresh || props.tasksFetching) {
      stop()
      return
    }
    start()
    return stop
  }, [
    props.autoRefresh,
    props.tasksFetching,
    props.tasksUpdatedAt,
    start,
    stop,
  ])

  return (
    <>
      <Button
        size='sm'
        variant={props.autoRefresh ? 'default' : 'outline'}
        aria-pressed={props.autoRefresh}
        onClick={() => props.onAutoRefreshChange(!props.autoRefresh)}
      >
        <HugeiconsIcon
          icon={RefreshIcon}
          strokeWidth={2}
          data-icon='inline-start'
          aria-hidden
        />
        {props.autoRefresh ? t('Auto refresh enabled') : t('Auto refresh')}
        {props.autoRefresh && (
          <span className='min-w-7 tabular-nums'>
            {props.tasksFetching
              ? t('Refreshing...')
              : t('{{seconds}}s', { seconds: secondsLeft })}
          </span>
        )}
      </Button>
      <Button
        variant='outline'
        size='sm'
        aria-busy={refreshing}
        disabled={refreshing}
        onClick={props.onRefresh}
      >
        <HugeiconsIcon
          icon={RefreshIcon}
          strokeWidth={2}
          data-icon='inline-start'
          className={cn(refreshing && 'animate-spin')}
          aria-hidden
        />
        {t('Refresh')}
      </Button>
    </>
  )
}
