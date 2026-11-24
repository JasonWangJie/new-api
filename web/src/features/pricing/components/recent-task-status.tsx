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

import { getSuccessRateDotClass } from '@/features/performance-metrics/lib/format'
import type { TaskOutcome } from '@/features/performance-metrics/types'
import { cn } from '@/lib/utils'

const taskOutcomeRates: Record<TaskOutcome, number> = {
  success: 100,
  failure: 0,
  unknown: Number.NaN,
}
const recentTaskSlots = Array.from({ length: 20 }, (_, slot) => slot)

export function RecentTaskStatus(props: {
  outcomes: readonly TaskOutcome[]
  className?: string
}) {
  const { t } = useTranslation()
  const labels: Record<TaskOutcome, string> = {
    success: t('Success'),
    failure: t('Failed'),
    unknown: t('Unknown'),
  }

  if (props.outcomes.length === 0) {
    return (
      <span className='text-muted-foreground text-xs'>
        {t('No data available')}
      </span>
    )
  }

  return (
    <ol
      role='list'
      aria-label={t('Last 20 tasks')}
      className={cn('flex h-3.5 list-none gap-px', props.className)}
    >
      {recentTaskSlots.slice(0, props.outcomes.length).map((slot) => {
        const outcome = props.outcomes[slot]
        return (
          <li
            key={slot}
            title={labels[outcome]}
            className={cn(
              'h-full w-1 shrink-0 rounded-xs',
              getSuccessRateDotClass(taskOutcomeRates[outcome])
            )}
          >
            <span className='sr-only'>{labels[outcome]}</span>
          </li>
        )
      })}
    </ol>
  )
}
