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
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'

import { Dialog } from '@/components/dialog'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { formatCnyCents, parseCnyCents } from '@/features/wallet/lib/billing'
import { useDebounce } from '@/hooks/use-debounce'
import { getCurrencyDisplay, getCurrencyLabel } from '@/lib/currency'
import { formatQuota, parseQuotaFromDollars } from '@/lib/format'
import { handleServerError } from '@/lib/handle-server-error'
import { getServerErrorStatus } from '@/lib/server-error-message'
import { cn } from '@/lib/utils'

import { adjustUserQuota, previewUserQuota } from '../api'
import type { ManageUserQuotaPayload, QuotaAdjustMode } from '../types'

interface UserQuotaDialogProps {
  open: boolean
  onOpenChange: (open: boolean) => void
  userId: number
  currentQuota: number
  onSuccess: () => void
}

export function UserQuotaDialog(props: UserQuotaDialogProps) {
  const { t } = useTranslation()
  const queryClient = useQueryClient()
  const [mode, setMode] = useState<QuotaAdjustMode>('add')
  const [amount, setAmount] = useState('')
  const [cashInput, setCashInput] = useState<string | null>(null)
  const [loading, setLoading] = useState(false)
  const [uncertain, setUncertain] = useState(false)
  const submitted = useRef<ManageUserQuotaPayload | null>(null)
  const submitting = useRef(false)

  const { meta: currencyMeta } = getCurrencyDisplay()
  const currencyLabel = getCurrencyLabel()
  const tokensOnly = currencyMeta.kind === 'tokens'
  const parsedAmount = Number(amount)
  const value =
    mode === 'override'
      ? parseQuotaFromDollars(parsedAmount)
      : parseQuotaFromDollars(Math.abs(parsedAmount))
  const validValue =
    amount.trim() !== '' &&
    Number.isFinite(parsedAmount) &&
    Number.isSafeInteger(value) &&
    (mode === 'override' || value > 0)
  const debouncedValue = useDebounce(value, 250)
  const quoteQuery = useQuery({
    queryKey: ['user-quota-preview', props.userId, mode, debouncedValue],
    queryFn: ({ signal }) =>
      previewUserQuota(
        { id: props.userId, mode, value: debouncedValue },
        signal
      ),
    enabled: props.open && validValue && debouncedValue === value && !uncertain,
    retry: false,
    staleTime: 0,
  })
  const quote =
    validValue && debouncedValue === value ? quoteQuery.data : undefined
  const createsOrder = quote !== undefined && BigInt(quote.delta_quota) > 0n
  const cash =
    cashInput ??
    (quote ? formatCnyCents(quote.suggested_payment_amount_cents) : '')
  const paymentCents = parseCnyCents(cash)
  const frozen = loading || uncertain
  const canConfirm =
    uncertain ||
    (!!quote &&
      !quoteQuery.isFetching &&
      (!createsOrder || paymentCents !== null))

  const handleClose = (next: boolean) => {
    if (!next && (submitting.current || submitted.current)) return
    if (!next) {
      setAmount('')
      setMode('add')
      setCashInput(null)
    }
    props.onOpenChange(next)
  }

  const handleConfirm = async () => {
    if (submitting.current || !canConfirm) return
    let payload = submitted.current
    if (!payload) {
      if (!quote) return
      payload = { id: props.userId, action: 'add_quota', mode, value }
      if (createsOrder) {
        if (paymentCents === null) return
        payload.payment_amount_cents = paymentCents
        payload.expected_delta_quota = quote.delta_quota
        if (typeof crypto.randomUUID === 'function') {
          payload.idempotency_key = crypto.randomUUID()
        } else {
          // getRandomValues also works when the dashboard uses an HTTP origin.
          const bytes = crypto.getRandomValues(new Uint8Array(16))
          bytes[6] = (bytes[6] & 0x0f) | 0x40
          bytes[8] = (bytes[8] & 0x3f) | 0x80
          const hex = Array.from(bytes, (byte) =>
            byte.toString(16).padStart(2, '0')
          ).join('')
          payload.idempotency_key = `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`
        }
      }
      submitted.current = payload
    }
    submitting.current = true
    setLoading(true)
    try {
      const result = await adjustUserQuota(payload)
      if (!result.success) {
        // A rejected replay cannot disprove an earlier uncertain commit.
        if (!uncertain) submitted.current = null
        handleServerError(result, t('Failed to adjust quota'))
        return
      }
      submitted.current = null
      setUncertain(false)
      if (result.data?.order) {
        toast.success(
          t('Quota adjusted; order {{order}} created', {
            order: result.data.order.trade_no,
          })
        )
      } else {
        toast.success(t('Quota adjusted successfully'))
      }
      setAmount('')
      setMode('add')
      setCashInput(null)
      void queryClient.invalidateQueries({ queryKey: ['users'] })
      void queryClient.invalidateQueries({
        queryKey: ['invoice-eligible-orders'],
      })
      props.onOpenChange(false)
      props.onSuccess()
    } catch (error: unknown) {
      const status = getServerErrorStatus(error)
      if (
        status !== undefined &&
        status >= 400 &&
        status < 500 &&
        (!uncertain || status === 409)
      ) {
        submitted.current = null
        setUncertain(false)
        if (status === 409) void quoteQuery.refetch()
      } else if (payload.idempotency_key) {
        // Keep the exact request until a replay confirms the transaction result.
        setUncertain(true)
      } else {
        submitted.current = null
      }
      handleServerError(error, t('Failed to adjust quota'))
    } finally {
      submitting.current = false
      setLoading(false)
    }
  }

  const placeholder = tokensOnly
    ? t('Enter amount in tokens')
    : t('Enter amount in {{currency}}', { currency: currencyLabel })

  return (
    <Dialog
      open={props.open}
      onOpenChange={handleClose}
      title={t('Adjust Quota')}
      description={t('Select an operation mode and enter the amount')}
      contentHeight='auto'
      showCloseButton={!frozen}
      bodyClassName='space-y-4'
      footer={
        <>
          <Button
            variant='outline'
            disabled={frozen}
            onClick={() => handleClose(false)}
          >
            {t('Cancel')}
          </Button>
          <Button onClick={handleConfirm} disabled={loading || !canConfirm}>
            {loading
              ? t('Processing...')
              : t(uncertain ? 'Retry confirmation' : 'Confirm')}
          </Button>
        </>
      }
    >
      <div className='space-y-4'>
        <div className='text-muted-foreground text-sm' aria-live='polite'>
          {t('Current quota')}:{' '}
          {formatQuota(quote?.before ?? props.currentQuota)}
          {quote && <> → {formatQuota(quote.after)}</>}
        </div>
        <div className='space-y-2'>
          <Label>{t('Mode')}</Label>
          <div className='flex gap-1'>
            {(['add', 'subtract', 'override'] as const).map((option) => (
              <Button
                key={option}
                type='button'
                variant='outline'
                size='sm'
                disabled={frozen}
                className={cn(
                  mode === option &&
                    'bg-primary text-primary-foreground hover:bg-primary/90 hover:text-primary-foreground'
                )}
                onClick={() => {
                  setMode(option)
                  setAmount('')
                  setCashInput(null)
                }}
              >
                {option === 'add' && t('Add')}
                {option === 'subtract' && t('Subtract')}
                {option === 'override' && t('Override')}
              </Button>
            ))}
          </div>
        </div>
        <div className='space-y-2'>
          <Label htmlFor='quota-adjustment-value'>
            {t('Amount')} ({currencyLabel})
          </Label>
          <Input
            id='quota-adjustment-value'
            type='number'
            step={tokensOnly ? 1 : 0.000001}
            min={mode === 'override' ? undefined : 0}
            placeholder={placeholder}
            value={amount}
            disabled={frozen}
            onChange={(event) => {
              setAmount(event.target.value)
              setCashInput(null)
            }}
            onKeyDown={(event) => {
              if (event.key === 'Enter') {
                event.preventDefault()
                void handleConfirm()
              }
            }}
          />
        </div>
        {quoteQuery.isFetching && (
          <p className='text-muted-foreground text-sm'>
            {t('Loading quota preview...')}
          </p>
        )}
        {quoteQuery.isError && (
          <div role='alert' className='space-y-2 text-sm'>
            <p>{t('Failed to load quota preview')}</p>
            <Button
              variant='outline'
              size='sm'
              onClick={() => void quoteQuery.refetch()}
            >
              {t('Retry')}
            </Button>
          </div>
        )}
        {createsOrder && (
          <div className='space-y-2 rounded-md border p-3'>
            <Label htmlFor='quota-adjustment-payment'>
              {t('Received amount (CNY)')}
            </Label>
            <Input
              id='quota-adjustment-payment'
              inputMode='decimal'
              value={cash}
              disabled={frozen}
              onChange={(event) => setCashInput(event.target.value)}
              onKeyDown={(event) => {
                if (event.key === 'Enter') {
                  event.preventDefault()
                  void handleConfirm()
                }
              }}
            />
            <p className='text-muted-foreground text-sm'>
              {t('Payment Method')}: {t('Manual backend')}
            </p>
            <p className='text-muted-foreground text-xs'>
              {t(
                'Confirm the actual RMB received. Changing it does not change the credited quota.'
              )}
            </p>
            {paymentCents === 0 && (
              <p className='text-muted-foreground text-xs'>
                {t(
                  'A zero payment creates a gift order that cannot be invoiced.'
                )}
              </p>
            )}
          </div>
        )}
        {uncertain && (
          <p role='alert' className='text-sm'>
            {t(
              'The result is not confirmed. Retry with the same values to avoid duplicate credit.'
            )}
          </p>
        )}
      </div>
    </Dialog>
  )
}
