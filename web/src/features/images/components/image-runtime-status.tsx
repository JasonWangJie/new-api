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
import { useQuery } from '@tanstack/react-query'
import { useTranslation } from 'react-i18next'

import { Alert, AlertDescription } from '@/components/ui/alert'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table'
import { SettingsCard } from '@/features/system-settings/components/settings-card'

import { imageRequest } from '../api'

type RuntimeStatus = {
  node: string
  configured: { worker_concurrency: number; recovery_interval_seconds: number }
  effective: {
    running: boolean
    worker_concurrency: number
    active_workers: number
    recovery_interval_seconds: number
  }
}

export function ImageRuntimeStatus() {
  const { t } = useTranslation()
  const status = useQuery({
    queryKey: ['image-runtime-status'],
    queryFn: ({ signal }) =>
      imageRequest<RuntimeStatus>(
        '/api/option/images/runtime-status',
        'GET',
        undefined,
        signal
      ),
    refetchInterval: 5000,
    meta: { errorToast: false },
  })
  const current = status.data
  const draining = current
    ? Math.max(
        0,
        current.effective.active_workers - current.effective.worker_concurrency
      )
    : 0
  const pending =
    current &&
    current.effective.running &&
    (current.configured.worker_concurrency !==
      current.effective.worker_concurrency ||
      current.configured.recovery_interval_seconds !==
        current.effective.recovery_interval_seconds ||
      draining > 0)
  return (
    <SettingsCard
      title={t('Image worker status')}
      description={t(
        'Worker counts and recovery intervals refresh automatically. Reducing workers lets current tasks finish. Other settings apply when the next task step reads them; frozen task settings stay unchanged.'
      )}
    >
      {status.isError && (
        <Alert variant='destructive'>
          <AlertDescription>
            {t('Worker status is unavailable')}
          </AlertDescription>
        </Alert>
      )}
      {!status.isError && !current && <p role='status'>{t('Loading...')}</p>}
      {!status.isError && current && (
        <div className='space-y-3'>
          <p className='text-muted-foreground text-sm'>
            {t('Serving node')}: <span>{current.node || t('Unknown')}</span>
          </p>
          <p className='text-muted-foreground text-sm'>
            {t(
              'This snapshot covers the responding node only. Other nodes may apply settings at different times.'
            )}
          </p>
          {!current.effective.running ? (
            <p role='status'>
              {t('Image workers are not running on this node')}
            </p>
          ) : (
            <>
              <Table aria-label={t('Image worker status')}>
                <TableHeader>
                  <TableRow>
                    <TableHead>{t('Parameter')}</TableHead>
                    <TableHead>{t('Saved value')}</TableHead>
                    <TableHead>{t('Effective value')}</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  <TableRow>
                    <TableCell>{t('Workers')}</TableCell>
                    <TableCell>
                      {current.configured.worker_concurrency}
                    </TableCell>
                    <TableCell>
                      {current.effective.worker_concurrency}
                    </TableCell>
                  </TableRow>
                  <TableRow>
                    <TableCell>{t('Recovery interval (seconds)')}</TableCell>
                    <TableCell>
                      {current.configured.recovery_interval_seconds}
                    </TableCell>
                    <TableCell>
                      {current.effective.recovery_interval_seconds}
                    </TableCell>
                  </TableRow>
                </TableBody>
              </Table>
              {draining > 0 && (
                <p>
                  {t('Workers still finishing tasks: {{count}}', {
                    count: draining,
                  })}
                </p>
              )}
              <p role='status'>
                {pending
                  ? t('Settings are being applied')
                  : t('Saved worker settings are effective on this node')}
              </p>
            </>
          )}
        </div>
      )}
    </SettingsCard>
  )
}
