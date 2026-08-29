// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

import { useState } from 'react'
import { motion } from 'framer-motion'
import {
  ArrowRight,
  BookOpen,
  Check,
  CheckCircle2,
  ChevronDown,
  ClipboardCheck,
  Copy,
  ExternalLink,
  FileText,
  KeyRound,
  LifeBuoy,
  LogIn,
  MessageCircle,
  PlayCircle,
  Ticket,
  WalletCards,
} from 'lucide-react'
import { Link } from 'react-router-dom'
import { useTranslation } from 'react-i18next'
import { Button } from '@/components/ui/button'
import { copyText } from '@/components/key-box'

type TutorialStep = {
  number: string
  icon: typeof BookOpen
  title: string
  summary: string
  actions: string[]
  check: string
  problem?: string
  link?: { to: string; label: string }
}

type TutorialCopy = {
  title: string
  subtitle: string
  badge: string
  endpointTitle: string
  endpointHint: string
  copyEndpoint: string
  copied: string
  beforeStart: string
  beforeStartItems: string[]
  stepsTitle: string
  stepsHint: string
  steps: TutorialStep[]
  callTitle: string
  callIntro: string
  callItems: string[]
  troubleshootingTitle: string
  troubleshooting: Array<{ title: string; detail: string }>
  finishTitle: string
  finishBody: string
  finishLinks: Array<{ to: string; icon: typeof KeyRound; label: string }>
}

