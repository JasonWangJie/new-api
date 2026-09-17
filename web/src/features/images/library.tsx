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
import { useEffect, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'

import { ConfirmDialog } from '@/components/confirm-dialog'
import { CopyButton } from '@/components/copy-button'
import { Dialog } from '@/components/dialog'
import { SectionPageLayout } from '@/components/layout'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Checkbox } from '@/components/ui/checkbox'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { useAuthStore } from '@/stores/auth-store'

import { imageRequest } from './api'
import { ImageResults } from './components/image-results'
import { imageLabel } from './lib/image-labels'
import { imageFileExtension } from './lib/image-request'
import {
  deleteLocalImage,
  imageChecksum,
  listLocalImages,
  saveLocalImage,
} from './lib/local-images'
import type {
  CursorPage,
  ImageMetadata,
  ImageSubmission,
  LocalImage,
} from './types'

export function ImageLibrary() {
  const userId = useAuthStore((state) => state.auth.user?.id)
  return userId ? <LibrarySession key={userId} userId={userId} /> : null
}

function LibrarySession({ userId }: { userId: number }) {
  const { t } = useTranslation()
  const queryClient = useQueryClient()
  const [q, setQ] = useState('')
  const [platform, setPlatform] = useState('')
  const [selected, setSelected] = useState<LocalImage | null>(null)
  const [deleting, setDeleting] = useState<LocalImage | null>(null)
  const [urls, setUrls] = useState<Record<string, string>>({})
  const [syncing, setSyncing] = useState('')
  const local = useQuery({
    queryKey: ['local-image-library', userId],
    queryFn: () => listLocalImages(userId),
  })
  const pending = useQuery({
    queryKey: ['local-image-pending', userId],
    queryFn: () => listLocalImages(userId, 'pending'),
  })
  const submissions = useQuery({
    queryKey: ['image-submissions', userId],
    queryFn: ({ signal }) =>
      imageRequest<CursorPage<ImageSubmission>>(
        '/api/user/image-library/submission-requests?limit=100',
        'GET',
        undefined,
        signal
      ),
    refetchInterval: 30000,
  })
  useEffect(() => {
    const next: Record<string, string> = {}
    for (const item of local.data || []) {
      next[item.id] = URL.createObjectURL(item.blob)
    }
    setUrls(next)
    return () => {
      for (const url of Object.values(next)) URL.revokeObjectURL(url)
    }
  }, [local.data])
  const refresh = async () => {
    await Promise.all([
      queryClient.invalidateQueries({
        queryKey: ['local-image-library', userId],
      }),
      queryClient.invalidateQueries({
        queryKey: ['local-image-pending', userId],
      }),
      queryClient.invalidateQueries({
        queryKey: ['image-submissions', userId],
      }),
    ])
  }
  const items = (local.data || []).filter(
    (item) =>
      (!platform || item.metadata.platform === platform) &&
      `${item.metadata.model} ${item.metadata.title} ${item.metadata.private_prompt}`
        .toLowerCase()
        .includes(q.toLowerCase())
  )
  const reusePrompt = (item: LocalImage) => {
    sessionStorage.setItem(
      `new-api:image-prompt:${userId}`,
      item.metadata.private_prompt
    )
    window.location.assign('/image-workbench')
  }
  const sync = async (submission: ImageSubmission, file?: File) => {
    setSyncing(submission.id)
    try {
      const original = (pending.data || []).find(
        (item) => item.metadata.client_blob_key === submission.client_blob_key
      )
      const blob = file || original?.blob
      if (!blob) {
        throw new Error(
          t(
            'Original image is missing. Select the exact original file to sync.'
          )
        )
      }
      if (
        (await imageChecksum(blob)) !== submission.checksum ||
        blob.size !== submission.byte_size ||
        blob.type !== submission.content_type
      ) {
        throw new Error(
          t('The selected file does not match the approved original.')
        )
      }
      const form = new FormData()
      form.append('file', blob, `original.${imageFileExtension(blob.type)}`)
      await imageRequest(
        `/api/user/image-library/submission-requests/${submission.id}/sync`,
        'POST',
        form
      )
      if (original) await deleteLocalImage(userId, original.id, 'pending')
      await refresh()
      toast.success(t('Published'))
    } catch (error) {
      toast.error(
        error instanceof Error ? error.message : t('Image request failed')
      )
    } finally {
      setSyncing('')
    }
  }
  const withdraw = async (submission: ImageSubmission) => {
    try {
      await imageRequest(
        `/api/user/image-library/submission-requests/${submission.id}`,
        'DELETE'
      )
      const original = pending.data?.find(
        (item) => item.metadata.client_blob_key === submission.client_blob_key
      )
      if (original) await deleteLocalImage(userId, original.id, 'pending')
      await refresh()
    } catch (error) {
      toast.error(
        error instanceof Error ? error.message : t('Image request failed')
      )
    }
  }
  return (
    <SectionPageLayout>
      <SectionPageLayout.Title>
        {t('Local Image Library')}
      </SectionPageLayout.Title>
      <SectionPageLayout.Actions>
        <Button variant='outline' onClick={() => void refresh()}>
          {t('Refresh')}
        </Button>
        <a href='/image-workbench' className='text-sm underline'>
          {t('Image Workbench')}
        </a>
      </SectionPageLayout.Actions>
      <SectionPageLayout.Content>
        <div className='space-y-6'>
          <div className='bg-muted/30 flex flex-wrap items-center justify-between gap-3 rounded-xl border p-4'>
            <div>
              <p className='text-sm font-medium'>{t('Saved on this device')}</p>
              <p className='text-muted-foreground mt-1 text-xs'>
                {t(
                  '30 days · 100 images · 200 MiB per user. Clearing site data removes these images.'
                )}
              </p>
            </div>
            <Badge variant='outline'>
              {local.data?.length || 0} / 100 ·{' '}
              {(
                (local.data || []).reduce(
                  (total, item) => total + item.blob.size,
                  0
                ) / 1048576
              ).toFixed(1)}{' '}
              MiB
            </Badge>
          </div>
          <div className='flex flex-wrap gap-2'>
            <Input
              aria-label={t('Search images')}
              placeholder={t('Search images')}
              value={q}
              onChange={(event) => setQ(event.target.value)}
              className='sm:max-w-sm'
            />
            {['', 'openai', 'gemini'].map((value) => (
              <Button
                key={value}
                size='sm'
                variant={platform === value ? 'default' : 'outline'}
                onClick={() => setPlatform(value)}
              >
                {value || t('All')}
              </Button>
            ))}
          </div>
          {local.isLoading && <p role='status'>{t('Loading...')}</p>}
          {local.isError && (
            <p role='alert' className='text-destructive text-sm'>
              {t('Local image storage is unavailable.')} {local.error.message}
            </p>
          )}
          <ImageResults
            images={items
              .filter((item) => urls[item.id])
              .map((item) => ({
                id: item.id,
                url: urls[item.id],
                title: item.metadata.title || item.metadata.model,
                description: `${item.metadata.platform} · ${new Date(item.createdAt).toLocaleString()} · ${(item.blob.size / 1048576).toFixed(1)} MiB`,
              }))}
            empty={t('Realtime images saved on this device will appear here.')}
            actions={(id) => {
              const item = items.find((candidate) => candidate.id === id)
              return item ? (
                <div className='flex flex-wrap gap-1'>
                  <CopyButton value={item.metadata.private_prompt} size='sm'>
                    {t('Copy prompt')}
                  </CopyButton>
                  <Button
                    size='sm'
                    variant='ghost'
                    onClick={() => reusePrompt(item)}
                  >
                    {t('Use prompt')}
                  </Button>
                  <Button
                    size='sm'
                    variant='outline'
                    onClick={() => setSelected(item)}
                  >
                    {t('Submit for review')}
                  </Button>
                  <Button
                    size='sm'
                    variant='ghost'
                    onClick={() => setDeleting(item)}
                  >
                    {t('Delete')}
                  </Button>
                </div>
              ) : null
            }}
          />
          <section className='space-y-3 border-t pt-5'>
            <div className='flex items-center justify-between'>
              <h3 className='font-semibold'>{t('My submissions')}</h3>
              <span className='text-muted-foreground text-xs'>
                {t('Pending originals have a separate storage quota.')}
              </span>
            </div>
            {submissions.isError && (
              <p role='alert' className='text-destructive text-sm'>
                {submissions.error.message}
              </p>
            )}
            {(pending.data || [])
              .filter(
                (item) => item.submissionOperationKey && !item.submissionId
              )
              .map((item) => (
                <article
                  key={item.id}
                  className='bg-card flex flex-wrap items-center justify-between gap-3 rounded-xl border p-4'
                >
                  <p className='text-sm'>
                    {item.metadata.public_title || item.metadata.title}
                  </p>
                  <Button
                    size='sm'
                    variant='outline'
                    onClick={() => setSelected(item)}
                  >
                    {t('Retry submission')}
                  </Button>
                </article>
              ))}
            {submissions.data?.items.map((submission) => (
              <article
                key={submission.id}
                className='bg-card space-y-3 rounded-xl border p-4'
              >
                <div className='flex flex-wrap items-center justify-between gap-2'>
                  <p className='font-mono text-xs'>{submission.id}</p>
                  <Badge variant='outline'>
                    {t(imageLabel(submission.status))}
                  </Badge>
                </div>
                {submission.reason && (
                  <p className='text-muted-foreground text-sm'>
                    {submission.reason}
                  </p>
                )}
                {submission.status === 'approved_pending_sync' && (
                  <div className='space-y-2'>
                    <p className='text-muted-foreground text-xs'>
                      {t(
                        'Approved for sync. The image becomes public only after the original is verified and uploaded.'
                      )}
                    </p>
                    <div className='flex flex-wrap gap-2'>
                      <Button
                        size='sm'
                        disabled={syncing === submission.id}
                        onClick={() => void sync(submission)}
                      >
                        {t('Sync original')}
                      </Button>
                      <label className='text-muted-foreground text-xs'>
                        {t('Select exact original')}
                        <Input
                          type='file'
                          accept='image/png,image/jpeg,image/webp'
                          disabled={!!syncing}
                          onChange={(event) => {
                            const file = event.target.files?.[0]
                            if (file) void sync(submission, file)
                            event.target.value = ''
                          }}
                        />
                      </label>
                    </div>
                  </div>
                )}
                {['pending_review', 'approved_pending_sync'].includes(
                  submission.status
                ) && (
                  <Button
                    size='sm'
                    variant='outline'
                    disabled={!!syncing}
                    onClick={() => void withdraw(submission)}
                  >
                    {t('Withdraw')}
                  </Button>
                )}
              </article>
            ))}
          </section>
          {selected && (
            <DeferredSubmissionDialog
              key={selected.id}
              image={selected}
              onClose={() => setSelected(null)}
              onSuccess={refresh}
            />
          )}
          <ConfirmDialog
            open={!!deleting}
            onOpenChange={(open) => {
              if (!open) setDeleting(null)
            }}
            title={t('Delete local image')}
            desc={t(
              'Pending submission originals are kept separately until withdrawal or expiry.'
            )}
            destructive
            handleConfirm={() => {
              if (!deleting) return
              void deleteLocalImage(userId, deleting.id)
                .then(refresh)
                .then(() => setDeleting(null))
                .catch((error: Error) => toast.error(error.message))
            }}
          />
        </div>
      </SectionPageLayout.Content>
    </SectionPageLayout>
  )
}

