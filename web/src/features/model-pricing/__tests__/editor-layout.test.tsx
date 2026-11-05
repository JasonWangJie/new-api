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
import {
  cleanup,
  render,
  screen,
  waitFor,
  within,
} from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { useState } from 'react'
import { useForm } from 'react-hook-form'
import { afterEach, expect, it, vi } from 'vitest'

import { ModelPriceCell } from '@/features/pricing/components/model-price-cell'
import { usePricingData } from '@/features/pricing/hooks/use-pricing-data'
import { evaluateBillingExpression } from '@/features/pricing/lib/billing-expression/runtime'
import { SettingsPageProvider } from '@/features/system-settings/components/settings-page-context'
import { ModelPricingEditorPanel } from '@/features/system-settings/models/model-pricing-sheet'
import { ModelRatioForm } from '@/features/system-settings/models/model-ratio-form'
import { api } from '@/lib/api'
import { useAuthStore } from '@/stores/auth-store'
import { usePricingPreferencesStore } from '@/stores/pricing-preferences-store'

import { ModelPricingPanel } from '../model-pricing-panel'
import type { PricingValues } from '../pricing'

const clients: QueryClient[] = []

it('refreshes an already mounted public price after saving mixed-price inheritance', async () => {
  useAuthStore
    .getState()
    .auth.setUser({ id: 1, username: 'administrator', role: 100 })
  let configured: PricingValues = {
    'billing_setting.billing_mode': 'tiered_expr',
    'billing_setting.billing_expr': 'tier("base", u("requests") * 3)',
    'billing_setting.mixed_video_billing_expr':
      'tier("mixed", u("requests") * 7)',
  }
  const usageSchema = { requests: { type: 'number', unit: 'count' } }
  vi.spyOn(api, 'get').mockImplementation(async (url) => {
    if (url === '/api/status') {
      return {
        data: { success: true, data: { price: 1, usd_exchange_rate: 1 } },
      }
    }
    const mixedExpression =
      configured['billing_setting.mixed_video_billing_expr'] ??
      configured['billing_setting.billing_expr']
    if (url === '/api/pricing') {
      return {
        data: {
          success: true,
          vendors: [],
          group_ratio: { default: 1 },
          data: [
            {
              id: 1,
              model_name: 'video',
              quota_type: 0,
              model_ratio: 1,
              completion_ratio: 1,
              enable_groups: ['default'],
              mixed_video_pricing: {
                groups: ['default'],
                enabled_groups: ['default'],
                source: 'override',
                billing_mode: 'tiered_expr',
                billing_expr: mixedExpression,
                billing_usage_schema: usageSchema,
              },
            },
          ],
        },
      }
    }
    expect(url).toBe('/api/option/model_pricing')
    return {
      data: {
        success: true,
        data: {
          entries: [
            {
              model_name: 'video',
              version: 'v1',
              configured,
              effective: configured,
              mixed_video: {
                eligible: true,
                valid: true,
                source: 'override',
                expression: mixedExpression,
                usage_schema: usageSchema,
              },
            },
          ],
        },
      },
    }
  })
  vi.spyOn(api, 'patch').mockImplementation(async (_url, body) => {
    configured = (body as { changes: { pricing: PricingValues }[] })
      .changes[0].pricing
    return { data: { success: true } }
  })
  function PublicPrice() {
    const pricing = usePricingData()
    return (
      <section aria-label='Public model price'>
        {pricing.models[0] && (
          <ModelPriceCell
            model={pricing.models[0]}
            options={{ selectedGroup: 'default' }}
          />
        )}
      </section>
    )
  }
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
  clients.push(client)
  render(
    <QueryClientProvider client={client}>
      <PublicPrice />
      <ModelPricingPanel modelName='video' />
    </QueryClientProvider>
  )
  const publicPrice = screen.getByRole('region', { name: 'Public model price' })
  await waitFor(() => expect(publicPrice).toHaveTextContent('7/unit'))
  const user = userEvent.setup()
  await user.click(screen.getByRole('button', { name: 'Use model pricing' }))
  await user.click(screen.getByRole('button', { name: 'Save mixed video price' }))
  await waitFor(() => expect(publicPrice).toHaveTextContent('3/unit'))
  expect(publicPrice).not.toHaveTextContent('7/unit')
})

