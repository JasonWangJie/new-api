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
import { useMemo, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'

import { ConfirmDialog } from '@/components/confirm-dialog'
import {
  StaticDataTable,
  type StaticDataTableColumn,
} from '@/components/data-table'
import { JsonCodeEditor } from '@/components/json-code-editor'
import {
  Accordion,
  AccordionContent,
  AccordionItem,
  AccordionTrigger,
} from '@/components/ui/accordion'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Checkbox } from '@/components/ui/checkbox'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table'
import { Tabs, TabsList, TabsTrigger } from '@/components/ui/tabs'
import { Textarea } from '@/components/ui/textarea'
import { SettingsCard } from '@/features/system-settings/components/settings-card'
import { SettingsSwitchField } from '@/features/system-settings/components/settings-form-layout'
import { hasPermission } from '@/lib/admin-permissions'
import { handleServerError } from '@/lib/handle-server-error'
import { useAuthStore } from '@/stores/auth-store'

import { imageRequest } from './api'
import { ImageGroupSelect } from './components/image-group-select'
import { ImagePolicyExampleDialog } from './components/image-policy-example-dialog'
import {
  ImagePoolEditor,
  type ImagePoolBinding,
} from './components/image-pool-editor'
import { ImageRoutingPreview } from './components/image-routing-preview'
import { ImageSelect } from './components/image-select'
import {
  imageLabel,
  imagePlatformLabel,
  imageRoutingPlatforms,
} from './lib/image-labels'
import type { ImageAdminConfiguration, ImageStorageProfile } from './types'
import { VideoConfiguration } from './video-settings'

type ImageConfiguration = ImageAdminConfiguration
type StorageProfile = ImageStorageProfile
type RuntimeField = {
  key: string
  label: string
  min?: number
  max?: number
}
const RUNTIME_FIELDS: readonly RuntimeField[] = [
  {
    key: 'failed_request_retention_hours',
    label: 'Failed request retention (hours)',
    min: 0,
    max: 168,
  },
  {
    key: 'account_cooldown_seconds',
    label: 'Account cooldown (seconds)',
    min: 0,
    max: 3600,
  },
  {
    key: 'supplier_retry_capacity',
    label: 'Supplier retry capacity',
    min: 0,
    max: 1000,
  },
  {
    key: 'supplier_retry_window_seconds',
    label: 'Supplier retry window (seconds)',
    min: 1,
    max: 3600,
  },
  { key: 'worker_concurrency', label: 'Workers', min: 1 },
  { key: 'image_concurrency', label: 'Image concurrency', min: 1 },
  {
    key: 'max_reference_images',
    label: 'Maximum reference images',
    min: 1,
    max: 128,
  },
  {
    key: 'worker_lease_seconds',
    label: 'Lease duration (seconds)',
    min: 1,
  },
  {
    key: 'execution_timeout_seconds',
    label: 'Execution timeout (seconds)',
    min: 1,
  },
  {
    key: 'account_attempt_timeout_seconds',
    label: 'Attempt timeout (seconds)',
    min: 1,
  },
  {
    key: 'signed_url_expiry_seconds',
    label: 'Signed URL lifetime (seconds)',
    min: 1,
  },
  { key: 'input_retention_hours', label: 'Input retention (hours)', min: 1 },
  { key: 'task_retention_days', label: 'Task retention (days)', min: 1 },
  { key: 'result_retention_days', label: 'Result retention (days)', min: 1 },
  { key: 'reference_fetch_max_retries', label: 'Reference retries', min: 0 },
  { key: 'storage_retry_attempts', label: 'Storage retries', min: 0 },
  { key: 'billing_retry_attempts', label: 'Billing retries', min: 0 },
]

type RuntimeFieldReference = {
  key: string
  description: string
}

