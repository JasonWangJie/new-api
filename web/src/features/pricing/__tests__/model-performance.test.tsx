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
import { render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { createInstance } from 'i18next'
import { I18nextProvider } from 'react-i18next'
import { afterEach, beforeEach, expect, it, vi } from 'vitest'

import { api } from '@/lib/api'

import { ModelDetailsContent } from '../components/model-details'
import { ModelDetailsPerformance } from '../components/model-details-performance'
import { ModelPerformanceSummary } from '../components/model-performance-summary'
import type { PricingModel } from '../types'

vi.mock('@visactor/react-vchart', () => ({ VChart: () => null }))
vi.mock('@visactor/vchart', () => ({
  ThemeManager: { setCurrentTheme: vi.fn() },
}))

const model: PricingModel = {
  id: 1,
  model_name: 'example-model',
  quota_type: 0,
  model_ratio: 1,
  completion_ratio: 1,
  enable_groups: ['default'],
}

let client: QueryClient
beforeEach(() => {
  client = new QueryClient({
    defaultOptions: { queries: { retry: false, gcTime: 0 } },
  })
})
afterEach(() => {
  client.clear()
})

const groups = [
  {
    group: 'default',
    avg_ttft_ms: 100,
    avg_latency_ms: 1000,
    avg_tps: 40,
    success_rate: 100,
    series: [],
  },
  {
    group: 'premium',
    avg_ttft_ms: 200,
    avg_latency_ms: 2000,
    avg_tps: 20,
    success_rate: 0,
    series: [],
  },
]

it.each(['overview', 'performance'])(
  'uses the server model summary in the %s view instead of averaging group rates',
  async (view) => {
    client.setQueryData(['perf-metrics', model.model_name], {
      success: true,
      data: {
        model_name: model.model_name,
        groups: [
          {
            ...groups[0],
            video_filtered_success_rate: {
              rate: 100,
              excluded_count: 0,
              sample_count: 1,
            },
          },
          groups[1],
        ],
        video_error_exclusions: ['Reference file unavailable'],
        window_hours: 24,
        updated_at: 1794096000,
        summary: {
          workload: 'video',
          request_count: 100,
          success_count: 1,
          unknown_count: 2,
          output_count: 3,
          avg_ttft_ms: 0,
          avg_latency_ms: 120000,
          avg_tps: 0,
          success_rate: 1,
          video_filtered_success_rate: {
            rate: 1.02,
            excluded_count: 2,
            sample_count: 98,
          },
        },
      },
    })
    render(
      <QueryClientProvider client={client}>
        {view === 'performance' ? (
          <ModelDetailsPerformance model={model} />
        ) : (
          <ModelDetailsContent
            model={model}
            groupRatio={{}}
            usableGroup={{}}
            endpointMap={{}}
            autoGroups={[]}
            priceRate={1}
            usdExchangeRate={1}
            tokenUnit='M'
          />
        )}
      </QueryClientProvider>
    )
    const summary = screen.getByLabelText('Model performance summary')
    expect(within(summary).getByText('1.00%')).toBeVisible()
    expect(within(summary).getByText('1.02%')).toBeVisible()
    expect(
      screen.queryByRole('columnheader', { name: 'Samples' })
    ).not.toBeInTheDocument()
    await userEvent.setup().click(
      within(summary).getByRole('button', {
        name: 'Show excluded video errors',
      })
    )
    const popup = await screen.findByRole('dialog', {
      name: 'Filtered success rate',
    })
    expect(within(popup).getByText('Reference file unavailable')).toBeVisible()
    expect(within(popup).queryByText('Samples')).not.toBeInTheDocument()
    expect(
      within(popup).queryByText('Excluded failures')
    ).not.toBeInTheDocument()
    expect(within(summary).queryByText('3 images')).not.toBeInTheDocument()
    expect(within(summary).queryByText('100 samples')).not.toBeInTheDocument()
    expect(within(summary).queryByText('2 unknown')).not.toBeInTheDocument()
    expect(within(summary).getByText('Average delivery time')).toBeVisible()
    expect(within(summary).getByText('120.00s')).toBeVisible()
    expect(within(summary).queryByText('TPS')).not.toBeInTheDocument()
    expect(
      screen.queryByRole('columnheader', { name: 'Completed (24h)' })
    ).not.toBeInTheDocument()
  }
)

it('shows separate filtered columns for mixed models and a placeholder when all image observations are excluded', () => {
  const summary = {
    workload: 'mixed',
    request_count: 3,
    avg_latency_ms: 1000,
    avg_tps: 0,
    success_rate: 33.33,
    image_filtered_success_rate: {
      rate: null,
      excluded_count: 1,
      sample_count: 0,
    },
    video_filtered_success_rate: {
      rate: 100,
      excluded_count: 1,
      sample_count: 1,
    },
  }
  client.setQueryData(['perf-metrics', model.model_name], {
    success: true,
    data: {
      model_name: model.model_name,
      image_error_exclusions: ['Image source unavailable'],
      video_error_exclusions: ['Video source unavailable'],
      summary,
      groups: [{ ...groups[0], ...summary }],
    },
  })
  render(
    <QueryClientProvider client={client}>
      <ModelDetailsPerformance model={model} />
    </QueryClientProvider>
  )
  const table = within(screen.getByRole('table'))
  expect(
    table.getByRole('columnheader', { name: 'Image filtered success rate' })
  ).toBeVisible()
  expect(
    table.getByRole('columnheader', { name: 'Video filtered success rate' })
  ).toBeVisible()
  expect(table.getByRole('cell', { name: '—' })).toBeVisible()
  expect(table.getByRole('cell', { name: '100.00%' })).toBeVisible()
  expect(table.queryByRole('columnheader', { name: 'Samples' })).toBeNull()
})

it('shows an unavailable filtered rate when all video observations are excluded', () => {
  render(
    <ModelPerformanceSummary
      videoErrorExclusions={['Reference file unavailable']}
      stats={{
        workload: 'video',
        request_count: 3,
        avg_latency_ms: 0,
        avg_tps: 0,
        success_rate: 0,
        video_filtered_success_rate: {
          rate: null,
          excluded_count: 3,
          sample_count: 0,
        },
      }}
    />
  )
  const summary = screen.getByLabelText('Model performance summary')
  expect(within(summary).getByText('0.00%')).toBeVisible()
  expect(within(summary).getByText('Filtered success rate')).toBeVisible()
  expect(within(summary).getAllByText('—')).toHaveLength(2)
})

it('keeps media labels without inventing overall metrics when an older response has multiple groups without a model summary', () => {
  client.setQueryData(['perf-metrics', model.model_name], {
    success: true,
    data: { model_name: model.model_name, groups },
  })
  render(
    <QueryClientProvider client={client}>
      <ModelDetailsPerformance
        model={{ ...model, supported_endpoint_types: ['image-generation'] }}
      />
    </QueryClientProvider>
  )
  const summary = screen.getByLabelText('Model performance summary')
  expect(within(summary).queryByText('50.00%')).not.toBeInTheDocument()
  expect(within(summary).getAllByText('—')).toHaveLength(2)
  expect(within(summary).queryByText('Completed (24h)')).not.toBeInTheDocument()
  expect(within(summary).queryByText('TPS')).not.toBeInTheDocument()
})

it('keeps a valid subsecond media observation in the latency trend', () => {
  const stats = {
    workload: 'image',
    request_count: 1,
    success_count: 1,
    latency_count: 1,
    output_count: 1,
    avg_ttft_ms: 0,
    avg_latency_ms: 0,
    success_rate: 100,
    avg_tps: 0,
  }
  client.setQueryData(['perf-metrics', model.model_name], {
    success: true,
    data: {
      model_name: model.model_name,
      summary: stats,
      groups: [
        { ...stats, group: 'default', series: [{ ...stats, ts: 1794096000 }] },
      ],
    },
  })
  render(
    <QueryClientProvider client={client}>
      <ModelDetailsPerformance model={model} />
    </QueryClientProvider>
  )
  expect(screen.getAllByText('<1s')).toHaveLength(2)
  expect(screen.queryAllByText('Limited samples')).toHaveLength(0)
  expect(
    screen.queryByText('No latency data available')
  ).not.toBeInTheDocument()
})

it('keeps media metric labels and neutral values when the model has zero samples', () => {
  render(
    <ModelPerformanceSummary
      stats={{
        workload: 'image',
        request_count: 0,
        output_count: 0,
        avg_latency_ms: 0,
        avg_tps: 0,
        success_rate: 0,
      }}
    />
  )
  const summary = screen.getByLabelText('Model performance summary')
  expect(within(summary).queryByText('Completed (24h)')).not.toBeInTheDocument()
  expect(within(summary).getAllByText('—')).toHaveLength(2)
  expect(within(summary).queryByText('0 samples')).not.toBeInTheDocument()
  expect(within(summary).queryByText('TPS')).not.toBeInTheDocument()
})

it.each([
  { language: 'zhCN', locale: 'zh-CN' },
  { language: 'zhTW', locale: 'zh-TW' },
])(
  'formats the query update time for project language $language without a locale error',
  async ({ language, locale }) => {
    const i18n = createInstance()
    await i18n.init({
      lng: language,
      fallbackLng: 'en',
      interpolation: { escapeValue: false },
      resources: {
        [language]: { translation: { 'Updated {{time}}': 'Updated {{time}}' } },
        en: { translation: {} },
      },
    })
    render(
      <I18nextProvider i18n={i18n}>
        <ModelPerformanceSummary updatedAt={1794096000} />
      </I18nextProvider>
    )
    expect(
      screen.getByText(
        `Updated ${new Date(1794096000 * 1000).toLocaleString(locale)}`,
        { collapseWhitespace: false }
      )
    ).toBeVisible()
  }
)

it('distinguishes a metrics failure from missing samples and lets the user retry', async () => {
  vi.spyOn(api, 'get')
    .mockRejectedValueOnce(new Error('metrics unavailable'))
    .mockResolvedValue({
      data: {
        success: true,
        data: { model_name: model.model_name, groups: [] },
      },
    })
  render(
    <QueryClientProvider client={client}>
      <ModelDetailsPerformance model={model} />
    </QueryClientProvider>
  )
  expect(
    await screen.findByText('Performance metrics unavailable')
  ).toBeVisible()
  expect(
    screen.queryByText('Performance data is not yet available for this model.')
  ).not.toBeInTheDocument()
  await userEvent.setup().click(screen.getByRole('button', { name: 'Retry' }))
  expect(
    await screen.findByText(
      'Performance data is not yet available for this model.'
    )
  ).toBeVisible()
})
