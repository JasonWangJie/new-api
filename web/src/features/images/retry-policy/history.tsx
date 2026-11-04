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

import { ConfirmDialog } from '@/components/confirm-dialog'
import { StaticDataTable } from '@/components/data-table'
import { Dialog } from '@/components/dialog'
import { Button } from '@/components/ui/button'
import { formatTimestampToDate } from '@/lib/format'
import { useAuthStore } from '@/stores/auth-store'

import { imageRequest } from '../api'
import { ConfigurationConflict } from '../components/configuration-conflict'
import { isConfigurationConflict } from '../lib/configuration-conflict'
import { RETRY_POLICY_URL, retryPolicyChanges } from './policy'
import type { RetryPolicy, RetryVersion } from './types'

export function RetryHistory(props: {
  policy: RetryPolicy
  canManage: boolean
  onSaved: (policy: RetryPolicy) => Promise<void>
}) {
  const { t } = useTranslation()
  const [page, setPage] = useState(1)
  const [selected, setSelected] = useState<RetryVersion | null>(null)
  const [confirm, setConfirm] = useState(false)
  const userId = useAuthStore((state) => state.auth.user?.id)
  const versions = useQuery({
    queryKey: ['retry-policy-versions', props.policy.revision, page],
    queryFn: () =>
      imageRequest<{ items: RetryVersion[]; total: number }>(
        `${RETRY_POLICY_URL}/versions?page=${page}&page_size=20`
      ),
  })
  const detail = useQuery({
    queryKey: ['retry-policy-version', selected?.revision],
    enabled: !!selected,
    queryFn: () =>
      imageRequest<{ policy: RetryPolicy }>(
        `${RETRY_POLICY_URL}/versions/${selected?.revision}`
      ),
  })
  const rollback = useMutation({
    mutationKey: ['media-retry-policy-save', userId, 'rollback'],
    mutationFn: (policy: RetryPolicy) =>
      imageRequest<RetryPolicy>(`${RETRY_POLICY_URL}/rollback`, 'POST', {
        revision: policy.revision,
        target_revision: selected?.revision,
      }),
    onSuccess: async (policy) => {
      await props.onSaved(policy)
      setConfirm(false)
      setSelected(null)
    },
    onError: () => setConfirm(false),
  })
  return (
    <div className='space-y-3'>
      {versions.error && <p role='alert'>{versions.error.message}</p>}
      <StaticDataTable
        data={versions.data?.items ?? []}
        emptyContent={t('No version history')}
        columns={[
          {
            id: 'revision',
            header: t('Version'),
            cell: (version) => version.revision,
          },
          {
            id: 'time',
            header: t('Time'),
            cell: (version) => formatTimestampToDate(version.created_at),
          },
          {
            id: 'actor',
            header: t('Operator'),
            cell: (version) => version.actor_id,
          },
          {
            id: 'action',
            header: t('Action'),
            cell: (version) =>
              version.action === 'rollback' ? t('Rollback') : t('Save'),
          },
          {
            id: 'view',
            header: t('Actions'),
            cell: (version) => (
              <Button variant='outline' onClick={() => setSelected(version)}>
                {t('Compare version')}
              </Button>
            ),
          },
        ]}
      />
      <div className='flex justify-end gap-2'>
        <Button
          variant='outline'
          disabled={page === 1}
          onClick={() => setPage(page - 1)}
        >
          {t('Previous')}
        </Button>
        <Button
          variant='outline'
          disabled={page * 20 >= (versions.data?.total ?? 0)}
          onClick={() => setPage(page + 1)}
        >
          {t('Next')}
        </Button>
      </div>
      {selected && (
        <Dialog
          open
          onOpenChange={(open) => {
            if (!open) setSelected(null)
          }}
          title={t('Compare version')}
          description={t(
            'Restoring a version creates a new revision and preserves retry counters.'
          )}
          contentClassName='sm:max-w-4xl'
          footer={
            <Button
              disabled={!props.canManage || !detail.data}
              onClick={() => setConfirm(true)}
            >
              {t('Restore this version')}
            </Button>
          }
        >
          {detail.data && (
            <StaticDataTable
              data={retryPolicyChanges(props.policy, detail.data.policy)}
              columns={[
                {
                  id: 'field',
                  header: t('Field'),
                  cell: (change) => (
                    <span className='font-mono text-xs break-all'>
                      {change.path}
                    </span>
                  ),
                },
                {
                  id: 'current',
                  header: t('Current version'),
                  cell: (change) => (
                    <span className='block max-w-sm text-xs break-all whitespace-normal'>
                      {JSON.stringify(change.before) ?? '—'}
                    </span>
                  ),
                },
                {
                  id: 'selected',
                  header: t('Selected version'),
                  cell: (change) => (
                    <span className='block max-w-sm text-xs break-all whitespace-normal'>
                      {JSON.stringify(change.after) ?? '—'}
                    </span>
                  ),
                },
              ]}
            />
          )}
          <div className='mt-4 grid gap-4 md:grid-cols-2'>
            {[
              { label: t('Current version'), value: props.policy },
              { label: t('Selected version'), value: detail.data?.policy },
            ].map((item) => (
              <section key={item.label} className='min-w-0'>
                <h3 className='mb-2 font-semibold'>{item.label}</h3>
                <pre className='max-h-96 overflow-auto rounded-lg border p-3 text-xs'>
                  {JSON.stringify(item.value, null, 2)}
                </pre>
              </section>
            ))}
          </div>
          {detail.error && <p role='alert'>{detail.error.message}</p>}
          <ConfigurationConflict
            error={rollback.error}
            onReload={async () => {
              await props.onSaved(
                await imageRequest<RetryPolicy>(RETRY_POLICY_URL)
              )
              rollback.reset()
            }}
          />
          {rollback.error && !isConfigurationConflict(rollback.error) && (
            <p role='alert'>{rollback.error.message}</p>
          )}
        </Dialog>
      )}
      <ConfirmDialog
        open={confirm}
        onOpenChange={setConfirm}
        title={t('Restore this version')}
        desc={t(
          'Restoring a version creates a new revision and preserves retry counters.'
        )}
        handleConfirm={() => rollback.mutate(props.policy)}
        isLoading={rollback.isPending}
      />
    </div>
  )
}
