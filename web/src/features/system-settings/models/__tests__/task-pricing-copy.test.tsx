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
import { act, fireEvent, render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import i18next from 'i18next'
import { useState } from 'react'
import { afterEach, expect, it, vi } from 'vitest'

import { evaluateBillingExpression } from '@/features/pricing/lib/billing-expression/runtime'
import { getTaskPricingDisplayTiers } from '@/features/pricing/lib/task-matrix-display'
import type { BillingUsageSchema } from '@/features/pricing/types'

import { TaskUsagePricingEditor } from '../task-usage-pricing-editor'

function renderPricing(unitLabel?: Record<string, string>) {
  const onBillingExprChange = vi.fn()
  render(
    <TaskUsagePricingEditor
      billingExpr='tier("music", 1 + u("clips") * 11)'
      requestRuleExpr=''
      usageSchema={{
        action: {
          enum: ['music', 'lyrics'],
          enumLabels: {
            music: { en: 'Generate songs', zh: '生成歌曲' },
            lyrics: { en: 'Generate lyrics', zh: '生成歌词' },
          },
          description: { en: 'Generate songs or lyrics', zh: '生成歌曲或歌词' },
        },
        clips: {
          type: 'number',
          unit: 'count',
          unitLabel,
          description: { en: 'Song generation unit price', zh: '生成歌曲单价' },
        },
      }}
      onBillingExprChange={onBillingExprChange}
      onRequestRuleExprChange={vi.fn()}
    />
  )
  return onBillingExprChange
}

afterEach(async () => {
  await act(() => i18next.changeLanguage('en'))
})

it('shows localized schema explanations in the price table and calculator', async () => {
  renderPricing()
  const table = screen.getByRole('table')
  expect(within(table).getByText('Song generation unit price')).toBeVisible()
  expect(within(table).getByText('Generate songs or lyrics')).toBeVisible()
  expect(
    screen.getByRole('spinbutton', {
      name: 'Usage · Song generation unit price',
    })
  ).toBeVisible()
  expect(
    within(table).getByText(
      'Added to the usage cost. Set to 0 for no additional charge.'
    )
  ).toBeVisible()
  await act(() => i18next.changeLanguage('zhCN'))
  expect(within(table).getByText('生成歌曲单价')).toBeVisible()
  expect(screen.getByRole('combobox', { name: '生成歌曲或歌词' })).toBeVisible()
})

it('uses count unit labels in the price matrix and calculator without changing the charge', async () => {
  renderPricing({ en: 'song', zh: '首' })
  expect(within(screen.getByRole('table')).getByText('$/song')).toBeVisible()
  expect(
    screen.getByText(
      'Additional charge: $1 + Song generation unit price: 1 song × $11/song = $12'
    )
  ).toBeVisible()
  await act(() => i18next.changeLanguage('zhCN'))
  expect(within(screen.getByRole('table')).getByText('$/首')).toBeVisible()
  expect(screen.getByText('首')).toBeVisible()
  expect(screen.getByText(/1 首 × \$11\/首 = \$12/)).toBeVisible()
})

it('identifies pricing conditions and keeps the additional charge unchanged when sample usage changes', async () => {
  renderPricing()
  expect(
    screen.getByText('Current pricing conditions: Generate songs')
  ).toBeVisible()
  expect(
    screen.getByText(
      'Additional charge: $1 + Song generation unit price: 1 unit × $11/unit = $12'
    )
  ).toBeVisible()
  const user = userEvent.setup()
  const quantity = screen.getByRole('spinbutton', {
    name: 'Usage · Song generation unit price',
  })
  await user.clear(quantity)
  await user.type(quantity, '2')
  expect(
    screen.getByText(
      'Additional charge: $1 + Song generation unit price: 2 unit × $11/unit = $23'
    )
  ).toBeVisible()
})

it('shows localized enum choices while preserving raw values in generated billing expressions', async () => {
  const onChange = renderPricing()
  const user = userEvent.setup()
  await user.click(
    screen.getByRole('combobox', { name: 'Generate songs or lyrics' })
  )
  await user.click(screen.getByRole('option', { name: 'Generate lyrics' }))
  expect(
    screen.getByText('Current pricing conditions: Generate lyrics')
  ).toBeVisible()
  const price = screen.getByRole('textbox', {
    name: 'Song generation unit price: Generate lyrics',
  })
  await user.clear(price)
  await user.type(price, '3')
  const expression = onChange.mock.lastCall?.[0]
  expect(expression).toContain('u("action") == "music"')
  expect(expression).toContain('tier("lyrics"')
  expect(expression).not.toContain('Generate lyrics')
})

function TaskPricingDraft(props: {
  expression: string
  onChange: (next: string) => void
  usageSchema?: BillingUsageSchema
}) {
  const [billingExpr, setBillingExpr] = useState(props.expression)
  const [requestRuleExpr, setRequestRuleExpr] = useState(
    '(header("x-priority") == "high" ? 2 : 1)'
  )
  return (
    <TaskUsagePricingEditor
      billingExpr={billingExpr}
      requestRuleExpr={requestRuleExpr}
      usageSchema={
        props.usageSchema ?? {
          seconds: {
            type: 'number',
            unit: 'second',
            description: 'Seconds price',
          },
          mode: { enum: ['std', 'pro'] },
        }
      }
      onBillingExprChange={(next) => {
        setBillingExpr(next)
        props.onChange(next)
      }}
      onRequestRuleExprChange={setRequestRuleExpr}
    />
  )
}

const videoMultiplierSchema: BillingUsageSchema = {
  seconds: {
    type: 'number',
    unit: 'second',
    description: 'Video generation unit price',
  },
  resolution: { enum: ['720p', '1080p'], description: 'Resolution' },
  video_input: {
    enum: ['none', 'video'],
    description: 'Reference video input',
    enumLabels: { none: 'No reference video', video: 'With reference video' },
  },
}

it('applies one reference-video multiplier across resolutions and reopens it without changing base prices', async () => {
  const onChange = vi.fn()
  const view = render(
    <TaskPricingDraft
      expression='u("resolution") == "720p" ? tier("720p", 1 + u("seconds") * 0.2) : tier("1080p", 2 + u("seconds") * 0.4)'
      usageSchema={videoMultiplierSchema}
      onChange={onChange}
    />
  )
  expect(
    screen.getByRole('textbox', { name: 'Without reference video multiplier' })
  ).toHaveValue('1')
  const multiplier = screen.getByRole('textbox', {
    name: 'With reference video multiplier',
  })
  expect(multiplier).toHaveValue('1')
  expect(onChange).not.toHaveBeenCalled()
  const user = userEvent.setup()
  await user.clear(multiplier)
  await user.type(multiplier, '1.5')
  const expression = onChange.mock.lastCall?.[0] as string
  for (const [resolution, video_input, expected] of [
    ['720p', 'none', 2],
    ['720p', 'video', 3],
    ['1080p', 'video', 6],
  ] as const) {
    expect(
      evaluateBillingExpression(expression, {
        usage: { seconds: 5, resolution, video_input },
      })
    ).toMatchObject({ status: 'success', cost: expected })
  }
  expect(
    screen.getByRole('textbox', {
      name: 'Video generation unit price: Resolution: 720p · With reference video',
    })
  ).toHaveValue('0.2')
  await user.click(
    screen.getByRole('combobox', { name: 'Reference video input' })
  )
  await user.click(screen.getByRole('option', { name: 'With reference video' }))
  expect(screen.getByText(/\) × 1\.5 = \$3/)).toBeVisible()
  view.unmount()
  render(
    <TaskPricingDraft
      expression={expression}
      usageSchema={videoMultiplierSchema}
      onChange={vi.fn()}
    />
  )
  expect(
    screen.getByRole('textbox', { name: 'With reference video multiplier' })
  ).toHaveValue('1.5')
  expect(
    screen.getByRole('textbox', { name: 'Without reference video multiplier' })
  ).toHaveValue('1')
  expect(screen.getByRole('table')).toBeVisible()
  await user.click(screen.getByRole('combobox', { name: 'Editor mode' }))
  await user.click(screen.getByRole('option', { name: 'Expression editor' }))
  expect(
    (
      screen.getByRole('textbox', {
        name: 'Billing expression',
      }) as HTMLTextAreaElement
    ).value
  ).toContain('header("x-priority")')
})

