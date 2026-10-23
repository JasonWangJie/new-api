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
import { useMutation, useQuery } from '@tanstack/react-query'
import { isAxiosError } from 'axios'
import { Clapperboard, Film } from 'lucide-react'
import { useEffect, useState } from 'react'
import { useTranslation } from 'react-i18next'

import { EmptyState } from '@/components/empty-state'
import { SectionPageLayout } from '@/components/layout/components/section-page-layout'
import { LoadingState } from '@/components/loading-state'
import { Badge } from '@/components/ui/badge'
import { Progress } from '@/components/ui/progress'
import { fetchTokenKey, getApiKeys } from '@/features/keys/api'

import { getImageCapabilities, getImageTask, imageRelayRequest } from './api'
import { ImageSelect } from './components/image-select'
import {
  VideoCapabilityForm,
  type VideoFormSubmission,
} from './components/video-capability-form'
import { VideoResults } from './components/video-results'
import { imageLabel } from './lib/image-labels'
import { terminalImageStatus } from './lib/image-request'
import {
  readWorkbenchTask,
  rememberWorkbenchTask,
  workbenchSubmissionKey,
} from './lib/workbench-session'

function pricingStatusLabel(status: string): string {
  if (status === 'configured') return 'Pricing configured'
  if (status === 'invalid_configuration') {
    return 'Pricing configuration is invalid'
  }
  return 'Pricing expression required'
}

