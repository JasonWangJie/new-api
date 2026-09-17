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
import { ArrowDown, ArrowUp, Plus, Sparkles, Trash2 } from 'lucide-react'
import { useEffect, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'

import { CopyButton } from '@/components/copy-button'
import { SectionPageLayout } from '@/components/layout'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
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
import { ImageResults } from './components/image-results'
import { ImageSelect } from './components/image-select'
import { imageLabel } from './lib/image-labels'
import {
  buildImageRequest,
  extractImageResponse,
  IMAGE_RATIOS,
  terminalImageStatus,
} from './lib/image-request'
import { imageChecksum, saveLocalImage } from './lib/local-images'
import type { ImageMetadata } from './types'

type ReferencePart = { id: string; type: 'text' | 'image'; value: string }
type SubmissionSnapshot = {
  url: string
  body: string
  key: string
  tokenId: number
  version: string
}

export function ImageWorkbench() {
  const userId = useAuthStore((state) => state.auth.user?.id)
  return userId ? <WorkbenchSession key={userId} userId={userId} /> : null
}

function WorkbenchSession({ userId }: { userId: number }) {
  const { t } = useTranslation()
  const [tokenId, setTokenId] = useState('')
  const [modelId, setModelId] = useState('')
  const [prompt, setPrompt] = useState(
    () => sessionStorage.getItem(`new-api:image-prompt:${userId}`) || ''
  )
  const [parts, setParts] = useState<ReferencePart[]>([])
  const [resolution, setResolution] = useState('1K')
  const [ratio, setRatio] = useState('1:1')
  const [quality, setQuality] = useState('')
  const [format, setFormat] = useState('')
  const [background, setBackground] = useState('')
  const [count, setCount] = useState(1)
  const [busy, setBusy] = useState(false)
  const [taskId, setTaskId] = useState('')
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
  const taskImages = useQuery({
    queryKey: ['workbench-image-results', userId, taskId, task.data?.results],
    queryFn: ({ signal }) =>
      Promise.all(
        (task.data?.results || []).map(async (result) => ({
          id: String(result.image_index),
          url: (
            await imageRequest<{ url: string }>(
              result.view_url,
              'GET',
              undefined,
              signal
            )
          ).url,
        }))
      ),
    enabled: !!task.data?.results.length,
    staleTime: 30000,
  })
  const activeTask =
    !!taskId &&
    (!task.data ||
      !terminalImageStatus(
        task.data.task.status,
        task.data.task.next_attempt_at
      ))
  const chooseModel = (value: string) => {
    setModelId(value)
    setQuality('')
    setFormat('')
    setBackground('')
    setCount(1)
    setResolution('1K')
  }
  const movePart = (index: number, offset: number) => {
    setParts((previous) => {
      const next = [...previous]
      ;[next[index], next[index + offset]] = [next[index + offset], next[index]]
      return next
    })
  }
  const loadReference = async (file: File) => {
    if (
      !['image/png', 'image/jpeg', 'image/webp'].includes(file.type) ||
      file.size > 32 * 1024 * 1024
    ) {
      toast.error(t('Unsupported image format'))
      return
    }
    const reader = new FileReader()
    reader.addEventListener('load', () => {
      if (currentOwner()) {
        setParts((previous) => [
          ...previous,
          {
            id: crypto.randomUUID(),
            type: 'image',
            value: String(reader.result),
          },
        ])
      }
    })
    reader.readAsDataURL(file)
  }
  const submit = async (retry = false) => {
    if (!model && !retry) return
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
          parts: parts.map((part) =>
            part.type === 'text'
              ? { type: 'text', text: part.value }
              : { type: 'image_url', image_url: { url: part.value } }
          ),
          resolution,
          ratio,
          quality,
          count: currentModel.platform === 'gemini' ? 1 : count,
          format,
          background,
        })
        accepted = {
          ...request,
          key: crypto.randomUUID(),
          tokenId: selectedId,
          version: fresh.capability_version,
        }
        snapshot.current = currentModel.mode === 'async' ? accepted : null
        setResults([])
        setTaskId('')
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
  return (
    <SectionPageLayout>
      <SectionPageLayout.Title>{t('Image Workbench')}</SectionPageLayout.Title>
      <SectionPageLayout.Actions>
        <a
          className='text-muted-foreground text-sm underline underline-offset-4'
          href='/guide/async-image-api'
        >
          {t('Async Image API Guide')}
        </a>
      </SectionPageLayout.Actions>
      <SectionPageLayout.Content>
        <div className='grid min-h-full gap-6 lg:grid-cols-[minmax(280px,360px)_1fr]'>
          <form
            className='bg-card space-y-5 rounded-xl border p-5'
            onSubmit={(event) => {
              event.preventDefault()
              void submit()
            }}
          >
            <div>
              <p className='text-muted-foreground mb-1 text-xs font-semibold tracking-widest'>
                {t('CREATE')}
              </p>
              <h3 className='font-serif text-2xl'>
                {t('A place for your next image')}
              </h3>
            </div>
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
                label: `${candidate.label || candidate.id} · ${candidate.platform} · ${t(candidate.mode === 'async' ? 'Asynchronous' : 'Realtime')}`,
              }))}
              onChange={chooseModel}
              disabled={busy || activeTask || !tokenId}
            />
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
            <div className='space-y-2'>
              <Label htmlFor='image-prompt'>{t('Prompt')}</Label>
              <Textarea
                id='image-prompt'
                value={prompt}
                onChange={(event) => setPrompt(event.target.value)}
                placeholder={t(
                  'Describe your image, its light, materials and composition'
                )}
                className='min-h-36'
                required
                disabled={busy || activeTask}
              />
            </div>
            <div className='grid grid-cols-2 gap-3'>
              <ImageSelect
                label={t('Resolution')}
                value={resolution}
                options={options(
                  model?.capability.resolutions?.length
                    ? model.capability.resolutions
                    : ['1K', '2K', '4K']
                )}
                onChange={setResolution}
                disabled={busy || activeTask}
              />
              <ImageSelect
                label={t('Aspect Ratio')}
                value={ratio}
                options={options(IMAGE_RATIOS)}
                onChange={setRatio}
                disabled={busy || activeTask}
              />
            </div>
            {model?.capability.qualities?.length ? (
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
            {model?.capability.formats?.length ? (
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
            {model?.capability.backgrounds?.length ? (
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
            {model?.platform === 'openai' && (
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
            <fieldset className='space-y-3' disabled={busy || activeTask}>
              <legend className='mb-2 text-sm font-medium'>
                {t('Reference images and text parts')}
              </legend>
              {parts.map((part, index) => (
                <div
                  key={part.id}
                  className='bg-muted/30 space-y-2 rounded-lg border p-2'
                >
                  {part.type === 'image' && part.value.startsWith('data:') ? (
                    <img
                      className='h-20 w-full object-contain'
                      src={part.value}
                      alt={t('Reference image')}
                    />
                  ) : (
                    <Input
                      value={part.value}
                      onChange={(event) =>
                        setParts((previous) =>
                          previous.map((candidate) =>
                            candidate.id === part.id
                              ? { ...candidate, value: event.target.value }
                              : candidate
                          )
                        )
                      }
                      aria-label={t(
                        part.type === 'text'
                          ? 'Text part'
                          : 'Reference image URL'
                      )}
                      placeholder={
                        part.type === 'image' ? 'https://…' : t('Text part')
                      }
                    />
                  )}
                  <div className='flex justify-end gap-1'>
                    <Button
                      type='button'
                      variant='ghost'
                      size='icon'
                      aria-label={t('Move up')}
                      disabled={!index}
                      onClick={() => movePart(index, -1)}
                    >
                      <ArrowUp className='size-4' />
                    </Button>
                    <Button
                      type='button'
                      variant='ghost'
                      size='icon'
                      aria-label={t('Move down')}
                      disabled={index === parts.length - 1}
                      onClick={() => movePart(index, 1)}
                    >
                      <ArrowDown className='size-4' />
                    </Button>
                    <Button
                      type='button'
                      variant='ghost'
                      size='icon'
                      aria-label={t('Remove')}
                      onClick={() =>
                        setParts((previous) =>
                          previous.filter(
                            (candidate) => candidate.id !== part.id
                          )
                        )
                      }
                    >
                      <Trash2 className='size-4' />
                    </Button>
                  </div>
                </div>
              ))}
              <div className='flex flex-wrap gap-2'>
                <Button
                  type='button'
                  variant='outline'
                  size='sm'
                  onClick={() =>
                    setParts((previous) => [
                      ...previous,
                      { id: crypto.randomUUID(), type: 'image', value: '' },
                    ])
                  }
                >
                  <Plus className='size-3' />
                  {t('Image URL')}
                </Button>
                <Button
                  type='button'
                  variant='outline'
                  size='sm'
                  onClick={() =>
                    setParts((previous) => [
                      ...previous,
                      { id: crypto.randomUUID(), type: 'text', value: '' },
                    ])
                  }
                >
                  {t('Add text part')}
                </Button>
                <Label
                  htmlFor='image-reference-file'
                  className='text-muted-foreground cursor-pointer text-xs underline'
                >
                  {t('Upload reference image')}
                </Label>
                <Input
                  id='image-reference-file'
                  type='file'
                  accept='image/png,image/jpeg,image/webp'
                  className='text-xs'
                  onChange={(event) => {
                    const file = event.target.files?.[0]
                    if (file) void loadReference(file)
                    event.target.value = ''
                  }}
                />
              </div>
            </fieldset>
            <Button
              type='submit'
              className='w-full'
              disabled={!model || busy || activeTask}
            >
              <Sparkles className='size-4' aria-hidden='true' />
              {busy && t('Submitting')}
              {!busy && activeTask && t('Processing')}
              {!busy && !activeTask && t('Generate image')}
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
          </form>
          <section className='min-w-0 space-y-4'>
            <div className='flex flex-wrap items-center justify-between gap-2 border-b pb-3'>
              <h3 className='text-sm font-semibold'>{t('Results')}</h3>
              {(taskId || results.length > 0) && (
                <Badge variant='outline'>
                  {t(taskId ? 'Asynchronous' : 'Realtime')}
                </Badge>
              )}
            </div>
            {taskId && (
              <div
                aria-live='polite'
                className='bg-muted/30 space-y-2 rounded-xl border p-4'
              >
                <div className='flex flex-wrap items-center justify-between gap-2'>
                  <Badge variant='outline'>
                    {t(imageLabel(task.data?.task.status || 'queued'))}
                  </Badge>
                  <CopyButton value={taskId} size='sm'>
                    {taskId}
                  </CopyButton>
                </div>
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
            <ImageResults images={shownImages} />
          </section>
        </div>
      </SectionPageLayout.Content>
    </SectionPageLayout>
  )
}