it('blocks invalid multiplier drafts and preserves an explicit zero multiplier', () => {
  const onChange = vi.fn()
  render(
    <TaskPricingDraft
      expression='tier("base", u("seconds") * 0.4)'
      usageSchema={videoMultiplierSchema}
      onChange={onChange}
    />
  )
  const multiplier = screen.getByRole('textbox', {
    name: 'With reference video multiplier',
  })
  for (const value of ['', '-1', 'Infinity', 'NaN']) {
    fireEvent.change(multiplier, { target: { value } })
    expect(multiplier).toHaveAttribute('aria-invalid', 'true')
    expect(multiplier.closest('[data-billing-invalid="true"]')).not.toBeNull()
    expect(
      within(
        multiplier.closest('[data-slot="field"]') as HTMLElement
      ).getByRole('alert')
    ).toHaveTextContent('Enter a finite multiplier of 0 or greater.')
    expect(onChange).not.toHaveBeenCalled()
  }
  fireEvent.change(multiplier, { target: { value: '0' } })
  expect(multiplier).not.toHaveAttribute('aria-invalid')
  expect(multiplier).toHaveValue('0')
  const expression = onChange.mock.lastCall?.[0] as string
  expect(
    evaluateBillingExpression(expression, {
      usage: { seconds: 5, resolution: '720p', video_input: 'video' },
    })
  ).toMatchObject({ status: 'success', cost: 0 })
  expect(
    evaluateBillingExpression(expression, {
      usage: { seconds: 5, resolution: '720p', video_input: 'none' },
    })
  ).toMatchObject({ status: 'success', cost: 2 })
})

