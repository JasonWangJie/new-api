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
import { createPortal } from 'react-dom'
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
  floating?: boolean
}

function ImageTaskRefreshButton(props: {
  refreshing: boolean
  onRefresh: () => void
  floating?: boolean
}) {
  const { t } = useTranslation()
  return (
    <Button
      type='button'
      variant={props.floating ? 'default' : 'outline'}
      size={props.floating ? 'icon' : 'sm'}
      aria-label={
        props.floating
          ? `${t('Refresh')} · ${t('Media task center')}`
          : undefined
      }
      title={t('Refresh')}
      aria-busy={props.refreshing}
      disabled={props.refreshing}
      onClick={props.onRefresh}
      className={cn(
        props.floating &&
          'fixed right-[calc(1rem+env(safe-area-inset-right))] bottom-[calc(5.5rem+env(safe-area-inset-bottom))] z-30 size-14 touch-manipulation rounded-full shadow-lg ring-4 ring-background/80 hover:bg-primary/90 active:scale-95 disabled:pointer-events-auto motion-reduce:transform-none motion-reduce:transition-none'
      )}
    >
      <HugeiconsIcon
        icon={RefreshIcon}
        strokeWidth={2}
        data-icon={props.floating ? undefined : 'inline-start'}
        className={cn(
          props.floating && 'size-6',
          props.refreshing && 'animate-spin motion-reduce:animate-none'
        )}
        aria-hidden
      />
      <span className={props.floating ? 'sr-only' : undefined}>
        {t('Refresh')}
      </span>
    </Button>
  )
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
      <ImageTaskRefreshButton
        refreshing={refreshing}
        onRefresh={props.onRefresh}
      />
      {props.floating &&
        typeof document !== 'undefined' &&
        createPortal(
          <ImageTaskRefreshButton
            floating
            refreshing={refreshing}
            onRefresh={props.onRefresh}
          />,
          document.body
        )}
    </>
  )
}
