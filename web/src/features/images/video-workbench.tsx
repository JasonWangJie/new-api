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
import { useState } from 'react'
import { useTranslation } from 'react-i18next'

import { SectionPageLayout } from '@/components/layout'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Textarea } from '@/components/ui/textarea'
import { fetchTokenKey, getApiKeys } from '@/features/keys/api'

import { getImageCapabilities, getImageTask, imageRelayRequest } from './api'
import { ImageSelect } from './components/image-select'
import { VideoResults } from './components/video-results'
import { imageLabel } from './lib/image-labels'

export function VideoWorkbench(props: { userId: number }) {
  const { t } = useTranslation()
  const [tokenId, setTokenId] = useState('')
  const [modelId, setModelId] = useState('')
  const [prompt, setPrompt] = useState('')
  const [seconds, setSeconds] = useState('8')
  const [size, setSize] = useState('1280x720')
  const [reference, setReference] = useState('')
  const [referenceFile, setReferenceFile] = useState<File | null>(null)
  const [additional, setAdditional] = useState('{}')
  const [taskId, setTaskId] = useState(
    () => sessionStorage.getItem(`new-api:video-task:${props.userId}`) || ''
  )
  const [submissionKey, setSubmissionKey] = useState(() => crypto.randomUUID())
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
  const model = models.find(
    (candidate) => `${candidate.provider}:${candidate.id}` === modelId
  )
  const task = useQuery({
    queryKey: ['video-workbench-task', props.userId, taskId],
    queryFn: ({ signal }) => getImageTask(false, taskId, signal),
    enabled: !!taskId,
    refetchInterval: (query) => {
      const status = query.state.data?.task.status
      return status &&
        [
          'succeeded',
          'failed',
          'expired',
          'execution_unknown',
          'storage_failed',
        ].includes(status)
        ? false
        : 3000
    },
  })
  const submit = useMutation({
    mutationFn: async () => {
      const extra: unknown = JSON.parse(additional)
      if (
        !model?.available ||
        !prompt.trim() ||
        !extra ||
        typeof extra !== 'object' ||
        Array.isArray(extra) ||
        !Number.isInteger(Number(seconds)) ||
        Number(seconds) < 1 ||
        Number(seconds) > model.max_duration_seconds
      ) {
        throw new Error(t('Invalid video request'))
      }
      const credential = await fetchTokenKey(Number(tokenId))
      const key = credential.data?.key
      if (!credential.success || !key) {
        throw new Error(t('Invalid video request'))
      }
      if (
        referenceFile &&
        (!['image/jpeg', 'image/png'].includes(referenceFile.type) ||
          referenceFile.size > 20 << 20)
      ) {
        throw new Error(t('Invalid video request'))
      }
      const fields = {
        ...extra,
        model: model.id,
        provider: model.provider,
        prompt: prompt.trim(),
        seconds: Number(seconds),
        size,
        ...(reference && !referenceFile ? { input_reference: reference } : {}),
      }
      let body: string | FormData = JSON.stringify(fields)
      if (referenceFile) {
        body = new FormData()
        for (const [name, value] of Object.entries(fields)) {
          body.append(
            name,
            typeof value === 'string' ? value : JSON.stringify(value)
          )
        }
        body.append('input_reference', referenceFile)
      }
      const response = await imageRelayRequest(
        '/v1/videos/generations_async',
        key,
        body,
        submissionKey
      )
      if (typeof response.task_id !== 'string') {
        throw new Error(t('Video task was not returned'))
      }
      return response.task_id
    },
    onSuccess: (id) => {
      setTaskId(id)
      sessionStorage.setItem(`new-api:video-task:${props.userId}`, id)
      setSubmissionKey(crypto.randomUUID())
    },
  })
  // Changing any input starts a new idempotency scope. Retrying unchanged input
  // after an uncertain network response retains its key.
  const changed = () => setSubmissionKey(crypto.randomUUID())
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
          <form
            className='space-y-4'
            onSubmit={(event) => {
              event.preventDefault()
              submit.mutate()
            }}
            onChange={changed}
          >
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
            {model && !model.available && (
              <p role='status' className='text-muted-foreground text-sm'>
                {t('Async video generation is disabled')}
              </p>
            )}
            <div className='space-y-2'>
              <Label htmlFor='video-prompt'>{t('Prompt')}</Label>
              <Textarea
                id='video-prompt'
                value={prompt}
                onChange={(event) => setPrompt(event.target.value)}
                required
                disabled={submit.isPending}
              />
            </div>
            <div className='grid grid-cols-2 gap-3'>
              <div className='space-y-2'>
                <Label htmlFor='video-seconds'>{t('Duration (seconds)')}</Label>
                <Input
                  id='video-seconds'
                  type='number'
                  min={1}
                  max={model?.max_duration_seconds}
                  value={seconds}
                  onChange={(event) => setSeconds(event.target.value)}
                  disabled={submit.isPending}
                />
              </div>
              <div className='space-y-2'>
                <Label htmlFor='video-size'>{t('Size')}</Label>
                <Input
                  id='video-size'
                  value={size}
                  onChange={(event) => setSize(event.target.value)}
                  disabled={submit.isPending}
                />
              </div>
            </div>
            {model?.supported_parameters.includes('input_reference') && (
              <div className='space-y-2'>
                <Label htmlFor='video-reference'>
                  {t('Reference image URL')}
                </Label>
                <Input
                  id='video-reference'
                  value={reference}
                  onChange={(event) => setReference(event.target.value)}
                  disabled={submit.isPending || !!referenceFile}
                />
                <Label htmlFor='video-reference-file'>
                  {t('Upload reference image')}
                </Label>
                <Input
                  id='video-reference-file'
                  type='file'
                  accept='image/png,image/jpeg'
                  disabled={submit.isPending}
                  onChange={(event) =>
                    setReferenceFile(event.target.files?.[0] || null)
                  }
                />
              </div>
            )}
            <div className='space-y-2'>
              <Label htmlFor='video-additional'>
                {t('Additional parameters')}
              </Label>
              <Textarea
                id='video-additional'
                value={additional}
                onChange={(event) => setAdditional(event.target.value)}
                disabled={submit.isPending}
              />
            </div>
            <Button
              type='submit'
              disabled={!model?.available || submit.isPending}
            >
              {t(submit.isPending ? 'Submitting...' : 'Generate video')}
            </Button>
            {submit.error && (
              <p role='alert' className='text-destructive text-sm'>
                {submit.error.message}
              </p>
            )}
          </form>
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
