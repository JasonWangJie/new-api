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
import { useId, useState, useRef, useEffect, type ReactNode } from 'react'
import { useTranslation } from 'react-i18next'

import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { NativeSelect, NativeSelectOption } from '@/components/ui/native-select'
import { Textarea } from '@/components/ui/textarea'

export function RetryField(props: {
  label: string
  value: string | number
  onChange: (value: string) => void
  multiline?: boolean
  type?: string
  min?: number
  max?: number
  required?: boolean
}) {
  const { t } = useTranslation()
  const id = useId()
  const focused = useRef(false)
  const [draft, setDraft] = useState(String(props.value))
  useEffect(() => {
    if (!focused.current) setDraft(String(props.value))
  }, [props.value])
  const attributes = {
    id,
    value: props.multiline ? draft : props.value,
    required: props.required,
    onFocus: () => {
      focused.current = true
    },
    onBlur: () => {
      focused.current = false
    },
    onChange: (
      event: React.ChangeEvent<HTMLInputElement | HTMLTextAreaElement>
    ) => {
      setDraft(event.target.value)
      props.onChange(event.target.value)
    },
  }
  return (
    <div className='min-w-0 space-y-1.5'>
      <Label htmlFor={id}>{t(props.label)}</Label>
      {props.multiline ? (
        <Textarea {...attributes} rows={3} />
      ) : (
        <Input
          {...attributes}
          type={props.type ?? 'text'}
          min={props.min}
          max={props.max}
        />
      )}
    </div>
  )
}
export function RetrySelect(props: {
  label: string
  value: string
  options: Record<string, string>
  onChange: (value: string) => void
  children?: ReactNode
}) {
  const { t } = useTranslation()
  const id = useId()
  return (
    <div className='min-w-0 space-y-1.5'>
      <Label htmlFor={id}>{t(props.label)}</Label>
      <NativeSelect
        id={id}
        className='w-full'
        value={props.value}
        onChange={(event) => props.onChange(event.target.value)}
      >
        {Object.entries(props.options).map(([value, label]) => (
          <NativeSelectOption key={value} value={value}>
            {t(label)}
          </NativeSelectOption>
        ))}
      </NativeSelect>
      {props.children}
    </div>
  )
}
