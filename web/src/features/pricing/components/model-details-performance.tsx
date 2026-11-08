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
import { useQuery } from '@tanstack/react-query'
import { HeartPulse, Timer } from 'lucide-react'
import { useMemo } from 'react'
import { useTranslation } from 'react-i18next'

import {
  StaticDataTable,
  type StaticDataTableColumn,
  staticDataTableClassNames as tableStyles,
} from '@/components/data-table'
import { EmptyState } from '@/components/empty-state'
import { ErrorState } from '@/components/error-state'
import { GroupBadge } from '@/components/group-badge'
import { LoadingState } from '@/components/loading-state'
import { getPerfMetrics } from '@/features/performance-metrics/api'
import {
  formatLatency,
  formatPerformanceOutput,
  formatUptimePct,
  getPerformanceSummary,
  isTaskPerformance,
} from '@/features/performance-metrics/lib/format'
import type { PerformanceGroup } from '@/features/performance-metrics/types'
import { requireServerSuccess } from '@/lib/server-error-message'

import type { UptimeDayPoint } from '../lib/mock-stats'
import {
  getModelPerformanceStats,
  isMediaModel,
} from '../lib/model-performance'
import type { PricingModel } from '../types'
import { LatencyTrendChart, UptimeTrendChart } from './model-details-charts'
import { UptimeSparkline } from './model-details-uptime-sparkline'
import {
  ModelPerformanceSummary,
  PerformanceSampleContext,
} from './model-performance-summary'

function toLatencySeries(
  groups: PerformanceGroup[],
  field: 'avg_ttft_ms' | 'avg_latency_ms'
) {
  return groups
    .flatMap((group) =>
      group.series
        .filter(
          (point) =>
            point[field] > 0 ||
            (field === 'avg_latency_ms' &&
              point[field] === 0 &&
              (point.latency_count ?? 0) > 0)
        )
        .map((point) => ({
          timestamp: new Date(point.ts * 1000).toISOString(),
          group: group.group,
          ttft_ms: point[field],
          latency_count:
            field === 'avg_latency_ms' ? point.latency_count : undefined,
        }))
    )
    .sort((a, b) => a.timestamp.localeCompare(b.timestamp))
}

function toUptimeSeries(groups: PerformanceGroup[]): UptimeDayPoint[] {
  const byTs = new Map<
    number,
    { requests: number; successes: number; rate?: number; complete: boolean }
  >()
  for (const group of groups) {
    for (const point of group.series) {
      const current = byTs.get(point.ts) ?? {
        requests: 0,
        successes: 0,
        complete: true,
      }
      if (point.request_count != null && point.success_count != null) {
        current.requests += point.request_count
        current.successes += point.success_count
      } else if (groups.length === 1) {
        current.rate = point.success_rate
      } else {
        current.complete = false
      }
      byTs.set(point.ts, current)
    }
  }
  return [...byTs.entries()]
    .filter(
      ([, value]) =>
        value.complete && (value.requests > 0 || Number.isFinite(value.rate))
    )
    .sort(([a], [b]) => a - b)
    .map(([ts, value]) => {
      const rate =
        value.requests > 0
          ? (value.successes / value.requests) * 100
          : (value.rate ?? 0)
      return {
        date: new Date(ts * 1000).toISOString(),
        uptime_pct: rate,
        incidents: rate < 100 ? 1 : 0,
        outage_minutes: 0,
      }
    })
}

