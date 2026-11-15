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
import { useMutation } from '@tanstack/react-query'
import { useState } from 'react'
import { useForm } from 'react-hook-form'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'
import { z } from 'zod'

import { ConfirmDialog } from '@/components/confirm-dialog'
import { StaticDataTable } from '@/components/data-table'
import { Button } from '@/components/ui/button'
import { Checkbox } from '@/components/ui/checkbox'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { SettingsCard } from '@/features/system-settings/components/settings-card'
import { hasPermission } from '@/lib/admin-permissions'
import { handleServerError } from '@/lib/handle-server-error'
import {
  getServerErrorMessage,
  getServerErrorStatus,
} from '@/lib/server-error-message'
import { useAuthStore } from '@/stores/auth-store'

import { imageRequest } from '../api'
import type { ImageAdminConfiguration, ImageStorageProfile } from '../types'
import { ImageSelect } from './image-select'

const storageSchema = z
  .object({
    name: z.string().trim().min(1).max(128),
    class: z.enum(['temporary', 'durable']),
    provider: z.string().min(1),
    root: z.string().max(4096),
    endpoint: z.string().max(2048),
    public_base_url: z.string().trim().max(2048),
    bucket: z.string().max(255),
    region: z.string().max(128),
    prefix: z.string().max(1024),
    path_style: z.boolean(),
    access_key: z.string().max(4096),
    secret_key: z.string().max(4096),
  })
  .superRefine((value, context) => {
    if (
      value.provider !== 'local' &&
      (value.provider === 'r2' || value.public_base_url)
    ) {
      let valid = false
      try {
        const address = new URL(value.public_base_url)
        valid =
          address.protocol === 'https:' &&
          !!address.hostname &&
          !address.username &&
          !address.password &&
          !value.public_base_url.includes('?') &&
          !value.public_base_url.includes('#') &&
          address.hostname !== 'r2.cloudflarestorage.com' &&
          !address.hostname.endsWith('.r2.cloudflarestorage.com')
      } catch {
        valid = false
      }
      if (!valid) {
        context.addIssue({
          code: 'custom',
          path: ['public_base_url'],
          message:
            'Enter a public HTTPS domain without credentials, query parameters or fragments. Do not use the R2 S3 endpoint.',
        })
      }
    }
    if (value.provider !== 'local' && !value.bucket.trim()) {
      context.addIssue({
        code: 'custom',
        path: ['bucket'],
        message: 'Check this value.',
      })
    }
    if (!!value.access_key !== !!value.secret_key) {
      context.addIssue({
        code: 'custom',
        path: ['secret_key'],
        message: 'Enter both storage credentials.',
      })
    }
  })
type StorageValues = z.infer<typeof storageSchema>
const emptyStorage: StorageValues = {
  name: '',
  class: 'temporary',
  provider: 'local',
  root: '',
  endpoint: '',
  public_base_url: '',
  bucket: '',
  region: 'us-east-1',
  prefix: '',
  path_style: false,
  access_key: '',
  secret_key: '',
}

