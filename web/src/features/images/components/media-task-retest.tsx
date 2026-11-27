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
import { keepPreviousData, useMutation, useQuery } from '@tanstack/react-query'
import type { TFunction } from 'i18next'
import { RotateCcw } from 'lucide-react'
import { useId, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'

import { ConfirmDialog } from '@/components/confirm-dialog'
import { Button } from '@/components/ui/button'
import { Combobox } from '@/components/ui/combobox'
import { Label } from '@/components/ui/label'
import { getApiKeys } from '@/features/keys/api'
import { formatTimestampToDate } from '@/lib/format'
import {
  getServerErrorMessage,
  getServerErrorSources,
  requireServerSuccess,
} from '@/lib/server-error-message'

import { getMediaTaskRetestOptions, retestMediaTask } from '../api'
import type {
  ImageTask,
  MediaTaskRetestOptions,
  MediaTaskRetestSelection,
} from '../types'
import { ImageSelect } from './image-select'

function retestAvailabilityMessage(code: unknown, t: TFunction) {
  switch (code) {
    case 'channel_not_recorded':
      return t('The original task has no recorded channel.')
    case 'channel_disabled':
      return t('Channel is disabled')
    case 'model_not_allowed':
      return t('This API key cannot use the original model.')
    case 'reference_unavailable':
      return t('An original reference could not be retrieved for retest.')
    case 'provider_reference_unavailable':
      return t(
        'A reference belongs to the original upstream account and cannot be reused.'
      )
  }
}

function retestErrorMessage(error: unknown, t: TFunction, fallback: string) {
  for (const { code } of getServerErrorSources(error)) {
    switch (code) {
      case 'request_expired':
        return t('Retest request expired')
      case 'request_unavailable':
        return t('The original request is unavailable for retest.')
      case 'idempotency_conflict':
        return t(
          'A retest has already been submitted with a different selection.'
        )
      case 'operator_key_unavailable':
        return t(
          'Use an active API key from your account that permits this address.'
        )
      case 'not_confirmed_failed':
        return t(
          'Retest requires a confirmed failure with no pending reconciliation.'
        )
      case 'operator_session_required':
        return t('Sign in to retest a media task.')
      default: {
        const message = retestAvailabilityMessage(code, t)
        if (message) return message
      }
    }
  }
  return getServerErrorMessage(error, fallback)
}

type RetestChoice = {
  options: MediaTaskRetestOptions
  channelId: number | null
  model: string
}

function retestModels(
  options: MediaTaskRetestOptions,
  channelId: number | null
) {
  if (channelId === 0) return options.auto_models ?? []
  return (
    options.channels?.find((channel) => channel.id === channelId)?.models ?? []
  )
}

export function MediaTaskRetest(props: {
  task: ImageTask
  userId: number
  now: number
  onAccepted: (taskId: string) => void
}) {
  const { t } = useTranslation()
  const fieldsId = useId()
  const [open, setOpen] = useState(false)
  const [tokenId, setTokenId] = useState('')
  const [choice, setChoice] = useState<RetestChoice>()
  const key = useRef('')
  const submitting = useRef(false)
  const until = props.task.retest_retain_until || 0
  const expired = until > 0 && until <= props.now
  const canRetest = props.task.can_retest === true && !expired

  const keys = useQuery({
    queryKey: ['media-retest-keys', props.userId],
    queryFn: async ({ signal }) => {
      const size = 100
      const first = requireServerSuccess(await getApiKeys({ p: 1, size })).data
      if (!first) return []
      const items = [...first.items]
      const pages = Math.ceil(first.total / size)
      for (let page = 2; page <= pages; page++) {
        signal.throwIfAborted()
        const next = requireServerSuccess(await getApiKeys({ p: page, size }))
        items.push(...(next.data?.items || []))
      }
      return items
    },
    enabled: open && canRetest,
    retry: false,
    meta: { errorToast: false },
  })
  const keyOptions = (keys.data || [])
    .filter(
      (item) =>
        item.status === 1 &&
        (item.expired_time === -1 || item.expired_time > props.now)
    )
    .map((item) => ({ value: String(item.id), label: item.name }))
  const selectedToken =
    keyOptions.find((option) => option.value === tokenId)?.value ||
    (keyOptions.length === 1 ? keyOptions[0].value : '')

  const options = useQuery({
    queryKey: [
      'media-retest-options',
      props.task.id,
      props.userId,
      selectedToken,
    ],
    queryFn: ({ signal }) =>
      getMediaTaskRetestOptions(props.task.id, Number(selectedToken), signal),
    enabled: open && canRetest && Boolean(selectedToken) && !keys.isError,
    placeholderData: keepPreviousData,
    retry: false,
    meta: { errorToast: false },
  })
  const candidates = selectedToken ? options.data : undefined

  if (candidates && !options.isFetching && candidates !== choice?.options) {
    let channelId = choice
      ? choice.channelId
      : candidates.original_channel_id || null
    if (
      channelId !== null &&
      retestModels(candidates, channelId).length === 0
    ) {
      channelId = null
    }
    const model = choice?.model ?? candidates.original_model
    setChoice({
      options: candidates,
      channelId,
      model:
        channelId === null ||
        retestModels(candidates, channelId).some((item) => item.model === model)
          ? model
          : '',
    })
  }

  const channelId = choice?.channelId ?? null
  const models = candidates ? retestModels(candidates, channelId) : []
  const model = models.find((item) => item.model === choice?.model)?.model ?? ''
  const originalUnavailable =
    candidates &&
    (retestAvailabilityMessage(candidates.original_unavailable_code, t) ||
      candidates.original_unavailable_reason)
  const canSubmit =
    canRetest &&
    selectedToken &&
    !keys.isError &&
    !options.isFetching &&
    !options.isError &&
    channelId !== null &&
    Boolean(model)

  const retest = useMutation({
    meta: { errorToast: false },
    mutationFn: (selection: MediaTaskRetestSelection) =>
      retestMediaTask(
        props.task.id,
        Number(selectedToken),
        key.current,
        selection
      ),
    onSuccess: (result) => {
      setOpen(false)
      props.onAccepted(result.task_id)
    },
    onSettled: () => {
      submitting.current = false
    },
  })

  function submit() {
    if (!canSubmit || channelId === null || submitting.current) {
      return
    }
    submitting.current = true
    retest.mutate({ channel_id: channelId, model })
  }

  if (!canRetest) {
    if (expired || props.task.retest_unavailable_reason === 'request_expired') {
      return (
        <p className='text-muted-foreground self-center text-xs'>
          {t('Retest request expired')}
        </p>
      )
    }
    if (props.task.retest_unavailable_reason === 'request_unavailable') {
      return (
        <p className='text-muted-foreground self-center text-xs'>
          {t('The original request is unavailable for retest.')}
        </p>
      )
    }
    return null
  }

  return (
    <>
      <Button
        size='sm'
        variant='outline'
        onClick={() => {
          retest.reset()
          setTokenId('')
          setChoice(undefined)
          key.current = crypto.randomUUID()
          setOpen(true)
        }}
      >
        <RotateCcw className='size-3.5' aria-hidden />
        {t('Retest')}
      </Button>
      <ConfirmDialog
        open={open}
        onOpenChange={(nextOpen) => {
          if (!submitting.current) setOpen(nextOpen)
        }}
        title={t('Retest failed task')}
        desc={t(
          'Create a new task with the original prompt and materials. Your selected API key pays for the new task.'
        )}
        confirmText={t('Retest')}
        disabled={!canSubmit}
        isLoading={retest.isPending}
        handleConfirm={submit}
      >
        <ImageSelect
          label={t('API Key')}
          value={selectedToken}
          options={keyOptions}
          onChange={(id) => {
            retest.reset()
            setTokenId(id)
          }}
          disabled={retest.isPending || keys.isError || keyOptions.length === 0}
        />
        {keys.isPending && <p role='status'>{t('Loading...')}</p>}
        {keys.isError && (
          <p role='alert' className='text-destructive text-sm'>
            {t('Failed to load API keys')}
          </p>
        )}
        {keys.isSuccess && keyOptions.length === 0 && (
          <p className='text-muted-foreground text-sm'>
            {t('Choose an enabled API key from your account to retest.')}
          </p>
        )}
        {selectedToken && options.isFetching && (
          <p role='status'>{t('Loading...')}</p>
        )}
        {options.isError && (
          <div className='flex items-start justify-between gap-2'>
            <p role='alert' className='text-destructive text-sm'>
              {retestErrorMessage(
                options.error,
                t,
                t('Failed to load retest options')
              )}
            </p>
            <Button
              variant='outline'
              size='sm'
              onClick={() => void options.refetch()}
            >
              {t('Retry')}
            </Button>
          </div>
        )}
        {candidates && (
          <div className='grid min-w-0 gap-3'>
            {originalUnavailable && (
              <p role='status' className='text-muted-foreground text-sm'>
                {t(
                  'Original selection ({{channel}}, {{model}}) is unavailable.',
                  {
                    channel:
                      candidates.original_channel_name ||
                      (candidates.original_channel_id > 0
                        ? `#${candidates.original_channel_id}`
                        : t('Unknown')),
                    model: candidates.original_model,
                  }
                )}{' '}
                {originalUnavailable}
              </p>
            )}
            <div className='min-w-0 space-y-2'>
              <Label htmlFor={`${fieldsId}-channel`}>{t('Channel')}</Label>
              <Combobox
                id={`${fieldsId}-channel`}
                placeholder={t('Search...')}
                disabled={
                  retest.isPending || options.isFetching || options.isError
                }
                value={channelId === null ? '' : String(channelId)}
                options={[
                  {
                    value: '0',
                    label: t('Automatic routing'),
                    disabled: !candidates.auto_models?.length,
                  },
                  ...(candidates.channels ?? []).map((channel) => ({
                    value: String(channel.id),
                    label: channel.name || `#${channel.id}`,
                    description: channel.name ? `#${channel.id}` : undefined,
                  })),
                ]}
                onValueChange={(value) => {
                  if (value === null) return
                  retest.reset()
                  const nextChannelId = Number(value)
                  const nextModels = retestModels(candidates, nextChannelId)
                  const previousModel = choice?.model ?? ''
                  setChoice({
                    options: candidates,
                    channelId: nextChannelId,
                    model: nextModels.some(
                      (item) => item.model === previousModel
                    )
                      ? previousModel
                      : '',
                  })
                }}
              />
            </div>
            <div className='min-w-0 space-y-2'>
              <Label htmlFor={`${fieldsId}-model`}>{t('Model')}</Label>
              <Combobox
                id={`${fieldsId}-model`}
                placeholder={t('Search models...')}
                disabled={
                  retest.isPending ||
                  options.isFetching ||
                  options.isError ||
                  channelId === null
                }
                value={model}
                options={models.map((item) => ({
                  value: item.model,
                  label:
                    item.upstream_model && item.upstream_model !== item.model
                      ? `${item.model} (${item.upstream_model})`
                      : item.model,
                }))}
                onValueChange={(value) => {
                  if (value === null) return
                  retest.reset()
                  setChoice({ options: candidates, channelId, model: value })
                }}
              />
            </div>
            {!candidates.channels?.length &&
              !candidates.auto_models?.length && (
                <p className='text-muted-foreground text-sm'>
                  {t('No compatible channels or models for this API key.')}
                </p>
              )}
          </div>
        )}
        {until > 0 && (
          <p className='text-muted-foreground text-xs'>
            {t('Available until {{time}}', {
              time: formatTimestampToDate(until),
            })}
          </p>
        )}
        {retest.isError && (
          <p role='alert' className='text-destructive text-sm'>
            {retestErrorMessage(retest.error, t, t('Image request failed'))}
          </p>
        )}
      </ConfirmDialog>
    </>
  )
}
