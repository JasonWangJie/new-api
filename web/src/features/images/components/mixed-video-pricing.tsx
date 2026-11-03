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
import { useEffect, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'

import { JsonCodeEditor } from '@/components/json-code-editor'
import { Button } from '@/components/ui/button'
import { Label } from '@/components/ui/label'
import { compileBillingExpression } from '@/features/pricing/lib/billing-expression/parser'
import { visitExpression } from '@/features/pricing/lib/billing-expression/types'
import { taskExpressionCompatible } from '@/features/pricing/lib/plugin-pricing'
import type { BillingUsageSchema } from '@/features/pricing/types'
import { SettingsCard } from '@/features/system-settings/components/settings-card'
import { TaskUsagePricingEditor } from '@/features/system-settings/models/task-usage-pricing-editor'
import { handleServerError } from '@/lib/handle-server-error'

import { imageRequest } from '../api'
import { isVideoModelEntry } from '../lib/image-labels'
import type { ImageAdminConfiguration } from '../types'
import { ImageSelect } from './image-select'
import { MixedVideoPriceSource } from './mixed-video-price-source'

function parseMixedVideoPrices(value: string): Record<string, string> | null {
  try {
    const parsed: unknown = JSON.parse(value)
    if (
      parsed &&
      typeof parsed === 'object' &&
      !Array.isArray(parsed) &&
      Object.values(parsed).every(
        (expression) => typeof expression === 'string'
      )
    ) {
      return parsed as Record<string, string>
    }
  } catch {
    // Invalid bulk JSON stays editable without replacing the last draft.
  }
  return null
}

function MixedVideoModelPricing(props: {
  expression: string
  schema: BillingUsageSchema
  onChange: (expression: string) => void
}) {
  // Keep the editor's dimensions stable while typing. Explicit reloads remount
  // it after selecting a model, copying a price, or editing the complete map.
  const [schema] = useState(() => {
    const used = new Set<string>()
    const compiled = compileBillingExpression(props.expression)
    if (compiled.status === 'ready') {
      visitExpression(compiled.ast, (node) => {
        if (node.kind !== 'call' || node.name !== 'u') return
        const key = node.args[0]
        if (key?.kind === 'literal' && typeof key.value === 'string') {
          used.add(key.value)
        }
      })
    }
    if (!used.has('requests') && !used.has('seconds')) used.add('requests')
    return Object.fromEntries(
      Object.entries(props.schema).filter(
        ([key]) => used.has(key) || key === 'video_input'
      )
    )
  })
  return (
    <TaskUsagePricingEditor
      billingExpr={props.expression}
      usageSchema={schema}
      allowRequestRules={false}
      requestRuleExpr=''
      onBillingExprChange={props.onChange}
      onRequestRuleExprChange={() => {}}
    />
  )
}

export function MixedVideoPricing(props: {
  config: ImageAdminConfiguration
  catalog: string
  onSuccess: () => Promise<unknown>
}) {
  const { t } = useTranslation()
  const [pricing, setPricing] = useState(() =>
    JSON.stringify(props.config.mixed_video_billing_expr || {}, null, 2)
  )
  const dirty = useRef(false)
  const [busy, setBusy] = useState(false)
  const [selectedModel, setSelectedModel] = useState('')
  const [showJson, setShowJson] = useState(false)
  const [revision, setRevision] = useState(0)
  const schema = props.config.mixed_video_usage_schema
  const prices = parseMixedVideoPrices(pricing)
  const names = new Set(Object.keys(prices ?? {}))
  try {
    const catalog: unknown = JSON.parse(props.catalog)
    if (Array.isArray(catalog)) {
      for (const entry of catalog) {
        if (isVideoModelEntry(entry) && typeof entry.id === 'string') {
          names.add(entry.id)
        }
      }
    }
  } catch {
    // A policy draft error must not discard independently configured prices.
  }
  const modelNames = [...names]
  const model = names.has(selectedModel) ? selectedModel : modelNames[0] || ''
  const visualAvailable = Boolean(schema && prices && model)
  const overridden = prices != null && Object.hasOwn(prices, model)
  const inheritedExpression =
    props.config.mixed_video_model_billing_expr?.[model]
  const inheritedCompatible = Boolean(
    inheritedExpression &&
    schema &&
    taskExpressionCompatible(inheritedExpression, schema, {
      usageOnly: true,
    }) === true
  )

  useEffect(() => {
    if (dirty.current) return
    setPricing(
      JSON.stringify(props.config.mixed_video_billing_expr || {}, null, 2)
    )
    setRevision((value) => value + 1)
  }, [props.config.mixed_video_billing_expr])

  const changePrice = (expression: string) => {
    if (!prices || !model) return
    dirty.current = true
    setPricing(JSON.stringify({ ...prices, [model]: expression }, null, 2))
  }

  const save = async () => {
    if (!prices) {
      toast.error(
        t(
          'Mixed video prices must map public model names to billing expressions.'
        )
      )
      return
    }
    setBusy(true)
    try {
      await imageRequest('/api/option/', 'PUT', {
        key: 'billing_setting.mixed_video_billing_expr',
        value: JSON.stringify(prices),
      })
      await props.onSuccess()
      dirty.current = false
      toast.success(t('Saved'))
    } catch (error) {
      handleServerError(error)
    } finally {
      setBusy(false)
    }
  }

  return (
    <SettingsCard
      title={t('Mixed video pricing')}
      description={t(
        'Mixed video uses model pricing by default. Set a custom mixed video price only when needed.'
      )}
    >
      <form
        className='space-y-4'
        onSubmit={(event) => {
          event.preventDefault()
          void save()
        }}
      >
        <fieldset disabled={busy} className='min-w-0 space-y-4'>
          {visualAvailable && !showJson && schema && prices ? (
            <>
              <ImageSelect
                label={t('Public video model')}
                value={model}
                options={modelNames.map((value) => ({ value, label: value }))}
                onChange={setSelectedModel}
              />
              <ImageSelect
                label={t('Mixed video price source')}
                value={overridden ? 'mixed' : 'model'}
                options={[
                  { value: 'model', label: t('Use model pricing') },
                  { value: 'mixed', label: t('Custom mixed video price') },
                ]}
                onChange={(source) => {
                  const next = { ...prices }
                  if (source === 'model') delete next[model]
                  else next[model] = inheritedExpression || ''
                  dirty.current = true
                  setPricing(JSON.stringify(next, null, 2))
                  setRevision((value) => value + 1)
                }}
              />
              {!overridden && (
                <div className='space-y-3'>
                  <p className='text-muted-foreground text-sm'>
                    {t(
                      'New tasks follow the model price automatically. Accepted tasks keep their original price.'
                    )}
                  </p>
                  {!inheritedCompatible && (
                    <p className='text-warning text-sm'>
                      {t(
                        'Configure a compatible model billing expression or set a custom mixed video price.'
                      )}
                    </p>
                  )}
                  <Button
                    type='button'
                    variant='outline'
                    role='link'
                    render={<a href='/system-settings/billing/model-pricing' />}
                  >
                    {t('Open model pricing')}
                  </Button>
                </div>
              )}
              <MixedVideoPriceSource
                key={model}
                model={model}
                schema={schema}
                disabled={busy}
                onCopy={(expression) => {
                  changePrice(expression)
                  setRevision((value) => value + 1)
                }}
              />
              {(overridden || inheritedExpression) && (
                <fieldset disabled={!overridden} className='min-w-0'>
                  <MixedVideoModelPricing
                    key={`${model}:${revision}:${overridden ? 'mixed' : inheritedExpression}`}
                    expression={
                      overridden ? prices[model] : inheritedExpression || ''
                    }
                    schema={schema}
                    onChange={(expression) => {
                      if (overridden) changePrice(expression)
                    }}
                  />
                </fieldset>
              )}
            </>
          ) : (
            <div className='space-y-2'>
              <Label htmlFor='mixed-video-prices'>
                {t('Public video model billing expressions')}
              </Label>
              <p className='text-muted-foreground text-sm'>
                {t(
                  'Only custom mixed video prices are listed. Remove a model to use model pricing.'
                )}
              </p>
              <JsonCodeEditor
                id='mixed-video-prices'
                ariaLabel={t('Public video model billing expressions')}
                value={pricing}
                onChange={(value) => {
                  dirty.current = true
                  setPricing(value)
                }}
                disabled={busy}
              />
            </div>
          )}
          <div className='flex flex-wrap gap-2'>
            {schema && (
              <Button
                type='button'
                variant='outline'
                disabled={!visualAvailable}
                onClick={() => {
                  setShowJson(!showJson)
                  setRevision((value) => value + 1)
                }}
              >
                {showJson
                  ? t('Visual editor')
                  : t('Public video model billing expressions')}
              </Button>
            )}
            <Button type='submit' disabled={busy}>
              {t('Save mixed video prices')}
            </Button>
          </div>
        </fieldset>
      </form>
    </SettingsCard>
  )
}
