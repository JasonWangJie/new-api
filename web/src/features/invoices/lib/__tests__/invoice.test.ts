/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.
*/
import { describe, expect, it } from 'vitest'

import type { InvoiceEligibleOrder } from '../../types'
import {
  formatInvoiceAmount,
  formatInvoiceDateTime,
  sumInvoiceOrders,
} from '../format'
import { invoiceApplicationSchema } from '../schemas'
import { reconcileInvoiceOrderSelection } from '../selection'

function order(id: number, amountCents: number): InvoiceEligibleOrder {
  return {
    top_up_id: id,
    trade_no: `order-${id}`,
    amount_cents: amountCents,
    create_time: 1,
    complete_time: 2,
  }
}

describe('invoice application helpers', () => {
  it.each([
    ['zhCN', 'zh-CN'],
    ['zhTW', 'zh-TW'],
  ])('formats invoice values for the project locale %s', (language, locale) => {
    const expected = new Intl.NumberFormat(locale, {
      style: 'currency',
      currency: 'CNY',
      minimumFractionDigits: 2,
      maximumFractionDigits: 2,
    }).format(12.34)
    const timestamp = 1_700_000_000

    expect(formatInvoiceAmount(1234, language)).toBe(expected)
    expect(formatInvoiceDateTime(timestamp, language)).toBe(
      new Date(timestamp * 1000).toLocaleString(locale)
    )
  })

  it('preserves off-page orders while updating the visible page selection', () => {
    const offPage = order(1, 100)
    const pageOrders = [order(2, 200), order(3, 300)]
    const selected = new Map([[offPage.top_up_id, offPage]])

    const next = reconcileInvoiceOrderSelection(selected, pageOrders, {
      '1': true,
      '2': true,
    })

    expect([...next.keys()]).toEqual([1, 2])
    expect(sumInvoiceOrders(next.values())).toBe(300)
  })

  it('validates all enterprise invoice fields without altering valid input', () => {
    expect(
      invoiceApplicationSchema.safeParse({
        company_name: '',
        tax_id: '',
        email: 'invalid',
      }).success
    ).toBe(false)

    const result = invoiceApplicationSchema.parse({
      company_name: ' Example Co. ',
      tax_id: ' TAX-001 ',
      email: ' billing@example.com ',
    })
    expect(result).toEqual({
      company_name: 'Example Co.',
      tax_id: 'TAX-001',
      email: 'billing@example.com',
    })
  })
})
