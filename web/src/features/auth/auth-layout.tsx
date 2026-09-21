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
import { Pause, Play } from 'lucide-react'
import { motion, useReducedMotion } from 'motion/react'
import { useState } from 'react'
import { useTranslation } from 'react-i18next'

import { Button } from '@/components/ui/button'
import { Skeleton } from '@/components/ui/skeleton'
import { useSystemConfig } from '@/hooks/use-system-config'
import { MOTION_TRANSITION } from '@/lib/motion'

import { AuthBackdrop } from './components/auth-backdrop'

import '@/styles/auth-cyber.css'

type AuthLayoutProps = {
  children: React.ReactNode
}

const PANEL_ENTER = {
  initial: { opacity: 0, y: 28, scale: 0.94, filter: 'blur(10px)' },
  animate: { opacity: 1, y: 0, scale: 1, filter: 'blur(0px)' },
}

const HEADER_ENTER = {
  initial: { opacity: 0, y: -16, filter: 'blur(6px)' },
  animate: { opacity: 1, y: 0, filter: 'blur(0px)' },
}

const FOOTER_ENTER = {
  initial: { opacity: 0, y: 12 },
  animate: { opacity: 1, y: 0 },
}

export function AuthLayout({ children }: AuthLayoutProps) {
  const { t } = useTranslation()
  const { systemName, logo, loading } = useSystemConfig()
  const shouldReduce = useReducedMotion()
  const [motionPaused, setMotionPaused] = useState(false)
  const motionLabel = motionPaused
    ? t('Resume animations')
    : t('Pause animations')
  const MotionIcon = motionPaused ? Play : Pause

  return (
    <div
      className='auth-cyber auth-cyber-shell dark'
      data-motion={motionPaused ? 'paused' : 'running'}
    >
      <AuthBackdrop />

      {shouldReduce ? (
        <header className='auth-cyber-header'>
          <Link to='/' className='auth-cyber-brand'>
            <div className='auth-cyber-brand-mark'>
              {loading ? (
                <Skeleton className='absolute inset-0 rounded-full' />
              ) : (
                <img src={logo} alt={t('Logo')} />
              )}
            </div>
            <div className='auth-cyber-brand-copy'>
              <span className='auth-cyber-brand-kicker'>
                {t('Identity gateway')}
              </span>
              {loading ? (
                <Skeleton className='h-6 w-28' />
              ) : (
                <span className='auth-cyber-brand-name'>{systemName}</span>
              )}
            </div>
          </Link>

          <Button
            type='button'
            variant='ghost'
            size='sm'
            className='auth-cyber-motion-toggle'
            aria-pressed={motionPaused}
            aria-label={motionLabel}
            onClick={() => setMotionPaused((paused) => !paused)}
          >
            <MotionIcon size={14} aria-hidden='true' />
            <span className='hidden sm:inline'>{motionLabel}</span>
          </Button>
        </header>
      ) : (
        <motion.header
          className='auth-cyber-header'
          initial={HEADER_ENTER.initial}
          animate={HEADER_ENTER.animate}
          transition={{ ...MOTION_TRANSITION.slow, delay: 0.02 }}
        >
          <Link to='/' className='auth-cyber-brand'>
            <div className='auth-cyber-brand-mark'>
              {loading ? (
                <Skeleton className='absolute inset-0 rounded-full' />
              ) : (
                <img src={logo} alt={t('Logo')} />
              )}
            </div>
            <div className='auth-cyber-brand-copy'>
              <span className='auth-cyber-brand-kicker'>
                {t('Identity gateway')}
              </span>
              {loading ? (
                <Skeleton className='h-6 w-28' />
              ) : (
                <span className='auth-cyber-brand-name'>{systemName}</span>
              )}
            </div>
          </Link>

          <Button
            type='button'
            variant='ghost'
            size='sm'
            className='auth-cyber-motion-toggle'
            aria-pressed={motionPaused}
            aria-label={motionLabel}
            onClick={() => setMotionPaused((paused) => !paused)}
          >
            <MotionIcon size={14} aria-hidden='true' />
            <span className='hidden sm:inline'>{motionLabel}</span>
          </Button>
        </motion.header>
      )}

      <main className='auth-cyber-main'>
        {shouldReduce ? (
          <div className='auth-cyber-panel'>
            <div className='auth-cyber-panel-glow' aria-hidden='true' />
            <div className='auth-cyber-panel-corners' aria-hidden='true'>
              <span />
              <span />
              <span />
              <span />
            </div>
            <div className='auth-cyber-panel-body'>{children}</div>
          </div>
        ) : (
          <motion.div
            className='auth-cyber-panel'
            initial={PANEL_ENTER.initial}
            animate={PANEL_ENTER.animate}
            transition={{
              type: 'spring',
              stiffness: 120,
              damping: 18,
              delay: 0.12,
            }}
          >
            <div className='auth-cyber-panel-glow' aria-hidden='true' />
            <div className='auth-cyber-panel-corners' aria-hidden='true'>
              <span />
              <span />
              <span />
              <span />
            </div>
            <div className='auth-cyber-panel-body'>{children}</div>
          </motion.div>
        )}
      </main>

      {shouldReduce ? (
        <footer className='auth-cyber-footer' aria-hidden='true'>
          <span className='auth-cyber-footer-live'>
            {t('Secure channel online')}
          </span>
          <span className='auth-cyber-footer-code'>AUTH / TLS · GATEWAY</span>
        </footer>
      ) : (
        <motion.footer
          className='auth-cyber-footer'
          aria-hidden='true'
          initial={FOOTER_ENTER.initial}
          animate={FOOTER_ENTER.animate}
          transition={{ ...MOTION_TRANSITION.slow, delay: 0.35 }}
        >
          <span className='auth-cyber-footer-live'>
            {t('Secure channel online')}
          </span>
          <span className='auth-cyber-footer-code'>AUTH / TLS · GATEWAY</span>
        </motion.footer>
      )}
    </div>
  )
}
