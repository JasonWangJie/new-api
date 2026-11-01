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
import { lazy, Suspense, useState } from 'react'
import { useTranslation } from 'react-i18next'

import { Dialog } from '@/components/dialog'
import { Button } from '@/components/ui/button'
import { hasPermission } from '@/lib/admin-permissions'
import { useAuthStore } from '@/stores/auth-store'

import type { RetryDraftSource } from './types'

const RetryPolicyCenter = lazy(() =>
  import('./index').then((module) => ({ default: module.RetryPolicyCenter }))
)
export function CreateRetryRuleFromError(props: { source: RetryDraftSource }) {
  const { t } = useTranslation()
  const user = useAuthStore((state) => state.auth.user)
  const [open, setOpen] = useState(false)
  if (
    !hasPermission(user, 'image_config', 'manage') ||
    !props.source.fact.message
  ) {
    return null
  }
  return (
    <>
      <Button size='sm' variant='outline' onClick={() => setOpen(true)}>
        {t('Create rule from error')}
      </Button>
      {open && (
        <Dialog
          open
          onOpenChange={setOpen}
          title={t('Error and retry rules')}
          contentClassName='sm:max-w-6xl'
        >
          <Suspense fallback={<p role='status'>{t('Loading...')}</p>}>
            <RetryPolicyCenter source={props.source} />
          </Suspense>
        </Dialog>
      )}
    </>
  )
}
