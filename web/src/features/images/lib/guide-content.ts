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
export const ASYNC_IMAGE_ENDPOINTS = [
  ['POST', '/v1/images/generations_async', 'JSON / multipart'],
  ['POST', '/v1/images/edits_async', 'JSON / multipart'],
  ['POST', '/v1/videos/generations_async', 'JSON / multipart'],
  ['POST', '/v1/videos/edits_async', 'JSON'],
  ['POST', '/v1/videos/extensions_async', 'JSON'],
  ['GET', '/v1/media/tasks_async/{task_id}', 'JSON'],
  ['GET', '/v1/media/tasks_async/stats', 'JSON'],
] as const

export type MediaGuideExample = {
  code: string
  title: string
  description?: string
}

type MediaGuideSection = {
  id: string
  title: string
  paragraphs: string[]
  parameters?: [string, string][]
  examples: MediaGuideExample[]
}

function mediaRequestExample(
  base: string,
  path: string,
  key: string,
  body: Record<string, unknown>
): string {
  return [
    `curl -i -sS '${base}${path}' \\`,
    '  -H "Authorization: Bearer $API_KEY" \\',
    "  -H 'Content-Type: application/json' \\",
    `  -H 'Idempotency-Key: ${key}-001' \\`,
    `  --data '${JSON.stringify(body, null, 2)}'`,
  ].join('\n')
}

