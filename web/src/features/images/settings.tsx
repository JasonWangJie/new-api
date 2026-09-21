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
import { useQuery } from '@tanstack/react-query'
import { useMemo, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'

import { StaticDataTable } from '@/components/data-table'
import { MultiSelect } from '@/components/multi-select'
import { Button } from '@/components/ui/button'
import { Checkbox } from '@/components/ui/checkbox'
import { Combobox } from '@/components/ui/combobox'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Tabs, TabsList, TabsTrigger } from '@/components/ui/tabs'
import { Textarea } from '@/components/ui/textarea'
import { SettingsCard } from '@/features/system-settings/components/settings-card'
import { hasPermission } from '@/lib/admin-permissions'
import { useAuthStore } from '@/stores/auth-store'

import { imageRequest } from './api'
import { ImagePolicyExampleDialog } from './components/image-policy-example-dialog'
import { ImageSelect } from './components/image-select'
import { imageLabel } from './lib/image-labels'
import { MediaSettingsCard } from './media-settings'

type StorageProfile = {
  profile_id: string
  class: string
  backend: string
  provider: string
  root: string
  endpoint: string
  bucket: string
  region: string
  prefix: string
  active: boolean
  path_style: boolean
}
type ImageConfiguration = {
  image_providers?: string[]
  media_providers?: string[]
  runtime: Record<string, string | number | boolean>
  storage_profiles: StorageProfile[]
  policies: {
    group: string
    platform: string
    pool_mode: string
    enabled: boolean
    async_enabled: boolean
    models: string
    version: number
  }[]
  pools: ImageChannelPool[]
  storage_providers: string[]
}
type ImageChannelPool = {
  binding_key: string
  group: string
  platform: string
  mode: string
  model: string
  resolution: string
  channel_id: number
  priority: number
}
type ImagePoolSelectionOptions = {
  models: { id: string; label: string; channel_ids: number[] }[]
  channels: { id: number; name: string }[]
}
const RUNTIME_FIELDS = [
  { key: 'worker_concurrency', label: 'Workers' },
  { key: 'image_concurrency', label: 'Image concurrency' },
  { key: 'worker_lease_seconds', label: 'Lease duration (seconds)' },
  { key: 'execution_timeout_seconds', label: 'Execution timeout (seconds)' },
  {
    key: 'account_attempt_timeout_seconds',
    label: 'Attempt timeout (seconds)',
  },
  { key: 'signed_url_expiry_seconds', label: 'Signed URL lifetime (seconds)' },
  { key: 'input_retention_hours', label: 'Input retention (hours)' },
  { key: 'task_retention_days', label: 'Task retention (days)' },
  { key: 'result_retention_days', label: 'Result retention (days)' },
  { key: 'reference_fetch_max_retries', label: 'Reference retries' },
  { key: 'storage_retry_attempts', label: 'Storage retries' },
  { key: 'billing_retry_attempts', label: 'Billing retries' },
] as const

export function ImageSettings() {
  const { t } = useTranslation()
  const user = useAuthStore((state) => state.auth.user)
  const allowed = hasPermission(user, 'image_config', 'read')
  const configuration = useQuery({
    queryKey: ['image-configuration', user?.id],
    queryFn: ({ signal }) =>
      imageRequest<ImageConfiguration>(
        '/api/option/images',
        'GET',
        undefined,
        signal
      ),
    enabled: allowed,
  })
  const [tab, setTab] = useState('runtime')
  if (!allowed) {
    return (
      <p className='text-muted-foreground text-sm'>
        {t('Image settings require administrator storage permissions.')}
      </p>
    )
  }
  if (configuration.isError) {
    return <p role='alert'>{configuration.error.message}</p>
  }
  if (!configuration.data) return <p role='status'>{t('Loading...')}</p>
  const config = configuration.data
  return (
    <div className='space-y-4'>
      <Tabs value={tab} onValueChange={(value) => setTab(String(value))}>
        <TabsList className='h-auto flex-wrap'>
          {[
            { id: 'runtime', label: 'Runtime' },
            { id: 'storage', label: 'Image storage' },
            { id: 'policy', label: 'Platform policies' },
            { id: 'pool', label: 'Channel pools' },
          ].map((item) => (
            <TabsTrigger key={item.id} value={item.id}>
              {t(item.label)}
            </TabsTrigger>
          ))}
        </TabsList>
      </Tabs>
      {tab === 'runtime' && <MediaSettingsCard />}
      {tab === 'runtime' && (
        <ImageRuntimeForm
          key={JSON.stringify(config.runtime)}
          initial={config.runtime}
          onSuccess={() => configuration.refetch()}
        />
      )}{' '}
      {tab === 'storage' && (
        <ImageStorageForm
          config={config}
          onSuccess={() => configuration.refetch()}
        />
      )}{' '}
      {tab === 'policy' && (
        <ImagePolicyForm
          config={config}
          onSuccess={() => configuration.refetch()}
        />
      )}{' '}
      {tab === 'pool' && (
        <ImagePoolForm
          config={configuration.data}
          onSuccess={() => configuration.refetch()}
        />
      )}
    </div>
  )
}

