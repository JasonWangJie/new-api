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

import {
  combineBillingExpr,
  parseTaskTiersFromExpr,
  splitBillingExprAndRequestRules,
} from '../lib/billing-expr'
import { evaluateBillingExpression } from '../lib/billing-expression/runtime'
import {
  evaluateTaskUsageExamples,
  evaluateTaskVisualConfig,
  generateTaskExprFromConfig,
  supportsTaskVideoInputMultipliers,
  taskMatrixToTiers,
  tryParseTaskMatrixConfig,
  tryParseTaskVisualConfig,
  type TaskVisualConfig,
} from '../lib/task-expr'
import {
  getTaskMatrixDisplayTiers,
  getTaskPricingDisplayTiers,
} from '../lib/task-matrix-display'
import type { BillingUsageSchema } from '../types'

const schema: BillingUsageSchema = {
  seconds: { type: 'number', unit: 'second' },
  clips: { type: 'number', unit: 'count' },
  mode: { enum: ['std', 'pro'] },
}

const videoSchema: BillingUsageSchema = {
  seconds: { type: 'number', unit: 'second' },
  resolution: { enum: ['720p', '1080p'] },
  video_input: { enum: ['none', 'video'] },
}

describe('model-wide reference-video multipliers', () => {
  const baseExpression =
    'u("resolution") == "720p" ? tier("720p", 2 + u("seconds") * 0.4) : tier("1080p", 3 + u("seconds") * 0.8)'

  test('saves and reopens separate multipliers without changing resolution base prices', () => {
    const base = tryParseTaskVisualConfig(baseExpression, videoSchema)
    assert.ok(base)
    const config = {
      ...base,
      videoInputMultipliers: { none: 1.2, video: 1.5 },
    }
    const expression = generateTaskExprFromConfig(config, videoSchema)
    const reopened = tryParseTaskVisualConfig(expression, videoSchema)
    expect(reopened).toEqual(config)
    const matrix = tryParseTaskMatrixConfig(expression, videoSchema)
    assert.ok(matrix)
    expect(matrix?.videoInputMultipliers).toEqual(config.videoInputMultipliers)
    expect(matrix?.rows.map((row) => row.unitPrices.seconds)).toEqual([
      0.4, 0.4, 0.8, 0.8,
    ])
    expect(matrix?.rows.map((row) => row.constant)).toEqual([2, 2, 3, 3])
    expect(
      generateTaskExprFromConfig(
        {
          tiers: taskMatrixToTiers(matrix, videoSchema),
          videoInputMultipliers: matrix.videoInputMultipliers,
        },
        videoSchema
      )
    ).not.toBe('')
    for (const [resolution, video_input, total] of [
      ['720p', 'none', 4.8],
      ['720p', 'video', 6],
      ['1080p', 'none', 8.4],
      ['1080p', 'video', 10.5],
    ] as const) {
      const usage = { seconds: 5, resolution, video_input }
      const preview = evaluateTaskVisualConfig(config, usage, videoSchema)
      expect(preview?.total).toBeCloseTo(total)
      expect(preview?.multiplier).toBe(
        config.videoInputMultipliers[video_input]
      )
      const actual = evaluateBillingExpression(expression, { usage })
      expect(actual.status).toBe('success')
      if (actual.status === 'success') expect(actual.cost).toBe(preview?.total)
    }
  })

  test('preserves existing input-specific base prices and separate request multipliers', () => {
    const base = tryParseTaskVisualConfig(
      'u("video_input") == "video" ? tier("video", u("seconds") * 0.8) : tier("none", u("seconds") * 0.4)',
      videoSchema
    )
    assert.ok(base)
    const expression = generateTaskExprFromConfig(
      { ...base, videoInputMultipliers: { none: 1, video: 1.5 } },
      videoSchema
    )
    const rules = '(header("x-priority") == "high" ? 2 : 1)'
    const combined = combineBillingExpr(expression, rules)
    const split = splitBillingExprAndRequestRules(combined)
    expect(split.requestRuleExpr).toBe(rules)
    const matrix = tryParseTaskMatrixConfig(split.billingExpr, videoSchema)
    expect(matrix?.rows.map((row) => row.unitPrices.seconds)).toEqual([
      0.4, 0.8, 0.4, 0.8,
    ])
    const reopened = tryParseTaskVisualConfig(split.billingExpr, videoSchema)
    expect(reopened?.videoInputMultipliers).toEqual({ none: 1, video: 1.5 })
    expect(
      combineBillingExpr(
        generateTaskExprFromConfig(reopened, videoSchema),
        rules
      )
    ).toBe(combined)
    const result = evaluateBillingExpression(combined, {
      usage: { video_input: 'video', seconds: 5 },
      request: { headers: { 'x-priority': 'high' } },
    })
    expect(result.status).toBe('success')
    if (result.status === 'success') expect(result.cost).toBe(12)
  })

  test.each([
    { field: 'requests', unit: 'count', quantity: 1, price: 4, total: 6 },
    { field: 'tokens', unit: 'token', quantity: 1_500_000, price: 4, total: 9 },
  ] as const)(
    'applies a uniform multiplier to $unit prices and examples',
    (sample) => {
      const schema: BillingUsageSchema = {
        [sample.field]: { type: 'number', unit: sample.unit },
        video_input: { enum: ['none', 'video'] },
      }
      const config: TaskVisualConfig = {
        tiers: [
          {
            label: 'base',
            conditions: [],
            constant: 0,
            unitPrices: { [sample.field]: sample.price },
          },
        ],
        videoInputMultipliers: { none: 1, video: 1.5 },
      }
      const facts = { video_input: 'video', [sample.field]: sample.quantity }
      const expression = generateTaskExprFromConfig(config, schema)
      expect(evaluateTaskVisualConfig(config, facts, schema)?.total).toBe(
        sample.total
      )
      expect(
        evaluateTaskUsageExamples(expression, schema, [
          { label: 'example', facts },
        ])
      ).toEqual([{ label: 'example', total: sample.total }])
    }
  )

  test('shows effective public prices and keeps a zero multiplier as free', () => {
    const config = tryParseTaskVisualConfig(baseExpression, videoSchema)
    assert.ok(config)
    const expression = generateTaskExprFromConfig(
      { ...config, videoInputMultipliers: { none: 0, video: 1.5 } },
      videoSchema
    )
    const matrix = getTaskMatrixDisplayTiers(expression, videoSchema)
    expect(matrix).toHaveLength(4)
    for (const [index, price] of [0, 0.6, 0, 1.2].entries()) {
      expect(matrix?.[index].unitPrices.seconds).toBeCloseTo(price)
    }
    expect(matrix?.map((tier) => tier.constant)).toEqual([0, 3, 0, 4.5])
    const published = getTaskPricingDisplayTiers(expression, videoSchema, true)
    expect(published).toHaveLength(2)
    expect(
      published.every((tier) =>
        tier.conditions.some(
          (condition) =>
            condition.field === 'video_input' && condition.value === 'video'
        )
      )
    ).toBe(true)
    const parsed = tryParseTaskVisualConfig(expression, videoSchema)
    assert.ok(parsed)
    const free = evaluateTaskVisualConfig(
      parsed,
      { resolution: '720p', video_input: 'none', seconds: 5 },
      videoSchema
    )
    expect(free?.total).toBe(0)
    expect(free?.multiplier).toBe(0)
  })

  test('leaves the default expression unchanged and rejects unsupported or invalid factors', () => {
    const base = tryParseTaskVisualConfig(baseExpression, videoSchema)
    assert.ok(base)
    expect(
      generateTaskExprFromConfig(
        { ...base, videoInputMultipliers: { none: 1, video: 1 } },
        videoSchema
      )
    ).toBe(baseExpression)
    expect(supportsTaskVideoInputMultipliers(videoSchema)).toBe(true)
    expect(
      supportsTaskVideoInputMultipliers({
        ...videoSchema,
        video_input: { enum: ['none', 'video', 'other'] },
      })
    ).toBe(false)
    for (const value of [-1, Number.NaN, Number.POSITIVE_INFINITY]) {
      const config = {
        ...base,
        videoInputMultipliers: { none: 1, video: value },
      }
      expect(generateTaskExprFromConfig(config, videoSchema)).toBe('')
      expect(
        evaluateTaskVisualConfig(
          config,
          { resolution: '720p', seconds: 5, video_input: 'video' },
          videoSchema
        )
      ).toBeNull()
    }
    const expression = `(${baseExpression}) * (u("video_input") == "video" ? 1.5 : 1)`
    expect(
      tryParseTaskVisualConfig(expression, {
        seconds: videoSchema.seconds,
        resolution: videoSchema.resolution,
      })
    ).toBeNull()
    expect(
      tryParseTaskMatrixConfig(
        `(${baseExpression}) * (u("video_input") == "video" ? -1 : 1)`,
        videoSchema
      )
    ).toBeNull()
    expect(
      tryParseTaskMatrixConfig(
        `(${baseExpression}) * (u("video_input") == "video" ? 1e309 : 1)`,
        videoSchema
      )
    ).toBeNull()
    expect(
      tryParseTaskMatrixConfig(`(${baseExpression}) * 2`, videoSchema)
    ).toBeNull()
  })
})

