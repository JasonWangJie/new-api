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
import { useStatus } from '@/hooks/use-status'
import { MOTION_TRANSITION } from '@/lib/motion'

import { AuthLayout } from '../auth-layout'
import { AuthPageHeader } from '../components/auth-page-header'
import { TermsFooter } from '../components/terms-footer'
import { SignUpForm } from './components/sign-up-form'

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

export function SignUp() {
  const { t } = useTranslation()
  const { status } = useStatus()

  return (
    <AuthLayout>
      <StaggerContainer className='w-full space-y-8' variants={AUTH_STAGGER}>
        <StaggerItem variants={AUTH_ITEM}>
          <AuthPageHeader
            accentTitle
            title={t('Create an account')}
            kicker={t('Join the network')}
            description={
              <>
                {t('Already have an account?')}{' '}
                <Link to='/sign-in'>{t('Sign in')}</Link>.
              </>
            }
          />
        </StaggerItem>

        <StaggerItem variants={AUTH_ITEM}>
          <SignUpForm />
        </StaggerItem>

        <StaggerItem variants={AUTH_ITEM}>
          <TermsFooter
            variant='sign-up'
            status={status}
            className='text-center'
          />
        </StaggerItem>
      </StaggerContainer>
    </AuthLayout>
  )
}