export function asyncImageGuide(
  base: string,
  t: (key: string) => string = (key) => key
): MediaGuideSection[] {
  return [
    {
      id: 'overview',
      title: t('Before you start'),
      paragraphs: [
        t(
          'Replace each YOUR_*_MODEL placeholder with an enabled model that supports the fields in that example. The gateway selects a compatible channel automatically. Example sizes and durations are not universal defaults.'
        ),
        t(
          'Use your gateway API key for submission and polling. Keep task_id and query_url: HTTP 202 means accepted, not completed. Poll with the same user and API key, at least 3 seconds apart.'
        ),
        t(
          'Examples use Bash. BASE_URL has no trailing /v1. In PowerShell, use curl.exe and a UTF-8 JSON file. Replace example media URLs with directly readable HTTPS files.'
        ),
      ],
      examples: [
        {
          title: t('Set your API key'),
          code: `export BASE_URL='${base}'\nexport API_KEY='YOUR_API_KEY'`,
        },
      ],
    },
    {
      id: 'unified-images',
      title: t('Image generation and editing'),
      paragraphs: [
        t(
          'Use generations_async for text-to-image and edits_async when changing a reference image. Both accept JSON and multipart. Edits require a reference image; a mask alone is insufficient.'
        ),
        t(
          'For JSON, image is a reference URL or Data URI; image_urls is an array. For local files, use multipart image=@reference.png and let curl set the boundary. Reference count, masks and geometry must be supported by the selected model.'
        ),
        t(
          'For the official OpenAI Images API, the gateway converts the requested dimensions to size. Supported pixel dimensions depend on the selected model.'
        ),
      ],
      parameters: [
        [
          'model / prompt',
          t('Required strings: an enabled model name and a nonempty prompt.'),
        ],
        [
          'n',
          t(
            'Optional integer, default 1, gateway range 1-128; the model may impose a lower limit.'
          ),
        ],
        [
          'size / resolution / aspect_ratio',
          t(
            'Optional model-specific geometry. Explicit size and resolution must agree; check supported values before sending.'
          ),
        ],
        [
          'quality / background / response_format',
          t(
            'Optional model-specific settings. Asynchronous task results still use data[].url.'
          ),
        ],
      ],
      examples: [
        {
          title: t('Generate an image from text'),
          code: mediaRequestExample(
            base,
            '/v1/images/generations_async',
            'image-text',
            {
              model: 'YOUR_IMAGE_MODEL',
              prompt: 'An orange ball on a pale blue background',
              n: 1,
              size: '1024x1024',
            }
          ),
        },
        {
          title: t('Edit a reference image'),
          code: mediaRequestExample(
            base,
            '/v1/images/edits_async',
            'image-edit',
            {
              model: 'YOUR_IMAGE_EDIT_MODEL',
              prompt: 'Keep the subject and add a soft shadow',
              image: 'https://cdn.example/source.png',
              n: 1,
            }
          ),
        },
      ],
    },
    {
      id: 'gif-svg',
      title: t('Image output options'),
      paragraphs: [
        t(
          'GIF, SVG and frame controls are limited to the OpenAI 2.6+ image family. The current implementation explicitly supports gpt-image-2.6-flare and gpt-image-2.6-sunburst. A newer version or similar model name does not establish support.'
        ),
        t(
          'Add the options below to either image scenario only when the selected model supports them. PNG, WebP and JPEG support also depends on the model. n counts output images, not GIF frames.'
        ),
        t(
          'Frame controls require output_format=gif. Invalid types, out-of-range values or frame controls with another format return HTTP 400. JSON requires integers, not quoted numbers or null.'
        ),
        t(
          'Frame values are requested settings. Actual GIF frame counts and delays can differ upstream; inspect the returned file when exact animation timing matters.'
        ),
      ],
      parameters: [
        [
          'output_format',
          t(
            'Optional string: png (default), webp, jpeg, gif or svg on the supported 2.6 models.'
          ),
        ],
        [
          'frame_count',
          t('GIF only: optional integer, 2-16 frames, default 9.'),
        ],
        [
          'frame_duration_ms',
          t(
            'GIF only: optional integer, 20-2000 milliseconds per frame, default 200.'
          ),
        ],
      ],
      examples: [],
    },
    {
      id: 'video',
      title: t('Video generation scenarios'),
      paragraphs: [
        t(
          'Choose a model for the scenario first. Video examples use integer seconds; do not also send duration. Resolution, aspect ratio, audio and media references vary by model and are not interchangeable.'
        ),
        t(
          'Reference media guide content and motion; image and last_frame pin endpoints only on models with that capability. Send only supported reference kinds. Audio URLs, multiple references and paired frames are not available on every model.'
        ),
      ],
      examples: [
        {
          title: t('Generate a video from text'),
          description: t(
            'Replace the model placeholder with one that accepts 8 seconds, 720p and 16:9, or adjust those values to its capabilities.'
          ),
          code: mediaRequestExample(
            base,
            '/v1/videos/generations_async',
            'video-text',
            {
              model: 'YOUR_VIDEO_MODEL',
              prompt: 'A slow camera move over a quiet lake',
              seconds: 8,
              resolution: '720p',
              aspect_ratio: '16:9',
            }
          ),
        },
        {
          title: t('Generate a video with reference media'),
          description: t(
            'This example requires a model that accepts image, video and audio URL arrays together. Remove unsupported media types and follow any model-specific prompt reference rules.'
          ),
          code: mediaRequestExample(
            base,
            '/v1/videos/generations_async',
            'video-reference',
            {
              model: 'YOUR_VIDEO_REFERENCE_MODEL',
              prompt: 'Use the character, movement and sound in the references',
              seconds: 8,
              resolution: '720p',
              aspect_ratio: '16:9',
              reference_images: ['https://cdn.example/character.png'],
              reference_videos: ['https://cdn.example/motion.mp4'],
              reference_audios: ['https://cdn.example/voice.mp3'],
            }
          ),
        },
        {
          title: t('Generate a video between first and last frames'),
          description: t(
            'Use a model with paired-frame support. image and last_frame are frame URLs. Add an aspect ratio only if that model requires it; do not assume ordinary references pin frames.'
          ),
          code: mediaRequestExample(
            base,
            '/v1/videos/generations_async',
            'video-keyframes',
            {
              model: 'YOUR_VIDEO_KEYFRAME_MODEL',
              prompt: 'Make a smooth transition between the two frames',
              seconds: 8,
              resolution: '720p',
              image: 'https://cdn.example/first.png',
              last_frame: 'https://cdn.example/last.png',
            }
          ),
        },
      ],
    },
    {
      id: 'video-edit',
      title: t('Video editing and extension'),
      paragraphs: [
        t(
          'These operations require models with the matching editing or extension capability. Source duration, geometry and supported directions are model-specific. video_input is a billing field, not a client media object.'
        ),
      ],
      examples: [
        {
          title: t('Edit an existing video'),
          description: t(
            'Supply a directly readable source video URL. Omit generation duration and geometry when the model inherits them from the source.'
          ),
          code: mediaRequestExample(
            base,
            '/v1/videos/edits_async',
            'video-edit',
            {
              model: 'YOUR_VIDEO_EDIT_MODEL',
              prompt: 'Change the sky to a warm sunset',
              video: 'https://cdn.example/source.mp4',
            }
          ),
        },
        {
          title: t('Extend an existing video'),
          description: t(
            'This example requires 6-second backward extension support. seconds is the added duration; adjust the direction and duration to the selected model.'
          ),
          code: mediaRequestExample(
            base,
            '/v1/videos/extensions_async',
            'video-extend',
            {
              model: 'YOUR_VIDEO_EXTEND_MODEL',
              prompt: 'Continue the camera movement',
              video: 'https://cdn.example/source.mp4',
              seconds: 6,
              extension_direction: 'backward',
            }
          ),
        },
      ],
    },
    {
      id: 'query',
      title: t('Poll and download results'),
      paragraphs: [
        t(
          'Poll query_url with the submitting API key. Stages are queued, generating, saving, completed and failed. Download data[].url after stage=completed and status=succeeded, provided result_expired is not true and data is available.'
        ),
        t(
          'Saved image and video links use /v1/media/files/. A successful upstream fallback can have result_source=upstream and storage_status=upstream. Expired results may retain a successful task status while returning empty data.'
        ),
        t(
          'Image result entries provide a URL; video entries may include content_type, byte_size and other metadata. Do not require video-only fields on image responses.'
        ),
      ],
      examples: [
        {
          title: t('Poll a task'),
          code: `curl -sS '${base}/v1/media/tasks_async/YOUR_TASK_ID' \\\n  -H "Authorization: Bearer $API_KEY"`,
        },
        {
          title: t('Accepted task response'),
          code: JSON.stringify(
            {
              task_id: 'asyncimg_example',
              query_url: `${base}/v1/media/tasks_async/asyncimg_example`,
            },
            null,
            2
          ),
        },
        {
          title: t('Completed image response'),
          code: JSON.stringify(
            {
              task_id: 'asyncimg_example',
              media_type: 'image',
              stage: 'completed',
              status: 'succeeded',
              storage_status: 'succeeded',
              data: [
                {
                  url: `${base}/v1/media/files/img_0123456789abcdef0123456789abcdef.svg`,
                },
              ],
            },
            null,
            2
          ),
        },
      ],
    },
    {
      id: 'statistics',
      title: t('Usage statistics'),
      paragraphs: [
        t(
          'Statistics cover the authenticated user across all API keys and workbench tasks. Today means tasks created today, not tasks completed today. Retryable storage or billing failures can be counted as failed.'
        ),
        t(
          'Use /v1/images/tasks_async/stats for images, /v1/videos/tasks_async/stats for videos, or /v1/media/tasks_async/stats for both. Balance and quota-derived amounts use the deployment quota unit, not a guaranteed USD amount.'
        ),
      ],
      examples: [
        {
          title: t('Query combined statistics'),
          code: `curl -sS '${base}/v1/media/tasks_async/stats' \\\n  -H "Authorization: Bearer $API_KEY"`,
        },
      ],
    },
    {
      id: 'errors',
      title: t('Errors and safe retries'),
      paragraphs: [
        t(
          'Use a new Idempotency-Key for each operation and reuse it only for the same request. Image JSON and multipart retries require identical raw bytes; a regenerated multipart boundary can cause a 409 conflict.'
        ),
        t(
          'HTTP 400 means invalid input or unsupported capabilities; 401/403 means access denied, 404 includes task isolation, 409 is an idempotency conflict, 413 is oversized input and 503 means a required service is unavailable.'
        ),
        t(
          'A failed task is different from an HTTP submission error. Image tasks have public codes 601-613; video errors follow their own contract. Execution uncertainty may appear as status=failed. Keep the task ID and recover existing storage instead of generating again.'
        ),
      ],
      examples: [],
    },
    {
      id: 'compatibility',
      title: t('Existing client compatibility'),
      paragraphs: [
        t(
          'Existing /v1/images/generations_oa, /v1/images/edits_oa, /v1/chat/completions_gm and /v1/images/generations_sc remain available with their original request and query envelopes. The unified examples above do not change those contracts.'
        ),
        t(
          'Legacy image clients retain /v1/images/tasks_async/{task_id} and /v1/tasks_sc/{task_id}. Existing video editing and extension routes without _async use their original task query flow. New integrations should use the scenario routes above.'
        ),
      ],
      examples: [],
    },
  ]
}
