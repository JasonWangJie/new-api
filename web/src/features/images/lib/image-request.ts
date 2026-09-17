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
import type { ImageModel } from '../types'

export const IMAGE_RATIOS = [
  'auto',
  '1:1',
  '3:2',
  '2:3',
  '4:3',
  '3:4',
  '16:9',
  '9:16',
  '5:4',
  '4:5',
  '21:9',
  '9:21',
  '2:1',
  '1:2',
]
export const OPENAI_SIZES: Record<string, string[]> = {
  '1:1': ['1024x1024', '2048x2048', '4096x4096'],
  '3:2': ['1536x1024', '2048x1152', '4096x2304'],
  '16:9': ['1536x1024', '2048x1152', '4096x2304'],
  '2:3': ['1024x1536', '1152x2048', '2304x4096'],
  '9:16': ['1024x1536', '1152x2048', '2304x4096'],
  '4:3': ['1360x1024', '2048x1536', '4096x3072'],
  '3:4': ['1024x1360', '1536x2048', '3072x4096'],
  '5:4': ['1280x1024', '2048x1632', '4096x3272'],
  '4:5': ['1024x1280', '1632x2048', '3272x4096'],
  '21:9': ['2384x1024', '2048x880', '4096x1752'],
  '9:21': ['1024x2384', '880x2048', '1752x4096'],
  '2:1': ['2048x1024', '2048x1024', '4096x2048'],
  '1:2': ['1024x2048', '1024x2048', '2048x4096'],
}
export type ImageRequestPart =
  | { type: 'text'; text: string }
  | { type: 'image_url'; image_url: { url: string } }
export type ImageRequestOptions = {
  model: ImageModel
  prompt: string
  parts: ImageRequestPart[]
  resolution: string
  ratio: string
  count: number
  quality: string
  format: string
  background: string
}

export function imageFileExtension(mime: string): string {
  return (
    (
      {
        'image/jpeg': 'jpg',
        'image/png': 'png',
        'image/webp': 'webp',
      } as Record<string, string>
    )[mime] || 'png'
  )
}

export function buildImageRequest(options: ImageRequestOptions): {
  url: string
  body: string
} {
  const {
    model,
    prompt,
    parts,
    resolution,
    ratio,
    count,
    quality,
    format,
    background,
  } = options
  const references = parts.filter((part) => part.type === 'image_url')
  if (
    !model.available ||
    !prompt.trim() ||
    !Number.isInteger(count) ||
    count < 1 ||
    count > Math.min(128, model.capability.max_output_images) ||
    references.length > model.capability.max_reference_images ||
    !IMAGE_RATIOS.includes(ratio)
  ) {
    throw new Error('Invalid image request')
  }
  if (model.platform === 'gemini' && count !== 1) {
    throw new Error('Gemini accepts one request at a time')
  }
  const ordered: ImageRequestPart[] = [
    { type: 'text', text: prompt.trim() },
    ...parts,
  ]
  if (model.platform === 'gemini' && model.mode === 'realtime') {
    return {
      url: `/v1beta/models/${encodeURIComponent(model.id)}:generateContent`,
      body: JSON.stringify({
        contents: [
          {
            role: 'user',
            parts: ordered.map((part) => {
              if (part.type === 'text') return { text: part.text }
              if (part.image_url.url.startsWith('data:')) {
                const match =
                  /^data:(image\/(?:png|jpeg|webp));base64,(.+)$/.exec(
                    part.image_url.url
                  )
                if (!match) throw new Error('Unsupported image format')
                return { inlineData: { mimeType: match[1], data: match[2] } }
              }
              return {
                fileData: {
                  fileUri: part.image_url.url,
                  mimeType: 'image/png',
                },
              }
            }),
          },
        ],
        generationConfig: {
          responseModalities: ['TEXT', 'IMAGE'],
          imageConfig: {
            imageSize: resolution,
            ...(ratio === 'auto' ? {} : { aspectRatio: ratio }),
          },
        },
      }),
    }
  }
  if (model.platform === 'gemini') {
    if (model.protocol !== 'gemini_bb') {
      return {
        url: '/v1/images/generations_sc',
        body: JSON.stringify({
          model: model.id,
          prompt: ordered
            .filter((part) => part.type === 'text')
            .map((part) => part.text)
            .join('\n'),
          resolution,
          ...(ratio === 'auto' ? {} : { aspect_ratio: ratio }),
          image_urls: references.map((part) => part.image_url.url),
        }),
      }
    }
    return {
      url: '/v1/chat/completions_gm',
      body: JSON.stringify({
        model: model.id,
        stream: false,
        messages: [{ role: 'user', content: ordered }],
        extra_body: {
          google: {
            image_config: {
              image_size: resolution,
              ...(ratio === 'auto' ? {} : { aspect_ratio: ratio }),
            },
          },
        },
      }),
    }
  }
  const fields = {
    model: model.id,
    prompt: ordered
      .filter((part) => part.type === 'text')
      .map((part) => part.text)
      .join('\n'),
    n: count,
    ...(quality ? { quality } : {}),
    ...(format ? { output_format: format } : {}),
    ...(background ? { background } : {}),
  }
  if (model.mode === 'async') {
    return {
      url: '/v1/images/generations_oa',
      body: JSON.stringify({
        ...fields,
        resolution,
        aspect_ratio: ratio,
        image_urls: references.map((part) => part.image_url.url),
      }),
    }
  }
  const size =
    ratio === 'auto'
      ? 'auto'
      : OPENAI_SIZES[ratio]?.[['1K', '2K', '4K'].indexOf(resolution)]
  if (!size) throw new Error('Unsupported image dimensions')
  return {
    url: references.length ? '/v1/images/edits' : '/v1/images/generations',
    body: JSON.stringify({
      ...fields,
      size,
      ...(references.length
        ? {
            images: references.map((part) => ({
              image_url: part.image_url.url,
            })),
          }
        : {}),
    }),
  }
}

export function extractImageResponse(
  response: Record<string, unknown>
): string[] {
  const urls: string[] = []
  if (Array.isArray(response.data)) {
    for (const item of response.data as { url?: string; b64_json?: string }[]) {
      if (item.url) urls.push(item.url)
      else if (item.b64_json) {
        const prefix = item.b64_json.slice(0, 16)
        let mime = 'image/png'
        if (prefix.startsWith('/9j/')) {
          mime = 'image/jpeg'
        } else if (prefix.startsWith('UklGR')) {
          mime = 'image/webp'
        }
        urls.push(`data:${mime};base64,${item.b64_json}`)
      }
    }
  }
  if (Array.isArray(response.candidates)) {
    for (const candidate of response.candidates as {
      content?: {
        parts?: {
          inlineData?: { data?: string; mimeType?: string }
          inline_data?: { data?: string; mime_type?: string }
        }[]
      }
    }[]) {
      for (const part of candidate.content?.parts || []) {
        const data = part.inlineData?.data || part.inline_data?.data
        const mime = part.inlineData?.mimeType || part.inline_data?.mime_type
        if (data && mime?.startsWith('image/')) {
          urls.push(`data:${mime};base64,${data}`)
        }
      }
    }
  }
  return urls
}

export function terminalImageStatus(
  status: string,
  nextAttemptAt = 0
): boolean {
  if (
    (status === 'storage_failed' || status === 'billing_failed') &&
    nextAttemptAt > 0
  ) {
    return false
  }
  return [
    'succeeded',
    'failed',
    'expired',
    'execution_unknown',
    'storage_failed',
    'billing_failed',
  ].includes(status)
}
