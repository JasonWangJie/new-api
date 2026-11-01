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
import { useId, useState } from 'react'
import { useTranslation } from 'react-i18next'

import {
  Field,
  FieldDescription,
  FieldGroup,
  FieldLabel,
  FieldLegend,
  FieldSet,
} from '@/components/ui/field'
import { Input } from '@/components/ui/input'
import type { TaskVideoInputMultipliers } from '@/features/pricing/lib/task-expr'

function VideoMultiplierField(props: {
  label: string
  value: number
  onChange: (value: number) => void
}) {
  const { t } = useTranslation()
  const id = useId()
  const [draft, setDraft] = useState<{ source: number; text: string } | null>(
    null
  )
  const text = draft?.source === props.value ? draft.text : String(props.value)
  const value = Number(text)
  const invalid = text.trim() === '' || !Number.isFinite(value) || value < 0
  const error = invalid ? t('Enter a finite multiplier of 0 or greater.') : ''

  return (
    <Field data-billing-invalid={invalid || undefined}>
      <FieldLabel htmlFor={id}>{props.label}</FieldLabel>
      <Input
        id={id}
        type='text'
        inputMode='decimal'
        value={text}
        aria-invalid={invalid || undefined}
        aria-describedby={invalid ? `${id}-error` : undefined}
        ref={(element) => element?.setCustomValidity(error)}
        onChange={(event) => {
          const nextText = event.target.value
          const nextValue = Number(nextText)
          const valid =
            nextText.trim() !== '' &&
            Number.isFinite(nextValue) &&
            nextValue >= 0
          setDraft({ source: valid ? nextValue : props.value, text: nextText })
          if (valid && nextValue !== props.value) props.onChange(nextValue)
        }}
        className='font-mono tabular-nums'
      />
      {invalid ? (
        <p id={`${id}-error`} role='alert' className='text-destructive text-xs'>
          {error}
        </p>
      ) : null}
    </Field>
  )
}

export function TaskVideoMultiplierFields(props: {
  value: TaskVideoInputMultipliers
  onChange: (value: TaskVideoInputMultipliers) => void
}) {
  const { t } = useTranslation()
  return (
    <FieldSet className='bg-background rounded-lg border p-4'>
      <FieldLegend variant='label'>
        {t('Reference video pricing multipliers')}
      </FieldLegend>
      <FieldDescription className='text-xs'>
        {t(
          'Multiply all base prices and additional charges for this model. 1 keeps the price unchanged; 1.5 adds 50%.'
        )}
      </FieldDescription>
      <FieldGroup className='grid gap-4 sm:grid-cols-2'>
        <VideoMultiplierField
          label={t('Without reference video multiplier')}
          value={props.value.none}
          onChange={(none) => props.onChange({ ...props.value, none })}
        />
        <VideoMultiplierField
          label={t('With reference video multiplier')}
          value={props.value.video}
          onChange={(video) => props.onChange({ ...props.value, video })}
        />
      </FieldGroup>
      <FieldDescription className='text-xs'>
        {t('0 makes matching requests free.')}
      </FieldDescription>
    </FieldSet>
  )
}
