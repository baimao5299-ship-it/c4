import { MutationCache, QueryCache, QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { RouterProvider, createBrowserRouter, Navigate } from 'react-router-dom'
import { ApiClient, ApiUnauthorized } from '@/lib/api/client'
import { ThemeProvider } from '@/components/theme-provider'
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

// 401 全局拦截（Task 2→3 handoff 硬性要求）：任何 query/mutation 收到
// ApiUnauthorized（client.ts 对 401 响应的归一化）→ 清 token + 跳 /login。
// 页面无需各自 onError 兜底；QueryCache/MutationCache 的 onError 在 React Query
// v5 中对所有活跃观测者/变更统一触发（queries.retry: 0 保证每个请求只报一次）。
const handleAuthError = (err: unknown) => {
  if (err instanceof ApiUnauthorized) {
    auth.clear()
    router.navigate('/login')
  }
}

const qc = new QueryClient({
  queryCache: new QueryCache({ onError: handleAuthError }),
  mutationCache: new MutationCache({ onError: handleAuthError }),
  defaultOptions: {
    queries: { retry: 0, refetchOnWindowFocus: false },
    mutations: { retry: 0 },
  },
})

export default function App() {
  return (
    <ThemeProvider defaultTheme="system" storageKey="vite-ui-theme">
      <QueryClientProvider client={qc}>
        <RouterProvider router={router} />
      </QueryClientProvider>
    </ThemeProvider>
  )
}
