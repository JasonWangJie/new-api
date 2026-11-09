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
export type FilteredVideoSuccessRate = {
  rate: number | null
  excluded_count: number
  sample_count: number
}

export type PerformanceStats = {
  avg_ttft_ms?: number
  avg_latency_ms: number
  success_rate: number
  avg_tps: number
  request_count?: number
  success_count?: number
  failure_count?: number
  unknown_count?: number
  latency_count?: number
  total_latency_ms?: number
  output_count?: number
  workload?: 'relay' | 'image' | 'video' | 'mixed'
  video_filtered_success_rate?: FilteredVideoSuccessRate
}

export type PerformanceSeriesPoint = PerformanceStats & {
  ts: number
  avg_ttft_ms: number
}

export type PerformanceGroup = PerformanceStats & {
  group: string
  avg_ttft_ms: number
  series: PerformanceSeriesPoint[]
}

export type PerformanceMetricsData = {
  success: boolean
  message?: string
  data: {
    model_name: string
    series_schema?: string
    summary?: PerformanceStats
    window_hours?: number
    updated_at?: number
    video_error_exclusions?: string[]
    groups: PerformanceGroup[]
  }
}

export type SuccessRatePoint = { ts: number; success_rate: number }

export type PerfModelSummary = PerformanceStats & {
  model_name: string
  recent_success_series?: SuccessRatePoint[]
}

export type PerfSummaryAllData = {
  success: boolean
  message?: string
  data: {
    models: PerfModelSummary[]
    window_hours?: number
    updated_at?: number
    video_error_exclusions?: string[]
  }
}
