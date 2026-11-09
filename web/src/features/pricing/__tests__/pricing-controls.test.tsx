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
import {
  createMemoryHistory,
  createRootRoute,
  createRoute,
  createRouter,
  RouterProvider,
} from '@tanstack/react-router'
import { render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'

import {
  PricingToolbar,
  type PricingToolbarProps,
} from '../components/pricing-toolbar'
import { useFilters } from '../hooks/use-filters'
import type { PricingModel } from '../types'

function toolbarProps(): PricingToolbarProps {
  return {
    filteredCount: 2,
    totalCount: 2,
    sortBy: 'name',
    tokenUnit: 'M',
    showRechargePrice: false,
    viewMode: 'card',
    quotaTypeFilter: 'all',
    endpointTypeFilter: 'all',
    vendorFilter: 'all',
    groupFilter: 'all',
    tagFilter: 'all',
    onSortChange: vi.fn(),
    onTokenUnitChange: vi.fn(),
    onRechargePriceChange: vi.fn(),
    onViewModeChange: vi.fn(),
    onQuotaTypeChange: vi.fn(),
    onEndpointTypeChange: vi.fn(),
    onVendorChange: vi.fn(),
    onGroupChange: vi.fn(),
    onTagChange: vi.fn(),
    vendors: [],
    groups: ['default', 'premium'],
    groupRatios: { default: 1, premium: 3 },
    tags: [],
    models: [],
    hasActiveFilters: false,
    activeFilterCount: 0,
    onClearFilters: vi.fn(),
  }
}

const modelOrderFixtures = [
  { model_name: 'gpt-5', supported_endpoint_types: ['openai'] },
  {
    model_name: 'sora-2',
    vendor_name: 'OpenAI',
    supported_endpoint_types: ['openai-video', 'openai'],
  },
  {
    model_name: 'seedream-5-pro',
    supported_endpoint_types: ['image-generation'],
  },
  {
    model_name: 'gemini-nano-banana-2.1',
    supported_endpoint_types: ['openai'],
  },
  { model_name: 'gpt-image-2', supported_endpoint_types: ['image-generation'] },
  { model_name: 'claude-sonnet', supported_endpoint_types: ['openai'] },
  {
    model_name: 'gemini-3-pro-image-preview',
    supported_endpoint_types: ['gemini'],
  },
  {
    model_name: 'codex-auto-review',
    supported_endpoint_types: ['openai-response'],
  },
  { model_name: 'dall-e-3', supported_endpoint_types: ['image-generation'] },
  {
    model_name: 'doubao-seedream-4-5',
    supported_endpoint_types: ['image-generation'],
  },
  {
    model_name: 'o3',
    vendor_name: 'OpenAI',
    supported_endpoint_types: ['openai'],
  },
].map(
  (model, index): PricingModel => ({
    id: index + 1,
    quota_type: 1,
    model_price: index + 1,
    model_ratio: 0,
    completion_ratio: 0,
    enable_groups: ['default'],
    ...model,
  })
)

function ModelSortFixture() {
  const { filteredModels, sortBy, setSortBy } = useFilters(modelOrderFixtures)

  return (
    <>
      <PricingToolbar
        {...toolbarProps()}
        sortBy={sortBy}
        onSortChange={setSortBy}
      />
      <ol aria-label='Models'>
        {filteredModels.map((model) => (
          <li key={model.id}>{model.model_name}</li>
        ))}
      </ol>
    </>
  )
}

async function renderModelSorting(initialUrl = '/pricing/') {
  vi.spyOn(window, 'scrollTo').mockImplementation(() => {})
  const root = createRootRoute()
  const pricing = createRoute({ getParentRoute: () => root, path: 'pricing' })
  const index = createRoute({
    getParentRoute: () => pricing,
    path: '/',
    component: ModelSortFixture,
  })
  const router = createRouter({
    routeTree: root.addChildren([pricing.addChildren([index])]),
    history: createMemoryHistory({ initialEntries: [initialUrl] }),
  })
  await router.load()
  render(<RouterProvider router={router} />)
  const list = await screen.findByRole('list', { name: 'Models' })
  return () =>
    within(list)
      .getAllByRole('listitem')
      .map((row) => row.textContent)
}

describe('pricing controls', () => {
  it('changes the token unit and keeps the selected unit pressed when clicked again', async () => {
    const props = toolbarProps()
    const user = userEvent.setup()
    const { rerender } = render(<PricingToolbar {...props} />)
    await user.click(screen.getByRole('button', { name: '/1K' }))
    expect(props.onTokenUnitChange).toHaveBeenCalledWith('K')
    rerender(<PricingToolbar {...props} tokenUnit='K' />)
    const selected = screen.getByRole('button', { name: '/1K' })
    expect(selected).toHaveAttribute('aria-pressed', 'true')
    await user.click(selected)
    expect(selected).toHaveAttribute('aria-pressed', 'true')
    expect(props.onTokenUnitChange).toHaveBeenCalledTimes(1)
  })

  it('changes the recharge display mode with an accessible selected state', async () => {
    const props = toolbarProps()
    const user = userEvent.setup()
    const { rerender } = render(<PricingToolbar {...props} />)
    await user.click(screen.getByRole('button', { name: 'Recharge' }))
    expect(props.onRechargePriceChange).toHaveBeenCalledWith(true)
    rerender(<PricingToolbar {...props} showRechargePrice />)
    expect(screen.getByRole('button', { name: 'Recharge' })).toHaveAttribute(
      'aria-pressed',
      'true'
    )
    expect(screen.getByRole('button', { name: 'Standard' })).toHaveAttribute(
      'aria-pressed',
      'false'
    )
  })

  it('switches to table view with the keyboard and exposes the selected view', async () => {
    const props = toolbarProps()
    const user = userEvent.setup()
    const { rerender } = render(<PricingToolbar {...props} />)
    const tableButton = screen.getByRole('button', { name: 'Table view' })
    tableButton.focus()
    await user.keyboard('{Enter}')
    expect(props.onViewModeChange).toHaveBeenCalledWith('table')
    rerender(<PricingToolbar {...props} viewMode='table' />)
    expect(tableButton).toHaveAttribute('aria-pressed', 'true')
    expect(screen.getByRole('button', { name: 'Card view' })).toHaveAttribute(
      'aria-pressed',
      'false'
    )
  })

  it('defaults to the recommended model order and orders each family by name', async () => {
    const modelNames = await renderModelSorting()
    expect(modelNames()).toEqual([
      'gemini-3-pro-image-preview',
      'gemini-nano-banana-2.1',
      'dall-e-3',
      'gpt-image-2',
      'doubao-seedream-4-5',
      'seedream-5-pro',
      'claude-sonnet',
      'sora-2',
      'codex-auto-review',
      'gpt-5',
      'o3',
    ])
  })

  it('honors an explicit name sort and switches to price sorting from the dropdown', async () => {
    const modelNames = await renderModelSorting('/pricing/?sort=name')
    expect(modelNames()[0]).toBe('claude-sonnet')
    const user = userEvent.setup()
    await user.click(screen.getByRole('button', { name: 'Name' }))
    await user.click(
      screen.getByRole('menuitem', { name: 'Price: Low to High' })
    )
    expect(modelNames()).toEqual(
      modelOrderFixtures.map((model) => model.model_name)
    )
    await user.click(screen.getByRole('button', { name: 'Price: Low to High' }))
    await user.click(screen.getByRole('menuitem', { name: 'Name' }))
    expect(modelNames()[0]).toBe('claude-sonnet')
  })

  it('opens mobile filters from the left, selects a group, and restores focus on close', async () => {
    const props = toolbarProps()
    const user = userEvent.setup()
    const { rerender } = render(<PricingToolbar {...props} />)
    await user.click(screen.getByRole('button', { name: 'Filter' }))
    const dialog = await screen.findByRole('dialog', { name: 'Filter' })
    expect(dialog).toHaveAttribute('data-side', 'left')
    expect(within(dialog).getByRole('button', { name: 'Reset' })).toBeDisabled()
    await user.click(within(dialog).getByRole('button', { name: /premium/ }))
    expect(props.onGroupChange).toHaveBeenCalledWith('premium')
    rerender(
      <PricingToolbar
        {...props}
        groupFilter='premium'
        hasActiveFilters
        activeFilterCount={1}
      />
    )
    expect(
      within(dialog).getByRole('button', { name: /premium/ })
    ).toHaveAttribute('aria-pressed', 'true')
    await user.click(within(dialog).getByRole('button', { name: 'Reset' }))
    expect(props.onClearFilters).toHaveBeenCalledOnce()
    await user.keyboard('{Escape}')
    expect(await screen.findByRole('button', { name: /Filter/ })).toHaveFocus()
  })
})
