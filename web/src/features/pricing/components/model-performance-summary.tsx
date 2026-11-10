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
import type { PerformanceStats } from '@/features/performance-metrics/types'
import { toIntlLocale } from '@/i18n/languages'
import { cn } from '@/lib/utils'

import {
  filteredMediaRateLabel,
  getFilteredMediaKinds,
  type FilteredMediaKind,
} from '../lib/model-performance'

export function FilteredMediaSuccessRateHelp(props: {
  kind: FilteredMediaKind
  label: string
  exclusions?: readonly string[]
}) {
  const { t } = useTranslation()
  if (!props.exclusions?.length) return null

  return (
    <LearnMore
      triggerProps={{
        'aria-label':
          props.kind === 'image'
            ? t('Show excluded image errors')
            : t('Show excluded video errors'),
        className: 'shrink-0 rounded-full',
      }}
      contentProps={{
        className:
          'max-h-[min(24rem,60dvh)] w-80 max-w-[calc(100vw-1.5rem)] overflow-y-auto overscroll-contain p-3',
      }}
    >
      <PopoverTitle className='text-foreground'>{props.label}</PopoverTitle>
      <p className='text-xs'>
        {props.kind === 'image'
          ? t(
              'Successful image tasks divided by terminal image tasks after excluding matching failures. Unknown outcomes are included.'
            )
          : t(
              'Successful video tasks divided by terminal video tasks after excluding matching failures. Unknown outcomes are included.'
            )}
      </p>
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

export function PerformanceTimeContext(props: {
  windowHours: number
  updatedAt?: number
}) {
  const { t, i18n } = useTranslation()
  return (
    <div className='text-muted-foreground flex min-w-0 flex-wrap gap-x-3 gap-y-1 text-[11px]'>
      <span>{t('Last {{hours}} hours', { hours: props.windowHours })}</span>
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
  imageErrorExclusions?: readonly string[]
  videoErrorExclusions?: readonly string[]
}) {
  const { t } = useTranslation()
  const stats = props.stats
  const hasSamples = stats != null && stats.request_count !== 0
  const filteredKinds = getFilteredMediaKinds([stats])
  const media =
    isTaskPerformance(stats) || props.mediaModel || filteredKinds.length > 0
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
  for (const kind of filteredKinds) {
    const rate = stats?.[`${kind}_filtered_success_rate`]?.rate ?? Number.NaN
    const label = filteredMediaRateLabel(t, kind, filteredKinds.length > 1)
    metrics.push({
      icon: HeartPulse,
      label,
      value: formatUptimePct(rate),
      valueClassName: getSuccessRateTextClass(rate),
      help: (
        <FilteredMediaSuccessRateHelp
          kind={kind}
          label={label}
          exclusions={
            kind === 'image'
              ? props.imageErrorExclusions
              : props.videoErrorExclusions
          }
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
          metrics.length === 2 && 'sm:grid-cols-2',
          metrics.length === 3 && 'sm:grid-cols-3',
          metrics.length === 4 && 'sm:grid-cols-4',
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
      <PerformanceTimeContext
        windowHours={props.windowHours ?? 24}
        updatedAt={props.updatedAt}
      />
    </div>
  )
}
