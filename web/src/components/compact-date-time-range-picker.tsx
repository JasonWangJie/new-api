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
import { CalendarDays } from 'lucide-react'
import { useMemo, useState } from 'react'
import { useTranslation } from 'react-i18next'

import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import {
  Popover,
  PopoverContent,
  PopoverTrigger,
} from '@/components/ui/popover'
import dayjs from '@/lib/dayjs'
import { cn } from '@/lib/utils'

interface CompactDateTimeRangePickerProps {
  id?: string
  start?: Date
  end?: Date
  precision?: 'day' | 'minute'
  onChange: (range: { start?: Date; end?: Date }) => void
  className?: string
}

function toInputValue(
  date: Date | undefined,
  precision: 'day' | 'minute'
): string {
  return date
    ? dayjs(date).format(
        precision === 'day' ? 'YYYY-MM-DD' : 'YYYY-MM-DDTHH:mm'
      )
    : ''
}

function fromInputValue(value: string): Date | undefined {
  if (!value) return undefined
  // Date-only values must retain the user's local day instead of parsing as UTC.
  const date = dayjs(value).toDate()
  return Number.isNaN(date.getTime()) ? undefined : date
}

export function CompactDateTimeRangePicker({
  id,
  start,
  end,
  precision = 'minute',
  onChange,
  className,
}: CompactDateTimeRangePickerProps) {
  const { t } = useTranslation()
  const [open, setOpen] = useState(false)
  const [draftStart, setDraftStart] = useState(toInputValue(start, precision))
  const [draftEnd, setDraftEnd] = useState(toInputValue(end, precision))
  const dateOnly = precision === 'day'
  const startLabel = dateOnly ? t('Start date') : t('Start Time')
  const endLabel = dateOnly ? t('End date') : t('End Time')

  const label = useMemo(() => {
    if (!start && !end) return t('Date Range')
    // The popover's <input type="datetime-local"> only supports minute
    // precision, so seconds are always 00 (manual pick) or 59 (preset
    // end-of-day). Hide them in the trigger label to keep the button
    // width compact while still showing the meaningful timestamp.
    const format = dateOnly ? 'YYYY-MM-DD' : 'YYYY-MM-DD HH:mm'
    const startText = start ? dayjs(start).format(format) : '-'
    const endText = end ? dayjs(end).format(format) : '-'
    if (dateOnly && start && end && startText === endText) return startText
    return `${startText} ~ ${endText}`
  }, [dateOnly, end, start, t])

  const mobileLabel = useMemo(() => {
    if (dateOnly || !start || !end) return label
    if (dayjs(start).isSame(end, 'day')) {
      return `${dayjs(start).format('MM/DD HH:mm')}–${dayjs(end).format('HH:mm')}`
    }
    return label
  }, [dateOnly, start, end, label])

  const handleOpenChange = (nextOpen: boolean) => {
    if (nextOpen) {
      setDraftStart(toInputValue(start, precision))
      setDraftEnd(toInputValue(end, precision))
    }
    setOpen(nextOpen)
  }

  const applyDraft = () => {
    onChange({
      start: fromInputValue(draftStart),
      end: fromInputValue(draftEnd),
    })
    setOpen(false)
  }

  const applyPreset = (kind: 'today' | '7d' | 'week' | '30d' | 'month') => {
    const now = dayjs()
    const presets = {
      today: {
        start: now.startOf('day').toDate(),
        end: now.endOf('day').toDate(),
      },
      '7d': {
        start: now.subtract(6, 'day').startOf('day').toDate(),
        end: now.endOf('day').toDate(),
      },
      week: {
        start: now.startOf('week').toDate(),
        end: now.endOf('week').toDate(),
      },
      '30d': {
        start: now.subtract(29, 'day').startOf('day').toDate(),
        end: now.endOf('day').toDate(),
      },
      month: {
        start: now.startOf('month').toDate(),
        end: now.endOf('month').toDate(),
      },
    }
    const range = presets[kind]
    setDraftStart(toInputValue(range.start, precision))
    setDraftEnd(toInputValue(range.end, precision))
    onChange(range)
    setOpen(false)
  }

  return (
    <Popover open={open} onOpenChange={handleOpenChange}>
      <PopoverTrigger
        render={
          <Button
            id={id}
            type='button'
            variant='outline'
            aria-label={dateOnly ? `${t('Date Range')}: ${label}` : label}
            className={cn(
              'w-full justify-start gap-2 px-2.5 text-sm leading-5 font-normal tabular-nums',
              !start && !end && 'text-muted-foreground',
              className
            )}
          />
        }
      >
        <CalendarDays
          className='text-muted-foreground size-4 shrink-0'
          aria-hidden
        />
        <span className='hidden truncate sm:block'>{label}</span>
        <span className='min-w-0 [overflow-wrap:anywhere] whitespace-normal sm:hidden'>
          {mobileLabel}
        </span>
      </PopoverTrigger>
      <PopoverContent
        align='start'
        className={cn(
          'w-[min(520px,calc(100vw-2rem))] p-3',
          dateOnly &&
            'sm:w-96 [&_button]:min-h-11 [&_input]:min-h-11 sm:[&_button]:min-h-8 sm:[&_input]:min-h-8'
        )}
      >
        <div className='space-y-3'>
          <div className='grid gap-2 sm:grid-cols-[1fr_auto_1fr] sm:items-end'>
            <div className='space-y-1.5'>
              <div className='text-muted-foreground text-xs'>{startLabel}</div>
              <Input
                type={dateOnly ? 'date' : 'datetime-local'}
                value={draftStart}
                aria-label={startLabel}
                max={dateOnly && draftEnd ? draftEnd : undefined}
                onChange={(e) => setDraftStart(e.target.value)}
                className='h-8 text-sm leading-5 tabular-nums'
              />
            </div>
            <span className='text-muted-foreground hidden pb-2 text-xs sm:block'>
              ~
            </span>
            <div className='space-y-1.5'>
              <div className='text-muted-foreground text-xs'>{endLabel}</div>
              <Input
                type={dateOnly ? 'date' : 'datetime-local'}
                value={draftEnd}
                aria-label={endLabel}
                min={dateOnly && draftStart ? draftStart : undefined}
                onChange={(e) => setDraftEnd(e.target.value)}
                className='h-8 text-sm leading-5 tabular-nums'
              />
            </div>
          </div>

          <div className='flex flex-wrap gap-1.5'>
            <Button
              type='button'
              variant='secondary'
              size='sm'
              className='h-7 flex-1 px-2 text-xs'
              onClick={() => applyPreset('today')}
            >
              {t('Today')}
            </Button>
            <Button
              type='button'
              variant='secondary'
              size='sm'
              className='h-7 flex-1 px-2 text-xs'
              onClick={() => applyPreset('7d')}
            >
              {t('7 Days')}
            </Button>
            <Button
              type='button'
              variant='secondary'
              size='sm'
              className='h-7 flex-1 px-2 text-xs'
              onClick={() => applyPreset('week')}
            >
              {t('This week')}
            </Button>
            <Button
              type='button'
              variant='secondary'
              size='sm'
              className='h-7 flex-1 px-2 text-xs'
              onClick={() => applyPreset('30d')}
            >
              {t('30 Days')}
            </Button>
            <Button
              type='button'
              variant='secondary'
              size='sm'
              className='h-7 flex-1 px-2 text-xs'
              onClick={() => applyPreset('month')}
            >
              {t('Current month')}
            </Button>
          </div>

          <div className='flex justify-end'>
            <Button
              type='button'
              size='sm'
              className='h-8'
              disabled={
                dateOnly && !!draftStart && !!draftEnd && draftStart > draftEnd
              }
              onClick={applyDraft}
            >
              {t('Confirm')}
            </Button>
          </div>
        </div>
      </PopoverContent>
    </Popover>
  )
}
