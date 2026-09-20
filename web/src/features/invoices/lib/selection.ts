/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.
*/
import type { RowSelectionState } from '@tanstack/react-table'

import type { InvoiceEligibleOrder } from '../types'

export function reconcileInvoiceOrderSelection(
  selectedOrders: ReadonlyMap<number, InvoiceEligibleOrder>,
  visibleOrders: InvoiceEligibleOrder[],
  rowSelection: RowSelectionState
): Map<number, InvoiceEligibleOrder> {
  const nextOrders = new Map(selectedOrders)
  for (const order of visibleOrders) {
    if (rowSelection[String(order.top_up_id)]) {
      nextOrders.set(order.top_up_id, order)
    } else {
      nextOrders.delete(order.top_up_id)
    }
  }
  return nextOrders
}
