/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.
*/
import { z } from 'zod'

export const invoiceApplicationSchema = z.object({
  company_name: z
    .string()
    .trim()
    .min(1, 'Company name is required')
    .max(200, 'Company name must be 200 characters or fewer'),
  tax_id: z
    .string()
    .trim()
    .min(1, 'Tax ID is required')
    .max(64, 'Tax ID must be 64 characters or fewer'),
  email: z
    .string()
    .trim()
    .min(1, 'Email is required')
    .email('Enter a valid email address')
    .max(254, 'Email must be 254 characters or fewer'),
})

export type InvoiceApplicationValues = z.infer<typeof invoiceApplicationSchema>