export function VideoWorkbench(props: { userId: number }) {
  const { t } = useTranslation()
  const [tokenId, setTokenId] = useState('')
  const [modelId, setModelId] = useState('')
  const [taskId, setTaskId] = useState(() =>
    readWorkbenchTask(props.userId, 'video')
  )
  const keys = useQuery({
    queryKey: ['image-keys', props.userId],
    queryFn: () => getApiKeys({ p: 1, size: 100 }),
  })
  const capabilities = useQuery({
    queryKey: ['image-capabilities', props.userId, tokenId],
    queryFn: ({ signal }) => getImageCapabilities(Number(tokenId), signal),
    enabled: !!tokenId,
  })
  const models = capabilities.data?.video_models || []
  const selectedModel = models.find(
    (candidate) => `${candidate.provider}:${candidate.id}` === modelId
  )
  const task = useQuery({
    queryKey: ['video-workbench-task', props.userId, taskId],
    queryFn: ({ signal }) => getImageTask(false, taskId, signal),
    enabled: !!taskId,
    refetchInterval: (query) => {
      const current = query.state.data?.task
      return current &&
        terminalImageStatus(current.status, current.next_attempt_at)
        ? false
        : Math.min(30000, 3000 * 2 ** query.state.fetchFailureCount)
    },
  })
  useEffect(() => {
    if (!isAxiosError(task.error) || task.error.response?.status !== 404) return
    rememberWorkbenchTask(props.userId, 'video', '')
    setTaskId('')
  }, [task.error, props.userId])
  const submit = useMutation({
    mutationFn: async (submission: VideoFormSubmission) => {
      if (!selectedModel?.available) {
        throw new Error(t('Invalid video request'))
      }
      const credential = await fetchTokenKey(Number(tokenId))
      const key = credential.data?.key
      if (!credential.success || !key) {
        throw new Error(t('Invalid video request'))
      }
      let body: string | FormData = JSON.stringify(submission.body)
      if (submission.files.length > 0) {
        body = new FormData()
        for (const [name, value] of Object.entries(submission.body)) {
          body.append(
            name,
            typeof value === 'string' ? value : JSON.stringify(value)
          )
        }
        for (const upload of submission.files) {
          body.append(upload.field, upload.file)
        }
      }
      let endpoint = '/v1/videos/generations_async'
      if (submission.mode === 'edit_video') {
        endpoint = '/v1/videos/edits_async'
      } else if (submission.mode === 'extend_video') {
        endpoint = '/v1/videos/extensions_async'
      }
      const response = await imageRelayRequest(
        endpoint,
        key,
        body,
        await workbenchSubmissionKey(
          props.userId,
          'video',
          Number(tokenId),
          endpoint,
          body
        )
      )
      if (typeof response.task_id !== 'string') {
        throw new Error(t('Video task was not returned'))
      }
      return response.task_id
    },
    onSuccess: (id) => {
      setTaskId(id)
      rememberWorkbenchTask(props.userId, 'video', id)
    },
  })
  const changed = () => submit.reset()
  const activeTask =
    !!taskId &&
    (!task.data ||
      !terminalImageStatus(
        task.data.task.status,
        task.data.task.next_attempt_at
      ))
  const results = task.data?.results || []

  return (
    <SectionPageLayout>
      <SectionPageLayout.Title>{t('Video workbench')}</SectionPageLayout.Title>
      <SectionPageLayout.Content>
        <div className='grid min-h-full items-start gap-5 xl:grid-cols-[minmax(360px,440px)_minmax(0,1fr)]'>
          <div className='bg-card min-w-0 overflow-hidden rounded-2xl border shadow-sm'>
            <div className='from-primary/5 flex items-center gap-4 border-b bg-linear-to-br to-transparent p-5 sm:p-6'>
              <div className='bg-primary/10 text-primary flex size-11 shrink-0 items-center justify-center rounded-xl'>
                <Clapperboard className='size-5' aria-hidden='true' />
              </div>
              <div className='min-w-0'>
                <p className='text-muted-foreground mb-1 text-[10px] font-semibold tracking-[0.2em]'>
                  {t('CREATE')}
                </p>
                <h3 className='font-serif text-xl leading-snug'>
                  {t('A place for your next video')}
                </h3>
              </div>
            </div>
            <div className='space-y-4 p-5 sm:p-6 [&_[data-slot=select-trigger]]:min-h-11'>
              <ImageSelect
                label={t('API Key')}
                value={tokenId}
                options={(keys.data?.data?.items || []).map((key) => ({
                  value: String(key.id),
                  label: key.name,
                }))}
                disabled={submit.isPending}
                onChange={(id) => {
                  setTokenId(id)
                  setModelId('')
                  changed()
                }}
              />
              <ImageSelect
                label={t('Model')}
                value={modelId}
                options={models.map((entry) => ({
                  value: `${entry.provider}:${entry.id}`,
                  label: `${entry.label} · ${entry.provider}`,
                }))}
                disabled={
                  submit.isPending || !tokenId || capabilities.isFetching
                }
                onChange={(id) => {
                  setModelId(id)
                  changed()
                }}
              />
              {capabilities.isError && (
                <p
                  role='alert'
                  className='text-destructive text-sm wrap-anywhere'
                >
                  {capabilities.error.message}
                </p>
              )}
              {selectedModel && (
                <div
                  className='flex flex-wrap items-center gap-2'
                  aria-live='polite'
                >
                  <Badge
                    variant={
                      selectedModel.available ? 'secondary' : 'destructive'
                    }
                  >
                    {t(selectedModel.available ? 'Available' : 'Unavailable')}
                  </Badge>
                  <Badge
                    variant={
                      selectedModel.pricing_status === 'configured'
                        ? 'outline'
                        : 'warning'
                    }
                    className='max-w-full whitespace-normal'
                  >
                    {t(
                      pricingStatusLabel(
                        selectedModel.pricing_status || 'needs_configuration'
                      )
                    )}
                  </Badge>
                </div>
              )}
              {selectedModel && !selectedModel.available && (
                <p
                  role='status'
                  className='text-muted-foreground bg-muted/50 rounded-lg border p-3 text-xs leading-relaxed'
                >
                  {t(
                    selectedModel.availability_reason ||
                      'Async video generation is disabled'
                  )}
                </p>
              )}
              {!selectedModel && (
                <p className='text-muted-foreground bg-muted/40 rounded-xl border border-dashed p-4 text-xs leading-relaxed'>
                  {t(
                    'Choose an API key and model, then describe the video you want to create.'
                  )}
                </p>
              )}
            </div>
            {selectedModel && (
              <VideoCapabilityForm
                key={`${selectedModel.provider}:${selectedModel.id}`}
                model={selectedModel}
                busy={submit.isPending}
                error={submit.error?.message}
                onChanged={changed}
                onSubmit={(submission) => submit.mutate(submission)}
              />
            )}
          </div>
          <section
            aria-label={t('Results')}
            className='bg-card min-w-0 overflow-hidden rounded-2xl border shadow-sm xl:sticky xl:top-0'
          >
            <div className='flex flex-wrap items-center justify-between gap-3 border-b px-5 py-4 sm:px-6'>
              <div className='flex items-center gap-3'>
                <div className='bg-muted text-muted-foreground flex size-9 items-center justify-center rounded-lg'>
                  <Film className='size-4' aria-hidden='true' />
                </div>
                <div>
                  <h3
                    id='video-preview-heading'
                    className='text-sm font-semibold'
                  >
                    {t('Video preview')}
                  </h3>
                  <p className='text-muted-foreground mt-0.5 text-xs'>
                    {t('Results')}
                  </p>
                </div>
              </div>
              {task.data && (
                <Badge variant='outline'>
                  {t(imageLabel(task.data.task.status))}
                </Badge>
              )}
            </div>
            <div className='space-y-4 p-4 sm:p-5'>
              {task.data && (
                <div className='bg-card/95 space-y-3 rounded-xl border p-4 shadow-sm'>
                  <p
                    role='status'
                    className='flex flex-wrap items-center justify-between gap-2 text-xs'
                  >
                    <span className='font-medium'>
                      {t(imageLabel(task.data.task.status))} ·{' '}
                      {task.data.task.progress}%
                    </span>
                    <span className='text-muted-foreground'>
                      {t('Actual cost')}: {task.data.task.cost}
                    </span>
                  </p>
                  {activeTask && (
                    <Progress
                      value={task.data.task.progress}
                      aria-label={t('Video preview')}
                    />
                  )}
                </div>
              )}
              {task.data?.task.error_message && (
                <p
                  role='alert'
                  className='text-destructive bg-card rounded-xl border p-4 text-sm wrap-anywhere'
                >
                  {task.data.task.error_message}
                </p>
              )}
              {task.error && (
                <p
                  role='alert'
                  className='text-destructive bg-card rounded-xl border p-4 text-sm wrap-anywhere'
                >
                  {task.error.message}
                </p>
              )}
              {results.length > 0 && (
                <div className='bg-card rounded-xl border p-3 shadow-sm sm:p-4'>
                  <VideoResults
                    results={results}
                    onRefresh={() => {
                      void task.refetch()
                    }}
                  />
                </div>
              )}
              {results.length === 0 && activeTask && !task.error && (
                <LoadingState
                  className='min-h-[340px]'
                  message={t('Processing...')}
                />
              )}
              {results.length === 0 && (!activeTask || task.error) && (
                <div className='bg-muted/20 flex min-h-80 items-center justify-center rounded-xl border border-dashed [background-image:radial-gradient(var(--border)_1px,transparent_1px)] [background-size:20px_20px] sm:min-h-[420px]'>
                  <EmptyState
                    icon={Clapperboard}
                    title={t('Your videos will appear here')}
                    description={t(
                      'Choose an API key and model, then describe the video you want to create.'
                    )}
                    className='bg-card/85 m-4 min-h-0 max-w-sm rounded-2xl border p-6 shadow-sm sm:p-8'
                  />
                </div>
              )}
            </div>
            <p className='text-muted-foreground border-t px-5 py-3 text-xs leading-relaxed'>
              {t(
                'Generated videos are saved on the server. Links can be refreshed from the task center.'
              )}
            </p>
          </section>
        </div>
      </SectionPageLayout.Content>
    </SectionPageLayout>
  )
}
