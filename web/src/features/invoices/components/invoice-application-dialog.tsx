/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.
*/
import { zodResolver } from '@hookform/resolvers/zod'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { useForm } from 'react-hook-form'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'

import { Dialog } from '@/components/dialog'
import { Button } from '@/components/ui/button'
import {
  Form,
  FormControl,
  FormField,
  FormItem,
  FormLabel,
} from '@/components/ui/form'
import { Input } from '@/components/ui/input'
import { handleServerError } from '@/lib/handle-server-error'

import { createInvoice } from '../api'
import { formatInvoiceAmount } from '../lib/format'
import {
  invoiceApplicationSchema,
  type InvoiceApplicationValues,
} from '../lib/schemas'
import type { InvoiceEligibleOrder } from '../types'

type InvoiceApplicationDialogProps = {
  open: boolean
  onOpenChange: (open: boolean) => void
  selectedOrders: Map<number, InvoiceEligibleOrder>
  totalAmountCents: number
  minAmountCents: number
  onSelectionReset: () => void
}

const formId = 'invoice-application-form'

export function InvoiceApplicationDialog(props: InvoiceApplicationDialogProps) {
  const { t, i18n } = useTranslation()
  const queryClient = useQueryClient()
  const form = useForm<InvoiceApplicationValues>({
    resolver: zodResolver(invoiceApplicationSchema),
    defaultValues: { company_name: '', tax_id: '', email: '' },
  })
  const mutation = useMutation({
    mutationFn: (values: InvoiceApplicationValues) =>
      createInvoice({
        ...values,
        order_ids: [...props.selectedOrders.keys()],
      }),
    onSuccess: async () => {
      toast.success(t('Invoice application submitted'))
      await Promise.all([
        queryClient.invalidateQueries({ queryKey: ['invoice-requests'] }),
        queryClient.invalidateQueries({
          queryKey: ['invoice-eligible-orders'],
        }),
      ])
      form.reset()
      props.onSelectionReset()
      props.onOpenChange(false)
    },
    onError: async (error) => {
      handleServerError(error, t('Failed to submit invoice application'))
      props.onSelectionReset()
      await queryClient.invalidateQueries({
        queryKey: ['invoice-eligible-orders'],
      })
    },
  })

  const canSubmit =
    props.selectedOrders.size > 0 &&
    props.totalAmountCents >= props.minAmountCents

  return (
    <Dialog
      open={props.open}
      onOpenChange={(open) => {
        if (!mutation.isPending) props.onOpenChange(open)
      }}
      title={t('Apply for enterprise invoice')}
      description={t(
        '{{count}} recharge orders selected, totaling {{amount}}. The invoice will be sent to the email address after offline processing.',
        {
          count: props.selectedOrders.size,
          amount: formatInvoiceAmount(
            props.totalAmountCents,
            i18n.resolvedLanguage
          ),
        }
      )}
      footer={
        <>
          <Button
            variant='outline'
            disabled={mutation.isPending}
            onClick={() => props.onOpenChange(false)}
          >
            {t('Cancel')}
          </Button>
          <Button
            type='submit'
            form={formId}
            disabled={!canSubmit || mutation.isPending}
          >
            {mutation.isPending ? t('Submitting...') : t('Submit application')}
          </Button>
        </>
      }
    >
      <Form {...form}>
        <form
          id={formId}
          className='space-y-4'
          onSubmit={form.handleSubmit((values) => mutation.mutate(values))}
        >
          <FormField
            control={form.control}
            name='company_name'
            render={({ field, fieldState }) => (
              <FormItem>
                <FormLabel>{t('Company name')}</FormLabel>
                <FormControl>
                  <Input
                    {...field}
                    autoComplete='organization'
                    maxLength={200}
                    placeholder={t('Enter the legal company name')}
                  />
                </FormControl>
                {fieldState.error?.message ? (
                  <p className='text-destructive text-xs'>
                    {t(fieldState.error.message)}
                  </p>
                ) : null}
              </FormItem>
            )}
          />
          <FormField
            control={form.control}
            name='tax_id'
            render={({ field, fieldState }) => (
              <FormItem>
                <FormLabel>{t('Enterprise tax ID')}</FormLabel>
                <FormControl>
                  <Input
                    {...field}
                    maxLength={64}
                    placeholder={t('Enter the enterprise tax ID')}
                  />
                </FormControl>
                {fieldState.error?.message ? (
                  <p className='text-destructive text-xs'>
                    {t(fieldState.error.message)}
                  </p>
                ) : null}
              </FormItem>
            )}
          />
          <FormField
            control={form.control}
            name='email'
            render={({ field, fieldState }) => (
              <FormItem>
                <FormLabel>{t('Invoice email')}</FormLabel>
                <FormControl>
                  <Input
                    {...field}
                    type='email'
                    autoComplete='email'
                    maxLength={254}
                    placeholder={t('Enter the email that receives the invoice')}
                  />
                </FormControl>
                {fieldState.error?.message ? (
                  <p className='text-destructive text-xs'>
                    {t(fieldState.error.message)}
                  </p>
                ) : null}
              </FormItem>
            )}
          />
        </form>
      </Form>
    </Dialog>
  )
}
