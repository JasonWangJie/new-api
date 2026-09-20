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
  { left: '8%', delay: '0s', duration: '11s' },
  { left: '18%', delay: '1.4s', duration: '13s' },
  { left: '27%', delay: '3.1s', duration: '10s' },
  { left: '39%', delay: '0.7s', duration: '14s' },
  { left: '48%', delay: '2.2s', duration: '12s' },
  { left: '57%', delay: '4.5s', duration: '15s' },
  { left: '66%', delay: '1.1s', duration: '11s' },
  { left: '74%', delay: '3.8s', duration: '13s' },
  { left: '83%', delay: '2.6s', duration: '16s' },
  { left: '91%', delay: '0.4s', duration: '12s' },
  { left: '14%', delay: '5.2s', duration: '14s' },
  { left: '62%', delay: '6.1s', duration: '10s' },
] as const

export function AuthBackdrop() {
  return (
    <div className='auth-cyber-backdrop' aria-hidden='true'>
      <div className='auth-cyber-grid' />
      <div className='auth-cyber-orb auth-cyber-orb-acid' />
      <div className='auth-cyber-orb auth-cyber-orb-cyan' />
      <div className='auth-cyber-orb auth-cyber-orb-core' />
      <div className='auth-cyber-rings'>
        <div className='auth-cyber-ring' />
        <div className='auth-cyber-ring auth-cyber-ring-two' />
        <div className='auth-cyber-ring auth-cyber-ring-three' />
      </div>
      <div className='auth-cyber-beams'>
        <span className='auth-cyber-beam' />
        <span className='auth-cyber-beam' />
        <span className='auth-cyber-beam' />
      </div>
      <div className='auth-cyber-scan' />
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
      <div className='auth-cyber-noise' />
      <div className='auth-cyber-vignette' />
    </div>
  )
}
