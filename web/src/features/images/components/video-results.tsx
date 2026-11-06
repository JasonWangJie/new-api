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
import { useEffect, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'

import { CopyButton } from '@/components/copy-button'
import { ErrorState } from '@/components/error-state'
import { LoadingState } from '@/components/loading-state'
import { Button } from '@/components/ui/button'
import { ArtifactMedia } from '@/features/usage-logs/components/task-artifacts'
import { useAuthStore } from '@/stores/auth-store'

import { authenticatedMediaBlob } from '../api'
import type { ImageResult } from '../types'

type VideoResultProps = {
  result: ImageResult
  onRefresh: () => void
  onRefreshUpstream?: () => void
}

function AuthenticatedVideoResult(props: VideoResultProps) {
  const userId = useAuthStore((state) => state.auth.user?.id)
  const path = props.result.url || props.result.view_url
  const media = useQuery({
    queryKey: ['authenticated-video-result', userId, path],
    queryFn: ({ signal }) => authenticatedMediaBlob(path, signal),
    retry: false,
    gcTime: 0,
    staleTime: Infinity,
    meta: { errorToast: false },
  })
  const [preview, setPreview] = useState<{ blob: Blob; url: string }>()
  useEffect(() => {
    if (!media.data) return
    const url = URL.createObjectURL(media.data)
    setPreview({ blob: media.data, url })
    return () => URL.revokeObjectURL(url)
  }, [media.data])
  const refresh = () => {
    void media.refetch()
  }
  if (media.isError) return <ErrorState onRetry={refresh} />
  if (!preview || preview.blob !== media.data) return <LoadingState />
  return (
    <VideoResult
      result={{ ...props.result, url: preview.url, view_url: preview.url }}
      onRefresh={refresh}
      onRefreshUpstream={refresh}
    />
  )
}

function VideoResult(props: VideoResultProps) {
  const { t } = useTranslation()
  const result = props.result
  const mediaUrl = result.url || result.view_url
  const failedUrl = useRef<string | null>(null)
  const [mediaRevision, setMediaRevision] = useState(0)
  const refresh = () => {
    if (result.source === 'upstream') {
      props.onRefreshUpstream?.()
      return
    }
    // A refetched task can retain the same file URL after a transfer failure.
    if (failedUrl.current === mediaUrl) {
      failedUrl.current = null
      setMediaRevision((revision) => revision + 1)
    }
    props.onRefresh()
  }
  return (
    <div className='space-y-3'>
      <ArtifactMedia
        key={mediaRevision}
        artifact={{
          key: String(result.id),
          type: 'video',
          content_url: mediaUrl,
        }}
        mediaUrl={mediaUrl}
        onError={() => {
          failedUrl.current = mediaUrl
          props.onRefresh()
        }}
      />
      <div className='flex flex-wrap items-center gap-2'>
        <Button
          variant='outline'
          size='sm'
          nativeButton={false}
          render={<a href={mediaUrl} download />}
        >
          {t('Download')}
        </Button>
        {!result.authenticated && (
          <CopyButton value={mediaUrl}>{t('Copy link')}</CopyButton>
        )}
        {(result.source !== 'upstream' || props.onRefreshUpstream) && (
          <Button variant='ghost' size='sm' onClick={refresh}>
            {t('Refresh link')}
          </Button>
        )}
        <span className='text-muted-foreground text-xs'>
          {result.source === 'upstream'
            ? t('Upstream link')
            : `${(result.byte_size / 1048576).toFixed(2)} MiB`}
        </span>
      </div>
    </div>
  )
}

export function VideoResults(props: {
  results: ImageResult[]
  onRefresh: () => void
  onRefreshUpstream?: () => void
}) {
  return (
    <div className='space-y-6'>
      {props.results.map((result) =>
        result.authenticated ? (
          <AuthenticatedVideoResult
            key={result.id}
            result={result}
            onRefresh={props.onRefresh}
            onRefreshUpstream={props.onRefreshUpstream}
          />
        ) : (
          <VideoResult
            key={result.id}
            result={result}
            onRefresh={props.onRefresh}
            onRefreshUpstream={props.onRefreshUpstream}
          />
        )
      )}
    </div>
  )
}
