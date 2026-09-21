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
import { Link } from '@tanstack/react-router'
import { useTranslation } from 'react-i18next'

import {
  StaggerContainer,
  StaggerItem,
} from '@/components/page-transition'
import { MOTION_TRANSITION } from '@/lib/motion'

import { AuthLayout } from '../auth-layout'
import { AuthPageHeader } from '../components/auth-page-header'
import { ForgotPasswordForm } from './components/forgot-password-form'

const AUTH_STAGGER = {
  initial: {},
  animate: { transition: { staggerChildren: 0.12, delayChildren: 0.28 } },
}

const AUTH_ITEM = {
  initial: { opacity: 0, y: 22, scale: 0.97, filter: 'blur(6px)' },
  animate: {
    opacity: 1,
    y: 0,
    scale: 1,
    filter: 'blur(0px)',
    transition: MOTION_TRANSITION.slow,
  },
}

export function ForgotPassword() {
  const { t } = useTranslation()
  return (
    <AuthLayout>
      <StaggerContainer className='w-full space-y-8' variants={AUTH_STAGGER}>
        <StaggerItem variants={AUTH_ITEM}>
          <AuthPageHeader
            accentTitle
            title={t('Forgot password')}
            kicker={t('Account recovery')}
            description={
              <div className='space-y-2'>
                <p>
                  {t(
                    'Enter your registered email and we will send you a link to reset your password.'
                  )}
                </p>
                <p>
                  {t("Don't have an account?")}{' '}
                  <Link to='/sign-up'>{t('Sign up')}</Link>.
                </p>
              </div>
            }
          />
        </StaggerItem>

        <StaggerItem variants={AUTH_ITEM}>
          <ForgotPasswordForm className='space-y-0' />
        </StaggerItem>
      </StaggerContainer>
    </AuthLayout>
  )
}