function assertConfigRoundTrip(config: TaskVisualConfig) {
  const expression = generateTaskExprFromConfig(config, schema)
  const parsed = tryParseTaskVisualConfig(expression, schema)
  assert.ok(parsed)
  assert.equal(generateTaskExprFromConfig(parsed, schema), expression)
}

describe('task billing expressions', () => {
  test('round-trips flat, enum-tiered, and additive canonical shapes', () => {
    assertConfigRoundTrip({
      tiers: [
        {
          label: 'base',
          conditions: [],
          constant: 0,
          unitPrices: { seconds: 0.4, clips: 0 },
        },
      ],
    })
    assertConfigRoundTrip({
      tiers: [
        {
          label: 'pro',
          conditions: [{ field: 'mode', value: 'pro' }],
          constant: 0,
          unitPrices: { seconds: 0.8, clips: 0 },
        },
        {
          label: 'std',
          conditions: [],
          constant: 0,
          unitPrices: { seconds: 0.4, clips: 0 },
        },
      ],
    })
    assertConfigRoundTrip({
      tiers: [
        {
          label: 'base',
          conditions: [],
          constant: 0.1,
          unitPrices: { seconds: 0.4, clips: 0.05 },
        },
      ],
    })
  })

  test('preserves request-rule factors around a canonical task expression', () => {
    const baseExpression = generateTaskExprFromConfig(
      {
        tiers: [
          {
            label: 'base',
            conditions: [],
            constant: 0,
            unitPrices: { seconds: 0.4, clips: 0 },
          },
        ],
      },
      schema
    )
    const requestRules = '(header("x-priority") == "high" ? 2 : 1)'
    const combined = combineBillingExpr(baseExpression, requestRules)
    const split = splitBillingExprAndRequestRules(combined)

    assert.equal(split.requestRuleExpr, requestRules)
    const parsed = tryParseTaskVisualConfig(split.billingExpr, schema)
    assert.ok(parsed)
    assert.equal(
      combineBillingExpr(
        generateTaskExprFromConfig(parsed, schema),
        requestRules
      ),
      combined
    )
  })

  test('rejects expressions outside the frozen task shapes', () => {
    assert.equal(tryParseTaskVisualConfig('u("seconds") * 0.4', schema), null)
    assert.equal(
      tryParseTaskVisualConfig(
        'u("seconds") > 30 ? tier("long", u("seconds") * 0.3) : tier("short", u("seconds") * 0.4)',
        schema
      ),
      null
    )
    assert.equal(
      tryParseTaskVisualConfig('tier("base", u("unknown") * 0.4)', schema),
      null
    )
  })
})

