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

const runtime = {
  revision: 0,
  async_enabled: true,
  mixed_timeout_retry_enabled: false,
  failed_request_retention_hours: 24,
  account_cooldown_seconds: 60,
  supplier_retry_capacity: 10,
  supplier_retry_window_seconds: 60,
  auto_archive_to_library: false,
  worker_concurrency: 4,
  worker_lease_seconds: 120,
  recovery_interval_seconds: 30,
  execution_timeout_seconds: 1200,
  account_attempt_timeout_seconds: 300,
  image_concurrency: 4,
  storage_retry_attempts: 5,
  billing_retry_attempts: 10,
  retry_backoff_seconds: 30,
  openai_reference_transport_mode: 'passthrough_fallback_local',
  gemini_reference_transport_mode: 'passthrough_fallback_local',
  gemini_async_max_account_switches: 3,
  image_circuit_breaker_enabled: true,
  failure_threshold: 5,
  cooldown_seconds: 300,
  reference_fetch_max_retries: 2,
  reference_retry_base_seconds: 15,
  reference_retry_max_seconds: 60,
  upstream_transient_max_retries: 3,
  upstream_transient_retry_base_seconds: 15,
  upstream_transient_retry_max_seconds: 60,
  capacity_max_retries: 5,
  capacity_retry_base_seconds: 30,
  capacity_retry_max_seconds: 300,
  total_max_retries: 16,
  retry_jitter_percent: 20,
  retry_after_max_seconds: 900,
  download_max_bytes: 33554432,
  download_max_pixels: 80000000,
  max_reference_images: 14,
  download_timeout_seconds: 30,
  download_max_redirects: 3,
  reference_fetch_concurrency: 8,
  reference_cache_ttl_seconds: 60,
  reference_cache_max_bytes: 134217728,
  upload_timeout_seconds: 300,
  upload_per_minute: 20,
  max_input_bytes_per_key: 1073741824,
  max_upload_bytes: 33554432,
  signed_url_expiry_seconds: 3600,
  input_retention_hours: 24,
  task_retention_days: 90,
  result_retention_days: 90,
  prompt_preview_enabled: true,
  prompt_preview_max_chars: 160,
  library_retention_days: 90,
  library_max_items_per_user: 1000,
  library_max_bytes_per_user: 5368709120,
  library_max_image_bytes: 20971520,
  library_max_image_pixels: 40000000,
  library_import_per_minute: 20,
  library_submission_per_minute: 10,
}

test('keeps an image configuration draft on conflict and reloads only when requested', async () => {
  const conflict = Object.assign(new Error('stale configuration'), {
    isAxiosError: true,
    response: { status: 409 },
  })
  vi.mocked(imageRequest)
    .mockRejectedValueOnce(conflict)
    .mockResolvedValueOnce({
      runtime: { ...runtime, revision: 2, worker_concurrency: 8 },
    })
  const onSuccess = vi.fn()
  render(<ImageRuntimeForm initial={runtime} onSuccess={onSuccess} />)
  const user = userEvent.setup()
  const workers = screen.getByRole('spinbutton', { name: 'Workers' })
  await user.clear(workers)
  await user.type(workers, '6')
  await user.click(screen.getByRole('button', { name: 'Save' }))
  expect(await screen.findByRole('alert')).toHaveTextContent(
    'Settings changed since you opened this form. Your edits have been kept. Reload saved settings before editing again.'
  )
  expect(workers).toHaveValue(6)
  expect(onSuccess).not.toHaveBeenCalled()
  await user.click(
    screen.getByRole('button', { name: 'Reload saved settings' })
  )
  await waitFor(() => expect(workers).toHaveValue(8))
  expect(screen.queryByRole('alert')).not.toBeInTheDocument()
  vi.mocked(imageRequest).mockResolvedValueOnce({
    ...runtime,
    revision: 3,
    worker_concurrency: 8,
  })
  await user.click(screen.getByRole('button', { name: 'Save' }))
  await waitFor(() =>
    expect(imageRequest).toHaveBeenLastCalledWith(
      '/api/option/images/runtime',
      'PUT',
      { ...runtime, revision: 2, worker_concurrency: 8 }
    )
  )
})

