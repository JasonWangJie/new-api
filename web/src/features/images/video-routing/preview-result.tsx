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
import { useTranslation } from 'react-i18next'

import { StaticDataTable } from '@/components/data-table'
import { Alert, AlertDescription } from '@/components/ui/alert'

import { imageLabel } from '../lib/image-labels'
import type { ConditionalRoutingPreview } from './types'

export function VideoRoutingPreviewResult(props: {
  result: ConditionalRoutingPreview
}) {
  const { t } = useTranslation()
  return (
    <div className='space-y-3 rounded-md border p-3'>
      <p className='font-medium'>
        {t('Conditional video routing')}:{' '}
        {props.result.rule_name || t('No matching rule')}
      </p>
      <p className='text-sm'>
        {t('Original request and billing')}: {props.result.source.model} ·{' '}
        {props.result.source.resolution || '—'} ·{' '}
        {props.result.source.seconds ?? '—'} s
      </p>
      {props.result.matched_rules?.length > 1 && (
        <Alert>
          <AlertDescription>
            {t('Several rules match. Only the first enabled rule is used.')}{' '}
            {props.result.matched_rules.map((rule) => rule.name).join(' → ')}
          </AlertDescription>
        </Alert>
      )}
      {!!props.result.targets?.length && (
        <StaticDataTable
          data={props.result.targets}
          getRowKey={(target) =>
            `${target.channel_id}:${target.model}:${target.resolution}`
          }
          columns={[
            {
              id: 'channel',
              header: t('Channel'),
              cell: (target) => `#${target.channel_id}`,
            },
            {
              id: 'model',
              header: t('Target model'),
              cell: (target) => target.model,
            },
            {
              id: 'upstream',
              header: t('Upstream model'),
              cell: (target) => target.upstream_model || '—',
            },
            {
              id: 'resolution',
              header: t('Output resolution'),
              cell: (target) =>
                target.resolution ||
                props.result.source.resolution ||
                t('Keep requested resolution'),
            },
            {
              id: 'status',
              header: t('Status'),
              cell: (target) => {
                if (
                  props.result.targets.indexOf(target) ===
                  props.result.selected_target_index
                ) {
                  return t('Selected')
                }
                return target.eligible
                  ? t('Eligible by configuration')
                  : t('Excluded')
              },
            },
            {
              id: 'reasons',
              header: t('Reason'),
              cell: (target) =>
                target.reasons
                  .map((reason) => t(imageLabel(reason)))
                  .join(' · ') || '—',
            },
          ]}
        />
      )}
      {props.result.rule_id && (
        <p className='text-sm'>
          {t('When targets are exhausted')}:{' '}
          {props.result.on_exhausted === 'original'
            ? t('Return to original routing')
            : t('Fail the task')}
          {props.result.is_fallback ? ` · ${t('Fallback channel')}` : ''}
        </p>
      )}
    </div>
  )
}