const copy: Record<'zh' | 'en', TutorialCopy> = {
  zh: {
    title: '第一次使用？照着这份教程做',
    subtitle: '每一步都写明“点哪里、填什么、看到什么才算成功”。手机浏览器也可以完成全部设置。',
    badge: '晴雨天 · 手机使用指南',
    endpointTitle: '先记住 API 地址',
    endpointHint: '所有兼容 OpenAI 的客户端都要填这个地址。请完整复制，末尾的 /v1 只保留一次。',
    copyEndpoint: '复制 API 地址',
    copied: '已复制',
    beforeStart: '开始前准备',
    beforeStartItems: [
      '准备一个可以收邮件的邮箱；密码建议使用 12 位以上的字母、数字组合。',
      '使用手机浏览器打开本网站，网络切换时先等页面恢复，不要连续重复提交。',
      'API 密钥只在创建成功时显示完整内容，请复制到自己的密码管理器中保存。',
    ],
    stepsTitle: '按顺序完成 6 步',
    stepsHint: '每一项都有完成标准。完成后再进入下一项，遇到问题先看该步骤底部的提示。',
    steps: [
      {
        number: '01', icon: LogIn, title: '注册并登录', summary: '先创建自己的晴雨天账户。',
        actions: [
          '打开登录页，点击“注册”；如果已经有账户，直接输入邮箱和密码登录。',
          '输入邮箱、密码和确认密码。两次密码必须完全一致，输入框右侧没有红色提示才可以提交。',
          '如果页面要求验证码，打开邮箱查看新邮件，复制验证码回到注册页填写。不要关闭原页面。',
          '看到总览页和自己的邮箱后，说明登录成功。',
        ],
        check: '完成标准：页面能看到“总览”、账户余额和底部导航。',
        problem: '收不到验证码时先检查垃圾邮件，再确认邮箱地址没有多余空格；等待一分钟后只重发一次。',
      },
      {
        number: '02', icon: KeyRound, title: '创建 API 密钥', summary: '密钥就是客户端访问晴雨天的密码。',
        actions: [
          '点击底部的“我的密钥”，再点击“新建”。手机上按钮通常在页面右上角。',
          '名称填写便于识别的文字，例如“手机 Claude”或“电脑备用”，不要把真实密码写进名称。',
          '在“分组”里选择管理员开放的公开分组；不知道选哪个时，优先选择渠道监控中状态正常的分组。',
          '并发和额度先保持默认即可，确认后点击“创建”。',
          '弹出完整密钥后，立即点复制，并粘贴到密码管理器或安全的备忘录；离开弹窗后通常只显示前后几位。',
        ],
        check: '完成标准：我的密钥列表出现新名称，状态为“启用”。',
        problem: '没有可选分组时，说明管理员还没有开放公开分组；这不是手机操作错误。',
        link: { to: '/user/keys', label: '打开我的密钥' },
      },
      {
        number: '03', icon: ClipboardCheck, title: '复制 API 地址和密钥', summary: '先复制，再粘贴到客户端，避免手动输入出错。',
        actions: [
          '在“我的密钥”页面找到刚才创建的那一行，点击密钥旁边的复制图标。',
          '点击页面上的 API 地址复制按钮。当前地址会自动跟随你正在访问的网站，不要自己改成 localhost。',
          '粘贴后检查：地址应以 https:// 或 http:// 开头，并以 /v1 结尾；不要在后面再次添加 /v1。',
          '密钥粘贴后如果前后出现空格，请删掉空格；不要把密钥发到群聊、截图或公开仓库。',
        ],
        check: '完成标准：你手边有两段独立内容：API 地址和 sk- 开头的密钥。',
        link: { to: '/user/keys', label: '去复制密钥和地址' },
      },
      {
        number: '04', icon: PlayCircle, title: '在客户端完成配置', summary: '大多数 AI 客户端都使用同一套三项设置。',
        actions: [
          '打开客户端的“设置”或“模型提供商”，选择“自定义 API”或“OpenAI 兼容”。',
          '把 API 地址粘贴到“Base URL / API 地址”；把密钥粘贴到“API Key / Token”。',
          '模型名称从“渠道监控”页面复制，大小写和短横线要保持一致；不要凭感觉输入旧模型名。',
          '点击客户端的“测试连接”或发送一句“你好”。第一次请求可能需要几秒，成功返回文字就表示配置完成。',
        ],
        check: '完成标准：客户端返回正常文字，而不是 401、403、404 或 429。',
        problem: '401/403 通常是密钥粘贴错误；404 多半是地址重复了 /v1；429 先查看渠道状态并稍后重试。',
        link: { to: '/user/models', label: '查看可用模型' },
      },
      {
        number: '05', icon: FileText, title: '查看渠道和消费日志', summary: '先看渠道状态，再核对每一次消费。',
        actions: [
          '进入“渠道监控”，查看公开分组最近的成功率、延迟和错误状态。优先使用成功率高、延迟低的渠道。',
          '发送请求后打开“消费日志”，按时间找到刚才的请求。',
          '逐项核对模型、输入/输出 Token、费用、延迟和请求结果；页面中的错误原因可直接复制给管理员。',
          '如果刚完成请求暂时没有记录，等待几十秒后刷新一次；不要连续点击发送造成重复扣费。',
        ],
        check: '完成标准：能在日志中找到一次成功请求，并看懂它的模型、Token 和费用。',
        problem: '日志为空不代表请求没成功，先确认客户端确实收到了回复，再检查筛选条件和时间范围。',
        link: { to: '/user/logs', label: '打开消费日志' },
      },
      {
        number: '06', icon: Ticket, title: '购买并充值', summary: '兑换码支持一次粘贴多个，逐个返回结果。',
        actions: [
          '进入“充值”，点击卡网购买链接，在卡网完成支付后复制卡密。',
          '回到充值页，把一个或多个卡密粘贴到输入框；每行一个，也可以用空格、逗号分隔。',
          '点击“兑换”。页面会逐个显示成功或失败原因，成功的卡密不会因为其他卡密失败而回滚。',
          '充值完成后回到总览，确认余额数字已经更新；余额显示保留两位小数。',
        ],
        check: '完成标准：兑换结果显示成功，且总览余额增加。',
        problem: '提示已使用、过期或格式错误时，不要重复提交同一张卡；把失败原因和订单号交给管理员核对。',
        link: { to: '/user/redemptions', label: '打开充值页面' },
      },
    ],
    callTitle: '第一次调用的检查清单',
    callIntro: '如果客户端有“高级设置”，只需要确认下面几项：',
    callItems: [
      '协议：OpenAI 兼容（不是 Anthropic 原生协议）。',
      'Base URL：上面复制的地址，包含 /v1，且只出现一次。',
      'API Key：完整粘贴 sk- 开头的密钥，不要加引号。',
      '模型：从渠道监控复制一个当前可用的模型名称。',
      '流式输出：遇到客户端显示空白时先关闭测试；普通请求成功后再打开。',
    ],
    troubleshootingTitle: '常见提示怎么处理',
    troubleshooting: [
      { title: '页面一直转圈', detail: '先检查手机网络，再刷新页面一次；不要同时开多个标签重复提交。' },
      { title: '401 / 未授权', detail: '重新从“我的密钥”复制密钥，确认没有换行和前后空格，并确认密钥状态为启用。' },
      { title: '404 / 找不到地址', detail: '删除客户端里多余的 /v1，确认使用的是本网站地址，而不是旧中转站地址。' },
      { title: '429 / 暂时繁忙', detail: '打开渠道监控换一个正常分组，等待几秒再试；连续重试不会提高成功率。' },
      { title: '余额没有变化', detail: '先查看消费日志是否已记录，统计有延迟时等待后再刷新；不要重复兑换卡密。' },
    ],
    finishTitle: '设置完成后，你只需要记住这三个入口',
    finishBody: '总览看余额和渠道状态；我的密钥复制地址和密钥；消费日志核对每次调用。需要充值时再打开充值页。',
    finishLinks: [
      { to: '/user', icon: WalletCards, label: '回到总览' },
      { to: '/user/keys', icon: KeyRound, label: '我的密钥' },
      { to: '/user/logs', icon: FileText, label: '消费日志' },
    ],
  },
  en: {
    title: 'New here? Follow this guide',
    subtitle: 'Every step says what to tap, what to enter, and what success looks like. You can finish everything on a phone.',
    badge: 'Qingyutian · Mobile guide',
    endpointTitle: 'Save the API address first',
    endpointHint: 'OpenAI-compatible clients use this address. Copy it exactly and keep /v1 only once.',
    copyEndpoint: 'Copy API address', copied: 'Copied',
    beforeStart: 'Before you start',
    beforeStartItems: [
      'Have an email inbox ready. A password with 12 or more letters and numbers is recommended.',
      'Open this site in your phone browser. After a network change, wait for the page to recover before submitting again.',
      'The full API key is shown only after creation. Save it in a password manager immediately.',
    ],
    stepsTitle: 'Complete these 6 steps',
    stepsHint: 'Each step has a completion check. Finish it before moving on, and read the troubleshooting note if needed.',
    steps: [
      { number: '01', icon: LogIn, title: 'Sign up and sign in', summary: 'Create your Qingyutian account.', actions: ['Open the login page and tap Sign up, or enter your email and password if you already have an account.', 'Enter your email, password, and confirmation. Both passwords must match without a red validation message.', 'If a code is requested, check your email and paste it back without closing the page.', 'Seeing your email and the Overview page means sign-in succeeded.'], check: 'Success: Overview, balance, and the bottom navigation are visible.', problem: 'Check spam and remove spaces from the email address. After one minute, resend the code once.' },
      { number: '02', icon: KeyRound, title: 'Create an API key', summary: 'The key is your client password.', actions: ['Open My Keys and tap New. On a phone, the button is usually at the top right.', 'Use a recognizable name such as “Phone Claude”; never put a real password in the name.', 'Choose a public group. If unsure, choose a group with a healthy status in Channel monitor.', 'Leave concurrency and quota at their defaults, then tap Create.', 'When the full secret appears, copy it immediately to a password manager.'], check: 'Success: the new name appears with an Enabled status.', problem: 'No selectable group means the administrator has not opened a public group yet.', link: { to: '/user/keys', label: 'Open My Keys' } },
      { number: '03', icon: ClipboardCheck, title: 'Copy the address and key', summary: 'Copy both values instead of typing them.', actions: ['Find the new row in My Keys and tap the copy icon next to the secret.', 'Tap the API address copy button. It follows the site you are currently viewing; do not change it to localhost.', 'The address should start with http:// or https:// and end with /v1. Do not append /v1 again.', 'Remove spaces around the key and never post it in chats, screenshots, or public repositories.'], check: 'Success: you have two separate values, an API address and an sk- secret.', link: { to: '/user/keys', label: 'Copy key and address' } },
      { number: '04', icon: PlayCircle, title: 'Configure your client', summary: 'Most clients need the same three values.', actions: ['Open Settings or Model provider and choose Custom API or OpenAI-compatible.', 'Paste the address into Base URL and the secret into API Key or Token.', 'Copy a model name from Channel monitor, preserving its case and hyphens.', 'Run Test connection or send “Hello”. A normal text reply means it works.'], check: 'Success: the client returns text instead of 401, 403, 404, or 429.', problem: '401/403 usually means a bad key, 404 often means a duplicate /v1, and 429 means the channel is busy.', link: { to: '/user/models', label: 'View available models' } },
      { number: '05', icon: FileText, title: 'Check channels and usage logs', summary: 'Choose a healthy channel and verify each call.', actions: ['Open Channel monitor and compare success rate, latency, and errors for public groups.', 'After a call, open Usage logs and locate it by time.', 'Check model, input/output tokens, cost, latency, and the error reason if it failed.', 'If the record is not visible yet, wait a few seconds and refresh once.'], check: 'Success: one successful call is visible with its model, tokens, and cost.', problem: 'An empty log does not prove failure. Check that the client received a reply and clear filters.', link: { to: '/user/logs', label: 'Open usage logs' } },
      { number: '06', icon: Ticket, title: 'Buy and redeem credit', summary: 'Paste multiple codes and receive separate results.', actions: ['Open Redeem, use the card-store link, complete payment, and copy the code.', 'Paste one or more codes. Separate them with new lines, spaces, or commas.', 'Tap Redeem. Each code reports its own success or failure; successful codes stay applied.', 'Return to Overview and confirm the balance increased.'], check: 'Success: the redemption succeeds and the Overview balance increases.', problem: 'For used, expired, or malformed codes, do not resubmit the same code. Send the reason and order number to an administrator.', link: { to: '/user/redemptions', label: 'Open Redeem' } },
    ],
    callTitle: 'First-call checklist', callIntro: 'If your client has advanced settings, confirm these values:',
    callItems: ['Protocol: OpenAI-compatible, not the native Anthropic protocol.', 'Base URL: the address above with exactly one /v1.', 'API Key: paste the complete sk- secret without quotes.', 'Model: copy a currently available name from Channel monitor.', 'Streaming: turn it off for the first test if the client shows a blank response.'],
    troubleshootingTitle: 'Common messages',
    troubleshooting: [{ title: 'The page keeps loading', detail: 'Check your phone network and refresh once. Do not submit repeatedly in multiple tabs.' }, { title: '401 / Unauthorized', detail: 'Copy the key again from My Keys, remove whitespace, and confirm it is enabled.' }, { title: '404 / Not found', detail: 'Remove a duplicate /v1 and confirm the current site address is used.' }, { title: '429 / Busy', detail: 'Choose another healthy public group and wait a few seconds. Repeated retries do not help.' }, { title: 'Balance did not change', detail: 'Check Usage logs first, wait for delayed statistics, and never redeem the same code twice.' }],
    finishTitle: 'After setup, remember these three places', finishBody: 'Overview shows balance and channels; My Keys copies your address and secret; Usage logs verifies every call. Open Redeem only when you need credit.',
    finishLinks: [{ to: '/user', icon: WalletCards, label: 'Back to Overview' }, { to: '/user/keys', icon: KeyRound, label: 'My Keys' }, { to: '/user/logs', icon: FileText, label: 'Usage logs' }],
  },
}

