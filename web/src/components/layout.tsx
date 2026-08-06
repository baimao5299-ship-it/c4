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
    /* 统一圆角玻璃胶囊外壳 + 底部标签栏（liquid-glass-webgl BottomTabs 形态）：
       顶 = 标题 + 操作（语言/登出），中 = 内容，底 = 玻璃胶囊标签栏；
       标签栏只有选中项是玻璃态（其余透明），选中项 = 白色玻璃药丸 + 蓝色 accent */
    <div className="min-h-screen p-3 md:p-4">
      <div className="glass-shell mx-auto flex max-w-[1600px] min-h-[calc(100vh-1.5rem)] flex-col overflow-hidden md:min-h-[calc(100vh-2rem)]">
        <header className="flex items-center justify-between gap-3 px-4 pt-3 md:px-5">
          <div className="flex items-center gap-2 text-lg font-semibold text-slate-800">{t('common.appTitle')}</div>
          <div className="flex items-center gap-2">
            <div className="glass-panel inline-flex items-center gap-1 p-0.5">
              {LANGS.map(({ code, label }) => (
                <Button
                  key={code}
                  size="sm"
                  variant="ghost"
                  className={cn('h-7 min-w-9 px-2 rounded-md', lang === code && 'bg-white/80 text-primary shadow-[inset_0_1px_3px_rgba(255,255,255,0.6)] hover:bg-white/80')}
                  onClick={() => setLang(code)}
                >
                  {label}
                </Button>
              ))}
            </div>
            <Button variant="ghost" size="sm" className="h-8 gap-1.5 text-slate-600 hover:bg-white/60 hover:text-slate-900" onClick={() => { auth.clear(); navTo('/login') }}>
              <LogOut className="h-4 w-4" />
              <span className="hidden md:inline">{t('common.logout')}</span>
            </Button>
          </div>
        </header>
        <main className="flex-1 overflow-auto p-4 md:p-5">
          <motion.div key={location.pathname} initial={{ opacity: 0, y: 8 }} animate={{ opacity: 1, y: 0 }} transition={{ duration: 0.2 }}>
            <Outlet />
          </motion.div>
        </main>
        <nav className="px-3 pb-3 md:px-4 md:pb-4">
          <div className="glass-bar mx-auto flex w-full max-w-4xl gap-1 rounded-full p-1">
            {nav.map(({ to, key, icon: Icon }) => (
              <NavLink key={to} to={to}
                className={({ isActive }) => `group relative flex flex-1 flex-col items-center justify-center gap-1 rounded-full px-3 py-2 text-xs transition-all ${isActive ? 'bg-white/80 text-primary shadow-[inset_0_1px_3px_rgba(255,255,255,0.6),0_2px_10px_rgba(0,0,0,0.08)]' : 'text-slate-600 hover:bg-white/30 hover:text-slate-900'}`}>
                {({ isActive }) => (
                  <>
                    <Icon className={`h-5 w-5 transition-transform duration-150 group-hover:scale-110 ${isActive ? 'text-primary' : 'text-slate-500'}`} />
                    <span className="text-[11px] leading-none">{t(key)}</span>
                  </>
                )}
              </NavLink>
            ))}
          </div>
        </nav>
      </div>
    </div>
  )
}