it('removes a mixed video override through the single-model versioned save without changing provider prices', async () => {
  useAuthStore
    .getState()
    .auth.setUser({ id: 1, username: 'administrator', role: 100 })
  const configured = {
    'billing_setting.billing_mode': 'tiered_expr',
    'billing_setting.billing_expr': 'u("seconds")',
    'billing_setting.plugin_billing_expr': { wolong: 'u("video_count")' },
    'billing_setting.mixed_video_billing_expr': 'u("requests")',
  }
  vi.spyOn(api, 'get').mockResolvedValue({
    data: {
      success: true,
      data: {
        entries: [
          {
            model_name: 'video',
            version: 'v1',
            configured,
            effective: configured,
            mixed_video: {
              eligible: true,
              source: 'override',
              expression: 'u("requests")',
              usage_schema: { requests: { type: 'number', unit: 'count' } },
            },
          },
        ],
      },
    },
  })
  const patch = vi
    .spyOn(api, 'patch')
    .mockResolvedValue({ data: { success: true } })
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
  clients.push(client)
  render(
    <QueryClientProvider client={client}>
      <ModelPricingPanel modelName='video' />
    </QueryClientProvider>
  )
  const user = userEvent.setup()
  await user.click(
    await screen.findByRole('button', { name: 'Use model pricing' })
  )
  await user.click(
    screen.getByRole('button', { name: 'Save mixed video price' })
  )
  await waitFor(() =>
    expect(patch).toHaveBeenCalledWith('/api/option/model_pricing', {
      changes: [
        {
          model_name: 'video',
          expected_version: 'v1',
          pricing: {
            'billing_setting.billing_mode': 'tiered_expr',
            'billing_setting.billing_expr': 'u("seconds")',
            'billing_setting.plugin_billing_expr': {
              wolong: 'u("video_count")',
            },
          },
        },
      ],
    })
  )
})

it('previews the model price when switching from a mixed override to inheritance', async () => {
  useAuthStore
    .getState()
    .auth.setUser({ id: 1, username: 'administrator', role: 100 })
  const configured = {
    'billing_setting.mixed_video_billing_expr':
      'tier("old", u("seconds") * 0.6)',
  }
  vi.spyOn(api, 'get').mockResolvedValue({
    data: {
      success: true,
      data: {
        entries: [
          {
            model_name: 'video',
            version: 'v1',
            configured,
            effective: {
              'billing_setting.billing_mode': 'tiered_expr',
              'billing_setting.billing_expr':
                'tier("base", u("seconds") * 9.2)',
            },
            mixed_video: {
              eligible: true,
              valid: true,
              source: 'override',
              expression:
                configured['billing_setting.mixed_video_billing_expr'],
              usage_schema: { seconds: { type: 'number', unit: 'second' } },
            },
          },
        ],
      },
    },
  })
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  clients.push(client)
  render(
    <QueryClientProvider client={client}>
      <ModelPricingPanel modelName='video' />
    </QueryClientProvider>
  )
  const user = userEvent.setup()
  await user.click(
    await screen.findByRole('button', { name: 'Use model pricing' })
  )
  const section = screen.getByRole('region', {
    name: 'Mixed video public price',
  })
  expect(section).toHaveTextContent('9.2')
  expect(section).not.toHaveTextContent('0.6')
})

