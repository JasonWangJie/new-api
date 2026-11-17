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
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useEffect, useState } from 'react'
import { useFieldArray, useForm, useWatch } from 'react-hook-form'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'

import { ConfirmDialog } from '@/components/confirm-dialog'
import { StaticDataTable } from '@/components/data-table'
import { Button } from '@/components/ui/button'
import { Checkbox } from '@/components/ui/checkbox'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { SettingsCard } from '@/features/system-settings/components/settings-card'
import { hasPermission } from '@/lib/admin-permissions'
import { handleServerError } from '@/lib/handle-server-error'
import { useAuthStore } from '@/stores/auth-store'

import { imageRequest } from '../api'
import {
  cleanupConfigSchema,
  cleanupIsActive,
  cleanupJobsPath,
  cleanupRequestSchema,
  imageCleanupChannelOptions,
  translateImageCleanupMessage,
  type CleanupConfig,
  type CleanupJob,
  type CleanupRequest,
  type CleanupRequestForm,
} from '../lib/image-cleanup'
import { imageLabel } from '../lib/image-labels'
import type { CursorPage, ImageStorageProfile } from '../types'
import { ImageSelect } from './image-select'

export function ImageCleanupSettings(props: {
  profiles: ImageStorageProfile[]
}) {
  const { t } = useTranslation()
  const user = useAuthStore((state) => state.auth.user)
  const canRead = hasPermission(user, 'image_moderation', 'read')
  const canManage = hasPermission(user, 'image_moderation', 'manage')
  const config = useQuery({
    queryKey: ['image-cleanup-config', user?.id],
    enabled: canRead,
    queryFn: ({ signal }) =>
      imageRequest<CleanupConfig>(
        '/api/admin/image-library/cleanup-config',
        'GET',
        undefined,
        signal
      ),
    refetchOnWindowFocus: false,
  })
  if (!canRead) return null
  return (
    <div className='space-y-4'>
      {config.isError && <p role='alert'>{config.error.message}</p>}
      {config.isPending && <p role='status'>{t('Loading...')}</p>}
      {config.data && (
        <ImageCleanupConfigForm
          key={config.data.revision}
          initial={config.data}
          profiles={props.profiles}
          canManage={canManage}
          refreshing={config.isFetching}
          onSaved={() => config.refetch()}
        />
      )}
      <ImageCleanupPanel profiles={props.profiles} />
    </div>
  )
}

