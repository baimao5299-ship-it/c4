import { NavLink, Outlet, useNavigate, useLocation } from 'react-router-dom'
import { motion } from 'framer-motion'
import { LayoutDashboard, Boxes, Users, FolderOpen, FileText, BarChart3, LogOut } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { auth } from '@/lib/auth'
import { setLang, type AppLang } from '@/lib/i18n'
import { Button } from '@/components/ui/button'
import { cn } from '@/lib/utils'

const nav = [
  { to: '/dashboard', key: 'nav.overview', icon: LayoutDashboard },
  { to: '/templates', key: 'nav.templates', icon: Boxes },
  { to: '/accounts', key: 'nav.accounts', icon: Users },
  { to: '/groups', key: 'nav.groups', icon: FolderOpen },
  { to: '/logs', key: 'nav.logs', icon: FileText },
  { to: '/stats', key: 'nav.stats', icon: BarChart3 },
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
      <aside className="glass-sidebar w-56 text-slate-100 flex flex-col">
        <div className="p-4 font-semibold text-lg">{t('common.appTitle')}</div>
        <nav className="flex-1 space-y-1 p-2">
          {nav.map(({ to, key, icon: Icon }) => (
            <NavLink key={to} to={to}
              className={({ isActive }) => `group flex items-center gap-2 rounded-md px-3 py-2 text-sm transition-colors ${isActive ? 'bg-indigo-400/25 text-white shadow-[inset_0_1px_0_rgba(255,255,255,0.15)]' : 'text-slate-400 hover:bg-indigo-400/10 hover:text-slate-100'}`}>
              <Icon className="h-4 w-4 transition-transform duration-150 group-hover:scale-110" /> {t(key)}
            </NavLink>
          ))}
        </nav>
        <div className="p-3 border-t border-white/10">
          <Button variant="ghost" className="w-full justify-start text-slate-400 hover:bg-indigo-400/15 hover:text-slate-100" onClick={() => { auth.clear(); navTo('/login') }}>
            <LogOut className="h-4 w-4 mr-2" /> {t('common.logout')}
          </Button>
        </div>
      </aside>
      <main className="flex-1 overflow-auto p-6">
        <header className="mb-4 flex items-center justify-end">
          <div className="glass-panel inline-flex items-center gap-1 p-0.5">
            {LANGS.map(({ code, label }) => (
              <Button
                key={code}
                size="sm"
                variant="ghost"
                className={cn('h-7 min-w-9 px-2 rounded-md', lang === code && 'bg-[linear-gradient(135deg,#6366f1,#8b5cf6)] text-white shadow-[inset_0_1px_0_rgba(255,255,255,0.25)] hover:brightness-110')}
                onClick={() => setLang(code)}
              >
                {label}
              </Button>
            ))}
          </div>
        </header>
        <motion.div key={location.pathname} initial={{ opacity: 0, y: 8 }} animate={{ opacity: 1, y: 0 }} transition={{ duration: 0.2 }}>
          <Outlet />
        </motion.div>
      </main>
    </div>
  )
}
