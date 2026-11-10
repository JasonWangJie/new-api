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
import { useFieldArray, useWatch, type UseFormReturn } from 'react-hook-form'
import { useTranslation } from 'react-i18next'

import { StaticDataTable } from '@/components/data-table'
import { Button } from '@/components/ui/button'
import { Combobox } from '@/components/ui/combobox'
import {
  FormControl,
  FormField,
  FormItem,
  FormMessage,
} from '@/components/ui/form'
import { Input } from '@/components/ui/input'

import {
  IMAGE_MODEL_RESOLUTION_FIELDS,
  VIDEO_MODEL_RESOLUTION_FIELDS,
  type ChannelFormValues,
} from '../lib/channel-form'

const imageColumns = IMAGE_MODEL_RESOLUTION_FIELDS.map(({ label, field }) => ({
  label,
  fieldName: (index: number) =>
    `image_resolution_model_mapping.${index}.${field}` as const,
}))

const videoColumns = VIDEO_MODEL_RESOLUTION_FIELDS.map(({ label, field }) => ({
  label,
  fieldName: (index: number) =>
    `video_resolution_model_mapping.${index}.${field}` as const,
}))

export function ResolutionModelMapping(props: {
  form: UseFormReturn<ChannelFormValues>
  models: string[]
  media: 'image' | 'video'
  disabled: boolean
}) {
  const { t } = useTranslation()
  const isVideo = props.media === 'video'
  const name = isVideo
    ? 'video_resolution_model_mapping'
    : 'image_resolution_model_mapping'
  const resolutions = isVideo ? videoColumns : imageColumns
  const title = isVideo
    ? t('Async video resolution model mapping')
    : t('Async image resolution model mapping')
  const mappings = useFieldArray({
    control: props.form.control,
    name,
  })
  const rows =
    useWatch({
      control: props.form.control,
      name,
    }) || []
  const availableModels = props.models.filter(
    (model) => !rows.some((row) => row.model.trim() === model)
  )

  return (
    <div
      role='group'
      aria-label={title}
      className='min-w-0 space-y-3 sm:col-span-2'
    >
      <p className='text-sm font-medium'>{title}</p>
      <p className='text-muted-foreground text-sm'>
        {isVideo
          ? t(
              'Mixed async video requires an explicit resolution for mapped models. Blank resolutions are unsupported. Delete a row to restore ordinary model mapping.'
            )
          : t(
              'Only asynchronous images use these upstream models. Blank resolutions are unsupported. Delete a row to restore ordinary model mapping.'
            )}
      </p>
      {mappings.fields.length > 0 && (
        <StaticDataTable
          data={mappings.fields}
          getRowKey={(row) => row.id}
          tableClassName={isVideo ? 'min-w-[960px]' : 'min-w-[640px]'}
          columns={[
            {
              id: 'model',
              header: t('Request Model Name'),
              className: 'min-w-48',
              cell: (_, index) => (
                <FormField
                  control={props.form.control}
                  name={`${name}.${index}.model`}
                  render={({ field }) => {
                    const options = [
                      ...new Set([...availableModels, field.value || '']),
                    ]
                      .filter(Boolean)
                      .map((model) => ({
                        value: model,
                        label: props.models.includes(model)
                          ? model
                          : `${model} · ${t('Unavailable')}`,
                      }))
                    return (
                      <FormItem>
                        <FormControl>
                          <Combobox
                            options={options}
                            value={field.value || null}
                            onValueChange={(value) =>
                              field.onChange(value || '')
                            }
                            onBlur={field.onBlur}
                            disabled={props.disabled}
                            placeholder={t('Select model')}
                            aria-label={t('Resolution mapping source {{row}}', {
                              row: index + 1,
                            })}
                          />
                        </FormControl>
                        <FormMessage />
                      </FormItem>
                    )
                  }}
                />
              ),
            },
            ...resolutions.map((resolution) => ({
              id: resolution.label,
              header: resolution.label,
              className: 'min-w-40',
              cell: (_: unknown, index: number) => (
                <FormField
                  control={props.form.control}
                  name={resolution.fieldName(index)}
                  render={({ field }) => (
                    <FormItem>
                      <FormControl>
                        <Input
                          {...field}
                          disabled={props.disabled}
                          placeholder={t('Not supported')}
                          aria-label={t(
                            'Upstream model {{resolution}} row {{row}}',
                            {
                              resolution: resolution.label,
                              row: index + 1,
                            }
                          )}
                        />
                      </FormControl>
                      <FormMessage />
                    </FormItem>
                  )}
                />
              ),
            })),
            {
              id: 'delete',
              header: t('Actions'),
              cell: (_, index) => (
                <Button
                  type='button'
                  variant='ghost'
                  size='sm'
                  disabled={props.disabled}
                  aria-label={t('Delete resolution mapping {{row}}', {
                    row: index + 1,
                  })}
                  onClick={() => mappings.remove(index)}
                >
                  {t('Delete')}
                </Button>
              ),
            },
          ]}
        />
      )}
      <Button
        type='button'
        variant='outline'
        size='sm'
        disabled={props.disabled || availableModels.length === 0}
        onClick={() => {
          const model = availableModels.length === 1 ? availableModels[0] : ''
          mappings.append(
            isVideo
              ? {
                  model,
                  resolution_480p: '',
                  resolution_720p: '',
                  resolution_1080p: '',
                  resolution_2k: '',
                  resolution_4k: '',
                }
              : {
                  model,
                  resolution_1k: '',
                  resolution_2k: '',
                  resolution_4k: '',
                }
          )
        }}
      >
        {t('Add resolution mapping')}
      </Button>
    </div>
  )
}
