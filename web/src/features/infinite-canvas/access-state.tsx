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

import { Main } from '@/components/layout'
import { Button } from '@/components/ui/button'
import {
  Empty,
  EmptyDescription,
  EmptyHeader,
  EmptyTitle,
} from '@/components/ui/empty'
import { Spinner } from '@/components/ui/spinner'

export function CanvasAccessState(props: { error?: unknown }) {
  const { t } = useTranslation()
  return (
    <Main>
      <Empty role={props.error ? 'alert' : 'status'}>
        <EmptyHeader>
          <EmptyTitle>{t('AI Infinite Canvas')}</EmptyTitle>
          <EmptyDescription>
            {props.error
              ? t(
                  'Canvas authorization failed. Return to the entry and try again.'
                )
              : t('Opening your canvas securely...')}
          </EmptyDescription>
        </EmptyHeader>
        {props.error ? (
          <Button render={<a href='/canvas' />}>{t('Try again')}</Button>
        ) : (
          <Spinner aria-label={t('Loading')} />
        )}
      </Empty>
    </Main>
  )
}
