// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// mergeBuckets/summarizeTTFT 语义断言（rewrite spec 2026-08-14 评审 P1；
// cache 字段求和断言 2026-08-14 图表增强 spec §5）：
// 跨维度行按 BucketTime 合并的 avg 加权 / max 取大 / pN 近似 / cache 求和数值断言。
// 直跑 TS 源码（node ≥23.6 type stripping；模块为纯函数 + import type，
// 运行期无别名解析）。用法：node scripts/check-stats-merge.mjs
import assert from 'node:assert/strict'
import { mergeBuckets, summarizeTTFT } from '../src/lib/stats-merge.ts'

// —— 同 BucketTime 两维度行（请求量不同）：P1 合并语义 ——
const rows = [
  {
    BucketTime: '2026-08-14T10:00:00Z',
    RequestCount: 5,
    ErrorCount: 1,
    CallCount: 2,
    InputTokens: 10,
    OutputTokens: 20,
    CacheReadTokens: 12,
    CacheCreationTokens: 3,
    TotalTokens: 30,
    Cost: 1.5,
    TTFTCount: 4,
    TTFTAvgMS: 100,
    TTFTMaxMS: 150,
    TTFTP50MS: 90,
    TTFTP95MS: 140,
    TTFTP99MS: 148,
  },
  {
    BucketTime: '2026-08-14T10:00:00Z',
    RequestCount: 10,
    CallCount: 3,
    InputTokens: 40,
    OutputTokens: 50,
    CacheReadTokens: 18,
    CacheCreationTokens: 7,
    TotalTokens: 90,
    Cost: 2.5,
    TTFTCount: 6,
    TTFTAvgMS: 200,
    TTFTMaxMS: 300,
    TTFTP50MS: 180,
    TTFTP95MS: 280,
    TTFTP99MS: 290,
  },
  // 无 TTFT 样本行（仅请求量）——合并后 TTFT 全 0
  { BucketTime: '2026-08-14T11:00:00Z', RequestCount: 2, TotalTokens: 6 },
]

const merged = mergeBuckets(rows, 'hour')
assert.equal(merged.length, 2, '两个时间桶')
// 标签按本地时区生成（既有约定）；10:00Z 的本地小时随机器时区
const localH = new Date('2026-08-14T10:00:00Z').getHours()
assert.equal(merged[0].label, `${String(localH).padStart(2, '0')}:00`, 'hour 粒度标签')
const m = merged[0]
assert.equal(m.RequestCount, 15)
assert.equal(m.CallCount, 5, 'call_count 求和')
assert.equal(m.CacheReadTokens, 30, 'cache read 求和')
assert.equal(m.CacheCreationTokens, 10, 'cache write 求和')
assert.equal(m.Cost, 4.0, 'Cost USD 求和')
assert.equal(m.TTFTCount, 10, 'TTFTCount 求和（加权分母）')
assert.equal(m.TTFTAvgMS, 160, 'avg = Σ(avg×count)/Σcount = (100×4+200×6)/10')
assert.equal(m.TTFTMaxMS, 300, 'max 取大')
assert.equal(m.TTFTP50MS, 180, 'pN 取请求量最大维度行的 pN（近似）')
assert.equal(m.TTFTP95MS, 280)
assert.equal(m.TTFTP99MS, 290)
const empty = merged[1]
assert.equal(empty.TTFTAvgMS, 0, '无样本 → avg 0')
assert.equal(empty.TTFTP95MS, 0, '无样本 → pN 0')
assert.equal(empty.TTFTMaxMS, 0, '无样本 → max 0')
assert.equal(empty.TTFTCount, 0)
assert.equal(empty.CacheReadTokens, 0, '无 cache 字段 → 0')
assert.equal(empty.CacheCreationTokens, 0, '无 cache 字段 → 0')

// —— 请求量并列：取先到的维度行 ——
const tie = mergeBuckets([
  { BucketTime: '2026-08-14T12:00:00Z', RequestCount: 7, TTFTCount: 2, TTFTAvgMS: 50, TTFTMaxMS: 80, TTFTP95MS: 500, TTFTP99MS: 510 },
  { BucketTime: '2026-08-14T12:00:00Z', RequestCount: 7, TTFTCount: 2, TTFTAvgMS: 60, TTFTMaxMS: 90, TTFTP95MS: 600, TTFTP99MS: 610 },
], 'hour')
assert.equal(tie[0].TTFTP95MS, 500, '请求量并列取先到的行')
assert.equal(tie[0].TTFTMaxMS, 90, 'max 仍取大')
assert.equal(tie[0].TTFTAvgMS, 55, 'avg 加权 (50×2+60×2)/4')

// —— day 粒度标签 ——
const day = mergeBuckets([{ BucketTime: '2026-08-14T10:00:00Z', RequestCount: 1 }], 'day')
assert.equal(day[0].label, '08-14', 'day 粒度标签')

// —— summarizeTTFT（TTFT 卡范围汇总：跨桶同款语义）——
const s = summarizeTTFT(merged)
assert.equal(s.avg, 160, '范围 avg = Σ(avg×count)/Σcount')
assert.equal(s.p95, 280, '范围 pN 取请求量最大桶')
assert.equal(s.p99, 290)
assert.equal(summarizeTTFT([]).avg, 0, '空 → 0')

console.log('STATS MERGE OK')
