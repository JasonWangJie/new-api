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
import { useQuery } from '@tanstack/react-query'
import { useId, useMemo } from 'react'
import { useTranslation } from 'react-i18next'

import { Combobox } from '@/components/ui/combobox'
import { Label } from '@/components/ui/label'
import { getGroups } from '@/features/users/api'
import { requireServerSuccess } from '@/lib/server-error-message'

import type { ImageAdminConfiguration } from '../types'

type ImageGroupSelectProps = {
  config?: ImageAdminConfiguration
  value: string
  onChange: (value: string) => void
  disabled?: boolean
}

export function ImageGroupSelect(props: ImageGroupSelectProps) {
  const { t } = useTranslation()
  const id = useId()
  const groups = useQuery({
    queryKey: ['groups'],
    queryFn: async () => requireServerSuccess(await getGroups()),
  })
  const options = useMemo(
    () =>
      [
        ...new Set([
          ...(groups.data?.data || []),
          'default',
          props.value,
          ...(props.config?.policies.map((policy) => policy.group) || []),
          ...(props.config?.pools.map((pool) => pool.group) || []),
        ]),
      ]
        .filter((group) => group.trim().length > 0)
        .sort((a, b) => a.localeCompare(b))
        .map((group) => ({ value: group, label: group })),
    [
      groups.data?.data,
      props.value,
      props.config?.policies,
      props.config?.pools,
    ]
  )

  return (
    <div className='min-w-0 space-y-2'>
      <Label htmlFor={id}>{t('Group')}</Label>
      <Combobox
        id={id}
        value={props.value}
        options={options}
        onValueChange={(value) => {
          if (value !== null) props.onChange(value)
        }}
        placeholder={groups.isLoading ? t('Loading...') : t('Search groups...')}
        emptyText={t('No results found')}
        disabled={props.disabled}
        aria-describedby={groups.isError ? `${id}-error` : undefined}
        className='w-full'
      />
      {groups.isError && (
        <p id={`${id}-error`} role='alert' className='text-destructive text-sm'>
          {groups.error.message}
        </p>
      )}
    </div>
  )
}