describe('task visual pricing preview', () => {
  test('totals a base charge and usage price for a single tier', () => {
    const tier = {
      label: 'base',
      conditions: [],
      constant: 0.02,
      unitPrices: { seconds: 0.1 },
    }

    const result = evaluateTaskVisualConfig({ tiers: [tier] }, { seconds: 5 })

    assert.ok(result)
    assert.equal(result.tier, tier)
    assert.equal(result.total, 0.52)
    assert.deepEqual(result.parts, [
      { kind: 'constant', amount: 0.02 },
      {
        kind: 'usage',
        field: 'seconds',
        amount: 0.5,
        quantity: 5,
        unitPrice: 0.1,
      },
    ])
  })

  test('matches enum tiers in order and otherwise uses the fallback', () => {
    const config: TaskVisualConfig = {
      tiers: [
        {
          label: 'pro',
          conditions: [{ field: 'mode', value: 'pro' }],
          constant: 0,
          unitPrices: { seconds: 0.8 },
        },
        {
          label: 'std',
          conditions: [],
          constant: 0,
          unitPrices: { seconds: 0.4 },
        },
      ],
    }

    assert.equal(
      evaluateTaskVisualConfig(config, { mode: 'pro', seconds: 1 })?.tier.label,
      'pro'
    )
    assert.equal(
      evaluateTaskVisualConfig(config, { mode: 'std', seconds: 1 })?.tier.label,
      'std'
    )
    assert.equal(
      evaluateTaskVisualConfig(config, { mode: 'unknown', seconds: 1 })?.tier
        .label,
      'std'
    )
  })

  test('requires every enum condition on a multi-condition tier', () => {
    const config: TaskVisualConfig = {
      tiers: [
        {
          label: 'extend-two',
          conditions: [
            { field: 'action', value: 'extend' },
            { field: 'quality', value: 'high' },
          ],
          constant: 0,
          unitPrices: { clips: 0.2 },
        },
        {
          label: 'base',
          conditions: [],
          constant: 0,
          unitPrices: { clips: 0.1 },
        },
      ],
    }

    assert.equal(
      evaluateTaskVisualConfig(config, {
        action: 'extend',
        quality: 'high',
        clips: 2,
      })?.tier.label,
      'extend-two'
    )
    assert.equal(
      evaluateTaskVisualConfig(config, {
        action: 'extend',
        quality: 'standard',
        clips: 2,
      })?.tier.label,
      'base'
    )
  })

  test('selects the same tier after round-tripping through the expression grammar', () => {
    const config: TaskVisualConfig = {
      tiers: [
        {
          label: 'pro',
          conditions: [{ field: 'mode', value: 'pro' }],
          constant: 0.05,
          unitPrices: { seconds: 0.8, clips: 0.1 },
        },
        {
          label: 'std',
          conditions: [],
          constant: 0.02,
          unitPrices: { seconds: 0.4, clips: 0.05 },
        },
      ],
    }
    const sample = { mode: 'pro', seconds: 5, clips: 2 }
    const expression = generateTaskExprFromConfig(config, schema)
    const parsedTiers = parseTaskTiersFromExpr(expression, schema)
    assert.ok(parsedTiers.length > 0)
    const grammarTier =
      parsedTiers
        .slice(0, -1)
        .find((tier) =>
          tier.conditions.every(
            (condition) =>
              sample[condition.field as keyof typeof sample] === condition.value
          )
        ) ?? parsedTiers.at(-1)
    assert.ok(grammarTier)

    const result = evaluateTaskVisualConfig(config, sample)

    assert.ok(result)
    assert.equal(result.tier.label, grammarTier.label)
  })

  test('round-trips a token field at the $/1M editor scale', () => {
    const tokenSchema: BillingUsageSchema = {
      tokens: { type: 'number', unit: 'token' },
    }
    const config: TaskVisualConfig = {
      tiers: [
        {
          label: 'base',
          conditions: [],
          constant: 0,
          unitPrices: { tokens: 9.8 },
        },
      ],
    }

    const expression = generateTaskExprFromConfig(config, tokenSchema)
    assert.match(expression, /\/ 1000000/)
    assert.equal(expression, 'tier("base", u("tokens") * 9.8 / 1000000)')

    const parsed = tryParseTaskVisualConfig(expression, tokenSchema)
    assert.ok(parsed)
    assert.equal(parsed.tiers[0].unitPrices.tokens, 9.8)
    assert.equal(generateTaskExprFromConfig(parsed, tokenSchema), expression)
  })

  test('treats a bare token term as unparseable so old $/token expressions stay raw', () => {
    const tokenSchema: BillingUsageSchema = {
      tokens: { type: 'number', unit: 'token' },
    }
    assert.equal(
      tryParseTaskVisualConfig(
        'tier("base", u("tokens") * 0.0000098)',
        tokenSchema
      ),
      null
    )
    assert.deepEqual(
      parseTaskTiersFromExpr(
        'tier("base", u("tokens") * 0.0000098)',
        tokenSchema
      ),
      []
    )
  })

  test('round-trips a credit field without a /1M division', () => {
    const creditSchema: BillingUsageSchema = {
      units: { type: 'number', unit: 'credit' },
    }
    const config: TaskVisualConfig = {
      tiers: [
        {
          label: 'base',
          conditions: [],
          constant: 0,
          unitPrices: { units: 0.14 },
        },
      ],
    }

    const expression = generateTaskExprFromConfig(config, creditSchema)
    assert.equal(expression, 'tier("base", u("units") * 0.14)')
    assert.doesNotMatch(expression, /\/ 1000000/)

    const parsed = tryParseTaskVisualConfig(expression, creditSchema)
    assert.ok(parsed)
    assert.equal(parsed.tiers[0].unitPrices.units, 0.14)
  })

  test('maps declared usage example labels to evaluated prices', () => {
    const tokenSchema: BillingUsageSchema = {
      tokens: { type: 'number', unit: 'token' },
    }
    const config = tryParseTaskVisualConfig(
      'tier("base", u("tokens") * 9.8 / 1000000)',
      tokenSchema
    )
    assert.ok(config)

    const result = evaluateTaskVisualConfig(
      config,
      { tokens: 108000 },
      tokenSchema
    )
    assert.ok(result)
    assert.equal(result.total, (108000 * 9.8) / 1_000_000)

    assert.deepEqual(
      evaluateTaskUsageExamples(
        'tier("base", u("tokens") * 9.8 / 1000000)',
        tokenSchema,
        [
          { label: '720p · 5s', facts: { tokens: 108000 } },
          { label: '1080p · 5s', facts: { tokens: 243000 } },
        ]
      ),
      [
        { label: '720p · 5s', total: (108000 * 9.8) / 1_000_000 },
        { label: '1080p · 5s', total: (243000 * 9.8) / 1_000_000 },
      ]
    )
  })

  test('returns no usage example prices for a raw unparseable expression', () => {
    assert.deepEqual(
      evaluateTaskUsageExamples(
        'u("tokens") * 0.00007 * (u("tokens") > 100000 ? 0.8 : 1)',
        { tokens: { type: 'number', unit: 'token' } },
        [{ label: '720p · 5s', facts: { tokens: 108000 } }]
      ),
      []
    )
  })
})