function DeferredSubmissionDialog({
  image,
  onClose,
  onSuccess,
}: {
  image: LocalImage
  onClose: () => void
  onSuccess: () => Promise<void>
}) {
  const { t } = useTranslation()
  const [title, setTitle] = useState(
    image.metadata.public_title || image.metadata.title
  )
  const [share, setShare] = useState(false)
  const [busy, setBusy] = useState(false)
  const operationKey = useRef<string>(crypto.randomUUID())
  const submittedMetadata = useRef<ImageMetadata | null>(null)
  const submit = async () => {
    setBusy(true)
    try {
      const pending = (await listLocalImages(image.userId, 'pending')).find(
        (item) =>
          item.id === image.id &&
          item.submissionOperationKey &&
          !item.submissionId
      )
      if (pending?.submissionOperationKey) {
        operationKey.current = pending.submissionOperationKey
      }
      const metadata = pending?.metadata ||
        submittedMetadata.current || {
          ...image.metadata,
          public_title: title,
          share_prompt: share,
          checksum_sha256: await imageChecksum(image.blob),
          byte_size: image.blob.size,
          content_type: image.blob.type,
        }
      submittedMetadata.current = metadata
      const owner = () => useAuthStore.getState().auth.user?.id === image.userId
      await saveLocalImage(
        {
          ...image,
          metadata,
          submissionOperationKey: operationKey.current,
          expiresAt: pending?.expiresAt || Date.now() + 90 * 86400000,
        },
        'pending',
        owner
      )
      const response = await imageRequest<{ item: ImageSubmission }>(
        '/api/user/image-library/submission-requests',
        'POST',
        metadata,
        undefined,
        { 'Idempotency-Key': operationKey.current }
      )
      if (!owner()) return
      await saveLocalImage(
        {
          ...image,
          metadata,
          submissionId: response.item.id,
          submissionOperationKey: operationKey.current,
          expiresAt: pending?.expiresAt || Date.now() + 90 * 86400000,
        },
        'pending',
        owner
      )
      await onSuccess()
      onClose()
      toast.success(t('Submitted for review'))
    } catch (error) {
      toast.error(
        error instanceof Error ? error.message : t('Image request failed')
      )
    } finally {
      setBusy(false)
    }
  }
  return (
    <Dialog
      open
      onOpenChange={(open) => {
        if (!open && !busy) onClose()
      }}
      title={t('Submit for review')}
      description={t(
        'Only metadata is sent now. Keep the original on this device until approved sync.'
      )}
      footer={
        <Button disabled={busy} onClick={() => void submit()}>
          {busy ? t('Submitting') : t('Submit')}
        </Button>
      }
    >
      <div className='space-y-4'>
        <Label htmlFor='submission-title'>{t('Public title')}</Label>
        <Input
          id='submission-title'
          value={title}
          onChange={(event) => setTitle(event.target.value)}
          maxLength={1000}
          disabled={busy || !!submittedMetadata.current}
        />
        <div className='flex items-center gap-2'>
          <Checkbox
            id='submission-share'
            checked={share}
            onCheckedChange={setShare}
            disabled={busy || !!submittedMetadata.current}
          />
          <Label htmlFor='submission-share'>{t('Share prompt publicly')}</Label>
        </div>
        <p className='text-muted-foreground text-sm'>
          {t('Prompt sharing is off by default.')}
        </p>
      </div>
    </Dialog>
  )
}
