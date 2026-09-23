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

import { CopyButton } from '@/components/copy-button'
import { Button } from '@/components/ui/button'
import { ArtifactMedia } from '@/features/usage-logs/components/task-artifacts'

import type { ImageResult } from '../types'

export function VideoResults(props: {
  results: ImageResult[]
  onRefresh: () => void
}) {
  const { t } = useTranslation()
  return (
    <div className='space-y-6'>
      {props.results.map((result) => (
        <div key={result.id} className='space-y-3'>
          <ArtifactMedia
            artifact={{
              key: String(result.id),
              type: 'video',
              content_url: result.url || result.view_url,
            }}
            mediaUrl={result.url || result.view_url}
            onError={props.onRefresh}
          />
          <div className='flex flex-wrap items-center gap-2'>
            <Button
              variant='outline'
              size='sm'
              nativeButton={false}
              render={<a href={result.url || result.view_url} download />}
            >
              {t('Download')}
            </Button>
            <CopyButton value={result.url || result.view_url}>
              {t('Copy link')}
            </CopyButton>
            {result.source !== 'upstream' && (
              <Button variant='ghost' size='sm' onClick={props.onRefresh}>
                {t('Refresh link')}
              </Button>
            )}
            <span className='text-muted-foreground text-xs'>
              {result.source === 'upstream'
                ? t('Upstream link')
                : `${(result.byte_size / 1048576).toFixed(2)} MiB`}
            </span>
          </div>
        </div>
      ))}
    </div>
  )
}