test('edits the global reference-image limit in the normal runtime form', async () => {
  vi.mocked(imageRequest).mockResolvedValue(undefined)
  const onSuccess = vi.fn().mockResolvedValue(undefined)

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

test('saves disabled encrypted retention and bounded retry controls from the runtime form', async () => {
  vi.mocked(imageRequest).mockResolvedValue(undefined)
  render(<ImageRuntimeForm initial={runtime} onSuccess={vi.fn()} />)
  const user = userEvent.setup()
  const retention = screen.getByRole('spinbutton', {
    name: 'Failed request retention (hours)',
  })
  expect(retention).toHaveAttribute('max', '168')
  await user.clear(retention)
  await user.type(retention, '0')
  expect(
    screen.getByRole('spinbutton', { name: 'Account cooldown (seconds)' })
  ).toHaveValue(60)
  expect(
    screen.getByRole('spinbutton', { name: 'Supplier retry capacity' })
  ).toHaveValue(10)
  expect(
    screen.getByRole('spinbutton', { name: 'Supplier retry window (seconds)' })
  ).toHaveAttribute('min', '1')
  await user.click(screen.getByRole('button', { name: 'Save' }))
  await waitFor(() =>
    expect(imageRequest).toHaveBeenCalledWith(
      '/api/option/images/runtime',
      'PUT',
      { ...runtime, failed_request_retention_hours: 0 }
    )
  )
})

test('keeps mixed-pool timeout retries disabled when loading a legacy runtime configuration', async () => {
  vi.mocked(imageRequest).mockResolvedValue(undefined)
  const legacyRuntime: Record<string, string | number | boolean> = {
    ...runtime,
  }
  delete legacyRuntime.mixed_timeout_retry_enabled
  render(<ImageRuntimeForm initial={legacyRuntime} onSuccess={vi.fn()} />)
  const retrySwitch = screen.getByRole('switch', {
    name: 'Allow timeout retries in mixed image pools',
  })
  expect(retrySwitch).not.toBeChecked()
  expect(retrySwitch).toHaveAccessibleDescription(
    'Disabled by default. New mixed-pool attempts may retry within existing timeout limits, which can cause duplicate upstream generation and charges. Historical unknown tasks are not restarted.'
  )

  const user = userEvent.setup()
  await user.click(screen.getByRole('button', { name: 'Save' }))
  await waitFor(() =>
    expect(imageRequest).toHaveBeenCalledWith(
      '/api/option/images/runtime',
      'PUT',
      runtime
    )
  )
})

test.each([false, true])(
  'saves a keyboard change to mixed-pool timeout retries when initially %s',
  async (enabled) => {
    vi.mocked(imageRequest).mockResolvedValue(undefined)
    render(
      <ImageRuntimeForm
        initial={{ ...runtime, mixed_timeout_retry_enabled: enabled }}
        onSuccess={vi.fn()}
      />
    )
    const retrySwitch = screen.getByRole('switch', {
      name: 'Allow timeout retries in mixed image pools',
    })
    expect(retrySwitch).toHaveAttribute('aria-checked', String(enabled))
    const user = userEvent.setup()
    retrySwitch.focus()
    await user.keyboard(' ')
    expect(retrySwitch).toHaveAttribute('aria-checked', String(!enabled))
    await user.click(screen.getByRole('button', { name: 'Save' }))

    await waitFor(() =>
      expect(imageRequest).toHaveBeenCalledWith(
        '/api/option/images/runtime',
        'PUT',
        { ...runtime, mixed_timeout_retry_enabled: !enabled }
      )
    )
  }
)

test('explains every advanced runtime field outside the strict JSON document', async () => {
  const user = userEvent.setup()
  render(
    <ImageRuntimeForm
      initial={runtime}
      onSuccess={vi.fn().mockResolvedValue(undefined)}
    />
  )

  await user.click(
    screen.getByRole('button', { name: 'Advanced runtime settings' })
  )

  expect(
    screen.getByText(
      'Use strict JSON. Comments such as // or /* */ are not supported.'
    )
  ).toBeVisible()
  expect(
    screen.getByRole('textbox', { name: 'Complete runtime configuration' })
  ).toHaveAttribute('aria-describedby', 'image-runtime-advanced-help')

  await user.click(
    screen.getByRole('button', { name: 'Runtime field reference' })
  )
  for (const key of Object.keys(runtime)) {
    expect(screen.getByText(key)).toBeVisible()
  }
  expect(
    screen.getByText(
      'Maximum downloaded bytes allowed for each reference image.'
    )
  ).toBeVisible()
})
