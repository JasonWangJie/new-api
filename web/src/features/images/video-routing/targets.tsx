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
import { useLayoutEffect, useRef } from 'react'
import { useTranslation } from 'react-i18next'

import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Combobox } from '@/components/ui/combobox'
import { Label } from '@/components/ui/label'

import { ImageSelect } from '../components/image-select'
import { moveVideoRoutingItem } from './policy'
import type { VideoRoutingOptions, VideoRoutingTarget } from './types'

export type VideoRoutingTargetDraft = VideoRoutingTarget & { draftID: string }

export function VideoRoutingTargets(props: {
  targets: VideoRoutingTargetDraft[]
  sourceResolutions: string[]
  options?: VideoRoutingOptions
  onChange: (targets: VideoRoutingTargetDraft[]) => void
}) {
  const { t } = useTranslation()
  const channelInputs = useRef(new Map<string, HTMLInputElement>())
  const reorderedTarget = useRef<string | null>(null)
  useLayoutEffect(() => {
    if (!reorderedTarget.current) return
    channelInputs.current.get(reorderedTarget.current)?.focus()
    reorderedTarget.current = null
  }, [props.targets])
  return (
    <div className='space-y-3'>
      <p className='text-muted-foreground text-sm'>
        {t(
          'Targets are tried in order. Keep the requested resolution unless an explicit output resolution is selected.'
        )}
      </p>
      {props.targets.map((target, index) => {
        const resolutions = ['480p', '720p', '1080p', '2k', '4k']
        const targetResolution = resolutions.indexOf(
          target.resolution.toLowerCase()
        )
        const mayDowngrade =
          target.resolution !== '' &&
          (props.sourceResolutions.length === 0 ||
            props.sourceResolutions.some((resolution) => {
              const sourceResolution = resolutions.indexOf(
                resolution.toLowerCase()
              )
              return (
                sourceResolution === -1 ||
                targetResolution === -1 ||
                sourceResolution > targetResolution
              )
            }))
        const channel = props.options?.channels.find(
          (item) => item.id === target.channel_id
        )
        const channels = (props.options?.channels || []).map((item) => ({
          value: String(item.id),
          label: `#${item.id} · ${item.name}`,
        }))
        if (target.channel_id && !channel) {
          channels.push({
            value: String(target.channel_id),
            label: `#${target.channel_id} · ${t('Unavailable')}`,
          })
        }
        const models = (channel?.models || []).map((item) => ({
          value: item.model,
          label:
            item.model === item.upstream_model
              ? item.model
              : `${item.model} → ${item.upstream_model}`,
        }))
        if (
          target.model &&
          !models.some((item) => item.value === target.model)
        ) {
          models.push({
            value: target.model,
            label: `${target.model} · ${t('Unavailable')}`,
          })
        }
        return (
          <fieldset
            key={target.draftID}
            className='min-w-0 space-y-3 rounded-md border p-3'
          >
            <legend className='px-1 text-sm font-medium'>
              {t('Target {{number}}', { number: index + 1 })}
            </legend>
            <div className='grid gap-3 sm:grid-cols-2'>
              <div className='space-y-2'>
                <Label htmlFor={`route-target-channel-${index}`}>
                  {t('Channel')}
                </Label>
                <Combobox
                  ref={(element: HTMLInputElement | null) => {
                    if (element) {
                      channelInputs.current.set(target.draftID, element)
                    } else {
                      channelInputs.current.delete(target.draftID)
                    }
                  }}
                  id={`route-target-channel-${index}`}
                  options={channels}
                  value={target.channel_id ? String(target.channel_id) : ''}
                  onValueChange={(value) => {
                    if (value !== null) {
                      props.onChange(
                        props.targets.map((item, at) =>
                          at === index
                            ? { ...item, channel_id: Number(value), model: '' }
                            : item
                        )
                      )
                    }
                  }}
                  className='w-full'
                />
              </div>
              <div className='space-y-2'>
                <Label htmlFor={`route-target-model-${index}`}>
                  {t('Target model')}
                </Label>
                <Combobox
                  id={`route-target-model-${index}`}
                  options={models}
                  value={target.model}
                  disabled={!target.channel_id}
                  onValueChange={(value) => {
                    if (value !== null) {
                      props.onChange(
                        props.targets.map((item, at) =>
                          at === index ? { ...item, model: value } : item
                        )
                      )
                    }
                  }}
                  className='w-full'
                />
              </div>
              <ImageSelect
                label={t('Output resolution')}
                value={target.resolution || 'keep'}
                options={[
                  { value: 'keep', label: t('Keep requested resolution') },
                  ...['480p', '720p', '1080p', '2k', '4k'].map((value) => ({
                    value,
                    label: value,
                  })),
                ]}
                onChange={(resolution) =>
                  props.onChange(
                    props.targets.map((item, at) =>
                      at === index
                        ? {
                            ...item,
                            resolution: resolution === 'keep' ? '' : resolution,
                          }
                        : item
                    )
                  )
                }
              />
            </div>
            {mayDowngrade && (
              <Alert>
                <AlertDescription>
                  {t(
                    'Some matching requests may be downgraded to this resolution. Billing still uses the original request resolution.'
                  )}
                </AlertDescription>
              </Alert>
            )}
            {props.options &&
              (!channel ||
                !channel.models.some((item) => item.model === target.model)) &&
              target.channel_id > 0 && (
                <p role='alert' className='text-destructive text-sm'>
                  {t(
                    'This target is unavailable for the selected groups. Check its channel and model.'
                  )}
                </p>
              )}
            <div className='flex flex-wrap gap-2'>
              <Button
                type='button'
                variant='outline'
                size='sm'
                aria-label={t('Move target {{number}} up', {
                  number: index + 1,
                })}
                disabled={index === 0}
                onClick={() => {
                  reorderedTarget.current = target.draftID
                  props.onChange(moveVideoRoutingItem(props.targets, index, -1))
                }}
              >
                {t('Move up')}
              </Button>
              <Button
                type='button'
                variant='outline'
                size='sm'
                aria-label={t('Move target {{number}} down', {
                  number: index + 1,
                })}
                disabled={index === props.targets.length - 1}
                onClick={() => {
                  reorderedTarget.current = target.draftID
                  props.onChange(moveVideoRoutingItem(props.targets, index, 1))
                }}
              >
                {t('Move down')}
              </Button>
              <Button
                type='button'
                variant='outline'
                size='sm'
                aria-label={t('Remove target {{number}}', {
                  number: index + 1,
                })}
                disabled={props.targets.length === 1}
                onClick={() =>
                  props.onChange(props.targets.filter((_, at) => at !== index))
                }
              >
                {t('Remove')}
              </Button>
            </div>
          </fieldset>
        )
      })}
      <Button
        type='button'
        variant='outline'
        disabled={props.targets.length >= 20}
        onClick={() =>
          props.onChange([
            ...props.targets,
            {
              channel_id: 0,
              model: '',
              resolution: '',
              draftID: crypto.randomUUID(),
            },
          ])
        }
      >
        {t('Add target')}
      </Button>
    </div>
  )
}
