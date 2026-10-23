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
import { Loader2, Settings2, Sparkles } from 'lucide-react'
import { useState } from 'react'
import { type FieldErrors, type Resolver, useForm } from 'react-hook-form'
import { useTranslation } from 'react-i18next'
import { z } from 'zod'

import { Button } from '@/components/ui/button'
import { Checkbox } from '@/components/ui/checkbox'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Textarea } from '@/components/ui/textarea'

import { imageLabel } from '../lib/image-labels'
import type {
  VideoInputCapability,
  VideoModeCapability,
  VideoModeName,
  VideoModel,
} from '../types'
import { ImageSelect } from './image-select'

export type VideoFormSubmission = {
  mode: VideoModeName
  body: Record<string, unknown>
  files: { field: string; file: File }[]
}

type VideoFormValues = {
  mode: VideoModeName
  prompt: string
  seconds: string
  resolution: string
  aspectRatio: string
  extensionDirection: string
  inputs: Record<string, string>
  options: Record<string, boolean>
  additional: string
}

const PROTECTED_FIELDS = new Set([
  'model',
  'provider',
  'action',
  'operation',
  'mode',
  'prompt',
  'seconds',
  'duration',
  'size',
  'resolution',
  'aspect_ratio',
  'ratio',
  'extension_direction',
  'generate_audio',
  'watermark',
  'return_last_frame',
  'video',
  'image',
  'images',
  'input_reference',
  'last_frame',
  'reference_images',
  'reference_videos',
  'reference_audios',
  'source_task_id',
])

function initialMode(model: VideoModel): VideoModeCapability {
  return model.capability.modes[0]
}

function modeDefaults(mode: VideoModeCapability): VideoFormValues {
  return {
    mode: mode.name,
    prompt: '',
    seconds: mode.duration ? String(mode.duration.default) : '',
    resolution: mode.defaultResolution || '',
    aspectRatio: mode.defaultAspectRatio || '',
    extensionDirection: mode.defaultExtensionDirection || '',
    inputs: {},
    options: {},
    additional: '{}',
  }
}

function parseReferenceValue(value: string): unknown {
  const trimmed = value.trim()
  if (trimmed.startsWith('{')) return JSON.parse(trimmed) as unknown
  return trimmed
}

function inputLabel(name: VideoInputCapability['name']): string {
  switch (name) {
    case 'video':
      return 'Source video'
    case 'image':
      return 'First frame or reference image'
    case 'last_frame':
      return 'Last frame'
    case 'reference_images':
      return 'Reference images'
    case 'reference_videos':
      return 'Reference videos'
    case 'reference_audios':
      return 'Reference audios or voice IDs'
  }
}

function inputAccept(kind: VideoInputCapability['kind']): string {
  if (kind === 'image') return 'image/png,image/jpeg,image/webp'
  if (kind === 'video') return 'video/*'
  return 'audio/*'
}