it('keeps a mixed price draft after a version conflict and offers confirmed reload', async () => {
  useAuthStore
    .getState()
    .auth.setUser({ id: 1, username: 'administrator', role: 100 })
  const configured = {
    'billing_setting.mixed_video_billing_expr':
      'tier("old", u("requests") * 2)',
  }
  const entry = {
    model_name: 'video',
    version: 'v1',
    configured,
    effective: {},
    mixed_video: {
      eligible: true,
      valid: true,
      source: 'override',
      expression: configured['billing_setting.mixed_video_billing_expr'],
      usage_schema: { requests: { type: 'number', unit: 'count' } },
    },
  }
  vi.spyOn(api, 'get').mockResolvedValue({
    data: { success: true, data: { entries: [entry] } },
  })
  vi.spyOn(api, 'patch').mockResolvedValue({
    data: { success: false, message: 'Price changed' },
  })
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
  clients.push(client)
  render(
    <QueryClientProvider client={client}>
      <ModelPricingPanel modelName='video' />
    </QueryClientProvider>
  )
  const user = userEvent.setup()
  await user.click(
    await screen.findByRole('button', { name: 'Use model pricing' })
  )
  await user.click(
    screen.getByRole('button', { name: 'Save mixed video price' })
  )
  expect(await screen.findByText('Price changed')).toHaveAttribute(
    'role',
    'alert'
  )
  expect(screen.getByText('No mixed video price configured')).toBeVisible()
  await user.click(screen.getByRole('button', { name: 'Reload pricing' }))
  const dialog = screen.getByRole('alertdialog')
  await user.click(
    within(dialog).getByRole('button', { name: 'Reload pricing' })
  )
  expect(
    await screen.findByRole('button', { name: 'Custom mixed video price' })
  ).toBeVisible()
})

it('copies a compatible provider price while excluding request rules and saves the expression unchanged', async () => {
  useAuthStore
    .getState()
    .auth.setUser({ id: 1, username: 'administrator', role: 100 })
  const copied =
    'tier("seconds", u("seconds") * 0.6) * (u("video_input") == "video" ? 1.65 : 1.45)'
  const schema = {
    seconds: { type: 'number', unit: 'second' },
    video_input: { enum: ['none', 'video'] },
  }
  const configured = {
    'billing_setting.mixed_video_billing_expr':
      'tier("old", u("seconds") * 0.3)',
  }
  const entry = {
    model_name: 'video',
    version: 'v1',
    configured,
    effective: {},
    mixed_video: {
      eligible: true,
      valid: true,
      source: 'override',
      expression: configured['billing_setting.mixed_video_billing_expr'],
      usage_schema: schema,
    },
    plugin_variants: [
      {
        plugin_key: 'drama',
        plugin_name: 'Drama',
        effective: copied,
        usage_schema: schema,
      },
      {
        plugin_key: 'rules',
        plugin_name: 'Request rules',
        effective: 'u("seconds") * (header("x") == "a" ? 2 : 1)',
        usage_schema: schema,
      },
      {
        plugin_key: 'dynamic',
        plugin_name: 'Dynamic usage key',
        effective: 'u("sec" + "onds")',
        usage_schema: schema,
      },
    ],
  }
  vi.spyOn(api, 'get').mockResolvedValue({
    data: { success: true, data: { entries: [entry] } },
  })
  const patch = vi
    .spyOn(api, 'patch')
    .mockResolvedValue({ data: { success: true } })
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
  clients.push(client)
  render(
    <QueryClientProvider client={client}>
      <ModelPricingPanel modelName='video' />
    </QueryClientProvider>
  )
  const user = userEvent.setup()
  await user.click(
    await screen.findByRole('button', { name: 'Load existing prices' })
  )
  await user.click(
    await screen.findByRole('combobox', { name: 'Price source' })
  )
  expect(
    screen.queryByRole('option', { name: 'Request rules' })
  ).not.toBeInTheDocument()
  expect(
    screen.queryByRole('option', { name: 'Dynamic usage key' })
  ).not.toBeInTheDocument()
  await user.click(screen.getByRole('option', { name: 'Drama' }))
  await user.click(screen.getByRole('button', { name: 'Copy existing price' }))
  expect(patch).not.toHaveBeenCalled()
  await user.click(
    screen.getByRole('button', { name: 'Save mixed video price' })
  )
  await waitFor(() =>
    expect(patch).toHaveBeenCalledWith('/api/option/model_pricing', {
      changes: [
        {
          model_name: 'video',
          expected_version: 'v1',
          pricing: { 'billing_setting.mixed_video_billing_expr': copied },
        },
      ],
    })
  )
})

