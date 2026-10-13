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
import type { StatusBadgeProps } from '@/components/status-badge'
import { formatTimestampToDate } from '@/lib/format'

import type { TopupStatus } from '../types'

// ============================================================================
// Billing Utility Functions
// ============================================================================

interface StatusConfig {
  variant: StatusBadgeProps['variant']
  label: string
}

/**
 * Status badge configuration
 */
export const STATUS_CONFIG: Record<TopupStatus, StatusConfig> = {
  success: {
    variant: 'success',
    label: 'Success',
  },
  pending: {
    variant: 'warning',
    label: 'Pending',
  },
  expired: {
    variant: 'danger',
    label: 'Expired',
  },
}

/**
 * Get status badge configuration
 */
export function getStatusConfig(status: TopupStatus): StatusConfig {
  return STATUS_CONFIG[status] || STATUS_CONFIG.pending
}

/**
 * Payment method display names
 */
export const PAYMENT_METHOD_NAMES: Record<string, string> = {
  stripe: 'Stripe',
  alipay: 'Alipay',
  wxpay: 'WeChat Pay',
  waffo: 'Waffo',
  manual_backend: 'Manual backend',
}

/** Format confirmed RMB cents without floating-point rounding. */
export function formatCnyCents(cents: number): string {
  const integer = BigInt(cents)
  return `${integer / 100n}.${String(integer % 100n).padStart(2, '0')}`
}

/** Money input is always RMB with at most two fractional digits. */
export function parseCnyCents(input: string): number | null {
  if (input.length > 32 || !/^\d+(?:\.\d{1,2})?$/.test(input)) return null
  const [whole, fraction = ''] = input.split('.')
  const cents = BigInt(whole) * 100n + BigInt(fraction.padEnd(2, '0'))
  if (cents > BigInt(Number.MAX_SAFE_INTEGER)) return null
  return Number(cents)
}

/**
 * Get payment method display name
 */
export function getPaymentMethodName(
  method: string,
  t?: (key: string) => string
): string {
  const name = PAYMENT_METHOD_NAMES[method] || method
  return t ? t(name) : name
}

/**
 * Format timestamp to readable date string
 */
export function formatTimestamp(timestamp: number): string {
  return formatTimestampToDate(timestamp)
}
