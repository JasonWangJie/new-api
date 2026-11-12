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
import { useForm } from 'react-hook-form'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'

import { ErrorState } from '@/components/error-state'
import { LoadingState } from '@/components/loading-state'
import { PasswordInput } from '@/components/password-input'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Textarea } from '@/components/ui/textarea'
import { SettingsCard } from '@/features/system-settings/components/settings-card'
import {
  SettingsFormGrid,
  SettingsSwitchField,
} from '@/features/system-settings/components/settings-form-layout'
import { hasPermission } from '@/lib/admin-permissions'
import { markServerErrorHandled } from '@/lib/handle-server-error'
import {
  getServerErrorMessage,
  getServerErrorStatus,
} from '@/lib/server-error-message'
import { useAuthStore } from '@/stores/auth-store'

import { imageRequest } from '../api'
import { isConfigurationConflict } from '../lib/configuration-conflict'
import {
  imageHostingFormSchema,
  imageHostingFormValues,
  type ImageHostingConfiguration,
  type ImageHostingFormValues,
} from '../lib/image-hosting'
import { ConfigurationConflict } from './configuration-conflict'
import { ImageSelect } from './image-select'

const CONFIG_PATH = '/api/option/images/hosting'
const TEXT_FIELDS = [
  {
    name: 'base_url',
    label: 'Image host URL',
    placeholder: 'https://images.example.com',
  },
  {
    name: 'file_upload_path',
    label: 'File upload path',
    placeholder: '/api/v1/upload',
  },
  {
    name: 'url_upload_path',
    label: 'URL upload path',
    placeholder: '/api/v1/upload/url',
  },
  { name: 'file_field', label: 'Upload file field', placeholder: 'file' },
  { name: 'url_field', label: 'Upload URL field', placeholder: 'url' },
  {
    name: 'response_url_path',
    label: 'Response image URL path',
    placeholder: 'data.links.url',
  },
  {
    name: 'token_header',
    label: 'Upload token header',
    placeholder: 'Authorization',
  },
  {
    name: 'token_prefix',
    label: 'Upload token prefix',
    placeholder: 'Bearer ',
  },
] as const
const LIMIT_FIELDS = [
  {
    name: 'timeout_seconds',
    label: 'Image host timeout (seconds)',
    min: 1,
    max: 120,
  },
  { name: 'retries', label: 'Image host retries', min: 0, max: 3 },
  { name: 'concurrency', label: 'Image host concurrency', min: 1, max: 32 },
  {
    name: 'failure_threshold',
    label: 'Image host failure threshold',
    min: 1,
    max: 100,
  },
  {
    name: 'cooldown_seconds',
    label: 'Image host cooldown (seconds)',
    min: 1,
    max: 3600,
  },
] as const

export function ExternalImageHostingSettings() {
  const user = useAuthStore((state) => state.auth.user)
  const allowed = hasPermission(user, 'image_config', 'manage')
  const queryClient = useQueryClient()
  const queryKey = ['image-hosting-configuration', user?.id]
  const configuration = useQuery({
    queryKey,
    queryFn: ({ signal }) =>
      imageRequest<ImageHostingConfiguration>(
        CONFIG_PATH,
        'GET',
        undefined,
        signal
      ),
    enabled: allowed,
    refetchOnWindowFocus: false,
    meta: { errorToast: false },
  })
  if (!allowed) return null
  if (configuration.isError && !configuration.data) {
    return (
      <ErrorState
        description={configuration.error.message}
        onRetry={() => void configuration.refetch()}
      />
    )
  }
  if (!configuration.data) return <LoadingState />
  return (
    <ImageHostingForm
      key={configuration.data.revision}
      initial={configuration.data}
      onSaved={(saved) => queryClient.setQueryData(queryKey, saved)}
      onReload={() => configuration.refetch({ throwOnError: true })}
    />
  )
}

