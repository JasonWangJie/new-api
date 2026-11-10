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
import { useState } from 'react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { api } from '@/lib/api'

import { SettingsPageProvider } from '../../components/settings-page-context'
import { MonitoringSettingsSection } from '../monitoring-settings-section'

const imageExclusionKey = 'perf_metrics_setting.image_error_exclusions'
const videoExclusionKey = 'perf_metrics_setting.video_error_exclusions'
const exclusionFields = [
  {
    key: imageExclusionKey,
    label: 'Async image error exclusions',
    saved: 'Previous image rule',
  },
  {
    key: videoExclusionKey,
    label: 'Async video error exclusions',
    saved: 'Previous video rule',
  },
]
const metricsQueryKeys = [['perf-metrics'], ['perf-metrics-summary']]
let client: QueryClient

function MonitoringFixture() {
  const [actionsContainer, setActionsContainer] =
    useState<HTMLDivElement | null>(null)
  return (
    <SettingsPageProvider actionsContainer={actionsContainer}>
      <div ref={setActionsContainer} />
      <MonitoringSettingsSection
        defaultValues={{
          QuotaRemindThreshold: '',
          'perf_metrics_setting.enabled': true,
          'perf_metrics_setting.flush_interval': 5,
          'perf_metrics_setting.bucket_time': 'hour',
          'perf_metrics_setting.retention_days': 0,
          [imageExclusionKey]: 'Previous image rule',
          [videoExclusionKey]: 'Previous video rule',
        }}
      />
    </SettingsPageProvider>
  )
}

function renderMonitoring() {
  const saveOption = vi
    .spyOn(api, 'put')
    .mockResolvedValue({ data: { success: true } })
  render(
    <QueryClientProvider client={client}>
      <MonitoringFixture />
    </QueryClientProvider>
  )
  return saveOption
}

beforeEach(() => {
  client = new QueryClient()
})

afterEach(() => {
  cleanup()
  client.clear()
  vi.restoreAllMocks()
})

describe.each(exclusionFields)('$label', ({ key, label, saved }) => {
  it('saves only the edited rule and refreshes metrics', async () => {
    const user = userEvent.setup()
    for (const queryKey of metricsQueryKeys) {
      client.setQueryData(queryKey, {})
    }
    const saveOption = renderMonitoring()
    const field = screen.getByRole('textbox', { name: label })
    expect(field).toHaveValue(saved)

    await user.clear(field)
    await user.paste('  Invalid input  \n\nInvalid input\ninvalid input  ')
    await user.click(screen.getByRole('button', { name: 'Save Changes' }))

    await waitFor(() => {
      expect(saveOption).toHaveBeenCalledExactlyOnceWith('/api/option/', {
        key,
        value: 'Invalid input\ninvalid input',
      })
      for (const queryKey of metricsQueryKeys) {
        expect(client.getQueryState(queryKey)?.isInvalidated).toBe(true)
      }
    })
  })

  it('clears the rule when the administrator submits a blank field', async () => {
    const user = userEvent.setup()
    const saveOption = renderMonitoring()
    await user.clear(screen.getByRole('textbox', { name: label }))
    await user.click(screen.getByRole('button', { name: 'Save Changes' }))

    await waitFor(() => {
      expect(saveOption).toHaveBeenCalledExactlyOnceWith('/api/option/', {
        key,
        value: '',
      })
    })
  })

  it.each([
    {
      limit: 'unique keywords',
      keywords: Array.from({ length: 41 }, (_, index) => `Error ${index}`).join(
        '\n'
      ),
    },
    { limit: 'characters per keyword', keywords: '界'.repeat(257) },
    {
      limit: 'total UTF-8 bytes',
      keywords: Array(22).fill('界'.repeat(256)).join('\n'),
    },
    { limit: 'control characters', keywords: 'Invalid\tinput' },
  ])('blocks invalid rules for $limit', async ({ keywords }) => {
    const user = userEvent.setup()
    const saveOption = renderMonitoring()
    const field = screen.getByRole('textbox', { name: label })
    await user.clear(field)
    await user.paste(keywords)
    await user.click(screen.getByRole('button', { name: 'Save Changes' }))

    await waitFor(() => expect(field).toHaveAttribute('aria-invalid', 'true'))
    expect(saveOption).not.toHaveBeenCalled()
  })
})
