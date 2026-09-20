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
export type DocsTextStyles = {
  bold?: boolean
  italic?: boolean
  code?: boolean
  strike?: boolean
}

export type DocsInlineNode =
  | {
      type: 'text'
      text: string
      styles?: DocsTextStyles
    }
  | {
      type: 'link'
      text: string
      href: string
      styles?: DocsTextStyles
    }

export type DocsBlock = {
  id: string
  type: string
  content?: DocsInlineNode[]
  children?: DocsBlock[]
  props?: Record<string, unknown>
}

export type DocsPageContent = {
  blocks: DocsBlock[]
  renderer_version?: number
  schema_version?: number
}

export type DocsPage = {
  id: number
  space_id: number
  space_slug: string
  parent_id: number
  slug: string
  title: string
  summary: string
  icon_key: string
  locale: string
  featured: boolean
  path: string
  content: DocsPageContent
  updated_at?: number
  published_at?: number
}

export type DocsNavNode = {
  type: 'group' | 'page'
  id: number
  slug: string
  title: string
  path?: string
  space_id?: number
  locale?: string
  enabled?: boolean
  parent_id?: number
  sort_key?: number
  children?: DocsNavNode[]
}

export type DocsBundle = {
  space: {
    slug: string
    title: string
    description: string
  }
  defaultPath: string
  navigation: DocsNavNode[]
  pages: Record<string, DocsPage>
}

export type DocsFlatPage = {
  path: string
  title: string
  slug: string
}
