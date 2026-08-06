import { NavLink, Outlet, useNavigate, useLocation } from 'react-router-dom'
import { motion } from 'framer-motion'
import { LayoutDashboard, Boxes, Users, FolderOpen, FileText, BarChart3, LogOut } from 'lucide-react'
import { auth } from '@/lib/auth'
import { Button } from '@/components/ui/button'

const nav = [
  { to: '/dashboard', label: '总览', icon: LayoutDashboard },
  { to: '/templates', label: '模板', icon: Boxes },
  { to: '/accounts', label: '账号', icon: Users },
  { to: '/groups', label: '分组', icon: FolderOpen },
  { to: '/logs', label: '日志', icon: FileText },
  { to: '/stats', label: '统计', icon: BarChart3 },
]

export default function Layout() {
  const navTo = useNavigate()
  const location = useLocation()
  return (
    <div className="flex min-h-screen">
      <aside className="w-56 border-r bg-slate-950 text-slate-100 flex flex-col">
        <div className="p-4 font-semibold text-lg">网关管理台</div>
        <nav className="flex-1 space-y-1 p-2">
          {nav.map(({ to, label, icon: Icon }) => (
            <NavLink key={to} to={to}
              className={({ isActive }) => `flex items-center gap-2 rounded-md px-3 py-2 text-sm transition-colors ${isActive ? 'bg-slate-800 text-white' : 'text-slate-400 hover:bg-slate-900 hover:text-slate-200'}`}>
              <Icon className="h-4 w-4" /> {label}
            </NavLink>
          ))}
        </nav>
        <div className="p-3 border-t">
          <Button variant="ghost" className="w-full justify-start text-slate-400" onClick={() => { auth.clear(); navTo('/login') }}>
            <LogOut className="h-4 w-4 mr-2" /> 退出
          </Button>
        </div>
      </aside>
      <main className="flex-1 overflow-auto p-6">
        <motion.div key={location.pathname} initial={{ opacity: 0, y: 8 }} animate={{ opacity: 1, y: 0 }} transition={{ duration: 0.2 }}>
          <Outlet />
        </motion.div>
      </main>
    </div>
  )
}
