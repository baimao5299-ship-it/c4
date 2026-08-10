import { NavLink, Outlet, useNavigate, useLocation } from 'react-router-dom'
import { motion } from 'framer-motion'
import { LayoutDashboard, Boxes, Users, UserCog, FolderOpen, FileText, BarChart3, ScrollText, Ticket, Coins, Settings, LogOut } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { userAuth } from '@/lib/auth'
import { setLang, type AppLang } from '@/lib/i18n'
import { Button } from '@/components/ui/button'
import { ModeToggle } from '@/components/mode-toggle'
import { cn } from '@/lib/utils'

const nav = [
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

export default function Layout() {
  const navTo = useNavigate()
  const location = useLocation()
  const { t, i18n } = useTranslation()
  const lang: AppLang = i18n.resolvedLanguage?.startsWith('zh') ? 'zh-CN' : 'en'
  return (
    <div className="flex min-h-screen">
      <aside className="w-56 border-r border-sidebar-border bg-sidebar text-sidebar-foreground flex flex-col">
        <div className="p-4 font-semibold text-lg">{t('common.appTitle')}</div>
        <nav className="flex-1 space-y-1 p-2">
          {nav.map(({ to, key, icon: Icon }) => (
            <NavLink key={to} to={to}
              className={({ isActive }) => `group flex items-center gap-2 rounded-md px-3 py-2 text-sm transition-colors ${isActive ? 'bg-sidebar-accent text-sidebar-accent-foreground' : 'text-sidebar-foreground/60 hover:bg-sidebar-accent hover:text-sidebar-accent-foreground'}`}>
              <Icon className="h-4 w-4 transition-transform duration-150 group-hover:scale-110" /> {t(key)}
            </NavLink>
          ))}
        </nav>
        <div className="p-3 border-t border-sidebar-border">
          <Button variant="ghost" className="w-full justify-start text-sidebar-foreground/60" onClick={() => { userAuth.clear(); navTo('/user/login') }}>
            <LogOut className="h-4 w-4 mr-2" /> {t('common.logout')}
          </Button>
        </div>
      </aside>
      <main className="flex-1 overflow-auto p-6">
        <header className="mb-4 flex items-center justify-between">
          <div />
          <div className="flex items-center gap-2">
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