function ImageRuntimeForm({
  initial,
  onSuccess,
}: {
  initial: ImageConfiguration['runtime']
  onSuccess: () => Promise<unknown>
}) {
  const { t } = useTranslation()
  const [values, setValues] = useState(initial)
  const [advanced, setAdvanced] = useState('')
  const [busy, setBusy] = useState(false)
  const save = async () => {
    setBusy(true)
    try {
      const cfg = advanced
        ? (JSON.parse(advanced) as ImageConfiguration['runtime'])
        : values
      await imageRequest('/api/option/images/runtime', 'PUT', cfg)
      await onSuccess()
      toast.success(t('Saved'))
    } catch (error) {
      toast.error(
        error instanceof Error ? error.message : t('Image request failed')
      )
    } finally {
      setBusy(false)
    }
  }
  return (
    <SettingsCard
      title={t('Image runtime')}
      description={t(
        'Async generation and automatic server archiving are disabled by default.'
      )}
    >
      <form
        className='space-y-5'
        onSubmit={(event) => {
          event.preventDefault()
          void save()
        }}
      >
        <div className='grid gap-3 sm:grid-cols-2'>
          {[
            { key: 'async_enabled', label: 'Enable async image generation' },
            {
              key: 'auto_archive_to_library',
              label: 'Automatically archive async results',
            },
            {
              key: 'image_circuit_breaker_enabled',
              label: 'Enable image circuit breaker',
            },
            { key: 'prompt_preview_enabled', label: 'Show prompt summaries' },
          ].map(({ key, label }) => (
            <div key={key} className='flex items-center gap-2'>
              <Checkbox
                id={key}
                checked={!!values[key]}
                onCheckedChange={(checked) => {
                  setValues((previous) => ({ ...previous, [key]: checked }))
                  setAdvanced('')
                }}
                disabled={busy}
              />
              <Label htmlFor={key}>{t(label)}</Label>
            </div>
          ))}
        </div>
        <div className='grid gap-3 sm:grid-cols-2 lg:grid-cols-3'>
          {RUNTIME_FIELDS.map(({ key, label }) => (
            <div key={key} className='space-y-2'>
              <Label htmlFor={key}>{t(label)}</Label>
              <Input
                id={key}
                type='number'
                min={
                  key.includes('retries') || key.includes('attempts') ? 0 : 1
                }
                value={Number(values[key])}
                onChange={(event) => {
                  setValues((previous) => ({
                    ...previous,
                    [key]: event.target.valueAsNumber,
                  }))
                  setAdvanced('')
                }}
                disabled={busy}
              />
            </div>
          ))}
        </div>
        <div className='grid gap-3 sm:grid-cols-2'>
          {['openai', 'gemini'].map((platform) => (
            <ImageSelect
              key={platform}
              label={`${platform} · ${t('Reference transport')}`}
              value={String(values[`${platform}_reference_transport_mode`])}
              options={[
                'passthrough',
                'local',
                'passthrough_fallback_local',
              ].map((value) => ({ value, label: t(imageLabel(value)) }))}
              onChange={(value) => {
                setValues((previous) => ({
                  ...previous,
                  [`${platform}_reference_transport_mode`]: value,
                }))
                setAdvanced('')
              }}
              disabled={busy}
            />
          ))}
        </div>
        <Button
          type='button'
          variant='outline'
          onClick={() =>
            setAdvanced(advanced ? '' : JSON.stringify(values, null, 2))
          }
        >
          {t('Advanced runtime settings')}
        </Button>
        {advanced && (
          <div className='space-y-2'>
            <Label htmlFor='image-runtime-advanced'>
              {t('Complete runtime configuration')}
            </Label>
            <Textarea
              id='image-runtime-advanced'
              className='min-h-72 font-mono text-xs'
              value={advanced}
              onChange={(event) => setAdvanced(event.target.value)}
              disabled={busy}
            />
          </div>
        )}
        <div>
          <Button type='submit' disabled={busy}>
            {t('Save')}
          </Button>
        </div>
      </form>
    </SettingsCard>
  )
}

