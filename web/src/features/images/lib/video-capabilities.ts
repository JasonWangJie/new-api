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
import type {
  VideoDurationCapability,
  VideoInputCapability,
  VideoModeCapability,
  VideoModeName,
  VideoModel,
} from '../types'

export type VideoSelection = {
  mode: VideoModeName
  seconds: string
  resolution: string
  aspectRatio: string
  extensionDirection: string
  inputs: Record<string, string>
  options: Record<string, boolean>
}

export function videoDurationForResolution(
  mode: VideoModeCapability,
  resolution: string
): VideoDurationCapability | undefined {
  const selected = resolution || mode.defaultResolution || ''
  return mode.durationsByResolution?.[selected] ?? mode.duration
}

export function videoDurationAllows(
  duration: VideoDurationCapability,
  seconds: number
): boolean {
  if (!Number.isInteger(seconds)) return false
  if (duration.values) return duration.values.includes(seconds)
  const minimum = duration.min ?? 1
  return (
    seconds >= minimum &&
    seconds <= (duration.max ?? Infinity) &&
    (seconds - minimum) % (duration.step ?? 1) === 0
  )
}

function mergeDurations(
  profiles: (VideoDurationCapability | undefined)[],
  maximumSeconds: number
): VideoDurationCapability | undefined {
  const durations = new Set<number>()
  for (const profile of profiles) {
    if (!profile) continue
    if (profile.values) {
      for (const value of profile.values) durations.add(value)
      continue
    }
    const maximum = Math.min(profile.max ?? maximumSeconds, maximumSeconds)
    for (
      let value = profile.min ?? 1;
      value <= maximum;
      value += profile.step ?? 1
    ) {
      durations.add(value)
    }
  }
  if (!durations.size) return undefined
  const values = [...durations].sort((a, b) => a - b)
  return { values, default: profiles[0]?.default ?? values[0] }
}

export function videoCapabilityModes(model: VideoModel): VideoModeCapability[] {
  if (
    model.routing_platform !== 'mixed' ||
    !model.capability_variants?.length
  ) {
    return model.capability.modes
  }
  const variants = model.capability_variants.flatMap((profile) => profile.modes)
  return [...new Set(variants.map((mode) => mode.name))].map((name) => {
    const modes = variants.filter((mode) => mode.name === name)
    const first = modes[0]
    const inputs: VideoInputCapability[] = []
    for (const inputName of new Set(
      modes.flatMap((mode) => (mode.inputs || []).map((input) => input.name))
    )) {
      const candidates = modes.flatMap((mode) =>
        (mode.inputs || []).filter((input) => input.name === inputName)
      )
      inputs.push({
        ...candidates[0],
        sources: [...new Set(candidates.flatMap((input) => input.sources))],
        maxItems: Math.max(...candidates.map((input) => input.maxItems)),
        required: modes.every((mode) =>
          mode.inputs?.some(
            (input) => input.name === inputName && input.required
          )
        ),
      })
    }
    const resolutions = [
      ...new Set(modes.flatMap((mode) => mode.resolutions || [])),
    ]
    const durationsByResolution: Record<string, VideoDurationCapability> = {}
    if (modes.some((mode) => mode.durationsByResolution)) {
      for (const resolution of resolutions) {
        const profiles = modes
          .filter((mode) => mode.resolutions?.includes(resolution))
          .map((mode) => videoDurationForResolution(mode, resolution))
        const duration = mergeDurations(profiles, model.max_duration_seconds)
        if (duration) durationsByResolution[resolution] = duration
      }
    }
    return {
      ...first,
      duration: mergeDurations(
        modes.map((mode) => mode.duration),
        model.max_duration_seconds
      ),
      durationsByResolution,
      resolutions,
      aspectRatios: [
        ...new Set(modes.flatMap((mode) => mode.aspectRatios || [])),
      ],
      extensionDirections: [
        ...new Set(modes.flatMap((mode) => mode.extensionDirections || [])),
      ],
      inputs,
      options: {
        generateAudio: modes.some((mode) => mode.options?.generateAudio),
        watermark: modes.some((mode) => mode.options?.watermark),
        returnLastFrame: modes.some((mode) => mode.options?.returnLastFrame),
      },
    }
  })
}

// Match a whole provider profile, rather than independent unions of dimensions.
export function videoSelectionMatches(
  mode: VideoModeCapability,
  selection: VideoSelection,
  uploads: Record<string, File[]>
): boolean {
  if (mode.name !== selection.mode) return false
  if (selection.seconds) {
    const duration = videoDurationForResolution(mode, selection.resolution)
    const seconds = Number(selection.seconds)
    if (!duration || !videoDurationAllows(duration, seconds)) return false
  }
  if (
    selection.resolution &&
    !mode.resolutions?.includes(selection.resolution)
  ) {
    return false
  }
  if (
    selection.aspectRatio &&
    !mode.aspectRatios?.includes(selection.aspectRatio)
  ) {
    return false
  }
  if (
    selection.extensionDirection &&
    !mode.extensionDirections?.includes(
      selection.extensionDirection as 'forward' | 'backward'
    )
  ) {
    return false
  }
  const supportedOptions: Record<string, boolean | undefined> = {
    generate_audio: mode.options?.generateAudio,
    watermark: mode.options?.watermark,
    return_last_frame: mode.options?.returnLastFrame,
  }
  if (Object.keys(selection.options).some((name) => !supportedOptions[name])) {
    return false
  }
  for (const inputName of new Set([
    ...Object.keys(selection.inputs),
    ...Object.keys(uploads),
    ...(mode.inputs || []).map((input) => input.name),
  ])) {
    const lines = (selection.inputs[inputName] || '')
      .split('\n')
      .map((line) => line.trim())
      .filter(Boolean)
    const files = uploads[inputName] || []
    const input = mode.inputs?.find((candidate) => candidate.name === inputName)
    const count = lines.length + files.length
    if (!input) {
      if (count > 0) return false
      continue
    }
    if (
      (input.required && count === 0) ||
      count > input.maxItems ||
      (files.length > 0 && !input.sources.includes('upload'))
    ) {
      return false
    }
    for (const line of lines) {
      let source: VideoInputCapability['sources'][number]
      if (/^https?:\/\//i.test(line)) source = 'url'
      else if (/^asset:\/\//i.test(line)) source = 'asset'
      else if (/^data:/i.test(line)) source = 'data_uri'
      else {
        try {
          const value: unknown = JSON.parse(line)
          if (!value || typeof value !== 'object' || Array.isArray(value)) {
            return false
          }
          if ('file_id' in value) source = 'file_id'
          else if ('voice_id' in value) source = 'voice_id'
          else if ('url' in value && typeof value.url === 'string') {
            if (/^https?:\/\//i.test(value.url)) source = 'url'
            else if (/^asset:\/\//i.test(value.url)) source = 'asset'
            else if (/^data:/i.test(value.url)) source = 'data_uri'
            else return false
          } else return false
        } catch {
          return false
        }
      }
      if (!input.sources.includes(source)) return false
    }
  }
  return true
}
