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
import { Braces, CircleCheck, CircleX } from 'lucide-react'
import { useEffect, useMemo, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'

import { CopyButton } from '@/components/copy-button'
import { Dialog } from '@/components/dialog'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Checkbox } from '@/components/ui/checkbox'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Textarea } from '@/components/ui/textarea'
import { SettingsCard } from '@/features/system-settings/components/settings-card'

import { imageRequest } from './api'
import { ImageSelect } from './components/image-select'
import { MediaSettingsCard } from './media-settings'
import type { ImageAdminConfiguration, VideoPluginTemplate } from './types'

const XAI_EXAMPLES = `# Platform policy
[
  {
    "id": "grok-imagine-video-1.5",
    "label": "Grok Imagine Video 1.5",
    "media_type": "video",
    "video_overrides": {}
  },
  {
    "id": "grok-imagine-video",
    "label": "Grok Imagine Video · edit and extend",
    "media_type": "video",
    "video_overrides": {
      "modes": ["edit_video", "extend_video"],
      "duration": {"max": 8, "default": 6},
      "extension_directions": ["backward"],
      "default_extension_direction": "backward"
    }
  }
]

# Text to video
curl -X POST "$NEW_API_BASE/v1/videos/generations_async" \\
  -H "Authorization: Bearer $NEW_API_KEY" \\
  -H "Content-Type: application/json" \\
  -d '{"provider":"xai","model":"grok-imagine-video-1.5","prompt":"Ocean sunrise","seconds":8,"resolution":"1080p","aspect_ratio":"16:9"}'

# Image to video
curl -X POST "$NEW_API_BASE/v1/videos/generations_async" \\
  -H "Authorization: Bearer $NEW_API_KEY" \\
  -H "Content-Type: application/json" \\
  -d '{"provider":"xai","model":"grok-imagine-video-1.5","prompt":"Camera pushes in","image":{"url":"https://cdn.example/start.png"},"seconds":8,"resolution":"720p"}'

# Reference images and preset voices
curl -X POST "$NEW_API_BASE/v1/videos/generations_async" \\
  -H "Authorization: Bearer $NEW_API_KEY" \\
  -H "Content-Type: application/json" \\
  -d '{"provider":"xai","model":"grok-imagine-video-1.5","prompt":"Keep the character consistent","reference_images":[{"url":"https://cdn.example/a.png"},{"file_id":"file_123"}],"reference_audios":[{"voice_id":"Ara"}],"seconds":8,"resolution":"720p"}'

# First and last frame
curl -X POST "$NEW_API_BASE/v1/videos/generations_async" \\
  -H "Authorization: Bearer $NEW_API_KEY" \\
  -H "Content-Type: application/json" \\
  -d '{"provider":"xai","model":"grok-imagine-video-1.5","prompt":"Transition between frames","image":{"url":"https://cdn.example/first.png"},"last_frame":{"url":"https://cdn.example/last.png"},"seconds":8,"resolution":"720p"}'

# Edit a classic-model video (durable endpoint)
curl -X POST "$NEW_API_BASE/v1/videos/edits_async" \\
  -H "Authorization: Bearer $NEW_API_KEY" \\
  -H "Content-Type: application/json" \\
  -H "Idempotency-Key: xai-edit-001" \\
  -d '{"provider":"xai","model":"grok-imagine-video","prompt":"Replace the sky with a sunset","video":{"url":"https://cdn.example/source.mp4"}}'

# Extend a classic-model video backward by 6 seconds
curl -X POST "$NEW_API_BASE/v1/videos/extensions_async" \\
  -H "Authorization: Bearer $NEW_API_KEY" \\
  -H "Content-Type: application/json" \\
  -H "Idempotency-Key: xai-extend-001" \\
  -d '{"provider":"xai","model":"grok-imagine-video","prompt":"Continue the camera pullback","video":{"file_id":"file_123"},"seconds":6,"extension_direction":"backward"}'

# Query a task returned by submission
curl "$NEW_API_BASE/v1/media/tasks_async/$TASK_ID" \\
  -H "Authorization: Bearer $NEW_API_KEY"`

