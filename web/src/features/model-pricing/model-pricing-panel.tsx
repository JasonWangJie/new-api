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
import { useEffect, useMemo, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'

import { ConfirmDialog } from '@/components/confirm-dialog'
import { ErrorState } from '@/components/error-state'
import { LoadingState } from '@/components/loading-state'
import { Button } from '@/components/ui/button'
import {
  Collapsible,
  CollapsibleContent,
  CollapsibleTrigger,
} from '@/components/ui/collapsible'
import { DynamicPricingBreakdown } from '@/features/pricing/components/dynamic-pricing-breakdown'
import { ModelPriceCell } from '@/features/pricing/components/model-price-cell'
import { isDynamicPricingModel } from '@/features/pricing/lib/dynamic-price'
import {
  buildPreviewRows,
  createInitialLaneState,
} from '@/features/system-settings/models/model-pricing-core'
import {
  ModelPricingEditorPanel,
  type ModelPricingEditorPanelHandle,
} from '@/features/system-settings/models/model-pricing-sheet'
import { handleServerError } from '@/lib/handle-server-error'
import { cn } from '@/lib/utils'
import { usePricingPreferencesStore } from '@/stores/pricing-preferences-store'
import { useSystemConfigStore } from '@/stores/system-config-store'

import {
  useCanEditModelPricing,
  useModelPricing,
  useSaveModelPricing,
  type ModelPricingEntry,
} from './api'
import {
  getSitePricingCurrency,
  isValidPricingCurrency,
  USD_PRICING_CURRENCY,
} from './currency'
import { MixedVideoPriceEditor } from './mixed-video-price-editor'
import { modelPricingDisplay, pricingFromDraft, pricingRow } from './pricing'

export function ModelPricingPanel(props: {
  modelName: string
  onDirtyChange?: (dirty: boolean) => void
}) {
  const { t } = useTranslation()
  const currencyConfig = useSystemConfigStore((state) => state.config.currency)
  const currencyPreference = usePricingPreferencesStore(
    (state) => state.currency
  )
  const canEdit = useCanEditModelPricing()
  const query = useModelPricing([props.modelName], Boolean(props.modelName))
  const save = useSaveModelPricing()
  const [entry, setEntry] = useState<ModelPricingEntry | null>(null)
  const [resetOpen, setResetOpen] = useState(false)
  const [reloadOpen, setReloadOpen] = useState(false)
  const [editorRevision, setEditorRevision] = useState(0)
  const [mixedDirty, setMixedDirty] = useState(false)
  const [baseDirty, setBaseDirty] = useState(false)
  const onDirtyChange = props.onDirtyChange
  const editor = useRef<ModelPricingEditorPanelHandle>(null)
  const editData = useMemo(() => {
    if (!entry) return null
    const values = { ...entry.configured }
    if (entry.effective['billing_setting.billing_mode'] === 'tiered_expr') {
      values['billing_setting.billing_mode'] = 'tiered_expr'
      values['billing_setting.billing_expr'] =
        entry.effective['billing_setting.billing_expr']
    }
    return pricingRow(entry.model_name, values)
  }, [entry])

  useEffect(() => {
    const loaded = query.data?.entries.find(
      (item) => item.model_name === props.modelName
    )
    if (loaded && (!entry || entry.model_name !== props.modelName)) {
      setEntry(loaded)
    }
  }, [query.data, entry, props.modelName])

  useEffect(() => {
    onDirtyChange?.(mixedDirty || baseDirty)
  }, [mixedDirty, baseDirty, onDirtyChange])

  const saveMixed = async (expression: string | undefined) => {
    if (!entry) return
    const pricing = { ...entry.configured }
    delete pricing['billing_setting.mixed_video_billing_expr']
    if (expression !== undefined) {
      pricing['billing_setting.mixed_video_billing_expr'] = expression
    }
    try {
      await save.mutateAsync([
        {
          model_name: entry.model_name,
          expected_version: entry.version,
          pricing,
        },
      ])
      const refreshed = await query.refetch({ throwOnError: true })
      setEntry(
        refreshed.data?.entries.find(
          (item) => item.model_name === props.modelName
        ) ?? null
      )
      setMixedDirty(false)
      toast.success(t('Model pricing saved'))
    } catch (error) {
      handleServerError(error)
    }
  }

  const persist = async (reset = false) => {
    if (!entry) return
    try {
      const draft = reset ? null : await editor.current?.commitDraft()
      if (!reset && !draft) return
      const mixed = entry.configured['billing_setting.mixed_video_billing_expr']
      const pricing = draft ? pricingFromDraft(draft) : {}
      if (mixed !== undefined) {
        pricing['billing_setting.mixed_video_billing_expr'] = mixed
      }
      await save.mutateAsync([
        {
          model_name: entry.model_name,
          expected_version: entry.version,
          pricing,
          reset: reset && mixed === undefined,
        },
      ])
      const refreshed = await query.refetch({ throwOnError: true })
      setEntry(
        refreshed.data?.entries.find(
          (item) => item.model_name === props.modelName
        ) ?? null
      )
      setResetOpen(false)
      setBaseDirty(false)
      toast.success(t('Model pricing saved'))
    } catch (error) {
      handleServerError(error)
    }
  }

  if (!canEdit) {
    return (
      <div className='text-muted-foreground p-6 text-sm'>
        {t('Model pricing is managed by a super administrator.')}
      </div>
    )
  }
  if (query.isError) {
    return (
      <ErrorState
        description={query.error.message}
        onRetry={() => void query.refetch()}
      />
    )
  }
  if (!editData || !entry) return <LoadingState />
  const effectivePricing = modelPricingDisplay(entry)
  const siteCurrency = getSitePricingCurrency(currencyConfig)
  const currency =
    currencyPreference === 'site' && isValidPricingCurrency(siteCurrency)
      ? siteCurrency
      : USD_PRICING_CURRENCY
  const current = pricingRow(entry.model_name, entry.effective)
  const currentLanes = createInitialLaneState(current)
  const details = buildPreviewRows(
    current,
    current.billingMode ?? 'per-token',
    '',
    '',
    currentLanes.promptPrice,
    currentLanes.prices,
    currentLanes.enabled,
    t,
    currency,
    entry.cache_write_mode,
    entry.billing_details
  ).filter(
    (row) =>
      row.key !== 'inputPrice' &&
      row.key !== 'completion' &&
      row.key !== 'price' &&
      row.value !== t('Empty')
  )
  const showMixed = Boolean(
    entry.mixed_video?.eligible ||
    entry.configured['billing_setting.mixed_video_billing_expr'] !== undefined
  )

  const reload = async () => {
    try {
      const refreshed = await query.refetch({ throwOnError: true })
      const loaded = refreshed.data?.entries.find(
        (item) => item.model_name === props.modelName
      )
      if (!loaded) return
      setEntry(loaded)
      setMixedDirty(false)
      setBaseDirty(false)
      setEditorRevision((value) => value + 1)
      setReloadOpen(false)
      save.reset()
    } catch (error) {
      handleServerError(error)
    }
  }

  const baseEditor = (
    <ModelPricingEditorPanel
      key={`${entry.model_name}:${editorRevision}`}
      embedded
      ref={editor}
      editData={editData}
      usageSchema={entry.usage_schema}
      pluginVariants={entry.plugin_variants}
      onDirtyChange={setBaseDirty}
      onSave={() => persist()}
      isSaving={save.isPending || mixedDirty}
      className='rounded-none border-0'
      scrollHeader={
        <>
          <div className='flex flex-wrap items-center justify-between gap-2'>
            <div className='min-w-0 flex-1 break-words'>
              <p className='text-muted-foreground text-xs'>
                {Object.keys(entry.configured).length
                  ? t('Stored configuration with effective defaults')
                  : t('Using built-in or default pricing')}
              </p>
            </div>
            <Button
              variant='outline'
              size='sm'
              onClick={() => setResetOpen(true)}
              disabled={save.isPending || mixedDirty}
            >
              {t('Restore default pricing')}
            </Button>
          </div>
          <section
            aria-label={t('Current Billing')}
            className='space-y-3 border-b pb-3'
          >
            <h3 className='text-muted-foreground text-xs'>
              {t('Current Billing')}
            </h3>
            <div className='max-w-xs'>
              <ModelPriceCell
                model={effectivePricing}
                options={{ tokenUnit: 'M' }}
                showExpression={false}
              />
            </div>
            {isDynamicPricingModel(effectivePricing) ? (
              <DynamicPricingBreakdown
                publishedPricesOnly
                compact
                billingExpr={effectivePricing.billing_expr}
                usageSchema={entry.usage_schema}
              />
            ) : (
              effectivePricing.quota_type === 0 &&
              Number.isFinite(effectivePricing.model_ratio) && (
                <dl className='grid grid-cols-2 gap-x-4 gap-y-2 text-xs sm:grid-cols-3'>
                  {details.map((row) => (
                    <div key={row.key}>
                      <dt className='text-muted-foreground'>{row.label}</dt>
                      <dd className='mt-1 font-mono tabular-nums'>
                        {row.value}
                        {row.unit !== 'none' && ' / 1M'}
                      </dd>
                    </div>
                  ))}
                </dl>
              )
            )}
          </section>
        </>
      }
    />
  )
  return (
    <div
      className={cn(
        'flex min-h-0 min-w-0 flex-1 flex-col gap-3',
        showMixed && 'overflow-y-auto overscroll-contain'
      )}
    >
      {(save.isError || mixedDirty || baseDirty) && (
        <div className='space-y-2'>
          {save.isError && (
            <p role='alert' className='text-destructive text-sm'>
              {save.error?.message}
            </p>
          )}
          <Button
            variant='outline'
            size='sm'
            disabled={save.isPending}
            onClick={() => {
              if (mixedDirty || baseDirty) setReloadOpen(true)
              else void reload()
            }}
          >
            {t('Reload pricing')}
          </Button>
        </div>
      )}
      {showMixed && entry.mixed_video && (
        <MixedVideoPriceEditor
          key={`${entry.model_name}:${entry.version}:${editorRevision}`}
          entry={entry}
          mixed={entry.mixed_video}
          disabled={save.isPending || baseDirty}
          onDirtyChange={setMixedDirty}
          onSave={saveMixed}
        />
      )}
      {showMixed ? (
        <Collapsible>
          <CollapsibleTrigger className='text-sm font-medium underline'>
            {t('Advanced model and provider pricing')}
          </CollapsibleTrigger>
          <CollapsibleContent>
            <p className='text-muted-foreground py-2 text-sm'>
              {t(
                'Model pricing applies outside mixed routing and is inherited by providers without their own price.'
              )}
            </p>
            {baseEditor}
          </CollapsibleContent>
        </Collapsible>
      ) : (
        baseEditor
      )}
      <ConfirmDialog
        open={reloadOpen}
        onOpenChange={setReloadOpen}
        title={t('Reload pricing')}
        desc={t(
          'Discard unsaved pricing changes and reload the current price?'
        )}
        confirmText={t('Reload pricing')}
        isLoading={query.isFetching}
        handleConfirm={() => void reload()}
      />
      <ConfirmDialog
        open={resetOpen}
        onOpenChange={setResetOpen}
        title={t('Restore default pricing')}
        desc={t(
          'Remove this model’s custom pricing and use the built-in defaults. A model without a default may become unpriced.'
        )}
        confirmText={t('Restore defaults')}
        isLoading={save.isPending}
        handleConfirm={() => void persist(true)}
      />
    </div>
  )
}
