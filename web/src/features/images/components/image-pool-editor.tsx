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
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useId, useMemo, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'

import { StaticDataTable } from '@/components/data-table'
import { Dialog } from '@/components/dialog'
import { MultiSelect } from '@/components/multi-select'
import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Combobox } from '@/components/ui/combobox'
import { Field, FieldDescription, FieldGroup } from '@/components/ui/field'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { handleServerError } from '@/lib/handle-server-error'

import { imageRequest } from '../api'
import { imageLabel } from '../lib/image-labels'
import type {
  ImageAdminConfiguration,
  ImageChannelPool,
  ImagePoolSelectionOptions,
} from '../types'
import { ImageGroupSelect } from './image-group-select'
import { ImageSelect } from './image-select'

export type ImagePoolBinding = ImageChannelPool & {
  channels: ImageChannelPool[]
  rowIds: (number | undefined)[]
}

type ImagePoolEditorProps = {
  config?: ImageAdminConfiguration
  binding: ImagePoolBinding | null
  canManage: boolean
  onSuccess: () => Promise<unknown>
  onClose: () => void
}

export function ImagePoolEditor(props: ImagePoolEditorProps) {
  const { t } = useTranslation()
  const queryClient = useQueryClient()
  const formId = useId()
  const weightDescriptionId = useId()
  const [copying, setCopying] = useState(false)
  const editing = !!props.binding && !copying
  const [group, setGroup] = useState(props.binding?.group || 'default')
  const [platform, setPlatform] = useState(props.binding?.platform || 'openai')
  const [mode, setMode] = useState(props.binding?.mode || 'resolution')
  const [model, setModel] = useState(props.binding?.model || '')
  const [resolution, setResolution] = useState(
    props.binding?.resolution || '1K'
  )
  const [channelIds, setChannelIds] = useState<number[]>(
    () => props.binding?.channels.map((row) => row.channel_id) || []
  )
  const [weights, setWeights] = useState<Record<number, number>>(() =>
    Object.fromEntries(
      props.binding?.channels.map((row) => [
        row.channel_id,
        Math.max(0, row.priority),
      ]) || []
    )
  )
  const targetBindings = (props.config?.pools || []).filter(
    (binding) =>
      binding.group === group.trim() &&
      binding.platform === platform &&
      binding.mode === mode &&
      binding.model === (mode === 'resolution' ? '' : model) &&
      binding.resolution === (mode === 'model' ? '' : resolution)
  )
  const targetConflict = !editing && targetBindings.length > 0
  const poolOptions = useQuery({
    queryKey: ['image-pool-options', group, platform],
    queryFn: ({ signal }) => {
      const params = new URLSearchParams({ group, platform })
      return imageRequest<ImagePoolSelectionOptions>(
        `/api/option/images/pool/options?${params}`,
        'GET',
        undefined,
        signal
      )
    },
    enabled: group.trim().length > 0 && platform.length > 0,
  })
  const modelOptions = useMemo(() => {
    const options = (poolOptions.data?.models || []).map((item) => ({
      value: item.id,
      label:
        item.media_types && item.media_types.length > 0
          ? `${item.label} · ${item.media_types.map((media) => t(media === 'video' ? 'Video' : 'Images')).join(' + ')}`
          : item.label,
    }))
    if (model && !options.some((option) => option.value === model)) {
      options.push({ value: model, label: `${model} · ${t('Unavailable')}` })
    }
    return options
  }, [model, poolOptions.data?.models, t])
  const availableModelIdSet = useMemo(
    () => new Set((poolOptions.data?.models || []).map((item) => item.id)),
    [poolOptions.data?.models]
  )
  const availableChannelIds = useMemo(() => {
    if (mode === 'resolution') {
      return (poolOptions.data?.channels || []).map((channel) => channel.id)
    }
    return (
      poolOptions.data?.models.find((item) => item.id === model)?.channel_ids ||
      []
    )
  }, [mode, model, poolOptions.data])
  const availableChannelIdSet = useMemo(
    () => new Set(availableChannelIds),
    [availableChannelIds]
  )
  const channelOptions = useMemo(() => {
    const channelsById = new Map(
      (poolOptions.data?.channels || []).map((channel) => [channel.id, channel])
    )
    const ids = new Set(availableChannelIds)
    for (const channelId of channelIds) {
      ids.add(channelId)
    }
    return [...ids].map((channelId) => {
      const channel = channelsById.get(channelId)
      return {
        value: String(channelId),
        label: channel
          ? `#${channelId} · ${channel.name}`
          : `#${channelId} · ${t('Unavailable')}`,
      }
    })
  }, [availableChannelIds, channelIds, poolOptions.data?.channels, t])
  const save = useMutation({
    mutationFn: async () => {
      if (targetConflict) {
        throw new Error(
          t(
            'A binding already exists for this target. Edit that binding instead.'
          )
        )
      }
      if (editing && props.binding?.rowIds.some((id) => !id)) {
        throw new Error(
          t('The binding has changed. Reload the configuration before saving.')
        )
      }
      if (mode !== 'resolution' && !model) {
        throw new Error(t('Model name is required'))
      }
      if (mode !== 'resolution' && !availableModelIdSet.has(model)) {
        throw new Error(
          t('A selected model no longer exists. Reload the model list.')
        )
      }
      if (channelIds.some((id) => !availableChannelIdSet.has(id))) {
        throw new Error(t('Invalid channel IDs'))
      }
      await imageRequest('/api/option/images/pool', 'PUT', {
        expected_ids: editing ? props.binding?.rowIds : [],
        group: group.trim(),
        platform,
        mode,
        model: mode === 'resolution' ? '' : model,
        resolution: mode === 'model' ? '' : resolution,
        channels: channelIds.map((id) => ({
          id,
          priority: weights[id] ?? 0,
        })),
      })
    },
    onSuccess: async () => {
      await Promise.all([
        props.onSuccess(),
        queryClient.invalidateQueries({ queryKey: ['image-capabilities'] }),
      ])
      toast.success(t('Saved'))
      props.onClose()
    },
    onError: (error) => handleServerError(error, t('Image request failed')),
  })
  const busy = save.isPending
  return (
    <Dialog
      open
      onOpenChange={(open, details) => {
        if (open) return
        if (busy) {
          details.cancel()
          return
        }
        props.onClose()
      }}
      title={
        editing ? t('Edit channel pool binding') : t('Add channel pool binding')
      }
      description={t(
        'Choose channels already enabled for this group and model. An empty list explicitly closes this binding.'
      )}
      contentClassName='sm:max-w-3xl'
      showCloseButton={!busy}
      footer={
        <>
          {editing && props.canManage && (
            <Button
              type='button'
              variant='outline'
              disabled={busy}
              onClick={() => setCopying(true)}
            >
              {t('Copy binding')}
            </Button>
          )}
          <Button
            type='button'
            variant='outline'
            disabled={busy}
            onClick={props.onClose}
          >
            {t('Cancel')}
          </Button>
          <Button
            type='submit'
            form={formId}
            aria-busy={busy}
            disabled={
              busy ||
              !props.canManage ||
              poolOptions.isLoading ||
              poolOptions.isError ||
              targetConflict ||
              !group.trim()
            }
          >
            {t('Save')}
          </Button>
        </>
      }
    >
      <form
        id={formId}
        onSubmit={(event) => {
          event.preventDefault()
          if (!busy && props.canManage && !targetConflict) save.mutate()
        }}
      >
        <fieldset disabled={busy || !props.canManage} className='min-w-0'>
          <FieldGroup className='grid grid-cols-1 gap-4 sm:grid-cols-2'>
            <ImageGroupSelect
              config={props.config}
              value={group}
              onChange={(value) => {
                setGroup(value)
                setModel('')
                setChannelIds([])
                setWeights({})
              }}
              disabled={busy || !props.canManage || editing}
            />
            <ImageSelect
              disabled={busy || !props.canManage || editing}
              label={t('Platform')}
              value={platform}
              options={(
                props.config?.media_providers ||
                props.config?.image_providers || ['openai', 'gemini']
              ).map((value) => ({
                value,
                label: value,
              }))}
              onChange={(value) => {
                setPlatform(value)
                setModel('')
                setChannelIds([])
                setWeights({})
              }}
            />
            <ImageSelect
              disabled={busy || !props.canManage || editing}
              label={t('Pool binding')}
              value={mode}
              options={['resolution', 'model', 'model_resolution'].map(
                (value) => ({
                  value,
                  label: t(imageLabel(value)),
                })
              )}
              onChange={(value) => {
                setMode(value)
                if (value === 'resolution') setModel('')
                setChannelIds([])
                setWeights({})
              }}
            />
            {mode !== 'resolution' && (
              <Field>
                <Label htmlFor='pool-model'>{t('Model')}</Label>
                <Combobox
                  id='pool-model'
                  options={modelOptions}
                  value={model || null}
                  onValueChange={(value) => {
                    setModel(value || '')
                    setChannelIds([])
                    setWeights({})
                  }}
                  placeholder={
                    poolOptions.isLoading ? t('Loading...') : t('Select model')
                  }
                  emptyText={t('No available models')}
                  disabled={
                    busy ||
                    !props.canManage ||
                    editing ||
                    poolOptions.isLoading ||
                    poolOptions.isError
                  }
                  className='w-full'
                />
              </Field>
            )}
            {mode !== 'model' && (
              <ImageSelect
                disabled={busy || !props.canManage || editing}
                label={t('Resolution')}
                value={resolution}
                options={['1K', '2K', '4K'].map((value) => ({
                  value,
                  label: value,
                }))}
                onChange={setResolution}
              />
            )}
            <Field>
              <Label htmlFor='pool-channels'>{t('Channels')}</Label>
              <MultiSelect
                id='pool-channels'
                options={channelOptions}
                selected={channelIds.map(String)}
                onChange={(values) => {
                  const ids = values.map(Number)
                  setChannelIds(ids)
                  setWeights((previous) =>
                    Object.fromEntries(ids.map((id) => [id, previous[id] ?? 0]))
                  )
                }}
                placeholder={
                  poolOptions.isLoading ? t('Loading...') : t('Channels')
                }
                emptyText={t('No available channels')}
                disabled={
                  busy ||
                  !props.canManage ||
                  poolOptions.isLoading ||
                  poolOptions.isError ||
                  (mode !== 'resolution' && !model)
                }
              />
            </Field>
            {poolOptions.isError && (
              <p
                className='text-destructive text-sm sm:col-span-2'
                role='alert'
              >
                {poolOptions.error.message}
              </p>
            )}
            <div className='sm:col-span-2'>
              {channelIds.length > 0 && (
                <StaticDataTable
                  data={[...new Set(channelIds)]}
                  getRowKey={(id) => String(id)}
                  columns={[
                    { id: 'channel', header: t('Channel'), cell: (id) => id },
                    {
                      id: 'weight',
                      header: t('Weight'),
                      cell: (id) => (
                        <Input
                          aria-label={`${t('Weight')} ${id}`}
                          aria-describedby={weightDescriptionId}
                          type='number'
                          min={0}
                          max={1000000}
                          value={weights[id] ?? 0}
                          onChange={(event) =>
                            setWeights((previous) => ({
                              ...previous,
                              [id]: Number(event.target.value),
                            }))
                          }
                        />
                      ),
                    },
                  ]}
                />
              )}
              <FieldDescription id={weightDescriptionId} className='mb-3'>
                {t(
                  'Pool weights share traffic proportionally. All zero means equal traffic; otherwise, zero-weight channels are excluded.'
                )}
              </FieldDescription>
            </div>
          </FieldGroup>
          <Alert
            className='mt-4'
            role={targetConflict ? 'alert' : 'status'}
            variant={targetConflict ? 'destructive' : 'default'}
          >
            <AlertTitle>{t('Binding impact')}</AlertTitle>
            <AlertDescription>
              <p className='break-words'>
                {group} / {platform} / {t(imageLabel(mode))} /{' '}
                {model || t('All Models')} /{' '}
                {mode === 'model' ? t('All resolutions') : resolution}
              </p>
              <p>
                {t('Channels: {{before}} → {{after}}', {
                  before: targetBindings.filter(
                    (binding) => binding.channel_id > 0
                  ).length,
                  after: channelIds.length,
                })}
              </p>
              {editing && (
                <p>
                  {t(
                    'The binding target is fixed while editing. Copy the binding to use another target.'
                  )}
                </p>
              )}
              {copying && (
                <p>{t('The original binding will remain unchanged.')}</p>
              )}
              {targetConflict && (
                <p>
                  {t(
                    'A binding already exists for this target. Edit that binding instead.'
                  )}
                </p>
              )}
              {channelIds.length === 0 && (
                <p>
                  {t(
                    'Saving an empty channel list closes this binding to new requests.'
                  )}
                </p>
              )}
            </AlertDescription>
          </Alert>
        </fieldset>
      </form>
      {save.isError && (
        <p role='alert' className='text-destructive mt-3 text-sm'>
          {save.error.message}
        </p>
      )}
    </Dialog>
  )
}