function ImageStorageForm({
  config,
  onSuccess,
}: {
  config: ImageConfiguration
  onSuccess: () => Promise<unknown>
}) {
  const { t } = useTranslation()
  const [profile, setProfile] = useState<StorageProfile>({
    profile_id: '',
    class: 'temporary',
    backend: 'local',
    provider: 'local',
    root: '',
    endpoint: '',
    bucket: '',
    region: 'us-east-1',
    prefix: '',
    active: true,
    path_style: false,
  })
  const [accessKey, setAccessKey] = useState('')
  const [secretKey, setSecretKey] = useState('')
  const [busy, setBusy] = useState(false)
  const save = async () => {
    setBusy(true)
    try {
      await imageRequest('/api/option/images/storage', 'POST', {
        profile,
        credentials:
          profile.backend === 's3'
            ? { access_key: accessKey, secret_key: secretKey }
            : undefined,
      })
      setAccessKey('')
      setSecretKey('')
      await onSuccess()
      toast.success(t('Saved'))
    } catch (error) {
      toast.error(
        error instanceof Error ? error.message : t('Image request failed')
      )
    } finally {
      setBusy(false)
    }
  }
  const test = async (id: string) => {
    setBusy(true)
    try {
      await imageRequest(`/api/option/images/storage/${id}/test`, 'POST', {})
      toast.success(t('Storage write, read and delete verified'))
    } catch (error) {
      toast.error(
        error instanceof Error ? error.message : t('Image request failed')
      )
    } finally {
      setBusy(false)
    }
  }
  return (
    <SettingsCard
      title={t('Image storage')}
      description={t(
        'Local files use the deployment directory and year/month/day folders. Storage revisions remain available for existing images.'
      )}
    >
      <div className='space-y-5'>
        <StaticDataTable
          data={config.storage_profiles}
          getRowKey={(item) => item.profile_id}
          columns={[
            {
              id: 'class',
              header: t('Purpose'),
              cell: (item) => `${t(imageLabel(item.class))} · ${item.provider}`,
            },
            {
              id: 'location',
              header: t('Location'),
              cell: (item) => (
                <span className='text-xs break-all'>
                  {item.root || `${item.endpoint} / ${item.bucket}`}
                </span>
              ),
            },
            {
              id: 'active',
              header: t('Active'),
              cell: (item) => t(item.active ? 'Yes' : 'No'),
            },
            {
              id: 'test',
              header: t('Actions'),
              cell: (item) => (
                <Button
                  size='sm'
                  variant='outline'
                  disabled={busy}
                  onClick={() => void test(item.profile_id)}
                >
                  {t('Test storage')}
                </Button>
              ),
            },
          ]}
        />
        <form
          className='space-y-4'
          onSubmit={(event) => {
            event.preventDefault()
            void save()
          }}
        >
          <div className='grid gap-3 sm:grid-cols-2'>
            <ImageSelect
              label={t('Purpose')}
              value={profile.class}
              options={['temporary', 'durable'].map((value) => ({
                value,
                label: t(imageLabel(value)),
              }))}
              onChange={(value) =>
                setProfile((previous) => ({ ...previous, class: value }))
              }
            />
            <ImageSelect
              label={t('Provider')}
              value={profile.provider}
              options={config.storage_providers.map((value) => ({
                value,
                label: value,
              }))}
              onChange={(value) =>
                setProfile((previous) => ({
                  ...previous,
                  provider: value,
                  backend: value === 'local' ? 'local' : 's3',
                }))
              }
            />
            {(profile.backend === 'local'
              ? [{ key: 'root', label: 'Storage directory (optional)' }]
              : [
                  { key: 'endpoint', label: 'S3 endpoint' },
                  { key: 'bucket', label: 'Bucket' },
                  { key: 'region', label: 'Region' },
                  { key: 'prefix', label: 'Object prefix' },
                ]
            ).map(({ key, label }) => (
              <div key={key} className='space-y-2'>
                <Label htmlFor={`image-storage-${key}`}>{t(label)}</Label>
                <Input
                  id={`image-storage-${key}`}
                  value={profile[key as keyof StorageProfile] as string}
                  onChange={(event) =>
                    setProfile((previous) => ({
                      ...previous,
                      [key]: event.target.value,
                    }))
                  }
                  disabled={busy}
                />
              </div>
            ))}
          </div>
          {profile.backend === 's3' && (
            <div className='grid gap-3 sm:grid-cols-2'>
              <div className='space-y-2'>
                <Label htmlFor='image-access-key'>{t('Access key')}</Label>
                <Input
                  id='image-access-key'
                  type='password'
                  autoComplete='off'
                  value={accessKey}
                  onChange={(event) => setAccessKey(event.target.value)}
                  disabled={busy}
                />
              </div>
              <div className='space-y-2'>
                <Label htmlFor='image-secret-key'>{t('Secret key')}</Label>
                <Input
                  id='image-secret-key'
                  type='password'
                  autoComplete='off'
                  value={secretKey}
                  onChange={(event) => setSecretKey(event.target.value)}
                  disabled={busy}
                />
              </div>
              <div className='flex items-center gap-2'>
                <Checkbox
                  id='image-path-style'
                  checked={profile.path_style}
                  onCheckedChange={(checked) =>
                    setProfile((previous) => ({
                      ...previous,
                      path_style: checked,
                    }))
                  }
                />
                <Label htmlFor='image-path-style'>
                  {t('Path-style access')}
                </Label>
              </div>
            </div>
          )}
          <Button type='submit' disabled={busy}>
            {t('Save storage revision')}
          </Button>
        </form>
      </div>
    </SettingsCard>
  )
}

