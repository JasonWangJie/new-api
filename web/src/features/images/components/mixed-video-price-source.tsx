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
import { useState } from 'react'
import { useTranslation } from 'react-i18next'

import { ErrorState } from '@/components/error-state'
import { Button } from '@/components/ui/button'
import {
  useCanEditModelPricing,
  useModelPricing,
} from '@/features/model-pricing/api'
import { taskExpressionCompatible } from '@/features/pricing/lib/plugin-pricing'
import type { BillingUsageSchema } from '@/features/pricing/types'
import { getServerErrorMessage } from '@/lib/server-error-message'

import { ImageSelect } from './image-select'

export function MixedVideoPriceSource(props: {
  model: string
  schema: BillingUsageSchema
  onCopy: (expression: string) => void
  disabled?: boolean
}) {
  const { t } = useTranslation()
  const canEdit = useCanEditModelPricing()
  const [requestedModel, setRequestedModel] = useState('')
  const [source, setSource] = useState('__model__')
  const requested = Boolean(props.model) && requestedModel === props.model
  const query = useModelPricing([props.model], requested && !props.disabled)
  const entry = query.data?.entries.find(
    (item) => item.model_name === props.model
  )
  const sources: { value: string; label: string; expression: string }[] = []
  const modelExpression = entry?.effective['billing_setting.billing_expr']
  if (
    entry?.effective['billing_setting.billing_mode'] === 'tiered_expr' &&
    typeof modelExpression === 'string'
  ) {
    sources.push({
      value: '__model__',
      label: t('Default'),
      expression: modelExpression,
    })
  }
  for (const variant of entry?.plugin_variants ?? []) {
    if (variant.stale) continue
    sources.push({
      value: variant.plugin_key,
      label: variant.plugin_name,
      expression: variant.effective,
    })
  }
  const available = sources.filter(
    (item) =>
      item.expression.trim() &&
      taskExpressionCompatible(item.expression, props.schema, {
        usageOnly: true,
      }) === true
  )
  const selected =
    available.find((item) => item.value === source) ?? available[0]

  if (!canEdit) return null

  return (
    <div className='space-y-3'>
      <Button
        type='button'
        variant='outline'
        disabled={props.disabled || !props.model || query.isFetching}
        onClick={() => {
          setRequestedModel(props.model)
          if (requested) void query.refetch()
        }}
      >
        {query.isFetching ? t('Loading...') : t('Load existing prices')}
      </Button>
      {requested && query.isError ? (
        <ErrorState
          className='min-h-0 py-4'
          description={getServerErrorMessage(query.error)}
          onRetry={() => void query.refetch()}
        />
      ) : null}
      {requested && query.isSuccess && selected ? (
        <>
          <div className='flex flex-wrap items-end gap-3'>
            <ImageSelect
              label={t('Price source')}
              value={selected.value}
              options={available}
              onChange={setSource}
              disabled={props.disabled || query.isFetching}
              className='min-w-48 flex-1'
            />
            <Button
              type='button'
              variant='outline'
              disabled={props.disabled || query.isFetching}
              onClick={() => props.onCopy(selected.expression)}
            >
              {t('Copy existing price')}
            </Button>
          </div>
          <p className='text-muted-foreground text-xs'>
            {t(
              'Replaces this draft with the complete expression and its reference video multipliers. Existing multipliers are not added. Save mixed video prices to apply.'
            )}
          </p>
        </>
      ) : null}
      {requested && query.isSuccess && !selected ? (
        <p className='text-muted-foreground text-xs'>
          {t('No existing expression is available for this mixed video model.')}
        </p>
      ) : null}
    </div>
  )
}
