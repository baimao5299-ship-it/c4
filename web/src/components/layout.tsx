import { Outlet, useLocation } from 'react-router-dom'
import { motion } from 'framer-motion'
import { LayoutDashboard, Boxes, Users, UserCog, FolderOpen, FileText, BarChart3, ScrollText, Ticket, Coins, Settings, KeyRound } from 'lucide-react'
import { useQuery } from '@tanstack/react-query'
import { useTranslation } from 'react-i18next'
import { userApi } from '@/lib/api/client'
import { setLang, type AppLang } from '@/lib/i18n'
import { Button } from '@/components/ui/button'
import { ModeToggle } from '@/components/mode-toggle'
import { cn } from '@/lib/utils'
import AppSidebar from '@/components/app-sidebar'

// 管理组（/app）与用户组（/user）两组导航；分组标题与用户端一致。
const navs = [
  {
    titleKey: 'user.nav.adminSection',
    items: [
      { to: '/app/dashboard', key: 'nav.overview', icon: LayoutDashboard },
      { to: '/app/templates', key: 'nav.templates', icon: Boxes },
      { to: '/app/accounts', key: 'nav.accounts', icon: Users },
      { to: '/app/users', key: 'nav.users', icon: UserCog },
      { to: '/app/groups', key: 'nav.groups', icon: FolderOpen },
      { to: '/app/logs', key: 'nav.logs', icon: FileText },
      { to: '/app/stats', key: 'nav.stats', icon: BarChart3 },
      { to: '/app/rules', key: 'nav.rules', icon: ScrollText },
      { to: '/app/redemption-codes', key: 'nav.redemptions', icon: Ticket },
      { to: '/app/pricing', key: 'nav.pricing', icon: Coins },
      { to: '/app/settings', key: 'nav.settings', icon: Settings },
    ],
  },
  {
    titleKey: 'user.nav.userSection',
    items: [
      { to: '/user', key: 'user.nav.overview', icon: LayoutDashboard, end: true },
      { to: '/user/keys', key: 'user.nav.keys', icon: KeyRound, end: false },
      { to: '/user/logs', key: 'user.nav.logs', icon: FileText, end: false },
      { to: '/user/stats', key: 'user.nav.stats', icon: BarChart3, end: false },
      { to: '/user/redemptions', key: 'user.nav.redemptions', icon: Ticket, end: false },
    ],
  },
]

const LANGS: { code: AppLang; label: string }[] = [
  { code: 'zh-CN', label: '中' },
  { code: 'en', label: 'EN' },
]

export default function Layout() {
  const location = useLocation()
  const { i18n } = useTranslation()
  const lang: AppLang = i18n.resolvedLanguage?.startsWith('zh') ? 'zh-CN' : 'en'
  // 顶栏 email 与用户端同查询键共享缓存；401 由 App.tsx 全局 handleAuthError 统一处理
  const { data: me } = useQuery({
    queryKey: ['user', 'me'],
    queryFn: () => userApi.me(),
    staleTime: 60_000,
  })
  return (
    <div className="flex min-h-screen">
      <AppSidebar navs={navs} />
      <main className="flex-1 overflow-auto p-6">
        <header className="mb-4 flex items-center justify-between">
          <div />
          <div className="flex items-center gap-2">
            <span className="text-sm text-muted-foreground">{me?.Email ?? ''}</span>
            <ModeToggle />
            <div className="inline-flex items-center gap-1 rounded-md border bg-background p-0.5">
              {LANGS.map(({ code, label }) => (
                <Button
                  key={code}
                  size="sm"
                  variant="ghost"
                  className={cn('h-7 min-w-9 px-2', lang === code && 'bg-secondary text-secondary-foreground')}
                  onClick={() => setLang(code)}
                >
                  {label}
                </Button>
              ))}
            </div>
          </div>
        </header>
        <motion.div key={location.pathname} initial={{ opacity: 0, y: 8 }} animate={{ opacity: 1, y: 0 }} transition={{ duration: 0.2 }}>
          <Outlet />
        </motion.div>
      </main>
    </div>
  )
}