function formSchema(
  model: VideoModel,
  invalid: string,
  protectedField: (field: string) => string
) {
  return z
    .object({
      mode: z.enum([
        'text_to_video',
        'image_to_video',
        'reference_to_video',
        'first_last_frame',
        'edit_video',
        'extend_video',
      ]),
      prompt: z.string(),
      seconds: z.string(),
      resolution: z.string(),
      aspectRatio: z.string(),
      extensionDirection: z.string(),
      inputs: z.record(z.string(), z.string()),
      options: z.record(z.string(), z.boolean()),
      additional: z.string(),
    })
    .superRefine((values, context) => {
      const mode = model.capability.modes.find(
        (candidate) => candidate.name === values.mode
      )
      if (!mode) {
        context.addIssue({ code: 'custom', path: ['mode'], message: invalid })
        return
      }
      if (
        ['text_to_video', 'edit_video', 'extend_video'].includes(mode.name) &&
        !values.prompt.trim()
      ) {
        context.addIssue({ code: 'custom', path: ['prompt'], message: invalid })
      }
      if (mode.duration) {
        const seconds = Number(values.seconds)
        const valuesList = mode.duration.values
        const minimum = mode.duration.min ?? 1
        const maximum = mode.duration.max ?? model.max_duration_seconds
        const step = mode.duration.step ?? 1
        if (
          !Number.isInteger(seconds) ||
          (valuesList
            ? !valuesList.includes(seconds)
            : seconds < minimum ||
              seconds > maximum ||
              (seconds - minimum) % step !== 0)
        ) {
          context.addIssue({
            code: 'custom',
            path: ['seconds'],
            message: invalid,
          })
        }
      }
      if (
        mode.resolutions?.length &&
        !mode.resolutions.includes(values.resolution)
      ) {
        context.addIssue({
          code: 'custom',
          path: ['resolution'],
          message: invalid,
        })
      }
      if (
        mode.aspectRatios?.length &&
        !mode.aspectRatios.includes(values.aspectRatio)
      ) {
        context.addIssue({
          code: 'custom',
          path: ['aspectRatio'],
          message: invalid,
        })
      }
      if (
        mode.name === 'extend_video' &&
        !mode.extensionDirections?.includes(
          values.extensionDirection as 'forward' | 'backward'
        )
      ) {
        context.addIssue({
          code: 'custom',
          path: ['extensionDirection'],
          message: invalid,
        })
      }
      try {
        const extra: unknown = JSON.parse(values.additional)
        if (!extra || typeof extra !== 'object' || Array.isArray(extra)) {
          throw new Error(invalid)
        }
        for (const field of Object.keys(extra)) {
          if (PROTECTED_FIELDS.has(field)) {
            context.addIssue({
              code: 'custom',
              path: ['additional'],
              message: protectedField(field),
            })
            return
          }
        }
        const metadata = (extra as { metadata?: unknown }).metadata
        if (
          metadata &&
          typeof metadata === 'object' &&
          !Array.isArray(metadata)
        ) {
          for (const field of Object.keys(metadata)) {
            if (PROTECTED_FIELDS.has(field)) {
              context.addIssue({
                code: 'custom',
                path: ['additional'],
                message: protectedField(`metadata.${field}`),
              })
              return
            }
          }
        }
      } catch {
        context.addIssue({
          code: 'custom',
          path: ['additional'],
          message: invalid,
        })
      }
    })
}

function firstError(errors: FieldErrors<VideoFormValues>, fallback: string) {
  for (const error of Object.values(errors)) {
    if (error && typeof error === 'object' && 'message' in error) {
      const message = error.message
      if (typeof message === 'string') return message
    }
  }
  return fallback
}

