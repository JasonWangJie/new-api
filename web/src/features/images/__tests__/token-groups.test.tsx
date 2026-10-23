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
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { cleanup, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, expect, test, vi } from 'vitest'

import { imageRequest } from '../api'
import { TokenImageGroups } from '../token-groups'

vi.mock('../api', () => ({ imageRequest: vi.fn() }))

afterEach(() => {
  cleanup()
  vi.clearAllMocks()
})

test('saving a mixed group preserves the existing platform mappings', async () => {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  vi.mocked(imageRequest).mockResolvedValue({
    openai: null,
    gemini: 'premium',
    mixed: null,
  })
  const user = userEvent.setup()
  render(
    <QueryClientProvider client={client}>
      <TokenImageGroups
        tokenId={7}
        groups={[{ value: 'premium', label: 'Premium' }]}
      />
    </QueryClientProvider>
  )
  try {
    await user.click(
      await screen.findByRole('combobox', { name: 'Mixed scheduling' })
    )
    await user.click(screen.getByRole('option', { name: 'Premium' }))
    await user.click(screen.getByRole('button', { name: 'Save image groups' }))
    await waitFor(() =>
      expect(imageRequest).toHaveBeenCalledWith(
        '/api/token/7/image-platform-groups',
        'PATCH',
        { openai: null, gemini: 'premium', mixed: 'premium' }
      )
    )
  } finally {
    client.clear()
  }
})
