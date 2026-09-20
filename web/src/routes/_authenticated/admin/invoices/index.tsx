/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.
*/
import { createFileRoute, redirect } from '@tanstack/react-router'

import { AdminInvoicesPage } from '@/features/invoices/admin-page'
import { hasPermission } from '@/lib/admin-permissions'
import { useAuthStore } from '@/stores/auth-store'

export const Route = createFileRoute('/_authenticated/admin/invoices/')({
  beforeLoad: () => {
    if (!hasPermission(useAuthStore.getState().auth.user, 'invoice', 'read')) {
      throw redirect({ to: '/403' })
    }
  },
  component: AdminInvoicesPage,
})