export function ModelDetailsPerformance(props: { model: PricingModel }) {
  const { t } = useTranslation()
  const metricsQuery = useQuery({
    queryKey: ['perf-metrics', props.model.model_name],
    queryFn: async () =>
      requireServerSuccess(await getPerfMetrics(props.model.model_name, 24)),
    staleTime: 60 * 1000,
  })
  const groups = useMemo(
    () =>
      metricsQuery.data?.data.groups.map(
        (group) => getModelPerformanceStats(props.model, group) ?? group
      ) ?? [],
    [metricsQuery.data, props.model]
  )
  const summary = getModelPerformanceStats(
    props.model,
    getPerformanceSummary(metricsQuery.data?.data)
  )
  const media = isTaskPerformance(summary) || groups.some(isTaskPerformance)
  const mixed =
    summary?.workload === 'mixed' ||
    groups.some((group) => group.workload === 'mixed')
  const latencyLabel =
    media && !mixed ? t('Average delivery time') : t('Average latency')
  const latencySeries = useMemo(
    () => toLatencySeries(groups, media ? 'avg_latency_ms' : 'avg_ttft_ms'),
    [groups, media]
  )
  const uptimeSeries = useMemo(() => toUptimeSeries(groups), [groups])

  if (metricsQuery.isError) {
    return (
      <ErrorState
        title={t('Performance metrics unavailable')}
        onRetry={() => void metricsQuery.refetch()}
        className='min-h-0 rounded-lg border p-6'
      />
    )
  }
  if (metricsQuery.isLoading) return <LoadingState className='min-h-0 p-6' />
  if (groups.length === 0) {
    return (
      <EmptyState
        title={t('Performance data is not yet available for this model.')}
        className='min-h-0 p-6'
        bordered
      />
    )
  }

  const columns: StaticDataTableColumn<PerformanceGroup>[] = [
    {
      id: 'group',
      header: t('Group'),
      className: tableStyles.compactHeaderCell,
      cellClassName: tableStyles.compactCell,
      cell: (perf) => <GroupBadge group={perf.group} size='sm' />,
    },
    {
      id: 'output',
      header: media ? t('Completed (24h)') : 'TPS',
      className: tableStyles.compactHeaderCellRight,
      cellClassName: tableStyles.compactNumericCell,
      cell: (perf) =>
        formatPerformanceOutput(
          mixed ? { ...perf, workload: 'mixed' } : perf,
          t
        ),
    },
    {
      id: 'latency',
      header: latencyLabel,
      className: tableStyles.compactHeaderCellRight,
      cellClassName: tableStyles.compactMutedNumericCell,
      cell: (perf) => formatLatency(perf.avg_latency_ms, perf.latency_count),
    },
    {
      id: 'success',
      header: t('Success rate'),
      className: tableStyles.compactHeaderCell,
      cellClassName: tableStyles.compactCell,
      cell: (perf) => (
        <div className='space-y-1'>
          <span>{formatUptimePct(perf.success_rate)}</span>
          <UptimeSparkline
            size='sm'
            showOverall={false}
            series={perf.series.map((point) => ({
              date: new Date(point.ts * 1000).toISOString(),
              uptime_pct: point.success_rate,
              incidents: point.success_rate < 100 ? 1 : 0,
              outage_minutes: 0,
            }))}
          />
        </div>
      ),
    },
    {
      id: 'samples',
      header: t('Samples'),
      className: tableStyles.compactHeaderCell,
      cellClassName: tableStyles.compactCell,
      cell: (perf) => <PerformanceSampleContext stats={perf} />,
    },
  ]
  if (!media) {
    columns.splice(2, 0, {
      id: 'ttft',
      header: t('Average TTFT'),
      className: tableStyles.compactHeaderCellRight,
      cellClassName: tableStyles.compactNumericCell,
      cell: (perf) => formatLatency(perf.avg_ttft_ms),
    })
  }

  return (
    <div className='flex flex-col gap-4'>
      <ModelPerformanceSummary
        stats={summary}
        mediaModel={isMediaModel(props.model)}
        windowHours={metricsQuery.data?.data.window_hours}
        updatedAt={metricsQuery.data?.data.updated_at}
      />
      <section>
        <SectionHeader icon={HeartPulse} title={t('Per-group performance')} />
        <StaticDataTable
          className='rounded-lg'
          tableClassName='text-sm'
          headerRowClassName={tableStyles.compactHeaderRow}
          data={groups}
          getRowKey={(perf) => perf.group}
          columns={columns}
        />
      </section>
      <section>
        <SectionHeader
          icon={Timer}
          title={t('Latency trend (last 24h)')}
          description={media ? latencyLabel : t('Average TTFT')}
        />
        <LatencyTrendChart
          series={latencySeries}
          metricLabel={media ? latencyLabel : t('Average TTFT')}
        />
      </section>
      <section>
        <SectionHeader
          icon={HeartPulse}
          title={t('Availability (last 24h)')}
          description={t('Request success rate sampled over the last 24 hours')}
        />
        <UptimeTrendChart series={uptimeSeries} />
      </section>
    </div>
  )
}

function SectionHeader(props: {
  icon: React.ComponentType<{ className?: string }>
  title: string
  description?: string
  accent?: React.ReactNode
}) {
  const Icon = props.icon
  return (
    <div className='mb-2 flex flex-wrap items-center justify-between gap-2'>
      <div className='flex min-w-0 items-center gap-2'>
        <Icon className='text-muted-foreground/70 size-3.5 shrink-0' />
        <div className='min-w-0'>
          <div className='text-foreground text-sm font-semibold'>
            {props.title}
          </div>
          {props.description && (
            <p className='text-muted-foreground/80 text-xs'>
              {props.description}
            </p>
          )}
        </div>
      </div>
      {props.accent && (
        <div className='shrink-0 text-xs font-medium'>{props.accent}</div>
      )}
    </div>
  )
}
