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
import { ChevronDown } from 'lucide-react'
import { useState } from 'react'
import { useTranslation } from 'react-i18next'

import { StaticDataTable } from '@/components/data-table'
import { EmptyState } from '@/components/empty-state'
import { ErrorState } from '@/components/error-state'
import { LoadingState } from '@/components/loading-state'
import { Button } from '@/components/ui/button'
import {
  Collapsible,
  CollapsibleContent,
  CollapsibleTrigger,
} from '@/components/ui/collapsible'

import { getImageReliability } from '../api'
import type { ImageRecoveryRate } from '../types'

function recoveryRateLabel(rate: ImageRecoveryRate) {
  return `${rate.percent === null ? '—' : `${rate.percent.toFixed(1)}%`} (${rate.succeeded}/${rate.resolved})`
}

export function ImageReliabilityPanel(props: {
  userId: number
  filters: Record<string, string>
  autoRefresh: boolean
}) {
  const { t } = useTranslation()
  const [open, setOpen] = useState(false)
  const params = new URLSearchParams(
    Object.entries(props.filters).filter(([, value]) => !!value)
  )
  const report = useQuery({
    queryKey: ['image-reliability', props.userId, params.toString()],
    queryFn: ({ signal }) => getImageReliability(params, signal),
    enabled: open,
    staleTime: 30_000,
    refetchInterval: open && props.autoRefresh ? 30_000 : false,
  })
  const totals = report.data?.totals
  return (
    <Collapsible
      open={open}
      onOpenChange={setOpen}
      className='bg-card shrink-0 overflow-hidden rounded-lg border'
    >
      <CollapsibleTrigger
        render={
          <Button
            variant='ghost'
            className='w-full justify-start rounded-none px-4'
          />
        }
      >
        <ChevronDown className='size-4' aria-hidden />
        {t('Image retry recovery statistics')}
      </CollapsibleTrigger>
      <CollapsibleContent>
        <div
          role='region'
          aria-label={t('Image retry recovery statistics')}
          className='max-h-80 space-y-4 overflow-auto border-t p-4'
        >
          <p className='text-muted-foreground text-xs'>
            {t(
              'Uses all filtered image tasks, up to 31 days. Unknown execution, pending work and manual termination are excluded from resolved success rates.'
            )}
          </p>
          <p className='text-muted-foreground text-xs'>
            {t(
              'Recovery rates use complete new attempt histories and actual reference transport. These are observed recoveries, not a comparison experiment.'
            )}
          </p>
          {report.isPending && <LoadingState className='min-h-24' size='sm' />}
          {report.isError && (
            <ErrorState
              className='min-h-24'
              description={report.error.message}
              onRetry={() => void report.refetch()}
            />
          )}
          {!report.isError && totals && (
            <>
              <dl
                aria-label={t('Retry recovery rates')}
                className='grid gap-3 text-xs sm:grid-cols-2 xl:grid-cols-5'
              >
                {[
                  {
                    label: t('Resolved success rate'),
                    rate: totals.final_success,
                  },
                  {
                    label: t('First submission success'),
                    rate: totals.first_submission_success,
                  },
                  { label: t('Retry recovery'), rate: totals.retry_recovery },
                  {
                    label: t('Account switch recovery'),
                    rate: totals.account_switch_recovery,
                  },
                  {
                    label: t('Local reference recovery'),
                    rate: totals.local_fallback_recovery,
                  },
                ].map(({ label, rate }) => (
                  <div
                    key={label}
                    className='bg-muted/40 space-y-1 rounded-md border p-3'
                  >
                    <dt className='text-muted-foreground'>{label}</dt>
                    <dd className='font-semibold tabular-nums'>
                      {recoveryRateLabel(rate)}
                    </dd>
                  </div>
                ))}
              </dl>
              <p className='text-muted-foreground text-xs'>
                {t(
                  'Audited tasks: {{audited}} / {{total}} · Unknown: {{unknown}} · Pending: {{pending}} · Output processing: {{postprocess}} · Manually terminated: {{terminated}}',
                  {
                    audited: totals.audited_tasks,
                    total: totals.total,
                    unknown: totals.execution_unknown,
                    pending: totals.pending,
                    postprocess: totals.postprocess_pending,
                    terminated: totals.admin_terminated,
                  }
                )}
              </p>
              {report.data?.groups.length ? (
                <StaticDataTable
                  data={report.data.groups}
                  getRowKey={(row) =>
                    `${row.provider}:${row.model}:${row.channel_id}`
                  }
                  tableClassName='text-xs'
                  columns={[
                    {
                      id: 'provider',
                      header: t('Provider'),
                      cell: (row) => row.provider,
                    },
                    {
                      id: 'model',
                      header: t('Model'),
                      cell: (row) => row.model,
                    },
                    {
                      id: 'channel',
                      header: t('Channel'),
                      cell: (row) => row.channel_name || t('Not selected'),
                    },
                    {
                      id: 'total',
                      header: t('Total'),
                      cell: (row) => row.total,
                    },
                    {
                      id: 'final',
                      header: t('Resolved success rate'),
                      cell: (row) => recoveryRateLabel(row.final_success),
                    },
                    {
                      id: 'switch',
                      header: t('Account switch recovery'),
                      cell: (row) =>
                        recoveryRateLabel(row.account_switch_recovery),
                    },
                    {
                      id: 'fallback',
                      header: t('Local reference recovery'),
                      cell: (row) =>
                        recoveryRateLabel(row.local_fallback_recovery),
                    },
                  ]}
                />
              ) : (
                <EmptyState className='min-h-24' />
              )}
            </>
          )}
        </div>
      </CollapsibleContent>
    </Collapsible>
  )
}
