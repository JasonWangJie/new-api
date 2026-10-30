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
import { isAxiosError } from 'axios'
import {
  Film,
  ImageIcon,
  Loader2,
  SlidersHorizontal,
  Sparkles,
} from 'lucide-react'
import { useEffect, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'

import { CopyButton } from '@/components/copy-button'
import { EmptyState } from '@/components/empty-state'
import { SectionPageLayout } from '@/components/layout/components/section-page-layout'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Progress } from '@/components/ui/progress'
import { Tabs, TabsList, TabsTrigger } from '@/components/ui/tabs'
import { Textarea } from '@/components/ui/textarea'
import { fetchTokenKey, getApiKeys } from '@/features/keys/api'
import { useAuthStore } from '@/stores/auth-store'

import {
  getImageCapabilities,
  getImageTask,
  imageBlob,
  imageRelayRequest,
  imageRequest,
} from './api'
import {
  ImageReferenceInput,
  type ReferencePart,
} from './components/image-reference-input'
import { ImageResults } from './components/image-results'
import { ImageSelect } from './components/image-select'
import { imageLabel, imagePlatformLabel } from './lib/image-labels'
import {
  buildImageRequest,
  extractImageResponse,
  imageDimensionOptions,
  imageRequestDimensions,
  terminalImageStatus,
  type ImageRequestPart,
} from './lib/image-request'
import { imageChecksum, saveLocalImage } from './lib/local-images'
import {
  readWorkbenchTask,
  rememberWorkbenchTask,
  workbenchSubmissionKey,
} from './lib/workbench-session'
import type { ImageMetadata } from './types'
import { VideoWorkbench } from './video-workbench'

type SubmissionSnapshot = {
  url: string
  body: string
  key: string
  tokenId: number
  version: string
}

export function ImageWorkbench() {
  const userId = useAuthStore((state) => state.auth.user?.id)
  const { t } = useTranslation()
  const [media, setMedia] = useState('image')
  return userId ? (
    <div className='flex min-h-0 flex-1 flex-col overflow-hidden'>
      <Tabs
        className='shrink-0 border-b px-3 py-3 sm:px-4'
        value={media}
        onValueChange={(value) => setMedia(String(value))}
      >
        <TabsList
          aria-label={t('Media type')}
          className='w-full rounded-xl p-1 group-data-horizontal/tabs:h-11 sm:w-fit sm:min-w-72'
        >
          <TabsTrigger value='image' className='gap-2 rounded-lg px-6'>
            <ImageIcon className='size-4' aria-hidden='true' />
            {t('Image')}
          </TabsTrigger>
          <TabsTrigger value='video' className='gap-2 rounded-lg px-6'>
            <Film className='size-4' aria-hidden='true' />
            {t('Video')}
          </TabsTrigger>
        </TabsList>
      </Tabs>
      {media === 'image' ? (
        <WorkbenchSession key={userId} userId={userId} />
      ) : (
        <VideoWorkbench key={userId} userId={userId} />
      )}
    </div>
  ) : null
}