it('keeps inherited video-reference prices while browsing or cancelling and changes only an explicitly edited price', async () => {
  const onChange = vi.fn()
  const expression =
    'u("resolution") == "480p" ? tier("480p", u("seconds") * 0.4) : tier("720p", u("seconds") * 0.8)'
  render(
    <TaskPricingDraft
      expression={expression}
      onChange={onChange}
      usageSchema={{
        seconds: {
          type: 'number',
          unit: 'second',
          description: 'Video generation unit price',
        },
        resolution: { enum: ['480p', '720p'], description: 'Resolution' },
        video_input: {
          enum: ['none', 'video'],
          description: 'Reference video input',
          enumLabels: {
            none: 'No reference video',
            video: 'With reference video',
          },
        },
      }}
    />
  )
  const ordinaryPrice = screen.getByRole('textbox', {
    name: 'Video generation unit price: Resolution: 720p · No reference video',
  })
  const referencePrice = screen.getByRole('textbox', {
    name: 'Video generation unit price: Resolution: 720p · With reference video',
  })
  expect(ordinaryPrice).toHaveValue('0.8')
  expect(referencePrice).toHaveValue('0.8')
  expect(onChange).not.toHaveBeenCalled()

  const user = userEvent.setup()
  await user.click(
    screen.getByRole('combobox', { name: 'Reference video input' })
  )
  await user.click(screen.getByRole('option', { name: 'With reference video' }))
  expect(onChange).not.toHaveBeenCalled()
  await user.clear(referencePrice)
  await user.type(referencePrice, '1.2')
  expect(referencePrice).toHaveValue('1.2')
  expect(ordinaryPrice).toHaveValue('0.8')
  const editedExpression = onChange.mock.lastCall?.[0]
  expect(editedExpression).toContain('u("video_input")')

  onChange.mockClear()
  await user.click(screen.getByRole('combobox', { name: 'Editor mode' }))
  await user.click(screen.getByRole('option', { name: 'Expression editor' }))
  const raw = screen.getByRole('textbox', {
    name: 'Billing expression',
  }) as HTMLTextAreaElement
  expect(raw.value).toContain(editedExpression)
  await user.click(screen.getByRole('combobox', { name: 'Editor mode' }))
  await user.click(screen.getByRole('option', { name: 'Visual editor' }))
  await user.click(
    within(screen.getByRole('alertdialog')).getByRole('button', {
      name: 'Cancel',
    })
  )
  expect(raw.value).toContain(editedExpression)
  expect(onChange).not.toHaveBeenCalled()
})

