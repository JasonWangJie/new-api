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

import { CopyButton } from '@/components/copy-button'
import { StaticDataTable } from '@/components/data-table'
import { SectionPageLayout } from '@/components/layout'
import { useStatus } from '@/hooks/use-status'

import {
  ASYNC_IMAGE_ENDPOINTS,
  asyncImageGuide,
  IMAGE_ERROR_CODES,
} from './lib/guide-content'
import { OPENAI_SIZES } from './lib/image-request'

export function AsyncImageGuide() {
  const { t, i18n } = useTranslation()
  const { status } = useStatus()
  const base = (
    typeof status?.server_address === 'string'
      ? status.server_address
      : window.location.origin
  )
    .replace(/\/+$/, '')
    .replace(/\/v1$/, '')
  const chinese = i18n.language.startsWith('zh')
  const sections = asyncImageGuide(chinese, base)
  return (
    <SectionPageLayout>
      <SectionPageLayout.Title>
        {t('Async Image API Guide')}
      </SectionPageLayout.Title>
      <SectionPageLayout.Actions>
        <CopyButton value={base} size='sm'>
          {t('Copy Base URL')}
        </CopyButton>
      </SectionPageLayout.Actions>
      <SectionPageLayout.Content>
        <div className='grid gap-8 lg:grid-cols-[200px_minmax(0,900px)]'>
          <nav
            aria-label={t('On this page')}
            className='lg:sticky lg:top-2 lg:self-start'
          >
            <p className='text-muted-foreground mb-3 text-xs font-semibold'>
              {t('On this page')}
            </p>
            <ol className='flex flex-wrap gap-x-4 gap-y-2 lg:block lg:space-y-2'>
              {sections.map((section) => (
                <li key={section.id}>
                  <a
                    className='text-muted-foreground hover:text-foreground text-sm underline-offset-4 hover:underline'
                    href={`#image-guide-${section.id}`}
                  >
                    {section.title}
                  </a>
                </li>
              ))}
            </ol>
          </nav>
          <article className='min-w-0 space-y-9 pb-8'>
            <header className='border-b pb-6'>
              <p className='text-muted-foreground mb-2 text-xs font-semibold tracking-widest'>
                new-api · IMAGE API
              </p>
              <h1 className='font-serif text-3xl'>
                {t('Async Image API Guide')}
              </h1>
              <p className='text-muted-foreground mt-3 font-mono text-xs break-all'>
                {base}
              </p>
            </header>
            <StaticDataTable
              data={[...ASYNC_IMAGE_ENDPOINTS]}
              getRowKey={(row) => row[1]}
              columns={[
                {
                  id: 'method',
                  header: t('Method'),
                  cell: (row) => <code>{row[0]}</code>,
                },
                {
                  id: 'path',
                  header: t('Endpoint'),
                  cell: (row) => (
                    <code className='text-xs break-all'>{row[1]}</code>
                  ),
                },
                {
                  id: 'protocol',
                  header: t('Protocol'),
                  cell: (row) => row[2],
                },
              ]}
            />
            {sections.map((section) => (
              <section
                key={section.id}
                id={`image-guide-${section.id}`}
                className='scroll-mt-6 space-y-4'
              >
                <h2 className='text-xl font-semibold tracking-tight'>
                  {section.title}
                </h2>
                {section.paragraphs.map((paragraph) => (
                  <p
                    key={paragraph}
                    className='text-muted-foreground text-sm leading-7'
                  >
                    {paragraph}
                  </p>
                ))}
                {section.examples.map((example) => (
                  <div
                    key={example}
                    className='bg-muted/40 overflow-hidden rounded-xl border'
                  >
                    <div className='flex items-center justify-end border-b px-3 py-2'>
                      <CopyButton value={example} size='sm'>
                        {t('Copy example')}
                      </CopyButton>
                    </div>
                    <pre className='overflow-x-auto p-4 font-mono text-xs leading-6'>
                      <code>{example}</code>
                    </pre>
                  </div>
                ))}
                {section.id === 'dimensions' && (
                  <StaticDataTable
                    data={Object.entries(OPENAI_SIZES)}
                    getRowKey={(row) => row[0]}
                    columns={[
                      {
                        id: 'ratio',
                        header: t('Aspect Ratio'),
                        cell: (row) => row[0],
                      },
                      ...['1K', '2K', '4K'].map((tier, index) => ({
                        id: tier,
                        header: tier,
                        cell: (row: [string, string[]]) => (
                          <code className='text-xs'>{row[1][index]}</code>
                        ),
                      })),
                    ]}
                  />
                )}
                {section.id === 'errors' && (
                  <StaticDataTable
                    data={[...IMAGE_ERROR_CODES]}
                    getRowKey={(row) => row[0]}
                    columns={[
                      { id: 'code', header: t('Code'), cell: (row) => row[0] },
                      {
                        id: 'meaning',
                        header: t('Description'),
                        cell: (row) => (chinese ? row[2] : row[1]),
                      },
                    ]}
                  />
                )}
              </section>
            ))}
          </article>
        </div>
      </SectionPageLayout.Content>
    </SectionPageLayout>
  )
}