export function VideoCapabilityForm(props: {
  model: VideoModel
  busy: boolean
  error?: string
  onChanged: () => void
  onSubmit: (submission: VideoFormSubmission) => void
}) {
  const { t } = useTranslation()
  const firstMode = initialMode(props.model)
  const schema = formSchema(props.model, t('Invalid video request'), (field) =>
    t('Additional parameters cannot override {{field}}.', { field })
  )
  const form = useForm<VideoFormValues>({
    resolver: zodResolver(schema) as Resolver<VideoFormValues>,
    defaultValues: modeDefaults(firstMode),
  })
  const [uploads, setUploads] = useState<Record<string, File[]>>({})
  const [localError, setLocalError] = useState('')
  const modeName = form.watch('mode')
  const mode =
    props.model.capability.modes.find((item) => item.name === modeName) ||
    firstMode
  const inputValues = form.watch('inputs')
  const options = form.watch('options')

  const changeMode = (name: string) => {
    const next = props.model.capability.modes.find((item) => item.name === name)
    if (!next) return
    form.reset(modeDefaults(next))
    setUploads({})
    setLocalError('')
    props.onChanged()
  }

  const submit = (values: VideoFormValues) => {
    try {
      const extra = JSON.parse(values.additional) as Record<string, unknown>
      const nativeContent = (extra as { metadata?: { content?: unknown } })
        .metadata?.content
      const body: Record<string, unknown> = {
        ...extra,
        model: props.model.id,
        provider: props.model.provider,
      }
      if (values.prompt.trim()) body.prompt = values.prompt.trim()
      if (mode.duration) body.seconds = Number(values.seconds)
      if (mode.resolutions?.length) body.resolution = values.resolution
      if (mode.aspectRatios?.length) body.aspect_ratio = values.aspectRatio
      if (mode.name === 'extend_video') {
        body.extension_direction = values.extensionDirection
      }
      const files: VideoFormSubmission['files'] = []
      for (const input of mode.inputs || []) {
        const lines = (values.inputs[input.name] || '')
          .split('\n')
          .map((line) => line.trim())
          .filter(Boolean)
        const inputFiles = uploads[input.name] || []
        if (
          input.required &&
          lines.length + inputFiles.length === 0 &&
          (input.name === 'video' || !Array.isArray(nativeContent))
        ) {
          throw new Error(
            t('{{input}} is required for this video mode.', {
              input: t(inputLabel(input.name)),
            })
          )
        }
        if (
          props.model.provider === 'doubao' &&
          lines.some(
            (line) => !/^https?:\/\//i.test(line) && !/^asset:\/\//i.test(line)
          )
        ) {
          throw new Error(
            t(
              'This provider does not accept browser file uploads here. Use a public URL or asset:// ID.'
            )
          )
        }
        if (lines.length + inputFiles.length > input.maxItems) {
          throw new Error(
            t('{{input}} accepts at most {{count}} items.', {
              input: t(inputLabel(input.name)),
              count: input.maxItems,
            })
          )
        }
        const parsedValues = lines.map(parseReferenceValue)
        if (parsedValues.length > 0) {
          body[input.name] =
            input.maxItems === 1 ? parsedValues[0] : parsedValues
        }
        for (const file of inputFiles) files.push({ field: input.name, file })
      }
      if (mode.options?.generateAudio) {
        body.generate_audio = values.options.generate_audio === true
      }
      if (mode.options?.watermark) {
        body.watermark = values.options.watermark === true
      }
      if (mode.options?.returnLastFrame) {
        body.return_last_frame = values.options.return_last_frame === true
      }
      setLocalError('')
      props.onSubmit({ mode: mode.name, body, files })
    } catch (error) {
      setLocalError(
        error instanceof Error ? error.message : t('Invalid video request')
      )
    }
  }

  let submitLabel = 'Generate video'
  if (mode.name === 'edit_video') {
    submitLabel = 'Edit video'
  } else if (mode.name === 'extend_video') {
    submitLabel = 'Extend video'
  }

  return (
    <form
      className='space-y-5 border-t p-5 sm:p-6 [&_[data-slot=select-trigger]]:min-h-11 [&_input:not([type=file])]:min-h-11'
      onSubmit={form.handleSubmit(submit, (errors) =>
        setLocalError(firstError(errors, t('Invalid video request')))
      )}
      onChange={props.onChanged}
    >
      <ImageSelect
        label={t('Generation mode')}
        value={mode.name}
        options={props.model.capability.modes.map((item) => ({
          value: item.name,
          label: t(imageLabel(item.name)),
        }))}
        onChange={changeMode}
        disabled={props.busy}
      />
      <div className='space-y-3'>
        <Label htmlFor='video-prompt' className='flex items-center gap-2'>
          <Sparkles
            className='text-muted-foreground size-3.5'
            aria-hidden='true'
          />
          {t('Prompt')}
        </Label>
        <Textarea
          id='video-prompt'
          className='min-h-36 resize-y rounded-xl text-sm leading-relaxed'
          placeholder={t(
            'Describe the scene, motion, and visual style you have in mind.'
          )}
          {...form.register('prompt')}
          required={['text_to_video', 'edit_video', 'extend_video'].includes(
            mode.name
          )}
          disabled={props.busy}
        />
      </div>
      <section
        aria-label={t('Generation settings')}
        className='space-y-4 border-t pt-5'
      >
        <h4 className='flex items-center gap-2 text-sm font-medium'>
          <Settings2
            className='text-muted-foreground size-3.5'
            aria-hidden='true'
          />
          {t('Generation settings')}
        </h4>
        <div className='grid min-w-0 gap-3 sm:grid-cols-2 xl:grid-cols-2'>
          {mode.duration?.values && (
            <ImageSelect
              label={t('Duration (seconds)')}
              value={form.watch('seconds')}
              options={mode.duration.values.map((value) => ({
                value: String(value),
                label: String(value),
              }))}
              onChange={(value) => {
                form.setValue('seconds', value, { shouldDirty: true })
                props.onChanged()
              }}
              disabled={props.busy}
            />
          )}
          {mode.duration && !mode.duration.values && (
            <div className='space-y-2'>
              <Label htmlFor='video-seconds'>{t('Duration (seconds)')}</Label>
              <Input
                id='video-seconds'
                type='number'
                min={mode.duration.min}
                max={mode.duration.max}
                step={mode.duration.step}
                {...form.register('seconds')}
                disabled={props.busy}
              />
            </div>
          )}
          {!!mode.resolutions?.length && (
            <ImageSelect
              label={t('Resolution')}
              value={form.watch('resolution')}
              options={mode.resolutions.map((value) => ({
                value,
                label: value,
              }))}
              onChange={(value) => {
                form.setValue('resolution', value, { shouldDirty: true })
                props.onChanged()
              }}
              disabled={props.busy}
            />
          )}
          {!!mode.aspectRatios?.length && (
            <ImageSelect
              label={t('Aspect ratio')}
              value={form.watch('aspectRatio')}
              options={mode.aspectRatios.map((value) => ({
                value,
                label: value,
              }))}
              onChange={(value) => {
                form.setValue('aspectRatio', value, { shouldDirty: true })
                props.onChanged()
              }}
              disabled={props.busy}
            />
          )}
          {mode.name === 'extend_video' &&
            !!mode.extensionDirections?.length && (
              <ImageSelect
                label={t('Extension direction')}
                value={form.watch('extensionDirection')}
                options={mode.extensionDirections.map((value) => ({
                  value,
                  label: t(value === 'forward' ? 'Forward' : 'Backward'),
                }))}
                onChange={(value) => {
                  form.setValue('extensionDirection', value, {
                    shouldDirty: true,
                  })
                  props.onChanged()
                }}
                disabled={props.busy}
              />
            )}
        </div>
        {(!mode.duration ||
          !mode.resolutions?.length ||
          !mode.aspectRatios?.length) && (
          <p className='text-muted-foreground text-xs leading-relaxed'>
            {t(
              'Fields hidden for this mode are inherited from the source video or fixed by the provider.'
            )}
          </p>
        )}
      </section>
      {(mode.inputs || []).map((input) => {
        const supportsUpload = input.sources.includes('upload')
        const visibleSources =
          props.model.provider === 'doubao'
            ? input.sources.filter(
                (source) => source === 'url' || source === 'asset'
              )
            : input.sources
        const multiple = input.maxItems > 1
        return (
          <div
            key={input.name}
            className='bg-muted/20 min-w-0 space-y-3 rounded-xl border p-4'
          >
            <Label htmlFor={`video-${input.name}`}>
              {t(inputLabel(input.name))}
              {input.required ? ' *' : ''}
            </Label>
            {multiple ? (
              <Textarea
                id={`video-${input.name}`}
                className='bg-background min-h-28 resize-y font-mono text-xs leading-relaxed'
                aria-describedby={`video-${input.name}-help`}
                value={inputValues[input.name] || ''}
                placeholder={t(
                  'Enter one URL, asset ID, file ID object, or voice ID per line'
                )}
                onChange={(event) =>
                  form.setValue(
                    'inputs',
                    {
                      ...form.getValues('inputs'),
                      [input.name]: event.target.value,
                    },
                    { shouldDirty: true }
                  )
                }
                disabled={props.busy}
              />
            ) : (
              <Input
                id={`video-${input.name}`}
                className='bg-background'
                aria-describedby={`video-${input.name}-help`}
                value={inputValues[input.name] || ''}
                placeholder={t('URL, asset ID, or file ID object')}
                onChange={(event) =>
                  form.setValue(
                    'inputs',
                    {
                      ...form.getValues('inputs'),
                      [input.name]: event.target.value,
                    },
                    { shouldDirty: true }
                  )
                }
                disabled={props.busy}
              />
            )}
            <p
              id={`video-${input.name}-help`}
              className='text-muted-foreground text-xs leading-relaxed wrap-anywhere'
            >
              {t('Allowed sources: {{sources}}. Maximum: {{count}}.', {
                sources: visibleSources.join(', '),
                count: input.maxItems,
              })}
            </p>
            {supportsUpload ? (
              <Input
                aria-label={t('Upload {{input}}', {
                  input: t(inputLabel(input.name)),
                })}
                type='file'
                className='bg-background h-auto min-h-11 min-w-0 cursor-pointer py-2 file:cursor-pointer'
                accept={inputAccept(input.kind)}
                multiple={multiple}
                disabled={props.busy}
                onChange={(event) => {
                  setUploads((previous) => ({
                    ...previous,
                    [input.name]: [...(event.target.files || [])],
                  }))
                  props.onChanged()
                }}
              />
            ) : (
              <p className='text-muted-foreground text-xs leading-relaxed'>
                {t(
                  props.model.provider === 'doubao'
                    ? 'This provider does not accept browser file uploads here. Use a public URL or asset:// ID.'
                    : 'Browser uploads are unavailable for this input. Use one of the allowed direct sources.'
                )}
              </p>
            )}
          </div>
        )
      })}
      {(mode.options?.generateAudio ||
        mode.options?.watermark ||
        mode.options?.returnLastFrame) && (
        <div className='grid gap-2'>
          {[
            [
              'generate_audio',
              'Generate synchronized audio',
              mode.options.generateAudio,
            ],
            ['watermark', 'Add watermark', mode.options.watermark],
            [
              'return_last_frame',
              'Return last frame',
              mode.options.returnLastFrame,
            ],
          ].map(([name, label, visible]) =>
            visible ? (
              <div
                key={String(name)}
                className='bg-muted/30 flex min-h-11 items-center gap-3 rounded-lg border px-3 py-2.5'
              >
                <Checkbox
                  id={`video-${name}`}
                  checked={options[String(name)] === true}
                  onCheckedChange={(checked) => {
                    form.setValue(
                      'options',
                      {
                        ...form.getValues('options'),
                        [String(name)]: checked === true,
                      },
                      { shouldDirty: true }
                    )
                    props.onChanged()
                  }}
                  disabled={props.busy}
                />
                <Label
                  htmlFor={`video-${name}`}
                  className='flex-1 cursor-pointer text-xs leading-relaxed'
                >
                  {t(String(label))}
                </Label>
              </div>
            ) : null
          )}
        </div>
      )}
      <div className='space-y-3 border-t pt-5'>
        <Label htmlFor='video-additional'>{t('Additional parameters')}</Label>
        <Textarea
          id='video-additional'
          className='bg-muted/20 min-h-28 resize-y rounded-xl font-mono text-xs leading-relaxed'
          aria-describedby='video-additional-help'
          {...form.register('additional')}
          disabled={props.busy}
        />
        <p
          id='video-additional-help'
          className='text-muted-foreground text-xs leading-relaxed'
        >
          {t(
            'Additional JSON may include provider-native content, but cannot override the selected operation or structured video fields.'
          )}
        </p>
      </div>
      <Button
        className='min-h-11 w-full rounded-xl shadow-sm'
        type='submit'
        disabled={!props.model.available || props.busy}
      >
        {props.busy ? (
          <Loader2 className='size-4 animate-spin' aria-hidden='true' />
        ) : (
          <Sparkles className='size-4' aria-hidden='true' />
        )}
        {t(props.busy ? 'Submitting...' : submitLabel)}
      </Button>
      {(localError || props.error) && (
        <p
          role='alert'
          className='text-destructive bg-destructive/5 border-destructive/20 rounded-lg border p-3 text-xs leading-relaxed wrap-anywhere'
        >
          {localError || props.error}
        </p>
      )}
    </form>
  )
}
