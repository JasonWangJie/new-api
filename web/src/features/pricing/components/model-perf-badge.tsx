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
import { memo, useMemo } from 'react'
import { useTranslation } from 'react-i18next'

import { LoadingState } from '@/components/loading-state'
import {
  formatLatency,
  formatThroughput,
  formatUptimePct,
  getSuccessRateDotClass,
  getSuccessRateTextClass,
  isTaskPerformance,
} from '@/features/performance-metrics/lib/format'
import type {
  PerformanceStats,
  SuccessRatePoint,
} from '@/features/performance-metrics/types'
import { cn } from '@/lib/utils'

import {
  FilteredVideoSuccessRateHelp,
  PerformanceSampleContext,
} from './model-performance-summary'

export type ModelPerfBadgeData = PerformanceStats & {
  recent_success_series?: SuccessRatePoint[]
}

export interface ModelPerfBadgeProps extends React.HTMLAttributes<HTMLDivElement> {
  perf: ModelPerfBadgeData | undefined
  mediaModel?: boolean
  status?: 'loading' | 'error' | 'ready'
  videoErrorExclusions?: readonly string[]
}

const STATUS_SLOTS = Array.from({ length: 24 }, (_, slot) => slot)

export const ModelPerfBadge = memo(function ModelPerfBadge(
  props: ModelPerfBadgeProps
) {
  const { t } = useTranslation()
  const hasSamples = props.perf != null && props.perf.request_count !== 0
  const filtered = props.perf?.video_filtered_success_rate
  const filteredRate = filtered?.rate ?? Number.NaN
  const media =
    isTaskPerformance(props.perf) || props.mediaModel || filtered != null
  const deliveredMedia =
    props.perf?.workload === 'image' || props.perf?.workload === 'video'
  const latencyText = hasSamples
    ? formatLatency(props.perf?.avg_latency_ms ?? 0, props.perf?.latency_count)
    : '—'
  const throughputText = (
    hasSamples ? formatThroughput(props.perf?.avg_tps ?? 0) : '—'
  ).replace(' t/s', 't/s')
  const successRate = props.perf?.success_rate
  const hasSuccessRate =
    hasSamples &&
    successRate != null &&
    Number.isFinite(successRate) &&
    successRate >= 0 &&
    successRate <= 100
  // Hourly points with timestamps, anchored to the client's current hour.
  // Hours without traffic stay gray. Slot 23 is the current, partial hour.
  const statusRates = useMemo(() => {
    const currentHourStart = Math.floor(Date.now() / 1000 / 3600) * 3600
    const ratesByHour = new Map<number, number>()
    for (const point of props.perf?.recent_success_series ?? []) {
      ratesByHour.set(point.ts, point.success_rate)
    }
    return STATUS_SLOTS.map((slot) => {
      const hourStart = currentHourStart - (23 - slot) * 3600
      return ratesByHour.get(hourStart)
    })
  }, [props.perf?.recent_success_series])

  let sampleContext: React.ReactNode = null
  if (props.status === 'loading') {
    sampleContext = <LoadingState inline size='sm' message={t('Loading...')} />
  } else if (props.status === 'error') {
    sampleContext = (
      <span className='text-muted-foreground text-[11px]'>
        {t('Performance metrics unavailable')}
      </span>
    )
  } else if (hasSamples) {
    sampleContext = <PerformanceSampleContext stats={props.perf} />
  } else if (props.status === 'ready') {
    sampleContext = (
      <span className='text-muted-foreground text-[11px]'>
        {t('No samples in the last 24 hours')}
      </span>
    )
  }

  return (
    <div
      aria-label={t('Performance metrics for the last 24 hours')}
      className={cn('flex w-full min-w-0 flex-col gap-2', props.className)}
    >
      <div className='flex w-full min-w-0 flex-wrap items-center justify-between gap-3'>
        <dl className='flex min-w-0 flex-1 basis-60 flex-wrap items-start gap-x-3 gap-y-2 text-xs tabular-nums'>
          <div className='w-24 shrink-0'>
            <dt
              title={t('Request success rate sampled over the last 24 hours')}
              className='text-muted-foreground flex items-center justify-between gap-1 text-[11px] leading-4'
            >
              <span>{t('Status')}</span>
              <span className='font-mono'>
                {hasSuccessRate ? `${successRate.toFixed(1)}%` : '—%'}
              </span>
            </dt>
            <dd
              role='img'
              aria-label={t(
                'Recent success-rate samples; gray bars indicate missing data.'
              )}
              title={t(
                'Recent success-rate samples; gray bars indicate missing data.'
              )}
              className='mt-1 flex h-3 w-24 items-center gap-px'
            >
              {STATUS_SLOTS.map((slot) => {
                const rate = statusRates[slot]
                return (
                  <span
                    key={slot}
                    aria-hidden
                    className={cn(
                      'h-full w-[3px] shrink-0 rounded-xs',
                      rate != null &&
                        Number.isFinite(rate) &&
                        rate >= 0 &&
                        rate <= 100
                        ? getSuccessRateDotClass(rate)
                        : 'bg-muted-foreground/15'
                    )}
                  />
                )
              })}
            </dd>
          </div>
          {filtered && (
            <div className='max-w-full min-w-0'>
              <dt className='text-muted-foreground flex items-center gap-1 text-[11px] leading-4'>
                <span className='min-w-0 [overflow-wrap:anywhere]'>
                  {t('Filtered success rate')}
                </span>
                <FilteredVideoSuccessRateHelp
                  stats={filtered}
                  exclusions={props.videoErrorExclusions}
                />
              </dt>
              <dd
                className={cn(
                  'mt-1 font-mono whitespace-nowrap',
                  getSuccessRateTextClass(filteredRate)
                )}
              >
                {formatUptimePct(filteredRate)}
              </dd>
            </div>
          )}
          <div
            title={
              deliveredMedia ? t('Average delivery time') : t('Average latency')
            }
            className='shrink-0'
          >
            <dt className='text-muted-foreground text-[11px] leading-4'>
              {deliveredMedia ? t('Delivery time') : t('Latency short')}
            </dt>
            <dd className='mt-1 font-mono whitespace-nowrap'>
              {latencyText === '—' ? '—s' : latencyText}
            </dd>
          </div>
          {!media && (
            <div title={t('Throughput')} className='max-w-full shrink-0'>
              <dt className='text-muted-foreground text-[11px] leading-4'>
                {t('Throughput short')}
              </dt>
              <dd className='mt-1 font-mono whitespace-nowrap'>
                {throughputText === '—' ? '—t/s' : throughputText}
              </dd>
            </div>
          )}
        </dl>
        {props.children}
      </div>
      {sampleContext}
    </div>
  )
})