const SEEDANCE_EXAMPLES = `# Platform policy
[
  {
    "id": "doubao-seedance-2-0-260128",
    "label": "Seedance 2.0",
    "media_type": "video",
    "video_overrides": {
      "modes": ["edit_video", "extend_video"],
      "duration": {"max": 12, "default": 8},
      "extension_directions": ["forward", "backward"],
      "default_extension_direction": "backward",
      "max_input_items": {"reference_images": 6}
    }
  }
]

# Text to video
curl -X POST "$NEW_API_BASE/v1/videos/generations_async" \\
  -H "Authorization: Bearer $NEW_API_KEY" \\
  -H "Content-Type: application/json" \\
  -d '{"provider":"doubao","model":"doubao-seedance-2-0-260128","prompt":"A paper boat crosses a neon canal","duration":8,"resolution":"1080p","aspect_ratio":"16:9","generate_audio":true}'

# Image to video (public URL or asset:// only)
curl -X POST "$NEW_API_BASE/v1/videos/generations_async" \\
  -H "Authorization: Bearer $NEW_API_KEY" \\
  -H "Content-Type: application/json" \\
  -d '{"provider":"doubao","model":"doubao-seedance-2-0-260128","prompt":"Slow orbit shot","image":"https://cdn.example/first.png","duration":8,"resolution":"720p","return_last_frame":true}'

# Multimodal references
curl -X POST "$NEW_API_BASE/v1/videos/generations_async" \\
  -H "Authorization: Bearer $NEW_API_KEY" \\
  -H "Content-Type: application/json" \\
  -d '{"provider":"doubao","model":"doubao-seedance-2-5-260628","prompt":"Use the visual rhythm and soundtrack","reference_images":["asset://character"],"reference_videos":["https://cdn.example/motion.mp4"],"reference_audios":["https://cdn.example/music.mp3"],"duration":12,"resolution":"720p"}'

# Edit with the primary video first, plus multimodal references
curl -X POST "$NEW_API_BASE/v1/videos/edits_async" \\
  -H "Authorization: Bearer $NEW_API_KEY" \\
  -H "Content-Type: application/json" \\
  -H "Idempotency-Key: seedance-edit-001" \\
  -d '{"provider":"doubao","model":"doubao-seedance-2-0-260128","prompt":"Change the coat to red","video":"https://cdn.example/source.mp4","reference_images":["asset://red-coat"],"duration":8,"resolution":"720p","generate_audio":true}'

# Extend forward; direct URL or asset:// source video only
curl -X POST "$NEW_API_BASE/v1/videos/extensions_async" \\
  -H "Authorization: Bearer $NEW_API_KEY" \\
  -H "Content-Type: application/json" \\
  -H "Idempotency-Key: seedance-extend-001" \\
  -d '{"provider":"doubao","model":"doubao-seedance-2-5-260628","prompt":"Reveal the city beyond the gate","video":"asset://source-video","duration":12,"extension_direction":"forward","resolution":"720p","watermark":false,"metadata":{"output_format":"mov"}}'

# Query a task returned by submission
curl "$NEW_API_BASE/v1/media/tasks_async/$TASK_ID" \\
  -H "Authorization: Bearer $NEW_API_KEY"`

function readinessLabel(key: string): string {
  const labels: Record<string, string> = {
    video_async_enabled: 'Global video switch',
    task_plugins: 'Video task plugins',
    redis: 'Redis queue',
    payload_encryption: 'Payload encryption key',
    url_signing: 'Media signing key',
  }
  return labels[key] || key
}

function defaultVideoCatalog(template?: VideoPluginTemplate): string {
  return JSON.stringify(
    (template?.models || []).map((model) => ({
      id: model,
      label: model,
      media_type: 'video',
      video_overrides: {},
    })),
    null,
    2
  )
}

function poolModeLabel(value: string): string {
  if (value === 'model_resolution') return 'Model and resolution'
  if (value === 'model') return 'Model'
  return 'Resolution'
}