const RUNTIME_FIELD_REFERENCES: readonly RuntimeFieldReference[] = [
  {
    key: 'failed_request_retention_hours',
    description:
      'Hours to retain failed requests encrypted for administrator diagnostics. 0 disables retention.',
  },
  {
    key: 'account_cooldown_seconds',
    description:
      'Cooldown after confirmed rate limits or temporary account failures. 0 disables account cooldown.',
  },
  {
    key: 'supplier_retry_capacity',
    description:
      'Shared retry tokens per supplier origin. Initial submissions and accepted-job polls do not consume tokens. 0 disables this budget.',
  },
  {
    key: 'supplier_retry_window_seconds',
    description:
      'Seconds to refill the supplier retry bucket from empty to full.',
  },
  {
    key: 'async_enabled',
    description: 'Enables asynchronous image task APIs and workers.',
  },
  {
    key: 'auto_archive_to_library',
    description:
      "Automatically adds completed asynchronous results to the user's image library.",
  },
  {
    key: 'worker_concurrency',
    description: 'Number of asynchronous task workers running on this node.',
  },
  {
    key: 'worker_lease_seconds',
    description: 'Seconds before an inactive worker lease can be recovered.',
  },
  {
    key: 'recovery_interval_seconds',
    description:
      'Seconds between scans for expired worker leases and recoverable tasks.',
  },
  {
    key: 'execution_timeout_seconds',
    description: 'Maximum total execution time for one image task, in seconds.',
  },
  {
    key: 'account_attempt_timeout_seconds',
    description: 'Maximum time for one upstream account attempt, in seconds.',
  },
  {
    key: 'mixed_timeout_retry_enabled',
    description:
      'Disabled by default. New mixed-pool attempts may retry within existing timeout limits, which can cause duplicate upstream generation and charges. Historical unknown tasks are not restarted.',
  },
  {
    key: 'image_concurrency',
    description:
      'Maximum image generations executed concurrently on this node.',
  },
  {
    key: 'storage_retry_attempts',
    description: 'Maximum retries for storing generated output.',
  },
  {
    key: 'billing_retry_attempts',
    description: 'Maximum retries for final billing settlement.',
  },
  {
    key: 'retry_backoff_seconds',
    description: 'Base delay used by general retry scheduling, in seconds.',
  },
  {
    key: 'openai_reference_transport_mode',
    description:
      'How OpenAI references are sent: upstream URL, server download, or URL with local fallback.',
  },
  {
    key: 'gemini_reference_transport_mode',
    description:
      'How Gemini references are sent: upstream URL, server download, or URL with local fallback.',
  },
  {
    key: 'gemini_async_max_account_switches',
    description: 'Maximum Gemini account switches during one task.',
  },
  {
    key: 'image_circuit_breaker_enabled',
    description:
      'Temporarily stops routing to repeatedly failing image channels.',
  },
  {
    key: 'failure_threshold',
    description:
      'Consecutive failures required to open an image channel circuit.',
  },
  {
    key: 'cooldown_seconds',
    description: 'Seconds an opened image channel circuit remains unavailable.',
  },
  {
    key: 'reference_fetch_max_retries',
    description: 'Maximum retries for temporary reference download failures.',
  },
  {
    key: 'reference_retry_base_seconds',
    description:
      'Initial delay before retrying a reference download, in seconds.',
  },
  {
    key: 'reference_retry_max_seconds',
    description:
      'Maximum delay between reference download retries, in seconds.',
  },
  {
    key: 'upstream_transient_max_retries',
    description: 'Maximum retries for temporary upstream service failures.',
  },
  {
    key: 'upstream_transient_retry_base_seconds',
    description:
      'Initial delay before retrying a temporary upstream failure, in seconds.',
  },
  {
    key: 'upstream_transient_retry_max_seconds',
    description:
      'Maximum delay between temporary upstream retries, in seconds.',
  },
  {
    key: 'capacity_max_retries',
    description:
      'Maximum retries while image execution capacity is unavailable.',
  },
  {
    key: 'capacity_retry_base_seconds',
    description:
      'Initial delay before retrying unavailable execution capacity, in seconds.',
  },
  {
    key: 'capacity_retry_max_seconds',
    description:
      'Maximum delay between execution-capacity retries, in seconds.',
  },
  {
    key: 'total_max_retries',
    description:
      'Maximum retries allowed across all retry categories for one task.',
  },
  {
    key: 'retry_jitter_percent',
    description:
      'Random percentage added to retry delays to avoid synchronized retries.',
  },
  {
    key: 'retry_after_max_seconds',
    description:
      'Maximum Retry-After delay accepted from an upstream response, in seconds.',
  },
  {
    key: 'download_max_bytes',
    description: 'Maximum downloaded bytes allowed for each reference image.',
  },
  {
    key: 'download_max_pixels',
    description: 'Maximum decoded pixels allowed for each reference image.',
  },
  {
    key: 'max_reference_images',
    description:
      'Global reference-image count limit; the effective limit is the lowest applicable model, channel, and global limit.',
  },
  {
    key: 'download_timeout_seconds',
    description: 'Timeout for downloading one reference image, in seconds.',
  },
  {
    key: 'download_max_redirects',
    description:
      'Maximum redirects followed while downloading a reference image.',
  },
  {
    key: 'reference_fetch_concurrency',
    description:
      'Maximum reference images downloaded concurrently on this node.',
  },
  {
    key: 'reference_cache_ttl_seconds',
    description:
      'Seconds downloaded reference bytes remain in the shared cache; 0 disables retention.',
  },
  {
    key: 'reference_cache_max_bytes',
    description:
      'Maximum bytes stored in the shared reference cache; 0 disables the cache.',
  },
  {
    key: 'upload_timeout_seconds',
    description: 'Timeout for completing one staged input upload, in seconds.',
  },
  {
    key: 'upload_per_minute',
    description: 'Maximum input uploads accepted per API key each minute.',
  },
  {
    key: 'max_input_bytes_per_key',
    description: 'Maximum retained input bytes per API key.',
  },
  {
    key: 'max_upload_bytes',
    description: 'Maximum bytes allowed for one uploaded image.',
  },
  {
    key: 'signed_url_expiry_seconds',
    description: 'Lifetime of generated signed image URLs, in seconds.',
  },
  {
    key: 'input_retention_hours',
    description: 'Hours uploaded input images are retained.',
  },
  {
    key: 'task_retention_days',
    description: 'Days image task records are retained.',
  },
  {
    key: 'result_retention_days',
    description: 'Days generated result objects are retained.',
  },
  {
    key: 'prompt_preview_enabled',
    description: 'Shows a shortened prompt summary in task lists and details.',
  },
  {
    key: 'prompt_preview_max_chars',
    description: 'Maximum characters stored in a prompt summary.',
  },
  {
    key: 'library_retention_days',
    description: 'Days archived library images are retained.',
  },
  {
    key: 'library_max_items_per_user',
    description: 'Maximum archived image items per user.',
  },
  {
    key: 'library_max_bytes_per_user',
    description: 'Maximum total archived image bytes per user.',
  },
  {
    key: 'library_max_image_bytes',
    description:
      'Maximum bytes allowed for one image imported into the library.',
  },
  {
    key: 'library_max_image_pixels',
    description:
      'Maximum decoded pixels allowed for one image imported into the library.',
  },
  {
    key: 'library_import_per_minute',
    description: 'Maximum external image imports per user each minute.',
  },
  {
    key: 'library_submission_per_minute',
    description: 'Maximum library submission operations per user each minute.',
  },
]

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
        {t('Media settings require administrator storage permissions.')}
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
            { id: 'runtime', label: 'Image runtime' },
            { id: 'storage', label: 'Image storage' },
            { id: 'policy', label: 'Image policies' },
            { id: 'video', label: 'Video configuration' },
            { id: 'pool', label: 'Channel pools' },
            { id: 'routing', label: 'Routing preview' },
          ].map((item) => (
            <TabsTrigger key={item.id} value={item.id}>
              {t(item.label)}
            </TabsTrigger>
          ))}
        </TabsList>
      </Tabs>
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
      {tab === 'video' && (
        <VideoConfiguration
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
      {tab === 'routing' && <ImageRoutingPreview config={config} />}
    </div>
  )
}

