/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.
*/
import { api } from '@/lib/api'
import { requireServerSuccess } from '@/lib/server-error-message'

import type {
  CreateInvoicePayload,
  HistoricalInvoicePayload,
  InvoiceConfig,
  InvoiceEligibleOrder,
  InvoicePage,
  InvoiceRequest,
  InvoiceStatus,
} from './types'

type ServerResponse<T> = {
  success: boolean
  message?: string
  data: T
}

async function invoiceRequest<T>(
  url: string,
  method = 'GET',
  data?: unknown,
  params?: Record<string, string | number | undefined>
): Promise<T> {
  const response = await api.request<ServerResponse<T>>({
    url,
    method,
    data,
    params,
  })
  requireServerSuccess(response.data)
  return response.data.data
}

export const getInvoiceConfig = () =>
  invoiceRequest<InvoiceConfig>('/api/invoice/config')

export const getEligibleInvoiceOrders = (params: {
  page: number
  pageSize: number
  keyword: string
  adminUserId?: number
}) =>
  invoiceRequest<InvoicePage<InvoiceEligibleOrder>>(
    params.adminUserId ? '/api/invoice/admin/orders' : '/api/invoice/orders',
    'GET',
    undefined,
    {
      p: params.page,
      page_size: params.pageSize,
      keyword: params.keyword || undefined,
      user_id: params.adminUserId,
    }
  )

export const getInvoiceRequests = (params: {
  page: number
  pageSize: number
  status?: InvoiceStatus | ''
  keyword?: string
  admin?: boolean
}) =>
  invoiceRequest<InvoicePage<InvoiceRequest>>(
    params.admin ? '/api/invoice/admin/requests' : '/api/invoice/requests',
    'GET',
    undefined,
    {
      p: params.page,
      page_size: params.pageSize,
      status: params.status || undefined,
      keyword: params.keyword || undefined,
    }
  )

export const createInvoice = (payload: CreateInvoicePayload) =>
  invoiceRequest<InvoiceRequest>('/api/invoice/requests', 'POST', payload)

export const completeInvoice = (id: number) =>
  invoiceRequest<InvoiceRequest>(
    `/api/invoice/admin/requests/${id}/complete`,
    'POST'
  )

export const rejectInvoice = (id: number, reason: string) =>
  invoiceRequest<InvoiceRequest>(
    `/api/invoice/admin/requests/${id}/reject`,
    'POST',
    { reason }
  )

export const createHistoricalInvoice = (payload: HistoricalInvoicePayload) =>
  invoiceRequest<InvoiceRequest>('/api/invoice/admin/history', 'POST', payload)