function VideoExamplesDialog(props: { provider: string }) {
  const { t } = useTranslation()
  const seedance = props.provider === 'doubao'
  const examples = seedance ? SEEDANCE_EXAMPLES : XAI_EXAMPLES
  return (
    <Dialog
      trigger={
        <Button type='button' variant='outline' size='sm'>
          <Braces aria-hidden='true' />
          {t('Video configuration examples')}
        </Button>
      }
      title={t('{{provider}} video policy and curl examples', {
        provider: seedance ? 'Seedance' : 'xAI',
      })}
      description={t(
        'Examples cover asynchronous generation, editing, extension, and the unified task query API.'
      )}
      contentClassName='sm:max-w-4xl'
      contentHeight='min(38rem, calc(100vh - 14rem))'
      footer={
        <CopyButton
          value={examples}
          variant='outline'
          size='default'
          tooltip={t('Copy example')}
          successTooltip={t('Copied!')}
          aria-label={t('Copy example')}
        >
          {t('Copy example')}
        </CopyButton>
      }
    >
      <pre
        role='region'
        aria-label={t('Video configuration examples')}
        className='border-border bg-muted/35 overflow-auto rounded-xl border p-4 font-mono text-xs leading-6'
      >
        <code>{examples}</code>
      </pre>
    </Dialog>
  )
}

