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
import { Download, Expand, ImageIcon } from 'lucide-react'
import { useState, type ReactNode } from 'react'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'

import { CopyButton } from '@/components/copy-button'
import { Button } from '@/components/ui/button'
import { ImageDialog } from '@/features/usage-logs/components/dialogs/image-dialog'

import { imageBlob } from '../api'
import { imageFileExtension } from '../lib/image-request'

export function ImageResults({
  images,
  actions,
  empty,
  previewMode = 'standard',
}: {
  images: {
    url: string
    id: string
    title?: string
    description?: string
    direct?: boolean
  }[]
  actions?: (id: string) => ReactNode
  empty?: string
  previewMode?: 'standard' | 'original'
}) {
  const { t } = useTranslation()
  const [preview, setPreview] = useState<number | null>(null)
  const download = async (index: number) => {
    try {
      const image = images[index]
      if (image.direct) {
        window.open(image.url, '_blank', 'noopener,noreferrer')
        return
      }
      const blob = await imageBlob(image.url)
      const url = URL.createObjectURL(blob)
      const anchor = document.createElement('a')
      anchor.href = url
      anchor.download = `${image.id}.${imageFileExtension(blob.type)}`
      anchor.click()
      setTimeout(() => URL.revokeObjectURL(url), 0)
    } catch (error) {
      toast.error(
        error instanceof Error ? error.message : t('Failed to load image')
      )
    }
  }
  if (!images.length) {
    return (
      <div className='bg-muted/20 flex min-h-72 flex-col items-center justify-center rounded-xl border border-dashed p-8 text-center'>
        <ImageIcon
          className='text-muted-foreground mb-4 size-9'
          aria-hidden='true'
        />
        <p className='text-muted-foreground max-w-sm text-sm'>
          {empty || t('Your images will appear here')}
        </p>
      </div>
    )
  }
  return (
    <>
      <div className='grid grid-cols-1 gap-4 sm:grid-cols-2 xl:grid-cols-3'>
        {images.map((image, index) => (
          <article
            key={image.id}
            className='bg-card overflow-hidden rounded-xl border'
          >
            <button
              type='button'
              className='bg-muted/30 group focus-visible:ring-ring relative block aspect-square w-full focus-visible:ring-2'
              aria-label={`${t('Image Preview')}: ${image.title || index + 1}`}
              onClick={() => setPreview(index)}
            >
              <img
                src={image.url}
                alt={image.title || t('Generated image')}
                className='h-full w-full object-contain'
                loading='lazy'
              />
              <span className='bg-background/90 absolute right-3 bottom-3 rounded-full p-2 opacity-0 transition-opacity group-hover:opacity-100 group-focus-visible:opacity-100'>
                <Expand className='size-4' aria-hidden='true' />
              </span>
            </button>
            <div className='space-y-2 p-3'>
              <div className='flex items-center justify-between gap-2'>
                <p className='truncate text-sm font-medium'>
                  {image.title || `${t('Image')} ${index + 1}`}
                </p>
                <Button
                  size='icon'
                  variant='ghost'
                  aria-label={t('Download image')}
                  onClick={() => void download(index)}
                >
                  <Download className='size-4' />
                </Button>
                {image.direct && (
                  <CopyButton value={image.url} aria-label={t('Copy link')} />
                )}
              </div>
              {image.description && (
                <p className='text-muted-foreground text-xs'>
                  {image.description}
                </p>
              )}
              {actions?.(image.id)}
            </div>
          </article>
        ))}
      </div>
      {preview !== null && images.length > 0 && (
        <ImageDialog
          key={images[preview]?.id}
          images={images.map((image) => image.url)}
          initialIndex={preview}
          open
          presentation={previewMode}
          onOpenChange={(open) => {
            if (!open) setPreview(null)
          }}
        />
      )}
    </>
  )
}
