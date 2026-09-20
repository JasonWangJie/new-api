/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.
*/
import type { Table } from '@tanstack/react-table'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'

import { DataTableBulkActions } from '../bulk-actions'

describe('DataTableBulkActions', () => {
  it('uses the external selection count and clear callback', async () => {
    const onClearSelection = vi.fn()
    const table = {
      getFilteredSelectedRowModel: () => ({ rows: [] }),
      resetRowSelection: vi.fn(),
    } as unknown as Table<{ id: number }>

    render(
      <DataTableBulkActions
        table={table}
        entityName='order'
        selectedCount={3}
        onClearSelection={onClearSelection}
      >
        <button type='button'>Process</button>
      </DataTableBulkActions>
    )

    expect(screen.getByText('3')).toBeInTheDocument()
    await userEvent.click(
      screen.getByRole('button', { name: 'Clear selection' })
    )
    expect(onClearSelection).toHaveBeenCalledOnce()
    expect(table.resetRowSelection).not.toHaveBeenCalled()
  })
})