it('clears every price in one resolution row and lets an administrator publish it again', async () => {
  const onChange = vi.fn()
  const usageSchema = {
    resolution: { enum: ['480p', '720p'], description: 'Resolution' },
    seconds: {
      type: 'number' as const,
      unit: 'second' as const,
      description: 'Seconds price',
    },
    requests: {
      type: 'number' as const,
      unit: 'count' as const,
      description: 'Video price',
    },
  }
  render(
    <TaskUsagePricingEditor
      billingExpr='u("resolution") == "480p" ? tier("480p", u("seconds") * 0.35 + u("requests") * 1 + 0.25) : tier("720p", u("seconds") * 0.5)'
      requestRuleExpr=''
      usageSchema={usageSchema}
      onBillingExprChange={onChange}
      onRequestRuleExprChange={vi.fn()}
    />
  )
  const user = userEvent.setup()
  const clear = screen.getByRole('button', {
    name: 'Clear price for Resolution: 480p',
  })
  await user.click(clear)
  for (const name of ['Seconds price', 'Video price', 'Additional charge']) {
    expect(
      screen.getByRole('textbox', { name: `${name}: Resolution: 480p` })
    ).toHaveValue('0')
  }
  expect(
    screen.getByRole('textbox', { name: 'Seconds price: Resolution: 720p' })
  ).toHaveValue('0.5')
  expect(clear).toBeDisabled()
  expect(
    getTaskPricingDisplayTiers(
      onChange.mock.lastCall?.[0],
      usageSchema,
      true
    ).map((tier) => tier.conditions)
  ).toEqual([[{ field: 'resolution', value: '720p' }]])
  const price = screen.getByRole('textbox', {
    name: 'Seconds price: Resolution: 480p',
  })
  await user.clear(price)
  await user.type(price, '0.4')
  expect(clear).toBeEnabled()
  expect(
    getTaskPricingDisplayTiers(onChange.mock.lastCall?.[0], usageSchema, true)
  ).toHaveLength(2)
})

it('lets users cancel or discard an unsupported expression and its request rules', async () => {
  const user = userEvent.setup()
  const onChange = vi.fn()
  render(
    <TaskPricingDraft
      expression='tier("custom", u("seconds") * u("seconds"))'
      onChange={onChange}
    />
  )
  const original = screen.getByRole('textbox', { name: 'Billing expression' })
  const originalValue = (original as HTMLTextAreaElement).value
  await user.click(screen.getByRole('combobox', { name: 'Editor mode' }))
  await user.click(screen.getByRole('option', { name: 'Visual editor' }))
  let dialog = screen.getByRole('alertdialog')
  expect(dialog).toHaveTextContent('resets all prices to zero')
  expect(onChange).not.toHaveBeenCalled()
  await user.click(within(dialog).getByRole('button', { name: 'Cancel' }))
  expect(original).toHaveValue(originalValue)
  expect(onChange).not.toHaveBeenCalled()

  await user.click(screen.getByRole('combobox', { name: 'Editor mode' }))
  await user.click(screen.getByRole('option', { name: 'Visual editor' }))
  dialog = screen.getByRole('alertdialog')
  await user.click(
    within(dialog).getByRole('button', { name: 'Switch to visual editor' })
  )
  expect(screen.queryByRole('alertdialog')).not.toBeInTheDocument()
  expect(screen.getByRole('table')).toBeVisible()
  expect(onChange).toHaveBeenLastCalledWith('tier("base", u("seconds") * 0)')
  for (const price of within(screen.getByRole('table')).getAllByRole(
    'textbox'
  )) {
    expect(price).toHaveValue('0')
  }
  await user.click(screen.getByRole('combobox', { name: 'Editor mode' }))
  await user.click(screen.getByRole('option', { name: 'Expression editor' }))
  expect(
    screen.getByRole('textbox', { name: 'Billing expression' })
  ).toHaveValue('tier("base", u("seconds") * 0)')
})

