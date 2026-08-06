import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { RouterProvider, createBrowserRouter, Navigate } from 'react-router-dom'
import { ApiClient } from '@/lib/api/client'
import { auth } from '@/lib/auth'
import Login from '@/pages/login'
import Layout from '@/components/layout'
import Dashboard from '@/pages/dashboard'
import Templates from '@/pages/templates'
import Accounts from '@/pages/accounts'
import Groups from '@/pages/groups'
import Logs from '@/pages/logs'
import Stats from '@/pages/stats'

export const api = new ApiClient(auth.getToken)

const router = createBrowserRouter([
  { path: '/login', element: <Login /> },
  {
    path: '/',
    element: <Layout />,
    children: [
      { index: true, element: <Navigate to="/dashboard" replace /> },
      { path: 'dashboard', element: <Dashboard /> },
      { path: 'templates', element: <Templates /> },
      { path: 'accounts', element: <Accounts /> },
      { path: 'groups', element: <Groups /> },
      { path: 'logs', element: <Logs /> },
      { path: 'stats', element: <Stats /> },
    ],
  },
])

export default function App() {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: 0, refetchOnWindowFocus: false } },
  })
  // 401 全局拦截：清 token 回登录（页面内抛 ApiUnauthorized 由 query onError 统一处理）
  return (
    <QueryClientProvider client={qc}>
      <RouterProvider router={router} />
    </QueryClientProvider>
  )
}