export function ImageHostingForm(props: {
  initial: ImageHostingConfiguration
  onSaved: (saved: ImageHostingConfiguration) => void
  onReload: () => Promise<unknown>
}) {
  const { t } = useTranslation()
  const form = useForm<ImageHostingFormValues>({
    resolver: zodResolver(imageHostingFormSchema),
    defaultValues: imageHostingFormValues(props.initial),
  })
  const enabled = form.watch('enabled')
  const imageEnabled = form.watch('image_enabled')
  const clearToken = form.watch('clear_token')
  const save = useMutation({
    // Keep credentials out of mutation variables and the query cache.
    mutationFn: async () => {
      const values = form.getValues()
      const { extra_fields_json, token, clear_token, ...configuration } = values
      try {
        return await imageRequest<ImageHostingConfiguration>(
          CONFIG_PATH,
          'PUT',
          {
            ...configuration,
            revision: props.initial.revision,
            extra_fields: JSON.parse(extra_fields_json) as Record<
              string,
              string | number | boolean
            >,
            ...(token ? { token } : {}),
            ...(clear_token ? { clear_token: true } : {}),
          }
        )
      } catch (error) {
        // Axios errors retain request bodies. Cache only a safe message/status.
        const status = getServerErrorStatus(error)
        throw Object.assign(
          new Error(getServerErrorMessage(error)),
          status ? { isAxiosError: true, response: { status } } : {}
        )
      }
    },
    onSuccess: (saved) => {
      form.reset(imageHostingFormValues(saved))
      props.onSaved(saved)
      toast.success(t('Saved'))
    },
    onError: (error) => markServerErrorHandled(error),
    meta: { errorToast: false },
    retry: false,
    gcTime: 0,
  })
  const busy = save.isPending
  const runtime = props.initial.runtime
  let circuitLabel = t('Image host available')
  if (runtime?.state === 'open') circuitLabel = t('Image host circuit open')
  if (runtime?.state === 'half_open') {
    circuitLabel = t('Image host recovery probe')
  }
  return (
    <SettingsCard
      title={t('External image hosting')}
      description={t(
        'Upload generated images to an image host. Asynchronous videos keep local storage.'
      )}
    >
      <form
        className='space-y-5'
        onSubmit={form.handleSubmit(() => save.mutate())}
      >
        <SettingsSwitchField
          controlId='image-hosting-enabled'
          label={t('Enable external image hosting')}
          checked={enabled}
          onCheckedChange={(checked) =>
            form.setValue('enabled', checked, { shouldDirty: true })
          }
          disabled={busy}
        />
        <SettingsSwitchField
          controlId='image-hosting-images'
          label={t('Upload generated image results')}
          description={t(
            'Reference uploads and video files use their existing storage settings.'
          )}
          checked={imageEnabled}
          onCheckedChange={(checked) =>
            form.setValue('image_enabled', checked, { shouldDirty: true })
          }
          disabled={busy || !enabled}
        />
        <p className='text-muted-foreground text-sm'>
          {t(
            'Image hosting returns externally readable links. Protected Canvas images keep their authorized delivery path.'
          )}
        </p>
        <p className='text-muted-foreground text-sm'>
          {t(
            'Allow cross-origin image reads from this site on the image host or CDN so previews and downloads work.'
          )}
        </p>
        {runtime && (
          <p className='text-muted-foreground text-sm' role='status'>
            {t('Image host circuit')}: {circuitLabel}
            {' · '}
            {t('Failures')}: {runtime.failures}
            {' · '}
            {t('In flight')}: {runtime.in_flight}
            {runtime.retry_after_seconds > 0 && (
              <>
                {' '}
                ·{' '}
                {t('Retry in {{seconds}} seconds', {
                  seconds: runtime.retry_after_seconds,
                })}
              </>
            )}
          </p>
        )}
        <fieldset disabled={busy} className='min-w-0 space-y-5'>
          <SettingsFormGrid>
            {TEXT_FIELDS.map((field) => (
              <div key={field.name} className='min-w-0 space-y-2'>
                <Label htmlFor={`image-hosting-${field.name}`}>
                  {t(field.label)}
                </Label>
                <Input
                  id={`image-hosting-${field.name}`}
                  placeholder={field.placeholder}
                  autoComplete='off'
                  aria-invalid={Boolean(form.formState.errors[field.name])}
                  {...form.register(field.name)}
                />
                {form.formState.errors[field.name] && (
                  <p role='alert' className='text-destructive text-xs'>
                    {t('Check this value.')}
                  </p>
                )}
              </div>
            ))}
            {(['file_upload_method', 'url_upload_method'] as const).map(
              (name) => (
                <ImageSelect
                  key={name}
                  label={t(
                    name === 'file_upload_method'
                      ? 'File upload method'
                      : 'URL upload method'
                  )}
                  value={form.watch(name)}
                  options={['POST', 'PUT'].map((value) => ({
                    value,
                    label: value,
                  }))}
                  onChange={(value) =>
                    form.setValue(name, value as 'POST' | 'PUT', {
                      shouldDirty: true,
                    })
                  }
                  disabled={busy}
                />
              )
            )}
            <div className='min-w-0 space-y-2'>
              <Label htmlFor='image-hosting-token'>
                {t('Image host upload token')}
              </Label>
              <PasswordInput
                id='image-hosting-token'
                autoComplete='new-password'
                maxLength={4096}
                aria-invalid={Boolean(form.formState.errors.token)}
                disabled={busy || clearToken}
                {...form.register('token')}
              />
              {form.formState.errors.token && (
                <p role='alert' className='text-destructive text-xs'>
                  {t('Check this value.')}
                </p>
              )}
              <p className='text-muted-foreground text-xs'>
                {t(
                  props.initial.has_token
                    ? 'An upload token is saved. Leave blank to keep it.'
                    : 'No upload token is saved.'
                )}
              </p>
            </div>
            <SettingsSwitchField
              controlId='image-hosting-clear-token'
              label={t('Remove saved upload token')}
              checked={clearToken}
              onCheckedChange={(checked) => {
                form.setValue('clear_token', checked, { shouldDirty: true })
                if (checked) form.setValue('token', '')
              }}
              disabled={busy || !props.initial.has_token}
            />
          </SettingsFormGrid>
          <p className='text-muted-foreground text-xs'>
            {t(
              'Enter a replacement token when changing upload endpoints. Token prefixes preserve spaces, for example Bearer followed by one space.'
            )}
          </p>
          <div className='space-y-2'>
            <Label htmlFor='image-hosting-extra-fields'>
              {t('Extra upload fields (JSON)')}
            </Label>
            <Textarea
              id='image-hosting-extra-fields'
              rows={3}
              {...form.register('extra_fields_json')}
              aria-invalid={Boolean(form.formState.errors.extra_fields_json)}
            />
            {form.formState.errors.extra_fields_json && (
              <p role='alert' className='text-destructive text-xs'>
                {t(
                  'Extra upload fields must be a JSON object with string, number or boolean values.'
                )}
              </p>
            )}
          </div>
          <SettingsFormGrid>
            <ImageSelect
              label={t('URL upload failure fallback')}
              value={form.watch('url_fallback')}
              options={[
                {
                  value: 'local',
                  label: t('Save locally and use the local link'),
                },
                {
                  value: 'upstream',
                  label: t('Use the original upstream URL when eligible'),
                },
              ]}
              onChange={(value) =>
                form.setValue('url_fallback', value as 'local' | 'upstream', {
                  shouldDirty: true,
                })
              }
              disabled={busy}
            />
            {LIMIT_FIELDS.map((field) => (
              <div key={field.name} className='space-y-2'>
                <Label htmlFor={`image-hosting-${field.name}`}>
                  {t(field.label)}
                </Label>
                <Input
                  id={`image-hosting-${field.name}`}
                  type='number'
                  min={field.min}
                  max={field.max}
                  step={1}
                  aria-invalid={Boolean(form.formState.errors[field.name])}
                  {...form.register(field.name, { valueAsNumber: true })}
                />
                {form.formState.errors[field.name] && (
                  <p role='alert' className='text-destructive text-xs'>
                    {t('Enter an integer from {{min}} to {{max}}.', {
                      min: field.min,
                      max: field.max,
                    })}
                  </p>
                )}
              </div>
            ))}
          </SettingsFormGrid>
          <p className='text-muted-foreground text-xs'>
            {t(
              'Base64 results fall back to their saved local file. Unusable upstream links also fall back locally. Upload retries do not generate or charge for new images.'
            )}
          </p>
          <SettingsSwitchField
            controlId='image-hosting-private-network'
            label={t('Allow image host private network access')}
            description={t(
              'Enable only for a trusted image host on your private network. HTTPS is still required.'
            )}
            checked={form.watch('allow_private_network')}
            onCheckedChange={(checked) =>
              form.setValue('allow_private_network', checked, {
                shouldDirty: true,
              })
            }
            disabled={busy}
          />
        </fieldset>
        {save.isError && !isConfigurationConflict(save.error) && (
          <Alert variant='destructive'>
            <AlertDescription>{save.error.message}</AlertDescription>
          </Alert>
        )}
        <ConfigurationConflict error={save.error} onReload={props.onReload} />
        <Button type='submit' disabled={busy}>
          {t(busy ? 'Saving...' : 'Save image hosting settings')}
        </Button>
      </form>
    </SettingsCard>
  )
}
