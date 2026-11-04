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
  useMutation,
  useMutationState,
  useQuery,
  useQueryClient,
} from '@tanstack/react-query'
import {
  type ColumnDef,
  getCoreRowModel,
  useReactTable,
} from '@tanstack/react-table'
import { useLayoutEffect, useMemo, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'

import { ConfirmDialog } from '@/components/confirm-dialog'
import { DataTablePage } from '@/components/data-table'
import { ErrorState } from '@/components/error-state'
import { LoadingState } from '@/components/loading-state'
import { Button } from '@/components/ui/button'
import { Checkbox } from '@/components/ui/checkbox'
import {
  Collapsible,
  CollapsibleContent,
  CollapsibleTrigger,
} from '@/components/ui/collapsible'
import { Label } from '@/components/ui/label'
import { SettingsCard } from '@/features/system-settings/components/settings-card'
import { hasPermission } from '@/lib/admin-permissions'
import { markServerErrorHandled } from '@/lib/handle-server-error'
import { useAuthStore } from '@/stores/auth-store'

import { imageRequest } from '../api'
import { ConfigurationConflict } from '../components/configuration-conflict'
import { ImageRoutingPreview } from '../components/image-routing-preview'
import { isConfigurationConflict } from '../lib/configuration-conflict'
import type { ImageAdminConfiguration } from '../types'
import {
  moveVideoRoutingItem,
  newVideoRoutingRule,
  VIDEO_ROUTING_URL,
} from './policy'
import { VideoRoutingRuleEditor } from './rule-editor'
import type { VideoRoutingConfig, VideoRoutingRule } from './types'

export function VideoRoutingSettings(props: {
  config: ImageAdminConfiguration
}) {
  const { t } = useTranslation()
  const queryClient = useQueryClient()
  const [open, setOpen] = useState(false)
  const user = useAuthStore((state) => state.auth.user)
  const saveKey = ['video-routing-rules-save', user?.id]
  const saves = useMutationState({
    filters: { mutationKey: saveKey },
    select: (mutation) => ({
      status: mutation.state.status,
      variables: mutation.state.variables as VideoRoutingConfig | undefined,
      data: mutation.state.data as VideoRoutingConfig | undefined,
      error: mutation.state.error,
    }),
  })
  const latestSave = saves.at(-1)
  const [resumeSave] = useState(
    () => latestSave?.status === 'pending' || latestSave?.status === 'error'
  )
  const waitingForSave = resumeSave && latestSave?.status === 'pending'
  const allowed = hasPermission(user, 'image_config', 'read')
  const canManage = hasPermission(user, 'image_config', 'manage')
  const settings = useQuery({
    queryKey: ['video-routing-rules', user?.id],
    queryFn: () => imageRequest<VideoRoutingConfig>(VIDEO_ROUTING_URL),
    enabled: open && allowed && !waitingForSave,
    refetchOnWindowFocus: false,
  })
  return (
    <SettingsCard
      title={t('Conditional video routing')}
      description={t(
        'Route providerless video creation requests by their original parameters. Billing always uses the original model and parameters.'
      )}
    >
      <Collapsible open={open} onOpenChange={setOpen}>
        <CollapsibleTrigger render={<Button variant='outline' />}>
          {t('Configure video routing rules')}
        </CollapsibleTrigger>
        <CollapsibleContent keepMounted className='mt-4 space-y-4'>
          {!allowed && (
            <p>
              {t('Media settings require administrator storage permissions.')}
            </p>
          )}
          {allowed && settings.isLoading && <LoadingState size='sm' />}
          {allowed && waitingForSave && <LoadingState size='sm' />}
          {settings.error && (
            <ErrorState
              description={settings.error.message}
              onRetry={() => settings.refetch()}
            />
          )}
          {settings.data && !waitingForSave && (
            <VideoRoutingForm
              initial={
                resumeSave
                  ? latestSave?.data || latestSave?.variables || settings.data
                  : settings.data
              }
              initialSaveError={resumeSave ? latestSave?.error : undefined}
              config={props.config}
              canManage={canManage}
              onSaved={(saved) => {
                queryClient.setQueryData(
                  ['video-routing-rules', user?.id],
                  saved
                )
                for (const mutation of queryClient
                  .getMutationCache()
                  .findAll({ mutationKey: saveKey, status: 'error' })) {
                  queryClient.getMutationCache().remove(mutation)
                }
              }}
            />
          )}
        </CollapsibleContent>
      </Collapsible>
    </SettingsCard>
  )
}

export function VideoRoutingForm(props: {
  initial: VideoRoutingConfig
  config: ImageAdminConfiguration
  canManage: boolean
  onSaved?: (saved: VideoRoutingConfig) => void
  initialSaveError?: unknown
}) {
  const { t } = useTranslation()
  const [draft, setDraft] = useState(() => structuredClone(props.initial))
  const [editing, setEditing] = useState<VideoRoutingRule | null>(null)
  const [deleting, setDeleting] = useState<VideoRoutingRule | null>(null)
  const [resumedError, setResumedError] = useState(props.initialSaveError)
  const addRuleButton = useRef<HTMLButtonElement>(null)
  const deleteFinalFocus = useRef<HTMLElement | null>(null)
  const ruleCheckboxes = useRef(new Map<string, HTMLElement>())
  const reorderedRule = useRef<string | null>(null)
  useLayoutEffect(() => {
    if (!reorderedRule.current) return
    ruleCheckboxes.current.get(reorderedRule.current)?.focus()
    reorderedRule.current = null
  }, [draft.rules])
  const userId = useAuthStore((state) => state.auth.user?.id)
  const save = useMutation({
    mutationKey: ['video-routing-rules-save', userId],
    mutationFn: (configuration: VideoRoutingConfig) =>
      imageRequest<VideoRoutingConfig>(VIDEO_ROUTING_URL, 'PUT', configuration),
    onSuccess: (saved) => {
      setDraft(saved)
      setResumedError(undefined)
      props.onSaved?.(saved)
      toast.success(t('Saved'))
    },
    onError: (error) => markServerErrorHandled(error),
  })
  const saveError = save.error || resumedError
  const blocked = !props.canManage || save.isPending
  const columns = useMemo<ColumnDef<VideoRoutingRule>[]>(
    () => [
      {
        id: 'rule',
        meta: { mobileTitle: true },
        header: t('Rule name'),
        cell: ({ row: { original: rule } }) => (
          <div className='min-w-0 space-y-1 whitespace-normal'>
            <p className='font-medium break-words'>{rule.name}</p>
            <p className='text-muted-foreground text-xs break-all'>
              {rule.match.models.join(', ') || t('All')}
            </p>
          </div>
        ),
      },
      {
        id: 'enabled',
        meta: { mobileBadge: true },
        header: t('Enabled'),
        cell: ({ row: { original: rule } }) => (
          <Checkbox
            ref={(element) => {
              if (element) ruleCheckboxes.current.set(rule.id, element)
              else ruleCheckboxes.current.delete(rule.id)
            }}
            aria-label={t('Enable rule {{name}}', { name: rule.name })}
            checked={rule.enabled}
            disabled={blocked}
            onCheckedChange={(checked) =>
              setDraft((current) => ({
                ...current,
                rules: current.rules.map((item) =>
                  item.id === rule.id
                    ? { ...item, enabled: checked === true }
                    : item
                ),
              }))
            }
          />
        ),
      },
      {
        id: 'targets',
        header: t('Execution targets'),
        cell: ({ row: { original: rule } }) => (
          <ol className='space-y-1 whitespace-normal'>
            {rule.targets.map((target) => (
              <li key={target.channel_id} className='break-words'>
                {`#${target.channel_id} · ${target.model}`}
                {target.resolution && (
                  <span className='whitespace-nowrap'>{` → ${target.resolution}`}</span>
                )}
              </li>
            ))}
          </ol>
        ),
      },
      {
        id: 'fallback',
        header: t('When targets are exhausted'),
        cell: ({ row: { original: rule } }) =>
          rule.on_exhausted === 'original'
            ? t('Return to original routing')
            : t('Fail the task'),
      },
      {
        id: 'actions',
        header: t('Actions'),
        cell: (context) => {
          const rule = context.row.original
          const index = context.row.index
          const count = context.table.getRowModel().rows.length
          return (
            <div className='flex flex-wrap gap-2'>
              <Button
                variant='outline'
                size='sm'
                disabled={blocked || index === 0}
                aria-label={t('Move rule {{name}} up', { name: rule.name })}
                onClick={() => {
                  reorderedRule.current = rule.id
                  setDraft((current) => ({
                    ...current,
                    rules: moveVideoRoutingItem(current.rules, index, -1),
                  }))
                }}
              >
                {t('Move up')}
              </Button>
              <Button
                variant='outline'
                size='sm'
                disabled={blocked || index === count - 1}
                aria-label={t('Move rule {{name}} down', {
                  name: rule.name,
                })}
                onClick={() => {
                  reorderedRule.current = rule.id
                  setDraft((current) => ({
                    ...current,
                    rules: moveVideoRoutingItem(current.rules, index, 1),
                  }))
                }}
              >
                {t('Move down')}
              </Button>
              <Button
                variant='outline'
                size='sm'
                disabled={blocked}
                onClick={() => setEditing(rule)}
              >
                {t('Edit')}
              </Button>
              <Button
                variant='outline'
                size='sm'
                disabled={blocked || count >= 100}
                onClick={() =>
                  setEditing({
                    ...structuredClone(rule),
                    id: crypto.randomUUID(),
                    name: t('{{name}} (copy)', { name: rule.name }),
                  })
                }
              >
                {t('Duplicate')}
              </Button>
              <Button
                variant='outline'
                size='sm'
                disabled={blocked}
                onClick={(event) => {
                  deleteFinalFocus.current = event.currentTarget
                  setDeleting(rule)
                }}
              >
                {t('Delete')}
              </Button>
            </div>
          )
        },
      },
    ],
    [blocked, t]
  )
  const table = useReactTable({
    data: draft.rules,
    columns,
    getCoreRowModel: getCoreRowModel(),
    getRowId: (rule) => rule.id,
  })
  return (
    <div className='space-y-4'>
      <p className='text-muted-foreground text-sm'>
        {t(
          'The first matching enabled rule wins. Targets are tried in order; later rules are not fallback routes.'
        )}
      </p>
      <div className='flex items-center gap-2'>
        <Checkbox
          id='video-routing-enabled'
          checked={draft.enabled}
          disabled={blocked}
          onCheckedChange={(checked) =>
            setDraft({ ...draft, enabled: checked === true })
          }
        />
        <Label htmlFor='video-routing-enabled'>
          {t('Enable conditional video routing')}
        </Label>
      </div>
      <DataTablePage
        table={table}
        columns={columns}
        fixedHeight={false}
        showPagination={false}
        toolbarProps={null}
      />
      <div className='flex flex-wrap gap-2'>
        <Button
          ref={addRuleButton}
          variant='outline'
          disabled={blocked || draft.rules.length >= 100}
          onClick={() => setEditing(newVideoRoutingRule())}
        >
          {t('Add routing rule')}
        </Button>
        <Button
          disabled={blocked || isConfigurationConflict(saveError)}
          onClick={() => {
            setResumedError(undefined)
            save.mutate(draft)
          }}
        >
          {t('Save routing rules')}
        </Button>
      </div>
      {!!saveError && !isConfigurationConflict(saveError) && (
        <p role='alert' className='text-destructive'>
          {saveError instanceof Error
            ? saveError.message
            : t('An unexpected error occurred')}
        </p>
      )}
      <ConfigurationConflict
        error={saveError}
        onReload={async () => {
          const saved =
            await imageRequest<VideoRoutingConfig>(VIDEO_ROUTING_URL)
          setDraft(saved)
          setResumedError(undefined)
          props.onSaved?.(saved)
          save.reset()
        }}
      />
      <ImageRoutingPreview
        config={props.config}
        videoRoutingConfig={draft}
        videoOnly
      />
      {editing && (
        <VideoRoutingRuleEditor
          rule={editing}
          config={props.config}
          onClose={() => setEditing(null)}
          onApply={(rule) => {
            const exists = draft.rules.some((item) => item.id === rule.id)
            setDraft({
              ...draft,
              rules: exists
                ? draft.rules.map((item) => (item.id === rule.id ? rule : item))
                : [...draft.rules, rule],
            })
            setEditing(null)
          }}
        />
      )}
      <ConfirmDialog
        finalFocus={deleteFinalFocus}
        open={!!deleting}
        onOpenChange={(open) => {
          if (!open) setDeleting(null)
        }}
        title={t('Delete routing rule')}
        desc={deleting?.name || ''}
        destructive
        disabled={blocked}
        handleConfirm={() => {
          if (!deleting || blocked) return
          deleteFinalFocus.current = addRuleButton.current
          setDraft({
            ...draft,
            rules: draft.rules.filter((item) => item.id !== deleting.id),
          })
          setDeleting(null)
        }}
      />
    </div>
  )
}
