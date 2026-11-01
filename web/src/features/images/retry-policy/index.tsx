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
import { useMutation, useQuery } from '@tanstack/react-query'
import { useState } from 'react'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'

import { StaticDataTable } from '@/components/data-table'
import { Button } from '@/components/ui/button'
import { Checkbox } from '@/components/ui/checkbox'
import { Label } from '@/components/ui/label'
import { Tabs, TabsList, TabsTrigger } from '@/components/ui/tabs'
import { SettingsCard } from '@/features/system-settings/components/settings-card'
import { hasPermission } from '@/lib/admin-permissions'
import { markServerErrorHandled } from '@/lib/handle-server-error'
import { useAuthStore } from '@/stores/auth-store'

import { imageRequest } from '../api'
import { ConfigurationConflict } from '../components/configuration-conflict'
import { isConfigurationConflict } from '../lib/configuration-conflict'
import { RetryHistory } from './history'
import {
  ACTION_LABELS,
  CATEGORY_LABELS,
  copyRetryRule,
  moveRetryItem,
  newRetryRule,
  RETRY_POLICY_URL,
} from './policy'
import { RetryPreview } from './preview'
import { RetryRuleEditor } from './rule-editor'
import { RetrySamples } from './samples'
import type { RetryDraftSource, RetryPolicy, RetryRule } from './types'

export function RetryPolicyCenter(props: { source?: RetryDraftSource } = {}) {
  const { t } = useTranslation()
  const user = useAuthStore((state) => state.auth.user)
  const allowed = hasPermission(user, 'image_config', 'read')
  const canManage = hasPermission(user, 'image_config', 'manage')
  const policy = useQuery({
    queryKey: ['media-retry-policy', user?.id],
    queryFn: () => imageRequest<RetryPolicy>(RETRY_POLICY_URL),
    enabled: allowed,
    refetchOnWindowFocus: false,
  })
  if (!allowed) {
    return (
      <p>{t('Media settings require administrator storage permissions.')}</p>
    )
  }
  if (policy.error) return <p role='alert'>{policy.error.message}</p>
  if (!policy.data) return <p role='status'>{t('Loading...')}</p>
  return (
    <RetryPolicyForm
      initial={policy.data}
      canManage={canManage}
      source={props.source}
    />
  )
}

