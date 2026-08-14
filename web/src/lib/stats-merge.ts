// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// /stats + /user/stats 响应桶跨维度行合并（rewrite spec 2026-08-14 评审 P1 钉死，
// 管理端/用户端两页共用同一实现）：后端按 (bucket_time, group, account, template,
// model, is_error) 返回多行——同一时间桶的多维度行按 BucketTime 合并求和。
// TTFT 合并语义：avg = Σ(avg×count)/Σcount 加权（TTFTCount 为分母）、
// max = max、**pN = 取请求量最大维度行的 pN**（分位数不可跨行合并——近似，
// 注释写明；请求量并列取先到的行）。Cost 为 API 边界已换算的 USD。
import type { components } from '@/lib/api/schema'

export type StatBucket = components['schemas']['StatBucket']
export type Granularity = 'hour' | 'day'

const pad2 = (n: number) => String(n).padStart(2, '0')

export interface BucketRow {
  time: string
  label: string
  RequestCount: number
  ErrorCount: number
  CallCount: number // 按次调用（图片生成张数、search 次数）
  InputTokens: number
  OutputTokens: number
  CacheReadTokens: number
  CacheCreationTokens: number
  TotalTokens: number
  Cost: number // USD（API 边界已 /1e5）
  TTFTCount: number // 加权 avg 分母（Σcount）
  TTFTAvgMS: number // 加权平均（合并后 Σ(avg×count)/Σcount）
  TTFTMaxMS: number // max
  TTFTP50MS: number // 请求量最大维度行的 pN（近似）
  TTFTP95MS: number
  TTFTP99MS: number
}

export function mergeBuckets(rows: StatBucket[], granularity: Granularity): BucketRow[] {
  const map = new Map<string, BucketRow>()
  const pnSrc = new Map<string, StatBucket>() // pN 近似来源：请求量最大维度行
  for (const r of rows) {
    if (!r.BucketTime) continue
    let b = map.get(r.BucketTime)
    if (!b) {
      const d = new Date(r.BucketTime)
      b = {
        time: r.BucketTime,
        label: granularity === 'hour'
          ? `${pad2(d.getHours())}:${pad2(d.getMinutes())}`
          : `${pad2(d.getMonth() + 1)}-${pad2(d.getDate())}`,
        RequestCount: 0, ErrorCount: 0, CallCount: 0, InputTokens: 0, OutputTokens: 0,
        CacheReadTokens: 0, CacheCreationTokens: 0, TotalTokens: 0,
        Cost: 0, TTFTCount: 0, TTFTAvgMS: 0, TTFTMaxMS: 0, TTFTP50MS: 0, TTFTP95MS: 0, TTFTP99MS: 0,
      }
      map.set(r.BucketTime, b)
    }
    const prev = pnSrc.get(r.BucketTime)
    if (!prev || (r.RequestCount ?? 0) > (prev.RequestCount ?? 0)) {
      pnSrc.set(r.BucketTime, r) // 请求量并列取先到的行
    }
    b.RequestCount += r.RequestCount ?? 0
    b.ErrorCount += r.ErrorCount ?? 0
    b.CallCount += r.CallCount ?? 0
    b.InputTokens += r.InputTokens ?? 0
    b.OutputTokens += r.OutputTokens ?? 0
    b.CacheReadTokens += r.CacheReadTokens ?? 0
    b.CacheCreationTokens += r.CacheCreationTokens ?? 0
    b.TotalTokens += r.TotalTokens ?? 0
    b.Cost += r.Cost ?? 0
    b.TTFTCount += r.TTFTCount ?? 0
    b.TTFTAvgMS += (r.TTFTAvgMS ?? 0) * (r.TTFTCount ?? 0) // 加权和，最后统一除 Σcount
    b.TTFTMaxMS = Math.max(b.TTFTMaxMS, r.TTFTMaxMS ?? 0)
  }
  const out: BucketRow[] = []
  for (const b of map.values()) {
    if (b.TTFTCount > 0) {
      b.TTFTAvgMS = b.TTFTAvgMS / b.TTFTCount
    } else {
      b.TTFTAvgMS = 0
    }
    const src = pnSrc.get(b.time)
    b.TTFTP50MS = src?.TTFTP50MS ?? 0
    b.TTFTP95MS = src?.TTFTP95MS ?? 0
    b.TTFTP99MS = src?.TTFTP99MS ?? 0
    out.push(b)
  }
  return out.sort((a, b) => a.time.localeCompare(b.time))
}

// TTFT 卡范围汇总（rewrite spec 2026-08-14：按 mergeBuckets 同款合并语义——
// avg = Σ(avg×count)/Σcount 加权、pN = 取请求量最大桶的 pN 近似；无样本 = 0）。
export function summarizeTTFT(rows: BucketRow[]): { avg: number; p95: number; p99: number } {
  let count = 0
  let avg = 0
  let p95 = 0
  let p99 = 0
  let maxReq = -1
  for (const r of rows) {
    count += r.TTFTCount
    avg += r.TTFTAvgMS * r.TTFTCount
    if (r.RequestCount > maxReq) {
      maxReq = r.RequestCount
      p95 = r.TTFTP95MS
      p99 = r.TTFTP99MS
    }
  }
  if (count > 0) {
    avg = avg / count
  } else {
    avg = 0
  }
  return { avg, p95, p99 }
}
