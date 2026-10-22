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
import { zodResolver } from '@hookform/resolvers/zod'
import { useTranslation } from 'react-i18next'
import { z } from 'zod'

import {
  Form,
  FormControl,
  FormDescription,
  FormField,
  FormItem,
  FormLabel,
  FormMessage,
} from '@/components/ui/form'
import { Input } from '@/components/ui/input'
import { Switch } from '@/components/ui/switch'
import { canvasOrigin } from '@/features/infinite-canvas/config'

import { FormDirtyIndicator } from '../components/form-dirty-indicator'
import { FormNavigationGuard } from '../components/form-navigation-guard'
import {
  SettingsForm,
  SettingsSwitchContent,
  SettingsSwitchItem,
} from '../components/settings-form-layout'
import { SettingsPageFormActions } from '../components/settings-page-context'
import { SettingsSection } from '../components/settings-section'
import { useSettingsForm } from '../hooks/use-settings-form'
import { useUpdateOption } from '../hooks/use-update-option'
import type { SiteSettings } from '../types'

export function InfiniteCanvasSection(props: { settings: SiteSettings }) {
  const { t } = useTranslation()
  const updateOption = useUpdateOption()
  const schema = z
    .object({
      infinite_canvas: z.object({
        enabled: z.boolean(),
        url: z
          .string()
          .refine(
            (value) => value === '' || canvasOrigin(value) !== null,
            t(
              'Enter an HTTPS origin without a path, credentials, query, or fragment'
            )
          ),
        client_id: z.string().min(1, t('Canvas application ID is required')),
      }),
    })
    .refine(
      (value) =>
        !value.infinite_canvas.enabled || value.infinite_canvas.url !== '',
      {
        path: ['infinite_canvas', 'url'],
        message: t('Canvas site URL is required'),
      }
    )
  const { form, handleSubmit, handleReset, isDirty, isSubmitting } =
    useSettingsForm<z.infer<typeof schema>>({
      resolver: zodResolver(schema),
      defaultValues: {
        infinite_canvas: {
          enabled: props.settings['infinite_canvas.enabled'],
          url: props.settings['infinite_canvas.url'],
          client_id: props.settings['infinite_canvas.client_id'],
        },
      },
      onSubmit: async (_values, changedFields) => {
        if (changedFields['infinite_canvas.enabled'] === false) {
          await updateOption.mutateAsync({
            key: 'infinite_canvas.enabled',
            value: false,
          })
        }
        for (const key of [
          'infinite_canvas.url',
          'infinite_canvas.client_id',
        ]) {
          if (typeof changedFields[key] === 'string') {
            await updateOption.mutateAsync({
              key,
              value:
                key === 'infinite_canvas.url'
                  ? (canvasOrigin(String(changedFields[key])) ?? '')
                  : String(changedFields[key]).trim(),
            })
          }
        }
        if (changedFields['infinite_canvas.enabled'] === true) {
          await updateOption.mutateAsync({
            key: 'infinite_canvas.enabled',
            value: true,
          })
        }
      },
    })
  return (
    <>
      <FormNavigationGuard when={isDirty} />
      <SettingsSection title={t('AI Infinite Canvas')}>
        <Form {...form}>
          <SettingsForm onSubmit={handleSubmit}>
            <SettingsPageFormActions
              onSave={handleSubmit}
              onReset={handleReset}
              isSaving={isSubmitting}
              isResetDisabled={!isDirty}
            />
            <FormDirtyIndicator isDirty={isDirty} />
            <FormField
              control={form.control}
              name='infinite_canvas.enabled'
              render={({ field }) => (
                <SettingsSwitchItem>
                  <SettingsSwitchContent>
                    <FormLabel>{t('Enable AI Infinite Canvas')}</FormLabel>
                    <FormDescription>
                      {t(
                        'Open the independent canvas with your current New API account'
                      )}
                    </FormDescription>
                  </SettingsSwitchContent>
                  <FormControl>
                    <Switch
                      checked={field.value}
                      onCheckedChange={field.onChange}
                    />
                  </FormControl>
                  <FormMessage />
                </SettingsSwitchItem>
              )}
            />
            <FormField
              control={form.control}
              name='infinite_canvas.url'
              render={({ field }) => (
                <FormItem>
                  <FormLabel>{t('Canvas site URL')}</FormLabel>
                  <FormControl>
                    <Input
                      placeholder='https://canvas.example.com'
                      {...field}
                    />
                  </FormControl>
                  <FormDescription>
                    {t('Use the independently deployed canvas origin')}
                  </FormDescription>
                  <FormMessage />
                </FormItem>
              )}
            />
            <FormField
              control={form.control}
              name='infinite_canvas.client_id'
              render={({ field }) => (
                <FormItem>
                  <FormLabel>{t('Canvas application ID')}</FormLabel>
                  <FormControl>
                    <Input {...field} />
                  </FormControl>
                  <FormDescription>
                    {t('Must match the canvas server application ID')}
                  </FormDescription>
                  <FormMessage />
                </FormItem>
              )}
            />
          </SettingsForm>
        </Form>
      </SettingsSection>
    </>
  )
}
