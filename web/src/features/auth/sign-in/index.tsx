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
import { Link, useSearch } from '@tanstack/react-router'
import { useTranslation } from 'react-i18next'

import {
  StaggerContainer,
  StaggerItem,
} from '@/components/page-transition'
import { useStatus } from '@/hooks/use-status'
import { CARD_ITEM_VARIANTS, CARD_STAGGER_VARIANTS } from '@/lib/motion'

import { AuthLayout } from '../auth-layout'
import { AuthPageHeader } from '../components/auth-page-header'
import { TermsFooter } from '../components/terms-footer'
import { UserAuthForm } from './components/user-auth-form'

export function SignIn() {
  const { t } = useTranslation()
  const { redirect } = useSearch({ from: '/(auth)/sign-in' })
  const { status } = useStatus()

  return (
    <AuthLayout>
      <StaggerContainer
        className='w-full space-y-8'
        variants={CARD_STAGGER_VARIANTS}
      >
        <StaggerItem variants={CARD_ITEM_VARIANTS}>
          <AuthPageHeader
            accentTitle
            title={t('Sign in')}
            kicker={t('Welcome back')}
            description={
              !status?.self_use_mode_enabled &&
              status?.register_enabled !== false ? (
                <>
                  {t("Don't have an account?")}{' '}
                  <Link to='/sign-up'>{t('Sign up')}</Link>.
                </>
              ) : undefined
            }
          />
        </StaggerItem>

        <StaggerItem variants={CARD_ITEM_VARIANTS}>
          <UserAuthForm redirectTo={redirect} />
        </StaggerItem>

        <StaggerItem variants={CARD_ITEM_VARIANTS}>
          <TermsFooter
            variant='sign-in'
            status={status}
            className='text-center'
          />
        </StaggerItem>
      </StaggerContainer>
    </AuthLayout>
  )
}