export function VideoConfiguration(props: {
  config: ImageAdminConfiguration
  onSuccess: () => Promise<unknown>
}) {
  const { t } = useTranslation()
  const templates = props.config.video_plugin_templates || []
  const initialProvider =
    templates.find((item) => item.provider === 'xai')?.provider ||
    templates[0]?.provider ||
    'xai'
  const [group, setGroup] = useState('default')
  const [provider, setProvider] = useState(initialProvider)
  const [poolMode, setPoolMode] = useState('model_resolution')
  const [enabled, setEnabled] = useState(false)
  const [asyncEnabled, setAsyncEnabled] = useState(false)
  const [models, setModels] = useState('[]')
  const [busy, setBusy] = useState(false)
  const template = templates.find((item) => item.provider === provider)
  const current = useMemo(
    () =>
      props.config.policies.find(
        (item) => item.group === group && item.platform === provider
      ),
    [group, props.config.policies, provider]
  )

  useEffect(() => {
    setPoolMode(current?.pool_mode || 'model_resolution')
    setEnabled(current?.enabled || false)
    setAsyncEnabled(current?.async_enabled || false)
    setModels(current?.models || defaultVideoCatalog(template))
  }, [current, template])

  const importModels = () => {
    try {
      const parsed = JSON.parse(models) as unknown
      if (!Array.isArray(parsed)) {
        throw new Error(t('Model capabilities must be a JSON array'))
      }
      const importedIds = new Set(template?.models || [])
      const retained = parsed.filter((item) => {
        if (!item || typeof item !== 'object') return true
        const entry = item as { id?: unknown; media_type?: unknown }
        return (
          entry.media_type !== 'video' || !importedIds.has(String(entry.id))
        )
      })
      setModels(
        JSON.stringify(
          [
            ...retained,
            ...(template?.models || []).map((model) => ({
              id: model,
              label: model,
              media_type: 'video',
              video_overrides: {},
            })),
          ],
          null,
          2
        )
      )
    } catch (error) {
      toast.error(
        error instanceof Error ? error.message : t('Invalid video policy')
      )
    }
  }

  const save = async () => {
    setBusy(true)
    try {
      const parsed = JSON.parse(models) as unknown
      if (!Array.isArray(parsed)) {
        throw new Error(t('Model capabilities must be a JSON array'))
      }
      await imageRequest('/api/option/images/policy', 'PUT', {
        group,
        platform: provider,
        pool_mode: poolMode,
        enabled,
        async_enabled: asyncEnabled,
        models: JSON.stringify(parsed),
        version: current?.version || 0,
      })
      await props.onSuccess()
      toast.success(t('Saved'))
    } catch (error) {
      toast.error(
        error instanceof Error ? error.message : t('Invalid video policy')
      )
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className='space-y-4'>
      <SettingsCard
        title={t('Video readiness')}
        description={t(
          'All checks must pass before asynchronous video requests can be accepted.'
        )}
      >
        <div className='grid gap-2 sm:grid-cols-2'>
          {(props.config.video_readiness?.checks || []).map((check) => (
            <div
              key={check.key}
              className='border-border flex items-start gap-3 rounded-lg border p-3'
            >
              {check.ready ? (
                <CircleCheck
                  className='text-success mt-0.5 size-4'
                  aria-hidden='true'
                />
              ) : (
                <CircleX
                  className='text-destructive mt-0.5 size-4'
                  aria-hidden='true'
                />
              )}
              <div className='min-w-0'>
                <p className='text-sm font-medium'>
                  {t(readinessLabel(check.key))}
                </p>
                {!check.ready && check.reason && (
                  <p className='text-muted-foreground mt-1 text-xs'>
                    {t(check.reason)}
                  </p>
                )}
              </div>
            </div>
          ))}
        </div>
      </SettingsCard>
      <MediaSettingsCard onSuccess={props.onSuccess} />
      <SettingsCard
        title={t('Video platform policy')}
        description={t(
          'Video overrides may only narrow plugin modes, durations, resolutions, aspect ratios, extension directions, defaults, and reference limits.'
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
              <Label htmlFor='video-policy-group'>{t('Group')}</Label>
              <Input
                id='video-policy-group'
                value={group}
                onChange={(event) => setGroup(event.target.value)}
              />
            </div>
            <ImageSelect
              label={t('Provider')}
              value={provider}
              options={templates.map((item) => ({
                value: item.provider,
                label: `${item.name} · ${item.provider}`,
              }))}
              onChange={setProvider}
            />
            <ImageSelect
              label={t('Pool binding')}
              value={poolMode}
              options={['resolution', 'model', 'model_resolution'].map(
                (value) => ({
                  value,
                  label: t(poolModeLabel(value)),
                })
              )}
              onChange={setPoolMode}
            />
          </div>
          <div className='flex flex-wrap gap-4'>
            <div className='flex items-center gap-2'>
              <Checkbox
                id='video-policy-enabled'
                checked={enabled}
                onCheckedChange={(checked) => setEnabled(checked === true)}
              />
              <Label htmlFor='video-policy-enabled'>
                {t('Enable media platform')}
              </Label>
            </div>
            <div className='flex items-center gap-2'>
              <Checkbox
                id='video-policy-async'
                checked={asyncEnabled}
                onCheckedChange={(checked) => setAsyncEnabled(checked === true)}
              />
              <Label htmlFor='video-policy-async'>
                {t('Use asynchronous execution')}
              </Label>
            </div>
          </div>
          <div className='flex flex-wrap items-center justify-between gap-2'>
            <Label htmlFor='video-policy-models'>
              {t('Combined image and video model catalog')}
            </Label>
            <div className='flex flex-wrap gap-2'>
              <Button
                type='button'
                variant='secondary'
                size='sm'
                onClick={importModels}
              >
                {t('Import {{provider}} video models', {
                  provider: provider === 'doubao' ? 'Seedance' : provider,
                })}
              </Button>
              <VideoExamplesDialog provider={provider} />
            </div>
          </div>
          <Textarea
            id='video-policy-models'
            className='min-h-72 font-mono text-xs'
            value={models}
            onChange={(event) => setModels(event.target.value)}
          />
          <div className='flex flex-wrap items-center gap-2 text-sm'>
            <Badge variant='outline'>{t('Shared channel pool')}</Badge>
            <span className='text-muted-foreground'>
              {t(
                'Video routing maps 480p and 720p to 1K, 1080p to 2K, and 4K to 4K.'
              )}
            </span>
          </div>
          {provider === 'doubao' && (
            <div className='border-warning/40 bg-warning/10 rounded-lg border p-3 text-sm'>
              <p>
                {t(
                  'Seedance has no built-in USD price. Configure a billing expression before production use.'
                )}
              </p>
              <Button
                className='mt-2'
                type='button'
                size='sm'
                variant='outline'
                render={<a href='/system-settings/billing/model-pricing' />}
              >
                {t('Open model pricing')}
              </Button>
            </div>
          )}
          <Button type='submit' disabled={busy}>
            {t('Save video policy')}
          </Button>
        </form>
      </SettingsCard>
    </div>
  )
}
