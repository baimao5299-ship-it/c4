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

// /app 与 /user 共用单一 AppShell：路由切换只换 Outlet 内容，
// 侧边栏/顶栏不重挂（消除闪烁）。导航由 me().Role 决定：
// platform_admin 同时看到管理组（/app）与用户组（/user），普通用户仅用户组。

// 用户中心菜单组
const userNav = [
  { to: '/user', key: 'user.nav.overview', icon: LayoutDashboard, end: true },
  { to: '/user/keys', key: 'user.nav.keys', icon: KeyRound, end: false },
  { to: '/user/logs', key: 'user.nav.logs', icon: FileText, end: false },
  { to: '/user/stats', key: 'user.nav.stats', icon: BarChart3, end: false },
  { to: '/user/redemptions', key: 'user.nav.redemptions', icon: Ticket, end: false },
]

// platform_admin 专属的管理端菜单组
const adminNav = [
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
]

const LANGS: { code: AppLang; label: string }[] = [
  { code: 'zh-CN', label: '中' },
  { code: 'en', label: 'EN' },
]

// me 未加载（undefined）时先按普通用户渲染用户组，避免侧边栏闪动；
// me 返回后 platform_admin 自动补上管理组。401 由 App.tsx 全局 handleAuthError 统一处理。
export default function AppShell() {
  const location = useLocation()
  const { i18n } = useTranslation()
  const lang: AppLang = i18n.resolvedLanguage?.startsWith('zh') ? 'zh-CN' : 'en'
  // 全局 qc 共享（与各页面同键缓存）；staleTime 60s 内路由切换不重复请求
  const { data: me } = useQuery({
    queryKey: ['user', 'me'],
    queryFn: () => userApi.me(),
    staleTime: 60_000,
  })
  const isAdmin = me?.Role === 'platform_admin'
  const navs = isAdmin
    ? [{ titleKey: 'user.nav.adminSection', items: adminNav }, { titleKey: 'user.nav.userSection', items: userNav }]
    : [{ titleKey: 'user.nav.userSection', items: userNav }]
  return (
    <div className="flex min-h-screen">
      <AppSidebar navs={navs} userEmail={me?.Email} />
      <main className="flex flex-1 flex-col overflow-auto">
        <header className="flex h-12 shrink-0 items-center gap-2 border-b px-4 lg:px-6">
          <div className="flex flex-1 items-center justify-end gap-2 lg:gap-3">
            <span className="text-sm text-muted-foreground">{me?.Email ?? ''}</span>
            <ModeToggle />
            <div className="inline-flex items-center gap-1 rounded-md border bg-background p-0.5">
              {LANGS.map(({ code, label }) => (
                <Button
                  key={code}
                  size="sm"
                  variant="ghost"
                  className={cn('h-8 min-w-10 px-2', lang === code && 'bg-secondary text-secondary-foreground')}
                  onClick={() => setLang(code)}
                >
                  {label}
                </Button>
              ))}
            </div>
          </div>
        </header>
        <div className="@container/main flex flex-1 flex-col gap-2 px-4 lg:px-6">
          <div className="flex flex-col gap-4 py-4 md:gap-6 md:py-6">
            <motion.div key={location.pathname} initial={{ opacity: 0, y: 8 }} animate={{ opacity: 1, y: 0 }} transition={{ duration: 0.2 }}>
              <Outlet />
            </motion.div>
          </div>
        </div>
      </main>
    </div>
  )
}
