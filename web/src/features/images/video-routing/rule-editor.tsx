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
import { zodResolver } from '@hookform/resolvers/zod'
import { useQuery } from '@tanstack/react-query'
import { useId, useState } from 'react'
import { useForm } from 'react-hook-form'
import { useTranslation } from 'react-i18next'

import { Dialog } from '@/components/dialog'
import { ErrorState } from '@/components/error-state'
import { LoadingState } from '@/components/loading-state'
import { MultiSelect } from '@/components/multi-select'
import { Button } from '@/components/ui/button'
import { Checkbox } from '@/components/ui/checkbox'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'

import { imageRequest } from '../api'
import { ImageSelect } from '../components/image-select'
import { imageLabel } from '../lib/image-labels'
import type { ImageAdminConfiguration } from '../types'
import { VIDEO_ROUTING_URL, videoRoutingRuleSchema } from './policy'
import { VideoRoutingTargets, type VideoRoutingTargetDraft } from './targets'
import type { VideoRoutingOptions, VideoRoutingRule } from './types'

export function VideoRoutingRuleEditor(props: {
  rule: VideoRoutingRule
  config: ImageAdminConfiguration
  onClose: () => void
  onApply: (rule: VideoRoutingRule) => void
}) {
  const { t } = useTranslation()
  const formId = useId()
  const [rule, setRule] = useState<
    Omit<VideoRoutingRule, 'targets'> & { targets: VideoRoutingTargetDraft[] }
  >(() => ({
    ...structuredClone(props.rule),
    targets: props.rule.targets.map((target) => ({
      ...target,
      draftID: crypto.randomUUID(),
    })),
  }))
  const form = useForm<VideoRoutingRule>({
    values: rule,
    resolver: zodResolver(videoRoutingRuleSchema),
  })
  const options = useQuery({
    queryKey: ['video-routing-options', rule.match.groups],
    queryFn: async ({ signal }) => {
      const groups = rule.match.groups.length ? rule.match.groups : ['']
      const results = await Promise.all(
        groups.map((group) =>
          imageRequest<VideoRoutingOptions>(
            `${VIDEO_ROUTING_URL}/options?${new URLSearchParams({ group })}`,
            'GET',
            undefined,
            signal
          )
        )
      )
      const channels = new Map<
        number,
        VideoRoutingOptions['channels'][number]
      >()
      for (const result of results) {
        for (const channel of result.channels) {
          const existing = channels.get(channel.id)
          const models = new Map(
            (existing?.models || []).map((item) => [item.model, item])
          )
          for (const model of channel.models) models.set(model.model, model)
          channels.set(channel.id, { ...channel, models: [...models.values()] })
        }
      }
      return { channels: [...channels.values()] }
    },
  })
  const groups = [
    ...new Set([
      'default',
      ...props.config.policies.map((item) => item.group),
      ...props.config.pools.map((item) => item.group),
      ...rule.match.groups,
    ]),
  ]
  const models = [
    ...new Set([
      ...rule.match.models,
      ...(options.data?.channels.flatMap((item) =>
        item.models.map((model) => model.model)
      ) || []),
    ]),
  ]
  const choices = [
    {
      key: 'groups' as const,
      label: t('Groups'),
      values: groups,
      create: true,
    },
    {
      key: 'models' as const,
      label: t('Requested models'),
      values: models,
      create: true,
    },
    {
      key: 'resolutions' as const,
      label: t('Input resolutions'),
      values: ['480p', '720p', '1080p', '2k', '4k'],
      create: false,
    },
    {
      key: 'modes' as const,
      label: t('Generation mode'),
      values: [
        'text_to_video',
        'image_to_video',
        'reference_to_video',
        'first_last_frame',
      ],
      create: false,
    },
    {
      key: 'video_inputs' as const,
      label: t('Reference video'),
      values: ['none', 'video'],
      create: false,
    },
  ]
  return (
    <Dialog
      open
      onOpenChange={(open) => {
        if (!open) props.onClose()
      }}
      title={t('Edit video routing rule')}
      description={t('Changes remain a draft until the policy is saved.')}
      contentClassName='sm:max-w-3xl'
      footer={
        <>
          <Button variant='outline' onClick={props.onClose}>
            {t('Cancel')}
          </Button>
          <Button type='submit' form={formId}>
            {t('Apply to draft')}
          </Button>
        </>
      }
    >
      <form
        id={formId}
        className='space-y-6'
        onSubmit={form.handleSubmit((value) => props.onApply(value))}
      >
        {Object.keys(form.formState.errors).length > 0 && (
          <p role='alert' className='text-destructive'>
            {t(
              'Check the rule name, requested models, duration range and targets.'
            )}
          </p>
        )}
        <div className='space-y-2'>
          <Label htmlFor={`${formId}-name`}>{t('Rule name')}</Label>
          <Input
            id={`${formId}-name`}
            required
            maxLength={128}
            value={rule.name}
            onChange={(event) => setRule({ ...rule, name: event.target.value })}
          />
        </div>
        <fieldset className='space-y-4'>
          <legend className='mb-2 font-medium'>
            {t('Request conditions')}
          </legend>
          <p className='text-muted-foreground text-sm'>
            {t(
              'Conditions are combined with AND. Empty selections match any value, including all models.'
            )}
          </p>
          <div className='grid gap-4 sm:grid-cols-2'>
            {choices.map((choice) => (
              <div key={choice.key} className='min-w-0 space-y-2'>
                <Label htmlFor={`${formId}-${choice.key}`}>
                  {choice.label}
                </Label>
                <MultiSelect
                  id={`${formId}-${choice.key}`}
                  placeholder={choice.label}
                  selected={rule.match[choice.key]}
                  options={choice.values.map((value) => {
                    let label = value
                    if (choice.key === 'modes') label = t(imageLabel(value))
                    if (choice.key === 'video_inputs') {
                      label = t(value === 'none' ? 'None' : 'Video')
                    }
                    return { value, label }
                  })}
                  allowCreate={choice.create}
                  onChange={(values) => {
                    if (choice.key === 'video_inputs') {
                      setRule({
                        ...rule,
                        match: {
                          ...rule.match,
                          video_inputs: values.filter(
                            (value): value is 'none' | 'video' =>
                              value === 'none' || value === 'video'
                          ),
                        },
                      })
                    } else {
                      setRule({
                        ...rule,
                        match: { ...rule.match, [choice.key]: values },
                      })
                    }
                  }}
                />
              </div>
            ))}
          </div>
          <div className='grid gap-4 sm:grid-cols-2'>
            {(['min', 'max'] as const).map((bound) => (
              <div key={bound} className='space-y-2'>
                <Label htmlFor={`${formId}-${bound}`}>
                  {bound === 'min'
                    ? t('Minimum duration (seconds)')
                    : t('Maximum duration (seconds)')}
                </Label>
                <Input
                  id={`${formId}-${bound}`}
                  type='number'
                  min={1}
                  max={3600}
                  step={1}
                  value={rule.match.seconds[bound] ?? ''}
                  onChange={(event) =>
                    setRule({
                      ...rule,
                      match: {
                        ...rule.match,
                        seconds: {
                          ...rule.match.seconds,
                          [bound]:
                            event.target.value === ''
                              ? undefined
                              : Number(event.target.value),
                          [`${bound}_exclusive`]:
                            event.target.value === ''
                              ? false
                              : rule.match.seconds[`${bound}_exclusive`],
                        },
                      },
                    })
                  }
                />
                <div className='flex items-center gap-2'>
                  <Checkbox
                    id={`${formId}-${bound}-exclusive`}
                    checked={rule.match.seconds[`${bound}_exclusive`]}
                    disabled={rule.match.seconds[bound] === undefined}
                    onCheckedChange={(checked) =>
                      setRule({
                        ...rule,
                        match: {
                          ...rule.match,
                          seconds: {
                            ...rule.match.seconds,
                            [`${bound}_exclusive`]: checked === true,
                          },
                        },
                      })
                    }
                  />
                  <Label htmlFor={`${formId}-${bound}-exclusive`}>
                    {bound === 'min'
                      ? t('Exclude minimum')
                      : t('Exclude maximum')}
                  </Label>
                </div>
              </div>
            ))}
          </div>
        </fieldset>
        <fieldset className='space-y-3'>
          <legend className='mb-2 font-medium'>{t('Execution targets')}</legend>
          {form.formState.errors.targets?.message ===
            'Targets in a rule must use different channels.' && (
            <p role='alert' className='text-destructive text-sm'>
              {t('Targets in a rule must use different channels.')}
            </p>
          )}
          {options.isLoading && <LoadingState size='sm' />}
          {options.error && (
            <ErrorState
              description={options.error.message}
              onRetry={() => options.refetch()}
            />
          )}
          <VideoRoutingTargets
            targets={rule.targets}
            sourceResolutions={rule.match.resolutions}
            options={options.data}
            onChange={(targets) => setRule({ ...rule, targets })}
          />
          <ImageSelect
            label={t('When targets are exhausted')}
            value={rule.on_exhausted}
            options={[
              { value: 'fail', label: t('Fail the task') },
              { value: 'original', label: t('Return to original routing') },
            ]}
            onChange={(value) =>
              setRule({
                ...rule,
                on_exhausted: value === 'original' ? 'original' : 'fail',
              })
            }
          />
        </fieldset>
      </form>
    </Dialog>
  )
}
