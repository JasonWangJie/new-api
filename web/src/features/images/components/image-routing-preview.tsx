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
import { useMutation } from '@tanstack/react-query'
import { useState } from 'react'
import { useTranslation } from 'react-i18next'

import { StaticDataTable } from '@/components/data-table'
import { ErrorState } from '@/components/error-state'
import { LoadingState } from '@/components/loading-state'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { SettingsCard } from '@/features/system-settings/components/settings-card'

import { imageRequest } from '../api'
import {
  imageLabel,
  imagePlatformLabel,
  imageRoutingPlatforms,
} from '../lib/image-labels'
import type { ImageAdminConfiguration } from '../types'
import { ImageGroupSelect } from './image-group-select'
import { ImageSelect } from './image-select'

type RouteCandidate = {
  group: string
  binding_key: string
  binding_found: boolean
  channel_id: number
  name: string
  upstream_model: string
  provider?: string
  protocol?: string
  priority: number
  weight: number
  available_accounts: number
  reasons: string[]
}
type RoutePreview = {
  routing_groups: string[]
  binding_key: string
  binding_found: boolean
  pool_mode: string
  eligible: RouteCandidate[]
  excluded: RouteCandidate[]
  warnings: string[]
  reasons: string[]
}

export function ImageRoutingPreview(props: {
  config: ImageAdminConfiguration
}) {
  const { t } = useTranslation()
  const [values, setValues] = useState({
    group: 'default',
    platform:
      props.config.media_providers?.[0] ||
      props.config.image_providers?.[0] ||
      'openai',
    media_type: 'image',
    execution_mode: 'async',
    model: '',
    resolution: '1K',
    operation: 'text_to_image',
    token_id: '',
    seconds: '',
  })
  const preview = useMutation({
    mutationFn: () =>
      imageRequest<RoutePreview>('/api/option/images/routing/preview', 'POST', {
        ...values,
        model: values.model.trim(),
        resolution: values.resolution.trim(),
        token_id: values.token_id ? Number(values.token_id) : undefined,
        seconds:
          values.media_type === 'video' && values.seconds
            ? Number(values.seconds)
            : undefined,
      }),
  })
  const operations =
    values.media_type === 'image'
      ? ['text_to_image', 'image_to_image']
      : [
          'text_to_video',
          'image_to_video',
          'reference_to_video',
          'first_last_frame',
          'edit_video',
          'extend_video',
        ]
  const change = (key: keyof typeof values, value: string) => {
    setValues((previous) => ({ ...previous, [key]: value }))
    preview.reset()
  }
  const rows = [
    ...(preview.data?.eligible || []).map((candidate) => ({
      ...candidate,
      eligible: true,
    })),
    ...(preview.data?.excluded || []).map((candidate) => ({
      ...candidate,
      eligible: false,
    })),
  ]
  return (
    <SettingsCard
      title={t('Routing preview')}
      description={t(
        'Preview the current configuration without sending upstream requests. Live capacity, cooldowns, circuit breakers, billing and complete input validation are checked on submission.'
      )}
    >
      <form
        className='space-y-4'
        onSubmit={(event) => {
          event.preventDefault()
          if (!preview.isPending) preview.mutate()
        }}
      >
        <fieldset
          className='grid gap-4 sm:grid-cols-2 lg:grid-cols-3'
          disabled={preview.isPending}
        >
          <ImageGroupSelect
            config={props.config}
            value={values.group}
            disabled={preview.isPending}
            onChange={(value) => change('group', value)}
          />
          <ImageSelect
            label={t('Platform')}
            value={values.platform}
            disabled={preview.isPending}
            options={imageRoutingPlatforms(
              props.config.routing_platforms ||
                props.config.media_providers ||
                props.config.image_providers
            ).map((value) => ({ value, label: t(imagePlatformLabel(value)) }))}
            onChange={(value) => {
              setValues((previous) => ({
                ...previous,
                platform: value,
                ...(value === 'mixed'
                  ? {
                      media_type: 'image',
                      execution_mode: 'async',
                      operation: 'text_to_image',
                      resolution: '1K',
                      seconds: '',
                    }
                  : {}),
              }))
              preview.reset()
            }}
          />
          <ImageSelect
            label={t('Media type')}
            value={values.media_type}
            disabled={preview.isPending || values.platform === 'mixed'}
            options={[
              { value: 'image', label: t('Image') },
              { value: 'video', label: t('Video') },
            ]}
            onChange={(value) => {
              setValues((previous) => ({
                ...previous,
                media_type: value,
                execution_mode: value === 'video' ? 'async' : previous.execution_mode,
                operation:
                  value === 'image' ? 'text_to_image' : 'text_to_video',
                resolution: value === 'image' ? '1K' : '',
              }))
              preview.reset()
            }}
          />
          <ImageSelect
            label={t('Execution mode')}
            value={values.execution_mode}
            disabled={preview.isPending || values.media_type === 'video' || values.platform === 'mixed'}
            options={[
              { value: 'sync', label: t('Synchronous image API') },
              { value: 'async', label: t('Background task API') },
            ]}
            onChange={(value) => change('execution_mode', value)}
          />
          <div className='space-y-2'>
            <Label htmlFor='route-preview-model'>{t('Model')}</Label>
            <Input
              id='route-preview-model'
              value={values.model}
              required
              onChange={(event) => change('model', event.target.value)}
            />
          </div>
          <div className='space-y-2'>
            <Label htmlFor='route-preview-resolution'>{t('Resolution')}</Label>
            <Input
              id='route-preview-resolution'
              value={values.resolution}
              required={values.media_type === 'image'}
              placeholder={t('Default')}
              onChange={(event) => change('resolution', event.target.value)}
            />
          </div>
          <ImageSelect
            label={t('Generation mode')}
            value={values.operation}
            disabled={preview.isPending}
            options={operations.map((value) => ({
              value,
              label: t(imageLabel(value)),
            }))}
            onChange={(value) => change('operation', value)}
          />
          <div className='space-y-2'>
            <Label htmlFor='route-preview-token'>
              {t('API key ID (optional)')}
            </Label>
            <Input
              id='route-preview-token'
              type='number'
              min={1}
              step={1}
              value={values.token_id}
              onChange={(event) => change('token_id', event.target.value)}
            />
          </div>
          {values.media_type === 'video' && (
            <div className='space-y-2'>
              <Label htmlFor='route-preview-seconds'>
                {t('Duration (seconds)')}
              </Label>
              <Input
                id='route-preview-seconds'
                type='number'
                min={1}
                step={1}
                value={values.seconds}
                onChange={(event) => change('seconds', event.target.value)}
              />
            </div>
          )}
        </fieldset>
        <Button
          type='submit'
          disabled={
            preview.isPending ||
            !values.model.trim() ||
            (values.media_type === 'image' && !values.resolution.trim())
          }
        >
          {t('Preview routing')}
        </Button>
      </form>
      <p className='mt-3 text-sm text-muted-foreground'>
        {t('Compare both execution modes with the same model and resolution. OpenAI synchronous preview includes compatible image providers; Gemini uses its native synchronous API.')}
      </p>
      {preview.isPending && <LoadingState size='sm' />}
      {preview.error && (
        <ErrorState
          description={preview.error.message}
          onRetry={() => preview.mutate()}
        />
      )}
      {preview.data && (
        <div className='mt-4 space-y-3' aria-live='polite'>
          <p className='text-sm break-words'>
            {t('Routing groups')}:{' '}
            {(preview.data.routing_groups || []).join(', ') || '—'}
          </p>
          {(!preview.data.binding_found || preview.data.binding_key) && (
            <p className='text-sm break-words'>
              {t('Pool binding')}:{' '}
              {preview.data.binding_found
                ? preview.data.binding_key
                : t('Auto')}
            </p>
          )}
          {!!preview.data.reasons?.length && (
            <Alert>
              <AlertDescription>
                {preview.data.reasons
                  .map((reason) => t(imageLabel(reason)))
                  .join(' · ')}
              </AlertDescription>
            </Alert>
          )}
          <StaticDataTable
            data={rows}
            getRowKey={(candidate) =>
              JSON.stringify([candidate.group, candidate.channel_id])
            }
            columns={[
              {
                id: 'group',
                header: t('Group'),
                cell: (candidate) => candidate.group || '—',
              },
              {
                id: 'binding',
                header: t('Pool binding'),
                cell: (candidate) =>
                  candidate.binding_found ? candidate.binding_key : t('Auto'),
              },
              {
                id: 'channel',
                header: t('Channel'),
                cell: (candidate) =>
                  `#${candidate.channel_id} · ${candidate.name}`,
              },
              {
                id: 'provider',
                header: t('Provider'),
                cell: (candidate) => candidate.provider || '—',
              },
              {
                id: 'protocol',
                header: t('Protocol'),
                cell: (candidate) => candidate.protocol || '—',
              },
              {
                id: 'model',
                header: t('Upstream model'),
                cell: (candidate) => candidate.upstream_model,
              },
              {
                id: 'priority',
                header: t('Priority'),
                cell: (candidate) => candidate.priority,
              },
              {
                id: 'weight',
                header: t('Weight'),
                cell: (candidate) => candidate.weight,
              },
              {
                id: 'accounts',
                header: t('Enabled accounts'),
                cell: (candidate) => candidate.available_accounts,
              },
              {
                id: 'status',
                header: t('Status'),
                cell: (candidate) => (
                  <Badge variant={candidate.eligible ? 'secondary' : 'outline'}>
                    {t(
                      candidate.eligible
                        ? 'Eligible by configuration'
                        : 'Excluded'
                    )}
                  </Badge>
                ),
              },
              {
                id: 'reasons',
                header: t('Reason'),
                cell: (candidate) =>
                  candidate.reasons
                    .map((reason) => t(imageLabel(reason)))
                    .join(' · ') || '—',
              },
            ]}
          />
        </div>
      )}
    </SettingsCard>
  )
}
