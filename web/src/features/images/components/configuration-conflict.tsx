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

import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { handleServerError } from '@/lib/handle-server-error'

import { isConfigurationConflict } from '../lib/configuration-conflict'

export function ConfigurationConflict(props: {
  error: unknown
  onReload: () => Promise<unknown>
}) {
  const { t } = useTranslation()
  const [loading, setLoading] = useState(false)
  if (!isConfigurationConflict(props.error)) return null
  return (
    <Alert>
      <AlertDescription className='space-y-3'>
        <p>
          {t(
            'Settings changed since you opened this form. Your edits have been kept. Reload saved settings before editing again.'
          )}
        </p>
        <Button
          type='button'
          variant='outline'
          disabled={loading}
          onClick={async () => {
            setLoading(true)
            try {
              await props.onReload()
            } catch (error) {
              handleServerError(error)
            } finally {
              setLoading(false)
            }
          }}
        >
          {t('Reload saved settings')}
        </Button>
      </AlertDescription>
    </Alert>
  )
}
