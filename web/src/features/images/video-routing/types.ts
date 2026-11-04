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
export type VideoRoutingTarget = {
  channel_id: number
  model: string
  resolution: string
}

export type VideoRoutingRule = {
  id: string
  name: string
  enabled: boolean
  match: {
    groups: string[]
    models: string[]
    resolutions: string[]
    modes: string[]
    video_inputs: ('none' | 'video')[]
    seconds: {
      min?: number
      max?: number
      min_exclusive: boolean
      max_exclusive: boolean
    }
  }
  targets: VideoRoutingTarget[]
  on_exhausted: 'original' | 'fail'
}

export type VideoRoutingConfig = {
  version: 1
  revision: number
  enabled: boolean
  rules: VideoRoutingRule[]
}

export type VideoRoutingOptions = {
  channels: {
    id: number
    name: string
    models: { model: string; upstream_model: string }[]
  }[]
}

export type ConditionalRoutingPreview = {
  matched_rules: { id: string; name: string }[]
  rule_id: string
  rule_name: string
  source: { model: string; resolution: string; seconds?: number }
  targets: {
    channel_id: number
    model: string
    upstream_model: string
    resolution: string
    eligible: boolean
    reasons: string[]
  }[]
  on_exhausted: 'original' | 'fail'
  selected_target_index?: number
  is_fallback: boolean
}
