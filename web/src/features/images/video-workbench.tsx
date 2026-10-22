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
import { useEffect, useState } from 'react'
import { useTranslation } from 'react-i18next'

import { SectionPageLayout } from '@/components/layout/components/section-page-layout'
import { Badge } from '@/components/ui/badge'
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

  return (
    <SectionPageLayout>
      <SectionPageLayout.Title>{t('Video workbench')}</SectionPageLayout.Title>
      <SectionPageLayout.Content>
        <p className='text-muted-foreground mb-4 text-sm'>
          {t(
            'Generated videos are saved on the server. Links can be refreshed from the task center.'
          )}
        </p>
        <div className='grid gap-6 lg:grid-cols-2'>
          <div className='space-y-4'>
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
              disabled={submit.isPending}
              onChange={(id) => {
                setModelId(id)
                changed()
              }}
            />
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
              <p role='status' className='text-muted-foreground text-sm'>
                {t(
                  selectedModel.availability_reason ||
                    'Async video generation is disabled'
                )}
              </p>
            )}
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
          <div className='space-y-4'>
            {task.data && (
              <p role='status'>
                {t(imageLabel(task.data.task.status))} ·{' '}
                {task.data.task.progress}% · {t('Actual cost')}:{' '}
                {task.data.task.cost}
              </p>
            )}
            {task.data?.task.error_message && (
              <p role='alert'>{task.data.task.error_message}</p>
            )}
            {task.error && <p role='alert'>{task.error.message}</p>}
            <VideoResults
              results={task.data?.results || []}
              onRefresh={() => {
                void task.refetch()
              }}
            />
          </div>
        </div>
      </SectionPageLayout.Content>
    </SectionPageLayout>
  )
}
