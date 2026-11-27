/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as published by
the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For commercial licensing, please contact support@quantumnous.com
*/
import { useId } from 'react'
import { useTranslation } from 'react-i18next'

import { Label } from '@/components/ui/label'
import { Textarea } from '@/components/ui/textarea'

export function ReferenceImageUrlsField(props: {
  value: string
  onChange: (value: string) => void
  disabled: boolean
}) {
  const { t } = useTranslation()
  const id = useId()

  return (
    <div className='space-y-2'>
      <Label htmlFor={id} className='text-muted-foreground text-xs'>
        {t('Reference image URLs')}
      </Label>
      <Textarea
        id={id}
        value={props.value}
        onChange={(event) => props.onChange(event.target.value)}
        aria-describedby={`${id}-hint`}
        placeholder={
          'https://example.com/reference-1.png\nhttps://example.com/reference-2.png'
        }
        className='bg-background/60 min-h-28 resize-y font-mono text-xs leading-relaxed md:text-xs'
        disabled={props.disabled}
        spellCheck={false}
      />
      <p
        id={`${id}-hint`}
        className='text-muted-foreground text-xs leading-relaxed'
      >
        {t('Paste one image URL per line. Blank lines are ignored.')}
      </p>
    </div>
  )
}