it('edits reference multipliers without adding an unused seconds meter', async () => {
  useAuthStore
    .getState()
    .auth.setUser({ id: 1, username: 'administrator', role: 100 })
  const expression =
    '(u("resolution") == "720p" ? tier("720p", u("requests") * 6) : tier("1080p", u("requests") * 9)) * (u("video_input") == "video" ? 1.5 : 1.25)'
  const configured = { 'billing_setting.mixed_video_billing_expr': expression }
  vi.spyOn(api, 'get').mockResolvedValue({
    data: {
      success: true,
      data: {
        entries: [
          {
            model_name: 'video',
            version: 'v1',
            configured,
            effective: {},
            mixed_video: {
              eligible: true,
              valid: true,
              source: 'override',
              expression,
              usage_schema: {
                requests: { type: 'number', unit: 'count' },
                seconds: { type: 'number', unit: 'second' },
                resolution: { enum: ['720p', '1080p'] },
                video_input: { enum: ['none', 'video'] },
              },
            },
          },
        ],
      },
    },
  })
  const patch = vi
    .spyOn(api, 'patch')
    .mockResolvedValue({ data: { success: true } })
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
  clients.push(client)
  render(
    <QueryClientProvider client={client}>
      <ModelPricingPanel modelName='video' />
    </QueryClientProvider>
  )
  const user = userEvent.setup()
  const multiplier = await screen.findByRole('textbox', {
    name: 'With reference video multiplier',
  })
  await user.clear(multiplier)
  await user.type(multiplier, '1.8')
  await user.click(
    screen.getByRole('button', { name: 'Save mixed video price' })
  )
  await waitFor(() => expect(patch).toHaveBeenCalled())
  const body = patch.mock.calls[0][1] as {
    changes: { pricing: PricingValues }[]
  }
  const saved =
    body.changes[0].pricing['billing_setting.mixed_video_billing_expr'] ?? ''
  expect(saved).not.toContain('u("seconds")')
  for (const [resolution, video_input, cost] of [
    ['720p', 'none', 7.5],
    ['720p', 'video', 10.8],
    ['1080p', 'none', 11.25],
    ['1080p', 'video', 16.2],
  ] as const) {
    const result = evaluateBillingExpression(saved, {
      usage: { requests: 1, resolution, video_input },
    })
    expect(result.status).toBe('success')
    if (result.status === 'success') expect(result.cost).toBeCloseTo(cost)
  }
})
const originalColumnVisibility = localStorage.getItem(
  'model-ratio-column-visibility'
)

afterEach(() => {
  cleanup()
  for (const client of clients) client.clear()
  clients.length = 0
  useAuthStore.getState().auth.setUser(null)
  if (originalColumnVisibility === null) {
    localStorage.removeItem('model-ratio-column-visibility')
  } else {
    localStorage.setItem(
      'model-ratio-column-visibility',
      originalColumnVisibility
    )
  }
})

