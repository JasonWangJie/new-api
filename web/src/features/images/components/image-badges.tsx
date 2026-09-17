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
import { useTranslation } from 'react-i18next'

import { Badge } from '@/components/ui/badge'
import { getLobeIcon } from '@/lib/lobe-icon'
import { cn } from '@/lib/utils'

import { imageLabel } from '../lib/image-labels'
import {
  imagePlatformVisual,
  imageRequestTypeClass,
  imageStatusBadgeClass,
  imageStatusBadgeVariant,
} from '../lib/image-visuals'

export function ImageStatusBadge(props: {
  status: string
  errorCode?: string | number | boolean | null
  className?: string
}) {
  const { t } = useTranslation()
  return (
    <Badge
      variant={imageStatusBadgeVariant(props.status, props.errorCode)}
      className={cn(
        imageStatusBadgeClass(props.status, props.errorCode),
        props.className
      )}
    >
      {t(imageLabel(props.status))}
    </Badge>
  )
}

export function ImagePlatformBadge(props: {
  platform: string
  className?: string
}) {
  const { t } = useTranslation()
  const visual = imagePlatformVisual(props.platform)
  return (
    <span
      className={cn(
        'inline-flex items-center gap-1.5 rounded-full border px-2 py-0.5 text-xs font-medium',
        visual.className,
        props.className
      )}
    >
      <span className='flex size-3.5 shrink-0 items-center justify-center'>
        {getLobeIcon(visual.iconKey, 14)}
      </span>
      {t(imageLabel(props.platform))}
    </span>
  )
}

export function ImageRequestTypeBadge(props: {
  requestType: string
  className?: string
}) {
  const { t } = useTranslation()
  return (
    <Badge
      variant='outline'
      className={cn(imageRequestTypeClass(props.requestType), props.className)}
    >
      {t(imageLabel(props.requestType))}
    </Badge>
  )
}