export function ImageCleanupConfigForm(props: {
  initial: CleanupConfig
  profiles: ImageStorageProfile[]
  canManage: boolean
  refreshing?: boolean
  onSaved: () => Promise<unknown>
}) {
  const { t } = useTranslation()
  const [saveError, setSaveError] = useState('')
  const [intervalUnits, setIntervalUnits] = useState<
    Record<string, 'minutes' | 'hours'>
  >({})
  const form = useForm<CleanupConfig>({
    resolver: zodResolver(cleanupConfigSchema),
    defaultValues: props.initial,
  })
  const policies = useFieldArray({
    control: form.control,
    name: 'policies',
    keyName: 'formKey',
  })
  const values = form.watch()
  const channels = imageCleanupChannelOptions(props.profiles)
  const save = useMutation({
    mutationFn: () => {
      const values = form.getValues()
      const writable = { ...values }
      delete writable.runtime
      return imageRequest<CleanupConfig>(
        '/api/admin/image-library/cleanup-config',
        'PUT',
        {
          ...writable,
          policies: values.policies.map((policy) => {
            const writablePolicy = { ...policy }
            delete writablePolicy.next_run_at
            return writablePolicy
          }),
        }
      )
    },
    onSuccess: async (saved) => {
      form.reset(saved)
      setSaveError('')
      await props.onSaved()
      toast.success(t('Saved'))
    },
    onError: (cause) => {
      setSaveError(
        cause instanceof Error ? cause.message : t('Image request failed')
      )
      handleServerError(cause)
    },
  })
  const disabled = !props.canManage || save.isPending
  return (
    <SettingsCard
      title={t('Scheduled image cleanup')}
      description={t(
        'Scheduled cleanup and new manual previews with a storage channel and image age ignore task and result retention periods. Active tasks, library images and published images remain protected.'
      )}
    >
      <section
        aria-label={t('Cleanup runtime status')}
        className='mb-4 space-y-2 rounded-lg border p-3 text-sm'
      >
        <div className='flex flex-wrap items-center justify-between gap-2'>
          <p>
            {t('Active cleanup workers')}:{' '}
            {props.initial.runtime?.active_workers ?? t('Unknown')}
          </p>
          <Button
            type='button'
            size='sm'
            variant='outline'
            disabled={props.refreshing || save.isPending}
            onClick={() => void props.onSaved()}
          >
            {t('Refresh')}
          </Button>
        </div>
        <p>
          {t('Cleanup pause reason')}:{' '}
          {props.initial.runtime?.pressure_reason
            ? translateImageCleanupMessage(
                props.initial.runtime.pressure_reason,
                t
              )
            : t('None')}
        </p>
        <p>
          {t('Last updated')}:{' '}
          {props.initial.runtime?.sampled_at
            ? new Date(props.initial.runtime.sampled_at * 1000).toLocaleString()
            : t('Unknown')}
        </p>
        <p className='text-muted-foreground'>
          {t(
            'This snapshot covers the responding node only. Other nodes may apply settings at different times.'
          )}
        </p>
      </section>
      <form
        className='space-y-4'
        onSubmit={form.handleSubmit(() => save.mutate())}
      >
        <div className='flex items-center gap-2'>
          <Checkbox
            id='cleanup-enabled'
            checked={values.enabled}
            onCheckedChange={(value) =>
              form.setValue('enabled', value === true)
            }
            disabled={disabled}
          />
          <Label htmlFor='cleanup-enabled'>
            {t('Enable scheduled cleanup')}
          </Label>
        </div>
        <p className='text-muted-foreground text-sm'>
          {t(
            'Manual jobs use the same resource limits. High resource use pauses cleanup; lowering concurrency lets current requests finish.'
          )}
        </p>
        <div className='grid gap-3 sm:grid-cols-2 lg:grid-cols-3'>
          {(
            [
              {
                key: 'concurrency',
                label: 'Cleanup concurrency',
                min: 1,
                max: 8,
              },
              {
                key: 'requests_per_second',
                label: 'Cleanup requests per second',
                min: 1,
                max: 20,
              },
              {
                key: 'batch_size',
                label: 'Cleanup scan batch size',
                min: 20,
                max: 200,
              },
              {
                key: 'scan_pages_per_second',
                label: 'Cleanup scan pages per second',
                min: 1,
                max: 5,
              },
              {
                key: 'request_timeout_seconds',
                label: 'Cleanup request timeout (seconds)',
                min: 5,
                max: 60,
              },
              {
                key: 'cpu_pause_percent',
                label: 'Pause cleanup at CPU usage (%)',
                min: 30,
                max: 90,
              },
              {
                key: 'memory_pause_percent',
                label: 'Pause cleanup at memory usage (%)',
                min: 40,
                max: 90,
              },
            ] as const
          ).map((field) => (
            <div key={field.key} className='space-y-2'>
              <Label htmlFor={`cleanup-${field.key}`}>{t(field.label)}</Label>
              <Input
                id={`cleanup-${field.key}`}
                type='number'
                min={field.min}
                max={field.max}
                {...form.register(field.key, { valueAsNumber: true })}
                disabled={disabled}
                aria-invalid={!!form.formState.errors[field.key]}
              />
            </div>
          ))}
        </div>
        {policies.fields.map((policy, index) => {
          const intervalUnit =
            intervalUnits[policy.formKey] ??
            (policy.interval_minutes % 60 === 0 ? 'hours' : 'minutes')
          const intervalFactor = intervalUnit === 'hours' ? 60 : 1
          const nextRunAt = props.initial.policies.find(
            (saved) => saved.id === policy.id
          )?.next_run_at
          let nextRunLabel = '—'
          if (nextRunAt === 0) {
            nextRunLabel = t('Disabled')
          }
          if (nextRunAt !== undefined && nextRunAt > 0) {
            nextRunLabel = new Date(nextRunAt * 1000).toLocaleString()
          }
          return (
            <section
              key={policy.formKey}
              className='space-y-3 rounded-lg border p-3'
              aria-label={t('Cleanup policy')}
            >
              <div className='flex items-center gap-2'>
                <Checkbox
                  id={`cleanup-policy-${index}`}
                  checked={values.policies[index]?.enabled}
                  onCheckedChange={(value) =>
                    form.setValue(`policies.${index}.enabled`, value === true)
                  }
                  disabled={disabled}
                />
                <Label htmlFor={`cleanup-policy-${index}`}>
                  {t('Enable policy')}
                </Label>
              </div>
              <div className='grid gap-3 sm:grid-cols-2 lg:grid-cols-3'>
                <div className='space-y-2'>
                  <Label htmlFor={`cleanup-policy-name-${index}`}>
                    {t('Policy name')}
                  </Label>
                  <Input
                    id={`cleanup-policy-name-${index}`}
                    {...form.register(`policies.${index}.name`)}
                    disabled={disabled}
                  />
                </div>
                <ImageSelect
                  label={t('Storage channel')}
                  value={values.policies[index]?.channel_id || ''}
                  options={channels}
                  disabled={disabled}
                  onChange={(value) =>
                    form.setValue(`policies.${index}.channel_id`, value)
                  }
                />
                <div className='space-y-2'>
                  <Label htmlFor={`cleanup-policy-age-${index}`}>
                    {t('Older than')}
                  </Label>
                  <Input
                    id={`cleanup-policy-age-${index}`}
                    type='number'
                    min={1}
                    max={
                      values.policies[index]?.age_unit === 'days'
                        ? 36500
                        : 876000
                    }
                    {...form.register(`policies.${index}.older_than`, {
                      valueAsNumber: true,
                    })}
                    disabled={disabled}
                  />
                </div>
                <ImageSelect
                  label={t('Age unit')}
                  value={values.policies[index]?.age_unit || 'days'}
                  options={[
                    { value: 'hours', label: t('Hours') },
                    { value: 'days', label: t('Days') },
                  ]}
                  disabled={disabled}
                  onChange={(value) =>
                    form.setValue(
                      `policies.${index}.age_unit`,
                      value as 'hours' | 'days'
                    )
                  }
                />
                <ImageSelect
                  label={t('Schedule')}
                  value={values.policies[index]?.schedule_type || 'daily'}
                  options={[
                    { value: 'daily', label: t('Daily') },
                    { value: 'interval', label: t('Interval') },
                  ]}
                  disabled={disabled}
                  onChange={(value) =>
                    form.setValue(
                      `policies.${index}.schedule_type`,
                      value as 'daily' | 'interval'
                    )
                  }
                />
                {values.policies[index]?.schedule_type === 'interval' ? (
                  <>
                    <div className='space-y-2'>
                      <Label htmlFor={`cleanup-policy-interval-${index}`}>
                        {intervalUnit === 'hours'
                          ? t('Interval (hours)')
                          : t('Interval (minutes)')}
                      </Label>
                      <Input
                        id={`cleanup-policy-interval-${index}`}
                        type='number'
                        required
                        min={5 / intervalFactor}
                        max={43200 / intervalFactor}
                        step={intervalUnit === 'hours' ? 'any' : 1}
                        value={
                          Number.isFinite(
                            values.policies[index]?.interval_minutes
                          )
                            ? values.policies[index].interval_minutes /
                              intervalFactor
                            : ''
                        }
                        onChange={(event) => {
                          let minutes =
                            event.target.valueAsNumber * intervalFactor
                          const rounded = Math.round(minutes)
                          // Correct conversion noise without rounding fractional minutes.
                          if (
                            intervalUnit === 'hours' &&
                            Math.abs(minutes - rounded) <=
                              Number.EPSILON *
                                Math.max(1, Math.abs(minutes)) *
                                4
                          ) {
                            minutes = rounded
                          }
                          form.setValue(
                            `policies.${index}.interval_minutes`,
                            minutes
                          )
                        }}
                        disabled={disabled}
                        aria-invalid={
                          !!form.formState.errors.policies?.[index]
                            ?.interval_minutes
                        }
                      />
                    </div>
                    <ImageSelect
                      label={t('Interval unit')}
                      value={intervalUnit}
                      options={[
                        { value: 'minutes', label: t('Minutes') },
                        { value: 'hours', label: t('Hours') },
                      ]}
                      onChange={(value) =>
                        setIntervalUnits((units) => ({
                          ...units,
                          [policy.formKey]: value as 'minutes' | 'hours',
                        }))
                      }
                      disabled={disabled}
                    />
                  </>
                ) : (
                  <>
                    <div className='space-y-2'>
                      <Label htmlFor={`cleanup-policy-time-${index}`}>
                        {t('Daily cleanup time')}
                      </Label>
                      <Input
                        id={`cleanup-policy-time-${index}`}
                        type='time'
                        {...form.register(`policies.${index}.daily_time`)}
                        disabled={disabled}
                      />
                    </div>
                    <div className='space-y-2'>
                      <Label htmlFor={`cleanup-policy-zone-${index}`}>
                        {t('Time zone')}
                      </Label>
                      <Input
                        id={`cleanup-policy-zone-${index}`}
                        {...form.register(`policies.${index}.timezone`)}
                        disabled={disabled}
                      />
                    </div>
                  </>
                )}
              </div>
              <p
                aria-label={t('Next scheduled run (saved settings)')}
                className='text-muted-foreground text-sm'
              >
                {t('Next scheduled run (saved settings)')}: {nextRunLabel}
              </p>
              {props.canManage && (
                <Button
                  type='button'
                  variant='outline'
                  disabled={disabled}
                  onClick={() => policies.remove(index)}
                >
                  {t('Remove policy')}
                </Button>
              )}
            </section>
          )
        })}
        {Object.keys(form.formState.errors).length > 0 && (
          <p role='alert' className='text-destructive text-sm'>
            {t('Check this value.')}
          </p>
        )}
        {saveError && (
          <p role='alert' className='text-destructive text-sm'>
            {saveError}
          </p>
        )}
        {props.canManage && (
          <div className='flex flex-wrap gap-2'>
            <Button
              type='button'
              variant='outline'
              disabled={
                disabled || !channels.length || policies.fields.length >= 50
              }
              onClick={() =>
                policies.append({
                  id: crypto.randomUUID(),
                  name: '',
                  enabled: false,
                  channel_id: channels[0].value,
                  older_than: 180,
                  age_unit: 'days',
                  schedule_type: 'daily',
                  interval_minutes: 1440,
                  daily_time: '03:00',
                  timezone: 'Asia/Shanghai',
                })
              }
            >
              {t('Add cleanup policy')}
            </Button>
            <Button type='submit' disabled={disabled}>
              {t('Save cleanup settings')}
            </Button>
            {saveError && (
              <Button
                type='button'
                variant='outline'
                disabled={disabled}
                onClick={() => void props.onSaved()}
              >
                {t('Reload saved settings')}
              </Button>
            )}
          </div>
        )}
      </form>
    </SettingsCard>
  )
}

