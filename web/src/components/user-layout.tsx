import { useState } from 'react'
import { NavLink, Outlet, useLocation, useNavigate, type NavigateFunction } from 'react-router-dom'
import { motion } from 'framer-motion'
import { QueryCache, MutationCache, QueryClient, QueryClientProvider, useQuery } from '@tanstack/react-query'
import { LayoutDashboard, KeyRound, FileText, BarChart3, Ticket, LogOut, Boxes, Users, UserCog, FolderOpen, ScrollText, Coins, Settings } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { ApiUnauthorized, userApi } from '@/lib/api/client'
import { userAuth } from '@/lib/auth'
import { setLang, type AppLang } from '@/lib/i18n'
import { Button } from '@/components/ui/button'
import { ModeToggle } from '@/components/mode-toggle'
import { cn } from '@/lib/utils'

const nav = [
  { to: '/user', key: 'user.nav.overview', icon: LayoutDashboard, end: true },
  { to: '/user/keys', key: 'user.nav.keys', icon: KeyRound, end: false },
  { to: '/user/logs', key: 'user.nav.logs', icon: FileText, end: false },
  { to: '/user/stats', key: 'user.nav.stats', icon: BarChart3, end: false },
  { to: '/user/redemptions', key: 'user.nav.redemptions', icon: Ticket, end: false },
]

// platform_admin 专属的管理端菜单组（图标与 /app 管理端 layout.tsx 保持一致）
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

// 401 拦截：任何 query/mutation 收到 ApiUnauthorized（client.ts 对 401 的归一化）
// → 清 userAuth + 跳 /user/login。独立 userQc 只作用于 /user 子树，管理端全局 qc 不受影响。
const handleAuthError = (err: unknown, navTo: NavigateFunction) => {
  if (err instanceof ApiUnauthorized) {
    userAuth.clear()
    navTo('/user/login')
  }
}

// 外层负责创建独立 QueryClient（把 useNavigate 捕获进 cache onError），
// 内层 Shell 在 Provider 之内执行 useQuery（me）与渲染。
export default function UserLayout() {
  const navTo = useNavigate()
  const [qc] = useState(() => new QueryClient({
    queryCache: new QueryCache({ onError: err => handleAuthError(err, navTo) }),
    mutationCache: new MutationCache({ onError: err => handleAuthError(err, navTo) }),
    defaultOptions: {
      queries: { retry: 0, refetchOnWindowFocus: false },
      mutations: { retry: 0 },
    },
  }))
  return (
    <QueryClientProvider client={qc}>
      <Shell />
    </QueryClientProvider>
  )
}

function Shell() {
  const navTo = useNavigate()
  const location = useLocation()
  const { t, i18n } = useTranslation()
  const lang: AppLang = i18n.resolvedLanguage?.startsWith('zh') ? 'zh-CN' : 'en'
  const { data: me } = useQuery({
    queryKey: ['user', 'me'],
    queryFn: () => userApi.me(),
    staleTime: 60_000,
  })
  const isAdmin = me?.Role === 'platform_admin'
  const navLinkCls = ({ isActive }: { isActive: boolean }) =>
    `group flex items-center gap-2 rounded-md px-3 py-2 text-sm transition-colors ${isActive ? 'bg-sidebar-accent text-sidebar-accent-foreground' : 'text-sidebar-foreground/60 hover:bg-sidebar-accent hover:text-sidebar-accent-foreground'}`
  const sectionTitle = (key: string) => (
    <p className="px-3 pt-3 pb-1 text-xs font-medium text-sidebar-foreground/40">{t(key)}</p>
  )
  const logout = () => {
    userAuth.clear()
    navTo('/user/login')
  }
  return (
    <div className="flex min-h-screen">
      <aside className="w-56 border-r border-sidebar-border bg-sidebar text-sidebar-foreground flex flex-col">
        <div className="p-4 font-semibold text-lg">{t('common.appTitle')}</div>
        <nav className="flex-1 space-y-1 overflow-y-auto p-2">
          {isAdmin && (
            <>
              {sectionTitle('user.nav.adminSection')}
              {adminNav.map(({ to, key, icon: Icon }) => (
                <NavLink key={to} to={to} className={navLinkCls}>
                  <Icon className="h-4 w-4 transition-transform duration-150 group-hover:scale-110" /> {t(key)}
                </NavLink>
              ))}
              {sectionTitle('user.nav.userSection')}
            </>
          )}
          {nav.map(({ to, key, icon: Icon, end }) => (
            <NavLink key={to} to={to} end={end} className={navLinkCls}>
              <Icon className="h-4 w-4 transition-transform duration-150 group-hover:scale-110" /> {t(key)}
            </NavLink>
          ))}
        </nav>
      </aside>
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
            <Button variant="ghost" size="sm" onClick={logout}>
              <LogOut className="h-4 w-4 mr-1" /> {t('user.nav.logout')}
            </Button>
          </div>
        </header>
        <motion.div key={location.pathname} initial={{ opacity: 0, y: 8 }} animate={{ opacity: 1, y: 0 }} transition={{ duration: 0.2 }}>
          <Outlet />
        </motion.div>
      </main>
    </div>
  )
}