function ImagePolicyForm({
  config,
  onSuccess,
}: {
  config: ImageConfiguration
  onSuccess: () => Promise<unknown>
}) {
  const { t } = useTranslation()
  const [group, setGroup] = useState('default')
  const [platform, setPlatform] = useState('openai')
  const [mode, setMode] = useState('resolution')
  const [enabled, setEnabled] = useState(false)
  const [asyncEnabled, setAsyncEnabled] = useState(false)
  const [models, setModels] = useState(
    JSON.stringify(
      [
        {
          id: 'gpt-image-2',
          label: 'gpt-image-2',
          qualities: ['low', 'medium', 'high'],
          resolutions: ['1K', '2K', '4K'],
          formats: ['png', 'jpeg', 'webp'],
          backgrounds: ['auto', 'opaque', 'transparent'],
          max_output_images: 128,
          max_reference_images: 8,
        },
      ],
      null,
      2
    )
  )
  const [busy, setBusy] = useState(false)
  const save = async () => {
    setBusy(true)
    try {
      JSON.parse(models)
      const current = config.policies.find(
        (item) => item.group === group && item.platform === platform
      )
      await imageRequest('/api/option/images/policy', 'PUT', {
        group,
        platform,
        pool_mode: mode,
        enabled,
        async_enabled: asyncEnabled,
        models,
        version: current?.version || 0,
      })
      await onSuccess()
      toast.success(t('Saved'))
    } catch (error) {
      toast.error(
        error instanceof Error ? error.message : t('Image request failed')
      )
    } finally {
      setBusy(false)
    }
  }
  return (
    <SettingsCard
      title={t('Platform policies')}
      description={t(
        'Model names are exact and case-sensitive. Empty channel pools remain closed.'
      )}
    >
      <form
        className='space-y-4'
        onSubmit={(event) => {
          event.preventDefault()
          void save()
        }}
      >
        <div className='grid gap-3 sm:grid-cols-3'>
          <div className='space-y-2'>
            <Label htmlFor='image-policy-group'>{t('Group')}</Label>
            <Input
              id='image-policy-group'
              value={group}
              onChange={(event) => setGroup(event.target.value)}
            />
          </div>
          <ImageSelect
            label={t('Platform')}
            value={platform}
            options={(
              config?.media_providers ||
              config?.image_providers || ['openai', 'gemini']
            ).map((value) => ({
              value,
              label: value,
            }))}
            onChange={setPlatform}
          />
          <ImageSelect
            label={t('Pool binding')}
            value={mode}
            options={['resolution', 'model', 'model_resolution'].map(
              (value) => ({ value, label: t(imageLabel(value)) })
            )}
            onChange={setMode}
          />
        </div>
        <div className='flex flex-wrap gap-4'>
          <div className='flex items-center gap-2'>
            <Checkbox
              id='policy-enabled'
              checked={enabled}
              onCheckedChange={setEnabled}
            />
            <Label htmlFor='policy-enabled'>{t('Enable image platform')}</Label>
          </div>
          <div className='flex items-center gap-2'>
            <Checkbox
              id='policy-async'
              checked={asyncEnabled}
              onCheckedChange={setAsyncEnabled}
            />
            <Label htmlFor='policy-async'>
              {t('Use asynchronous execution')}
            </Label>
          </div>
        </div>
        <div className='flex flex-wrap items-center justify-between gap-2'>
          <Label htmlFor='image-model-catalog'>{t('Model capabilities')}</Label>
          <ImagePolicyExampleDialog platform={platform} />
        </div>
        <Textarea
          id='image-model-catalog'
          className='min-h-64 font-mono text-xs'
          value={models}
          onChange={(event) => setModels(event.target.value)}
        />
        <Button disabled={busy} type='submit'>
          {t('Save')}
        </Button>
        <StaticDataTable
          data={config.policies}
          getRowKey={(item) => `${item.group}:${item.platform}`}
          columns={[
            { id: 'group', header: t('Group'), cell: (item) => item.group },
            {
              id: 'platform',
              header: t('Platform'),
              cell: (item) => item.platform,
            },
            {
              id: 'mode',
              header: t('Execution mode'),
              cell: (item) =>
                t(item.async_enabled ? 'Asynchronous' : 'Realtime'),
            },
            {
              id: 'edit',
              header: t('Actions'),
              cell: (item) => (
                <Button
                  type='button'
                  size='sm'
                  variant='outline'
                  onClick={() => {
                    setGroup(item.group)
                    setPlatform(item.platform)
                    setMode(item.pool_mode)
                    setModels(item.models)
                    setEnabled(item.enabled)
                    setAsyncEnabled(item.async_enabled)
                  }}
                >
                  {t('Edit')}
                </Button>
              ),
            },
          ]}
        />
      </form>
    </SettingsCard>
  )
}