export function ImageCleanupPanel(props: { profiles?: ImageStorageProfile[] }) {
  const { t } = useTranslation()
  const user = useAuthStore((state) => state.auth.user)
  const canRead = hasPermission(user, 'image_moderation', 'read')
  const canManage = hasPermission(user, 'image_moderation', 'manage')
  const canReadStorage = hasPermission(user, 'image_config', 'read')
  const queryClient = useQueryClient()
  const config = useQuery({
    queryKey: ['image-configuration', user?.id],
    queryFn: ({ signal }) =>
      imageRequest<{ storage_profiles: ImageStorageProfile[] }>(
        '/api/option/images',
        'GET',
        undefined,
        signal
      ),
    enabled: props.profiles === undefined && canReadStorage,
    refetchOnWindowFocus: false,
  })
  const profiles = props.profiles || config.data?.storage_profiles || []
  const channels = imageCleanupChannelOptions(profiles)
  const form = useForm<CleanupRequestForm>({
    resolver: zodResolver(cleanupRequestSchema),
    defaultValues: {
      scope: 'all',
      channel_id: '',
      older_than: 180,
      age_unit: 'days',
      user_id: 0,
    },
  })
  const values = useWatch({ control: form.control })
  const signature = JSON.stringify(values)
  const [preview, setPreview] = useState<{
    job: CleanupJob
    request: CleanupRequest
  } | null>(null)
  const [confirm, setConfirm] = useState(false)
  const [cancel, setCancel] = useState<CleanupJob | null>(null)
  useEffect(() => {
    setPreview(null)
    setConfirm(false)
  }, [signature])
  const jobs = useQuery({
    queryKey: ['image-cleanup-jobs', user?.id],
    enabled: canRead,
    queryFn: ({ signal }) =>
      imageRequest<CursorPage<CleanupJob>>(
        `${cleanupJobsPath}?limit=5`,
        'GET',
        undefined,
        signal
      ),
    refetchInterval: (query) =>
      query.state.data?.items.slice(0, 5).some(cleanupIsActive) ? 5000 : 60000,
  })
  const previewJob = useQuery({
    queryKey: ['image-cleanup-preview', user?.id, preview?.job.job_id],
    enabled: !!preview && canManage,
    queryFn: ({ signal }) =>
      imageRequest<CleanupJob>(
        `${cleanupJobsPath}/${encodeURIComponent(preview?.job.job_id || '')}`,
        'GET',
        undefined,
        signal
      ),
    refetchInterval: (query) =>
      !query.state.data || cleanupIsActive(query.state.data) ? 2000 : false,
  })
  const currentPreview = previewJob.data || preview?.job
  const previewMutation = useMutation({
    mutationFn: (request: CleanupRequest) =>
      imageRequest<CleanupJob>(`${cleanupJobsPath}/preview`, 'POST', request),
    onSuccess: async (job, request) => {
      setPreview({ job, request })
      await queryClient.invalidateQueries({ queryKey: ['image-cleanup-jobs'] })
    },
    onError: (cause) => handleServerError(cause),
  })
  const create = useMutation({
    mutationFn: () =>
      imageRequest<CleanupJob>(cleanupJobsPath, 'POST', {
        ...preview?.request,
        preview_fingerprint: currentPreview?.preview_fingerprint,
      }),
    onSuccess: async () => {
      setConfirm(false)
      setPreview(null)
      await queryClient.invalidateQueries({ queryKey: ['image-cleanup-jobs'] })
      toast.success(t('Cleanup job queued'))
    },
    onError: (cause) => handleServerError(cause),
  })
  const control = useMutation({
    mutationFn: (input: { job: CleanupJob; action: string }) =>
      imageRequest(
        `${cleanupJobsPath}/${encodeURIComponent(input.job.job_id)}/${input.action}`,
        'POST',
        {}
      ),
    onSuccess: async () => {
      setCancel(null)
      await Promise.all([
        queryClient.invalidateQueries({ queryKey: ['image-cleanup-jobs'] }),
        queryClient.invalidateQueries({ queryKey: ['image-cleanup-preview'] }),
      ])
    },
    onError: (cause) => handleServerError(cause),
  })
  if (!canRead) return null
  return (
    <SettingsCard
      title={t('Cleanup jobs')}
      description={t(
        'New previews use image age instead of task or result retention; existing previews keep their original rules. Age is measured from file creation. Preview fixes the cutoff time; live references are checked again before deletion. Deleted files cannot be restored.'
      )}
    >
      <div className='space-y-4'>
        {canManage && (
          <form
            className='space-y-3'
            onSubmit={form.handleSubmit((input) =>
              previewMutation.mutate({
                scope: input.scope,
                filters: {
                  channel_id: input.channel_id,
                  older_than: input.older_than,
                  age_unit: input.age_unit,
                  ...(input.user_id ? { user_id: input.user_id } : {}),
                },
              })
            )}
          >
            <fieldset
              disabled={previewMutation.isPending || create.isPending}
              className='grid gap-3 sm:grid-cols-2 lg:grid-cols-3'
            >
              <ImageSelect
                label={t('Storage channel')}
                value={values.channel_id || ''}
                options={channels}
                disabled={previewMutation.isPending || create.isPending}
                onChange={(value) => form.setValue('channel_id', value)}
              />
              <div className='space-y-2'>
                <Label htmlFor='cleanup-age'>{t('Older than')}</Label>
                <Input
                  id='cleanup-age'
                  type='number'
                  min={1}
                  max={values.age_unit === 'days' ? 36500 : 876000}
                  {...form.register('older_than', { valueAsNumber: true })}
                />
              </div>
              <ImageSelect
                label={t('Age unit')}
                value={values.age_unit || 'days'}
                options={[
                  { value: 'hours', label: t('Hours') },
                  { value: 'days', label: t('Days') },
                ]}
                disabled={previewMutation.isPending || create.isPending}
                onChange={(value) =>
                  form.setValue('age_unit', value as 'hours' | 'days')
                }
              />
              <ImageSelect
                label={t('Scope')}
                value={values.scope || 'all'}
                options={[
                  'expired',
                  'deleted',
                  'user',
                  'async_results',
                  'all',
                ].map((value) => ({ value, label: t(imageLabel(value)) }))}
                disabled={previewMutation.isPending || create.isPending}
                onChange={(value) =>
                  form.setValue('scope', value as CleanupRequestForm['scope'])
                }
              />
              <div className='space-y-2'>
                <Label htmlFor='cleanup-user'>{t('User ID (0 for all)')}</Label>
                <Input
                  id='cleanup-user'
                  type='number'
                  min={0}
                  {...form.register('user_id', { valueAsNumber: true })}
                />
              </div>
              <Button
                type='submit'
                variant='outline'
                className='self-end'
                disabled={
                  !channels.length ||
                  previewMutation.isPending ||
                  create.isPending
                }
              >
                {t('Preview cleanup')}
              </Button>
            </fieldset>
            {config.isError && <p role='alert'>{config.error.message}</p>}
            {!canReadStorage && props.profiles === undefined && (
              <p role='status'>
                {t(
                  'Storage configuration read permission is required to select a cleanup channel.'
                )}
              </p>
            )}
            {Object.keys(form.formState.errors).length > 0 && (
              <p role='alert' className='text-destructive text-sm'>
                {t('Check this value.')}
              </p>
            )}
          </form>
        )}
        {previewJob.isError && <p role='alert'>{previewJob.error.message}</p>}
        {currentPreview && (
          <section
            className='space-y-2 rounded-lg border p-3'
            aria-label={t('Cleanup preview')}
          >
            <p role='status'>{t(imageLabel(currentPreview.status))}</p>
            <p>
              {t('Cleanup cutoff')}:{' '}
              {new Date(currentPreview.cutoff_at * 1000).toLocaleString()}
            </p>
            <p>
              {t('Scanned')}: {currentPreview.scanned} · {t('Matched files')}:{' '}
              {currentPreview.matched_items} · {t('Protected files')}:{' '}
              {currentPreview.protected_items} ·{' '}
              {(currentPreview.matched_bytes / 1048576).toFixed(1)} MiB
            </p>
            <ImageCleanupProtectionSummary job={currentPreview} />
            {currentPreview.last_error && (
              <p role='alert'>
                {translateImageCleanupMessage(currentPreview.last_error, t)}
              </p>
            )}
            {currentPreview.status === 'preview_ready' &&
              currentPreview.preview_fingerprint && (
                <Button
                  variant='destructive'
                  disabled={
                    create.isPending || currentPreview.matched_items === 0
                  }
                  onClick={() => setConfirm(true)}
                >
                  {t('Create cleanup job')}
                </Button>
              )}
          </section>
        )}
        <div className='flex flex-wrap items-start justify-between gap-2'>
          <div className='text-muted-foreground text-sm'>
            <p>{t('Showing the 5 most recent storage cleanup jobs.')}</p>
            <p>
              {t(
                'Finished job records are automatically removed after 24 hours. Unfinished jobs and jobs awaiting deletion verification are kept.'
              )}
            </p>
          </div>
          <Button
            variant='outline'
            disabled={jobs.isFetching}
            onClick={() => void jobs.refetch()}
          >
            {t('Refresh')}
          </Button>
        </div>
        {jobs.isError && <p role='alert'>{jobs.error.message}</p>}
        <StaticDataTable
          // Animated row transforms can leave horizontally scrolled cells unpainted.
          tableClassName='[&_tbody_tr]:animate-none!'
          data={jobs.data?.items.slice(0, 5) || []}
          getRowKey={(job) => job.job_id}
          emptyContent={jobs.isPending ? t('Loading...') : t('No items found')}
          columns={[
            {
              id: 'job',
              header: t('Cleanup job'),
              cell: (job) => (
                <div className='max-w-56 text-xs break-all'>
                  <p>{job.job_id}</p>
                  <p>
                    {t(
                      (
                        {
                          scheduled: 'Scheduled',
                          manual: 'Manual',
                          manual_age: 'Manual',
                          legacy: 'Legacy',
                        } as Record<string, string>
                      )[job.source] || 'Legacy'
                    )}
                  </p>
                  {job.policy_id && <p>{job.policy_id}</p>}
                </div>
              ),
            },
            {
              id: 'channel',
              header: t('Storage channel'),
              cell: (job) => {
                let channelID = ''
                try {
                  const filters: unknown = JSON.parse(job.filters)
                  if (
                    filters &&
                    typeof filters === 'object' &&
                    'channel_id' in filters &&
                    typeof filters.channel_id === 'string'
                  ) {
                    channelID = filters.channel_id
                  }
                } catch {
                  // Legacy jobs may not carry structured channel filters.
                }
                return (
                  channels.find((channel) => channel.value === channelID)
                    ?.label ||
                  channelID ||
                  t('All')
                )
              },
            },
            {
              id: 'status',
              header: t('Status'),
              cell: (job) => (
                <div className='max-w-64 text-xs'>
                  <p>{t(imageLabel(job.status))}</p>
                  {job.next_attempt_at > 0 && (
                    <p>
                      {t('Next attempt')}:{' '}
                      {new Date(job.next_attempt_at * 1000).toLocaleString()}
                    </p>
                  )}
                  {job.last_error && (
                    <p className='text-destructive break-words'>
                      {translateImageCleanupMessage(job.last_error, t)}
                    </p>
                  )}
                </div>
              ),
            },
            {
              id: 'progress',
              header: t('Progress'),
              cell: (job) => (
                <div className='text-xs tabular-nums'>
                  <p>
                    {t('Scanned')}: {job.scanned} · {t('Matched files')}:{' '}
                    {job.matched_items}
                  </p>
                  <p>
                    {t('Deleted files')}: {job.deleted} · {t('Skipped')}:{' '}
                    {job.skipped} · {t('Protected files')}:{' '}
                    {job.protected_items} · {t('Errors')}: {job.errors}
                  </p>
                  <ImageCleanupProtectionSummary job={job} />
                  <p>
                    {t('Recorded size of confirmed deleted objects')}:{' '}
                    {(job.deleted_bytes / 1048576).toFixed(1)} MiB
                  </p>
                </div>
              ),
            },
            {
              id: 'cutoff',
              header: t('Cleanup cutoff'),
              cell: (job) =>
                job.cutoff_at
                  ? new Date(job.cutoff_at * 1000).toLocaleString()
                  : '—',
            },
            {
              id: 'actions',
              header: t('Actions'),
              cell: (job) =>
                canManage && (
                  <div className='flex flex-wrap gap-2'>
                    {cleanupIsActive(job) && (
                      <Button
                        size='sm'
                        variant='outline'
                        disabled={control.isPending}
                        onClick={() => control.mutate({ job, action: 'pause' })}
                      >
                        {t('Pause')}
                      </Button>
                    )}
                    {job.status === 'paused' && (
                      <Button
                        size='sm'
                        variant='outline'
                        disabled={control.isPending}
                        onClick={() =>
                          control.mutate({ job, action: 'resume' })
                        }
                      >
                        {t('Resume')}
                      </Button>
                    )}
                    {[
                      'previewing',
                      'preview_ready',
                      'queued',
                      'running',
                      'paused',
                    ].includes(job.status) && (
                      <Button
                        size='sm'
                        variant='outline'
                        disabled={control.isPending}
                        onClick={() => setCancel(job)}
                      >
                        {t('Cancel cleanup')}
                      </Button>
                    )}
                  </div>
                ),
            },
          ]}
        />
      </div>
      <ConfirmDialog
        open={confirm}
        onOpenChange={(open) => {
          if (!create.isPending) setConfirm(open)
        }}
        title={t('Delete unreferenced image files')}
        desc={
          <div className='space-y-2'>
            <p>
              {t(
                'References are checked again before deletion. Deleted files cannot be restored.'
              )}
            </p>
            <p>
              {t('Storage channel')}:{' '}
              {
                channels.find(
                  (channel) =>
                    channel.value === preview?.request.filters.channel_id
                )?.label
              }
            </p>
            <p>
              {t('Cleanup cutoff')}:{' '}
              {currentPreview
                ? new Date(currentPreview.cutoff_at * 1000).toLocaleString()
                : ''}
            </p>
            <p>
              {t('Matched files')}: {currentPreview?.matched_items} ·{' '}
              {t('Protected files')}: {currentPreview?.protected_items}
            </p>
            {currentPreview && (
              <ImageCleanupProtectionSummary job={currentPreview} />
            )}
          </div>
        }
        destructive
        isLoading={create.isPending}
        handleConfirm={() => create.mutate()}
      />
      <ConfirmDialog
        open={!!cancel}
        onOpenChange={(open) => {
          if (!open && !control.isPending) setCancel(null)
        }}
        title={t('Cancel cleanup')}
        desc={t(
          'Cancellation stops new deletion requests. Requests already sent may finish; deleted files cannot be restored.'
        )}
        isLoading={control.isPending}
        handleConfirm={() => {
          if (cancel) control.mutate({ job: cancel, action: 'cancel' })
        }}
      />
    </SettingsCard>
  )
}

function ImageCleanupProtectionSummary(props: { job: CleanupJob }) {
  const { t } = useTranslation()
  return (
    <p className='text-xs tabular-nums'>
      {t('Live references')}: {props.job.reference_skipped ?? 0} ·{' '}
      {t('Within retention period')}: {props.job.retention_skipped ?? 0} ·{' '}
      {t('Shared or unknown storage location')}:{' '}
      {props.job.location_skipped ?? 0}
    </p>
  )
}
