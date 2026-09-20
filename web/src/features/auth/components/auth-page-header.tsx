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
import type { ReactNode } from 'react'
import { useTranslation } from 'react-i18next'

import { cn } from '@/lib/utils'

type AuthPageHeaderProps = {
  title: string
  description?: ReactNode
  kicker?: string
  className?: string
  accentTitle?: boolean
}

export function AuthPageHeader(props: AuthPageHeaderProps) {
  const { t } = useTranslation()
  const kicker = props.kicker ?? t('Identity gateway')

  return (
    <div className={cn('space-y-1', props.className)}>
      <div className='auth-page-kicker'>
        <span className='auth-page-signal' aria-hidden='true' />
        {kicker}
      </div>
      <h2 className='auth-page-title'>
        {props.accentTitle ? (
          <span className='auth-page-title-accent'>{props.title}</span>
        ) : (
          props.title
        )}
      </h2>
      {props.description ? (
        <div className='auth-page-description'>{props.description}</div>
      ) : null}
    </div>
  )
}