it.each(['none', 'standard', 'claude_ttl'] as const)(
  'uses the %s cache profile consistently in current billing and draft previews',
  async (mode) => {
    const cacheWriteMode = mode
    const effective = {
      ModelRatio: 2,
      CompletionRatio: 4,
      CacheRatio: 1,
      CreateCacheRatio: 1.25,
      ImageRatio: 1,
    }
    const configured = { ModelRatio: 2 }
    useAuthStore
      .getState()
      .auth.setUser({ id: 1, username: 'administrator', role: 100 })
    usePricingPreferencesStore.setState({ currency: 'USD' })
    vi.spyOn(api, 'get').mockImplementation(async (url) => ({
      data: {
        success: true,
        data:
          url === '/api/option/model_pricing'
            ? {
                entries: [
                  {
                    model_name: 'gpt-4o-custom',
                    version: 'v1',
                    configured,
                    effective,
                    cache_write_mode: cacheWriteMode,
                  },
                ],
              }
            : [],
        vendors: [],
      },
    }))
    vi.spyOn(api, 'post').mockResolvedValue({
      data: {
        success: true,
        data: { effective, cache_write_mode: cacheWriteMode },
      },
    })
    const client = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    })
    clients.push(client)
    render(
      <QueryClientProvider client={client}>
        <ModelPricingPanel modelName='gpt-4o-custom' />
      </QueryClientProvider>
    )
    const billing = await screen.findByRole('region', {
      name: 'Current Billing',
    })
    expect(within(billing).getByText('16')).toBeVisible()
    expect(within(billing).queryByText('Image input')).not.toBeInTheDocument()
    const preview = screen.getByRole('complementary', { name: 'Preview' })
    expect(await within(preview).findByText('$16')).toBeVisible()
    expect(
      within(preview).queryByText('Image input price')
    ).not.toBeInTheDocument()
    expect(
      within(preview).queryByText('Cache read price')
    ).not.toBeInTheDocument()
    expect(
      within(billing).queryByText('Cache read price')
    ).not.toBeInTheDocument()
    for (const panel of [billing, preview]) {
      if (mode === 'none') {
        expect(
          within(panel).queryByText('Cache write price')
        ).not.toBeInTheDocument()
        expect(
          within(panel).queryByText('Cache create (1h) price')
        ).not.toBeInTheDocument()
      } else {
        expect(
          within(panel).getByText(
            mode === 'claude_ttl' ? 'Cache Creation (5m)' : 'Cache write price'
          ).nextElementSibling
        ).toHaveTextContent('$5')
        if (mode === 'claude_ttl') {
          expect(
            within(panel).getByText('Cache create (1h) price')
              .nextElementSibling
          ).toHaveTextContent('$8')
        } else {
          expect(
            within(panel).queryByText('Cache create (1h) price')
          ).not.toBeInTheDocument()
        }
      }
    }
    expect(
      screen.getByRole('textbox', { name: 'Completion price' })
    ).toBeDisabled()
    expect(
      screen.getByRole('textbox', { name: 'Cache read price' })
    ).toBeDisabled()
  }
)

function renderEditor(embedded = false) {
  vi.spyOn(api, 'get').mockResolvedValue({
    data: { success: true, data: [], vendors: [] },
  })
  vi.spyOn(api, 'post').mockImplementation(async (_url, request) => {
    const pricing = (request as { pricing: PricingValues }).pricing
    return {
      data: {
        success: true,
        data: {
          effective: {
            ...pricing,
            CompletionRatio: pricing.CompletionRatio ?? 1,
            CacheRatio: pricing.CacheRatio ?? 1,
            CreateCacheRatio: pricing.CreateCacheRatio ?? 1.25,
            ImageRatio: pricing.ImageRatio ?? 1,
          },
        },
      },
    }
  })
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  })
  clients.push(client)
  render(
    <QueryClientProvider client={client}>
      <ModelPricingEditorPanel
        embedded={embedded}
        editData={{
          name: 'example-model',
          billingMode: 'per-token',
          ratio: '3.25',
          completionRatio: '2',
          cacheRatio: '0.2',
        }}
        onSave={() => {}}
      />
    </QueryClientProvider>
  )
}

it('keeps the preview expanded and the save action outside the scrolling embedded form', () => {
  renderEditor(true)
  const scrollRegion = screen.getByRole('region', {
    name: 'Edit model pricing',
  })
  expect(scrollRegion).toHaveClass(
    'overflow-y-auto',
    'min-h-0',
    '@container/pricing-editor'
  )
  expect(
    within(scrollRegion).getByRole('complementary', { name: 'Preview' })
  ).toBeVisible()
  expect(
    within(scrollRegion).queryByRole('button', { name: 'Save model prices' })
  ).not.toBeInTheDocument()
  expect(
    screen.getByRole('button', { name: 'Save model prices' })
  ).toBeVisible()
  expect(
    screen.queryByRole('heading', { name: 'Edit model pricing' })
  ).not.toBeInTheDocument()
  expect(
    screen.queryByRole('textbox', { name: 'Model name' })
  ).not.toBeInTheDocument()
  expect(screen.getAllByText(/USD price per 1M tokens\./)).toHaveLength(1)
})

