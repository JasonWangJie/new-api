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
const PARTICLE_SEEDS = [
  { left: '4%', delay: '0s', duration: '9s' },
  { left: '9%', delay: '1.1s', duration: '12s' },
  { left: '14%', delay: '2.4s', duration: '10s' },
  { left: '19%', delay: '0.6s', duration: '14s' },
  { left: '25%', delay: '3.2s', duration: '11s' },
  { left: '31%', delay: '1.8s', duration: '13s' },
  { left: '37%', delay: '4.1s', duration: '9s' },
  { left: '43%', delay: '0.3s', duration: '15s' },
  { left: '49%', delay: '2.7s', duration: '12s' },
  { left: '55%', delay: '5.0s', duration: '10s' },
  { left: '61%', delay: '1.4s', duration: '14s' },
  { left: '67%', delay: '3.6s', duration: '11s' },
  { left: '73%', delay: '0.9s', duration: '13s' },
  { left: '79%', delay: '4.5s', duration: '9s' },
  { left: '85%', delay: '2.1s', duration: '16s' },
  { left: '91%', delay: '5.6s', duration: '12s' },
  { left: '96%', delay: '3.0s', duration: '10s' },
  { left: '22%', delay: '6.2s', duration: '14s' },
  { left: '58%', delay: '6.8s', duration: '11s' },
  { left: '78%', delay: '7.3s', duration: '13s' },
] as const

const RAIN_SEEDS = [
  { left: '7%', delay: '0s', duration: '4.2s' },
  { left: '18%', delay: '1.1s', duration: '5.1s' },
  { left: '29%', delay: '2.3s', duration: '3.8s' },
  { left: '41%', delay: '0.7s', duration: '4.8s' },
  { left: '53%', delay: '1.9s', duration: '5.4s' },
  { left: '64%', delay: '3.1s', duration: '4.0s' },
  { left: '76%', delay: '0.4s', duration: '4.6s' },
  { left: '88%', delay: '2.6s', duration: '5.2s' },
] as const

const METEOR_SEEDS = [
  { top: '12%', delay: '0s', duration: '7s' },
  { top: '28%', delay: '2.4s', duration: '8.5s' },
  { top: '46%', delay: '4.8s', duration: '6.8s' },
  { top: '62%', delay: '1.2s', duration: '9s' },
] as const

export function AuthBackdrop() {
  return (
    <div className='auth-cyber-backdrop' aria-hidden='true'>
      <div className='auth-cyber-grid' />
      <div className='auth-cyber-hex' />
      <div className='auth-cyber-orb auth-cyber-orb-acid' />
      <div className='auth-cyber-orb auth-cyber-orb-cyan' />
      <div className='auth-cyber-orb auth-cyber-orb-core' />
      <div className='auth-cyber-orb auth-cyber-orb-side' />

      <div className='auth-cyber-radar'>
        <span className='auth-cyber-radar-sweep' />
        <span className='auth-cyber-radar-ring' />
        <span className='auth-cyber-radar-ring auth-cyber-radar-ring-mid' />
        <span className='auth-cyber-radar-ring auth-cyber-radar-ring-outer' />
        <span className='auth-cyber-radar-cross' />
      </div>

      <div className='auth-cyber-rings'>
        <div className='auth-cyber-ring' />
        <div className='auth-cyber-ring auth-cyber-ring-two' />
        <div className='auth-cyber-ring auth-cyber-ring-three' />
        <div className='auth-cyber-ring auth-cyber-ring-four' />
      </div>

      <svg
        className='auth-cyber-constellation'
        viewBox='0 0 100 100'
        preserveAspectRatio='none'
      >
        <path
          className='auth-cyber-constellation-path'
          d='M8 22 L24 18 L38 34 L52 20 L68 40 L82 28 L92 48'
        />
        <path
          className='auth-cyber-constellation-path auth-cyber-constellation-path-b'
          d='M12 78 L28 64 L44 72 L60 58 L76 70 L90 62'
        />
        <circle cx='24' cy='18' r='0.7' />
        <circle cx='38' cy='34' r='0.55' />
        <circle cx='68' cy='40' r='0.7' />
        <circle cx='28' cy='64' r='0.55' />
        <circle cx='60' cy='58' r='0.7' />
        <circle cx='90' cy='62' r='0.55' />
      </svg>

      <div className='auth-cyber-beams'>
        <span className='auth-cyber-beam' />
        <span className='auth-cyber-beam' />
        <span className='auth-cyber-beam' />
        <span className='auth-cyber-beam auth-cyber-beam-vertical' />
        <span className='auth-cyber-beam auth-cyber-beam-vertical auth-cyber-beam-vertical-b' />
      </div>

      <div className='auth-cyber-scan' />
      <div className='auth-cyber-scan auth-cyber-scan-fast' />

      <div className='auth-cyber-rain'>
        {RAIN_SEEDS.map((drop) => (
          <span
            key={`rain-${drop.left}-${drop.delay}`}
            className='auth-cyber-rain-drop'
            style={{
              left: drop.left,
              animationDelay: drop.delay,
              animationDuration: drop.duration,
            }}
          />
        ))}
      </div>

      <div className='auth-cyber-meteors'>
        {METEOR_SEEDS.map((meteor) => (
          <span
            key={`meteor-${meteor.top}-${meteor.delay}`}
            className='auth-cyber-meteor'
            style={{
              top: meteor.top,
              animationDelay: meteor.delay,
              animationDuration: meteor.duration,
            }}
          />
        ))}
      </div>

      <div className='auth-cyber-particles'>
        {PARTICLE_SEEDS.map((particle) => (
          <span
            key={`${particle.left}-${particle.delay}`}
            className='auth-cyber-particle'
            style={{
              left: particle.left,
              bottom: '-4%',
              animationDelay: particle.delay,
              animationDuration: particle.duration,
            }}
          />
        ))}
      </div>

      <div className='auth-cyber-hud'>
        <span />
        <span />
        <span />
        <span />
      </div>

      <div className='auth-cyber-noise' />
      <div className='auth-cyber-vignette' />
    </div>
  )
}