export function RetryPolicyForm(props: {
  initial: RetryPolicy
  canManage: boolean
  source?: RetryDraftSource
}) {
  const { t } = useTranslation()
  const [policy, setPolicy] = useState(() => structuredClone(props.initial))
  const [dirty, setDirty] = useState(false)
  const [tab, setTab] = useState('rules')
  const [editor, setEditor] = useState<RetryRule | null>(() =>
    props.source && props.canManage ? newRetryRule(props.source) : null
  )
  const [source, setSource] = useState(props.source)
  const [preview, setPreview] = useState(false)
  const save = useMutation({
    mutationFn: () =>
      imageRequest<RetryPolicy>(RETRY_POLICY_URL, 'PUT', policy),
    onSuccess: (saved) => {
      setPolicy(saved)
      setDirty(false)
      toast.success(t('Saved'))
    },
    onError: (error) => markServerErrorHandled(error),
  })
  const editable = props.canManage && !save.isPending
  function update(next: RetryPolicy) {
    setPolicy(next)
    setDirty(true)
  }
  function draft(input: RetryDraftSource) {
    setSource(input)
    setEditor(newRetryRule(input))
    setTab('rules')
  }
  return (
    <div className='space-y-5'>
      <SettingsCard
        title={t('Error and retry rules')}
        description={t(
          'Saved rules apply at the next decision. Requests already sent continue normally.'
        )}
      >
        <div className='flex flex-wrap items-center justify-between gap-4'>
          <div className='space-y-3'>
            <Label className='flex items-center gap-2'>
              <Checkbox
                disabled={!editable}
                checked={policy.pause_resubmissions}
                onCheckedChange={(value) =>
                  update({ ...policy, pause_resubmissions: value === true })
                }
              />
              {t('Pause automatic resubmissions')}
            </Label>
            <Label className='flex items-center gap-2'>
              <Checkbox
                disabled={
                  !editable ||
                  !(
                    policy.legacy_timeout_scope_keys?.length ||
                    policy.legacy_video_channel_ids?.length
                  )
                }
                checked={policy.legacy_timeout_enabled}
                onCheckedChange={(value) =>
                  update({ ...policy, legacy_timeout_enabled: value === true })
                }
              />
              {t('Keep existing timeout retry policies')}
            </Label>
            <p className='text-muted-foreground max-w-2xl text-xs'>
              {t(
                'Legacy timeout policies may resend uncertain requests. New keyword rules cannot grant this permission.'
              )}
            </p>
          </div>
          <div className='flex flex-wrap items-center gap-2'>
            <span className='text-muted-foreground text-xs'>
              {t('Version')} {policy.revision}
              {dirty ? ` · ${t('Unsaved changes')}` : ''}
            </span>
            <Button variant='outline' onClick={() => setPreview(true)}>
              {t('Test retry policy')}
            </Button>
            {editable && (
              <Button
                disabled={save.isPending || !dirty}
                onClick={() => save.mutate()}
              >
                {t('Save and activate')}
              </Button>
            )}
          </div>
        </div>
        <details className='mt-4 rounded-lg border p-3 text-sm'>
          <summary className='cursor-pointer font-medium'>
            {t('Captured timeout compatibility policy')}
          </summary>
          <p className='text-muted-foreground my-3 text-xs'>
            {t(
              'Captured scopes and limits are read-only. Disabling compatibility prevents uncertain requests from using this legacy exception.'
            )}
          </p>
          <div className='grid min-w-0 gap-4 sm:grid-cols-2'>
            <section className='min-w-0'>
              <h3 className='mb-2 font-medium'>{t('Captured scopes')}</h3>
              {policy.legacy_timeout_scope_keys?.length ? (
                <ul className='space-y-1'>
                  {policy.legacy_timeout_scope_keys.map((scope) => (
                    <li className='font-mono text-xs break-all' key={scope}>
                      {scope}
                    </li>
                  ))}
                </ul>
              ) : (
                <p className='text-muted-foreground'>
                  {t('No captured scope; compatibility cannot be enabled.')}
                </p>
              )}
            </section>
            <section className='min-w-0'>
              <h3 className='mb-2 font-medium'>{t('Captured limits')}</h3>
              <dl className='space-y-2 text-xs'>
                {Object.entries(policy.legacy_timeout_config ?? {}).map(
                  ([key, value]) => (
                    <div key={key} className='flex justify-between gap-2'>
                      <dt className='font-mono break-all'>{key}</dt>
                      <dd className='shrink-0 tabular-nums'>{value}</dd>
                    </div>
                  )
                )}
              </dl>
              {!!policy.legacy_video_channel_ids?.length && (
                <p className='mt-3 break-words'>
                  {t('Video')} · {t('Channels')}:{' '}
                  {policy.legacy_video_channel_ids.join(', ')}
                </p>
              )}
            </section>
          </div>
        </details>
        <ConfigurationConflict
          error={save.error}
          onReload={async () => {
            const saved = await imageRequest<RetryPolicy>(RETRY_POLICY_URL)
            setPolicy(saved)
            setDirty(false)
            save.reset()
          }}
        />
        {save.error && !isConfigurationConflict(save.error) && (
          <p role='alert' className='text-destructive mt-3'>
            {save.error.message}
          </p>
        )}
      </SettingsCard>
      <Tabs value={tab} onValueChange={(value) => setTab(String(value))}>
        <TabsList className='h-auto flex-wrap'>
          {[
            { id: 'rules', label: 'Rules' },
            { id: 'samples', label: 'Error samples' },
            { id: 'versions', label: 'Version history' },
          ].map((item) => (
            <TabsTrigger key={item.id} value={item.id}>
              {t(item.label)}
            </TabsTrigger>
          ))}
        </TabsList>
      </Tabs>
      {tab === 'rules' && (
        <div className='space-y-4'>
          <div className='flex flex-wrap items-center justify-between gap-3'>
            <p className='text-muted-foreground text-sm'>
              {t(
                'First matching custom rule wins. Unmatched errors use built-in policies.'
              )}
            </p>
            {editable && (
              <Button
                variant='outline'
                onClick={() => setEditor(newRetryRule())}
              >
                {t('Add rule')}
              </Button>
            )}
          </div>
          <StaticDataTable
            data={policy.rules}
            getRowKey={(rule) => rule.id}
            emptyContent={t(
              'No custom rules. Built-in policies remain active.'
            )}
            columns={[
              {
                id: 'rule',
                header: t('Rule'),
                cell: (rule, index) => (
                  <div className='min-w-48 space-y-1'>
                    <p className='font-medium'>
                      {index + 1}. {rule.name}
                    </p>
                    <p className='text-muted-foreground text-xs'>
                      {t(CATEGORY_LABELS[rule.category] ?? rule.category)}
                    </p>
                    <p className='text-muted-foreground max-w-sm text-xs break-words whitespace-normal'>
                      {[
                        ...(rule.scope.media_types ?? []),
                        ...(rule.scope.providers ?? []),
                        ...(rule.scope.models ?? []),
                      ].join(' · ') || t('All')}
                    </p>
                    <p className='max-w-sm text-xs break-words whitespace-normal'>
                      {rule.match.contains_any?.filter(Boolean).join(' / ') ||
                        rule.match.error_codes?.join(' / ') ||
                        rule.match.regex ||
                        '—'}
                    </p>
                  </div>
                ),
              },
              {
                id: 'chain',
                header: t('Recovery steps'),
                cell: (rule) => (
                  <ol className='min-w-64 space-y-1 text-xs whitespace-normal'>
                    {rule.steps.map((step) => (
                      <li key={step.id}>
                        {t(ACTION_LABELS[step.action])} × {step.max_attempts}
                      </li>
                    ))}
                  </ol>
                ),
              },
              {
                id: 'enabled',
                header: t('Enabled'),
                cell: (rule) => (
                  <Checkbox
                    aria-label={`${t('Enabled')}: ${rule.name}`}
                    disabled={!editable}
                    checked={rule.enabled}
                    onCheckedChange={(enabled) =>
                      update({
                        ...policy,
                        rules: policy.rules.map((item) =>
                          item.id === rule.id
                            ? { ...item, enabled: enabled === true }
                            : item
                        ),
                      })
                    }
                  />
                ),
              },
              {
                id: 'actions',
                header: t('Actions'),
                cell: (rule, index) =>
                  editable ? (
                    <div className='flex max-w-72 flex-wrap gap-1'>
                      <Button
                        size='sm'
                        variant='ghost'
                        onClick={() => setEditor(rule)}
                      >
                        {t('Edit')}
                      </Button>
                      <Button
                        size='sm'
                        variant='ghost'
                        onClick={() => setEditor(copyRetryRule(rule))}
                      >
                        {t('Duplicate')}
                      </Button>
                      <Button
                        size='sm'
                        variant='ghost'
                        disabled={index === 0}
                        onClick={() =>
                          update({
                            ...policy,
                            rules: moveRetryItem(policy.rules, index, -1),
                          })
                        }
                      >
                        {t('Move up')}
                      </Button>
                      <Button
                        size='sm'
                        variant='ghost'
                        disabled={index === policy.rules.length - 1}
                        onClick={() =>
                          update({
                            ...policy,
                            rules: moveRetryItem(policy.rules, index, 1),
                          })
                        }
                      >
                        {t('Move down')}
                      </Button>
                    </div>
                  ) : null,
              },
            ]}
          />
          <SettingsCard
            title={t('Existing built-in policies')}
            description={t(
              'Execution facts, task budgets and billing safeguards always take precedence.'
            )}
          >
            <div className='mb-4 space-y-3 text-sm'>
              <p>
                {t(
                  'Existing media policies control routing, cooldowns and retry budgets. The categories below are classification presets, not a list of executable built-in rules.'
                )}
              </p>
              <p className='text-muted-foreground'>
                {t(
                  'Unknown results remain under query or review. Successful results only resume storage or billing. Reference, account and capacity recovery uses the existing media settings.'
                )}
              </p>
              <Button
                variant='outline'
                render={<a href='/system-settings/operations/images' />}
              >
                {t('View media settings and retry budgets')}
              </Button>
            </div>
            <div className='grid gap-3 sm:grid-cols-2'>
              {Object.entries(CATEGORY_LABELS).map(([category, label]) => (
                <div key={category} className='rounded-lg border p-3'>
                  <div className='flex items-center justify-between gap-2'>
                    <p className='text-sm font-medium'>{t(label)}</p>
                    {editable && (
                      <Button
                        size='sm'
                        variant='ghost'
                        onClick={() =>
                          setEditor({
                            ...newRetryRule(),
                            name: t(label),
                            category,
                            scope: { categories: [category] },
                          })
                        }
                      >
                        {t('Create override')}
                      </Button>
                    )}
                  </div>
                  <p className='text-muted-foreground text-xs'>
                    {t('Category preset')}
                  </p>
                </div>
              ))}
            </div>
          </SettingsCard>
        </div>
      )}
      {tab === 'samples' && (
        <RetrySamples
          policy={policy}
          canManage={editable}
          onDraft={draft}
          onTest={(input) => {
            setSource(input)
            setPreview(true)
          }}
        />
      )}
      {tab === 'versions' && (
        <RetryHistory
          policy={policy}
          canManage={editable && !dirty}
          onSaved={(saved) => {
            setPolicy(saved)
            setDirty(false)
          }}
        />
      )}
      {editor && (
        <RetryRuleEditor
          rule={editor}
          onClose={() => setEditor(null)}
          onApply={(rule) => {
            update({
              ...policy,
              rules: policy.rules.some((item) => item.id === rule.id)
                ? policy.rules.map((item) =>
                    item.id === rule.id ? rule : item
                  )
                : [...policy.rules, rule],
            })
            setEditor(null)
          }}
        />
      )}
      {preview && (
        <RetryPreview
          policy={policy}
          source={source}
          onClose={() => setPreview(false)}
        />
      )}
    </div>
  )
}
