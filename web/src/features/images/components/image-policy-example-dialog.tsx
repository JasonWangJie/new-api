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
import { Braces } from 'lucide-react'
import { useTranslation } from 'react-i18next'

import { CopyButton } from '@/components/copy-button'
import { Dialog } from '@/components/dialog'
import { Button } from '@/components/ui/button'

import { imageLabel } from '../lib/image-labels'

type ImageModelCapabilityExample = {
  id: string
  label: string
  media_type: 'image'
  qualities?: string[]
  resolutions: string[]
  formats?: string[]
  backgrounds?: string[]
  max_output_images: number
  max_reference_images: number
  allow_half_k: boolean
}

const OPENAI_MODEL_EXAMPLE: ImageModelCapabilityExample[] = [
  {
    id: 'gpt-image-2',
    label: 'gpt-image-2',
    media_type: 'image',
    qualities: ['low', 'medium', 'high'],
    resolutions: ['1K', '2K', '4K'],
    formats: ['png', 'jpeg', 'webp'],
    backgrounds: ['auto', 'opaque', 'transparent'],
    max_output_images: 128,
    max_reference_images: 14,
    allow_half_k: false,
  },
]

const GEMINI_MODEL_EXAMPLE: ImageModelCapabilityExample[] = [
  {
    id: 'gemini-3-pro-image-preview',
    label: 'Gemini 3 Pro Image',
    media_type: 'image',
    resolutions: ['1K', '2K', '4K'],
    max_output_images: 1,
    max_reference_images: 14,
    allow_half_k: false,
  },
  {
    id: 'gemini-2.5-flash-image',
    label: 'Gemini 2.5 Flash Image',
    media_type: 'image',
    resolutions: ['1K', '2K', '4K'],
    max_output_images: 1,
    max_reference_images: 14,
    allow_half_k: false,
  },
]

const VERTEX_MODEL_EXAMPLE: ImageModelCapabilityExample[] = [
  {
    id: 'gemini-2.5-flash-image',
    label: 'Gemini 2.5 Flash Image',
    media_type: 'image',
    resolutions: ['1K', '2K', '4K'],
    max_output_images: 1,
    max_reference_images: 14,
    allow_half_k: false,
  },
  {
    id: 'imagen-4.0-generate-001',
    label: 'Imagen 4',
    media_type: 'image',
    resolutions: ['1K', '2K'],
    max_output_images: 1,
    max_reference_images: 0,
    allow_half_k: false,
  },
]

const PROVIDER_MODEL_EXAMPLES: Record<string, string> = {
  advanced_custom: 'private-image-model',
  ali: 'qwen-image',
  azure: 'gpt-image-2',
  jimeng: 'jimeng_high_aes_general_v21_L',
  minimax: 'image-01',
  moonshot: 'private-image-model',
  newapi: 'gpt-image-2',
  openrouter: 'gpt-image-2',
  replicate: 'black-forest-labs/flux-1.1-pro',
  siliconflow: 'Kwai-Kolors/Kolors',
  sub2api: 'gpt-image-2',
  volcengine: 'doubao-seedream-4-0-250828',
  xai: 'grok-2-image-1212',
  xinference: 'gpt-image-2',
  zhipu_v4: 'cogview-4',
}

const REFERENCE_IMAGE_PROVIDERS = new Set([
  'advanced_custom',
  'ali',
  'azure',
  'moonshot',
  'newapi',
  'openrouter',
  'replicate',
  'sub2api',
  'volcengine',
  'xinference',
])

function imagePolicyExample(platform: string): string {
  const normalizedPlatform = platform.trim().toLowerCase()
  if (normalizedPlatform === 'openai') {
    return JSON.stringify(OPENAI_MODEL_EXAMPLE, null, 2)
  }
  if (normalizedPlatform === 'gemini') {
    return JSON.stringify(GEMINI_MODEL_EXAMPLE, null, 2)
  }
  if (normalizedPlatform === 'vertex') {
    return JSON.stringify(VERTEX_MODEL_EXAMPLE, null, 2)
  }

  const model =
    PROVIDER_MODEL_EXAMPLES[normalizedPlatform] ||
    `${normalizedPlatform || 'provider'}-image-model`
  const example: ImageModelCapabilityExample[] = [
    {
      id: model,
      label: model,
      media_type: 'image',
      resolutions: ['1K', '2K', '4K'],
      max_output_images: 1,
      max_reference_images: REFERENCE_IMAGE_PROVIDERS.has(normalizedPlatform)
        ? 14
        : 0,
      allow_half_k: false,
    },
  ]
  return JSON.stringify(example, null, 2)
}

export function ImagePolicyExampleDialog(props: { platform: string }) {
  const { t } = useTranslation()
  const example = imagePolicyExample(props.platform)
  const platformLabel = t(imageLabel(props.platform))
  const normalizedPlatform = props.platform.trim().toLowerCase()
  const isGemini = normalizedPlatform === 'gemini'
  const isVertex = normalizedPlatform === 'vertex'
  const title = t('Model capability example for {{platform}}', {
    platform: platformLabel,
  })

  return (
    <Dialog
      trigger={
        <Button type='button' variant='outline' size='sm'>
          <Braces aria-hidden='true' />
          <span
            className='max-w-32 truncate font-mono text-[0.7rem]'
            title={platformLabel}
          >
            {platformLabel}
          </span>
          <span aria-hidden='true'>·</span>
          {t('Configuration example')}
        </Button>
      }
      title={title}
      description={t(
        'Copy this JSON into Model capabilities, then replace model IDs and limits with the exact values supported by your channels.'
      )}
      contentClassName='sm:max-w-3xl'
      contentHeight='min(32rem, calc(100vh - 16rem))'
      bodyClassName='space-y-4'
      footer={
        <CopyButton
          value={example}
          variant='outline'
          size='default'
          className='w-full sm:w-auto'
          iconClassName='mr-1 size-4'
          tooltip={t('Copy example')}
          successTooltip={t('Copied!')}
          aria-label={t('Copy example')}
        >
          {t('Copy example')}
        </CopyButton>
      }
    >
      <div className='border-border bg-muted/35 overflow-hidden rounded-xl border'>
        <div className='border-border text-muted-foreground flex items-center justify-between border-b px-4 py-2 text-xs'>
          <span>{t('Model capabilities')}</span>
          <span
            className='max-w-[60%] truncate font-mono'
            title={platformLabel}
          >
            {platformLabel}
          </span>
        </div>
        <pre
          role='region'
          aria-label={title}
          className='max-h-96 overflow-auto p-4 font-mono text-xs leading-6'
        >
          <code>{example}</code>
        </pre>
      </div>
      <div className='text-muted-foreground space-y-2 text-sm leading-6'>
        <p>
          {t(
            'Optional arrays control which quality, resolution, format and background choices appear in the image workbench.'
          )}
        </p>
        {isGemini && (
          <p>
            {t(
              'Gemini-native models generate one image per request. Keep max_output_images at 1.'
            )}
          </p>
        )}
        {isVertex && (
          <p>
            {t(
              'Gemini-native models generate one image per request. Keep max_output_images at 1; Imagen models do not accept reference images.'
            )}
          </p>
        )}
        <p>
          {t(
            'This example is a template only and does not change the current form.'
          )}
        </p>
      </div>
    </Dialog>
  )
}
