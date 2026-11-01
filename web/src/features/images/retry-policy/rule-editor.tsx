import { zodResolver } from '@hookform/resolvers/zod'
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
import { useState } from 'react'
import { useForm } from 'react-hook-form'
import { useTranslation } from 'react-i18next'

import { Dialog } from '@/components/dialog'
import { Button } from '@/components/ui/button'
import { Checkbox } from '@/components/ui/checkbox'
import {
  Collapsible,
  CollapsibleContent,
  CollapsibleTrigger,
} from '@/components/ui/collapsible'
import { Label } from '@/components/ui/label'

import { RetryField, RetrySelect } from './fields'
import {
  ACTION_LABELS,
  CATEGORY_LABELS,
  moveRetryItem,
  newRetryStep,
  retryLines,
} from './policy'
import type { RetryAction, RetryRule } from './types'
import { retryRuleSchema } from './validation'

export function RetryRuleEditor(props: {
  rule: RetryRule
  onClose: () => void
  onApply: (rule: RetryRule) => void
}) {
  const { t } = useTranslation()
  const [rule, setRule] = useState(() => structuredClone(props.rule))
  const [advanced, setAdvanced] = useState(false)
  const form = useForm({ values: rule, resolver: zodResolver(retryRuleSchema) })
  return (
    <Dialog
      open
      onOpenChange={(open) => {
        if (!open) props.onClose()
      }}
      title={t('Edit retry rule')}
      description={t('Changes remain a draft until the policy is saved.')}
      contentClassName='sm:max-w-3xl'
      footer={
        <>
          <Button variant='outline' onClick={props.onClose}>
            {t('Cancel')}
          </Button>
          <Button type='submit' form='retry-rule-form'>
            {t('Apply to draft')}
          </Button>
        </>
      }
    >
      <form
        id='retry-rule-form'
        className='space-y-6'
        onSubmit={form.handleSubmit(props.onApply)}
      >
        {Object.keys(form.formState.errors).length > 0 && (
          <p role='alert' className='text-destructive'>
            {t('Check rule fields, channel IDs and retry limits.')}
          </p>
        )}
        <div className='grid gap-4 sm:grid-cols-2'>
          <RetryField
            label='Rule name'
            required
            value={rule.name}
            onChange={(name) => setRule({ ...rule, name })}
          />
          <RetrySelect
            label='Media type'
            value={
              rule.scope.media_types?.length === 1
                ? rule.scope.media_types[0]
                : 'all'
            }
            options={{
              all: 'Images and videos',
              image: 'Image',
              video: 'Video',
            }}
            onChange={(media) =>
              setRule({
                ...rule,
                scope: {
                  ...rule.scope,
                  media_types: media === 'all' ? [] : [media],
                },
              })
            }
          />
        </div>
        <div className='grid gap-4 sm:grid-cols-2'>
          <RetryField
            label='Providers (one per line)'
            multiline
            value={rule.scope.providers?.join('\n') ?? ''}
            onChange={(value) =>
              setRule({
                ...rule,
                scope: { ...rule.scope, providers: value.split('\n') },
              })
            }
          />
          <RetryField
            label='Models (one per line)'
            multiline
            value={rule.scope.models?.join('\n') ?? ''}
            onChange={(value) =>
              setRule({
                ...rule,
                scope: { ...rule.scope, models: value.split('\n') },
              })
            }
          />
        </div>
        <div className='grid gap-4 sm:grid-cols-2'>
          <RetryField
            label='Any matching keyword (one per line)'
            multiline
            value={rule.match.contains_any?.join('\n') ?? ''}
            onChange={(value) =>
              setRule({
                ...rule,
                match: { ...rule.match, contains_any: value.split('\n') },
              })
            }
          />
          <RetryField
            label='Excluded keywords (one per line)'
            multiline
            value={rule.match.excludes?.join('\n') ?? ''}
            onChange={(value) =>
              setRule({
                ...rule,
                match: { ...rule.match, excludes: value.split('\n') },
              })
            }
          />
        </div>
        <div className='grid gap-4 sm:grid-cols-2'>
          <RetryField
            label='Error category'
            value={t(CATEGORY_LABELS[rule.category] ?? rule.category)}
            required
            onChange={(category) =>
              setRule({
                ...rule,
                category:
                  Object.entries(CATEGORY_LABELS).find(
                    ([, label]) => t(label) === category
                  )?.[0] ?? category,
              })
            }
          />
          <RetrySelect
            label='Category preset'
            value={CATEGORY_LABELS[rule.category] ? rule.category : 'custom'}
            options={{ custom: 'Custom', ...CATEGORY_LABELS }}
            onChange={(category) => {
              if (category !== 'custom') setRule({ ...rule, category })
            }}
          />
        </div>
        <section className='space-y-3' aria-label={t('Recovery steps')}>
          <div className='flex flex-wrap items-center justify-between gap-2'>
            <h3 className='font-semibold'>{t('Recovery steps')}</h3>
            <Button
              type='button'
              variant='outline'
              onClick={() =>
                setRule({
                  ...rule,
                  category: 'reference_download',
                  steps: [
                    newRetryStep('local_same_channel'),
                    { ...newRetryStep('local_other_channel'), max_attempts: 2 },
                  ],
                })
              }
            >
              {t('Reference download template')}
            </Button>
          </div>
          {rule.steps.map((step, index) => (
            <fieldset
              key={step.id}
              className='border-l-primary/50 space-y-3 rounded-lg border border-l-4 p-4'
            >
              <legend className='px-2 text-xs font-semibold'>
                {t('Step {{number}}', { number: index + 1 })}
              </legend>
              <RetrySelect
                label='Action'
                value={step.action}
                options={ACTION_LABELS}
                onChange={(action) =>
                  setRule({
                    ...rule,
                    steps: rule.steps.map((item) =>
                      item.id === step.id
                        ? { ...item, action: action as RetryAction }
                        : item
                    ),
                  })
                }
              />
              <div className='grid gap-3 sm:grid-cols-3'>
                <RetryField
                  label='Maximum attempts'
                  type='number'
                  min={1}
                  max={100}
                  required
                  value={step.max_attempts}
                  onChange={(value) =>
                    setRule({
                      ...rule,
                      steps: rule.steps.map((item) =>
                        item.id === step.id
                          ? { ...item, max_attempts: Number(value) }
                          : item
                      ),
                    })
                  }
                />
                <RetryField
                  label='Delay (seconds)'
                  type='number'
                  min={0}
                  max={86400}
                  required
                  value={step.delay_seconds}
                  onChange={(value) =>
                    setRule({
                      ...rule,
                      steps: rule.steps.map((item) =>
                        item.id === step.id
                          ? { ...item, delay_seconds: Number(value) }
                          : item
                      ),
                    })
                  }
                />
                <RetrySelect
                  label='Backoff'
                  value={step.backoff ?? 'fixed'}
                  options={{ fixed: 'Fixed', exponential: 'Exponential' }}
                  onChange={(backoff) =>
                    setRule({
                      ...rule,
                      steps: rule.steps.map((item) =>
                        item.id === step.id
                          ? {
                              ...item,
                              backoff: backoff as 'fixed' | 'exponential',
                            }
                          : item
                      ),
                    })
                  }
                />
              </div>
              <div className='flex flex-wrap gap-2'>
                <Button
                  type='button'
                  size='sm'
                  variant='ghost'
                  disabled={index === 0}
                  onClick={() =>
                    setRule({
                      ...rule,
                      steps: moveRetryItem(rule.steps, index, -1),
                    })
                  }
                >
                  {t('Move up')}
                </Button>
                <Button
                  type='button'
                  size='sm'
                  variant='ghost'
                  disabled={index === rule.steps.length - 1}
                  onClick={() =>
                    setRule({
                      ...rule,
                      steps: moveRetryItem(rule.steps, index, 1),
                    })
                  }
                >
                  {t('Move down')}
                </Button>
                <Button
                  type='button'
                  size='sm'
                  variant='ghost'
                  disabled={rule.steps.length === 1}
                  onClick={() =>
                    setRule({
                      ...rule,
                      steps: rule.steps.filter((item) => item.id !== step.id),
                    })
                  }
                >
                  {t('Remove')}
                </Button>
              </div>
            </fieldset>
          ))}
          <Button
            type='button'
            variant='outline'
            disabled={rule.steps.length >= 8}
            onClick={() =>
              setRule({ ...rule, steps: [...rule.steps, newRetryStep()] })
            }
          >
            {t('Add recovery step')}
          </Button>
          <RetryField
            label='Rule attempt limit (0 uses step limits)'
            type='number'
            min={0}
            max={100}
            value={rule.max_attempts ?? 0}
            onChange={(value) =>
              setRule({ ...rule, max_attempts: Number(value) })
            }
          />
          <RetrySelect
            label='When attempts are exhausted'
            value={rule.on_exhausted}
            options={{
              stop: ACTION_LABELS.stop,
              manual_review: ACTION_LABELS.manual_review,
            }}
            onChange={(action) =>
              setRule({ ...rule, on_exhausted: action as RetryAction })
            }
          />
        </section>
        <Collapsible open={advanced} onOpenChange={setAdvanced}>
          <CollapsibleTrigger
            render={<Button type='button' variant='outline' />}
          >
            {t('Advanced conditions')}
          </CollapsibleTrigger>
          <CollapsibleContent className='mt-4 grid gap-4 sm:grid-cols-2'>
            {(['channel_ids', 'stages', 'categories'] as const).map((key) => (
              <RetryField
                key={key}
                label={
                  {
                    channel_ids: 'Matching channel IDs (one per line)',
                    stages: 'Stages (one per line)',
                    categories: 'Original categories (one per line)',
                  }[key]
                }
                multiline
                value={rule.scope[key]?.join('\n') ?? ''}
                onChange={(value) =>
                  setRule({
                    ...rule,
                    scope: {
                      ...rule.scope,
                      [key]:
                        key === 'channel_ids'
                          ? retryLines(value).map(Number)
                          : value.split('\n'),
                    },
                  })
                }
              />
            ))}
            {(['http_statuses', 'error_codes', 'contains_all'] as const).map(
              (key) => (
                <RetryField
                  key={key}
                  label={
                    {
                      http_statuses: 'HTTP statuses (one per line)',
                      error_codes: 'Error codes (one per line)',
                      contains_all: 'All required keywords (one per line)',
                    }[key]
                  }
                  multiline
                  value={rule.match[key]?.join('\n') ?? ''}
                  onChange={(value) =>
                    setRule({
                      ...rule,
                      match: {
                        ...rule.match,
                        [key]:
                          key === 'http_statuses'
                            ? retryLines(value).map(Number)
                            : value.split('\n'),
                      },
                    })
                  }
                />
              )
            )}
            <RetryField
              label='Regular expression'
              value={rule.match.regex ?? ''}
              onChange={(regex) =>
                setRule({ ...rule, match: { ...rule.match, regex } })
              }
            />
            {(['allowed_channel_ids', 'excluded_channel_ids'] as const).map(
              (key) => (
                <RetryField
                  key={key}
                  label={
                    key === 'allowed_channel_ids'
                      ? 'Allowed target channel IDs (one per line)'
                      : 'Excluded target channel IDs (one per line)'
                  }
                  multiline
                  value={rule[key]?.join('\n') ?? ''}
                  onChange={(value) =>
                    setRule({ ...rule, [key]: retryLines(value).map(Number) })
                  }
                />
              )
            )}
          </CollapsibleContent>
        </Collapsible>
        <Label className='flex items-center gap-2'>
          <Checkbox
            checked={rule.enabled}
            onCheckedChange={(enabled) =>
              setRule({ ...rule, enabled: enabled === true })
            }
          />
          {t('Enabled')}
        </Label>
      </form>
    </Dialog>
  )
}
