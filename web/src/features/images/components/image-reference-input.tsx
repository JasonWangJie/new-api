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
import { ArrowDown, ArrowUp, Link2, Plus, Trash2, Upload } from 'lucide-react'
import type { Dispatch, SetStateAction } from 'react'
import { useTranslation } from 'react-i18next'

import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Textarea } from '@/components/ui/textarea'

export type ReferencePart = {
  id: string
  type: 'text' | 'image' | 'image_urls'
  value: string
}

export function ImageReferenceInput(props: {
  parts: ReferencePart[]
  onChange: Dispatch<SetStateAction<ReferencePart[]>>
  onUpload: (files: File[]) => Promise<void>
  disabled: boolean
}) {
  const { t } = useTranslation()
  const referenceCount = props.parts.reduce((total, part) => {
    if (part.type === 'text') return total
    if (part.type === 'image') return total + 1
    return total + part.value.split(/\r?\n/).filter((url) => url.trim()).length
  }, 0)
  const movePart = (index: number, offset: number) => {
    props.onChange((previous) => {
      const next = [...previous]
      ;[next[index], next[index + offset]] = [next[index + offset], next[index]]
      return next
    })
  }

  return (
    <fieldset
      className='min-w-0 space-y-3 border-b p-5 sm:p-6'
      disabled={props.disabled}
    >
      <legend className='sr-only'>
        {t('Reference images and text parts')}
      </legend>
      <div className='flex flex-wrap items-center justify-between gap-2'>
        <h4 className='flex items-center gap-2 text-sm font-medium'>
          <Link2 className='text-muted-foreground size-4' aria-hidden='true' />
          {t('Reference images and text parts')}
        </h4>
        <Badge variant='secondary' className='text-xs tabular-nums'>
          {t('References: {{total}}', { total: referenceCount })}
        </Badge>
      </div>
      {props.parts.map((part, index) => (
        <div
          key={part.id}
          className={
            part.type === 'image_urls'
              ? 'space-y-2'
              : 'bg-muted/25 space-y-2 rounded-xl border p-3'
          }
        >
          {part.type === 'image_urls' && (
            <div className='space-y-2'>
              <Label
                htmlFor='image-reference-urls'
                className='text-muted-foreground text-xs'
              >
                {t('Reference image URLs')}
              </Label>
              <Textarea
                id='image-reference-urls'
                value={part.value}
                onChange={(event) =>
                  props.onChange((previous) =>
                    previous.map((candidate) =>
                      candidate.id === part.id
                        ? { ...candidate, value: event.target.value }
                        : candidate
                    )
                  )
                }
                aria-describedby='image-reference-urls-hint'
                placeholder={
                  'https://example.com/reference-1.png\nhttps://example.com/reference-2.png'
                }
                className='bg-background/60 min-h-28 resize-y font-mono text-xs leading-relaxed md:text-xs'
                disabled={props.disabled}
                spellCheck={false}
              />
              <p
                id='image-reference-urls-hint'
                className='text-muted-foreground text-xs leading-relaxed'
              >
                {t('Paste one image URL per line. Blank lines are ignored.')}
              </p>
            </div>
          )}
          {part.type === 'image' && (
            <img
              className='bg-background/60 h-28 w-full rounded-lg object-contain'
              src={part.value}
              alt={t('Reference image')}
            />
          )}
          {part.type === 'text' && (
            <Textarea
              value={part.value}
              onChange={(event) =>
                props.onChange((previous) =>
                  previous.map((candidate) =>
                    candidate.id === part.id
                      ? { ...candidate, value: event.target.value }
                      : candidate
                  )
                )
              }
              aria-label={t('Text part')}
              placeholder={t('Text part')}
              className='bg-background/60 min-h-20 resize-y'
            />
          )}
          {props.parts.length > 1 && (
            <div className='flex justify-end gap-1'>
              <Button
                type='button'
                variant='ghost'
                size='icon-sm'
                aria-label={t('Move up')}
                disabled={props.disabled || !index}
                onClick={() => movePart(index, -1)}
              >
                <ArrowUp className='size-3.5' aria-hidden='true' />
              </Button>
              <Button
                type='button'
                variant='ghost'
                size='icon-sm'
                aria-label={t('Move down')}
                disabled={props.disabled || index === props.parts.length - 1}
                onClick={() => movePart(index, 1)}
              >
                <ArrowDown className='size-3.5' aria-hidden='true' />
              </Button>
              {part.type !== 'image_urls' && (
                <Button
                  type='button'
                  variant='ghost'
                  size='icon-sm'
                  aria-label={t('Remove')}
                  disabled={props.disabled}
                  onClick={() =>
                    props.onChange((previous) =>
                      previous.filter((candidate) => candidate.id !== part.id)
                    )
                  }
                >
                  <Trash2 className='size-3.5' aria-hidden='true' />
                </Button>
              )}
            </div>
          )}
        </div>
      ))}
      <div className='space-y-2 rounded-xl border border-dashed p-3'>
        <Label
          htmlFor='image-reference-file'
          className='flex items-center gap-2 text-xs'
        >
          <Upload
            className='text-muted-foreground size-3.5'
            aria-hidden='true'
          />
          {t('Upload reference image')}
        </Label>
        <Input
          id='image-reference-file'
          type='file'
          accept='image/png,image/jpeg,image/webp'
          multiple
          className='h-auto min-h-9 min-w-0 py-1.5 text-xs file:mr-2 file:text-xs'
          aria-describedby='image-reference-file-hint'
          onChange={(event) => {
            const files = [...(event.target.files || [])]
            if (files.length) void props.onUpload(files)
            event.target.value = ''
          }}
        />
        <p
          id='image-reference-file-hint'
          className='text-muted-foreground text-[11px] leading-relaxed'
        >
          {t('PNG, JPG or WebP · Up to 32 MB each')}
        </p>
      </div>
      <Button
        type='button'
        variant='ghost'
        size='sm'
        disabled={props.disabled}
        className='text-muted-foreground'
        onClick={() =>
          props.onChange((previous) => [
            ...previous,
            { id: crypto.randomUUID(), type: 'text', value: '' },
          ])
        }
      >
        <Plus className='size-3.5' aria-hidden='true' />
        {t('Add text part')}
      </Button>
    </fieldset>
  )
}