const fadeUp = { initial: { opacity: 0, y: 12 }, animate: { opacity: 1, y: 0 } }

export default function UserTutorial() {
  const { i18n } = useTranslation()
  const language = i18n.resolvedLanguage?.startsWith('zh') ? 'zh' : 'en'
  const content = copy[language]
  const endpoint = typeof window === 'undefined' ? '' : `${window.location.origin}/v1`
  const [endpointCopied, setEndpointCopied] = useState(false)

  const handleCopyEndpoint = async () => {
    if (!endpoint || !(await copyText(endpoint))) return
    setEndpointCopied(true)
    window.setTimeout(() => setEndpointCopied(false), 2000)
  }

  return (
    <div className="mx-auto w-full max-w-4xl space-y-7 pb-4">
      <motion.header {...fadeUp} transition={{ duration: 0.3 }} className="space-y-3">
        <div className="inline-flex items-center gap-2 rounded-full border border-primary/25 bg-primary/8 px-3 py-1.5 text-xs font-semibold text-primary">
          <BookOpen className="size-4" aria-hidden="true" />
          <span>{content.badge}</span>
        </div>
        <h1 className="max-w-2xl text-3xl font-semibold tracking-tight sm:text-4xl">{content.title}</h1>
        <p className="max-w-2xl text-base leading-7 text-muted-foreground">{content.subtitle}</p>
      </motion.header>

      <motion.section {...fadeUp} transition={{ duration: 0.3, delay: 0.04 }} aria-labelledby="tutorial-endpoint" className="overflow-hidden rounded-[16px] border border-primary/30 bg-[linear-gradient(120deg,rgba(0,113,227,0.16),rgba(41,151,255,0.07))] p-4 shadow-sm sm:p-5">
        <div className="flex items-start gap-3">
          <span className="flex size-10 shrink-0 items-center justify-center rounded-xl bg-primary text-primary-foreground"><ExternalLink className="size-5" aria-hidden="true" /></span>
          <div className="min-w-0 flex-1">
            <h2 id="tutorial-endpoint" className="text-lg font-semibold">{content.endpointTitle}</h2>
            <p className="mt-1 text-sm leading-6 text-muted-foreground">{content.endpointHint}</p>
            <div className="mt-4 flex min-h-14 items-center gap-2 rounded-xl border border-primary/25 bg-background/80 p-2 pl-3">
              <code className="min-w-0 flex-1 break-all font-mono text-sm font-semibold text-foreground sm:text-base">{endpoint || '—'}</code>
              <Button type="button" variant="default" size="lg" className="min-h-11 shrink-0 px-3" onClick={() => { void handleCopyEndpoint() }} disabled={!endpoint} aria-label={endpointCopied ? content.copied : content.copyEndpoint}>
                {endpointCopied ? <Check aria-hidden="true" /> : <Copy aria-hidden="true" />}
                <span className="hidden sm:inline">{endpointCopied ? content.copied : content.copyEndpoint}</span>
              </Button>
            </div>
          </div>
        </div>
      </motion.section>

      <section aria-labelledby="tutorial-prep" className="space-y-3">
        <h2 id="tutorial-prep" className="flex items-center gap-2 text-xl font-semibold"><ClipboardCheck className="size-5 text-primary" aria-hidden="true" />{content.beforeStart}</h2>
        <ul className="grid gap-2 sm:grid-cols-3">
          {content.beforeStartItems.map(item => <li key={item} className="flex gap-2 rounded-xl border border-border/70 bg-card/55 p-3 text-sm leading-6"><CheckCircle2 className="mt-0.5 size-4 shrink-0 text-emerald-600 dark:text-emerald-400" aria-hidden="true" /><span>{item}</span></li>)}
        </ul>
      </section>

      <section aria-labelledby="tutorial-steps" className="space-y-3">
        <div>
          <h2 id="tutorial-steps" className="flex items-center gap-2 text-xl font-semibold"><ArrowRight className="size-5 text-primary" aria-hidden="true" />{content.stepsTitle}</h2>
          <p className="mt-1 text-sm leading-6 text-muted-foreground">{content.stepsHint}</p>
        </div>
        <div className="space-y-3">
          {content.steps.map((step, index) => {
            const Icon = step.icon
            return (
              <motion.details key={step.number} {...fadeUp} transition={{ duration: 0.28, delay: index * 0.04 }} open={index === 0} className="group overflow-hidden rounded-[14px] border border-border/80 bg-card/65 shadow-sm backdrop-blur-sm">
                <summary className="flex min-h-20 cursor-pointer list-none items-center gap-3 px-4 py-3 focus-visible:outline-none focus-visible:ring-3 focus-visible:ring-ring/50 sm:px-5 [&::-webkit-details-marker]:hidden">
                  <span className="flex size-11 shrink-0 items-center justify-center rounded-xl bg-primary/12 text-sm font-bold tabular-nums text-primary">{step.number}</span>
                  <span className="min-w-0 flex-1"><span className="flex items-center gap-2 text-base font-semibold"><Icon className="size-4 shrink-0 text-primary" aria-hidden="true" />{step.title}</span><span className="mt-1 block text-sm text-muted-foreground">{step.summary}</span></span>
                  <ChevronDown className="size-5 shrink-0 text-muted-foreground transition-transform duration-200 group-open:rotate-180" aria-hidden="true" />
                </summary>
                <div className="border-t border-border/70 px-4 pb-5 pt-4 sm:px-5">
                  <ol className="space-y-3 pl-1">
                    {step.actions.map((action, actionIndex) => <li key={action} className="flex gap-3 text-sm leading-6"><span className="flex size-6 shrink-0 items-center justify-center rounded-full bg-primary/10 text-xs font-semibold text-primary">{actionIndex + 1}</span><span className="min-w-0">{action}</span></li>)}
                  </ol>
                  <div className="mt-4 space-y-2 rounded-xl bg-emerald-500/8 p-3 text-sm leading-6"><p className="flex gap-2 font-medium text-emerald-800 dark:text-emerald-300"><CheckCircle2 className="mt-1 size-4 shrink-0" aria-hidden="true" />{step.check}</p>{step.problem && <p className="flex gap-2 text-muted-foreground"><LifeBuoy className="mt-1 size-4 shrink-0 text-primary" aria-hidden="true" />{step.problem}</p>}</div>
                  {step.link && <Button render={<Link to={step.link.to} />} variant="outline" className="mt-4 min-h-11"><ArrowRight aria-hidden="true" />{step.link.label}</Button>}
                </div>
              </motion.details>
            )
          })}
        </div>
      </section>

      <div className="grid gap-4 md:grid-cols-2">
        <section aria-labelledby="tutorial-call" className="rounded-[14px] border border-border/80 bg-card/55 p-4 sm:p-5">
          <h2 id="tutorial-call" className="flex items-center gap-2 text-lg font-semibold"><MessageCircle className="size-5 text-primary" aria-hidden="true" />{content.callTitle}</h2>
          <p className="mt-1 text-sm leading-6 text-muted-foreground">{content.callIntro}</p>
          <ul className="mt-3 space-y-2 text-sm leading-6">{content.callItems.map(item => <li key={item} className="flex gap-2"><Check className="mt-1 size-4 shrink-0 text-emerald-600 dark:text-emerald-400" aria-hidden="true" /><span>{item}</span></li>)}</ul>
        </section>
        <section aria-labelledby="tutorial-troubleshooting" className="rounded-[14px] border border-border/80 bg-card/55 p-4 sm:p-5">
          <h2 id="tutorial-troubleshooting" className="flex items-center gap-2 text-lg font-semibold"><LifeBuoy className="size-5 text-primary" aria-hidden="true" />{content.troubleshootingTitle}</h2>
          <div className="mt-3 divide-y divide-border/70">{content.troubleshooting.map(item => <details key={item.title} className="group py-2 first:pt-0 last:pb-0"><summary className="flex min-h-11 cursor-pointer list-none items-center gap-2 text-sm font-medium focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/50 [&::-webkit-details-marker]:hidden"><ChevronDown className="size-4 shrink-0 text-muted-foreground transition-transform duration-200 group-open:rotate-180" aria-hidden="true" />{item.title}</summary><p className="pl-6 text-sm leading-6 text-muted-foreground">{item.detail}</p></details>)}</div>
        </section>
      </div>

      <motion.section {...fadeUp} transition={{ duration: 0.3 }} aria-labelledby="tutorial-finish" className="rounded-[14px] border border-primary/20 bg-primary/[0.045] p-4 sm:p-5">
        <h2 id="tutorial-finish" className="flex items-center gap-2 text-lg font-semibold"><CheckCircle2 className="size-5 text-primary" aria-hidden="true" />{content.finishTitle}</h2>
        <p className="mt-1 text-sm leading-6 text-muted-foreground">{content.finishBody}</p>
        <div className="mt-4 grid gap-2 sm:grid-cols-3">{content.finishLinks.map(({ to, icon: Icon, label }) => <Button key={to} render={<Link to={to} />} variant="outline" className="min-h-11 justify-start"><Icon aria-hidden="true" />{label}<ArrowRight className="ml-auto" aria-hidden="true" /></Button>)}</div>
      </motion.section>
    </div>
  )
}
