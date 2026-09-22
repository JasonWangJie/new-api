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
import { useState } from 'react'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'

import { Button } from '@/components/ui/button'
import { Checkbox } from '@/components/ui/checkbox'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { SettingsCard } from '@/features/system-settings/components/settings-card'

import { imageRequest } from './api'

type MediaSettings = {
  video_async_enabled: boolean
  local_path: string
  retention_days: number
  signed_url_expiry_seconds: number
  max_file_bytes: number
  download_timeout_seconds: number
  download_concurrency: number
  storage_retry_attempts: number
}

export function MediaSettingsCard(
  props: {
    onSuccess?: () => Promise<unknown> | unknown
  } = {}
) {
  const settings = useQuery({
    queryKey: ['media-settings'],
    queryFn: () => imageRequest<MediaSettings>('/api/option/images/media'),
  })
  if (settings.error) {
    return (
      <p role='alert' className='text-destructive text-sm'>
        {settings.error.message}
      </p>
    )
  }
  return settings.data ? (
    <MediaSettingsForm
      key={JSON.stringify(settings.data)}
      initial={settings.data}
      onSuccess={props.onSuccess}
    />
  ) : null
}

function MediaSettingsForm(props: {
  initial: MediaSettings
  onSuccess?: () => Promise<unknown> | unknown
}) {
  const { t } = useTranslation()
  const queryClient = useQueryClient()
  const [values, setValues] = useState(props.initial)
  const save = useMutation({
    mutationFn: () => imageRequest('/api/option/images/media', 'PUT', values),
    onSuccess: async () => {
      toast.success(t('Saved'))
      void queryClient.invalidateQueries({ queryKey: ['media-settings'] })
      await props.onSuccess?.()
    },
  })
  const fields: {
    key: keyof Omit<MediaSettings, 'video_async_enabled' | 'local_path'>
    label: string
  }[] = [
    { key: 'retention_days', label: 'Video retention (days)' },
    { key: 'signed_url_expiry_seconds', label: 'Link validity (seconds)' },
    { key: 'max_file_bytes', label: 'Maximum video file size (bytes)' },
    {
      key: 'download_timeout_seconds',
      label: 'Video download timeout (seconds)',
    },
    { key: 'download_concurrency', label: 'Video download concurrency' },
    { key: 'storage_retry_attempts', label: 'Storage retries' },
  ]
  return (
    <SettingsCard
      title={t('Async video and local storage')}
      description={t(
        'Storage retries only save generated files and do not generate or charge again.'
      )}
    >
      <form
        className='space-y-4'
        onSubmit={(event) => {
          event.preventDefault()
          save.mutate()
        }}
      >
        <div className='flex items-center gap-2'>
          <Checkbox
            id='video-async-enabled'
            checked={values.video_async_enabled}
            onCheckedChange={(checked) =>
              setValues((previous) => ({
                ...previous,
                video_async_enabled: checked === true,
              }))
            }
          />
          <Label htmlFor='video-async-enabled'>
            {t('Enable async video generation')}
          </Label>
        </div>
        <div className='space-y-2'>
          <Label htmlFor='video-local-path'>{t('Local storage path')}</Label>
          <Input
            id='video-local-path'
            value={values.local_path}
            onChange={(event) =>
              setValues((previous) => ({
                ...previous,
                local_path: event.target.value,
              }))
            }
          />
        </div>
        <div className='grid gap-4 sm:grid-cols-2'>
          {fields.map((field) => (
            <div key={field.key} className='space-y-2'>
              <Label htmlFor={`video-${field.key}`}>{t(field.label)}</Label>
              <Input
                id={`video-${field.key}`}
                type='number'
                min={field.key === 'storage_retry_attempts' ? 0 : 1}
                value={values[field.key]}
                onChange={(event) =>
                  setValues((previous) => ({
                    ...previous,
                    [field.key]: Number(event.target.value),
                  }))
                }
              />
            </div>
          ))}
        </div>
        <Button type='submit' disabled={save.isPending}>
          {t('Save')}
        </Button>
        {save.error && (
          <p role='alert' className='text-destructive text-sm'>
            {save.error.message}
          </p>
        )}
      </form>
    </SettingsCard>
  )
}
