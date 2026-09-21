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
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, expect, test, vi } from 'vitest'

import { imageRequest } from '../api'
import { ImageRuntimeForm } from '../settings'

vi.mock('../api', () => ({ imageRequest: vi.fn() }))

afterEach(() => {
  vi.clearAllMocks()
})

test('edits the global reference-image limit in the normal runtime form', async () => {
  vi.mocked(imageRequest).mockResolvedValue(undefined)
  const onSuccess = vi.fn().mockResolvedValue(undefined)
  const runtime = {
    async_enabled: true,
    auto_archive_to_library: false,
    image_circuit_breaker_enabled: true,
    prompt_preview_enabled: true,
    worker_concurrency: 4,
    image_concurrency: 4,
    max_reference_images: 14,
    worker_lease_seconds: 120,
    execution_timeout_seconds: 1200,
    account_attempt_timeout_seconds: 300,
    signed_url_expiry_seconds: 3600,
    input_retention_hours: 24,
    task_retention_days: 90,
    result_retention_days: 90,
    reference_fetch_max_retries: 2,
    storage_retry_attempts: 5,
    billing_retry_attempts: 10,
    openai_reference_transport_mode: 'passthrough_fallback_local',
    gemini_reference_transport_mode: 'passthrough_fallback_local',
  }

  render(<ImageRuntimeForm initial={runtime} onSuccess={onSuccess} />)
  const input = screen.getByRole('spinbutton', {
    name: 'Maximum reference images',
  })
  expect(input).toHaveValue(14)
  expect(input).toHaveAttribute('max', '128')

  const user = userEvent.setup()
  await user.clear(input)
  await user.type(input, '12')
  await user.click(screen.getByRole('button', { name: 'Save' }))

  await waitFor(() =>
    expect(imageRequest).toHaveBeenCalledWith(
      '/api/option/images/runtime',
      'PUT',
      { ...runtime, max_reference_images: 12 }
    )
  )
  expect(onSuccess).toHaveBeenCalledOnce()
})