it('confirms regeneration of supported expressions and preserves their prices and request rules', async () => {
  const user = userEvent.setup()
  const onChange = vi.fn()
  const expression =
    'u("mode") == "std" ? tier("old-standard", u("seconds") * 0.4) : tier("old-pro", u("seconds") * 0.8)'
  render(<TaskPricingDraft expression={expression} onChange={onChange} />)
  await user.click(screen.getByRole('combobox', { name: 'Editor mode' }))
  await user.click(screen.getByRole('option', { name: 'Expression editor' }))
  expect(
    (
      screen.getByRole('textbox', {
        name: 'Billing expression',
      }) as HTMLTextAreaElement
    ).value
  ).toContain(expression)
  expect(onChange).not.toHaveBeenCalled()
  await user.click(screen.getByRole('combobox', { name: 'Editor mode' }))
  await user.click(screen.getByRole('option', { name: 'Visual editor' }))
  const dialog = screen.getByRole('alertdialog')
  expect(dialog).toHaveTextContent(
    'replaces its original formatting and tier names'
  )
  expect(onChange).not.toHaveBeenCalled()
  await user.click(
    within(dialog).getByRole('button', { name: 'Switch to visual editor' })
  )
  expect(onChange).toHaveBeenLastCalledWith(
    'u("mode") == "std" ? tier("std", u("seconds") * 0.4) : tier("pro", u("seconds") * 0.8)'
  )
  expect(
    screen.getByRole('textbox', { name: 'Seconds price: mode: std' })
  ).toHaveValue('0.4')
  expect(
    screen.getByRole('textbox', { name: 'Seconds price: mode: pro' })
  ).toHaveValue('0.8')
  await user.click(screen.getByRole('combobox', { name: 'Editor mode' }))
  await user.click(screen.getByRole('option', { name: 'Expression editor' }))
  expect(
    (
      screen.getByRole('textbox', {
        name: 'Billing expression',
      }) as HTMLTextAreaElement
    ).value
  ).toContain('header("x-priority")')
})

it.each(['image', 'video'] as const)(
  'limits pricing fields to the resolved %s model schema',
  (kind) => {
    const usageSchema = {
      image: {
        image_count: {
          type: 'number' as const,
          unit: 'count' as const,
          unitLabel: { en: 'image', zh: '张' },
          description: { en: 'Image quantity' },
        },
      },
      video: {
        seconds: {
          type: 'number' as const,
          unit: 'second' as const,
          description: { en: 'Video duration' },
        },
        resolution: {
          enum: ['720P', '1080P'],
          description: { en: 'Resolution' },
        },
      },
    }
    const field = kind === 'image' ? 'image_count' : 'seconds'
    render(
      <TaskUsagePricingEditor
        billingExpr={`tier("base", u("${field}") * 1)`}
        requestRuleExpr=''
        usageSchema={usageSchema[kind]}
        onBillingExprChange={vi.fn()}
        onRequestRuleExprChange={vi.fn()}
      />
    )
    const presentLabel = kind === 'image' ? 'Image quantity' : 'Video duration'
    const absentLabel = kind === 'image' ? 'Video duration' : 'Image quantity'
    expect(
      screen.getByRole('spinbutton', { name: `Usage · ${presentLabel}` })
    ).toBeVisible()
    expect(
      screen.queryByRole('spinbutton', { name: `Usage · ${absentLabel}` })
    ).not.toBeInTheDocument()
    expect(
      screen.queryByRole('textbox', { name: 'With reference video multiplier' })
    ).not.toBeInTheDocument()
    if (kind === 'image') {
      expect(screen.getByText('$/image')).toBeVisible()
      expect(
        screen.queryByRole('combobox', { name: 'Resolution' })
      ).not.toBeInTheDocument()
    } else {
      expect(screen.getByRole('combobox', { name: 'Resolution' })).toBeVisible()
    }
  }
)
