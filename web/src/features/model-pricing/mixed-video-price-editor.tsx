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
import { useMemo, useState } from 'react'
import { useTranslation } from 'react-i18next'

import { Button } from '@/components/ui/button'
import { MixedVideoPriceSource } from '@/features/images/components/mixed-video-price-source'
import { DynamicPricingBreakdown } from '@/features/pricing/components/dynamic-pricing-breakdown'
import { compileBillingExpression } from '@/features/pricing/lib/billing-expression/parser'
import { visitExpression } from '@/features/pricing/lib/billing-expression/types'
import { taskExpressionCompatible } from '@/features/pricing/lib/plugin-pricing'
import { TaskUsagePricingEditor } from '@/features/system-settings/models/task-usage-pricing-editor'

import type { ModelPricingEntry } from './api'

export function MixedVideoPriceEditor(props: {
  entry: ModelPricingEntry
  mixed: NonNullable<ModelPricingEntry['mixed_video']>
  disabled: boolean
  onDirtyChange: (dirty: boolean) => void
  onSave: (expression: string | undefined) => Promise<void>
}) {
  const { t } = useTranslation()
  const mixed = props.mixed
  const configured =
    props.entry.configured['billing_setting.mixed_video_billing_expr']
  const [override, setOverride] = useState(typeof configured === 'string')
  const [expression, setExpression] = useState(
    configured ?? mixed.expression ?? ''
  )
  const [revision, setRevision] = useState(0)
  const [schemaExpression, setSchemaExpression] = useState(expression)
  const schema = useMemo(() => {
    const used = new Set<string>()
    const compiled = compileBillingExpression(schemaExpression)
    if (compiled.status === 'ready') {
      visitExpression(compiled.ast, (node) => {
        if (
          node.kind === 'call' &&
          node.name === 'u' &&
          node.args[0]?.kind === 'literal' &&
          typeof node.args[0].value === 'string'
        ) {
          used.add(node.args[0].value)
        }
      })
    }
    if (!used.has('requests') && !used.has('seconds')) used.add('requests')
    return Object.fromEntries(
      Object.entries(mixed.usage_schema).filter(
        ([key]) => used.has(key) || key === 'video_input'
      )
    )
  }, [mixed.usage_schema, schemaExpression])
  const inherited =
    props.entry.effective['billing_setting.billing_mode'] === 'tiered_expr' &&
    typeof props.entry.effective['billing_setting.billing_expr'] === 'string'
      ? props.entry.effective['billing_setting.billing_expr']
      : ''
  const inheritedValid = Boolean(
    inherited &&
    taskExpressionCompatible(inherited, mixed.usage_schema, {
      usageOnly: true,
    }) === true &&
    (mixed.source !== 'model' || mixed.valid)
  )
  const dirty =
    override !== (typeof configured === 'string') ||
    (override && expression !== configured)
  const change = (nextOverride: boolean, nextExpression: string) => {
    setOverride(nextOverride)
    setExpression(nextExpression)
    props.onDirtyChange(
      nextOverride !== (typeof configured === 'string') ||
        (nextOverride && nextExpression !== configured)
    )
  }
  let sourceLabel = t('Using model pricing')
  if (override) sourceLabel = t('Custom mixed video price')
  else if (!inherited) {
    sourceLabel = t('No mixed video price configured')
  }
  return (
    <section
      aria-label={t('Mixed video public price')}
      className='border-primary/40 bg-primary/5 space-y-3 rounded-lg border p-4'
    >
      <h3 className='font-semibold'>{t('Mixed video public price')}</h3>
      <p className='text-muted-foreground text-sm'>
        {t(
          'This price applies to mixed video requests and is shown in the model marketplace.'
        )}
      </p>
      <p className='text-sm'>{sourceLabel}</p>
      {(override
        ? !mixed.valid && !dirty
        : Boolean(inherited) && !inheritedValid) && (
        <p role='alert' className='text-destructive text-sm'>
          {t(
            'The current mixed video price is not compatible with mixed video billing.'
          )}
        </p>
      )}
      <div className='flex flex-wrap gap-2'>
        <Button
          type='button'
          variant={override ? 'outline' : 'secondary'}
          disabled={props.disabled}
          onClick={() => change(false, expression)}
        >
          {t('Use model pricing')}
        </Button>
        <Button
          type='button'
          variant={override ? 'secondary' : 'outline'}
          disabled={props.disabled}
          onClick={() => change(true, expression)}
        >
          {t('Custom mixed video price')}
        </Button>
      </div>
      <MixedVideoPriceSource
        model={props.entry.model_name}
        schema={mixed.usage_schema}
        disabled={props.disabled}
        onCopy={(next) => {
          change(true, next)
          setSchemaExpression(next)
          setRevision((value) => value + 1)
        }}
      />
      {override && !expression.trim() && (
        <div className='flex flex-wrap gap-2'>
          <Button
            type='button'
            variant='outline'
            disabled={props.disabled}
            onClick={() => {
              const next = 'tier("request", u("requests") * 0)'
              change(true, next)
              setSchemaExpression(next)
              setRevision((value) => value + 1)
            }}
          >
            {t('Start with per-request pricing')}
          </Button>
          <Button
            type='button'
            variant='outline'
            disabled={props.disabled}
            onClick={() => {
              const next = 'tier("seconds", u("seconds") * 0)'
              change(true, next)
              setSchemaExpression(next)
              setRevision((value) => value + 1)
            }}
          >
            {t('Start with per-second pricing')}
          </Button>
        </div>
      )}
      {override && expression.trim() !== '' && (
        <fieldset disabled={props.disabled} className='min-w-0'>
          <TaskUsagePricingEditor
            key={revision}
            billingExpr={expression}
            usageSchema={schema}
            allowRequestRules={false}
            requestRuleExpr=''
            onBillingExprChange={(next) => change(true, next)}
            onRequestRuleExprChange={() => {}}
          />
        </fieldset>
      )}
      {!override && inheritedValid && (
        <DynamicPricingBreakdown
          compact
          publishedPricesOnly
          billingExpr={inherited}
          usageSchema={mixed.usage_schema}
        />
      )}
      <Button
        type='button'
        disabled={props.disabled || !dirty || (override && !expression.trim())}
        onClick={() => void props.onSave(override ? expression : undefined)}
      >
        {t('Save mixed video price')}
      </Button>
    </section>
  )
}
