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
import { describe, expect, it } from 'vitest'

import type { ImageModel } from '../../types'
import { ASYNC_IMAGE_ENDPOINTS, asyncImageGuide } from '../guide-content'
import {
  buildImageRequest,
  extractImageResponse,
  terminalImageStatus,
} from '../image-request'

const model: ImageModel = {
  id: 'image-model',
  label: 'Image model',
  platform: 'openai',
  mode: 'async',
  protocol: 'openai_async',
  available: true,
  capability: { max_output_images: 128, max_reference_images: 3 },
}
const options = {
  model,
  prompt: 'A vase',
  parts: [],
  resolution: '1K',
  ratio: '21:9',
  count: 1,
  quality: 'high',
  format: 'png',
  background: 'opaque',
}
describe('capability-driven image request contracts', () => {
  it('documents every asynchronous media path with valid standalone examples', () => {
    for (const chinese of [true, false]) {
      const examples = asyncImageGuide(
        chinese,
        'https://gateway.example'
      ).flatMap((section) => section.examples.map((example) => example.code))
      for (const example of examples) {
        if (example.startsWith('{')) {
          expect(() => JSON.parse(example)).not.toThrow()
        }
        expect(example).not.toMatch(/\n\+\s+-H/)
        const body = /--data '([\s\S]+)'$/.exec(example)?.[1]
        if (body) expect(() => JSON.parse(body)).not.toThrow()
      }
      for (const [, path] of ASYNC_IMAGE_ENDPOINTS) {
        const examplePath = path
          .replace('{task_id}', 'YOUR_TASK_ID')
          .replace('{object_id}', 'YOUR_OBJECT_ID')
        expect(examples.join('\n')).toContain(examplePath)
      }
      const video = examples
        .filter((example) => example.startsWith('{'))
        .map((example) => JSON.parse(example))
        .find((example) => example.media_type === 'video')
      expect(video).toMatchObject({
        protocol: 'openai_video',
        storage_status: 'succeeded',
        data: [{ content_type: 'video/mp4', url: expect.any(String) }],
      })
      const failed = examples
        .filter((example) => example.startsWith('{'))
        .map((example) => JSON.parse(example))
        .find((example) => example.status === 'failed')
      expect(failed).toMatchObject({
        error_code: 601,
        fail_reason: expect.any(String),
      })
      expect(failed).not.toHaveProperty('error')
    }
  })
  it('explains every video example and keeps reference images distinct from pinned first frames', () => {
    const video = asyncImageGuide(true, 'https://gateway.example').find(
      (section) => section.id === 'video'
    )
    expect(video).toBeDefined()
    const examples = video?.examples ?? []
    for (const example of examples) {
      expect(example.title).toBeTruthy()
      expect(example.description).toBeTruthy()
    }
    const bodies = examples.flatMap((example) => {
      const body = /--data '([\s\S]+)'$/.exec(example.code)?.[1]
      return body ? [JSON.parse(body) as Record<string, unknown>] : []
    })
    expect(bodies).toContainEqual(
      expect.objectContaining({
        provider: 'xai',
        image: { url: 'https://cdn.example/reference.png' },
      })
    )
    expect(bodies).toContainEqual(
      expect.objectContaining({
        provider: 'drama',
        model: 'seedance2.0-A',
        seconds: 4,
        resolution: '480p',
      })
    )
    expect(bodies).toContainEqual(
      expect.objectContaining({
        provider: 'drama',
        model: 'seedance2.5-A',
        prompt: expect.stringContaining('@图1'),
        reference_images: ['https://cdn.example/character.png'],
        reference_videos: ['https://cdn.example/motion.mp4'],
        reference_audios: ['https://cdn.example/voice.mp3'],
      })
    )
    expect(bodies).toContainEqual(
      expect.objectContaining({
        provider: 'drama',
        model: 'seedance2.0-A',
        image: 'https://cdn.example/first.png',
        last_frame: 'https://cdn.example/last.png',
        aspect_ratio: 'Auto',
      })
    )
    expect(bodies).toContainEqual(
      expect.objectContaining({
        provider: 'drama',
        model: 'minimax-h3',
        generate_audio: false,
        reference_images: ['https://cdn.example/character.png'],
      })
    )
    expect(bodies).toContainEqual(
      expect.objectContaining({
        provider: 'drama',
        model: 'seedance2.5-F',
        reference_images: ['https://cdn.example/character.png'],
      })
    )
    expect(bodies).toContainEqual(
      expect.objectContaining({
        provider: 'drama',
        model: 'seedance2.5-E',
        reference_videos: ['https://cdn.example/motion.mp4'],
      })
    )
    expect(bodies).toContainEqual(
      expect.objectContaining({
        provider: 'drama',
        model: 'seedance2.5-E',
        references: [
          {
            type: 'image',
            role: 'reference',
            source: 'https://cdn.example/character.png',
          },
          {
            type: 'video',
            role: 'reference',
            source: 'https://cdn.example/motion.mp4',
          },
          {
            type: 'audio',
            role: 'reference',
            source: 'https://cdn.example/voice.mp3',
          },
        ],
      })
    )
    expect(bodies).toContainEqual(
      expect.objectContaining({
        provider: 'drama',
        model: 'seedance2.5-G',
        image: 'https://cdn.example/first.png',
        last_frame: 'https://cdn.example/last.png',
        aspect_ratio: 'adaptive',
      })
    )
  })
  it('sends explicit async aliases and native realtime dimensions without switching mode', () => {
    const async = buildImageRequest(options)
    expect(async.url).toBe('/v1/images/generations_oa')
    expect(JSON.parse(async.body)).toMatchObject({
      resolution: '1K',
      aspect_ratio: '21:9',
      n: 1,
    })
    const realtime = buildImageRequest({
      ...options,
      model: { ...model, mode: 'realtime' },
    })
    expect(realtime.url).toBe('/v1/images/generations')
    expect(JSON.parse(realtime.body).size).toBe('2384x1024')
  })
  it.each([
    { resolution: '1K', ratio: '16:9' },
    { resolution: '1K', ratio: '4:3' },
    { resolution: '2K', ratio: '4:3' },
    { resolution: '2K', ratio: 'auto' },
  ])(
    'preserves $resolution and $ratio for New API models declaring output_resolution',
    ({ resolution, ratio }) => {
      const request = buildImageRequest({
        ...options,
        resolution,
        ratio,
        model: {
          ...model,
          provider: 'newapi',
          supported_parameters: ['output_resolution', 'aspect_ratio'],
          supports_reference_generation: true,
        },
        parts: [
          {
            type: 'image_url',
            image_url: { url: 'data:image/png;base64,aW1hZ2U=' },
          },
        ],
      })
      const body = JSON.parse(request.body)
      expect(request.url).toBe('/v1/images/generations_async')
      expect(body).toMatchObject({
        resolution,
        images: [{ image_url: 'data:image/png;base64,aW1hZ2U=' }],
      })
      expect(body).not.toHaveProperty('size')
      if (ratio === 'auto') expect(body).not.toHaveProperty('aspect_ratio')
      else expect(body.aspect_ratio).toBe(ratio)
    }
  )
  it.each([
    { provider: 'newapi', supported_parameters: undefined },
    { provider: 'openai', supported_parameters: ['output_resolution'] },
  ])(
    'keeps pixel sizes for $provider without the New API output_resolution capability',
    ({ provider, supported_parameters }) => {
      const request = buildImageRequest({
        ...options,
        ratio: '16:9',
        model: { ...model, provider, supported_parameters },
      })
      const body = JSON.parse(request.body)
      expect(body.size).toBe('1536x1024')
      expect(body).not.toHaveProperty('resolution')
      expect(body).not.toHaveProperty('aspect_ratio')
    }
  )
  it('rejects unavailable capabilities, zero or excessive counts and excessive reference images', () => {
    for (const count of [0, 129, Number.NaN]) {
      expect(() => buildImageRequest({ ...options, count })).toThrow()
    }
    expect(() =>
      buildImageRequest({ ...options, model: { ...model, available: false } })
    ).toThrow()
    expect(() =>
      buildImageRequest({
        ...options,
        parts: Array.from({ length: 4 }, () => ({
          type: 'image_url' as const,
          image_url: { url: 'https://example.com/reference.png' },
        })),
      })
    ).toThrow()
  })
  it('keeps BB alternating parts in order and honors SC protocol capabilities', () => {
    const parts = [
      {
        type: 'image_url' as const,
        image_url: { url: 'https://example.com/reference.png' },
      },
      { type: 'text' as const, text: 'Change the light' },
    ]
    const bb = buildImageRequest({
      ...options,
      parts,
      model: { ...model, platform: 'gemini', protocol: 'gemini_bb' },
    })
    expect(bb.url).toBe('/v1/chat/completions_gm')
    expect(JSON.parse(bb.body).messages[0].content).toEqual([
      { type: 'text', text: 'A vase' },
      ...parts,
    ])
    const sc = buildImageRequest({
      ...options,
      parts,
      model: { ...model, platform: 'gemini', protocol: 'gemini_sc' },
    })
    expect(sc.url).toBe('/v1/images/generations_sc')
    expect(JSON.parse(sc.body)).toMatchObject({
      prompt: 'A vase\nChange the light',
      image_urls: ['https://example.com/reference.png'],
    })
  })
  it('captures every returned image and keeps retryable post-processing in the polling state', () => {
    expect(
      extractImageResponse({
        data: [
          { url: 'https://example.com/one.png' },
          { url: 'https://example.com/two.png' },
        ],
      })
    ).toHaveLength(2)
    expect(
      extractImageResponse({
        candidates: [
          {
            content: {
              parts: [
                { inlineData: { mimeType: 'image/jpeg', data: '/9j/' } },
                { inlineData: { mimeType: 'image/png', data: 'iVBOR' } },
              ],
            },
          },
        ],
      })
    ).toEqual(['data:image/jpeg;base64,/9j/', 'data:image/png;base64,iVBOR'])
    expect(terminalImageStatus('storage_failed', 1234)).toBe(false)
    expect(terminalImageStatus('storage_failed', 0)).toBe(true)
    expect(terminalImageStatus('execution_unknown')).toBe(true)
  })
})
