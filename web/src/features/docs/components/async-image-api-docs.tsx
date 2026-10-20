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

import { PublicLayout } from '@/components/layout/components/public-layout'
import { RichContent } from '@/components/rich-content'

import apiDocumentation from '../data/async-image-api.zh.md?raw'

export function AsyncImageApiDocs() {
  const { t } = useTranslation()

  return (
    <PublicLayout>
      <article
        aria-label={t('Async image API docs')}
        lang='zh-CN'
        className='mx-auto max-w-5xl min-w-0 py-6 sm:py-10 [&_h1]:scroll-mt-24 [&_h2]:scroll-mt-24 [&_h3]:scroll-mt-24 [&_h4]:scroll-mt-24'
      >
        <RichContent
          mode='markdown'
          content={apiDocumentation}
          documentNavigation
          fragmentLinksOnly
        />
      </article>
    </PublicLayout>
  )
}