it('retains the model identity and heading when the editor is used standalone', () => {
  renderEditor()
  expect(
    screen.getByRole('heading', { name: 'Edit model pricing' })
  ).toBeVisible()
  expect(screen.getByRole('textbox', { name: 'Model name' })).toHaveValue(
    'example-model'
  )
  expect(screen.getByRole('textbox', { name: 'Model name' })).toBeDisabled()
})

it('updates the preview for explicit zero and disabled prices and explains dependent audio controls', async () => {
  renderEditor(true)
  const user = userEvent.setup()
  const preview = screen.getByRole('complementary', { name: 'Preview' })
  const cache = screen.getByRole('textbox', { name: 'Cache read price' })
  await user.clear(cache)
  await user.type(cache, '0')
  expect(await within(preview).findByText('$0')).toBeVisible()
  await user.click(screen.getByRole('switch', { name: 'Cache read price' }))
  expect(cache).toBeDisabled()
  expect(within(preview).queryByText('$0')).not.toBeInTheDocument()
  await waitFor(() =>
    expect(
      within(preview).queryByText('Cache read price')
    ).not.toBeInTheDocument()
  )
  const audio = screen.getByRole('switch', { name: 'Audio output price' })
  expect(audio).toHaveAttribute('aria-disabled', 'true')
  expect(audio).toHaveAccessibleDescription(
    'Audio output price requires an audio input price.'
  )
  await user.click(screen.getByRole('switch', { name: 'Audio input price' }))
  await user.type(
    screen.getByRole('textbox', { name: 'Audio input price' }),
    '1'
  )
  expect(audio).not.toHaveAttribute('aria-disabled', 'true')
})

function PricingFormFixture(props: {
  variant: 'default' | 'unset'
  onSave: () => Promise<void>
}) {
  const values = {
    ModelPrice: props.variant === 'default' ? '{"example-model":0.1}' : '{}',
    ModelRatio: '{}',
    CacheRatio: '{}',
    CreateCacheRatio: '{}',
    CompletionRatio: '{}',
    ImageRatio: '{}',
    AudioRatio: '{}',
    AudioCompletionRatio: '{}',
    BillingMode: '{}',
    BillingExpr: '{}',
    PluginBillingExpr: '{}',
    ExposeRatioEnabled: false,
  }
  const [actionsContainer, setActionsContainer] =
    useState<HTMLDivElement | null>(null)
  const form = useForm({ defaultValues: values })
  return (
    <>
      <header>
        <div ref={setActionsContainer} />
      </header>
      <SettingsPageProvider actionsContainer={actionsContainer}>
        <ModelRatioForm
          form={form}
          savedValues={values}
          variant={props.variant}
          onSave={props.onSave}
          onReset={() => undefined}
          isSaving={false}
          isResetting={false}
        />
      </SettingsPageProvider>
    </>
  )
}

