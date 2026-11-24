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

import {
  formatLatency,
  formatThroughput,
  formatUptimePct,
  getDisplayedSuccessRate,
  getSuccessRateTextClass,
  isTaskPerformance,
} from '@/features/performance-metrics/lib/format'
import type { PerformanceStats } from '@/features/performance-metrics/types'
import { toIntlLocale } from '@/i18n/languages'
import { cn } from '@/lib/utils'

import { RecentTaskStatus } from './recent-task-status'

export function PerformanceTimeContext(props: {
  windowHours: number
  updatedAt?: number
  showWindow?: boolean
}) {
  const { t, i18n } = useTranslation()
  const updatedAt = props.updatedAt
  const hasUpdateTime = updatedAt != null && updatedAt > 0
  if (props.showWindow === false && !hasUpdateTime) return null
  return (
    <div className='text-muted-foreground flex min-w-0 flex-wrap gap-x-3 gap-y-1 text-[11px]'>
      {props.showWindow !== false && (
        <span>{t('Last {{hours}} hours', { hours: props.windowHours })}</span>
      )}
      {hasUpdateTime && (
        <span>
          {t('Updated {{time}}', {
            time: new Date(updatedAt * 1000).toLocaleString(
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
}) {
  const { t } = useTranslation()
  const stats = props.stats
  const hasSamples = stats != null && stats.request_count !== 0
  const outcomes = stats?.recent_task_outcomes
  const media = isTaskPerformance(stats) || props.mediaModel || outcomes != null
  const deliveredMedia =
    stats?.workload === 'image' ||
    stats?.workload === 'video' ||
    (outcomes != null && !hasSamples)
  const successRate = getDisplayedSuccessRate(stats)
  const metrics: {
    icon: typeof Timer
    label: string
    value: string
    valueClassName?: string
    sampleLabel?: string
    sampleStatus?: React.ReactNode
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
      sampleLabel: outcomes ? t('Last 20 tasks') : undefined,
      sampleStatus: props.compact && outcomes && (
        <RecentTaskStatus outcomes={outcomes} />
      ),
    },
  ]
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
            </dt>
            <dd
              className={cn(
                'text-foreground mt-1 break-words font-mono font-semibold tabular-nums',
                props.compact ? 'text-sm' : 'text-lg',
                metric.valueClassName
              )}
            >
              <span title={metric.sampleLabel}>{metric.value}</span>
            </dd>
            {metric.sampleStatus && (
              <dd className='mt-1'>{metric.sampleStatus}</dd>
            )}
          </div>
        ))}
      </dl>
      <PerformanceTimeContext
        windowHours={props.windowHours ?? 24}
        updatedAt={props.updatedAt}
        showWindow={outcomes == null}
      />
    </div>
  )
}
