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
export interface CanvasIntegration {
  enabled: boolean
  url: string
  client_id: string
}

export function canvasOrigin(value: string): string | null {
  // Reject inputs that URL would silently rewrite into an origin.
  const origin = /^https?:\/\/(\[[^\]]+\]|[^/:]+)(?::\d*)?\/?$/i.exec(value)
  if (/[\s\\?#@]/.test(value) || !origin) {
    return null
  }
  try {
    const url = new URL(value)
    if (
      /^\d+\.\d+\.\d+\.\d+$/.test(url.hostname) &&
      origin[1] !== url.hostname
    ) {
      return null
    }
    const local =
      ['localhost', '[::1]'].includes(url.hostname) ||
      /^127\.(?:\d{1,3}\.){2}\d{1,3}$/.test(url.hostname) ||
      /^\[::ffff:7f[0-9a-f]{2}:[0-9a-f]{1,4}\]$/.test(url.hostname)
    if (url.protocol !== 'https:' && !(url.protocol === 'http:' && local)) {
      return null
    }
    if (
      url.username ||
      url.password ||
      url.search ||
      url.hash ||
      url.pathname !== '/'
    ) {
      return null
    }
    return url.origin
  } catch {
    return null
  }
}

export function canvasIntegration(
  status: Record<string, unknown> | null
): CanvasIntegration {
  const raw = status?.infinite_canvas
  if (!raw || typeof raw !== 'object') {
    return { enabled: false, url: '', client_id: '' }
  }
  const config = raw as Record<string, unknown>
  const origin =
    typeof config.url === 'string' ? canvasOrigin(config.url) : null
  return {
    enabled: config.enabled === true && origin !== null,
    url: origin ?? '',
    client_id: typeof config.client_id === 'string' ? config.client_id : '',
  }
}
