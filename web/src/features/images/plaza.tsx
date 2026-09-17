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
import { useInfiniteQuery } from '@tanstack/react-query'
import { useState } from 'react'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'

import { CopyButton } from '@/components/copy-button'
import { Dialog } from '@/components/dialog'
import { SectionPageLayout } from '@/components/layout'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Textarea } from '@/components/ui/textarea'
import { useAuthStore } from '@/stores/auth-store'

import { imageRequest } from './api'
import { ImageResults } from './components/image-results'
import { ImageSelect } from './components/image-select'
import { imageLabel } from './lib/image-labels'
import type { CursorPage, ImagePublication } from './types'

export function ImagePlaza() {
  const { t } = useTranslation()
  const userId = useAuthStore((state) => state.auth.user?.id)
  const [q, setQ] = useState('')
  const [platform, setPlatform] = useState('')
  const [sort, setSort] = useState('newest')
  const [report, setReport] = useState<ImagePublication | null>(null)
  const publications = useInfiniteQuery({
    queryKey: ['image-plaza', userId, q, platform, sort],
    initialPageParam: '',
    queryFn: ({ pageParam, signal }) =>
      imageRequest<CursorPage<ImagePublication>>(
        `/api/image-plaza?${new URLSearchParams({ q, platform, sort, cursor: pageParam, limit: '9' })}`,
        'GET',
        undefined,
        signal
      ),
    getNextPageParam: (page) => page.next_cursor || undefined,
  })
  const items = publications.data?.pages.flatMap((page) => page.items) || []
  const withdraw = async (item: ImagePublication) => {
    try {
      await imageRequest(
        `/api/user/image-library/${item.asset_id}/publication`,
        'DELETE'
      )
      await publications.refetch()
    } catch (error) {
      toast.error(
        error instanceof Error ? error.message : t('Image request failed')
      )
    }
  }
  return (
    <SectionPageLayout>
      <SectionPageLayout.Title>{t('Image Plaza')}</SectionPageLayout.Title>
      <SectionPageLayout.Actions>
        <a href='/image-workbench' className='text-sm underline'>
          {t('Create an image')}
        </a>
      </SectionPageLayout.Actions>
      <SectionPageLayout.Content>
        <div className='space-y-6'>
          <header className='border-b pb-5'>
            <p className='text-muted-foreground mb-2 text-xs font-semibold tracking-widest'>
              {t('COMMUNITY GALLERY')}
            </p>
            <h3 className='font-serif text-3xl tracking-tight'>
              {t('Images worth sharing')}
            </h3>
            <p className='text-muted-foreground mt-2 text-sm'>
              {t(
                'Approved works from the community. Prompts appear only when their creators choose to share them.'
              )}
            </p>
          </header>
          <div className='grid gap-3 sm:grid-cols-[1fr_180px_180px]'>
            <div className='space-y-2'>
              <Label htmlFor='plaza-search'>{t('Search images')}</Label>
              <Input
                id='plaza-search'
                value={q}
                onChange={(event) => setQ(event.target.value)}
                placeholder={t('Search images')}
              />
            </div>
            <ImageSelect
              label={t('Platform')}
              value={platform}
              options={[
                { value: '', label: t('All') },
                { value: 'openai', label: 'OpenAI' },
                { value: 'gemini', label: 'Gemini' },
              ]}
              onChange={setPlatform}
            />
            <ImageSelect
              label={t('Sort')}
              value={sort}
              options={[
                { value: 'newest', label: t('Newest first') },
                { value: 'oldest', label: t('Oldest first') },
              ]}
              onChange={setSort}
            />
          </div>
          {publications.isLoading && <p role='status'>{t('Loading...')}</p>}
          {publications.isError && (
            <div role='alert' className='text-destructive text-sm'>
              {publications.error.message}
              <Button
                variant='outline'
                size='sm'
                onClick={() => void publications.refetch()}
              >
                {t('Retry')}
              </Button>
            </div>
          )}
          <ImageResults
            images={items.map((item) => ({
              id: item.id,
              url: item.image_url || '',
              title: item.title,
              description: `${item.creator || ''} · ${item.model || ''}`,
            }))}
            empty={t('No published images yet.')}
            actions={(id) => {
              const item = items.find((candidate) => candidate.id === id)
              return item ? (
                <div className='space-y-2'>
                  {item.prompt && (
                    <p className='text-muted-foreground line-clamp-3 text-xs'>
                      {item.prompt}
                    </p>
                  )}
                  <div className='flex flex-wrap gap-1'>
                    {item.prompt && (
                      <>
                        <CopyButton value={item.prompt} size='sm'>
                          {t('Copy prompt')}
                        </CopyButton>
                        <Button
                          size='sm'
                          variant='ghost'
                          onClick={() => {
                            if (!userId) return
                            sessionStorage.setItem(
                              `new-api:image-prompt:${userId}`,
                              item.prompt || ''
                            )
                            window.location.assign('/image-workbench')
                          }}
                        >
                          {t('Use prompt')}
                        </Button>
                      </>
                    )}
                    <Button
                      size='sm'
                      variant='outline'
                      onClick={() => setReport(item)}
                    >
                      {t('Report')}
                    </Button>
                    {item.is_owner && (
                      <Button
                        size='sm'
                        variant='ghost'
                        onClick={() => void withdraw(item)}
                      >
                        {t('Withdraw')}
                      </Button>
                    )}
                  </div>
                </div>
              ) : null
            }}
          />
          {publications.hasNextPage && (
            <div className='text-center'>
              <Button
                variant='outline'
                disabled={publications.isFetchingNextPage}
                onClick={() => void publications.fetchNextPage()}
              >
                {t('Load more')}
              </Button>
            </div>
          )}
          {report && (
            <ImageReportDialog
              key={report.id}
              publication={report}
              onClose={() => setReport(null)}
            />
          )}
        </div>
      </SectionPageLayout.Content>
    </SectionPageLayout>
  )
}

function ImageReportDialog({
  publication,
  onClose,
}: {
  publication: ImagePublication
  onClose: () => void
}) {
  const { t } = useTranslation()
  const [reason, setReason] = useState('other')
  const [details, setDetails] = useState('')
  const [busy, setBusy] = useState(false)
  const submit = async () => {
    setBusy(true)
    try {
      await imageRequest(`/api/image-plaza/${publication.id}/reports`, 'POST', {
        reason,
        details,
      })
      onClose()
      toast.success(t('Report submitted'))
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
      title={t('Report image')}
      description={publication.title}
      footer={
        <Button disabled={busy} onClick={() => void submit()}>
          {t('Submit report')}
        </Button>
      }
    >
      <div className='space-y-4'>
        <ImageSelect
          label={t('Reason')}
          value={reason}
          options={[
            'copyright',
            'privacy',
            'illegal',
            'inappropriate',
            'other',
          ].map((value) => ({ value, label: t(imageLabel(value)) }))}
          onChange={setReason}
          disabled={busy}
        />
        <Label htmlFor='report-details'>{t('Details')}</Label>
        <Textarea
          id='report-details'
          value={details}
          onChange={(event) => setDetails(event.target.value)}
          maxLength={10000}
          disabled={busy}
        />
      </div>
    </Dialog>
  )
}