export function ImageRuntimeForm({
  initial,
  onSuccess,
}: {
  initial: ImageConfiguration['runtime']
  onSuccess: () => Promise<unknown>
}) {
  const { t } = useTranslation()
  const [values, setValues] = useState<ImageConfiguration['runtime']>(() => ({
    mixed_timeout_retry_enabled: false,
    ...initial,
  }))
  const [advanced, setAdvanced] = useState('')
  const [busy, setBusy] = useState(false)
  const advancedValues = useMemo(() => {
    if (!advanced) return values
    try {
      const parsed: unknown = JSON.parse(advanced)
      if (parsed && typeof parsed === 'object' && !Array.isArray(parsed)) {
        return parsed as ImageConfiguration['runtime']
      }
    } catch {
      // Keep the last valid form values visible while the JSON draft is invalid.
    }
    return values
  }, [advanced, values])
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
        <SettingsSwitchField
          controlId='mixed_timeout_retry_enabled'
          checked={values.mixed_timeout_retry_enabled === true}
          onCheckedChange={(checked) => {
            setValues((previous) => ({
              ...previous,
              mixed_timeout_retry_enabled: checked,
            }))
            setAdvanced('')
          }}
          label={t('Allow timeout retries in mixed image pools')}
          description={t(
            'Disabled by default. New mixed-pool attempts may retry within existing timeout limits, which can cause duplicate upstream generation and charges. Historical unknown tasks are not restarted.'
          )}
          disabled={busy}
        />
        <div className='grid gap-3 sm:grid-cols-2 lg:grid-cols-3'>
          {RUNTIME_FIELDS.map(({ key, label, min, max }) => (
            <div key={key} className='space-y-2'>
              <Label htmlFor={key}>{t(label)}</Label>
              <Input
                id={key}
                type='number'
                min={min}
                max={max}
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
          <div className='space-y-3'>
            <Label htmlFor='image-runtime-advanced'>
              {t('Complete runtime configuration')}
            </Label>
            <p
              id='image-runtime-advanced-help'
              className='text-muted-foreground text-sm'
            >
              {t(
                'Use strict JSON. Comments such as // or /* */ are not supported.'
              )}
            </p>
            <JsonCodeEditor
              id='image-runtime-advanced'
              value={advanced}
              onChange={setAdvanced}
              disabled={busy}
              heightClassName='h-72 min-h-72 max-h-72'
              ariaLabel={t('Complete runtime configuration')}
              aria-describedby='image-runtime-advanced-help'
            />
            <Accordion>
              <AccordionItem value='runtime-field-reference'>
                <AccordionTrigger>
                  {t('Runtime field reference')}
                </AccordionTrigger>
                <AccordionContent>
                  <Table>
                    <TableHeader>
                      <TableRow>
                        <TableHead>{t('Field')}</TableHead>
                        <TableHead>{t('Description')}</TableHead>
                        <TableHead>{t('Current value')}</TableHead>
                      </TableRow>
                    </TableHeader>
                    <TableBody>
                      {RUNTIME_FIELD_REFERENCES.map((field) => (
                        <TableRow key={field.key}>
                          <TableCell>
                            <code className='text-xs'>{field.key}</code>
                          </TableCell>
                          <TableCell className='min-w-80 whitespace-normal'>
                            {t(field.description)}
                          </TableCell>
                          <TableCell>
                            <code className='text-xs'>
                              {String(advancedValues[field.key])}
                            </code>
                          </TableCell>
                        </TableRow>
                      ))}
                    </TableBody>
                  </Table>
                </AccordionContent>
              </AccordionItem>
            </Accordion>
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
  const user = useAuthStore((state) => state.auth.user)
  const canManage = hasPermission(user, 'image_config', 'manage')
  const queryClient = useQueryClient()
  const [deleting, setDeleting] = useState<
    ImageConfiguration['policies'][number] | null
  >(null)
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
          media_type: 'image',
          qualities: ['low', 'medium', 'high'],
          resolutions: ['1K', '2K', '4K'],
          formats: ['png', 'jpeg', 'webp'],
          backgrounds: ['auto', 'opaque', 'transparent'],
          max_output_images: 128,
          max_reference_images: 14,
        },
      ],
      null,
      2
    )
  )
  const [busy, setBusy] = useState(false)
  const deletion = useMutation({
    mutationFn: (policy: ImageConfiguration['policies'][number]) =>
      imageRequest('/api/option/images/policy', 'DELETE', {
        group: policy.group,
        platform: policy.platform,
        version: policy.version,
      }),
    onSuccess: async () => {
      setDeleting(null)
      await Promise.all([
        onSuccess(),
        queryClient.invalidateQueries({ queryKey: ['image-pool-options'] }),
        queryClient.invalidateQueries({ queryKey: ['image-capabilities'] }),
      ])
      toast.success(t('Deleted'))
    },
    onError: (error) => handleServerError(error, t('Image request failed')),
  })
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
        async_enabled: platform === 'mixed' || asyncEnabled,
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
          <ImageGroupSelect
            config={config}
            value={group}
            onChange={setGroup}
            disabled={busy || deletion.isPending}
          />
          <ImageSelect
            label={t('Platform')}
            value={platform}
            options={imageRoutingPlatforms(
              config?.routing_platforms ||
                config?.media_providers ||
                config?.image_providers
            ).map((value) => ({
              value,
              label: t(imagePlatformLabel(value)),
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
        {platform === 'mixed' && (
          <p role='status' className='text-muted-foreground text-sm'>
            {t(
              'Mixed scheduling accepts asynchronous image requests without provider. Explicit provider requests keep their platform routing.'
            )}
          </p>
        )}
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
              checked={platform === 'mixed' || asyncEnabled}
              disabled={platform === 'mixed'}
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
        <Button disabled={busy || deletion.isPending} type='submit'>
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
              cell: (item) => t(imagePlatformLabel(item.platform)),
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
                <div className='flex flex-wrap gap-2'>
                  <Button
                    type='button'
                    size='sm'
                    variant='outline'
                    disabled={busy || deletion.isPending}
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
                  <Button
                    type='button'
                    size='sm'
                    variant='destructive'
                    disabled={!canManage || busy || deletion.isPending}
                    onClick={() => setDeleting(item)}
                  >
                    {t('Delete')}
                  </Button>
                </div>
              ),
            },
          ]}
        />
      </form>
      <ConfirmDialog
        open={deleting !== null}
        onOpenChange={(open) => {
          if (!open && !deletion.isPending) setDeleting(null)
        }}
        title={t('Delete platform policy')}
        desc={
          <div className='flex flex-col gap-2'>
            <p className='break-words'>
              {deleting?.group} / {deleting?.platform}
            </p>
            <p>
              {t(
                'This removes the platform settings and model capabilities for this group. Channel pool bindings are kept.'
              )}
            </p>
          </div>
        }
        disabled={!canManage || busy}
        confirmText={t('Delete')}
        destructive
        isLoading={deletion.isPending}
        handleConfirm={() => {
          if (deleting && canManage && !busy && !deletion.isPending) {
            deletion.mutate(deleting)
          }
        }}
      />
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
  const user = useAuthStore((state) => state.auth.user)
  const canManage = hasPermission(user, 'image_config', 'manage')
  const queryClient = useQueryClient()
  const [deleting, setDeleting] = useState<
    ImageConfiguration['pools'][number] | null
  >(null)
  const [editing, setEditing] = useState<ImagePoolBinding | null>()
  const [expanded, setExpanded] = useState<string[]>([])
  const deletion = useMutation({
    mutationFn: (binding: ImageConfiguration['pools'][number]) =>
      imageRequest(
        `/api/option/images/pool/${encodeURIComponent(binding.binding_key)}`,
        'DELETE'
      ),
    onSuccess: async () => {
      setDeleting(null)
      await Promise.all([
        onSuccess(),
        queryClient.invalidateQueries({ queryKey: ['image-pool-options'] }),
        queryClient.invalidateQueries({ queryKey: ['image-capabilities'] }),
      ])
      toast.success(t('Deleted'))
    },
    onError: (error) => handleServerError(error, t('Image request failed')),
  })
  const bindings = useMemo(() => {
    const rowsByBinding = [
      ...new Set(config?.pools.map((pool) => pool.binding_key) || []),
    ].flatMap((key) => {
      const rows =
        config?.pools.filter((pool) => pool.binding_key === key) || []
      const first = rows[0]
      if (!first) return []
      return [
        {
          ...first,
          channels: rows.filter((pool) => pool.channel_id > 0),
          rowIds: rows.map((pool) => pool.id),
        },
      ]
    })
    const collator = new Intl.Collator('en', { numeric: true })
    return rowsByBinding.sort(
      (a, b) =>
        collator.compare(a.model, b.model) ||
        collator.compare(a.resolution, b.resolution)
    )
  }, [config?.pools])
  const sections = useMemo(() => {
    const grouped = new Map<
      string,
      {
        key: string
        group: string
        platform: string
        bindings: ImagePoolBinding[]
      }
    >()
    for (const binding of bindings) {
      const key = JSON.stringify([binding.group, binding.platform])
      const section = grouped.get(key)
      if (section) {
        section.bindings.push(binding)
      } else {
        grouped.set(key, {
          key,
          group: binding.group,
          platform: binding.platform,
          bindings: [binding],
        })
      }
    }
    const collator = new Intl.Collator('en', { numeric: true })
    return [...grouped.values()].sort(
      (a, b) =>
        collator.compare(a.group, b.group) ||
        collator.compare(a.platform, b.platform)
    )
  }, [bindings])
  const expandedKeys = new Set(expanded)
  const allExpanded =
    sections.length > 0 &&
    sections.every((section) => expandedKeys.has(section.key))
  const columns: StaticDataTableColumn<ImagePoolBinding>[] = [
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
      cell: (binding) => binding.resolution || t('All resolutions'),
    },
    {
      id: 'channels',
      header: t('Channel'),
      cell: (binding) =>
        binding.channels
          .map((row) => `${row.channel_id} (${Math.max(0, row.priority)})`)
          .join(', ') || t('Disabled'),
    },
    {
      id: 'edit',
      header: t('Actions'),
      cell: (binding) => (
        <div className='flex flex-wrap gap-2'>
          <Button
            type='button'
            size='sm'
            variant='outline'
            disabled={deletion.isPending}
            onClick={() => setEditing(binding)}
          >
            {t('Edit')}
          </Button>
          <Button
            type='button'
            size='sm'
            variant='destructive'
            disabled={!canManage || deletion.isPending}
            onClick={() => setDeleting(binding)}
          >
            {t('Delete')}
          </Button>
        </div>
      ),
    },
  ]
  return (
    <SettingsCard
      title={t('Channel pools')}
      description={t(
        'Choose channels already enabled for this group and model. An empty list explicitly closes this binding.'
      )}
    >
      <div className='mb-4 flex flex-wrap items-center justify-between gap-3'>
        <span className='text-muted-foreground flex items-center gap-2 text-sm'>
          {t('Pool binding')}
          <Badge variant='secondary'>{bindings.length}</Badge>
        </span>
        <div className='flex flex-wrap items-center gap-2'>
          <Button
            type='button'
            variant='outline'
            disabled={sections.length === 0}
            onClick={() =>
              setExpanded(
                allExpanded ? [] : sections.map((section) => section.key)
              )
            }
          >
            {allExpanded ? t('Collapse all') : t('Expand all')}
          </Button>
          <Button
            type='button'
            disabled={!canManage || deletion.isPending}
            onClick={() => setEditing(null)}
          >
            {t('Add channel pool binding')}
          </Button>
        </div>
      </div>
      {editing !== undefined && (
        <ImagePoolEditor
          config={config}
          binding={editing}
          canManage={canManage}
          onSuccess={onSuccess}
          onClose={() => setEditing(undefined)}
        />
      )}
      {sections.length === 0 ? (
        <StaticDataTable data={[]} columns={columns} />
      ) : (
        <Accordion
          multiple
          value={expanded}
          onValueChange={setExpanded}
          className='gap-3'
        >
          {sections.map((section) => (
            <AccordionItem
              key={section.key}
              value={section.key}
              className='rounded-lg border last:border-b'
            >
              <AccordionTrigger className='items-center gap-3 px-4 py-3 hover:no-underline'>
                <span className='flex min-w-0 flex-1 flex-wrap items-center gap-2'>
                  <span className='min-w-0 font-semibold break-all'>
                    {section.group}
                  </span>
                  <Badge
                    variant='outline'
                    className='h-auto min-h-5 max-w-full break-all whitespace-normal'
                  >
                    {t(imagePlatformLabel(section.platform))}
                  </Badge>
                </span>
                <Badge
                  variant='secondary'
                  aria-label={`${t('Pool binding')}: ${section.bindings.length}`}
                >
                  {section.bindings.length}
                </Badge>
              </AccordionTrigger>
              <AccordionContent className='px-3 pb-3'>
                <StaticDataTable
                  data={section.bindings}
                  getRowKey={(binding) => binding.binding_key}
                  tableProps={{
                    'aria-label': `${section.group} / ${section.platform}`,
                  }}
                  columns={columns}
                />
              </AccordionContent>
            </AccordionItem>
          ))}
        </Accordion>
      )}
      <ConfirmDialog
        open={deleting !== null}
        onOpenChange={(open) => {
          if (!open && !deletion.isPending) setDeleting(null)
        }}
        title={t('Delete channel pool binding')}
        desc={
          <div className='flex flex-col gap-2'>
            <p className='break-words'>
              {deleting?.group} / {deleting?.platform} ·{' '}
              {deleting && t(imageLabel(deleting.mode))}
              {deleting?.model && ` · ${deleting.model}`}
              {deleting?.resolution && ` · ${deleting.resolution}`}
            </p>
            <p>
              {t(
                'Deleting this binding restores automatic channel selection. To close the binding, save an empty channel list instead.'
              )}
            </p>
          </div>
        }
        disabled={!canManage}
        confirmText={t('Delete')}
        destructive
        isLoading={deletion.isPending}
        handleConfirm={() => {
          if (deleting && canManage && !deletion.isPending) {
            deletion.mutate(deleting)
          }
        }}
      />
    </SettingsCard>
  )
}
