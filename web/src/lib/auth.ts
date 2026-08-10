const KEY = 'gpm_admin_token'
const ROLE_KEY = 'gpm_admin_role'
export const auth = {
  getToken: () => localStorage.getItem(KEY),
  setToken: (t: string) => localStorage.setItem(KEY, t),
  getRole: () => localStorage.getItem(ROLE_KEY),
  setRole: (r: string) => localStorage.setItem(ROLE_KEY, r),
  clear: () => {
    localStorage.removeItem(KEY)
    localStorage.removeItem(ROLE_KEY)
  },
}
const USER_KEY = 'gpm_user_token'
export const userAuth = {
  getToken: () => localStorage.getItem(USER_KEY),
  setToken: (t: string) => localStorage.setItem(USER_KEY, t),
  clear: () => localStorage.removeItem(USER_KEY),
}