export function ImageStorageForm(props: {
  config: ImageAdminConfiguration
  onSuccess: () => Promise<unknown>
}) {
  const { t } = useTranslation()
  const user = useAuthStore((state) => state.auth.user)
  const canManage = hasPermission(user, 'image_config', 'manage')
  const [editing, setEditing] = useState<ImageStorageProfile | null>(null)
  const [action, setAction] = useState<{
    kind: 'activate' | 'credentials' | 'delete'
    profile: ImageStorageProfile
  } | null>(null)
  const [error, setError] = useState('')
  const form = useForm<StorageValues>({
    resolver: zodResolver(storageSchema),
    defaultValues: emptyStorage,
  })
  const provider = form.watch('provider')
  const profiles = props.config.storage_profiles.filter(
    (item) => item.current_revision !== false
  )
  const save = useMutation({
    gcTime: 0,
    retry: false,
    mutationFn: async () => {
      const { access_key, secret_key, ...values } = form.getValues()
      try {
        return await imageRequest(
          `/api/option/images/storage${editing ? `/${encodeURIComponent(editing.profile_id)}` : ''}`,
          editing ? 'PUT' : 'POST',
          {
            revision: props.config.storage_revision ?? 0,
            profile: {
              ...editing,
              ...values,
              profile_id: editing?.profile_id || '',
              backend: values.provider === 'local' ? 'local' : 's3',
              public_base_url:
                values.provider === 'local'
                  ? ''
                  : values.public_base_url.trim(),
              active: editing?.active ?? false,
            },
            ...(access_key && secret_key
              ? { credentials: { access_key, secret_key } }
              : {}),
          }
        )
      } catch (cause) {
        // Axios errors retain the credential-bearing request body.
        const status = getServerErrorStatus(cause)
        throw Object.assign(
          new Error(getServerErrorMessage(cause)),
          status ? { isAxiosError: true, response: { status } } : {}
        )
      }
    },
    onSuccess: async () => {
      form.reset(emptyStorage)
      setEditing(null)
      setError('')
      await props.onSuccess()
      toast.success(t('Saved'))
    },
    onError: (cause) => {
      setError(
        cause instanceof Error ? cause.message : t('Image request failed')
      )
      handleServerError(cause)
    },
  })
  const operation = useMutation({
    gcTime: 0,
    retry: false,
    mutationFn: async () => {
      if (!action) return
      const { access_key, secret_key } = form.getValues()
      try {
        return await imageRequest(
          `/api/option/images/storage/${encodeURIComponent(action.profile.profile_id)}${action.kind === 'delete' ? '' : `/${action.kind}`}`,
          action.kind === 'delete' ? 'DELETE' : 'POST',
          {
            revision: props.config.storage_revision ?? 0,
            ...(action.kind === 'credentials'
              ? { credentials: { access_key, secret_key } }
              : {}),
          }
        )
      } catch (cause) {
        const status = getServerErrorStatus(cause)
        throw Object.assign(
          new Error(t(getServerErrorMessage(cause))),
          status ? { isAxiosError: true, response: { status } } : {}
        )
      }
    },
    onSuccess: async () => {
      if (
        action?.kind === 'delete' &&
        editing &&
        (editing.channel_id || editing.profile_id) ===
          (action.profile.channel_id || action.profile.profile_id)
      ) {
        setEditing(null)
        form.reset(emptyStorage)
      }
      setAction(null)
      form.setValue('access_key', '')
      form.setValue('secret_key', '')
      setError('')
      await props.onSuccess()
      toast.success(
        t(action?.kind === 'delete' ? 'Storage channel removed' : 'Saved')
      )
    },
    onError: (cause) => {
      setError(
        cause instanceof Error ? cause.message : t('Image request failed')
      )
      handleServerError(cause)
    },
  })
  const test = useMutation({
    mutationFn: (id: string) =>
      imageRequest(
        `/api/option/images/storage/${encodeURIComponent(id)}/test`,
        'POST',
        {}
      ),
    onSuccess: () =>
      toast.success(t('Storage write, read and delete verified')),
    onError: (cause) => handleServerError(cause),
  })
  const busy = save.isPending || operation.isPending || test.isPending
  let formError = 'Check this value.'
  if (form.formState.errors.public_base_url?.message) {
    formError = form.formState.errors.public_base_url.message
  } else if (form.formState.errors.secret_key?.message) {
    formError = form.formState.errors.secret_key.message
  }
  let actionTitle = 'Enable channel'
  let actionDescription =
    'This channel will replace the current writer for the same purpose. Existing files will stay in their original storage.'
  if (action?.kind === 'credentials') {
    actionTitle = 'Rotate channel credentials'
    actionDescription =
      'The new credentials will update historical revisions at this storage location. Other endpoints and buckets keep their credentials.'
  }
  if (action?.kind === 'delete') {
    actionTitle = 'Delete storage channel'
    actionDescription =
      'Remove this inactive channel from the storage list? Historical configuration and existing images are kept. Uploads already in progress can finish.'
  }
  return (
    <SettingsCard
      title={t('Image storage')}
      description={t(
        'Enable one channel per purpose. Existing images keep their storage revision when the active channel changes.'
      )}
    >
      <div className='space-y-5'>
        {props.config.storage_upload_status && (
          <section
            aria-label={t('Storage upload concurrency')}
            className='space-y-2 rounded-lg border p-3 text-sm'
          >
            <div className='flex flex-wrap items-center justify-between gap-2'>
              <p>
                {t('Storage upload concurrency')}:{' '}
                {props.config.storage_upload_status.limit} · {t('Active')}:{' '}
                {props.config.storage_upload_status.active} · {t('Queued')}:{' '}
                {props.config.storage_upload_status.queued}
              </p>
              <Button
                size='sm'
                variant='outline'
                disabled={busy}
                onClick={() => void props.onSuccess()}
              >
                {t('Refresh')}
              </Button>
            </div>
            <p className='text-muted-foreground'>
              {t(
                'This snapshot covers the responding node only. Other nodes may apply settings at different times.'
              )}
            </p>
          </section>
        )}
        {error && (
          <div role='alert' className='text-destructive text-sm'>
            {error}
            <Button variant='link' onClick={() => void props.onSuccess()}>
              {t('Reload saved settings')}
            </Button>
          </div>
        )}
        <StaticDataTable
          // Animated row transforms can leave horizontally scrolled cells unpainted.
          tableClassName='[&_tbody_tr]:animate-none!'
          data={profiles}
          getRowKey={(item) => item.profile_id}
          emptyContent={t('No items found')}
          columns={[
            {
              id: 'name',
              header: t('Name'),
              cell: (item) => (
                <div>
                  <p>{item.name || item.channel_id || item.profile_id}</p>
                  <p className='text-muted-foreground text-xs'>
                    {item.provider} ·{' '}
                    {t(item.class === 'temporary' ? 'Temporary' : 'Durable')}
                  </p>
                </div>
              ),
            },
            {
              id: 'location',
              header: t('Location'),
              cell: (item) => (
                <span className='text-xs break-all'>
                  {item.root ||
                    `${item.endpoint} / ${item.bucket} / ${item.prefix}`}
                </span>
              ),
            },
            {
              id: 'active',
              header: t('Active'),
              cell: (item) => t(item.active ? 'Yes' : 'No'),
            },
            {
              id: 'actions',
              header: t('Actions'),
              cell: (item) =>
                canManage && (
                  <div className='flex flex-wrap gap-2'>
                    <Button
                      size='sm'
                      variant='outline'
                      disabled={busy}
                      onClick={() => {
                        setEditing(item)
                        setError('')
                        form.reset({
                          ...emptyStorage,
                          ...item,
                          name: item.name || item.channel_id || item.profile_id,
                          class:
                            item.class === 'durable' ? 'durable' : 'temporary',
                          access_key: '',
                          secret_key: '',
                        })
                      }}
                    >
                      {t('Edit')}
                    </Button>
                    {!item.active && (
                      <Button
                        size='sm'
                        variant='outline'
                        disabled={busy}
                        onClick={() => {
                          setError('')
                          setAction({
                            kind: 'activate',
                            profile: item,
                          })
                        }}
                      >
                        {t('Enable channel')}
                      </Button>
                    )}
                    <Button
                      size='sm'
                      variant='outline'
                      disabled={busy}
                      onClick={() => test.mutate(item.profile_id)}
                    >
                      {t('Test storage')}
                    </Button>
                    <Button
                      size='sm'
                      variant='destructive'
                      disabled={busy || item.active}
                      title={
                        item.active
                          ? t(
                              'Active storage channels cannot be deleted. Enable another channel for the same purpose first.'
                            )
                          : undefined
                      }
                      onClick={() => {
                        setError('')
                        setAction({ kind: 'delete', profile: item })
                      }}
                    >
                      {t('Delete storage channel')}
                    </Button>
                  </div>
                ),
            },
          ]}
        />
        {profiles.some((profile) => profile.active) && (
          <p className='text-muted-foreground text-sm'>
            {t(
              'Enable another channel for the same purpose to replace the current writer.'
            )}
          </p>
        )}
        {canManage && (
          <form
            className='space-y-4'
            onSubmit={form.handleSubmit(() => save.mutate())}
          >
            <h3 className='font-medium'>
              {t(editing ? 'Edit storage channel' : 'Add storage channel')}
            </h3>
            <p className='text-muted-foreground text-sm'>
              {t(
                editing?.active
                  ? 'Saving an active channel switches new writes to its new revision. Existing files keep the old revision.'
                  : 'New channels are saved inactive. Test the saved channel before enabling it.'
              )}
            </p>
            <div className='grid gap-3 sm:grid-cols-2'>
              <div className='space-y-2'>
                <Label htmlFor='storage-name'>
                  {t('Storage channel name')}
                </Label>
                <Input
                  id='storage-name'
                  {...form.register('name')}
                  disabled={busy}
                  aria-invalid={!!form.formState.errors.name}
                />
              </div>
              <ImageSelect
                label={t('Purpose')}
                value={form.watch('class')}
                options={[
                  { value: 'temporary', label: t('Temporary') },
                  { value: 'durable', label: t('Durable') },
                ]}
                disabled={busy || !!editing}
                onChange={(value) =>
                  form.setValue('class', value as StorageValues['class'])
                }
              />
              <ImageSelect
                label={t('Provider')}
                value={provider}
                options={props.config.storage_providers.map((value) => ({
                  value,
                  label: value === 'r2' ? 'Cloudflare R2' : value,
                }))}
                disabled={busy}
                onChange={(value) => {
                  form.setValue('provider', value)
                  form.setValue('region', value === 'r2' ? 'auto' : 'us-east-1')
                }}
              />
              {(provider === 'local'
                ? [{ key: 'root', label: 'Storage directory (optional)' }]
                : [
                    { key: 'endpoint', label: 'S3 endpoint' },
                    { key: 'bucket', label: 'Bucket' },
                    { key: 'region', label: 'Region' },
                    { key: 'prefix', label: 'Object prefix' },
                  ]
              ).map(({ key, label }) => (
                <div key={key} className='space-y-2'>
                  <Label htmlFor={`storage-${key}`}>{t(label)}</Label>
                  <Input
                    id={`storage-${key}`}
                    {...form.register(key as keyof StorageValues)}
                    disabled={busy}
                    aria-invalid={
                      !!form.formState.errors[key as keyof StorageValues]
                    }
                  />
                </div>
              ))}
            </div>
            {provider !== 'local' && (
              <div className='space-y-3'>
                <div className='space-y-2'>
                  <Label
                    htmlFor='storage-public-base-url'
                    required={provider === 'r2'}
                  >
                    {t('Public access domain')}
                  </Label>
                  <Input
                    id='storage-public-base-url'
                    placeholder='https://images.example.com'
                    {...form.register('public_base_url')}
                    disabled={busy}
                    aria-required={provider === 'r2'}
                    aria-invalid={!!form.formState.errors.public_base_url}
                    aria-describedby='storage-public-base-url-help'
                  />
                  <p
                    id='storage-public-base-url-help'
                    className='text-muted-foreground text-sm'
                  >
                    {t(
                      'R2 requires a public bucket domain, such as a custom domain or r2.dev address. S3 domains are optional. Image results use this domain without signatures; leave it empty to use this site.'
                    )}
                  </p>
                </div>
                {editing?.credential_configured && (
                  <p className='text-muted-foreground text-sm'>
                    {t(
                      'Storage credentials are saved. Leave both fields blank to keep them.'
                    )}
                  </p>
                )}
                <div className='grid gap-3 sm:grid-cols-2'>
                  {[
                    { key: 'access_key', label: 'Access key' },
                    { key: 'secret_key', label: 'Secret key' },
                  ].map(({ key, label }) => (
                    <div key={key} className='space-y-2'>
                      <Label htmlFor={`storage-${key}`}>{t(label)}</Label>
                      <Input
                        id={`storage-${key}`}
                        type='password'
                        autoComplete='new-password'
                        {...form.register(key as 'access_key' | 'secret_key')}
                        disabled={busy}
                      />
                    </div>
                  ))}
                </div>
                <div className='flex items-center gap-2'>
                  <Checkbox
                    id='storage-path-style'
                    checked={form.watch('path_style')}
                    onCheckedChange={(value) =>
                      form.setValue('path_style', value === true)
                    }
                    disabled={busy}
                  />
                  <Label htmlFor='storage-path-style'>
                    {t('Path-style access')}
                  </Label>
                </div>
              </div>
            )}
            {Object.keys(form.formState.errors).length > 0 && (
              <p role='alert' className='text-destructive text-sm'>
                {t(formError)}
              </p>
            )}
            <div className='flex flex-wrap gap-2'>
              <Button type='submit' disabled={busy}>
                {t('Save storage channel')}
              </Button>
              {editing && (
                <Button
                  type='button'
                  variant='outline'
                  disabled={busy}
                  onClick={() => {
                    setEditing(null)
                    form.reset(emptyStorage)
                    setError('')
                  }}
                >
                  {t('Cancel editing')}
                </Button>
              )}
              {editing && provider !== 'local' && (
                <Button
                  type='button'
                  variant='outline'
                  disabled={
                    busy ||
                    !form.watch('access_key') ||
                    !form.watch('secret_key')
                  }
                  onClick={() => {
                    setError('')
                    setAction({ kind: 'credentials', profile: editing })
                  }}
                >
                  {t('Rotate channel credentials')}
                </Button>
              )}
            </div>
          </form>
        )}
      </div>
      <ConfirmDialog
        open={!!action}
        onOpenChange={(open) => {
          if (!open && !operation.isPending) setAction(null)
        }}
        title={t(actionTitle)}
        desc={
          <div className='space-y-2'>
            <p>{action?.profile.name || action?.profile.profile_id}</p>
            <p className='text-xs break-all'>
              {action?.profile.root ||
                `${action?.profile.endpoint || ''} / ${action?.profile.bucket || ''}`}
            </p>
            <p>{t(actionDescription)}</p>
            {error && (
              <p role='alert' className='text-destructive'>
                {error}
              </p>
            )}
          </div>
        }
        isLoading={operation.isPending}
        destructive={action?.kind === 'delete'}
        handleConfirm={() => operation.mutate()}
      />
    </SettingsCard>
  )
}
