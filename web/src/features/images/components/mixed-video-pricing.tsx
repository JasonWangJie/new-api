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

import { ModelPricingPanel } from '@/features/model-pricing/model-pricing-panel'
import { SettingsCard } from '@/features/system-settings/components/settings-card'

import { isVideoModelEntry } from '../lib/image-labels'
import type { ImageAdminConfiguration } from '../types'
import { ImageSelect } from './image-select'

export function MixedVideoPricing(props: {
  config: ImageAdminConfiguration
  catalog: string
  onSuccess: () => Promise<unknown>
}) {
  const { t } = useTranslation()
  const [selectedModel, setSelectedModel] = useState('')
  const [dirty, setDirty] = useState(false)
  const names = new Set(
    Object.keys(props.config.mixed_video_billing_expr || {})
  )
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
    // Keep existing configured models available while a policy draft is invalid.
  }
  const modelNames = [...names].sort()
  const model = names.has(selectedModel) ? selectedModel : modelNames[0] || ''
  return (
    <SettingsCard
      title={t('Mixed video pricing')}
      description={t(
        'Select a model to edit its public and provider prices together.'
      )}
    >
      {model ? (
        <div className='space-y-4'>
          <ImageSelect
            label={t('Public video model')}
            value={model}
            options={modelNames.map((value) => ({ value, label: value }))}
            onChange={setSelectedModel}
            disabled={dirty}
          />
          <ModelPricingPanel
            key={model}
            modelName={model}
            onDirtyChange={setDirty}
          />
        </div>
      ) : (
        <p className='text-muted-foreground text-sm'>
          {t('No mixed video model is configured.')}
        </p>
      )}
    </SettingsCard>
  )
}
