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
import assert from 'node:assert/strict'

import { describe, expect, test } from 'vitest'

import { getDynamicPriceEntries } from '../lib/dynamic-price'
import {
  getTaskMatrixDisplayTiers,
  getTaskPricingDisplayTiers,
} from '../lib/task-matrix-display'
import type { BillingUsageSchema } from '../types'

const resolutionSchema: BillingUsageSchema = {
  seconds: { type: 'number', unit: 'second' },
  resolution: { enum: ['480P', '720P', '1080P'] },
}

const doubleEnumSchema: BillingUsageSchema = {
  quality: { enum: ['high', 'low'] },
  seconds: { type: 'number', unit: 'second' },
  mode: { enum: ['std', 'pro'] },
}

const numberOnlySchema: BillingUsageSchema = {
  seconds: { type: 'number', unit: 'second' },
}

describe('task matrix marketplace display rows', () => {
  test('published resolution prices omit zero combinations while the editor retains them', () => {
    const expression =
      'u("resolution") == "480P" ? tier("480P", u("seconds") * 0) : u("resolution") == "720P" ? tier("720P", u("seconds") * 7) : tier("1080P", u("seconds") * 0)'
    const published = getTaskPricingDisplayTiers(
      expression,
      resolutionSchema,
      true
    )
    expect(published.map((tier) => tier.conditions)).toEqual([
      [{ field: 'resolution', value: '720P' }],
    ])
    expect(
      getTaskPricingDisplayTiers(expression, resolutionSchema)
    ).toHaveLength(3)
  })

  test('published prices omit unreachable resolutions from older expressions', () => {
    const expression =
      'u("resolution") == "480P" ? tier("480P", u("seconds") * 4) : u("resolution") == "720P" ? tier("720P", u("seconds") * 7) : u("resolution") == "1080P" ? tier("1080P", u("seconds") * 9) : tier("4K", u("seconds") * 20)'
    const published = getTaskPricingDisplayTiers(
      expression,
      {
        ...resolutionSchema,
        resolution: { enum: ['480P', '720P'] },
      },
      true
    )
    expect(published.map((tier) => tier.unitPrices.seconds)).toEqual([4, 7])
    expect(published.map((tier) => tier.conditions[0].value)).toEqual([
      '480P',
      '720P',
    ])
  })

  test('a nonzero additional charge keeps a resolution published even when usage is free', () => {
    const published = getTaskPricingDisplayTiers(
      'tier("base", 0.25 + u("seconds") * 0)',
      resolutionSchema,
      true
    )
    expect(published).toHaveLength(3)
    expect(published.every((tier) => tier.constant === 0.25)).toBe(true)
  })

  test('expands a uniform flat expression into every enum combination', () => {
    const rows = getTaskMatrixDisplayTiers(
      'tier("base", u("seconds") * 0.4)',
      resolutionSchema
    )

    assert.deepEqual(rows, [
      {
        label: '480P',
        conditions: [{ field: 'resolution', value: '480P' }],
        constant: 0,
        unitPrices: { seconds: 0.4 },
      },
      {
        label: '720P',
        conditions: [{ field: 'resolution', value: '720P' }],
        constant: 0,
        unitPrices: { seconds: 0.4 },
      },
      {
        label: '1080P',
        conditions: [{ field: 'resolution', value: '1080P' }],
        constant: 0,
        unitPrices: { seconds: 0.4 },
      },
    ])
  })

  test('expands a full non-uniform partition in canonical order with combination labels', () => {
    const expression =
      'u("mode") == "std" && u("quality") == "high" ? tier("std·high", 0.1 + u("seconds") * 0.2) : u("mode") == "std" && u("quality") == "low" ? tier("std·low", 0.2 + u("seconds") * 0.3) : u("mode") == "pro" && u("quality") == "high" ? tier("pro·high", 0.3 + u("seconds") * 0.4) : tier("pro·low", 0.4 + u("seconds") * 0.5)'

    assert.deepEqual(getTaskMatrixDisplayTiers(expression, doubleEnumSchema), [
      {
        label: 'std·high',
        conditions: [
          { field: 'mode', value: 'std' },
          { field: 'quality', value: 'high' },
        ],
        constant: 0.1,
        unitPrices: { seconds: 0.2 },
      },
      {
        label: 'std·low',
        conditions: [
          { field: 'mode', value: 'std' },
          { field: 'quality', value: 'low' },
        ],
        constant: 0.2,
        unitPrices: { seconds: 0.3 },
      },
      {
        label: 'pro·high',
        conditions: [
          { field: 'mode', value: 'pro' },
          { field: 'quality', value: 'high' },
        ],
        constant: 0.3,
        unitPrices: { seconds: 0.4 },
      },
      {
        label: 'pro·low',
        conditions: [
          { field: 'mode', value: 'pro' },
          { field: 'quality', value: 'low' },
        ],
        constant: 0.4,
        unitPrices: { seconds: 0.5 },
      },
    ])
  })

  test('returns null for a number-only schema so the single-row display stays', () => {
    assert.equal(
      getTaskMatrixDisplayTiers(
        'tier("base", u("seconds") * 0.4)',
        numberOnlySchema
      ),
      null
    )
  })

  test('returns null for an unrecognizable sparse expression', () => {
    assert.equal(
      getTaskMatrixDisplayTiers(
        'u("seconds") > 30 ? tier("long", u("seconds") * 0.3) : tier("short", u("seconds") * 0.4)',
        resolutionSchema
      ),
      null
    )
  })

  test('returns null when there is no usage schema', () => {
    assert.equal(
      getTaskMatrixDisplayTiers('tier("base", p * 2 + c * 8)', undefined),
      null
    )
  })

  test('keeps group-ratio multiplication on expanded display-row unit prices', () => {
    const rows = getTaskMatrixDisplayTiers(
      'tier("base", 0.1 + u("seconds") * 0.4)',
      resolutionSchema
    )
    assert.ok(rows)
    assert.equal(rows.length, 3)

    const baseEntries = getDynamicPriceEntries(rows[0], {
      tokenUnit: 'K',
      showRechargePrice: false,
      usageSchema: resolutionSchema,
      groupRatioMultiplier: 1,
    })
    const doubledEntries = getDynamicPriceEntries(rows[0], {
      tokenUnit: 'K',
      showRechargePrice: false,
      usageSchema: resolutionSchema,
      groupRatioMultiplier: 2,
    })

    assert.equal(baseEntries[0]?.value, 0.4)
    assert.equal(doubledEntries[0]?.value, 0.4)
    assert.match(baseEntries[0]?.formatted ?? '', /0[.,]4/)
    assert.match(doubledEntries[0]?.formatted ?? '', /0[.,]8/)
    assert.equal(baseEntries.at(-1)?.value, 0.1)
    assert.match(doubledEntries.at(-1)?.formatted ?? '', /0[.,]2/)
  })
})
