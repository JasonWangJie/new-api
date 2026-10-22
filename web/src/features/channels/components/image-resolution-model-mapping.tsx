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

import type { ChannelFormValues } from '../lib/channel-form'

const resolutions = [
  { label: '1K', field: 'resolution_1k' },
  { label: '2K', field: 'resolution_2k' },
  { label: '4K', field: 'resolution_4k' },
] as const

export function ImageResolutionModelMapping(props: {
  form: UseFormReturn<ChannelFormValues>
  models: string[]
  disabled: boolean
}) {
  const { t } = useTranslation()
  const mappings = useFieldArray({
    control: props.form.control,
    name: 'image_resolution_model_mapping',
  })
  const rows =
    useWatch({
      control: props.form.control,
      name: 'image_resolution_model_mapping',
    }) || []
  const availableModels = props.models.filter(
    (model) => !rows.some((row) => row.model.trim() === model)
  )

  return (
    <div className='space-y-3 sm:col-span-2'>
      <p className='text-sm font-medium'>
        {t('Async image resolution model mapping')}
      </p>
      <p className='text-muted-foreground text-sm'>
        {t(
          'Only asynchronous images use these upstream models. Blank resolutions are unsupported. Delete a row to restore ordinary model mapping.'
        )}
      </p>
      {mappings.fields.length > 0 && (
        <StaticDataTable
          data={mappings.fields}
          getRowKey={(row) => row.id}
          tableClassName='min-w-[640px]'
          columns={[
            {
              id: 'model',
              header: t('Request Model Name'),
              className: 'min-w-48',
              cell: (_, index) => (
                <FormField
                  control={props.form.control}
                  name={`image_resolution_model_mapping.${index}.model`}
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
                  name={`image_resolution_model_mapping.${index}.${resolution.field}`}
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
        onClick={() =>
          mappings.append({
            model: availableModels.length === 1 ? availableModels[0] : '',
            resolution_1k: '',
            resolution_2k: '',
            resolution_4k: '',
          })
        }
      >
        {t('Add resolution mapping')}
      </Button>
    </div>
  )
}
