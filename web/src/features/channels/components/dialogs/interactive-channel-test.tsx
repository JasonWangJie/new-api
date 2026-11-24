/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as published by
the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For commercial licensing, please contact support@quantumnous.com
*/
import { useMutation, useQuery } from '@tanstack/react-query'
import type { TFunction } from 'i18next'
import { Play, RotateCw } from 'lucide-react'
import { type FormEvent, useEffect, useId, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'

import { ResponseImage } from '@/components/ai-elements/response-renderer-image'
import { LoadingState } from '@/components/loading-state'
import { StatusBadge } from '@/components/status-badge'
import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Combobox } from '@/components/ui/combobox'
import { Label } from '@/components/ui/label'
import { Textarea } from '@/components/ui/textarea'
import {
  getServerErrorMessage,
  getServerErrorSources,
} from '@/lib/server-error-message'

import {
  getInteractiveChannelTestModels,
  runInteractiveChannelTest,
} from '../../api'
import { formatResponseTime } from '../../lib/channel-utils'
import type {
  InteractiveChannelTestMode,
  InteractiveChannelTestRequest,
} from '../../types'

type InteractiveChannelTestProps = {
  channelId: number
  defaultModel?: string
  endpointOptions: readonly { value: string; label: string }[]
}

const MAX_PROMPT_LENGTH = 4096

function interactiveTestMessage(
  t: TFunction,
  error: unknown,
  fallback: string
) {
  const message = getServerErrorMessage(error, fallback)
  const code = getServerErrorSources(error).find(
    (source) => typeof source.error_code === 'string'
  )?.error_code
  const detail = t(message, { defaultValue: message })
  switch (code) {
    case 'channel_test_upstream_image_pending':
      return t(
        'The upstream accepted the image task, but its result could not be confirmed.'
      )
    case 'channel_test_upstream_acceptance_unknown':
      return t(
        'Could not confirm whether the upstream accepted the image task.'
      )
    case 'channel_test_upstream_image_failed': {
      const summary = t('The upstream image task failed.')
      return message === 'The TabTab image task failed'
        ? summary
        : `${summary}\n${detail}`
    }
    case 'channel_test_upstream_image_preview_failed':
      return `${t('The upstream generated the image, but the preview could not be loaded.')}\n${detail}`
  }
  const httpError = /^Upstream returned HTTP (\d{3})(?:: ([\s\S]+))?$/.exec(
    message
  )
  if (httpError) {
    const summary = t('Upstream returned HTTP {{status}}', {
      status: httpError[1],
    })
    return httpError[2] ? `${summary}\n${httpError[2]}` : summary
  }
  return detail
}