function WorkbenchSession({ userId }: { userId: number }) {
  const { t } = useTranslation()
  const [tokenId, setTokenId] = useState('')
  const [modelId, setModelId] = useState('')
  const [prompt, setPrompt] = useState(() => {
    try {
      return sessionStorage.getItem(`new-api:image-prompt:${userId}`) || ''
    } catch {
      return ''
    }
  })
  const [parts, setParts] = useState<ReferencePart[]>([
    { id: 'reference-urls', type: 'image_urls', value: '' },
  ])
  const [resolution, setResolution] = useState('1K')
  const [ratio, setRatio] = useState('1:1')
  const [quality, setQuality] = useState('')
  const [format, setFormat] = useState('')
  const [background, setBackground] = useState('')
  const [count, setCount] = useState(1)
  const [loadingReferences, setLoadingReferences] = useState(false)
  const [busy, setBusy] = useState(false)
  const [taskId, setTaskId] = useState(() => readWorkbenchTask(userId, 'image'))
  const [results, setResults] = useState<{ id: string; url: string }[]>([])
  const [submissionError, setSubmissionError] = useState('')
  const [saveError, setSaveError] = useState('')
  const snapshot = useRef<SubmissionSnapshot | null>(null)
  const operations = useRef(new AbortController())
  const currentOwner = () =>
    useAuthStore.getState().auth.user?.id === userId &&
    !operations.current.signal.aborted
  useEffect(() => {
    const controller = new AbortController()
    operations.current = controller
    return () => {
      controller.abort()
      snapshot.current = null
    }
  }, [])
  const keys = useQuery({
    queryKey: ['image-keys', userId],
    queryFn: () => getApiKeys({ p: 1, size: 100 }),
  })
  const capabilities = useQuery({
    queryKey: ['image-capabilities', userId, tokenId],
    queryFn: ({ signal }) => getImageCapabilities(Number(tokenId), signal),
    enabled: !!tokenId,
    staleTime: 0,
  })
  const models =
    capabilities.data?.models.filter((model) => model.available) || []
  const model = models.find(
    (candidate) => `${candidate.platform}:${candidate.id}` === modelId
  )
  const task = useQuery({
    queryKey: ['workbench-image-task', userId, taskId],
    queryFn: ({ signal }) => getImageTask(false, taskId, signal),
    enabled: !!taskId,
    staleTime: 10_000,
    refetchInterval: (query) =>
      query.state.data &&
      terminalImageStatus(
        query.state.data.task.status,
        query.state.data.task.next_attempt_at
      )
        ? false
        : Math.min(30000, 3000 * 2 ** query.state.fetchFailureCount),
    retry: false,
  })
  useEffect(() => {
    if (!isAxiosError(task.error) || task.error.response?.status !== 404) return
    rememberWorkbenchTask(userId, 'image', '')
    setTaskId('')
  }, [task.error, userId])
  const taskImages = useQuery({
    queryKey: [
      'workbench-image-results',
      userId,
      taskId,
      (task.data?.results || [])
        .map(
          (result) =>
            `${result.image_index}:${result.url || result.view_url}:${!!result.authenticated}`
        )
        .join('|'),
    ],
    queryFn: async ({ signal }) =>
      Promise.all(
        (task.data?.results || []).map(async (result) => {
          const url =
            result.url ||
            (
              await imageRequest<{ url: string }>(
                result.view_url,
                'GET',
                undefined,
                signal
              )
            ).url
          return {
            id: String(result.image_index),
            url,
            blob: result.authenticated
              ? await imageBlob(url, signal, true)
              : undefined,
          }
        })
      ),
    enabled: !!task.data?.results.length,
    staleTime: 60_000,
  })
  const activeTask =
    !!taskId &&
    (!task.data ||
      !terminalImageStatus(
        task.data.task.status,
        task.data.task.next_attempt_at
      ))
  const chooseModel = (value: string) => {
    const selectedModel = models.find(
      (candidate) => `${candidate.platform}:${candidate.id}` === value
    )
    setModelId(value)
    setQuality('')
    setFormat('')
    setBackground('')
    setCount(1)
    const dimensions = imageDimensionOptions(selectedModel)
    setResolution(dimensions.resolutions?.[0] || 'AUTO')
    if (!dimensions.ratios?.includes(ratio)) {
      setRatio(dimensions.ratios?.[0] || 'auto')
    }
  }
  const loadReferences = async (files: File[]) => {
    if (
      files.some(
        (file) =>
          !['image/png', 'image/jpeg', 'image/webp'].includes(file.type) ||
          file.size > 32 * 1024 * 1024
      )
    ) {
      toast.error(t('Unsupported image format'))
      return
    }
    setLoadingReferences(true)
    try {
      const uploadedParts = await Promise.all(
        files.map(
          (file) =>
            new Promise<ReferencePart>((resolve, reject) => {
              const reader = new FileReader()
              reader.addEventListener('load', () =>
                resolve({
                  id: crypto.randomUUID(),
                  type: 'image',
                  value: String(reader.result),
                })
              )
              reader.addEventListener('error', () =>
                reject(new Error(t('Failed to load image')))
              )
              reader.readAsDataURL(file)
            })
        )
      )
      if (currentOwner()) {
        setParts((previous) => [...previous, ...uploadedParts])
      }
    } catch (error) {
      if (currentOwner()) {
        toast.error(
          error instanceof Error ? error.message : t('Failed to load image')
        )
      }
    } finally {
      if (currentOwner()) setLoadingReferences(false)
    }
  }
  const submit = async (retry = false) => {
    if (loadingReferences || (!model && !retry)) return
    setBusy(true)
    setSubmissionError('')
    setSaveError('')
    try {
      let accepted = snapshot.current
      const selectedId = retry && accepted ? accepted.tokenId : Number(tokenId)
      const fresh = await getImageCapabilities(
        selectedId,
        operations.current.signal
      )
      if (!currentOwner()) return
      if (!retry) {
        if (
          fresh.capability_version !== capabilities.data?.capability_version
        ) {
          await capabilities.refetch()
          throw new Error(
            t(
              'Image capabilities changed. Review your selection and submit again.'
            )
          )
        }
        const currentModel = fresh.models.find(
          (candidate) =>
            `${candidate.platform}:${candidate.id}` === modelId &&
            candidate.available
        )
        if (!currentModel) {
          throw new Error(t('No executable image model is available'))
        }
        const request = buildImageRequest({
          model: currentModel,
          prompt,
          parts: parts.flatMap((part): ImageRequestPart[] => {
            if (part.type === 'text') {
              return [{ type: 'text', text: part.value }]
            }
            if (part.type === 'image') {
              return [{ type: 'image_url', image_url: { url: part.value } }]
            }
            return part.value
              .split(/\r?\n/)
              .map((url) => url.trim())
              .filter(Boolean)
              .map((url) => ({ type: 'image_url', image_url: { url } }))
          }),
          resolution,
          ratio,
          quality,
          count,
          format,
          background,
        })
        accepted = {
          ...request,
          key:
            currentModel.mode === 'async'
              ? await workbenchSubmissionKey(
                  userId,
                  'image',
                  selectedId,
                  request.url,
                  request.body
                )
              : crypto.randomUUID(),
          tokenId: selectedId,
          version: fresh.capability_version,
        }
        snapshot.current = currentModel.mode === 'async' ? accepted : null
        setResults([])
        setTaskId('')
        rememberWorkbenchTask(userId, 'image', '')
      }
      if (!accepted) return
      const key = await fetchTokenKey(selectedId)
      if (!key.success || !key.data?.key) {
        throw new Error(key.message || t('Failed to load API key'))
      }
      const response = await imageRelayRequest(
        accepted.url,
        key.data.key,
        accepted.body,
        accepted.key,
        operations.current.signal
      )
      if (!currentOwner()) return
      let id = ''
      if (typeof response.task_id === 'string') {
        id = response.task_id
      } else if (
        typeof response.id === 'string' &&
        response.object === 'image.task'
      ) {
        id = response.id
      }
      if (id) {
        setTaskId(id)
        rememberWorkbenchTask(userId, 'image', id)
        snapshot.current = null
        return
      }
      const urls = extractImageResponse(response)
      if (!urls.length) throw new Error(t('No image was returned'))
      setResults(urls.map((url) => ({ id: crypto.randomUUID(), url })))
      snapshot.current = null
      for (const url of urls) {
        try {
          const blob = await imageBlob(url, operations.current.signal)
          const id = crypto.randomUUID()
          const metadata: ImageMetadata = {
            title: '',
            private_prompt: prompt,
            public_title: '',
            share_prompt: false,
            platform: model?.platform || 'openai',
            model: model?.id || '',
            generation_mode: 'realtime',
            source_type: 'realtime_import',
            requested_size: resolution,
            aspect_ratio: ratio,
            quality,
            content_type: blob.type,
            byte_size: blob.size,
            checksum_sha256: await imageChecksum(blob),
            client_blob_key: id,
          }
          await saveLocalImage(
            {
              id,
              userId,
              blob,
              metadata,
              createdAt: Date.now(),
              expiresAt: Date.now() + 30 * 86400000,
            },
            'library',
            currentOwner
          )
        } catch (error) {
          if (currentOwner()) {
            setSaveError(
              error instanceof Error
                ? error.message
                : t('Failed to save image locally')
            )
          }
        }
      }
    } catch (error) {
      if (currentOwner()) {
        setSubmissionError(
          error instanceof Error ? error.message : t('Image request failed')
        )
      }
    } finally {
      if (currentOwner()) setBusy(false)
    }
  }
  const options = (values: string[]) =>
    values.map((value) => ({ value, label: value }))
  const shownImages = taskId ? taskImages.data || [] : results
  const dimensions = model
    ? imageRequestDimensions(model, resolution, ratio)
    : undefined
  const dimensionOptions = imageDimensionOptions(model)
  const supportedParameters = model?.supported_parameters ?? []
  const isTabtab = model?.provider === 'tabtab'
  return (
    <SectionPageLayout>
      <SectionPageLayout.Title>{t('Image Workbench')}</SectionPageLayout.Title>
      <SectionPageLayout.Actions>
        <Button
          variant='outline'
          size='sm'
          render={<a href='/guide/async-image-api' />}
        >
          {t('Async Image API Guide')}
        </Button>
      </SectionPageLayout.Actions>
      <SectionPageLayout.Content>
        <div className='grid min-h-full items-start gap-5 xl:grid-cols-[minmax(360px,440px)_minmax(0,1fr)]'>
          <form
            aria-label={t('Image generation settings')}
            className='bg-card min-w-0 overflow-hidden rounded-2xl border shadow-sm [&_[data-slot=select-trigger]]:min-h-11 [&_input:not([type=file])]:min-h-11'
            onSubmit={(event) => {
              event.preventDefault()
              void submit()
            }}
          >
            <div className='from-primary/5 flex items-center gap-4 border-b bg-linear-to-br to-transparent p-5 sm:p-6'>
              <div className='bg-primary/10 text-primary flex size-11 shrink-0 items-center justify-center rounded-xl'>
                <ImageIcon className='size-5' aria-hidden='true' />
              </div>
              <div className='min-w-0'>
                <p className='text-muted-foreground mb-1 text-[10px] font-semibold tracking-[0.2em]'>
                  {t('CREATE')}
                </p>
                <h3 className='font-serif text-xl leading-snug'>
                  {t('A place for your next image')}
                </h3>
              </div>
            </div>
            <div className='space-y-4 border-b p-5 sm:p-6'>
              <ImageSelect
                label={t('API Key')}
                value={tokenId}
                options={(keys.data?.data?.items || [])
                  .filter((key) => key.status === 1)
                  .map((key) => ({ value: String(key.id), label: key.name }))}
                onChange={(value) => {
                  setTokenId(value)
                  chooseModel('')
                }}
                disabled={busy || activeTask}
              />
              <ImageSelect
                label={t('Model')}
                value={modelId}
                options={models.map((candidate) => ({
                  value: `${candidate.platform}:${candidate.id}`,
                  label: `${candidate.label || candidate.id} · ${t(imagePlatformLabel(candidate.platform))} · ${t(candidate.mode === 'async' ? 'Asynchronous' : 'Realtime')}`,
                }))}
                onChange={chooseModel}
                disabled={busy || activeTask || !tokenId}
              />
              {model?.platform === 'mixed' && (
                <p role='status' className='text-muted-foreground text-sm'>
                  {t(
                    'Mixed scheduling accepts asynchronous image requests without provider. Explicit provider requests keep their platform routing.'
                  )}
                </p>
              )}
              {capabilities.isError && (
                <p role='alert' className='text-destructive text-sm'>
                  {capabilities.error.message}
                </p>
              )}
              {capabilities.data && !models.length && (
                <p role='status' className='text-muted-foreground text-sm'>
                  {t('No executable image model is available')}
                </p>
              )}
            </div>
            <div className='space-y-3 border-b p-5 sm:p-6'>
              <Label htmlFor='image-prompt'>{t('Prompt')}</Label>
              <Textarea
                id='image-prompt'
                value={prompt}
                onChange={(event) => setPrompt(event.target.value)}
                placeholder={t(
                  'Describe your image, its light, materials and composition'
                )}
                className='bg-background/60 min-h-36 resize-y text-sm leading-relaxed'
                required
                disabled={busy || activeTask}
              />
            </div>
            <ImageReferenceInput
              parts={parts}
              onChange={setParts}
              onUpload={loadReferences}
              disabled={busy || activeTask || loadingReferences}
            />
            <div className='space-y-4 border-b p-5 sm:p-6'>
              <h4 className='flex items-center gap-2 text-sm font-medium'>
                <SlidersHorizontal
                  className='text-muted-foreground size-4'
                  aria-hidden='true'
                />
                {t('Generation settings')}
              </h4>
              <div className='grid grid-cols-2 gap-3'>
                {dimensionOptions.resolutions && (
                  <ImageSelect
                    label={t('Resolution')}
                    value={resolution}
                    options={options(dimensionOptions.resolutions)}
                    onChange={setResolution}
                    disabled={
                      busy || activeTask || !dimensionOptions.resolutions.length
                    }
                  />
                )}
                {dimensionOptions.ratios && (
                  <ImageSelect
                    label={t('Aspect Ratio')}
                    value={ratio}
                    options={options(dimensionOptions.ratios)}
                    onChange={setRatio}
                    disabled={busy || activeTask}
                  />
                )}
              </div>
              {dimensions && (
                <p role='status' className='text-muted-foreground text-sm'>
                  {t('Submitted size: {{size}}', {
                    size:
                      dimensions.size === 'auto' ? t('Auto') : dimensions.size,
                  })}
                  {dimensions.approximate && (
                    <>
                      {' '}
                      {t(
                        'The selected aspect ratio is approximated by this size.'
                      )}
                    </>
                  )}
                </p>
              )}
              {model?.capability.qualities?.length &&
              (!isTabtab || supportedParameters.includes('quality')) ? (
                <ImageSelect
                  label={t('Quality')}
                  value={quality}
                  options={[
                    { value: '', label: t('Default') },
                    ...options(model.capability.qualities),
                  ]}
                  onChange={setQuality}
                  disabled={busy || activeTask}
                />
              ) : null}
              {model?.capability.formats?.length &&
              (!isTabtab || supportedParameters.includes('output_format')) ? (
                <ImageSelect
                  label={t('Output format')}
                  value={format}
                  options={[
                    { value: '', label: t('Default') },
                    ...options(model.capability.formats),
                  ]}
                  onChange={setFormat}
                  disabled={busy || activeTask}
                />
              ) : null}
              {model?.capability.backgrounds?.length &&
              (!isTabtab || supportedParameters.includes('background')) ? (
                <ImageSelect
                  label={t('Background')}
                  value={background}
                  options={[
                    { value: '', label: t('Default') },
                    ...options(model.capability.backgrounds),
                  ]}
                  onChange={setBackground}
                  disabled={busy || activeTask}
                />
              ) : null}
              {model &&
                model.capability.max_output_images > 1 &&
                model.protocol !== 'gemini_native' &&
                (model.provider || model.platform !== 'gemini') && (
                  <div className='space-y-2'>
                    <Label htmlFor='image-count'>{t('Image count')}</Label>
                    <Input
                      id='image-count'
                      type='number'
                      min={1}
                      max={Math.min(128, model.capability.max_output_images)}
                      value={count}
                      onChange={(event) => setCount(event.target.valueAsNumber)}
                      disabled={busy || activeTask}
                    />
                  </div>
                )}
            </div>
            <div className='bg-muted/15 space-y-3 p-5 sm:p-6'>
              <Button
                type='submit'
                className='h-11 w-full gap-2 rounded-xl shadow-sm'
                disabled={
                  !model ||
                  busy ||
                  activeTask ||
                  loadingReferences ||
                  dimensionOptions.resolutions?.length === 0
                }
              >
                {busy || activeTask || loadingReferences ? (
                  <Loader2
                    className='size-4 animate-spin motion-reduce:animate-none'
                    aria-hidden='true'
                  />
                ) : (
                  <Sparkles className='size-4' aria-hidden='true' />
                )}
                {busy && t('Submitting')}
                {!busy && activeTask && t('Processing')}
                {!busy && !activeTask && loadingReferences && t('Loading...')}
                {!busy &&
                  !activeTask &&
                  !loadingReferences &&
                  t('Generate image')}
              </Button>
              {submissionError && (
                <div role='alert' className='text-destructive text-sm'>
                  {submissionError}
                  {snapshot.current && (
                    <Button
                      type='button'
                      variant='outline'
                      className='mt-2 w-full'
                      disabled={busy}
                      onClick={() => void submit(true)}
                    >
                      {t('Retry the same submission')}
                    </Button>
                  )}
                </div>
              )}
              <p className='text-muted-foreground text-xs leading-relaxed'>
                {t(
                  'Realtime images are saved on this device. Async images remain in your task center.'
                )}
              </p>
            </div>
          </form>
          <section
            aria-label={t('Results')}
            className='bg-card min-w-0 overflow-hidden rounded-2xl border shadow-sm xl:sticky xl:top-0'
          >
            <div className='flex flex-wrap items-center justify-between gap-3 border-b px-5 py-4 sm:px-6'>
              <div className='flex items-center gap-3'>
                <div className='bg-muted text-muted-foreground flex size-9 items-center justify-center rounded-lg'>
                  <ImageIcon className='size-4' aria-hidden='true' />
                </div>
                <div>
                  <h3 className='text-sm font-semibold'>
                    {t('Image preview')}
                  </h3>
                  <p className='text-muted-foreground mt-0.5 text-xs'>
                    {t('Results')}
                  </p>
                </div>
              </div>
              {(taskId || results.length > 0) && (
                <Badge variant='outline'>
                  {t(taskId ? 'Asynchronous' : 'Realtime')}
                </Badge>
              )}
            </div>
            <div className='space-y-4 p-4 sm:p-5'>
              {taskId && (
                <div
                  aria-live='polite'
                  className='bg-muted/30 min-w-0 space-y-3 rounded-xl border p-4'
                >
                  <div className='flex flex-wrap items-center justify-between gap-2'>
                    <Badge variant='outline'>
                      {t(imageLabel(task.data?.task.status || 'queued'))}
                    </Badge>
                    <CopyButton
                      value={taskId}
                      size='sm'
                      className='max-w-full min-w-0'
                    >
                      <span className='truncate'>{taskId}</span>
                    </CopyButton>
                  </div>
                  <Progress
                    value={task.data?.task.progress || 0}
                    aria-label={t('Processing')}
                  />
                  <p className='text-muted-foreground text-sm'>
                    {task.data?.task.progress || 0}% · {t('Task center')}:{' '}
                    <a href='/async-image-tasks' className='underline'>
                      {t('View tasks')}
                    </a>
                  </p>
                  {task.data?.task.error_message && (
                    <p className='text-destructive text-sm'>
                      {task.data.task.error_message}
                    </p>
                  )}
                  {task.isError && (
                    <p role='alert' className='text-muted-foreground text-sm'>
                      {t(
                        'Task query is temporarily unavailable. Polling will resume.'
                      )}
                    </p>
                  )}
                </div>
              )}
              {saveError && (
                <p role='alert' className='text-destructive text-sm'>
                  {t(
                    'Generation succeeded, but local saving failed. Download the image before leaving.'
                  )}{' '}
                  {saveError}
                </p>
              )}
              {shownImages.length > 0 ? (
                <ImageResults images={shownImages} />
              ) : (
                <div className='bg-muted/20 flex min-h-80 items-center justify-center rounded-xl border border-dashed [background-image:radial-gradient(var(--border)_1px,transparent_1px)] [background-size:20px_20px] sm:min-h-[420px]'>
                  <EmptyState
                    icon={ImageIcon}
                    title={t('Your images will appear here')}
                    description={t(
                      'Describe your image, its light, materials and composition'
                    )}
                    className='bg-card/85 m-4 min-h-0 max-w-sm rounded-2xl border p-6 shadow-sm sm:p-8'
                  />
                </div>
              )}
            </div>
          </section>
        </div>
      </SectionPageLayout.Content>
    </SectionPageLayout>
  )
}
