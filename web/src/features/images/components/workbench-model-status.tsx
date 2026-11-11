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

import { ErrorState } from '@/components/error-state'
import { LoadingState } from '@/components/loading-state'

export function WorkbenchModelStatus(props: {
  loading: boolean
  error: Error | null
  onRetry: () => void
}) {
  const { t } = useTranslation()
  if (props.loading) {
    return (
      <div role='status'>
        <LoadingState inline size='sm' message={t('Loading...')} />
      </div>
    )
  }
  if (!props.error) return null
  return (
    <div role='alert'>
      <ErrorState
        title={t('Failed to load models')}
        description={props.error.message}
        onRetry={props.onRetry}
        className='min-h-0 p-3'
      />
    </div>
  )
}
