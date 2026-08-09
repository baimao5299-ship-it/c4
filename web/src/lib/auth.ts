const KEY = 'gpm_admin_token'
export const auth = {
  getToken: () => localStorage.getItem(KEY),
  setToken: (t: string) => localStorage.setItem(KEY, t),
  clear: () => localStorage.removeItem(KEY),
}
const USER_KEY = 'gpm_user_token'
export const userAuth = {
  getToken: () => localStorage.getItem(USER_KEY),
  setToken: (t: string) => localStorage.setItem(USER_KEY, t),
  clear: () => localStorage.removeItem(USER_KEY),
}
