/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.
*/
import { toIntlLocale } from '@/i18n/languages'

export function formatInvoiceAmount(cents: number, locale?: string): string {
  return new Intl.NumberFormat(toIntlLocale(locale), {
    style: 'currency',
    currency: 'CNY',
    minimumFractionDigits: 2,
    maximumFractionDigits: 2,
  }).format(cents / 100)
}

export function formatInvoiceDateTime(
  timestamp: number,
  locale?: string
): string {
  return new Date(timestamp * 1000).toLocaleString(toIntlLocale(locale))
}

export function sumInvoiceOrders(
  orders: Iterable<{ amount_cents: number }>
): number {
  let total = 0
  for (const order of orders) total += order.amount_cents
  return total
}
