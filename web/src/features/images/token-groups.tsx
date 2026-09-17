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
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'

import {
  SideDrawerSection,
  SideDrawerSectionHeader,
} from '@/components/drawer-layout'
import { Button } from '@/components/ui/button'
import { useAuthStore } from '@/stores/auth-store'

import { imageRequest } from './api'
import { ImageSelect } from './components/image-select'

type ImageGroups = { openai: string | null; gemini: string | null }

export function TokenImageGroups({
  tokenId,
  groups,
}: {
  tokenId: number
  groups: { value: string; label: string }[]
}) {
  const { t } = useTranslation()
  const userId = useAuthStore((state) => state.auth.user?.id)
  const queryClient = useQueryClient()
  const [draft, setDraft] = useState<ImageGroups | null>(null)
  const [busy, setBusy] = useState(false)
  const url = `/api/token/${tokenId}/image-platform-groups`
  const mapping = useQuery({
    queryKey: ['token-image-groups', userId, tokenId],
    queryFn: ({ signal }) =>
      imageRequest<ImageGroups>(url, 'GET', undefined, signal),
    staleTime: 0,
  })
  const value = draft || mapping.data
  const save = async () => {
    if (!value) return
    setBusy(true)
    try {
      await imageRequest(url, 'PATCH', value)
      await Promise.all([
        queryClient.invalidateQueries({
          queryKey: ['token-image-groups', userId, tokenId],
        }),
        queryClient.invalidateQueries({
          queryKey: ['image-capabilities', userId, String(tokenId)],
        }),
      ])
      setDraft(null)
      toast.success(t('Saved'))
    } catch (error) {
      toast.error(
        error instanceof Error ? error.message : t('Image request failed')
      )
    } finally {
      setBusy(false)
    }
  }
  return (
    <SideDrawerSection className='max-h-72 shrink-0 overflow-y-auto px-4 py-4 sm:px-6'>
      <SideDrawerSectionHeader
        title={t('Image platform groups')}
        description={t(
          'Platform settings override the primary token group for image requests.'
        )}
      />
      {mapping.isError && (
        <p role='alert' className='text-destructive text-sm'>
          {mapping.error.message}
        </p>
      )}
      {value && (
        <div className='grid gap-3'>
          {(['openai', 'gemini'] as const).map((platform) => (
            <ImageSelect
              key={platform}
              label={platform === 'openai' ? 'OpenAI' : 'Gemini'}
              value={value[platform] || '__primary__'}
              disabled={busy}
              options={[
                { value: '__primary__', label: t('Use primary token group') },
                ...groups.filter((group) => group.value !== 'auto'),
              ]}
              onChange={(group) =>
                setDraft({
                  ...value,
                  [platform]: group === '__primary__' ? null : group,
                })
              }
            />
          ))}
          <Button
            type='button'
            variant='outline'
            disabled={busy || !draft}
            onClick={() => void save()}
          >
            {t('Save image groups')}
          </Button>
        </div>
      )}
    </SideDrawerSection>
  )
}