it.each(['default', 'unset'] as const)(
  'keeps the %s pricing workspace shrinkable and saves from its fixed action bar',
  async (variant) => {
    const user = userEvent.setup()
    const save = vi.fn(async () => undefined)
    vi.spyOn(api, 'get').mockImplementation(async (url) => ({
      data: {
        success: true,
        data: url === '/api/channel/models_enabled' ? ['example-model'] : [],
        vendors: [],
      },
    }))
    localStorage.removeItem('model-ratio-column-visibility')
    const client = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    })
    clients.push(client)
    render(
      <QueryClientProvider client={client}>
        <PricingFormFixture variant={variant} onSave={save} />
      </QueryClientProvider>
    )
    const workspace = screen.getByRole('region', { name: 'Model prices' })
    expect(workspace).toHaveClass(
      'flex-1',
      'min-h-0',
      'grid-rows-[minmax(0,1fr)]'
    )
    await waitFor(() => expect(client.isFetching()).toBe(0))
    if (variant === 'default') {
      const toggle = screen.getByRole('switch', { name: 'Expose ratio API' })
      expect(screen.getByRole('banner')).toContainElement(toggle)
      const help = within(screen.getByRole('banner')).getByRole('button', {
        name: 'Learn more',
      })
      await user.click(help)
      expect(screen.getByRole('dialog')).toHaveTextContent(
        'Allow clients to query configured prices via `/api/ratio`.'
      )
      expect(toggle).not.toBeChecked()
      await user.keyboard('{Escape}')
      await waitFor(() =>
        expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
      )
      expect(help).toHaveFocus()
      expect(toggle).toHaveAccessibleDescription(
        'Allow clients to query configured prices via `/api/ratio`.'
      )
      await user.click(toggle)
      expect(toggle).toBeChecked()
      await user.click(screen.getByRole('button', { name: 'Switch to JSON' }))
      expect(
        screen.getAllByRole('switch', { name: 'Expose ratio API' })
      ).toHaveLength(1)
      expect(
        screen.getByRole('switch', { name: 'Expose ratio API' })
      ).toBeChecked()
      await user.click(screen.getByRole('button', { name: 'Switch to Visual' }))
    } else {
      expect(
        screen.queryByRole('switch', { name: 'Expose ratio API' })
      ).not.toBeInTheDocument()
    }
    await user.click(await screen.findByRole('button', { name: 'Edit' }))
    await user.click(
      screen.getByRole('tab', { name: 'Per-request (deprecated)' })
    )
    const price = screen.getByRole('textbox', { name: 'Fixed price' })
    await user.clear(price)
    await user.type(price, '0.25')
    const region = screen.getByRole('region', { name: 'Edit model pricing' })
    const button = screen.getByRole('button', { name: 'Save model prices' })
    expect(region).not.toContainElement(button)
    expect(button.parentElement?.parentElement).toHaveClass('shrink-0')
    await user.click(button)
    await waitFor(() => expect(save).toHaveBeenCalledOnce())
    expect(save).toHaveBeenCalledWith(
      expect.objectContaining({ ExposeRatioEnabled: variant === 'default' }),
      undefined
    )
  }
)

it.each([false, true])(
  'waits for pricing metadata and mounts one editor with mobile=%s',
  async (mobile) => {
    const matchMedia = window.matchMedia
    vi.spyOn(window, 'matchMedia').mockImplementation((query) => ({
      ...matchMedia(query),
      matches: mobile && query === '(max-width: 767px)',
    }))
    useAuthStore
      .getState()
      .auth.setUser({ id: 1, username: 'administrator', role: 100 })
    let release: (() => void) | undefined
    const ready = new Promise<void>((resolve) => {
      release = resolve
    })
    vi.spyOn(api, 'get').mockImplementation(async (url) => {
      if (url === '/api/option/model_pricing') {
        await ready
        return {
          data: {
            success: true,
            data: {
              entries: [
                {
                  model_name: 'example-model',
                  version: 'v1',
                  configured: {},
                  effective: {},
                  mixed_video: {
                    eligible: true,
                    valid: false,
                    expression: '',
                    source: 'unconfigured',
                    usage_schema: {
                      requests: { type: 'number', unit: 'count' },
                    },
                  },
                },
              ],
            },
          },
        }
      }
      return {
        data: {
          success: true,
          data: url === '/api/channel/models_enabled' ? ['example-model'] : [],
          vendors: [],
        },
      }
    })
    const client = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    })
    clients.push(client)
    render(
      <QueryClientProvider client={client}>
        <PricingFormFixture variant='default' onSave={async () => undefined} />
      </QueryClientProvider>
    )
    const user = userEvent.setup()
    await waitFor(() => expect(client.isFetching()).toBe(0))
    await user.click(await screen.findByRole('button', { name: 'Edit' }))
    expect(await screen.findByText('Loading...')).toBeVisible()
    expect(
      screen.queryByRole('region', { name: 'Edit model pricing' })
    ).not.toBeInTheDocument()
    release?.()
    expect(
      await screen.findByRole('region', { name: 'Mixed video public price' })
    ).toBeVisible()
    expect(
      screen.getAllByRole('region', { name: 'Mixed video public price' })
    ).toHaveLength(1)
    if (mobile) {
      expect(screen.getByRole('dialog')).toHaveClass('w-full', 'p-4', 'pt-12')
    }
  }
)
