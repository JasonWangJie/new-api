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
import { CherryStudio, OpenAI } from '@lobehub/icons'
import { Link } from '@tanstack/react-router'
import {
  ArrowDown,
  ArrowRight,
  ArrowUpRight,
  AudioLines,
  Braces,
  CircuitBoard,
  Command,
  Image,
  Layers3,
  Pause,
  Play,
  Radio,
  ShieldCheck,
  Workflow,
} from 'lucide-react'
import { useState } from 'react'
import { useTranslation } from 'react-i18next'

import { AnimateInView } from '@/components/animate-in-view'
import { Button } from '@/components/ui/button'
import { useStatus } from '@/hooks/use-status'
import { DEFAULT_SYSTEM_NAME } from '@/lib/constants'

import { HeroTerminalDemo } from './hero-terminal-demo'

interface CyberLandingProps {
  isAuthenticated: boolean
}

export function CyberLanding(props: CyberLandingProps) {
  const { t } = useTranslation()
  const { status } = useStatus()
  const [motionPaused, setMotionPaused] = useState(false)
  const systemName = status?.system_name?.trim() || DEFAULT_SYSTEM_NAME
  const primaryLabel = props.isAuthenticated
    ? t('Go to Dashboard')
    : t('Get Started')
  const primaryRoute = props.isAuthenticated ? '/dashboard' : '/sign-up'
  const motionLabel = motionPaused
    ? t('Resume animations')
    : t('Pause animations')
  const MotionIcon = motionPaused ? Play : Pause
  const capabilities = [
    {
      icon: Workflow,
      code: '01',
      title: t('Connect a universe of models.'),
      desc: t(
        'OpenAI, Claude, Gemini and more, connected in one gateway. Give every request its own path through your model universe.'
      ),
    },
    {
      icon: Image,
      code: '02',
      title: t('Make imagination visible.'),
      desc: t(
        'Think in conversation, create in images, express through audio. Push the boundaries of your ideas with one shared infrastructure.'
      ),
    },
    {
      icon: ShieldCheck,
      code: '03',
      title: t('Your rules. Your creative freedom.'),
      desc: t(
        'Channels, tokens, usage and billing in one dashboard. Give the system your rules and give your ideas your full attention.'
      ),
    },
  ]
  const steps = [
    {
      number: '01',
      title: t('Configure'),
      description: t(
        'Add your API keys, set up channels and configure access permissions'
      ),
    },
    {
      number: '02',
      title: t('Connect'),
      description: t(
        'Connect through OpenAI, Claude, Gemini, and other compatible API routes'
      ),
    },
    {
      number: '03',
      title: t('Monitor'),
      description: t(
        'Track usage, costs and performance with real-time analytics'
      ),
    },
  ]

  return (
    <main
      className='cyber-landing'
      data-motion={motionPaused ? 'paused' : 'running'}
    >
      <section className='cyber-hero' aria-labelledby='cyber-hero-title'>
        <div className='cyber-hero-dust' aria-hidden />
        <div className='cyber-hero-grid cyber-shell'>
          <div className='cyber-hero-copy'>
            <div className='cyber-kicker cyber-hero-entry'>
              <span className='cyber-signal' aria-hidden />
              {t('A launchpad for AI imagination')}
            </div>
            <h1 id='cyber-hero-title' className='cyber-hero-entry'>
              {t('Let imagination')} <br />
              <span className='cyber-title-accent'>
                {t('break new dimensions.')}
              </span>
            </h1>
            <p className='cyber-hero-description cyber-hero-entry'>
              {t(
                'From a spark in your mind to a moment on screen. Connect models, awaken creativity and give every idea a way to shine.'
              )}
            </p>
            <div className='cyber-hero-actions cyber-hero-entry'>
              <Button
                size='lg'
                role='link'
                className='cyber-button-primary'
                render={<Link to={primaryRoute} />}
              >
                {primaryLabel}
                <ArrowUpRight aria-hidden />
              </Button>
              <Button
                size='lg'
                role='link'
                variant='outline'
                className='cyber-button-secondary'
                render={<a href='#protocols' />}
              >
                <Braces aria-hidden />
                {t('Explore the API')}
              </Button>
            </div>
          </div>

          <div
            className='cyber-network'
            aria-label={t('Unified AI gateway network illustration')}
            role='img'
          >
            <div className='cyber-network-top' aria-hidden>
              <span>
                <Radio size={12} /> NEURAL NETWORK
              </span>
              <span>SYS / 001</span>
            </div>
            <div className='cyber-network-glow' aria-hidden />
            <div className='cyber-orbit cyber-orbit-one' aria-hidden>
              <span />
            </div>
            <div className='cyber-orbit cyber-orbit-two' aria-hidden>
              <span />
            </div>
            <div className='cyber-orbit cyber-orbit-three' aria-hidden>
              <span />
            </div>
            <svg
              className='cyber-network-lines'
              viewBox='0 0 600 540'
              fill='none'
              aria-hidden
              focusable='false'
            >
              <path d='M300 270L101 126M300 270L489 105M300 270L514 341M300 270L135 416M300 270L72 287M300 270L396 453' />
              <path
                className='cyber-data-line'
                d='M101 126L300 270L514 341M489 105L300 270L135 416M72 287L300 270L396 453'
              />
              <circle
                cx='300'
                cy='270'
                r='166'
                className='cyber-network-ring'
              />
              <circle
                cx='300'
                cy='270'
                r='220'
                className='cyber-network-ring cyber-network-ring-outer'
              />
            </svg>
            <div className='cyber-core' aria-hidden>
              <CircuitBoard strokeWidth={1} />
              <strong>{systemName}</strong>
              <span>{t('Unified API Gateway')}</span>
              <div className='cyber-core-bars'>
                {Array.from({ length: 12 }, (_, index) => (
                  <i
                    key={index}
                    style={{ animationDelay: `${index * 120}ms` }}
                  />
                ))}
              </div>
            </div>
            <div className='cyber-node cyber-node-openai' aria-hidden>
              <OpenAI className='cyber-node-logo' size={26} />
              <span>
                OpenAI<small>CHAT / RESPONSES</small>
              </span>
            </div>
            <div className='cyber-node cyber-node-claude' aria-hidden>
              <span className='cyber-node-mark cyber-node-mark-amber'>✳</span>
              <span>
                Claude<small>MESSAGES</small>
              </span>
            </div>
            <div className='cyber-node cyber-node-gemini' aria-hidden>
              <span className='cyber-node-mark cyber-node-mark-cyan'>✦</span>
              <span>
                Gemini<small>GENERATE CONTENT</small>
              </span>
            </div>
            <div className='cyber-node cyber-node-image' aria-hidden>
              <Image size={20} />
              <span>
                {t('Image generation')}
                <small>IMAGE / ASYNC</small>
              </span>
            </div>
            <div className='cyber-node cyber-node-audio' aria-hidden>
              <AudioLines size={19} />
            </div>
            <div className='cyber-node cyber-node-models' aria-hidden>
              <Layers3 size={19} />
              <span>{t('Multi-provider')}</span>
            </div>
            <div className='cyber-network-bottom' aria-hidden>
              <span>REQUEST → ROUTE → RESPONSE</span>
              <span className='cyber-network-coordinate'>[ 30.0 / 60.0 ]</span>
            </div>
          </div>
        </div>
        <div className='cyber-hero-bottom cyber-shell'>
          <a href='#capabilities' className='cyber-scroll-link'>
            <ArrowDown size={15} aria-hidden />
            {t('Discover the gateway')}
          </a>
          <Button
            variant='ghost'
            className='cyber-motion-toggle'
            aria-label={motionLabel}
            aria-pressed={motionPaused}
            onClick={() => setMotionPaused((paused) => !paused)}
          >
            <MotionIcon size={14} aria-hidden />
            <span>{motionLabel}</span>
          </Button>
        </div>
      </section>

      <section
        className='cyber-integrations'
        aria-label={t('Supported Applications')}
      >
        <div className='cyber-shell cyber-integrations-inner'>
          <div className='cyber-integration-label'>
            <span className='cyber-kicker'>{t('Fits your workflow')}</span>
            <p>
              {t(
                'Supports one-click configuration and perfectly adapts to NewAPI multi-protocol configuration.'
              )}
            </p>
          </div>
          <div className='cyber-integration-apps'>
            <a
              href='https://cherry-ai.com/'
              target='_blank'
              rel='noopener noreferrer'
            >
              <CherryStudio size={24} aria-hidden />
              <span>Cherry Studio</span>
              <ArrowUpRight size={13} aria-hidden />
            </a>
            <a
              href='https://ccswitch.io/'
              target='_blank'
              rel='noopener noreferrer'
            >
              <Command size={23} aria-hidden />
              <span>CC Switch</span>
              <ArrowUpRight size={13} aria-hidden />
            </a>
            <span className='cyber-integration-more'>
              <Braces size={24} aria-hidden />
              {t('Your application')}
            </span>
          </div>
        </div>
      </section>

      <section
        id='capabilities'
        className='cyber-section cyber-shell'
        aria-labelledby='cyber-capabilities-title'
      >
        <AnimateInView className='cyber-section-heading'>
          <div>
            <span className='cyber-kicker'>
              <span className='cyber-section-index'>01 /</span>
              {t('The control layer')}
            </span>
            <h2 id='cyber-capabilities-title'>
              {t('Less friction.')} <br />
              <span className='cyber-title-accent'>
                {t('More room to create.')}
              </span>
            </h2>
          </div>
          <p>
            {t(
              'Keep the complexity behind the scenes. Put your next creation in the spotlight.'
            )}
          </p>
        </AnimateInView>
        <div className='cyber-capability-grid'>
          {capabilities.map((capability, index) => (
            <AnimateInView
              key={capability.code}
              delay={index * 90}
              className='cyber-capability-reveal'
            >
              <article
                className={`cyber-capability cyber-capability-${capability.code}`}
                aria-labelledby={`cyber-capability-title-${capability.code}`}
              >
                <div
                  className='auto-group-flow-border cyber-capability-border'
                  aria-hidden
                />
                <div className='cyber-capability-scan' aria-hidden />
                <div className='cyber-capability-top'>
                  <capability.icon strokeWidth={1.3} size={28} aria-hidden />
                  <span>{capability.code}</span>
                </div>
                <div
                  className={`cyber-capability-art cyber-capability-art-${capability.code}`}
                  aria-hidden
                >
                  <span />
                  <span />
                  <span />
                  <span />
                  <span />
                </div>
                <h3 id={`cyber-capability-title-${capability.code}`}>
                  {capability.title}
                </h3>
                <p>{capability.desc}</p>
              </article>
            </AnimateInView>
          ))}
        </div>
      </section>

      <section
        id='protocols'
        className='cyber-protocol-section'
        aria-labelledby='cyber-protocol-title'
      >
        <div className='cyber-shell cyber-protocol-grid'>
          <AnimateInView className='cyber-protocol-copy'>
            <span className='cyber-kicker'>
              <span className='cyber-section-index'>02 /</span>
              {t('Made for developers')}
            </span>
            <h2 id='cyber-protocol-title'>
              {t('Familiar code.')} <br />
              <span className='cyber-title-accent'>
                {t('Unfamiliar possibilities.')}
              </span>
            </h2>
            <p>
              {t(
                'Use familiar API formats with your configured models. Explore the request and response examples to start connecting.'
              )}
            </p>
            <div className='cyber-protocol-tags'>
              <span>OpenAI</span>
              <span>Anthropic</span>
              <span>Gemini</span>
              <span>SSE</span>
            </div>
          </AnimateInView>
          <AnimateInView className='cyber-protocol-demo' delay={120}>
            <div className='cyber-terminal-caption'>
              <span>
                <Braces size={14} aria-hidden />
                {t('Protocol preview')}
              </span>
              <span>{t('Illustrative examples')}</span>
            </div>
            <HeroTerminalDemo className='cyber-terminal' autoPlay={false} />
          </AnimateInView>
        </div>
      </section>

      <section
        className='cyber-section cyber-shell cyber-launch'
        aria-labelledby='cyber-launch-title'
      >
        <AnimateInView className='cyber-section-heading'>
          <div>
            <span className='cyber-kicker'>
              <span className='cyber-section-index'>03 /</span>
              {t('How It Works')}
            </span>
            <h2 id='cyber-launch-title'>{t('Three steps to get started')}</h2>
          </div>
          <span className='cyber-launch-decoration' aria-hidden>
            <span />
            {t('From setup to your first request')}
          </span>
        </AnimateInView>
        <div className='cyber-steps'>
          {steps.map((step, index) => (
            <AnimateInView
              key={step.number}
              delay={index * 90}
              className='cyber-step'
            >
              <span className='cyber-step-number'>{step.number}</span>
              <div>
                <h3>{step.title}</h3>
                <p>{step.description}</p>
              </div>
              <ArrowRight className='cyber-step-arrow' aria-hidden />
            </AnimateInView>
          ))}
        </div>
        <AnimateInView className='cyber-cta'>
          <div className='cyber-cta-grid' aria-hidden />
          <div className='cyber-cta-copy'>
            <span className='cyber-kicker'>
              {t('Your next idea deserves a debut')}
            </span>
            <h2 className='cyber-title-accent'>
              {t('Give your imagination a stage.')}
            </h2>
            <p>{t('From a passing thought to something worth sharing.')}</p>
          </div>
          <Button
            size='lg'
            role='link'
            className='cyber-button-primary'
            render={<Link to={primaryRoute} />}
          >
            {primaryLabel}
            <ArrowUpRight aria-hidden />
          </Button>
        </AnimateInView>
      </section>
    </main>
  )
}
