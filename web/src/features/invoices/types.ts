/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.
*/

export type InvoiceStatus = 'pending' | 'completed' | 'rejected'
export type InvoiceSource = 'user' | 'admin_history'

export type InvoiceProfile = {
  company_name: string
  tax_id: string
  email: string
}

export type InvoiceConfig = {
  enabled: boolean
  min_amount_cents: number
  last_invoice_profile: InvoiceProfile
}

export type InvoiceEligibleOrder = {
  top_up_id: number
  trade_no: string
  payment_method: string
  amount_cents: number
  create_time: number
  complete_time: number
}

export type InvoiceRequestItem = {
  id: number
  request_id: number
  top_up_id: number
  trade_no: string
  payment_method: string
  amount_cents: number
  create_time: number
  complete_time: number
}

export type InvoiceRequest = {
  id: number
  user_id: number
  username: string
  display_name: string
  source: InvoiceSource
  status: InvoiceStatus
  company_name: string
  tax_id: string
  email: string
  total_amount_cents: number
  create_time: number
  processed_time: number
  operator_id: number
  operator_username: string
  reject_reason: string
  note: string
  items: InvoiceRequestItem[]
}

export type InvoicePage<T> = {
  page: number
  page_size: number
  total: number
  items: T[]
}

export type CreateInvoicePayload = {
  order_ids: number[]
  company_name: string
  tax_id: string
  email: string
}

export type HistoricalInvoicePayload = {
  user_id: number
  order_ids: number[]
  note: string
}
