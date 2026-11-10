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

import { JsonEditor } from '@/components/json-editor'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'

import {
  VIDEO_MODEL_RESOLUTION_FIELDS,
  type ChannelFormValues,
} from '../lib/channel-form'
import {
  importMissingTaskVideoModels,
  taskVideoCatalogConfig,
} from '../lib/task-video-models'
import type { TaskVideoModelConfig, TaskVideoModelDetail } from '../types'

interface TaskVideoModelEditorProps {
  manualOnly?: boolean
  defaultModels?: readonly string[]
  sourceURL: string
  onSourceURLChange: (value: string) => void
  value: string
  onChange: (value: string) => void
  models: string[]
  modelMapping?: string
  videoResolutionMappings?: ChannelFormValues['video_resolution_model_mapping']
  details?: Record<string, TaskVideoModelDetail>
  previewReady: boolean
  disabled?: boolean
}

export function TaskVideoModelEditor(props: TaskVideoModelEditorProps) {
  const { t } = useTranslation()
  let configs: Record<string, TaskVideoModelConfig> = {}
  let mapping: Record<string, string> = {}
  try {
    const parsed: unknown = JSON.parse(props.value || '{}')
    if (parsed && typeof parsed === 'object' && !Array.isArray(parsed)) {
      configs = parsed as Record<string, TaskVideoModelConfig>
    }
  } catch {
    /* The shared editor reports invalid JSON. */
  }
  try {
    const parsed: unknown = JSON.parse(props.modelMapping || '{}')
    if (parsed && typeof parsed === 'object' && !Array.isArray(parsed)) {
      mapping = parsed as Record<string, string>
    }
  } catch {
    /* Mapping validation belongs to the channel form. */
  }
  const resolutionTargets = new Map(
    props.videoResolutionMappings?.map((row) => [
      row.model.trim(),
      VIDEO_MODEL_RESOLUTION_FIELDS.map(({ field }) =>
        row[field].trim()
      ).filter(Boolean),
    ])
  )
  const upstreamModels = [
    ...new Set(
      props.models.flatMap((model) => {
        const targets = resolutionTargets.get(model) || []
        if (targets.length && !mapping[model]) return targets
        const seen = new Set<string>()
        while (mapping[model] && !seen.has(model)) {
          seen.add(model)
          model = mapping[model]
        }
        return [...targets, model]
      })
    ),
  ]
  const preview =
    !props.manualOnly && props.previewReady ? props.details : undefined
  const changed = upstreamModels.filter(
    (model) =>
      preview?.[model] &&
      JSON.stringify(taskVideoCatalogConfig(preview[model])) !==
        JSON.stringify(configs[model])
  )

  return (
    <div className='mt-4 space-y-3 rounded-lg border p-4'>
      {!props.manualOnly && (
        <>
          <Label htmlFor='task-video-model-source'>
            {t('Model catalog URL')}
          </Label>
          <Input
            id='task-video-model-source'
            value={props.sourceURL}
            disabled={props.disabled}
            placeholder='https://catalog.sudashuiapi.com/v1/models'
            onChange={(event) => props.onSourceURLChange(event.target.value)}
          />
          <p className='text-muted-foreground text-xs'>
            {t(
              'Catalog fetches are previews. Manual capabilities are preserved until you explicitly replace them and save the channel.'
            )}
          </p>
        </>
      )}
      <Button
        type='button'
        variant='outline'
        size='sm'
        disabled={props.disabled}
        onClick={() =>
          props.onChange(
            JSON.stringify(
              importMissingTaskVideoModels(
                configs,
                upstreamModels.filter(
                  (model) => !props.defaultModels?.includes(model)
                ),
                preview,
                props.manualOnly
              ),
              null,
              2
            )
          )
        }
      >
        {t('Initialize missing model capabilities')}
      </Button>
      <p className='text-muted-foreground text-xs'>
        {t(
          'Complete duration, resolutions, aspect_ratios, modes, references and billing_mode for every selected upstream model. Unknown reference limits require manual input.'
        )}
      </p>
      <JsonEditor
        value={props.value}
        onChange={props.onChange}
        disabled={props.disabled}
        valueType='any'
        keyLabel={t('Upstream model')}
        valueLabel={t('Model capabilities')}
        template={{
          'upstream-model': taskVideoCatalogConfig(undefined, props.manualOnly),
        }}
      />
      {changed.map((model) => (
        <div key={model} className='space-y-2 rounded-md border p-3'>
          <p className='text-sm font-medium break-all'>
            {model} · {t('Catalog capability changes')}
          </p>
          <pre className='max-h-48 overflow-auto text-xs'>
            {JSON.stringify(taskVideoCatalogConfig(preview?.[model]), null, 2)}
          </pre>
          {preview?.[model]?.currency && (
            <p className='text-muted-foreground text-xs'>
              {t(
                'Upstream reference price; site pricing is configured separately'
              )}
              : {preview[model].final_price ?? preview[model].price ?? '—'}{' '}
              {preview[model].currency}
            </p>
          )}
          <Button
            type='button'
            variant='outline'
            size='sm'
            disabled={props.disabled}
            onClick={() =>
              props.onChange(
                JSON.stringify(
                  {
                    ...configs,
                    [model]: taskVideoCatalogConfig(preview?.[model]),
                  },
                  null,
                  2
                )
              )
            }
          >
            {t('Use catalog capabilities')}
          </Button>
        </div>
      ))}
    </div>
  )
}