export function InteractiveChannelTest(props: InteractiveChannelTestProps) {
  const { t } = useTranslation()
  const id = useId()
  const request = useRef<AbortController | null>(null)
  const [modelChoice, setModelChoice] = useState(props.defaultModel)
  const [modeChoice, setModeChoice] = useState<InteractiveChannelTestMode>()
  const [endpointChoice, setEndpointChoice] = useState<string>()
  const [prompts, setPrompts] = useState<
    Partial<Record<InteractiveChannelTestMode, string>>
  >({})

  const options = useQuery({
    queryKey: ['channels', 'interactive-test-models', props.channelId],
    queryFn: ({ signal }) =>
      getInteractiveChannelTestModels(props.channelId, signal),
    retry: false,
    meta: { errorToast: false },
  })
  const test = useMutation({
    mutationFn: (variables: {
      input: InteractiveChannelTestRequest
      signal: AbortSignal
    }) =>
      runInteractiveChannelTest(
        props.channelId,
        variables.input,
        variables.signal
      ),
    retry: false,
    gcTime: 0,
    meta: { errorToast: false },
  })

  useEffect(() => () => request.current?.abort(), [])

  if (options.isPending) {
    return <LoadingState className='min-h-32' />
  }

  if (options.isError) {
    const title = t('Failed to load test models')
    const message = interactiveTestMessage(t, options.error, title)
    return (
      <Alert variant='destructive'>
        <AlertTitle>{title}</AlertTitle>
        <AlertDescription>
          {message !== title && message}
          <Button
            variant='outline'
            size='sm'
            className='mt-2 w-fit'
            onClick={() => void options.refetch()}
          >
            {t('Retry')}
          </Button>
        </AlertDescription>
      </Alert>
    )
  }

  const models = options.data.filter(
    (model) =>
      model.chat_endpoint_types?.length || model.image_endpoint_types?.length
  )
  const model = models.find((item) => item.model === modelChoice) ?? models[0]
  if (!model) {
    return (
      <p className='text-muted-foreground py-8 text-center text-sm'>
        {t('This channel has no models available for interactive testing.')}
      </p>
    )
  }

  const chatEndpoints = model.chat_endpoint_types ?? []
  const imageEndpoints = model.image_endpoint_types ?? []
  const modeOptions: { value: InteractiveChannelTestMode; label: string }[] = []
  if (chatEndpoints.length) {
    modeOptions.push({ value: 'chat', label: t('Chat') })
  }
  if (imageEndpoints.length) {
    modeOptions.push({ value: 'image', label: t('Image generation') })
  }
  const mode =
    modeOptions.find(
      (option) => option.value === (modeChoice ?? model.default_mode)
    )?.value ?? modeOptions[0].value
  const endpoints = mode === 'image' ? imageEndpoints : chatEndpoints
  const endpoint =
    endpointChoice && endpoints.includes(endpointChoice)
      ? endpointChoice
      : endpoints[0]
  const endpointOptions = endpoints.map((value) => ({
    value,
    label:
      props.endpointOptions.find((option) => option.value === value)?.label ??
      value,
  }))
  const promptLabel = mode === 'image' ? t('Image prompt') : t('Chat message')
  const prompt =
    prompts[mode] ??
    (mode === 'image'
      ? t('A friendly robot watering a tiny garden on the Moon.')
      : t('hi'))
  const promptTooLong = [...prompt.trim()].length > MAX_PROMPT_LENGTH
  const submittedPrompt = test.variables?.input.prompt
  const retry =
    (test.isError || test.isSuccess) && submittedPrompt === prompt.trim()
  const failedResponse: unknown = test.error?.cause
  const failureTime =
    failedResponse &&
    typeof failedResponse === 'object' &&
    'time' in failedResponse
      ? failedResponse.time
      : undefined
  const time = test.data?.time ?? failureTime
  const duration =
    typeof time === 'number' && time > 0
      ? formatResponseTime(Math.max(1, Math.round(time * 1000)), t)
      : undefined

  const submit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault()
    const input = prompt.trim()
    if (!input || promptTooLong || request.current) return
    const controller = new AbortController()
    request.current = controller
    test.mutate(
      {
        input: {
          model: model.model,
          mode,
          endpoint_type: endpoint,
          prompt: input,
        },
        signal: controller.signal,
      },
      {
        onSettled: () => {
          request.current = null
        },
      }
    )
  }

  const result = test.data?.data
  const failureTitle = t('Test failed')
  const failureMessage = test.isError
    ? interactiveTestMessage(t, test.error, failureTitle)
    : undefined

  return (
    <div className='mx-auto w-full max-w-xl space-y-4'>
      <form className='space-y-4' onSubmit={submit}>
        <div className='grid min-w-0 gap-3 sm:grid-cols-[minmax(0,3fr)_minmax(0,2fr)]'>
          <div className='grid min-w-0 gap-2'>
            <Label htmlFor={`${id}-model`}>{t('Model')}</Label>
            <Combobox
              id={`${id}-model`}
              value={model.model}
              options={models.map((item) => ({
                value: item.model,
                label: item.model,
              }))}
              disabled={test.isPending}
              onValueChange={(value) => {
                if (!value || value === model.model) return
                setModelChoice(value)
                setModeChoice(undefined)
                setEndpointChoice(undefined)
                test.reset()
              }}
            />
          </div>
          <div className='grid min-w-0 gap-2'>
            <Label htmlFor={`${id}-mode`}>{t('Test Mode')}</Label>
            <Combobox
              id={`${id}-mode`}
              value={mode}
              options={modeOptions}
              disabled={test.isPending || modeOptions.length < 2}
              onValueChange={(value: string | null) => {
                if ((value !== 'chat' && value !== 'image') || value === mode) {
                  return
                }
                setModeChoice(value)
                setEndpointChoice(undefined)
                test.reset()
              }}
            />
          </div>
        </div>
        {endpointOptions.length > 1 && (
          <div className='grid min-w-0 gap-2'>
            <Label htmlFor={`${id}-endpoint`}>{t('Endpoint Type')}</Label>
            <Combobox
              id={`${id}-endpoint`}
              value={endpoint}
              options={endpointOptions}
              disabled={test.isPending}
              onValueChange={(value) => {
                if (!value || value === endpoint) return
                setEndpointChoice(value)
                test.reset()
              }}
            />
          </div>
        )}
        <div className='grid gap-2'>
          <Label htmlFor={`${id}-prompt`}>{promptLabel}</Label>
          <Textarea
            id={`${id}-prompt`}
            required
            className='min-h-20 resize-y'
            value={prompt}
            disabled={test.isPending}
            aria-invalid={promptTooLong}
            aria-describedby={promptTooLong ? `${id}-prompt-limit` : undefined}
            onChange={(event) =>
              setPrompts({ ...prompts, [mode]: event.target.value })
            }
          />
          {promptTooLong && (
            <p
              id={`${id}-prompt-limit`}
              role='alert'
              className='text-destructive text-sm'
            >
              {t('Use at most {{limit}} characters.', {
                limit: MAX_PROMPT_LENGTH,
              })}
            </p>
          )}
        </div>
        <div className='flex justify-end'>
          <Button
            type='submit'
            disabled={test.isPending || !prompt.trim() || promptTooLong}
          >
            {retry ? (
              <RotateCw data-icon='inline-start' />
            ) : (
              <Play data-icon='inline-start' />
            )}
            {retry ? t('Retry') : t('Start test')}
          </Button>
        </div>
      </form>
      {test.isPending && (
        <div role='status' className='bg-muted/35 rounded-lg border px-3 py-4'>
          <LoadingState
            inline
            size='sm'
            message={
              mode === 'image'
                ? t('Waiting for the test image...')
                : t('Testing...')
            }
          />
        </div>
      )}
      {failureMessage && (
        <Alert variant='destructive'>
          <AlertTitle>{failureTitle}</AlertTitle>
          <AlertDescription className='wrap-anywhere whitespace-pre-wrap'>
            {failureMessage !== failureTitle && failureMessage}
            {duration && (
              <p className='mt-1 text-xs'>
                {t('Response time: {{duration}}', { duration })}
              </p>
            )}
          </AlertDescription>
        </Alert>
      )}
      {result && (
        <section
          aria-live='polite'
          className='bg-muted/35 rounded-lg border px-3 py-3 sm:px-4'
        >
          {result.mode === 'chat' && (
            <div className='border-border/75 mb-3 border-b pb-3'>
              <p className='text-muted-foreground text-xs font-medium'>
                {t('Chat message')}
              </p>
              <p className='mt-1 text-sm wrap-anywhere whitespace-pre-wrap'>
                {submittedPrompt}
              </p>
            </div>
          )}
          <div className='flex flex-wrap items-center gap-x-3 gap-y-1'>
            <h3 className='text-sm font-medium'>
              {result.mode === 'image'
                ? t('Generated image')
                : t('Model response')}
            </h3>
            <StatusBadge
              label={t('Success')}
              variant='success'
              copyable={false}
            />
            {duration && (
              <span className='text-muted-foreground text-xs'>
                {t('Response time: {{duration}}', { duration })}
              </span>
            )}
          </div>
          {result.mode === 'chat' && (
            <p className='mt-3 text-sm leading-relaxed wrap-anywhere whitespace-pre-wrap'>
              {result.text}
            </p>
          )}
          {result.image && (
            <ResponseImage
              key={test.submittedAt}
              node={{
                type: 'image',
                raw: '',
                src: result.image.data_url,
                alt: t('Generated image'),
                title: null,
              }}
            />
          )}
        </section>
      )}
    </div>
  )
}