export function ImagePoolForm({
  config,
  onSuccess,
}: {
  config?: ImageConfiguration
  onSuccess: () => Promise<unknown>
}) {
  const { t } = useTranslation()
  const [group, setGroup] = useState('default')
  const [platform, setPlatform] = useState('openai')
  const [mode, setMode] = useState('resolution')
  const [model, setModel] = useState('')
  const [resolution, setResolution] = useState('1K')
  const [channelIds, setChannelIds] = useState<number[]>([])
  const [priorities, setPriorities] = useState<Record<number, number>>({})
  const [busy, setBusy] = useState(false)
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
      label: item.label,
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
  const bindings = [
    ...new Set(config?.pools.map((pool) => pool.binding_key) || []),
  ].flatMap((key) => {
    const rows = config?.pools.filter((pool) => pool.binding_key === key) || []
    const first = rows[0]
    if (!first) return []
    return [{ ...first, channels: rows.filter((pool) => pool.channel_id > 0) }]
  })
  const save = async () => {
    setBusy(true)
    try {
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
        group,
        platform,
        mode,
        model: mode === 'resolution' ? '' : model,
        resolution: mode === 'model' ? '' : resolution,
        channels: channelIds.map((id) => ({
          id,
          priority: priorities[id] || 0,
        })),
      })
      await onSuccess()
      toast.success(t('Saved'))
    } catch (error) {
      toast.error(
        error instanceof Error ? error.message : t('Image request failed')
      )
    } finally {
      setBusy(false)
    }
  }
  return (
    <SettingsCard
      title={t('Channel pools')}
      description={t(
        'Choose channels already enabled for this group and model. An empty list explicitly closes this binding.'
      )}
    >
      <form
        className='grid gap-4 sm:grid-cols-2'
        onSubmit={(event) => {
          event.preventDefault()
          void save()
        }}
      >
        <div className='space-y-2'>
          <Label htmlFor='pool-group'>{t('Group')}</Label>
          <Input
            id='pool-group'
            value={group}
            onChange={(event) => {
              setGroup(event.target.value)
              setModel('')
              setChannelIds([])
              setPriorities({})
            }}
          />
        </div>
        <ImageSelect
          label={t('Platform')}
          value={platform}
          options={(
            config?.media_providers ||
            config?.image_providers || ['openai', 'gemini']
          ).map((value) => ({
            value,
            label: value,
          }))}
          onChange={(value) => {
            setPlatform(value)
            setModel('')
            setChannelIds([])
            setPriorities({})
          }}
        />
        <ImageSelect
          label={t('Pool binding')}
          value={mode}
          options={['resolution', 'model', 'model_resolution'].map((value) => ({
            value,
            label: t(imageLabel(value)),
          }))}
          onChange={(value) => {
            setMode(value)
            if (value === 'resolution') setModel('')
            setChannelIds([])
            setPriorities({})
          }}
        />
        {mode !== 'resolution' && (
          <div className='space-y-2'>
            <Label htmlFor='pool-model'>{t('Model')}</Label>
            <Combobox
              id='pool-model'
              options={modelOptions}
              value={model || null}
              onValueChange={(value) => {
                setModel(value || '')
                setChannelIds([])
                setPriorities({})
              }}
              placeholder={
                poolOptions.isLoading ? t('Loading...') : t('Select model')
              }
              emptyText={t('No available models')}
              disabled={poolOptions.isLoading || poolOptions.isError}
              className='w-full'
            />
          </div>
        )}
        {mode !== 'model' && (
          <ImageSelect
            label={t('Resolution')}
            value={resolution}
            options={['1K', '2K', '4K'].map((value) => ({
              value,
              label: value,
            }))}
            onChange={setResolution}
          />
        )}
        <div className='space-y-2'>
          <Label htmlFor='pool-channels'>{t('Channels')}</Label>
          <MultiSelect
            id='pool-channels'
            options={channelOptions}
            selected={channelIds.map(String)}
            onChange={(values) => {
              const ids = values.map(Number)
              setChannelIds(ids)
              setPriorities((previous) =>
                Object.fromEntries(ids.map((id) => [id, previous[id] || 0]))
              )
            }}
            placeholder={
              poolOptions.isLoading ? t('Loading...') : t('Channels')
            }
            emptyText={t('No available channels')}
            disabled={
              poolOptions.isLoading ||
              poolOptions.isError ||
              (mode !== 'resolution' && !model)
            }
          />
        </div>
        {poolOptions.isError && (
          <p className='text-destructive text-sm sm:col-span-2' role='alert'>
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
                  id: 'priority',
                  header: t('Priority'),
                  cell: (id) => (
                    <Input
                      aria-label={`${t('Priority')} ${id}`}
                      type='number'
                      min={-1000000}
                      max={1000000}
                      value={priorities[id] || 0}
                      onChange={(event) =>
                        setPriorities((previous) => ({
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
          <Button
            disabled={
              busy ||
              poolOptions.isLoading ||
              poolOptions.isError ||
              !group.trim()
            }
            type='submit'
          >
            {t('Save')}
          </Button>
        </div>
      </form>
      <StaticDataTable
        data={bindings}
        getRowKey={(binding) => binding.binding_key}
        columns={[
          { id: 'group', header: t('Group'), cell: (binding) => binding.group },
          {
            id: 'platform',
            header: t('Platform'),
            cell: (binding) => binding.platform,
          },
          {
            id: 'mode',
            header: t('Pool binding'),
            cell: (binding) => t(imageLabel(binding.mode)),
          },
          {
            id: 'model',
            header: t('Model'),
            cell: (binding) => binding.model || '—',
          },
          {
            id: 'resolution',
            header: t('Resolution'),
            cell: (binding) => binding.resolution || '—',
          },
          {
            id: 'channels',
            header: t('Channel'),
            cell: (binding) =>
              binding.channels
                .map((row) => `${row.channel_id} (${row.priority})`)
                .join(', ') || t('Disabled'),
          },
          {
            id: 'edit',
            header: t('Actions'),
            cell: (binding) => (
              <Button
                type='button'
                size='sm'
                variant='outline'
                onClick={() => {
                  setGroup(binding.group)
                  setPlatform(binding.platform)
                  setMode(binding.mode)
                  setModel(binding.model)
                  setResolution(binding.resolution || '1K')
                  setChannelIds(binding.channels.map((row) => row.channel_id))
                  setPriorities(
                    Object.fromEntries(
                      binding.channels.map((row) => [
                        row.channel_id,
                        row.priority,
                      ])
                    )
                  )
                }}
              >
                {t('Edit')}
              </Button>
            ),
          },
        ]}
      />
    </SettingsCard>
  )
}
