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
import { useTranslation } from 'react-i18next'

import { CopyButton } from '@/components/copy-button'
import { Dialog } from '@/components/dialog'
import { Button } from '@/components/ui/button'
import { ScrollArea } from '@/components/ui/scroll-area'
import { Skeleton } from '@/components/ui/skeleton'

interface ImageDialogProps {
  imageUrl?: string
  images?: string[]
  initialIndex?: number
  taskId?: string
  open: boolean
  onOpenChange: (open: boolean) => void
  presentation?: 'standard' | 'original'
}

export function ImageDialog({
  imageUrl,
  images,
  initialIndex = 0,
  taskId,
  open,
  onOpenChange,
  presentation = 'standard',
}: ImageDialogProps) {
  const { t } = useTranslation()
  const [isLoading, setIsLoading] = useState(true)
  const [hasError, setHasError] = useState(false)
  const sources = images?.length
    ? images
    : [imageUrl].filter((url): url is string => !!url)
  const [index, setIndex] = useState(Math.max(0, initialIndex))
  const source = sources[Math.min(index, Math.max(0, sources.length - 1))]
  const changeImage = (offset: number) => {
    setIndex((current) => (current + offset + sources.length) % sources.length)
    setIsLoading(true)
    setHasError(false)
  }

  // Reset loading state when dialog opens or image URL changes
  const handleOpenChange = (newOpen: boolean) => {
    if (newOpen) {
      setIndex(Math.max(0, Math.min(initialIndex, sources.length - 1)))
      setIsLoading(true)
      setHasError(false)
    }
    onOpenChange(newOpen)
  }

  const handleImageLoad = () => {
    setIsLoading(false)
    setHasError(false)
  }

  const handleImageError = () => {
    setIsLoading(false)
    setHasError(true)
  }

  return (
    <Dialog
      open={open}
      onOpenChange={handleOpenChange}
      title={t('Image Preview')}
      description={
        taskId ? `${t('Task ID:')} ${taskId}` : t('View the generated image')
      }
      contentClassName={
        presentation === 'original'
          ? 'h-dvh max-h-dvh w-screen max-w-none gap-0 rounded-none border-0 bg-black p-0 shadow-none ring-0 sm:max-w-none sm:p-0 [&>[data-slot=dialog-close]]:z-10 [&>[data-slot=dialog-close]]:bg-background/90'
          : 'sm:max-w-3xl'
      }
      contentHeight='auto'
      headerClassName={presentation === 'original' ? 'sr-only' : undefined}
      scrollAreaClassName={
        presentation === 'original'
          ? 'm-0 h-dvh max-h-none overflow-hidden'
          : undefined
      }
      bodyClassName={
        presentation === 'original'
          ? 'flex h-dvh w-full items-center justify-center overflow-hidden p-0'
          : 'space-y-4'
      }
    >
      <div
        className={
          presentation === 'original'
            ? 'relative flex h-full w-full items-center justify-center'
            : undefined
        }
        onKeyDown={(event) => {
          if (sources.length < 2) return
          if (event.key === 'ArrowLeft' || event.key === 'ArrowRight') {
            event.preventDefault()
            changeImage(event.key === 'ArrowLeft' ? -1 : 1)
          }
        }}
      >
        {sources.length > 1 && (
          <div
            className={
              presentation === 'original'
                ? 'bg-background/90 fixed bottom-4 left-1/2 z-10 flex -translate-x-1/2 items-center gap-3 rounded-lg p-2'
                : 'flex items-center justify-between gap-3'
            }
          >
            <Button
              variant='outline'
              onClick={() => changeImage(-1)}
              aria-label={t('Previous image')}
            >
              {t('Previous')}
            </Button>
            <span aria-live='polite' className='text-muted-foreground text-sm'>
              {index + 1} / {sources.length}
            </span>
            <Button
              variant='outline'
              onClick={() => changeImage(1)}
              aria-label={t('Next image')}
            >
              {t('Next')}
            </Button>
          </div>
        )}
        {presentation === 'original' ? (
          <>
            {isLoading && !hasError && (
              <p role='status' className='bg-background/90 rounded-lg p-4'>
                {t('Loading...')}
              </p>
            )}
            {hasError && (
              <p role='alert' className='bg-background/90 rounded-lg p-4'>
                {t('Failed to load image')}
              </p>
            )}
            <img
              key={source}
              src={source}
              alt={t('Generated image')}
              className={`block max-h-dvh max-w-[100vw] object-contain ${isLoading || hasError ? 'hidden' : ''}`}
              onLoad={handleImageLoad}
              onError={handleImageError}
            />
          </>
        ) : (
          <ScrollArea className='max-h-[600px]'>
            <div className='py-4'>
              <div className='bg-muted/50 relative flex min-h-[300px] items-center justify-center rounded-lg border'>
                {/* Skeleton - show when loading or error */}
                {(isLoading || hasError) && (
                  <Skeleton className='absolute inset-0 h-full w-full rounded-lg' />
                )}

                {/* Actual Image */}
                <img
                  key={source}
                  src={source}
                  alt={t('Generated image')}
                  className={`max-h-[550px] w-full rounded-lg object-contain ${
                    isLoading || hasError ? 'opacity-0' : 'opacity-100'
                  }`}
                  onLoad={handleImageLoad}
                  onError={handleImageError}
                  loading='lazy'
                />

                {/* Error text overlay (shown on skeleton) */}
                {hasError && (
                  <div className='absolute inset-0 flex items-center justify-center'>
                    <p className='text-muted-foreground text-sm'>
                      {t('Failed to load image')}
                    </p>
                  </div>
                )}
              </div>

              {/* Image URL */}
              <div className='bg-muted mt-4 rounded-md p-3'>
                <p className='text-muted-foreground font-mono text-xs break-all'>
                  {source?.startsWith('data:') ? t('Embedded image') : source}
                </p>
                {source && (
                  <CopyButton value={source} size='sm'>
                    {t('Copy URL')}
                  </CopyButton>
                )}
              </div>
            </div>
          </ScrollArea>
        )}
      </div>
    </Dialog>
  )
}
