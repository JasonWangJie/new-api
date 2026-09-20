/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.
*/
import { zodResolver } from '@hookform/resolvers/zod'
import { useForm, type Resolver } from 'react-hook-form'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'
import { z } from 'zod'

import {
  Form,
  FormControl,
  FormDescription,
  FormField,
  FormItem,
  FormLabel,
} from '@/components/ui/form'
import { Input } from '@/components/ui/input'
import { Switch } from '@/components/ui/switch'

import {
  SettingsForm,
  SettingsSwitchContent,
  SettingsSwitchItem,
} from '../components/settings-form-layout'
import { SettingsPageFormActions } from '../components/settings-page-context'
import { SettingsSection } from '../components/settings-section'
import { useUpdateOption } from '../hooks/use-update-option'

const schema = z.object({
  enabled: z.boolean(),
  minAmount: z.coerce
    .number()
    .min(0, 'Minimum invoice amount cannot be negative')
    .refine(
      (value) =>
        Math.abs(value * 100 - Math.round(value * 100)) < Number.EPSILON * 100,
      'Invoice amount supports at most two decimal places'
    ),
})

type Values = z.infer<typeof schema>

export function InvoiceSettingsSection(props: {
  defaultValues: { enabled: boolean; minAmountCents: number }
}) {
  const { t } = useTranslation()
  const updateOption = useUpdateOption()
  const form = useForm<Values>({
    resolver: zodResolver(schema) as Resolver<Values>,
    defaultValues: {
      enabled: props.defaultValues.enabled,
      minAmount: props.defaultValues.minAmountCents / 100,
    },
  })
  const { isDirty, isSubmitting } = form.formState

  async function onSubmit(values: Values) {
    const minAmountCents = Math.round(values.minAmount * 100)
    const updates: Array<{ key: string; value: string }> = []
    if (values.enabled !== props.defaultValues.enabled) {
      updates.push({
        key: 'payment_setting.invoice_enabled',
        value: String(values.enabled),
      })
    }
    if (minAmountCents !== props.defaultValues.minAmountCents) {
      updates.push({
        key: 'payment_setting.invoice_min_amount_cents',
        value: String(minAmountCents),
      })
    }
    if (updates.length === 0) {
      toast.info(t('No changes to save'))
      return
    }
    for (const update of updates) await updateOption.mutateAsync(update)
    form.reset(values)
  }

  return (
    <SettingsSection title={t('Enterprise Invoice Settings')}>
      <Form {...form}>
        <SettingsForm onSubmit={form.handleSubmit(onSubmit)} autoComplete='off'>
          <SettingsPageFormActions
            onSave={form.handleSubmit(onSubmit)}
            isSaving={updateOption.isPending || isSubmitting}
            isSaveDisabled={!isDirty}
            saveLabel='Save enterprise invoice settings'
          />
          <FormField
            control={form.control}
            name='enabled'
            render={({ field }) => (
              <SettingsSwitchItem>
                <SettingsSwitchContent>
                  <FormLabel>
                    {t('Enable enterprise invoice applications')}
                  </FormLabel>
                  <FormDescription>
                    {t(
                      'When disabled, users cannot submit new applications, while records and admin processing remain available.'
                    )}
                  </FormDescription>
                </SettingsSwitchContent>
                <FormControl>
                  <Switch
                    checked={field.value}
                    onCheckedChange={field.onChange}
                    disabled={updateOption.isPending || isSubmitting}
                  />
                </FormControl>
              </SettingsSwitchItem>
            )}
          />
          <FormField
            control={form.control}
            name='minAmount'
            render={({ field, fieldState }) => (
              <FormItem className='max-w-sm'>
                <FormLabel>{t('Minimum invoice amount (CNY)')}</FormLabel>
                <FormControl>
                  <Input
                    {...field}
                    type='number'
                    min={0}
                    step='0.01'
                    inputMode='decimal'
                  />
                </FormControl>
                <FormDescription>
                  {t(
                    'Set to 0 to allow applications without an additional amount threshold.'
                  )}
                </FormDescription>
                {fieldState.error?.message ? (
                  <p className='text-destructive text-xs'>
                    {t(fieldState.error.message)}
                  </p>
                ) : null}
              </FormItem>
            )}
          />
        </SettingsForm>
      </Form>
    </SettingsSection>
  )
}
