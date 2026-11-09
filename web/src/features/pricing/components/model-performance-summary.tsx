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
import { HeartPulse, Timer } from 'lucide-react'
import { useTranslation } from 'react-i18next'

import { LearnMore } from '@/components/learn-more'
import { PopoverTitle } from '@/components/ui/popover'
import {
  formatLatency,
  formatThroughput,
  formatUptimePct,
  getSuccessRateTextClass,
  isTaskPerformance,
} from '@/features/performance-metrics/lib/format'
import type {
  FilteredVideoSuccessRate,
  PerformanceStats,
} from '@/features/performance-metrics/types'
import { toIntlLocale } from '@/i18n/languages'
import { cn } from '@/lib/utils'

export function FilteredVideoSuccessRateHelp(props: {
  stats: FilteredVideoSuccessRate
  exclusions?: readonly string[]
}) {
  const { t } = useTranslation()
  if (!props.exclusions?.length) return null

  return (
    <LearnMore
      triggerProps={{
        'aria-label': t('Show excluded video errors'),
        className: 'shrink-0 rounded-full',
      }}
      contentProps={{
        className:
          'max-h-[min(24rem,60dvh)] w-80 max-w-[calc(100vw-1.5rem)] overflow-y-auto overscroll-contain p-3',
      }}
    >
      <PopoverTitle className='text-foreground'>
        {t('Filtered success rate')}
      </PopoverTitle>
      <p className='text-xs'>
        {t(
          'Successful video tasks divided by terminal video tasks after excluding matching failures. Unknown outcomes are included.'
        )}
      </p>
      <dl className='grid grid-cols-[1fr_auto] gap-x-3 gap-y-1 text-xs tabular-nums'>
        <dt>{t('Samples')}</dt>
        <dd>{props.stats.sample_count}</dd>
        <dt>{t('Excluded failures')}</dt>
        <dd>{props.stats.excluded_count}</dd>
      </dl>
      <p className='text-foreground text-xs font-medium'>
        {t('Error phrases excluded from this rate')}
      </p>
      <ul className='list-disc space-y-1 ps-4 text-xs [overflow-wrap:anywhere]'>
        {props.exclusions.map((phrase) => (
          <li key={phrase}>{phrase}</li>
        ))}
      </ul>
    </LearnMore>
  )
}

export function PerformanceSampleContext(props: {
  stats?: PerformanceStats
  windowHours?: number
  updatedAt?: number
}) {
  const { t, i18n } = useTranslation()
  const count = props.stats?.request_count
  return (
    <div className='text-muted-foreground flex min-w-0 flex-wrap gap-x-3 gap-y-1 text-[11px]'>
      {props.windowHours != null && (
        <span>{t('Last {{hours}} hours', { hours: props.windowHours })}</span>
      )}
      {count != null && (
        <span>
          {count === 1
            ? t('{{count}} sample', { count })
            : t('{{count}} samples', { count })}
        </span>
      )}
      {count != null && count > 0 && count < 20 && (
        <span
          title={t('Fewer than 20 samples')}
          className='text-amber-600 dark:text-amber-400'
        >
          {t('Limited samples')}
        </span>
      )}
      {(props.stats?.unknown_count ?? 0) > 0 && (
        <span className='text-amber-600 dark:text-amber-400'>
          {props.stats?.unknown_count === 1
            ? t('{{count}} unknown result', { count: 1 })
            : t('{{count}} unknown', { count: props.stats?.unknown_count })}
        </span>
      )}
      {props.updatedAt != null && props.updatedAt > 0 && (
        <span>
          {t('Updated {{time}}', {
            time: new Date(props.updatedAt * 1000).toLocaleString(
              toIntlLocale(i18n.resolvedLanguage ?? i18n.language)
            ),
          })}
        </span>
      )}
    </div>
  )
}

export function ModelPerformanceSummary(props: {
  stats?: PerformanceStats
  compact?: boolean
  windowHours?: number
  updatedAt?: number
  mediaModel?: boolean
  videoErrorExclusions?: readonly string[]
}) {
  const { t } = useTranslation()
  const stats = props.stats
  const hasSamples = stats != null && stats.request_count !== 0
  const filtered = stats?.video_filtered_success_rate
  const media = isTaskPerformance(stats) || props.mediaModel || filtered != null
  const deliveredMedia =
    stats?.workload === 'image' || stats?.workload === 'video'
  const successRate = hasSamples ? stats.success_rate : Number.NaN
  const metrics: {
    icon: typeof Timer
    label: string
    value: string
    valueClassName?: string
    help?: React.ReactNode
  }[] = [
    {
      icon: Timer,
      label: deliveredMedia ? t('Average delivery time') : t('Average latency'),
      value: hasSamples
        ? formatLatency(stats.avg_latency_ms, stats.latency_count)
        : '—',
    },
    {
      icon: HeartPulse,
      label: t('Success rate'),
      value: formatUptimePct(successRate),
      valueClassName: getSuccessRateTextClass(successRate),
    },
  ]
  if (filtered) {
    const rate = filtered.rate ?? Number.NaN
    metrics.push({
      icon: HeartPulse,
      label: t('Filtered success rate'),
      value: formatUptimePct(rate),
      valueClassName: getSuccessRateTextClass(rate),
      help: (
        <FilteredVideoSuccessRateHelp
          stats={filtered}
          exclusions={props.videoErrorExclusions}
        />
      ),
    })
  }
  if (!media) {
    metrics.unshift({
      icon: Timer,
      label: 'TPS',
      value: hasSamples ? formatThroughput(stats.avg_tps) : '—',
    })
  }
  return (
    <div aria-label={t('Model performance summary')} className='space-y-2'>
      <dl
        className={cn(
          'grid overflow-hidden rounded-lg',
          media && !filtered ? 'sm:grid-cols-2' : 'sm:grid-cols-3',
          props.compact ? 'bg-muted/20 border sm:divide-x' : 'gap-2'
        )}
      >
        {metrics.map((metric) => (
          <div
            key={metric.label}
            className={cn(
              'min-w-0 px-3 py-2',
              !props.compact && 'bg-background rounded-lg border p-3'
            )}
          >
            <dt className='text-muted-foreground flex items-center gap-1.5 text-[10px] font-medium tracking-wider uppercase'>
              <metric.icon aria-hidden className='size-3 shrink-0' />
              <span className='min-w-0 [overflow-wrap:anywhere]'>
                {metric.label}
              </span>
              {metric.help}
            </dt>
            <dd
              className={cn(
                'text-foreground mt-1 break-words font-mono font-semibold tabular-nums',
                props.compact ? 'text-sm' : 'text-lg',
                metric.valueClassName
              )}
            >
              {metric.value}
            </dd>
          </div>
        ))}
      </dl>
      <PerformanceSampleContext
        stats={props.stats}
        windowHours={props.windowHours ?? 24}
        updatedAt={props.updatedAt}
      />
    </div>
  )
}
